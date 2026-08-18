package proxy

// SSE streaming pump, ported from src/proxy/stream.mjs (pipeSse). It wires an
// upstream response body into the client http.ResponseWriter while providing
// keepalive pings, an idle/stall watchdog, backpressure, and a synthesized
// terminal SSE sequence on abnormal end.
//
// Concurrency model (keeps every http.ResponseWriter write in ONE goroutine):
//
//   - A single OUTPUT goroutine owns `w`: it consumes frames from a buffered
//     channel (cap 8; a full channel is natural backpressure) and writes +
//     flushes each one. On a terminal `done` frame it injects the protocol
//     error frame and abnormal-finish tail (unless the stream already saw a
//     message_stop / [DONE] marker or the client disconnected) and forwards
//     the Result via resultCh.
//
//   - A READER goroutine drains upBody through a bufio.Reader, applies
//     think-stripping and the OpenAI keepalive-frame filter, classifies SSE
//     frames, and forwards data frames. On clean EOF / read error / stream
//     cancel it emits the `done` frame carrying the computed Result.
//
//   - A KEEPALIVE goroutine (SSE only) emits `:\n\n` liveness frames every
//     interval while the stream is alive.
//
//   - A WATCHDOG goroutine (SSE only) implements the SLOW STREAM log and the
//     idle timeout. It never writes to `w`: on idle it cancels the stream
//     context the reader selects on and closes upBody to unblock any in-flight
//     read.
//
// The synchronous PumpSSE call blocks until the OUTPUT goroutine completes and
// returns the Result; the future handler reads it.
//
// Per-chunk order inside the reader (per the port spec): think-strip, then the
// OpenAI frame filter, then classifier.Analyze, then forward. This differs
// from stream.mjs (which analyzes the raw chunk before stripping) only in the
// EmptyOutput edge cases: content that ends up stripped/dropped before the
// classifier sees it does not count as a data event.
//
// Known deviation: stream.mjs appends a ` (cause: <code>)` suffix when the
// upstream error carries a cause code; Go errors expose no such chain, so the
// UPSTREAM STREAM ERROR log carries the error text only.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Defaults (stream.mjs KEEPALIVE_INTERVAL).
const (
	defaultKeepaliveInterval = 10 * time.Second
	watchdogTick             = 250 * time.Millisecond
	readBufferSize           = 32 * 1024
)

// Options configures the stream pump. Zero values fall back to defaults
// (KeepaliveInterval → 10s); nil Log/LogDebug are treated as no-ops.
// SlowResponse is reserved for the handler phase (slow-response degrade) and
// is not used by the pump itself.
type Options struct {
	IsSSE             bool
	Format            StreamFormat
	ChunkTimeout      time.Duration
	IdleTimeout       time.Duration
	SlowResponse      time.Duration
	StripThinkingTags bool
	KeepaliveInterval time.Duration // 0 → 10s
	Log               func(msg string)
	LogDebug          func(format string, args ...any)
}

// Result is the outcome of a streamed response, mirroring the object pipeSse
// reports via onResult. StatusCode is the effective status (200 on clean end,
// 502/504 on abnormal ends). Error carries the reason for non-200 results.
// EmptyOutput reports whether an SSE stream produced no real data event.
type Result struct {
	StatusCode  int
	DurationMs  int64
	Chunks      int
	Error       string
	EmptyOutput bool
}

// frame is a unit of output handed to the single-writer OUTPUT goroutine.
// reason != "" on a done frame marks an abnormal end (and names the protocol
// error frame); client_disconnected is special-cased to skip error/EOM writes.
type frame struct {
	data           []byte
	isKeepalive    bool
	done           bool
	reason         string
	sawMessageStop bool
	result         Result
}

// closeTracked wraps upBody so the reader can distinguish "the body was closed
// prematurely" (upstream_closed) from a clean EOF and from genuine read
// errors, mirroring stream.mjs's separate 'end' / 'error' / 'close' events.
type closeTracked struct {
	io.ReadCloser
	closed atomic.Bool
}

func (c *closeTracked) Close() error {
	c.closed.Store(true)
	return c.ReadCloser.Close()
}

type readRes struct {
	n   int
	err error
}

// PumpSSE streams upBody into w according to Options, blocking until the
// stream ends. It is synchronous and returns the final Result.
func PumpSSE(ctx context.Context, w http.ResponseWriter, upBody io.ReadCloser, o Options) Result {
	if o.KeepaliveInterval <= 0 {
		o.KeepaliveInterval = defaultKeepaliveInterval
	}
	if o.Log == nil {
		o.Log = func(string) {}
	}
	if o.LogDebug == nil {
		o.LogDebug = func(string, ...any) {}
	}
	reqStart := time.Now()

	frameCh := make(chan frame, 8)
	resultCh := make(chan Result, 1)
	stop := make(chan struct{})
	streamDone := make(chan struct{})
	// outDone closes when the OUTPUT goroutine terminates (write failure or
	// stop). Consumers select on it so a dead OUTPUT can never block a send
	// to frameCh forever (the wedge: OUTPUT stuck on w.Write to a stalled
	// client, frameCh full, reader pinned on frameCh <- ).
	outDone := make(chan struct{})

	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	defer close(stop)

	body := &closeTracked{ReadCloser: upBody}
	var (
		lastChunkAt   atomic.Int64
		sawData       atomic.Bool
		idleTriggered atomic.Bool
	)
	lastChunkAt.Store(time.Now().UnixNano())

	durMS := func() int64 { return time.Since(reqStart).Milliseconds() }

	// finish sends the terminal done frame. Called exactly once per stream.
	// If OUTPUT already died (client write failure), it has delivered its own
	// Result; skip the send instead of blocking on a channel nobody reads.
	finish := func(res Result, reason string, sawMessageStop bool) {
		select {
		case frameCh <- frame{done: true, reason: reason, sawMessageStop: sawMessageStop, result: res}:
		case <-outDone:
		}
	}

	// ── Single-writer OUTPUT goroutine ──
	go func() {
		defer close(outDone)
		rc := http.NewResponseController(w)
		// Per-frame write deadline: a client that stops reading fills its TCP
		// send buffer and w.Write blocks until the kernel gives up (minutes).
		// Enforce the chunk timeout so the pump fails the write, tears the
		// stream down and records the failure instead of wedging. The
		// deadline is re-armed on every frame so live streams are unaffected.
		writeDeadline := o.ChunkTimeout
		if writeDeadline <= 0 {
			writeDeadline = o.IdleTimeout
		}
		if writeDeadline <= 0 {
			writeDeadline = 30 * time.Second // sane cap for zero-config callers
		}
		writeFrame := func(p []byte) error {
			_ = rc.SetWriteDeadline(time.Now().Add(writeDeadline))
			if _, err := w.Write(p); err != nil {
				return err
			}
			return rc.Flush()
		}
		for {
			select {
			case f := <-frameCh:
				if f.done {
					if f.reason != "" && o.IsSSE && !f.sawMessageStop && f.reason != "client_disconnected" {
						_ = writeFrame([]byte(SSEErrorFrame(o.Format, f.reason)))
						_ = writeFrame([]byte(AbnormalFinish(o.Format)))
					}
					resultCh <- f.result
					return
				}
				var p []byte
				if f.isKeepalive {
					p = []byte(":\n\n")
				} else {
					p = f.data
				}
				if err := writeFrame(p); err != nil {
					// Client stalled/disconnected mid-stream: stop the whole
					// pump. Closing the body and cancelling the stream context
					// unblocks the reader (and any in-flight upstream read);
					// the reader's finish select sees outDone and skips the
					// done frame — this Result is the terminal one.
					o.Log(fmt.Sprintf("SSE WRITE FAILED (client stalled or disconnected): %v", err))
					_ = body.Close()
					cancelStream()
					resultCh <- Result{StatusCode: http.StatusBadGateway, DurationMs: durMS(), Error: "client_write_failed"}
					return
				}
			case <-stop:
				return
			}
		}
	}()

	// ── Reader ──
	classifier := NewFrameClassifier(nil, o.LogDebug)
	thinkStripper := NewThinkStripper()
	var openaiPending []byte
	chunkCount := 0

	// filterOpenAI ports stream.mjs l.308-341 verbatim: bare
	// `data: null` / `data:null` / `data:` keepalive frames are dropped at
	// frame level. An incomplete frame tail (no trailing \n\n) is held back so
	// frames split across TCP chunks are still filtered; all other bytes pass
	// through untouched.
	filterOpenAI := func(chunk []byte) []byte {
		var combined []byte
		if len(openaiPending) > 0 {
			combined = append(append([]byte(nil), openaiPending...), chunk...)
			openaiPending = nil
		} else {
			combined = chunk
		}
		lastIdx := bytes.LastIndex(combined, []byte("\n\n"))
		if lastIdx == -1 {
			openaiPending = append([]byte(nil), combined...)
			return nil
		}
		readyEnd := lastIdx + 2
		ready := combined[:readyEnd]
		openaiPending = append([]byte(nil), combined[readyEnd:]...)
		hasBadFrame := bytes.Contains(ready, []byte("data: null")) ||
			bytes.Contains(ready, []byte("data:null")) ||
			bytes.Contains(ready, []byte("data:\n"))
		if hasBadFrame {
			readyText := string(ready)
			lines := strings.Split(readyText, "\n")
			kept := lines[:0]
			for _, line := range lines {
				t := strings.TrimSpace(line)
				if t != "data: null" && t != "data:null" && t != "data:" {
					kept = append(kept, line)
				}
			}
			cleaned := strings.Join(kept, "\n")
			if cleaned != readyText {
				o.LogDebug("dropped invalid data: keepalive frame(s), %d -> %d bytes", len(ready), len(cleaned))
				return []byte(cleaned)
			}
			return ready
		}
		return ready
	}

	// ── Terminal handlers (run in the reader goroutine) ──

	// handleCleanEnd: upstream returned io.EOF (stream.mjs 'end' handler).
	handleCleanEnd := func() {
		classifier.Flush()
		forward, unterminated := thinkStripper.Flush()
		if unterminated {
			// Withheld bytes were never forwarded: silent 200 would be
			// truncation. Surface as a 502 with an error frame (l.368-375).
			o.Log(fmt.Sprintf("UPSTREAM ENDED MID-SPAN (unterminated thinking tag, %d chunks)", chunkCount))
			finish(Result{StatusCode: 502, DurationMs: durMS(), Chunks: chunkCount, Error: "upstream_ended_mid_frame"},
				"upstream_ended_mid_frame", classifier.SawMessageStop())
			return
		}
		// Forward a legitimate trailing tag prefix and any OpenAI tail frame
		// so no bytes are lost (verbatim-streaming contract, l.376-391).
		var pend []byte
		if len(forward) > 0 {
			pend = append(pend, forward...)
		}
		if len(openaiPending) > 0 {
			pend = append(pend, openaiPending...)
			openaiPending = nil
		}
		if len(pend) > 0 {
			select {
			case frameCh <- frame{data: append([]byte(nil), pend...)}:
			case <-outDone:
				return
			}
		}
		empty := o.IsSSE && !classifier.SawDataEvent()
		if empty {
			o.Log(fmt.Sprintf("EMPTY SSE STREAM (%d chunks)", chunkCount))
		}
		o.Log(fmt.Sprintf("200 (stream complete, %dms, %d chunks)", durMS(), chunkCount))
		finish(Result{StatusCode: 200, DurationMs: durMS(), Chunks: chunkCount, EmptyOutput: empty},
			"", classifier.SawMessageStop())
	}

	// handleStreamError: non-EOF read error (stream.mjs 'error' handler).
	handleStreamError := func(err error) {
		classifier.Flush()
		openaiPending = nil
		o.Log(fmt.Sprintf("UPSTREAM STREAM ERROR: %s", err))
		finish(Result{StatusCode: 502, DurationMs: durMS(), Chunks: chunkCount, Error: err.Error()},
			"upstream_stream_error", classifier.SawMessageStop())
	}

	// handleUpstreamClosed: body closed / truncated before a clean end
	// (stream.mjs 'close' handler, connection terminated prematurely).
	handleUpstreamClosed := func() {
		classifier.Flush()
		openaiPending = nil
		o.Log(fmt.Sprintf("UPSTREAM CLOSED (connection terminated prematurely, %dms, %d chunks)", durMS(), chunkCount))
		finish(Result{StatusCode: 502, DurationMs: durMS(), Chunks: chunkCount, Error: "upstream_closed"},
			"upstream_closed", classifier.SawMessageStop())
	}

	// handleCtxCancel: stream context cancelled (client gone or idle timeout).
	handleCtxCancel := func() {
		_ = body.Close()
		if idleTriggered.Load() {
			o.Log(fmt.Sprintf("SSE IDLE TIMEOUT (%gs no events)", o.IdleTimeout.Seconds()))
			finish(Result{StatusCode: 504, DurationMs: durMS(), Chunks: chunkCount, Error: "sse_idle_timeout"},
				"sse_idle_timeout", classifier.SawMessageStop())
			return
		}
		finish(Result{StatusCode: 502, DurationMs: durMS(), Chunks: chunkCount, Error: "client_disconnected"},
			"client_disconnected", classifier.SawMessageStop())
	}

	// ── Reader loop ──
	go func() {
		defer close(streamDone)
		br := bufio.NewReader(body)
		buff := make([]byte, readBufferSize)
		readCh := make(chan readRes, 1)
		spawn := func() {
			go func() {
				n, err := br.Read(buff)
				readCh <- readRes{n: n, err: err}
			}()
		}
		spawn()

		for {
			select {
			case rr := <-readCh:
				// Go's io.Reader contract allows a single Read to return
				// n > 0 bytes together with io.EOF (the transport may coalesce
				// the final chunk with the clean end). Process the data before
				// acting on the terminal error — mirroring Node's separate
				// 'data' then 'end' events.
				if rr.n > 0 {
					chunkCount++
					chunk := buff[:rr.n]
					out := chunk
					if o.IsSSE && o.Format == StreamOpenAI {
						if o.StripThinkingTags {
							out = thinkStripper.Process(out)
						}
						out = filterOpenAI(out)
					}
					if o.IsSSE {
						classifier.Analyze(out)
						sawData.Store(classifier.SawDataEvent())
					}
					lastChunkAt.Store(time.Now().UnixNano())
					o.LogDebug("CHUNK #%d %db, elapsed %dms", chunkCount, rr.n, durMS())
					if len(out) > 0 {
						// Never block on a full frameCh forever: if the pump is
						// being torn down (client gone, idle timeout, OUTPUT
						// write failure) the send must yield so the watchdog's
						// cancelStream can actually terminate the stream.
						select {
						case frameCh <- frame{data: append([]byte(nil), out...)}:
						case <-streamCtx.Done():
							handleCtxCancel()
							return
						}
					}
				}
				if rr.err != nil {
					switch {
					case rr.err == io.EOF && !body.closed.Load():
						handleCleanEnd()
					case idleTriggered.Load():
						handleCtxCancel()
					case streamCtx.Err() != nil:
						handleCtxCancel()
					case body.closed.Load() || errors.Is(rr.err, io.ErrUnexpectedEOF) || errors.Is(rr.err, io.ErrClosedPipe):
						handleUpstreamClosed()
					default:
						handleStreamError(rr.err)
					}
					return
				}
				spawn()
			case <-streamCtx.Done():
				handleCtxCancel()
				return
			}
		}
	}()

	// ── Keepalive goroutine (SSE only) ──
	if o.IsSSE {
		go func() {
			t := time.NewTicker(o.KeepaliveInterval)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-streamDone:
					return
				case <-streamCtx.Done():
					return
				case <-t.C:
					select {
					case frameCh <- frame{isKeepalive: true}:
					case <-stop:
						return
					case <-streamDone:
						return
					case <-streamCtx.Done():
						return
					default:
						o.LogDebug("keepalive backpressure, skipping tick")
					}
				}
			}
		}()
	}

	// ── Watchdog goroutine (SSE only) ──
	if o.IsSSE {
		var stallLogged bool
		go func() {
			t := time.NewTicker(watchdogTick)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-streamDone:
					return
				case <-streamCtx.Done():
					return
				case <-t.C:
					elapsed := time.Since(time.Unix(0, lastChunkAt.Load()))
					if o.IdleTimeout > 0 && elapsed > o.IdleTimeout {
						idleTriggered.Store(true)
						_ = body.Close() // unblock any in-flight read
						cancelStream()   // reader emits the idle done frame
						return
					}
					if o.ChunkTimeout > 0 && elapsed > o.ChunkTimeout && !stallLogged {
						stallLogged = true
						kind := "data gap"
						if !sawData.Load() {
							kind = "no data yet"
						}
						o.Log(fmt.Sprintf("SLOW STREAM (%s > %gs) — keeping stream alive", kind, o.ChunkTimeout.Seconds()))
					}
				}
			}
		}()
	}

	return <-resultCh
}
