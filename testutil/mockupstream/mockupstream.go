// Package mockupstream provides a scripted mock upstream for E2E tests,
// ported from tests/mock-upstream.mjs (behavior is the spec). The mock answers
// the warmup GET "/", the chat POST routes (/v1/messages,
// /v1/chat/completions), and a set of per-test scenarios: status codes (WAF
// block pages, 429, 5xx), SSE frame sequences, Set-Cookie rotation, delays and
// abrupt mid-stream closes via http.Hijacker on HTTP/1.1.
//
// The mock is NOT safe for parallel tests: the scenario and the received
// request log are shared mutable state (mirrors the Node mock).
package mockupstream

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Scenario names, mirroring the mock-upstream.mjs switch cases. The E2E tests
// use these as the argument to SetScenario.
const (
	ScenarioSuccess           = "success"
	ScenarioCookieRefresh     = "cookie_refresh"
	ScenarioThinkingStream    = "thinking_stream"
	ScenarioOpenaiStream      = "openai_stream"
	ScenarioOpenaiThink       = "openai_think_stream"
	ScenarioOpenaiNullFrames  = "openai_null_frames"
	ScenarioEmptySSE          = "empty_sse"
	ScenarioSuccessStreaming  = "success_streaming"
	ScenarioWAF405            = "waf_405"
	ScenarioWAF403            = "waf_403"
	ScenarioNonWAF405         = "non_waf_405"
	ScenarioError500          = "error_500"
	ScenarioError400          = "error_400"
	ScenarioError429          = "error_429"
	ScenarioError502          = "error_502"
	ScenarioError503          = "error_503"
	ScenarioTimeout           = "timeout"
	ScenarioNoResponseHeaders = "no_response_headers"
	ScenarioPartialClose      = "partial_close"
	ScenarioSlowStream        = "slow_stream"
	ScenarioHang              = "hang"
	ScenarioConnectionError   = "connection_error"
	ScenarioFailThenSuccess   = "fail_then_success"
)

// Anthropic SSE chunk sequences, byte-identical to mock-upstream.mjs.
var (
	// SSEChunks is a complete Anthropic /v1/messages stream.
	SSEChunks = []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"content\":[],\"model\":\"claude-opus-4-8\",\"role\":\"assistant\",\"stop_reason\":null,\"stop_sequence\":null,\"type\":\"message\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"block_type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello from mock upstream\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":3}}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}

	// ThinkingChunks is an Anthropic interleaved-thinking stream (thinking
	// block first, then text).
	ThinkingChunks = []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_think\",\"content\":[],\"model\":\"claude-opus-4-8\",\"role\":\"assistant\",\"stop_reason\":null,\"stop_sequence\":null,\"type\":\"message\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"block_type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Let me reason through this carefully.\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"block_type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Here is the final answer.\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}

	// OpenAIChunks is an OpenAI chat.completions streaming format.
	OpenAIChunks = []string{
		"data: {\"id\":\"chatcmpl-9router-test\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-9router-test\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-9router-test\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" from OpenAI\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-9router-test\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	}

	// OpenAIThinkChunks is a Claude-style <think> span inside OpenAI-format
	// SSE, with the open tag split across chunks and multi-byte UTF-8 after
	// the span. Exercises think-stripping end to end (Go-only scenario).
	OpenAIThinkChunks = []string{
		"data: {\"id\":\"chatcmpl-think\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-think\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"<think>Let me reason\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-think\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" here.</think>Final answer caf\u00e9 \u2713\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-think\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	}

	// OpenAINullFrames is OpenAI chunks interleaved with `data: null` /
	// `data:null` keepalive frames that must be dropped at frame level
	// (Go-only scenario).
	OpenAINullFrames = []string{
		"data: {\"id\":\"chatcmpl-null\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n",
		"data: null\n\n",
		"data: {\"id\":\"chatcmpl-null\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n",
		"data:null\n\n",
		"data: {\"id\":\"chatcmpl-null\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-5.6-sol\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":null}]}\n\n",
		"data: [DONE]\n\n",
	}
)

// wafBlockPage is the Alibaba-Cloud-style WAF block page (mock-upstream.mjs
// waf_405 case).
const wafBlockPage = `<html><body><script src="//alicdn.com/waf.js"></script><p>block_message</p></body></html>`

// Received is one recorded upstream request (mock-upstream.mjs received[]).
// Body is the JSON-decoded object (map[string]any) when the body parses as
// JSON, nil otherwise; Raw always carries the raw body bytes.
type Received struct {
	Method  string
	URL     string // raw request URI, path + query
	Headers http.Header
	Body    any
	Raw     []byte
}

// MockUpstream is the scripted mock upstream. New binds the listener; Close
// stops the server immediately.
type MockUpstream struct {
	mu        sync.Mutex
	scenario  string
	received  []Received
	posts     int // chat POST counter (fail_then_success sequencing)
	failN     int // how many leading chat POSTs fail (fail_then_success)
	ctx       context.Context
	cancel    context.CancelFunc
	server    *http.Server
	ln        net.Listener
	port      int
	closeOnce sync.Once
}

// New constructs a mock in the "success" scenario and starts listening on an
// ephemeral 127.0.0.1 port (mirrors mock-upstream.mjs start()).
func New() (*MockUpstream, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &MockUpstream{
		scenario: ScenarioSuccess,
		failN:    2,
		ctx:      ctx,
		cancel:   cancel,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		return nil, err
	}
	m.ln = ln
	m.port = ln.Addr().(*net.TCPAddr).Port
	m.server = &http.Server{Handler: m}
	go func() { _ = m.server.Serve(ln) }()
	return m, nil
}

// Close stops the mock immediately, unblocking any sleeping scenario handler
// (timeout / no_response_headers / hang). Safe to call multiple times.
func (m *MockUpstream) Close() error {
	var err error
	m.closeOnce.Do(func() {
		m.cancel()
		if m.server != nil {
			err = m.server.Close() // immediate: closes active conns, no drain wait
		}
	})
	return err
}

// Port returns the bound mock port.
func (m *MockUpstream) Port() int { return m.port }

// SetScenario switches the scripted response for chat POST routes.
func (m *MockUpstream) SetScenario(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scenario = s
}

// SetFailThenSuccess scripts the fail_then_success scenario: the first n chat
// POSTs return 500, subsequent ones stream the success SSE.
func (m *MockUpstream) SetFailThenSuccess(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scenario = ScenarioFailThenSuccess
	m.failN = n
}

// Reset clears the received log and returns the mock to the success scenario
// (mock-upstream.mjs reset()).
func (m *MockUpstream) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.received = nil
	m.posts = 0
	m.scenario = ScenarioSuccess
}

// Received returns a copy of the recorded request log.
func (m *MockUpstream) Received() []Received {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Received, len(m.received))
	copy(out, m.received)
	return out
}

// PostCount returns the number of recorded POST requests, the subset the
// Node tests filter with `r.method === "POST"`.
func (m *MockUpstream) PostCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.received {
		if r.Method == http.MethodPost {
			n++
		}
	}
	return n
}

// LastPost returns the most recent recorded POST request, or nil.
func (m *MockUpstream) LastPost() *Received {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.received) - 1; i >= 0; i-- {
		if m.received[i].Method == http.MethodPost {
			r := m.received[i]
			return &r
		}
	}
	return nil
}

// ── HTTP routing (mock-upstream.mjs _route) ──────────────────────────────────

func (m *MockUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Record first: method, URL, headers, body (mirrors req.on("end") then
	// _route in the Node mock).
	raw, _ := io.ReadAll(r.Body)
	rec := Received{
		Method:  r.Method,
		URL:     r.URL.RequestURI(),
		Headers: r.Header.Clone(),
		Raw:     raw,
	}
	if len(raw) > 0 {
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) == nil {
			rec.Body = parsed
		}
	}

	if r.Method == http.MethodGet {
		m.record(rec)
		m.handleGet(w)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/chat/completions") || strings.HasPrefix(r.URL.Path, "/v1/messages") {
		m.record(rec)
		m.handleChat(w)
		return
	}
	m.record(rec)
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "not found")
}

func (m *MockUpstream) record(rec Received) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.received = append(m.received, rec)
}

// handleGet mirrors _get: a plain HTML page that issues the warmup challenge
// cookie. Not scenario-dependent, so the background warmup goroutine always
// succeeds quickly.
func (m *MockUpstream) handleGet(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Set-Cookie", "acw_tc=test_mock_cookie; Path=/; Secure")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "<html><body>mock ok</body></html>")
}

func (m *MockUpstream) handleChat(w http.ResponseWriter) {
	m.mu.Lock()
	scenario := m.scenario
	m.mu.Unlock()

	switch scenario {
	case ScenarioSuccess:
		m.sse(w, SSEChunks, nil, nil)

	case ScenarioCookieRefresh:
		m.sse(w, SSEChunks, map[string]string{"Set-Cookie": "cdn_sec_tc=traffic_cookie_1; Path=/; Secure"}, nil)

	case ScenarioThinkingStream:
		m.sse(w, ThinkingChunks, nil, nil)

	case ScenarioOpenaiStream:
		m.sse(w, OpenAIChunks, nil, nil)

	case ScenarioOpenaiThink:
		m.sse(w, OpenAIThinkChunks, nil, nil)

	case ScenarioOpenaiNullFrames:
		m.sse(w, OpenAINullFrames, nil, nil)

	case ScenarioEmptySSE:
		m.sse(w, []string{":\n\n", ":\n\n"}, nil, nil)

	case ScenarioSuccessStreaming:
		m.sse(w, SSEChunks, nil, func(int) { m.sleep(10 * time.Millisecond) })

	case ScenarioSlowStream:
		m.sse(w, SSEChunks, nil, func(int) { m.sleep(50 * time.Millisecond) })

	case ScenarioWAF405:
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, wafBlockPage)

	case ScenarioWAF403:
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, wafBlockPage)

	case ScenarioNonWAF405:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, `{"error":{"message":"method not allowed"}}`)

	case ScenarioError500:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"internal error"}}`)

	case ScenarioError400:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid request"}}`)

	case ScenarioError429:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)

	case ScenarioError502:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"bad gateway"}}`)

	case ScenarioError503:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"NoChannelError"}}`)

	case ScenarioTimeout, ScenarioNoResponseHeaders:
		// Accept the request but never send response headers (the adaptive
		// RESPONSE_TIMEOUT_MS path). Block until the mock shuts down so the
		// handler goroutine cannot leak past the test.
		m.blockUntilClosed()

	case ScenarioPartialClose:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, SSEChunks[0])
		_, _ = io.WriteString(w, SSEChunks[1])
		fl.Flush()
		m.sleep(5 * time.Millisecond)
		m.hijackClose(w)

	case ScenarioHang:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, SSEChunks[0])
		_, _ = io.WriteString(w, SSEChunks[1])
		fl.Flush()
		m.blockUntilClosed()

	case ScenarioConnectionError:
		m.hijackClose(w)

	case ScenarioFailThenSuccess:
		m.mu.Lock()
		m.posts++
		fail := m.posts <= m.failN
		m.mu.Unlock()
		if fail {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"internal error"}}`)
			return
		}
		m.sse(w, SSEChunks, nil, nil)

	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}
}

// sse writes an SSE response with the given chunks, flushing each (mirrors the
// Node _sse helper: writeHead + write each chunk, then end).
func (m *MockUpstream) sse(w http.ResponseWriter, chunks []string, extra map[string]string, perChunk func(int)) {
	for k, v := range extra {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	for i, c := range chunks {
		if _, err := io.WriteString(w, c); err != nil {
			return // client (the proxy) went away mid-stream
		}
		fl.Flush()
		if perChunk != nil {
			perChunk(i)
		}
	}
}

// hijackClose abruptly terminates the HTTP/1.1 connection mid-stream
// (mock-upstream.mjs req.socket.destroy()).
func (m *MockUpstream) hijackClose(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// blockUntilClosed sleeps until the mock shuts down (or a 60s safety cap).
func (m *MockUpstream) blockUntilClosed() {
	select {
	case <-m.ctx.Done():
	case <-time.After(60 * time.Second):
	}
}

func (m *MockUpstream) sleep(d time.Duration) {
	select {
	case <-m.ctx.Done():
	case <-time.After(d):
	}
}

