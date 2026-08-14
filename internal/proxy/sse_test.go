package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Test doubles ──────────────────────────────────────────────────────────────

// fakeResponseWriter is a minimal http.ResponseWriter: it buffers written bytes
// and counts flushes (so http.NewResponseController can drive it).
type fakeResponseWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	flushes int
}

func (w *fakeResponseWriter) Header() http.Header { return http.Header{} }
func (w *fakeResponseWriter) WriteHeader(int)     {}
func (w *fakeResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// Flush satisfies http.Flusher for http.NewResponseController.
func (w *fakeResponseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushes++
}

func (w *fakeResponseWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// fakeBody is a controllable io.ReadCloser. It serves queued chunks, then
// blocks until pushed more data / end() / fail() / Close(). Reads are
// interruptible so the pump's in-flight read goroutine never leaks.
type fakeBody struct {
	mu     sync.Mutex
	q      [][]byte
	idx    int
	err    error // returned (once) when the queue is exhausted
	eof    bool  // end() seen: return io.EOF after the queue
	closed bool
	notify chan struct{} // closed to wake a blocked Read
}

func (b *fakeBody) push(data string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.q = append(b.q, []byte(data))
	b.wakeLocked()
}

func (b *fakeBody) end() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.eof = true
	b.wakeLocked()
}

func (b *fakeBody) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
	b.wakeLocked()
}

func (b *fakeBody) wakeLocked() {
	if b.notify != nil {
		close(b.notify)
		b.notify = nil
	}
}

func (b *fakeBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	for {
		if b.idx < len(b.q) {
			d := b.q[b.idx]
			b.idx++
			n := copy(p, d)
			b.mu.Unlock()
			return n, nil
		}
		if b.err != nil {
			e := b.err
			b.err = nil
			b.mu.Unlock()
			return 0, e
		}
		if b.eof || b.closed {
			b.mu.Unlock()
			return 0, io.EOF
		}
		if b.notify == nil {
			b.notify = make(chan struct{})
		}
		n := b.notify
		b.mu.Unlock()
		<-n
		b.mu.Lock()
	}
}

func (b *fakeBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.wakeLocked()
	return nil
}

// logSink collects Log/LogDebug output for assertions.
type logSink struct {
	mu   sync.Mutex
	msgs []string
}

func (s *logSink) log(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
}

func (s *logSink) logf(format string, args ...any) {
	s.log(fmt.Sprintf(format, args...))
}

func (s *logSink) has(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

func (s *logSink) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.msgs {
		if strings.Contains(m, substr) {
			n++
		}
	}
	return n
}

// ── Helpers ──────────────────────────────────────────────────────────────────

type pumpCall struct {
	res Result
	out string
}

// runPump runs PumpSSE to completion in the caller goroutine. It is only safe
// for bodies that terminate without external coordination (EOF/error bodies).
func runPump(t *testing.T, body *fakeBody, o Options) pumpCall {
	t.Helper()
	rw := &fakeResponseWriter{}
	res := PumpSSE(context.Background(), rw, body, o)
	return pumpCall{res: res, out: rw.String()}
}

// pumpAsync starts PumpSSE in a background goroutine; callers must release the
// body (end/fail/Close) and then await the returned channel.
func pumpAsync(body *fakeBody, o Options) (chan pumpCall, *fakeResponseWriter) {
	rw := &fakeResponseWriter{}
	ch := make(chan pumpCall, 1)
	go func() {
		res := PumpSSE(context.Background(), rw, body, o)
		ch <- pumpCall{res: res, out: rw.String()}
	}()
	return ch, rw
}

func waitPump(t *testing.T, ch chan pumpCall, timeout time.Duration) pumpCall {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(timeout):
		t.Fatal("PumpSSE did not return within", timeout)
		return pumpCall{}
	}
}

// ── Tests ────────────────────────────────────────────────────────────────────

// Non-SSE 200s are pure passthrough: raw bytes, no keepalive/watchdog/EOM.
func TestPumpSSEPassthroughClean200(t *testing.T) {
	body := &fakeBody{}
	body.push("hello ")
	body.push("world")
	body.end()
	o := Options{Log: func(string) {}}
	got := runPump(t, body, o)
	if got.res.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", got.res.StatusCode)
	}
	if got.res.EmptyOutput {
		t.Error("EmptyOutput = true, want false for non-SSE")
	}
	if got.res.Chunks != 2 {
		t.Errorf("Chunks = %d, want 2", got.res.Chunks)
	}
	if got.out != "hello world" {
		t.Errorf("output = %q, want %q", got.out, "hello world")
	}
	if strings.Contains(got.out, "message_stop") || strings.Contains(got.out, "[DONE]") {
		t.Errorf("passthrough must not inject EOM, got %q", got.out)
	}
}

// Keepalive frames flow while the upstream is silent.
func TestPumpSSEKeepaliveWhileSilent(t *testing.T) {
	body := &fakeBody{}
	o := Options{
		IsSSE:             true,
		Format:            StreamAnthropic,
		KeepaliveInterval: 30 * time.Millisecond,
		Log:               func(string) {},
	}
	ch, rw := pumpAsync(body, o)
	time.Sleep(150 * time.Millisecond) // ~5 keepalive ticks with no upstream data
	body.end()
	got := waitPump(t, ch, 3*time.Second)
	if got.res.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", got.res.StatusCode)
	}
	if !strings.Contains(got.out, ":\n\n") {
		t.Errorf("expected keepalive frames, got %q", got.out)
	}
	rw.mu.Lock()
	fl := rw.flushes
	rw.mu.Unlock()
	if fl == 0 {
		t.Error("expected at least one flush")
	}
}

// Idle timeout terminates a silent stream: 504 + error frame + abnormal finish.
func TestPumpSSEIdleTimeout(t *testing.T) {
	body := &fakeBody{}
	o := Options{
		IsSSE:        true,
		Format:       StreamAnthropic,
		IdleTimeout:  100 * time.Millisecond,
		ChunkTimeout: 0, // only SLOW logging, no termination
		Log:          func(string) {},
	}
	ch, _ := pumpAsync(body, o) // body never delivers data
	got := waitPump(t, ch, 3*time.Second)
	if got.res.StatusCode != 504 {
		t.Errorf("StatusCode = %d, want 504", got.res.StatusCode)
	}
	if got.res.Error != "sse_idle_timeout" {
		t.Errorf("Error = %q, want sse_idle_timeout", got.res.Error)
	}
	if !strings.Contains(got.out, "sse_idle_timeout") {
		t.Errorf("expected error frame carrying reason, got %q", got.out)
	}
	if !strings.HasSuffix(got.out, EomTail(StreamAnthropic)) {
		t.Errorf("expected abnormal finish tail, got %q", got.out)
	}
}

// A slow-but-live stream logs SLOW STREAM exactly once and still completes 200.
func TestPumpSSESlowLoggedOnceStreamCompletes(t *testing.T) {
	body := &fakeBody{}
	sink := &logSink{}
	o := Options{
		IsSSE:        true,
		Format:       StreamAnthropic,
		ChunkTimeout: 100 * time.Millisecond,
		IdleTimeout:  5 * time.Second, // long enough to never fire
		Log:          sink.log,
	}
	body.push("data: a\n\n")
	ch, _ := pumpAsync(body, o)
	time.Sleep(400 * time.Millisecond) // gap > ChunkTimeout: watchdog tick logs SLOW once
	body.push("data: b\n\n")
	body.end()
	got := waitPump(t, ch, 3*time.Second)
	if got.res.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", got.res.StatusCode)
	}
	if n := sink.count("SLOW STREAM"); n != 1 {
		t.Errorf("SLOW STREAM logged %d times, want exactly 1", n)
	}
	if !sink.has("data gap") {
		t.Errorf("expected 'data gap' variant (data was seen), logs: %v", sink.msgs)
	}
	if got.res.Chunks != 2 {
		t.Errorf("Chunks = %d, want 2", got.res.Chunks)
	}
}

// An abnormal end after message_stop is NOT wrapped in error frame + EOM.
func TestPumpSSEEOMSuppressedAfterMessageStop(t *testing.T) {
	body := &fakeBody{}
	body.push("event: message_stop\n\ndata: {}\n\n")
	body.fail(io.ErrUnexpectedEOF) // abnormal end
	o := Options{IsSSE: true, Format: StreamAnthropic, Log: func(string) {}}
	got := runPump(t, body, o)
	if got.res.StatusCode != 502 {
		t.Errorf("StatusCode = %d, want 502", got.res.StatusCode)
	}
	if got.res.Error != "upstream_closed" {
		t.Errorf("Error = %q, want upstream_closed", got.res.Error)
	}
	if strings.Contains(got.out, "proxy_error") {
		t.Errorf("error frame must be suppressed after message_stop, got %q", got.out)
	}
	if strings.Contains(got.out, "message_delta") {
		t.Errorf("abnormal finish must be suppressed after message_stop, got %q", got.out)
	}
}

// Premature connection termination (truncated body) → 502 upstream_closed + EOM.
func TestPumpSSEUpstreamClosed(t *testing.T) {
	body := &fakeBody{}
	body.push("data: hi\n\n")
	body.fail(io.ErrUnexpectedEOF)
	o := Options{IsSSE: true, Format: StreamAnthropic, Log: func(string) {}}
	got := runPump(t, body, o)
	if got.res.StatusCode != 502 {
		t.Errorf("StatusCode = %d, want 502", got.res.StatusCode)
	}
	if got.res.Error != "upstream_closed" {
		t.Errorf("Error = %q, want upstream_closed", got.res.Error)
	}
	if !strings.Contains(got.out, "proxy_error") {
		t.Errorf("expected error frame, got %q", got.out)
	}
	if !strings.HasSuffix(got.out, EomTail(StreamAnthropic)) {
		t.Errorf("expected abnormal finish tail, got %q", got.out)
	}
}

// Generic read error → 502 with the error message + EOM.
func TestPumpSSEReadError(t *testing.T) {
	body := &fakeBody{}
	body.push("data: hi\n\n")
	body.fail(errors.New("boom"))
	o := Options{IsSSE: true, Format: StreamAnthropic, Log: func(string) {}}
	got := runPump(t, body, o)
	if got.res.StatusCode != 502 {
		t.Errorf("StatusCode = %d, want 502", got.res.StatusCode)
	}
	if got.res.Error != "boom" {
		t.Errorf("Error = %q, want boom", got.res.Error)
	}
	if !strings.Contains(got.out, "proxy_error") {
		t.Errorf("expected error frame, got %q", got.out)
	}
	if !strings.HasSuffix(got.out, EomTail(StreamAnthropic)) {
		t.Errorf("expected abnormal finish tail, got %q", got.out)
	}
}

// Upstream ends with an unclosed <think> span: withheld bytes must never be
// leaked; the stream surfaces as a 502 with an error frame.
func TestPumpSSEMidThinkSpanEnd(t *testing.T) {
	body := &fakeBody{}
	body.push("hello<think>secret")
	body.end()
	o := Options{
		IsSSE:             true,
		Format:            StreamOpenAI,
		StripThinkingTags: true,
		Log:               func(string) {},
	}
	got := runPump(t, body, o)
	if got.res.StatusCode != 502 {
		t.Errorf("StatusCode = %d, want 502", got.res.StatusCode)
	}
	if got.res.Error != "upstream_ended_mid_frame" {
		t.Errorf("Error = %q, want upstream_ended_mid_frame", got.res.Error)
	}
	if strings.Contains(got.out, "secret") {
		t.Errorf("withheld think bytes leaked: %q", got.out)
	}
	// The clean "hello" prefix was held back by the OpenAI frame filter (no
	// trailing \n\n) and, like stream.mjs, the mid-span 502 path drops it
	// (l.368-375 returns before the openaiPending forward at l.388-391).
	if strings.Contains(got.out, "hello") {
		t.Errorf("partial frame must be dropped on abnormal end, got %q", got.out)
	}
	if !strings.Contains(got.out, "proxy_error") {
		t.Errorf("expected error frame, got %q", got.out)
	}
	if !strings.HasSuffix(got.out, EomTail(StreamOpenAI)) {
		t.Errorf("expected abnormal finish tail, got %q", got.out)
	}
}

// Comment-only keepalive lines are not data: the stream is reported empty.
func TestPumpSSEEmptySSE(t *testing.T) {
	body := &fakeBody{}
	body.push(": ping\n\n")
	body.end()
	sink := &logSink{}
	o := Options{IsSSE: true, Format: StreamAnthropic, Log: sink.log}
	got := runPump(t, body, o)
	if got.res.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", got.res.StatusCode)
	}
	if !got.res.EmptyOutput {
		t.Error("EmptyOutput = false, want true")
	}
	if !sink.has("EMPTY SSE STREAM") {
		t.Errorf("expected EMPTY SSE STREAM log, got %v", sink.msgs)
	}
	// Comments are forwarded verbatim.
	if !strings.Contains(got.out, ": ping\n\n") {
		t.Errorf("comment keepalive must be forwarded, got %q", got.out)
	}
}

// Bare data:null / data: keepalive frames are dropped at frame level, including
// frames split across TCP chunks.
func TestPumpSSEDataNullFramesDropped(t *testing.T) {
	body := &fakeBody{}
	body.push("data: null\n\n")
	body.push("data: hel") // split frame: tail held back
	body.push("lo\n\n")
	body.end()
	o := Options{IsSSE: true, Format: StreamOpenAI, Log: func(string) {}}
	got := runPump(t, body, o)
	if got.res.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", got.res.StatusCode)
	}
	if strings.Contains(got.out, "data: null") || strings.Contains(got.out, "data:null") {
		t.Errorf("data:null keepalive leaked: %q", got.out)
	}
	if !strings.Contains(got.out, "data: hello\n\n") {
		t.Errorf("good frame must be forwarded, got %q", got.out)
	}
}

// Client disconnect: 502 client_disconnected, body closed, NO error/EOM writes.
func TestPumpSSEClientCancel(t *testing.T) {
	body := &fakeBody{}
	ctx, cancel := context.WithCancel(context.Background())
	rw := &fakeResponseWriter{}
	resCh := make(chan Result, 1)
	go func() {
		resCh <- PumpSSE(ctx, rw, body, Options{
			IsSSE:  true,
			Format: StreamAnthropic,
			Log:    func(string) {},
		})
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case res := <-resCh:
		if res.StatusCode != 502 {
			t.Errorf("StatusCode = %d, want 502", res.StatusCode)
		}
		if res.Error != "client_disconnected" {
			t.Errorf("Error = %q, want client_disconnected", res.Error)
		}
		if out := rw.String(); strings.Contains(out, "proxy_error") || strings.Contains(out, "message_delta") {
			t.Errorf("client disconnect must not write error/EOM frames, got %q", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PumpSSE did not return after client cancel")
	}
	body.mu.Lock()
	closed := body.closed
	body.mu.Unlock()
	if !closed {
		t.Error("upstream body was not closed on client disconnect")
	}
}

// Multi-byte UTF-8 adjacent to think tags passes through byte-faithfully.
func TestPumpSSEMultiByteUTF8NearThinkTags(t *testing.T) {
	body := &fakeBody{}
	body.push("你好<thi")
	body.push("nk>秘密</think>世界")
	body.end()
	o := Options{
		IsSSE:             true,
		Format:            StreamOpenAI,
		StripThinkingTags: true,
		Log:               func(string) {},
	}
	got := runPump(t, body, o)
	if got.res.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", got.res.StatusCode)
	}
	if got.out != "你好世界" {
		t.Errorf("output = %q, want %q", got.out, "你好世界")
	}
	if strings.Contains(got.out, "<think>") || strings.Contains(got.out, "秘密") {
		t.Errorf("thinking content leaked: %q", got.out)
	}
}
