package proxy

// Request handler: buffers the client body, then proxies to upstream with
// retry, WAF cookie refresh, model-health marking, and SSE streaming.
//
// Port of src/proxy/handler.mjs (handleProxyRequest). The Node state machine's
// invariant flags map to Go primitives:
//
//   - proxyDone / finishProxy → the retry loop returns once; Active is
//     decremented via defer.
//   - errorHandled per attempt → disappears: the sequential retry loop returns
//     one error per attempt, so no multiple-listener double-fire is possible.
//   - upstreamResponded disarms the adaptive response timeout →
//     context.WithCancel + time.AfterFunc; the timer is stopped once the
//     response headers arrive so long SSE streams are never cut.
//   - req "close" → r.Context().Done() cancels the upstream request (the http
//     server cancels the request context on client disconnect).
//
// Known deviations:
//   - Node logs a ` (cause: <code>)` suffix for upstream errors; Go errors have
//     no such chain (the UPSTREAM STREAM ERROR log carries the text only).
//   - The socket-level setKeepAlive/setNoDelay/setTimeout hints Node applies on
//     the client socket for SSE are unnecessary in Go's http server and are
//     omitted.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/trefeon/agentrouter-spoof-proxy/internal/auth"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/config"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/logstore"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/models"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/resilience"
)

// Handler holds the injected dependencies for the request handler. All fields
// are safe for concurrent use by multiple requests.
type Handler struct {
	Cfg       *config.Config
	WAF       *auth.Store
	Breaker   *resilience.Breaker
	Discovery *models.Discovery
	Health    *models.Health
	Recorder  *models.Recorder
	Client    *http.Client
	Active    *atomic.Int64
	Log       *slog.Logger
	Logs      *logstore.Store
}

// NewHandler wires the injected dependencies into a Handler.
func NewHandler(cfg *config.Config, wafStore *auth.Store, breaker *resilience.Breaker, discovery *models.Discovery, health *models.Health, recorder *models.Recorder, client *http.Client, log *slog.Logger, logs *logstore.Store) *Handler {
	return &Handler{
		Cfg:       cfg,
		WAF:       wafStore,
		Breaker:   breaker,
		Discovery: discovery,
		Health:    health,
		Recorder:  recorder,
		Client:    client,
		Active:    &atomic.Int64{},
		Log:       log,
		Logs:      logs,
	}
}

// logEntry records a dashboard log entry; a nil store is a no-op.
func (h *Handler) logEntry(e logstore.Entry) {
	if h.Logs != nil {
		h.Logs.Add(e)
	}
}

// ServeProxy handles one client request: body buffering with 413/408
// rejection, model/stream summarization, header spoofing, the retry loop with
// WAF/5xx/transport retries, and SSE streaming via PumpSSE.
func (h *Handler) ServeProxy(w http.ResponseWriter, r *http.Request) {
	rawPath := r.URL.RequestURI()
	method := r.Method
	path := RewritePath(rawPath)
	streamFormat := StreamFormatForPath(path)

	// Early Content-Length check: reject before buffering any bytes.
	if r.ContentLength > MaxBodySize {
		h.Log.Info(fmt.Sprintf("%s %s -> REJECTED 413 (payload_too_large)", method, rawPath))
		h.logEntry(logstore.Entry{Time: time.Now(), Level: logstore.LevelError, Path: rawPath, Format: string(streamFormat), Status: http.StatusRequestEntityTooLarge, Error: "payload_too_large"})
		h.rejectBody(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body exceeds 20MB limit", true)
		return
	}

	body, rejected := h.readBody(r, w)
	if rejected != 0 {
		if rejected == http.StatusRequestTimeout {
			// The upload deadline fired. Go's server cancels the request
			// context when a body read hits the read deadline (the same
			// internal path as a client disconnect), so this must NOT be
			// gated on r.Context().Err(), a still-connected client has to
			// receive the 408. It also skips the bounded drain: the missing
			// body never arrives, so draining would block the handler.
			h.Log.Info(fmt.Sprintf("%s %s -> REJECTED %d (%s)",
				method, rawPath, rejected, rejectionCode(rejected)))
			h.logEntry(logstore.Entry{Time: time.Now(), Level: logstore.LevelError, Path: rawPath, Format: string(streamFormat), Status: rejected, Error: rejectionCode(rejected)})
			h.rejectBody(w, r, rejected, rejectionCode(rejected), rejectionMessage(rejected), false)
			return
		}
		if r.Context().Err() != nil {
			return // client gone while uploading; nothing to respond to
		}
		h.Log.Info(fmt.Sprintf("%s %s -> REJECTED %d (%s)",
			method, rawPath, rejected, rejectionCode(rejected)))
		// The request body was never fully consumed, so Go's http server
		// will not flush the response on a keep-alive connection (it waits
		// for the remaining body to decide connection reuse). Declaring
		// Connection: close forces an immediate flush, mirroring the Node
		// rejectOversizedWithStatus drain-then-destroy behavior.
		h.logEntry(logstore.Entry{Time: time.Now(), Level: logstore.LevelError, Path: rawPath, Format: string(streamFormat), Status: rejected, Error: rejectionCode(rejected)})
		h.rejectBody(w, r, rejected, rejectionCode(rejected), rejectionMessage(rejected), true)
		return
	}
	if r.Context().Err() != nil {
		return // client disconnected during upload
	}

	summary := SummarizeRequest(body, path, method)
	h.Log.Debug(fmt.Sprintf("%s %s -> REQUEST %s", method, rawPath, summaryDebug(summary)))

	model := ""
	if summary.Model != "" {
		model = summary.Model
		h.Recorder.Start(model)
	}

	// The proxy's spoof owns the Anthropic-* headers; clients supply only
	// Authorization / x-api-key (nothing else passes through).
	// Profile-aware: default is opencode (SPOOF_PROFILE). For /v1/messages
	// (Anthropic) the full spoof set is sent, otherwise only the generic UA.
	isAnthropic := strings.HasPrefix(path, "/v1/messages")
	spoof := auth.HeadersForProfile(h.Cfg.NormalizedSpoofProfile(), isAnthropic)
	upstreamHeaders := make(http.Header)
	for k, v := range spoof {
		upstreamHeaders.Set(k, v)
	}
	upstreamHeaders.Set("Content-Type", "application/json")
	if a := r.Header.Get("Authorization"); a != "" {
		upstreamHeaders.Set("Authorization", a)
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		upstreamHeaders.Set("x-api-key", k)
	}
	if c := h.WAF.Get(); c != "" {
		upstreamHeaders.Set("Cookie", c)
	}

	body = InjectPrompt(body, path, h.Cfg.InjectSystemPrompt)

	if h.Breaker.IsOpen() {
		h.Log.Info(fmt.Sprintf("%s %s -> REJECTED (circuit open)", method, rawPath))
		h.logEntry(logstore.Entry{Time: time.Now(), Level: logstore.LevelError, Path: rawPath, Format: string(streamFormat), Status: http.StatusServiceUnavailable, Error: "circuit_open"})
		h.respondJSON(w, http.StatusServiceUnavailable, errorBody("circuit_open", "Upstream circuit breaker open, retry later"))
		return
	}

	h.Active.Add(1)
	defer h.Active.Add(-1)

	h.doRequest(w, r, body, path, streamFormat, summary, upstreamHeaders, model)
}

// StreamFormatForPath selects the SSE terminal format for a proxied path:
// StreamOpenAI for the OpenAI-style completion routes, StreamAnthropic for
// everything else. /v1/responses/compact shares the /v1/responses prefix.
func StreamFormatForPath(path string) StreamFormat {
	if strings.HasPrefix(path, "/v1/chat/completions") ||
		strings.HasPrefix(path, "/v1/completions") ||
		strings.HasPrefix(path, "/v1/responses") {
		return StreamOpenAI
	}
	return StreamAnthropic
}

func rejectionCode(status int) string {
	if status == http.StatusRequestTimeout {
		return "request_timeout"
	}
	return "payload_too_large"
}

func rejectionMessage(status int) string {
	if status == http.StatusRequestTimeout {
		return "Request body upload timed out"
	}
	return "Request body exceeds 20MB limit"
}

// readBody buffers the request body up to MaxBodySize, enforcing the body
// upload deadline. The deadline is applied via the response controller's read
// deadline rather than a watchdog goroutine: Go's net/http holds the request
// body's internal mutex across every r.Body.Read, so a concurrent reader
// blocked on a stalled upload would deadlock both r.Body.Close() and the
// server's own response-header flush (they take the same mutex) and the 408
// would never reach the client. A synchronous read with a deadline avoids the
// goroutine entirely. It returns (data, 0) on success, (nil, status) when the
// request was rejected with 413 or 408. On client disconnect it returns
// (nil, 0) and the caller must not respond.
//
// The read deadline is deliberately NOT cleared on the rejection paths:
// rejectBody drains and closes the body afterwards, and Go's request-body
// Close internally drains up to 256KB, on a stalled upload that drain would
// block the handler forever unless the (now-expired) read deadline makes the
// socket reads fail fast. rejectBody clears the deadline once the body is
// handled.
func (h *Handler) readBody(r *http.Request, w http.ResponseWriter) ([]byte, int) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(h.Cfg.BodyUploadTimeout()))

	data, err := io.ReadAll(io.LimitReader(r.Body, MaxBodySize+1))
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, http.StatusRequestTimeout
		}
		_ = rc.SetReadDeadline(time.Time{}) // disconnect: no drain will follow
		return nil, 0                       // client body error (disconnect); nothing to respond
	}
	if len(data) > MaxBodySize {
		return nil, http.StatusRequestEntityTooLarge
	}
	_ = rc.SetReadDeadline(time.Time{}) // body fully read; clear the deadline
	return data, 0
}

// rejectBody writes a rejection response and then, when drain is true, drains
// a bounded amount of the still-arriving upload so the client can finish
// reading the response over a live connection (never buffer, never forward the
// oversized body). The response declares Connection: close so Go's http server
// flushes it immediately even though the request body was never fully consumed
// (without it, a keep-alive server holds the response waiting for the rest of
// the body).
func (h *Handler) rejectBody(w http.ResponseWriter, r *http.Request, status int, code, message string, drain bool) {
	w.Header().Set("Connection", "close")
	h.respondJSON(w, status, errorBody(code, message))
	if drain {
		_, _ = io.CopyN(io.Discard, r.Body, 1<<20)
	}
	_ = r.Body.Close()
	// The read deadline set by readBody stays active through the drain and
	// Close above (a stalled upload would otherwise block the 256KB drain
	// inside request-body Close forever). Clear it now that the body is
	// handled.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
}

// respondJSON sends JSON with the CORS allow header, mirroring utils.mjs respondJson.
func (h *Handler) respondJSON(w http.ResponseWriter, status int, data any) {
	b, _ := json.Marshal(data)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func errorBody(code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message, "type": "proxy_error"}}
}

// summaryDebug formats the request summary for debug logging.
func summaryDebug(s RequestSummary) string {
	b, _ := json.Marshal(map[string]any{
		"method": s.Method, "path": s.Path, "bodyBytes": s.BodyBytes, "parseOk": s.ParseOK,
		"model": nullIfEmpty(s.Model), "stream": s.Stream,
		"maxTokens": nullIfNil(s.MaxTokens), "messageCount": nullIfNil(s.MessageCount),
	})
	return string(b)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfNil[T any](v *T) any {
	if v == nil {
		return nil
	}
	return *v
}

// sleepCtx sleeps for d or returns early if the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// doRequest runs the retry loop (handler.mjs l.167-440). Each iteration either
// produces a terminal client response and returns, or continues with the next
// attempt.
func (h *Handler) doRequest(w http.ResponseWriter, r *http.Request, body []byte, path string, streamFormat StreamFormat, summary RequestSummary, headers http.Header, model string) {
	rawPath := r.URL.RequestURI()
	method := r.Method
	upstreamURL := h.Cfg.TargetProto + "://" + net.JoinHostPort(h.Cfg.TargetHost, strconv.Itoa(h.Cfg.TargetPort)) + path

	reqCtx, cancelReq := context.WithCancel(r.Context())
	defer cancelReq()

	adaptiveTimeout := ResponseTimeout(len(body), h.Cfg.ResponseTimeoutMs)

	for attempt := 0; ; attempt++ {
		if reqCtx.Err() != nil {
			return // client gone
		}
		h.Log.Info(fmt.Sprintf("%s %s -> %s (attempt %d)", method, rawPath, path, attempt+1))

		attemptHeaders := headers.Clone() // Cookie may be refreshed between attempts
		resp, timedOut, err := h.roundTrip(reqCtx, method, upstreamURL, attemptHeaders, body, adaptiveTimeout)
		if err != nil {
			if h.handleTransportError(w, r, model, attempt, err, timedOut, reqCtx, streamFormat) {
				return // terminal error response written
			}
			continue // retried
		}

		// Response received: capture any rotated WAF cookies immediately
		// (handler.mjs captureWafCookies on every response).
		h.WAF.Capture(resp)
		status := resp.StatusCode
		respStart := time.Now()

		// WAF 403/405 on the first attempt → re-warmup and retry once.
		if (status == http.StatusForbidden || status == http.StatusMethodNotAllowed) && attempt == 0 {
			if h.handleWafResponse(w, r, resp, model, headers, reqCtx, status, respStart, streamFormat) {
				return // forwarded a non-WAF response (or gave up)
			}
			continue // WAF retried; next attempt
		}

		// 5xx retry, if exhausted, mark model unhealthy.
		if IsRetryable(status, "", h.Cfg.RetryOn5xx) && attempt < h.Cfg.MaxRetries {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			h.Log.Info(fmt.Sprintf("%s %s <- %d, retrying (%d/%d)...", method, rawPath, status, attempt+1, h.Cfg.MaxRetries))
			if !sleepCtx(reqCtx, RetryDelay(attempt, h.Cfg.RetryDelayMs)) {
				return
			}
			continue
		}

		// Immediately remove rate-limited models so 9Router falls back.
		if status == http.StatusTooManyRequests {
			h.Health.MarkExhausted(model)
		}
		if status >= 500 {
			h.Health.MarkFailed(model, status)
		}

		// Circuit accounting for a *final* upstream response: 5xx → failure,
		// 429 → neither (model lockout only), 4xx → neither, <400 → success.
		if status >= 500 {
			h.Breaker.RecordFailure()
		} else if status < 400 {
			h.Breaker.RecordSuccess()
		}

		filtered := FilterHeaders(resp.Header)
		// SSE detection: trust the upstream Content-Type first; fall back to
		// the request intent ("stream": true).
		sseByContentType := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
		isSSE := sseByContentType || (status == http.StatusOK && summary.Stream)
		if isSSE {
			filtered.Set("X-Accel-Buffering", "no")
			filtered.Set("Cache-Control", "no-cache")
			filtered.Set("Connection", "keep-alive")
			filtered.Set("Pragma", "no-cache")
			filtered.Set("Expires", "0")
		}

		if status != http.StatusOK {
			h.forwardNon200(w, r, resp, model, status, filtered, respStart, streamFormat)
			return
		}

		// 200 response: stream via PumpSSE for SSE, plain copy otherwise.
		h.Log.Debug(fmt.Sprintf("%s %s <- TTFB %dms, status 200, SSE=%v",
			method, rawPath, time.Since(respStart).Milliseconds(), isSSE))
		h.writeHead(w, http.StatusOK, filtered)
		if isSSE {
			h.streamSSE(w, r, resp, model, streamFormat, reqCtx)
		} else {
			h.copyBody(w, r, resp, model, streamFormat, respStart)
		}
		return
	}
}

// roundTrip performs one upstream request with an adaptive header-wait
// deadline. The timer is stopped once the response headers arrive so the
// deadline never applies to the streaming body. Returns timedOut=true when the
// adaptive deadline (or a transport DeadlineExceeded) fired, the caller must
// treat it as a timeout, never stream the body.
func (h *Handler) roundTrip(ctx context.Context, method, url string, headers http.Header, body []byte, adaptiveTimeout time.Duration) (*http.Response, bool, error) {
	var timedOut atomic.Bool
	var received atomic.Bool
	attemptCtx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(adaptiveTimeout, func() {
		if received.Load() {
			return // headers already arrived; the deadline no longer applies
		}
		timedOut.Store(true)
		cancel()
	})

	req, err := http.NewRequestWithContext(attemptCtx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header = headers

	resp, err := h.Client.Do(req)
	received.Store(true)
	timer.Stop() // headers arrived (or attempt failed): deadline no longer applies
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		timedOut.Store(true)
	}
	if err == nil && timedOut.Load() {
		// The deadline fired in the instant between headers arriving and the
		// received flag being set; the response body is already dead. Surface
		// it as a timeout instead of streaming a broken body.
		_ = resp.Body.Close()
		return nil, true, context.DeadlineExceeded
	}
	return resp, timedOut.Load(), err
}

// handleTransportError handles a failed attempt. When the error is retryable
// and attempts remain, it sleeps the backoff and returns false so the caller
// continues the loop; otherwise it writes the terminal 504/502 response and
// returns true.
func (h *Handler) handleTransportError(w http.ResponseWriter, r *http.Request, model string, attempt int, err error, timedOut bool, ctx context.Context, streamFormat StreamFormat) bool {
	rawPath := r.URL.RequestURI()
	method := r.Method
	msg := transportMessage(err)
	if timedOut {
		msg = "timeout"
	}

	// Client disconnected while waiting for a response: abort silently, no
	// breaker/health accounting, nothing to respond to (mirrors req.on("close")
	// destroying the upstream without recordFailure).
	if r.Context().Err() != nil {
		return true
	}

	// Retryable? Mirror handleError: attempt < MAX_RETRIES && isRetryable(null,
	// e.message, RETRY_ON_5XX).
	if attempt < h.Cfg.MaxRetries && IsRetryable(0, msg, h.Cfg.RetryOn5xx) {
		h.Log.Info(fmt.Sprintf("%s %s -> ERROR: %s, retrying (%d/%d)...", method, rawPath, msg, attempt+1, h.Cfg.MaxRetries))
		if !sleepCtx(ctx, RetryDelay(attempt, h.Cfg.RetryDelayMs)) {
			return true // client gone
		}
		return false
	}

	h.Breaker.RecordFailure()
	h.Health.MarkFailed(model, 503)
	h.Log.Info(fmt.Sprintf("%s %s -> ERROR: %s (final)", method, rawPath, msg))

	var status int
	var code, message string
	if timedOut {
		status, code, message = http.StatusGatewayTimeout, "timeout", "Upstream request timed out"
	} else {
		status, code, message = http.StatusBadGateway, "proxy_error", msg
	}
	// Node's handleError records the result without a durationMs (defaults 0).
	h.Recorder.Result(model, models.ResultArgs{StatusCode: status, Error: msg})
	h.logEntry(logstore.Entry{Time: time.Now(), Level: logstore.LevelError, Path: rawPath, Model: model, Format: string(streamFormat), Status: status, Error: msg})
	h.respondJSON(w, status, errorBody(code, message))
	return true
}

// transportMessage maps a Go transport error to the Node-style message the
// isRetryable keyword scan understands. Node surfaces connection teardown as
// "socket hang up" / "ECONNRESET" etc.; Go surfaces io.EOF / net.OpError
// wrapping syscall codes, so we translate the common shapes.
func transportMessage(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "socket hang up"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Node's e.message for these syscall-level failures is the literal
		// code ("ECONNRESET" / "ETIMEDOUT" / "ENETUNREACH"), a retryable
		// keyword. Go's OpError.Error() renders "connection reset by peer"
		// etc., which the keyword scan would miss, so map back to the code.
		switch {
		case errors.Is(opErr.Err, syscall.ECONNRESET):
			return "ECONNRESET"
		case errors.Is(opErr.Err, syscall.ETIMEDOUT):
			return "ETIMEDOUT"
		case errors.Is(opErr.Err, syscall.ENETUNREACH):
			return "ENETUNREACH"
		}
	}
	return err.Error()
}

// handleWafResponse handles a 403/405 on attempt 0: it reads the full body,
// marks the model degraded on empty output, and either retries after warmup
// (returning false so the caller continues the loop) or forwards the non-WAF
// response as-is (returning true).
func (h *Handler) handleWafResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, model string, headers http.Header, ctx context.Context, status int, respStart time.Time, streamFormat StreamFormat) bool {
	rawPath := r.URL.RequestURI()
	method := r.Method
	raw, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		// Upstream died mid-body; treat like a transport failure of this attempt.
		h.Breaker.RecordFailure()
		h.Log.Info(fmt.Sprintf("%s %s -> ERROR: %s (final)", method, rawPath, readErr))
		h.Recorder.Result(model, models.ResultArgs{StatusCode: http.StatusBadGateway, Error: "upstream_response_error"})
		h.logEntry(logstore.Entry{Time: time.Now(), Level: logstore.LevelError, Path: rawPath, Model: model, Format: string(streamFormat), Status: http.StatusBadGateway, Error: "upstream_response_error"})
		h.respondJSON(w, http.StatusBadGateway, errorBody("proxy_error", readErr.Error()))
		return true
	}

	emptyOutput := ResponseHasEmptyOutput(status, raw)
	if emptyOutput {
		h.Health.MarkDegraded(model, "empty_output")
	}
	if !IsWafBlock(status, raw) {
		// Not a WAF block page: forward the response as-is.
		h.Log.Info(fmt.Sprintf("%s %s <- %d (%db)", method, rawPath, status, len(raw)))
		h.Log.Info(fmt.Sprintf("RESPONSE BODY: %s", RedactSensitive(Truncate(string(raw), 1000))))
		dur := time.Since(respStart).Milliseconds()
		h.Recorder.Result(model, models.ResultArgs{
			StatusCode: status, DurationMs: dur,
			Error: "http_" + strconv.Itoa(status), EmptyOutput: emptyOutput,
		})
		h.logEntry(logstore.Entry{Time: time.Now(), Level: logstore.LevelError, Path: rawPath, Model: model, Format: string(streamFormat), Status: status, DurationMs: dur, Error: "http_" + strconv.Itoa(status)})
		// 4xx is "neither" for the circuit breaker (see the accounting comment
		// in doRequest): a non-WAF 403/405 is a client-side rejection, not an
		// upstream outage. Recording it as a failure would let an expired API
		// key (403s) trip the circuit and 503 all traffic for 60s+.
		h.writeFull(w, status, FilterHeaders(resp.Header), raw)
		return true
	}

	h.Log.Info(fmt.Sprintf("%s %s WAF %d detected, refreshing cookie and retrying...", method, rawPath, status))
	h.WAF.Warmup(ctx)
	if c := h.WAF.Get(); c != "" {
		headers.Set("Cookie", c)
	}
	if !sleepCtx(ctx, RetryDelay(0, h.Cfg.RetryDelayMs)) {
		return true
	}
	return false // continue the retry loop
}

// forwardNon200 buffers a non-200 upstream body and forwards it to the client
// with filtered headers (handler.mjs l.294-322).
func (h *Handler) forwardNon200(w http.ResponseWriter, r *http.Request, resp *http.Response, model string, status int, filtered http.Header, respStart time.Time, streamFormat StreamFormat) {
	rawPath := r.URL.RequestURI()
	method := r.Method
	raw, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		// Upstream died mid-body: surface as a 502 with an error body.
		// Without this, Go's ResponseWriter emits a silent 200 empty body
		// because no header was ever written, and the caller (9Router)
		// interprets it as a successful but empty completion.
		h.Log.Info(fmt.Sprintf("%s %s -> ERROR: %s (final)", method, rawPath, readErr))
		h.Recorder.Result(model, models.ResultArgs{StatusCode: http.StatusBadGateway, Error: "upstream_response_error"})
		h.logEntry(logstore.Entry{Time: time.Now(), Level: logstore.LevelError, Path: rawPath, Model: model, Format: string(streamFormat), Status: http.StatusBadGateway, Error: "upstream_response_error"})
		h.respondJSON(w, http.StatusBadGateway, errorBody("upstream_response_error", readErr.Error()))
		return
	}
	emptyOutput := ResponseHasEmptyOutput(status, raw)
	if emptyOutput {
		h.Health.MarkDegraded(model, "empty_output")
	}
	h.Log.Info(fmt.Sprintf("%s %s <- %d (%db)", method, rawPath, status, len(raw)))
	h.Log.Info(fmt.Sprintf("RESPONSE BODY: %s", RedactSensitive(Truncate(string(raw), 1000))))
	dur := time.Since(respStart).Milliseconds()
	h.Recorder.Result(model, models.ResultArgs{
		StatusCode: status, DurationMs: dur,
		Error: "http_" + strconv.Itoa(status), EmptyOutput: emptyOutput,
	})
	level, entryErr := logstore.LevelInfo, ""
	if status >= 400 {
		level, entryErr = logstore.LevelError, "http_"+strconv.Itoa(status)
	}
	h.logEntry(logstore.Entry{Time: time.Now(), Level: level, Path: rawPath, Model: model, Format: string(streamFormat), Status: status, DurationMs: dur, Error: entryErr})
	h.writeFull(w, status, filtered, raw)
}

// streamSSE runs the SSE pump and records the result (handler.mjs l.337-349).
func (h *Handler) streamSSE(w http.ResponseWriter, r *http.Request, resp *http.Response, model string, streamFormat StreamFormat, ctx context.Context) {
	rawPath := r.URL.RequestURI()
	method := r.Method
	prefix := fmt.Sprintf("%s %s <- ", method, rawPath)

	res := PumpSSE(ctx, w, resp.Body, Options{
		IsSSE:             true,
		Format:            streamFormat,
		ChunkTimeout:      h.Cfg.SSEChunkTimeout(),
		IdleTimeout:       h.Cfg.SSEIdleTimeout(),
		SlowResponse:      h.Cfg.SlowResponse(),
		StripThinkingTags: h.Cfg.StripThinkingTags,
		Log:               func(msg string) { h.Log.Info(prefix + msg) },
		LogDebug:          func(format string, args ...any) { h.Log.Debug(prefix + fmt.Sprintf(format, args...)) },
	})

	h.Recorder.Result(model, models.ResultArgs{
		StatusCode: res.StatusCode, DurationMs: res.DurationMs, Chunks: res.Chunks,
		Error: res.Error, EmptyOutput: res.EmptyOutput,
	})

	level := logstore.LevelInfo
	if res.Error != "" || res.StatusCode >= 400 {
		level = logstore.LevelError
	}
	h.logEntry(logstore.Entry{
		Time: time.Now(), Level: level, Path: rawPath, Model: model,
		Format: string(streamFormat), Status: res.StatusCode, DurationMs: res.DurationMs,
		TokensIn: res.TokensIn, TokensOut: res.TokensOut,
		CacheRead: res.CacheRead, CacheWrite: res.CacheWrite,
		Error: res.Error,
	})

	// Degrade the model on SSE-timeout / mid-span / empty output (mirror
	// onDegrade in stream.mjs). "empty_sse" arrives via EmptyOutput.
	switch res.Error {
	case "sse_idle_timeout", "upstream_ended_mid_frame":
		h.Health.MarkDegraded(model, res.Error)
	}
	if res.EmptyOutput {
		h.Health.MarkDegraded(model, "empty_sse")
	}

	// Slow-response degrade on a clean 200 (stream.mjs end-handler onDegrade).
	if res.StatusCode == http.StatusOK && res.DurationMs >= h.Cfg.SlowResponse().Milliseconds() {
		h.Health.MarkDegraded(model, fmt.Sprintf("slow_%dms", res.DurationMs))
	}
}

// copyBody copies a non-SSE 200 body to the client and records the result.
func (h *Handler) copyBody(w http.ResponseWriter, r *http.Request, resp *http.Response, model string, streamFormat StreamFormat, respStart time.Time) {
	rawPath := r.URL.RequestURI()
	copyStart := time.Now()
	// Write deadline: a stalled client that stops reading fills its TCP send
	// buffer and io.Copy blocks indefinitely. The deadline ensures the copy
	// fails and the goroutine is released rather than pinning forever.
	if dl := h.Cfg.ResponseTimeout(); dl > 0 {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(dl))
	}
	// Tee the body into a bounded capture so token usage can be parsed from
	// the response; the client still receives every byte.
	capture := &cappedBuffer{max: maxUsageCapture}
	_, _ = io.Copy(w, io.TeeReader(resp.Body, capture))
	_ = resp.Body.Close()
	dur := time.Since(copyStart).Milliseconds()
	usage := UsageFromBody(capture.buf.Bytes())
	h.Recorder.Result(model, models.ResultArgs{StatusCode: http.StatusOK, DurationMs: dur})
	h.logEntry(logstore.Entry{
		Time: time.Now(), Level: logstore.LevelInfo, Path: rawPath, Model: model,
		Format: string(streamFormat), Status: http.StatusOK, DurationMs: dur,
		TokensIn: usage.Input, TokensOut: usage.Output,
		CacheRead: usage.CacheRead, CacheWrite: usage.CacheWrite,
	})
	if dur >= h.Cfg.SlowResponse().Milliseconds() {
		h.Health.MarkDegraded(model, fmt.Sprintf("slow_%dms", dur))
	}
}

// maxUsageCapture caps how much of a non-SSE response body is buffered for
// token parsing (1 MiB); usage blocks sit at the top of the JSON payload.
const maxUsageCapture = 1 << 20

// cappedBuffer captures writes into an internal buffer, silently dropping
// bytes beyond max so a TeeReader never errors.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if room > 0 {
		if len(p) > room {
			_, _ = c.buf.Write(p[:room])
		} else {
			_, _ = c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (h *Handler) writeHead(w http.ResponseWriter, status int, headers http.Header) {
	for k, vv := range headers {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
}

func (h *Handler) writeFull(w http.ResponseWriter, status int, headers http.Header, body []byte) {
	for k, vv := range headers {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}
