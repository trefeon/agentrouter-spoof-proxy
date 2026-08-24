package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trefeon/agentrouter-spoof-proxy/internal/auth"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/config"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/models"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/resilience"
)

// testUpstream starts an httptest server as the mock upstream and returns a
// config pointing at it.
func testUpstream(t *testing.T, h http.Handler) *config.Config {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	addr := up.Listener.Addr().(*net.TCPAddr)
	cfg := &config.Config{
		ListenAddr:  "127.0.0.1",
		TargetProto: "http",
		TargetHost:  "127.0.0.1",
		TargetPort:  addr.Port,

		RequestTimeoutMs:    300000,
		ResponseTimeoutMs:   30000,
		SSEIdleTimeoutMs:    600000,
		SSEChunkTimeoutMs:   30000,
		BodyUploadTimeoutMs: 60000,
		SlowResponseMs:      30000,
		WarmupIntervalMs:    180000,
		DiscoveryIntervalMs: 600000,
		MaxRetries:          2,
		RetryDelayMs:        1,
		RetryOn5xx:          true,
		StripThinkingTags:   true,
		ModelsCSV:           "m1,m2",
		LogLevel:            "info",
	}
	return cfg
}

// testHandler builds a Handler wired to the given upstream config with fresh
// deps. Returns the handler plus the shared deps for assertions.
func testHandler(cfg *config.Config) (*Handler, *auth.Store, *resilience.Breaker, *models.Health, *models.Recorder) {
	wafStore := auth.NewStore()
	wafStore.SetTarget(cfg.TargetProto, cfg.TargetHost, cfg.TargetPort)
	breaker := resilience.NewBreaker()
	health := models.NewHealth()
	recorder := models.NewRecorder(cfg.SlowResponseMs)
	discovery := models.NewDiscovery(cfg.StaticModelIDs())
	client := &http.Client{Transport: cfg.Transport()}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(cfg, wafStore, breaker, discovery, health, recorder, client, logger)
	return h, wafStore, breaker, health, recorder
}

// proxyRequest runs ServeProxy against a recorder with the given method, path,
// body and headers.
func proxyRequest(t *testing.T, h *Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)
	return rec
}

func (a *Handler) assertActive(t *testing.T, want int64) {
	t.Helper()
	if got := a.Active.Load(); got != want {
		t.Errorf("Active = %d, want %d", got, want)
	}
}

// ── Tests ────────────────────────────────────────────────────────────────────

// 200 non-SSE passthrough + recorder stats.
func TestHandlerNonSSE200(t *testing.T) {
	cfg := testUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	h, _, _, _, recorder := testHandler(cfg)
	rec := proxyRequest(t, h, http.MethodPost, "/v1/messages", `{"model":"m1","messages":[]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("body = %q", rec.Body.String())
	}
	h.assertActive(t, 0)
	stats := recorder.Snapshot()
	if len(stats) != 1 || stats[0].Model != "m1" {
		t.Fatalf("stats = %+v", stats)
	}
	if stats[0].Requests != 1 || stats[0].Successes != 1 || stats[0].Failures != 0 {
		t.Errorf("stats = %+v, want requests=1 successes=1", stats[0])
	}
}

// SSE 200: upstream streams events then hangs up; the pump injects an abnormal
// finish (error frame + EOM) and records upstream_closed.
func TestHandlerSSEUpstreamClosed(t *testing.T) {
	cfg := testUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: hi\n\n")
		fl.Flush()
		// Abruptly close the connection mid-stream (truncated body).
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	h, _, breaker, _, recorder := testHandler(cfg)
	rec := proxyRequest(t, h, http.MethodPost, "/v1/chat/completions", `{"model":"m1","stream":true,"messages":[]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers already sent)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: hi\n\n") {
		t.Errorf("missing streamed event: %q", body)
	}
	if !strings.Contains(body, `"type":"proxy_error"`) {
		t.Errorf("missing error frame: %q", body)
	}
	if !strings.HasSuffix(body, "\ndata: [DONE]\n\n") {
		t.Errorf("missing OpenAI EOM tail: %q", body)
	}
	stats := recorder.Snapshot()
	if len(stats) != 1 || stats[0].LastError != "upstream_closed" {
		t.Fatalf("stats = %+v, want error upstream_closed", stats)
	}
	if stats[0].Failures != 1 || stats[0].UpstreamErrors != 1 {
		t.Errorf("stats = %+v", stats[0])
	}
	if breaker.ConsecutiveFails() != 0 {
		t.Errorf("breaker consecutive fails = %d, want 0 (200 response)", breaker.ConsecutiveFails())
	}
}

// WAF 403 block on first attempt → warmup + cookie refresh + retry succeeds.
func TestHandlerWafRetry(t *testing.T) {
	var mu atomic.Int64
	var secondCookie string
	cfg := testUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := mu.Add(1)
		if n == 1 && r.URL.Path == "/v1/messages" {
			// WAF block page with a rotated cookie.
			w.Header().Set("Set-Cookie", "acw_tc=first; Path=/")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<html>alicdn block_message waf.js</html>`)
			return
		}
		if n == 2 && r.URL.Path == "/" {
			// Warmup GET / returns a fresh cookie.
			w.Header().Set("Set-Cookie", "acw_tc=second; Path=/")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
			return
		}
		if n >= 3 && r.URL.Path == "/v1/messages" {
			secondCookie = r.Header.Get("Cookie")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	h, wafStore, _, _, _ := testHandler(cfg)
	rec := proxyRequest(t, h, http.MethodPost, "/v1/messages", `{"model":"m1","messages":[]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after WAF retry", rec.Code)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("body = %q", rec.Body.String())
	}
	if mu.Load() < 3 {
		t.Errorf("upstream calls = %d, want >= 3 (block + warmup + retry)", mu.Load())
	}
	if secondCookie == "" || !strings.Contains(secondCookie, "acw_tc=second") {
		t.Errorf("retry did not carry refreshed WAF cookie, got %q", secondCookie)
	}
	if !strings.Contains(wafStore.Get(), "acw_tc=second") {
		t.Errorf("WAF store did not capture refreshed cookie: %q", wafStore.Get())
	}
}

// 5xx with RetryOn5xx=true retries until 200.
func TestHandler5xxRetry(t *testing.T) {
	var mu atomic.Int64
	cfg := testUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mu.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	h, _, breaker, health, recorder := testHandler(cfg)
	rec := proxyRequest(t, h, http.MethodPost, "/v1/messages", `{"model":"m1","messages":[]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if mu.Load() != 2 {
		t.Errorf("upstream calls = %d, want 2", mu.Load())
	}
	if breaker.ConsecutiveFails() != 0 {
		t.Errorf("breaker fails = %d, want 0 (success reset)", breaker.ConsecutiveFails())
	}
	if !health.IsHealthy("m1") {
		t.Error("model must stay healthy after retry succeeded")
	}
	stats := recorder.Snapshot()
	if len(stats) != 1 || stats[0].Successes != 1 {
		t.Errorf("stats = %+v", stats)
	}
}

// 5xx with RetryOn5xx=false passes through once (no retry).
func TestHandler5xxNoRetry(t *testing.T) {
	var mu atomic.Int64
	cfg := testUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"up"}`)
	}))
	cfg.RetryOn5xx = false
	h, _, breaker, health, recorder := testHandler(cfg)
	rec := proxyRequest(t, h, http.MethodPost, "/v1/messages", `{"model":"m1","messages":[]}`, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if mu.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", mu.Load())
	}
	if breaker.ConsecutiveFails() != 1 {
		t.Errorf("breaker fails = %d, want 1", breaker.ConsecutiveFails())
	}
	if health.IsHealthy("m1") {
		t.Error("model must be marked failed after final 5xx")
	}
	stats := recorder.Snapshot()
	if len(stats) != 1 || stats[0].Failures != 1 || stats[0].UpstreamErrors != 1 {
		t.Errorf("stats = %+v", stats)
	}
}

// Transport error (upstream closes immediately) retries then 502.
func TestHandlerTransportErrorRetries(t *testing.T) {
	var mu atomic.Int64
	cfg := testUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Add(1)
		// Hijack and close without writing a response → transport EOF.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	cfg.RetryDelayMs = 1
	h, _, breaker, health, recorder := testHandler(cfg)
	rec := proxyRequest(t, h, http.MethodPost, "/v1/messages", `{"model":"m1","messages":[]}`, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if mu.Load() < 2 {
		t.Errorf("upstream calls = %d, want retries (>= 2)", mu.Load())
	}
	// Only the final failure counts toward the circuit (retries don't).
	if breaker.ConsecutiveFails() != 1 {
		t.Errorf("breaker fails = %d, want 1 (final attempt only)", breaker.ConsecutiveFails())
	}
	if health.IsHealthy("m1") {
		t.Error("model must be marked failed after transport error")
	}
	stats := recorder.Snapshot()
	if len(stats) != 1 || stats[0].Failures != 1 {
		t.Errorf("stats = %+v", stats)
	}
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	errObj, _ := payload["error"].(map[string]any)
	if errObj["code"] != "proxy_error" {
		t.Errorf("error code = %v, want proxy_error", errObj["code"])
	}
}

// Transport timeout (upstream that never responds) → 504 timeout.
func TestHandlerTransportTimeout(t *testing.T) {
	// A listener that accepts but never responds.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Never read or write: the client request hangs until timeout.
			go func(c net.Conn) { time.Sleep(5 * time.Second); _ = c.Close() }(conn)
		}
	}()

	cfg := &config.Config{
		ListenAddr: "127.0.0.1", TargetProto: "http",
		TargetHost: "127.0.0.1", TargetPort: ln.Addr().(*net.TCPAddr).Port,
		RequestTimeoutMs: 300000, ResponseTimeoutMs: 100, // 100ms adaptive
		SSEIdleTimeoutMs: 600000, SSEChunkTimeoutMs: 30000,
		BodyUploadTimeoutMs: 60000, SlowResponseMs: 30000,
		WarmupIntervalMs: 180000, DiscoveryIntervalMs: 600000,
		MaxRetries: 0, RetryDelayMs: 1, RetryOn5xx: true,
		StripThinkingTags: true, ModelsCSV: "m1", LogLevel: "info",
	}
	h, _, _, _, recorder := testHandler(cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"m1"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeProxy(rec, req)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (took %s)", rec.Code, time.Since(start))
	}
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	errObj, _ := payload["error"].(map[string]any)
	if errObj["code"] != "timeout" {
		t.Errorf("error code = %v, want timeout", errObj["code"])
	}
	stats := recorder.Snapshot()
	if len(stats) != 1 || stats[0].LastError != "timeout" {
		t.Errorf("stats = %+v", stats)
	}
}

// Content-Length > 20MB → 413 immediately, upstream never called.
func TestHandlerOversizedBody(t *testing.T) {
	var mu atomic.Int64
	cfg := testUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	h, _, _, _, _ := testHandler(cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.ContentLength = MaxBodySize + 1
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if mu.Load() != 0 {
		t.Errorf("upstream called %d times, want 0", mu.Load())
	}
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	errObj, _ := payload["error"].(map[string]any)
	if errObj["code"] != "payload_too_large" {
		t.Errorf("error code = %v", errObj["code"])
	}
	h.assertActive(t, 0)
}

// 429 → model exhausted (locked), breaker untouched.
func TestHandler429RateLimit(t *testing.T) {
	var mu atomic.Int64
	cfg := testUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	h, _, breaker, health, recorder := testHandler(cfg)
	rec := proxyRequest(t, h, http.MethodPost, "/v1/messages", `{"model":"m1","messages":[]}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if health.IsHealthy("m1") {
		t.Error("model must be locked out after 429")
	}
	if breaker.ConsecutiveFails() != 0 {
		t.Errorf("breaker fails = %d, want 0 (429 never opens circuit)", breaker.ConsecutiveFails())
	}
	stats := recorder.Snapshot()
	if len(stats) != 1 || stats[0].RateLimits != 1 || stats[0].Failures != 1 {
		t.Errorf("stats = %+v", stats)
	}
}

// Circuit open → 503 before any upstream work.
func TestHandlerCircuitOpen(t *testing.T) {
	var mu atomic.Int64
	cfg := testUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	h, _, breaker, _, _ := testHandler(cfg)
	for i := 0; i < 5; i++ {
		breaker.RecordFailure()
	}
	rec := proxyRequest(t, h, http.MethodPost, "/v1/messages", `{"model":"m1"}`, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if mu.Load() != 0 {
		t.Errorf("upstream called %d times, want 0", mu.Load())
	}
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	errObj, _ := payload["error"].(map[string]any)
	if errObj["code"] != "circuit_open" {
		t.Errorf("error code = %v", errObj["code"])
	}
	h.assertActive(t, 0)
}

// Client Authorization + x-api-key forwarded; other client headers are not.
func TestHandlerHeaderForwarding(t *testing.T) {
	var gotAuth, gotAPIKey, gotContentType, gotAnthropicVersion string
	var gotCustom string
	cfg := testUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotContentType = r.Header.Get("Content-Type")
		gotAnthropicVersion = r.Header.Get("Anthropic-Version")
		gotCustom = r.Header.Get("X-Custom-Secret")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	h, _, _, _, _ := testHandler(cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"m1"}`)))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("x-api-key", "client-key")
	req.Header.Set("X-Custom-Secret", "do-not-forward")
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.ServeProxy(rec, req)

	if gotAuth != "Bearer client-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAPIKey != "client-key" {
		t.Errorf("x-api-key = %q", gotAPIKey)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (client content-type must not pass through)", gotContentType)
	}
	if gotAnthropicVersion == "" {
		t.Error("Anthropic-Version spoof header missing on /v1/messages")
	}
	if gotCustom != "" {
		t.Errorf("X-Custom-Secret leaked upstream: %q", gotCustom)
	}
}

// Non-200 passthrough forwards status + body.
func TestHandlerNon200Passthrough(t *testing.T) {
	cfg := testUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad input"}`)
	}))
	h, _, breaker, _, recorder := testHandler(cfg)
	rec := proxyRequest(t, h, http.MethodPost, "/v1/messages", `{"model":"m1","messages":[]}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if rec.Body.String() != `{"error":"bad input"}` {
		t.Errorf("body = %q", rec.Body.String())
	}
	if breaker.ConsecutiveFails() != 0 {
		t.Errorf("breaker fails = %d, want 0 (4xx neither fails nor resets)", breaker.ConsecutiveFails())
	}
	stats := recorder.Snapshot()
	if len(stats) != 1 || stats[0].Failures != 1 || stats[0].LastError != "http_400" {
		t.Errorf("stats = %+v", stats)
	}
}

// A non-WAF 403/405 must NOT count as a circuit-breaker failure: 4xx is a
// client-side rejection (e.g. expired API key), not an upstream outage.
// Regression: handleWafResponse used to RecordFailure on any non-WAF
// 403/405, so five such responses opened the circuit and 503'd all traffic.
func TestHandlerNonWaf4xxDoesNotTripBreaker(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusMethodNotAllowed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cfg := testUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"not authorized"}`) // no WAF markers
			}))
			h, _, breaker, _, _ := testHandler(cfg)
			for i := 0; i < 6; i++ {
				rec := proxyRequest(t, h, http.MethodPost, "/v1/messages", `{"model":"m1","messages":[]}`, nil)
				if rec.Code != status {
					t.Fatalf("request %d: status = %d, want %d", i, rec.Code, status)
				}
				if breaker.IsOpen() {
					t.Fatalf("request %d: circuit opened on a non-WAF %d", i, status)
				}
			}
			if fails := breaker.ConsecutiveFails(); fails != 0 {
				t.Errorf("breaker fails = %d, want 0 (non-WAF 4xx neither fails nor resets)", fails)
			}
		})
	}
}

// Client context cancellation aborts the request without a failure response.
func TestHandlerClientDisconnect(t *testing.T) {
	// A body that blocks reading until released (simulates a slow/hanging
	// upstream that never sends a response).
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		close(release)
		up.Close()
	})
	addr := up.Listener.Addr().(*net.TCPAddr)
	cfg := &config.Config{
		ListenAddr: "127.0.0.1", TargetProto: "http",
		TargetHost: "127.0.0.1", TargetPort: addr.Port,
		RequestTimeoutMs: 300000, ResponseTimeoutMs: 30000,
		SSEIdleTimeoutMs: 600000, SSEChunkTimeoutMs: 30000,
		BodyUploadTimeoutMs: 60000, SlowResponseMs: 30000,
		WarmupIntervalMs: 180000, DiscoveryIntervalMs: 600000,
		MaxRetries: 0, RetryDelayMs: 1, RetryOn5xx: true,
		StripThinkingTags: true, ModelsCSV: "m1", LogLevel: "info",
	}
	h, _, breaker, _, _ := testHandler(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"m1"}`)))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeProxy(rec, req)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeProxy did not return after client disconnect")
	}
	// No failure counted: a client abort is not an upstream outage.
	if breaker.ConsecutiveFails() != 0 {
		t.Errorf("breaker fails = %d, want 0", breaker.ConsecutiveFails())
	}
	if rec.Code != http.StatusOK && rec.Code != 0 {
		t.Errorf("unexpected response status %d written for disconnected client", rec.Code)
	}
}

