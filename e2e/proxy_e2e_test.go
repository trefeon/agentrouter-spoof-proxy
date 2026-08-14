// Package e2e runs the ported E2E suite (tests/proxy.test.mjs) against an
// in-process proxy + scripted mock upstream. No child processes: each test
// builds a fresh env (mock + proxy) via newEnv and shuts it down via t.Cleanup.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/trefeon/agentrouter-spoof-proxy/internal/config"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/server"
	"github.com/trefeon/agentrouter-spoof-proxy/testutil/mockupstream"
)

// Env is one proxy + mock pair for a single test.
type Env struct {
	Mock    *mockupstream.MockUpstream
	Server  *server.Server
	BaseURL string
	Addr    string // host:port of the proxy listener
}

// defaultConfig mirrors the Node suite's main before() env (proxy.test.mjs
// l.144-157): fast timeouts, 1 retry, no 5xx retry, long scheduler intervals.
func defaultConfig(mock *mockupstream.MockUpstream) *config.Config {
	return &config.Config{
		ListenAddr: "127.0.0.1", ListenPort: 0,
		TargetProto: "http", TargetHost: "127.0.0.1", TargetPort: mock.Port(),
		RequestTimeoutMs: 5000, ResponseTimeoutMs: 30000,
		SSEIdleTimeoutMs: 600000, SSEChunkTimeoutMs: 30000,
		BodyUploadTimeoutMs: 60000, SlowResponseMs: 30000,
		WarmupIntervalMs: 600000, DiscoveryIntervalMs: 600000,
		MaxRetries: 1, RetryDelayMs: 10, RetryOn5xx: false,
		StripThinkingTags: true, ModelsCSV: "claude-opus-4-8,gpt-5.6-sol",
		LogLevel: "info",
	}
}

// newEnv builds a fresh mock + proxy, serves it on 127.0.0.1:0, and registers
// teardown. mutate can override config fields before the server starts.
func newEnv(t *testing.T, mutate func(*config.Config)) *Env {
	t.Helper()
	mock, err := mockupstream.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mock.Close() })

	cfg := defaultConfig(mock)
	if mutate != nil {
		mutate(cfg)
	}

	srv := server.New(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	addr := ln.Addr().String()
	return &Env{
		Mock:    mock,
		Server:  srv,
		BaseURL: "http://" + addr,
		Addr:    addr,
	}
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 30 * time.Second}

// proxyHeaders mirrors tests/proxy.test.mjs proxyHeaders().
func proxyHeaders() map[string]string {
	return map[string]string{
		"Content-Type":      "application/json",
		"Authorization":     "Bearer sk_test",
		"Anthropic-Version": "2023-06-01",
	}
}

// chatBody mirrors chatBody().
func chatBody(model string) string {
	if model == "" {
		model = "claude-opus-4-8"
	}
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":10}`, model)
}

// request sends a request through the proxy and returns the response.
func request(t *testing.T, env *Env, method, path string, body string, headers map[string]string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, env.BaseURL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// readBody reads a response body fully and closes it.
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// postBody performs a POST and returns (status, body).
func postBody(t *testing.T, env *Env, path, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	resp := request(t, env, http.MethodPost, path, body, headers)
	return resp.StatusCode, readBody(t, resp)
}

// getJSON performs a GET and decodes the JSON body.
func getJSON(t *testing.T, env *Env, path string) (int, map[string]any) {
	t.Helper()
	resp := request(t, env, http.MethodGet, path, "", nil)
	b := readBody(t, resp)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("GET %s: bad JSON %q: %v", path, b, err)
	}
	return resp.StatusCode, m
}

// splitSSE mirrors the Node collectSse event splitter: blocks separated by
// blank lines, trimmed, empties dropped.
func splitSSE(raw string) []string {
	var events []string
	for _, part := range strings.Split(raw, "\n\n") {
		if t := strings.TrimSpace(part); t != "" {
			events = append(events, t)
		}
	}
	return events
}

// stream performs a POST, reads the full body (SSE), and returns status, raw
// body and split events.
func stream(t *testing.T, env *Env, path, body string, headers map[string]string) (int, string, []string) {
	t.Helper()
	resp := request(t, env, http.MethodPost, path, body, headers)
	raw := string(readBody(t, resp))
	return resp.StatusCode, raw, splitSSE(raw)
}

// activeStreams polls /health until activeStreams equals target.
func waitActiveStreams(t *testing.T, env *Env, target int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, h := getJSON(t, env, "/health")
		if v, ok := h["activeStreams"].(float64); ok && int64(v) == target {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, h := getJSON(t, env, "/health")
	t.Fatalf("activeStreams never reached %d, got %v", target, h["activeStreams"])
}

// health returns the decoded /health payload.
func health(t *testing.T, env *Env) map[string]any {
	t.Helper()
	_, h := getJSON(t, env, "/health")
	return h
}

// lastPost returns the mock's most recent POST request.
func lastPost(t *testing.T, env *Env) mockupstream.Received {
	t.Helper()
	r := env.Mock.LastPost()
	if r == nil {
		t.Fatal("no POST request recorded by the mock upstream")
	}
	return *r
}

// bodyMap extracts the parsed JSON body of a recorded request.
func bodyMap(r mockupstream.Received) map[string]any {
	m, _ := r.Body.(map[string]any)
	return m
}

// rawConn sends a raw HTTP request over TCP (for 413 tests that need to
// control Content-Length framing) and returns the status + body.
func rawConn(t *testing.T, env *Env, requestStr string) (int, []byte) {
	t.Helper()
	conn, err := net.Dial("tcp", env.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(conn, requestStr); err != nil {
		t.Fatalf("raw write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("raw read response: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("raw read body: %v", err)
	}
	return resp.StatusCode, b
}

// ── Helper for the client-disconnect test ─────────────────────────────────────

// dialThenClose opens a POST connection, reads a little, then closes it
// abruptly (simulates a client aborting mid-stream).
func dialThenClose(t *testing.T, env *Env, path, body string) {
	t.Helper()
	conn, err := net.Dial("tcp", env.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		path, env.Addr, len(body), body)
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Read a chunk (the SSE starts streaming), then hang up.
	buf := make([]byte, 512)
	_, _ = conn.Read(buf)
	_ = conn.Close()
}

// ── The tests ─────────────────────────────────────────────────────────────────

func TestHealthPayload(t *testing.T) {
	env := newEnv(t, nil)
	status, body := getJSON(t, env, "/health")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if _, ok := body["activeStreams"].(float64); !ok {
		t.Errorf("activeStreams not a number: %v", body["activeStreams"])
	}
	if _, ok := body["wafCookie"].(bool); !ok {
		t.Errorf("wafCookie not a bool: %v", body["wafCookie"])
	}
	if _, ok := body["circuitOpen"].(bool); !ok {
		t.Errorf("circuitOpen not a bool: %v", body["circuitOpen"])
	}
	if _, ok := body["upstream"].(string); !ok {
		t.Errorf("upstream not a string: %v", body["upstream"])
	}
	if n, ok := body["staticModels"].(float64); !ok || n < 1 {
		t.Errorf("staticModels = %v, want >= 1", body["staticModels"])
	}
	if _, ok := body["modelHealth"].([]any); !ok {
		t.Errorf("modelHealth not an array: %v", body["modelHealth"])
	}
	// /api/health alias.
	status2, body2 := getJSON(t, env, "/api/health")
	if status2 != http.StatusOK || body2["ok"] != true {
		t.Errorf("/api/health = %d %v, want 200 ok", status2, body2["ok"])
	}
}

func TestActiveStreamsZeroAtStartup(t *testing.T) {
	env := newEnv(t, nil)
	body := health(t, env)
	if v, ok := body["activeStreams"].(float64); !ok || v != 0 {
		t.Errorf("activeStreams = %v, want 0 at startup", body["activeStreams"])
	}
}

func TestStaticModelList(t *testing.T) {
	env := newEnv(t, nil)
	status, body := getJSON(t, env, "/v1/models")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	data, ok := body["data"].([]any)
	if !ok || len(data) < 1 {
		t.Fatalf("data = %v, want non-empty array", body["data"])
	}
	found := false
	for _, m := range data {
		mm, _ := m.(map[string]any)
		if mm["id"] == "claude-opus-4-8" {
			found = true
		}
	}
	if !found {
		t.Errorf("static model claude-opus-4-8 missing from %v", data)
	}
}

func TestModelSuccessMetricsNoPromptContent(t *testing.T) {
	env := newEnv(t, nil)
	body := fmt.Sprintf(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"super secret prompt text"}],"stream":true,"max_tokens":10}`)
	status, _, _ := stream(t, env, "/v1/messages", body, proxyHeaders())
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	h := health(t, env)
	stats, _ := h["modelHealth"].([]any)
	var stat map[string]any
	for _, s := range stats {
		if sm, _ := s.(map[string]any); sm["model"] == "claude-opus-4-8" {
			stat = sm
		}
	}
	if stat == nil {
		t.Fatalf("no stats for claude-opus-4-8 in %v", stats)
	}
	if succ, _ := stat["successes"].(float64); succ < 1 {
		t.Errorf("successes = %v, want >= 1", stat["successes"])
	}
	raw, _ := json.Marshal(stat)
	if strings.Contains(string(raw), "super secret prompt text") {
		t.Errorf("model health stats leaked prompt content: %s", raw)
	}
}

func TestRewriteMessagesToV1Messages(t *testing.T) {
	env := newEnv(t, nil)
	status, _, _ := stream(t, env, "/messages", chatBody(""), proxyHeaders())
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	r := lastPost(t, env)
	if !strings.HasPrefix(r.URL, "/v1/messages") {
		t.Errorf("upstream URL = %q, want /v1/messages prefix", r.URL)
	}
}

func TestSpoofHeadersInjected(t *testing.T) {
	env := newEnv(t, nil)
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	r := lastPost(t, env)
	if !strings.Contains(r.Headers.Get("User-Agent"), "claude-cli") {
		t.Errorf("User-Agent not spoofed: %q", r.Headers.Get("User-Agent"))
	}
	if r.Headers.Get("Anthropic-Version") == "" {
		t.Error("Anthropic-Version missing")
	}
	if r.Headers.Get("X-Stainless-Runtime") == "" {
		t.Error("X-Stainless-Runtime missing")
	}
	if r.Headers.Get("Anthropic-Dangerous-Direct-Browser-Access") != "true" {
		t.Error("Anthropic-Dangerous-Direct-Browser-Access missing")
	}
}

func TestAuthorizationForwarded(t *testing.T) {
	env := newEnv(t, nil)
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	r := lastPost(t, env)
	if got := r.Headers.Get("Authorization"); got != "Bearer sk_test" {
		t.Errorf("Authorization = %q, want Bearer sk_test", got)
	}
}

func TestGenericSpoofHeadersNoAnthropic(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioOpenaiStream)
	_, _, _ = stream(t, env, "/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, proxyHeaders())
	r := lastPost(t, env)
	if !strings.Contains(r.Headers.Get("User-Agent"), "claude-cli") {
		t.Errorf("User-Agent not spoofed: %q", r.Headers.Get("User-Agent"))
	}
	if r.Headers.Get("X-Stainless-Runtime") == "" {
		t.Error("X-Stainless-Runtime missing")
	}
	if r.Headers.Get("Anthropic-Version") != "" {
		t.Errorf("Anthropic-Version must NOT be passed to OpenAI route, got %q", r.Headers.Get("Anthropic-Version"))
	}
	if r.Headers.Get("Anthropic-Beta") != "" {
		t.Errorf("Anthropic-Beta must NOT be present on OpenAI route")
	}
	if r.Headers.Get("Anthropic-Dangerous-Direct-Browser-Access") != "" {
		t.Errorf("Anthropic dangerous header must NOT be present on OpenAI route")
	}
}

func TestWafCookieForwarded(t *testing.T) {
	env := newEnv(t, nil)
	// Wait for the warmup scheduler to acquire the acw_tc cookie.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h := health(t, env); h["wafCookie"] == true {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	r := lastPost(t, env)
	cookie := r.Headers.Get("Cookie")
	if cookie == "" {
		t.Fatal("Cookie header should be present")
	}
	if !strings.Contains(cookie, "acw_tc") {
		t.Errorf("Cookie should contain acw_tc, got %q", cookie)
	}
}

func TestClientAnthropicVersionIgnored(t *testing.T) {
	env := newEnv(t, nil)
	hdr := proxyHeaders()
	hdr["Anthropic-Version"] = "2024-10-01" // client override attempt
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), hdr)
	r := lastPost(t, env)
	if got := r.Headers.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q, want spoofed 2023-06-01 to win", got)
	}
}

func TestWafCookieCaptureOnApiResponse(t *testing.T) {
	env := newEnv(t, nil)
	// Wait for the warmup cookie first (acw_tc).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h := health(t, env); h["wafCookie"] == true {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// First request rotates cdn_sec_tc on the SSE response headers.
	env.Mock.SetScenario(mockupstream.ScenarioCookieRefresh)
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	// Second request must carry both warmup + traffic cookies.
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	r := lastPost(t, env)
	cookie := r.Headers.Get("Cookie")
	if !strings.Contains(cookie, "acw_tc=test_mock_cookie") {
		t.Errorf("warmup cookie must be preserved, got %q", cookie)
	}
	if !strings.Contains(cookie, "cdn_sec_tc=traffic_cookie_1") {
		t.Errorf("traffic cookie must be captured from API response, got %q", cookie)
	}
}

func TestSseChunksForwarded(t *testing.T) {
	env := newEnv(t, nil)
	status, raw, events := stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(events) < 1 {
		t.Fatalf("no SSE events in %q", raw)
	}
	found := false
	for _, e := range events {
		if strings.Contains(e, "message_stop") {
			found = true
		}
	}
	if !found {
		t.Errorf("should contain message_stop, got %v", events)
	}
}

func TestActiveStreamsReturnsToZeroAfterStream(t *testing.T) {
	env := newEnv(t, nil)
	before := health(t, env)["activeStreams"].(float64)
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	waitActiveStreams(t, env, int64(before))
}

func TestNonWaf405Forwarded(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioNonWAF405)
	status, body := postBody(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", status)
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	errObj, _ := parsed["error"].(map[string]any)
	if _, ok := errObj["message"]; !ok {
		t.Errorf("body should carry error.message, got %s", body)
	}
}

func TestError500ForwardsWithoutRetry(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioError500)
	status, _ := postBody(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusInternalServerError && status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 or 500", status)
	}
}

func TestError503Forwarded(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioError503)
	status, _ := postBody(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusServiceUnavailable && status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 503 or 502", status)
	}
}

func TestWaf405RetrySucceeds(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioWAF405)
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if n := env.Mock.PostCount(); n < 2 {
		t.Errorf("should retry after WAF block, got %d upstream POSTs", n)
	}
}

func TestWaf403RetrySucceeds(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioWAF403)
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if n := env.Mock.PostCount(); n < 2 {
		t.Errorf("should retry after 403 WAF block, got %d upstream POSTs", n)
	}
}

func TestThreeParallelSSEStreams(t *testing.T) {
	env := newEnv(t, nil)
	before := health(t, env)["activeStreams"].(float64)

	results := make(chan []string, 3)
	for i := 0; i < 3; i++ {
		go func() {
			_, _, events := stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
			results <- events
		}()
	}
	for i := 0; i < 3; i++ {
		events := <-results
		ok := false
		for _, e := range events {
			if strings.Contains(e, "message_stop") {
				ok = true
			}
		}
		if !ok {
			t.Errorf("a parallel stream did not complete with message_stop: %v", events)
		}
	}
	waitActiveStreams(t, env, int64(before))
}

func TestSequentialRequestsNoLeak(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioSuccessStreaming)
	for i := 0; i < 3; i++ {
		_, _, _ = stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	}
	h := health(t, env)
	if v, ok := h["activeStreams"].(float64); !ok || v != 0 {
		t.Errorf("activeStreams = %v, want 0 after sequential requests", h["activeStreams"])
	}
}

func TestSseAntiBufferingHeaders(t *testing.T) {
	env := newEnv(t, nil)
	resp := request(t, env, http.MethodPost, "/v1/messages", chatBody(""), proxyHeaders())
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
}

func TestAnthropicStreamEndsWithMessageStop(t *testing.T) {
	env := newEnv(t, nil)
	status, _, events := stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	found := false
	for _, e := range events {
		if strings.Contains(e, "event: message_stop") {
			found = true
		}
	}
	if !found {
		t.Errorf("anthropic terminal event missing, got %v", events)
	}
}

func TestOpenAIStreamEndsWithDone(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioOpenaiStream)
	status, raw, _ := stream(t, env, "/v1/chat/completions", chatBody("gpt-5.6-sol"), proxyHeaders())
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(raw, "chatcmpl-9router-test") {
		t.Errorf("OpenAI chunk ids not forwarded: %q", raw)
	}
	if !strings.Contains(raw, "data: [DONE]") {
		t.Errorf("OpenAI stream must end with [DONE]: %q", raw)
	}
	if strings.Contains(raw, "event: message_stop") {
		t.Errorf("no Anthropic EOM may leak into OpenAI stream: %q", raw)
	}
	if strings.Contains(raw, "data: {}") {
		t.Errorf("no empty {} chunk corruption: %q", raw)
	}
}

func TestAnthropicThinkingStreamClean(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioThinkingStream)
	status, raw, events := stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "thinking_delta") {
		t.Errorf("thinking delta not forwarded: %q", raw)
	}
	if !strings.Contains(joined, "text_delta") {
		t.Errorf("text delta not forwarded: %q", raw)
	}
	if !strings.Contains(joined, "event: message_stop") {
		t.Errorf("message_stop must terminate: %q", raw)
	}
	time.Sleep(50 * time.Millisecond)
	h := health(t, env)
	stats, _ := h["modelHealth"].([]any)
	for _, s := range stats {
		if sm, _ := s.(map[string]any); sm["model"] == "claude-opus-4-8" {
			if succ, _ := sm["successes"].(float64); succ < 1 {
				t.Errorf("thinking stream must count as a success, got %v", sm["successes"])
			}
		}
	}
}

func TestClientDisconnectMidStream(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioSlowStream)
	before := health(t, env)["activeStreams"].(float64)
	dialThenClose(t, env, "/v1/messages", chatBody(""))
	waitActiveStreams(t, env, int64(before))
}

func TestHopByHopNotForwarded(t *testing.T) {
	env := newEnv(t, nil)
	hdr := proxyHeaders()
	hdr["Connection"] = "close"
	hdr["X-Custom-Hop"] = "should-not-forward"
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), hdr)
	r := lastPost(t, env)
	if got := r.Headers.Get("X-Custom-Hop"); got != "" {
		t.Errorf("custom hop-by-hop header forwarded upstream: %q", got)
	}
	if got := r.Headers.Get("Authorization"); got != "Bearer sk_test" {
		t.Errorf("Authorization = %q, want forwarded", got)
	}
}

// ── Error handling ────────────────────────────────────────────────────────────

func TestUpstreamConnectionError502(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioConnectionError)
	status, _ := postBody(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
}

func TestUpstreamTimeout504(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.ResponseTimeoutMs = 1000
	})
	env.Mock.SetScenario(mockupstream.ScenarioTimeout)
	status, body := postBody(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body %s)", status, body)
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	errObj, _ := parsed["error"].(map[string]any)
	if errObj["code"] != "timeout" {
		t.Errorf("error code = %v, want timeout", errObj["code"])
	}
}

func TestPrematureUpstreamCloseInjectsEOM(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioPartialClose)
	_, raw, _ := stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if !strings.Contains(raw, "message_stop") {
		t.Errorf("should inject synthetic message_stop after premature close, got %q", raw)
	}
	if !strings.Contains(raw, "proxy_error") {
		t.Errorf("error frame should precede the injected EOM, got %q", raw)
	}
}

func TestActiveStreamsZeroAfterConnectionError(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioConnectionError)
	before := health(t, env)["activeStreams"].(float64)
	_, _ = postBody(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	waitActiveStreams(t, env, int64(before))
}

func TestActiveStreamsZeroAfterPartialClose(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioPartialClose)
	before := health(t, env)["activeStreams"].(float64)
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	waitActiveStreams(t, env, int64(before))
}

// ── Adaptive response timeout ─────────────────────────────────────────────────

func TestResponseTimeoutWithheldHeaders504(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.ResponseTimeoutMs = 600
		c.RequestTimeoutMs = 30000
		c.MaxRetries = 0
	})
	env.Mock.SetScenario(mockupstream.ScenarioNoResponseHeaders)
	start := time.Now()
	status, body := postBody(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	elapsed := time.Since(start)
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body %s)", status, body)
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	errObj, _ := parsed["error"].(map[string]any)
	if errObj["code"] != "timeout" {
		t.Errorf("error code = %v, want timeout", errObj["code"])
	}
	if elapsed > 10*time.Second {
		t.Errorf("should time out via RESPONSE_TIMEOUT_MS, took %s", elapsed)
	}
}

func TestResponseTimeoutNoStreamLeak(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.ResponseTimeoutMs = 600
		c.RequestTimeoutMs = 30000
		c.MaxRetries = 0
	})
	env.Mock.SetScenario(mockupstream.ScenarioNoResponseHeaders)
	before := health(t, env)["activeStreams"].(float64)
	status, _ := postBody(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", status)
	}
	waitActiveStreams(t, env, int64(before))
}

func TestResponseTimeoutRetries(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.ResponseTimeoutMs = 500
		c.RequestTimeoutMs = 30000
		c.MaxRetries = 1
	})
	env.Mock.SetScenario(mockupstream.ScenarioNoResponseHeaders)
	status, _ := postBody(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (exhausted retries)", status)
	}
	if n := env.Mock.PostCount(); n < 2 {
		t.Errorf("response timeout should retry, got %d upstream attempts", n)
	}
}

// ── Circuit breaker accounting ────────────────────────────────────────────────

func cbEnv(t *testing.T) *Env {
	return newEnv(t, func(c *config.Config) {
		c.RequestTimeoutMs = 5000
		c.MaxRetries = 0
		c.RetryDelayMs = 10
	})
}

func breakerFails(t *testing.T, env *Env) float64 {
	t.Helper()
	h := health(t, env)
	f, _ := h["consecutiveFails"].(float64)
	return f
}

func TestFinal5xxCountsAsCircuitFailure(t *testing.T) {
	env := cbEnv(t)
	_, _, _ = stream(t, env, "/v1/messages", chatBody("cb-reset-5xx"), proxyHeaders())
	if f := breakerFails(t, env); f != 0 {
		t.Fatalf("success should zero the counter, got %v", f)
	}
	env.Mock.SetScenario(mockupstream.ScenarioError500)
	status, _ := postBody(t, env, "/v1/messages", chatBody("cb-500"), proxyHeaders())
	if status != http.StatusInternalServerError {
		t.Fatalf("final 5xx must be forwarded, got %d", status)
	}
	if f := breakerFails(t, env); f != 1 {
		t.Errorf("a final 5xx must increment consecutiveFails, got %v", f)
	}
}

func TestCircuitOpensAfterFive5xx(t *testing.T) {
	env := cbEnv(t)
	_, _, _ = stream(t, env, "/v1/messages", chatBody("cb-open-reset"), proxyHeaders())
	if f := breakerFails(t, env); f != 0 {
		t.Fatalf("breaker not reset, got %v", f)
	}
	env.Mock.SetScenario(mockupstream.ScenarioError500)
	for i := 0; i < 5; i++ {
		_, _ = postBody(t, env, "/v1/messages", chatBody(fmt.Sprintf("cb-open-%d", i)), proxyHeaders())
	}
	h := health(t, env)
	if f, _ := h["consecutiveFails"].(float64); f < 5 {
		t.Errorf("expected >= 5 fails, got %v", h["consecutiveFails"])
	}
	if open, _ := h["circuitOpen"].(bool); !open {
		t.Error("5 consecutive final 5xx must open the circuit")
	}
	status, _ := postBody(t, env, "/v1/messages", chatBody("cb-open-blocked"), proxyHeaders())
	if status != http.StatusServiceUnavailable {
		t.Errorf("open circuit returns %d, want 503", status)
	}
}

func TestSuccessResetsBreaker(t *testing.T) {
	env := cbEnv(t)
	env.Mock.SetScenario(mockupstream.ScenarioError500)
	_, _ = postBody(t, env, "/v1/messages", chatBody("cb-reset-a"), proxyHeaders())
	_, _ = postBody(t, env, "/v1/messages", chatBody("cb-reset-b"), proxyHeaders())
	if f := breakerFails(t, env); f != 2 {
		t.Fatalf("two 5xx accumulate to %v, want 2", f)
	}
	env.Mock.SetScenario(mockupstream.ScenarioSuccess)
	_, _, _ = stream(t, env, "/v1/messages", chatBody("cb-reset-ok"), proxyHeaders())
	if f := breakerFails(t, env); f != 0 {
		t.Errorf("a success must reset the breaker, got %v", f)
	}
}

func Test4xxNotCircuitFailure(t *testing.T) {
	env := cbEnv(t)
	env.Mock.SetScenario(mockupstream.ScenarioError400)
	for i := 0; i < 6; i++ {
		_, _ = postBody(t, env, "/v1/messages", chatBody(fmt.Sprintf("cb-400-%d", i)), proxyHeaders())
	}
	h := health(t, env)
	if f, _ := h["consecutiveFails"].(float64); f != 0 {
		t.Errorf("4xx must not increment the breaker, got %v", f)
	}
	if open, _ := h["circuitOpen"].(bool); open {
		t.Error("4xx must never open the circuit")
	}
}

func Test429NotCircuitFailure(t *testing.T) {
	env := cbEnv(t)
	env.Mock.SetScenario(mockupstream.ScenarioError429)
	for i := 0; i < 6; i++ {
		_, _ = postBody(t, env, "/v1/messages", chatBody(fmt.Sprintf("cb-429-%d", i)), proxyHeaders())
	}
	h := health(t, env)
	if open, _ := h["circuitOpen"].(bool); open {
		t.Error("429 must not open the global circuit")
	}
	if f, _ := h["consecutiveFails"].(float64); f != 0 {
		t.Errorf("429 must not count as an outage failure, got %v", f)
	}
}

// ── Route allowlist ───────────────────────────────────────────────────────────

func TestUnknownPaths404(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.MaxRetries = 0
	})
	for _, p := range []string{"/unknown", "/v1/unknown", "/healthz", "/", "/v1/models/extra"} {
		status, body := postBody(t, env, p, chatBody(""), proxyHeaders())
		if status != http.StatusNotFound {
			t.Errorf("%s should be rejected locally, got %d", p, status)
		}
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		errObj, _ := parsed["error"].(map[string]any)
		if errObj["code"] != "not_found" {
			t.Errorf("%s error code = %v, want not_found", p, errObj["code"])
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := env.Mock.PostCount(); n != 0 {
		t.Errorf("no unknown-path request may reach upstream, got %d", n)
	}
}

func TestUnsupportedMethod405(t *testing.T) {
	env := newEnv(t, nil)
	resp := request(t, env, http.MethodGet, "/v1/messages", "", proxyHeaders())
	b := readBody(t, resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (body %s)", resp.StatusCode, b)
	}
	if n := env.Mock.PostCount(); n != 0 {
		t.Errorf("GET must not reach upstream, got %d POSTs", n)
	}
}

func TestQueryStringPreserved(t *testing.T) {
	env := newEnv(t, nil)
	_, _, _ = stream(t, env, "/v1/messages?beta=true", chatBody(""), proxyHeaders())
	r := lastPost(t, env)
	if !strings.HasPrefix(r.URL, "/v1/messages?beta=true") {
		t.Errorf("query string not preserved, got %q", r.URL)
	}
}

// ── Inbound proxy authentication ──────────────────────────────────────────────

func authEnv(t *testing.T) *Env {
	return newEnv(t, func(c *config.Config) {
		c.ProxyAuthToken = "test-proxy-secret-token"
		c.MaxRetries = 0
	})
}

func TestAuthRejectsWithoutCredentials(t *testing.T) {
	env := authEnv(t)
	status, _ := postBody(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if n := env.Mock.PostCount(); n != 0 {
		t.Error("no upstream work before auth")
	}
}

func TestAuthRejectsInvalidCredentials(t *testing.T) {
	env := authEnv(t)
	hdr := proxyHeaders()
	hdr["X-Proxy-Token"] = "wrong-token"
	status, _ := postBody(t, env, "/v1/messages", chatBody(""), hdr)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

func TestAuthAcceptsBearer(t *testing.T) {
	env := authEnv(t)
	hdr := proxyHeaders()
	hdr["Authorization"] = "Bearer test-proxy-secret-token"
	status, _, _ := stream(t, env, "/v1/messages", chatBody(""), hdr)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

func TestAuthAcceptsXProxyToken(t *testing.T) {
	env := authEnv(t)
	hdr := proxyHeaders()
	hdr["X-Proxy-Token"] = "test-proxy-secret-token"
	status, _, _ := stream(t, env, "/v1/messages", chatBody(""), hdr)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

func TestAuthXProxyTokenNotForwarded(t *testing.T) {
	env := authEnv(t)
	hdr := proxyHeaders()
	hdr["X-Proxy-Token"] = "test-proxy-secret-token"
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), hdr)
	r := lastPost(t, env)
	if got := r.Headers.Get("Authorization"); got != "Bearer sk_test" {
		t.Errorf("client Authorization must be forwarded unchanged, got %q", got)
	}
	if got := r.Headers.Get("X-Proxy-Token"); got != "" {
		t.Errorf("X-Proxy-Token must never reach upstream, got %q", got)
	}
}

func TestHealthOpenWithoutAuth(t *testing.T) {
	env := authEnv(t)
	status, _ := getJSON(t, env, "/health")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

// ── Oversized request bodies ──────────────────────────────────────────────────

func TestOversizedContentLength413(t *testing.T) {
	env := newEnv(t, nil)
	big := strings.Repeat("x", 21*1024*1024)
	body := fmt.Sprintf(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":%q}]}`, big)
	req := fmt.Sprintf("POST /v1/messages HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		env.Addr, len(body), body)
	status, respBody := rawConn(t, env, req)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", status)
	}
	var parsed map[string]any
	_ = json.Unmarshal(respBody, &parsed)
	errObj, _ := parsed["error"].(map[string]any)
	if errObj["code"] != "payload_too_large" {
		t.Errorf("error code = %v, want payload_too_large", errObj["code"])
	}
	time.Sleep(100 * time.Millisecond)
	if n := env.Mock.PostCount(); n != 0 {
		t.Errorf("oversized body must never reach upstream, got %d", n)
	}
}

func TestOversizedChunked413(t *testing.T) {
	env := newEnv(t, nil)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("POST /v1/messages HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n", env.Addr))
	chunk := strings.Repeat("z", 2*1024*1024)
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&sb, "%x\r\n%s\r\n", len(chunk), chunk)
	}
	sb.WriteString("0\r\n\r\n")
	status, _ := rawConn(t, env, sb.String())
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", status)
	}
	time.Sleep(100 * time.Millisecond)
	if n := env.Mock.PostCount(); n != 0 {
		t.Errorf("oversized body must never reach upstream, got %d", n)
	}
}

func TestStalledUpload408(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.BodyUploadTimeoutMs = 800
	})
	req := fmt.Sprintf("POST /v1/messages HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 10000\r\n\r\n", env.Addr)
	conn, err := net.Dial("tcp", env.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	// Write a little body then never end the request (the upload deadline
	// must fire).
	if _, err := io.WriteString(conn, strings.Repeat("a", 1024)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408 (body %s)", resp.StatusCode, body)
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	errObj, _ := parsed["error"].(map[string]any)
	if errObj["code"] != "request_timeout" {
		t.Errorf("error code = %v, want request_timeout", errObj["code"])
	}
}

// ── Circuit breaker (main env) ────────────────────────────────────────────────

func TestCircuitOpensAndBlocksRequests(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.RequestTimeoutMs = 5000
		c.MaxRetries = 0
	})
	env.Mock.SetScenario(mockupstream.ScenarioConnectionError)
	// Send enough failures to reach 5 consecutive.
	for i := 0; i < 6; i++ {
		_, _ = postBody(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	}
	h := health(t, env)
	if open, _ := h["circuitOpen"].(bool); !open {
		t.Fatal("circuit should be open after 5+ failures")
	}
	status, _ := postBody(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusServiceUnavailable {
		t.Errorf("open circuit returns %d, want 503", status)
	}
}

// ── Prompt injection ──────────────────────────────────────────────────────────

func injEnv(t *testing.T) *Env {
	return newEnv(t, func(c *config.Config) {
		c.InjectSystemPrompt = "TEST_INJECTION_SYSTEM_PROMPT"
		c.MaxRetries = 1
	})
}

func TestInjectPromptMessages(t *testing.T) {
	env := injEnv(t)
	_, _, _ = stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	r := lastPost(t, env)
	sys, _ := bodyMap(r)["system"]
	switch s := sys.(type) {
	case string:
		if !strings.Contains(s, "TEST_INJECTION_SYSTEM_PROMPT") {
			t.Errorf("string system must contain injected prompt, got %q", s)
		}
	case []any:
		found := false
		for _, b := range s {
			if bm, _ := b.(map[string]any); bm["type"] == "text" {
				if txt, _ := bm["text"].(string); strings.Contains(txt, "TEST_INJECTION_SYSTEM_PROMPT") {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("array system must contain injected prompt, got %v", s)
		}
	default:
		t.Errorf("system should be string or array, got %T %v", sys, sys)
	}
}

func TestInjectPromptMessagesRewritten(t *testing.T) {
	env := injEnv(t)
	_, _, _ = stream(t, env, "/messages", chatBody(""), proxyHeaders())
	r := lastPost(t, env)
	if !strings.HasPrefix(r.URL, "/v1/messages") {
		t.Fatalf("upstream URL = %q, want rewritten /v1/messages", r.URL)
	}
	sys, _ := bodyMap(r)["system"]
	ok := false
	switch s := sys.(type) {
	case string:
		ok = strings.Contains(s, "TEST_INJECTION_SYSTEM_PROMPT")
	case []any:
		for _, b := range s {
			if bm, _ := b.(map[string]any); bm["type"] == "text" {
				if txt, _ := bm["text"].(string); strings.Contains(txt, "TEST_INJECTION_SYSTEM_PROMPT") {
					ok = true
				}
			}
		}
	}
	if !ok {
		t.Errorf("system should contain injected prompt after rewrite, got %v", sys)
	}
}

func TestInjectPromptChatCompletions(t *testing.T) {
	env := injEnv(t)
	body := `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hello"}],"stream":true,"max_tokens":10}`
	_, _, _ = stream(t, env, "/v1/chat/completions", body, proxyHeaders())
	r := lastPost(t, env)
	msgs, _ := bodyMap(r)["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("messages should be an array")
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("first message role = %v, want system", first["role"])
	}
	content, _ := first["content"].(string)
	if !strings.Contains(content, "TEST_INJECTION_SYSTEM_PROMPT") {
		t.Errorf("first message must contain injected prompt, got %q", content)
	}
}

func TestInjectPromptAppendsToSystem(t *testing.T) {
	env := injEnv(t)
	body := `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}],"system":"original system prompt","stream":true,"max_tokens":10}`
	_, _, _ = stream(t, env, "/v1/messages", body, proxyHeaders())
	r := lastPost(t, env)
	sys, _ := bodyMap(r)["system"].(string)
	if !strings.HasPrefix(sys, "TEST_INJECTION_SYSTEM_PROMPT") {
		t.Errorf("injected prompt should be prepended, got %q", sys)
	}
	if !strings.Contains(sys, "original system prompt") {
		t.Errorf("original system should be preserved, got %q", sys)
	}
}

// ── Model health (unhealthy filtering) ────────────────────────────────────────

func TestUnhealthyModelFilteredFromModels(t *testing.T) {
	env := newEnv(t, func(c *config.Config) { c.MaxRetries = 0 })
	env.Mock.SetScenario(mockupstream.ScenarioError500)
	_, _ = postBody(t, env, "/v1/messages", chatBody("claude-opus-4-8"), proxyHeaders())
	_, body := getJSON(t, env, "/v1/models")
	data, _ := body["data"].([]any)
	for _, m := range data {
		if mm, _ := m.(map[string]any); mm["id"] == "claude-opus-4-8" {
			t.Errorf("unhealthy model claude-opus-4-8 still listed in /v1/models")
		}
	}
}

// ── Graceful shutdown ─────────────────────────────────────────────────────────

func TestGracefulShutdown(t *testing.T) {
	env := newEnv(t, nil)
	status, _ := getJSON(t, env, "/health")
	if status != http.StatusOK {
		t.Fatalf("health before shutdown: %d", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := env.Server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// After shutdown the listener is closed: new connections must fail.
	if _, err := httpClient.Get(env.BaseURL + "/health"); err == nil {
		t.Error("request after Shutdown succeeded, want connection error")
	}
}

// ── OpenAI streaming edge cases (checklist additions) ─────────────────────────

// TestOpenAIThinkStrippedSse verifies Claude-style <think> spans inside
// OpenAI-format SSE are stripped byte-safely: the span is split across two
// chunks and followed by multi-byte UTF-8, which must survive untouched.
func TestOpenAIThinkStrippedSse(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioOpenaiThink)
	status, raw, _ := stream(t, env, "/v1/chat/completions", chatBody("gpt-5.6-sol"), proxyHeaders())
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if strings.Contains(raw, "<think>") || strings.Contains(raw, "</think>") {
		t.Errorf("think tags must be stripped from the client stream: %q", raw)
	}
	if !strings.Contains(raw, "Final answer") {
		t.Errorf("post-span text must be forwarded: %q", raw)
	}
	if !strings.Contains(raw, "café ✓") {
		t.Errorf("multi-byte UTF-8 must survive think stripping intact: %q", raw)
	}
	if !strings.Contains(raw, "data: [DONE]") {
		t.Errorf("OpenAI stream must still terminate with [DONE]: %q", raw)
	}
}

// TestOpenAIDataNullFramesDropped verifies OpenAI `data: null` / `data:null`
// keepalive frames are filtered at frame level.
func TestOpenAIDataNullFramesDropped(t *testing.T) {
	env := newEnv(t, nil)
	env.Mock.SetScenario(mockupstream.ScenarioOpenaiNullFrames)
	status, raw, _ := stream(t, env, "/v1/chat/completions", chatBody("gpt-5.6-sol"), proxyHeaders())
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if strings.Contains(raw, "data: null") || strings.Contains(raw, "data:null") {
		t.Errorf("data: null keepalive frames must be dropped: %q", raw)
	}
	for _, want := range []string{"Hello", " world", "data: [DONE]"} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %q in stream: %q", want, raw)
		}
	}
}

// TestEmptySseFlagsModel verifies an SSE stream with no data events is flagged
// as empty and the model is degraded (filtered from /v1/models).
func TestEmptySseFlagsModel(t *testing.T) {
	env := newEnv(t, func(c *config.Config) { c.MaxRetries = 0 })
	env.Mock.SetScenario(mockupstream.ScenarioEmptySSE)
	status, _, _ := stream(t, env, "/v1/messages", chatBody("claude-opus-4-8"), proxyHeaders())
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	time.Sleep(50 * time.Millisecond)
	_, body := getJSON(t, env, "/v1/models")
	data, _ := body["data"].([]any)
	for _, m := range data {
		if mm, _ := m.(map[string]any); mm["id"] == "claude-opus-4-8" {
			t.Errorf("model degraded by empty SSE must be filtered from /v1/models")
		}
	}
}

// Test5xxRetriedWhenRetryOn5xx verifies RETRY_ON_5XX=true retries a 5xx
// upstream response until it succeeds (up to MaxRetries).
func Test5xxRetriedWhenRetryOn5xx(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.RetryOn5xx = true
		c.MaxRetries = 2
		c.RetryDelayMs = 10
	})
	env.Mock.SetFailThenSuccess(1) // first POST -> 500, second -> SSE success
	status, _, _ := stream(t, env, "/v1/messages", chatBody(""), proxyHeaders())
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 after 5xx retry", status)
	}
	if n := env.Mock.PostCount(); n < 2 {
		t.Errorf("RetryOn5xx should retry the 5xx, got %d upstream POSTs", n)
	}
}
