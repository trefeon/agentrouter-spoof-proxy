package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trefeon/agentrouter-spoof-proxy/internal/config"
)

// startServer builds a Server on an ephemeral port pointing at the given mock
// upstream, serves it, and returns the base URL plus the server for shutdown.
func startServer(t *testing.T, upstream *httptest.Server, mutate func(*config.Config)) (*Server, string) {
	t.Helper()
	addr := upstream.Listener.Addr().(*net.TCPAddr)
	cfg := &config.Config{
		ListenAddr: "127.0.0.1", ListenPort: 0,
		TargetProto: "http", TargetHost: "127.0.0.1", TargetPort: addr.Port,
		RequestTimeoutMs: 300000, ResponseTimeoutMs: 30000,
		SSEIdleTimeoutMs: 600000, SSEChunkTimeoutMs: 30000,
		BodyUploadTimeoutMs: 60000, SlowResponseMs: 30000,
		WarmupIntervalMs: 180000, DiscoveryIntervalMs: 600000,
		MaxRetries: 2, RetryDelayMs: 1, RetryOn5xx: true,
		StripThinkingTags: true, ModelsCSV: "m1,m2",
		LogLevel: "info",
	}
	if mutate != nil {
		mutate(cfg)
	}
	srv := New(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	go func() { _ = srv.Serve(ln) }()
	return srv, "http://" + ln.Addr().String()
}

// mockUpstream is a minimal upstream that answers all POST /v1/messages and
// GET / with a JSON body.
func mockUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			// Warmup GET slash, respond 200 so the warmup scheduler succeeds fast.
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "ok")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)
	return up
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

// GET /health returns the full status payload with all expected keys.
func TestHealthEndpoint(t *testing.T) {
	up := mockUpstream(t)
	_, base := startServer(t, up, nil)

	status, body := getJSON(t, base+"/health")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, key := range []string{
		"ok", "upstream", "modelSource", "staticModels", "availableModels",
		"activeStreams", "wafCookie", "circuitOpen", "consecutiveFails", "modelHealth",
	} {
		if _, ok := body[key]; !ok {
			t.Errorf("missing /health key %q (got %v)", key, body)
		}
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if body["modelSource"] != "static" {
		t.Errorf("modelSource = %v, want static", body["modelSource"])
	}
	if body["staticModels"] != float64(2) {
		t.Errorf("staticModels = %v, want 2", body["staticModels"])
	}

	// /api/health alias.
	status2, _ := getJSON(t, base+"/api/health")
	if status2 != http.StatusOK {
		t.Errorf("/api/health status = %d, want 200", status2)
	}
}

// GET /v1/models and /models list the static models.
func TestModelsEndpoint(t *testing.T) {
	up := mockUpstream(t)
	_, base := startServer(t, up, nil)

	for _, path := range []string{"/v1/models", "/models"} {
		status, body := getJSON(t, base+path)
		if status != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, status)
		}
		if body["object"] != "list" {
			t.Errorf("%s object = %v, want list", path, body["object"])
		}
		data, _ := body["data"].([]any)
		if len(data) != 2 {
			t.Fatalf("%s data len = %d, want 2", path, len(data))
		}
		first := data[0].(map[string]any)
		if first["object"] != "model" || first["owned_by"] != "agentrouter" {
			t.Errorf("%s first model = %v", path, first)
		}
	}
}

// Unknown route → 404; wrong method on a known route → 405.
func TestUnknownRoute404AndMethod405(t *testing.T) {
	up := mockUpstream(t)
	_, base := startServer(t, up, nil)

	status, body := getJSON(t, base+"/unknown")
	if status != http.StatusNotFound {
		t.Fatalf("unknown status = %d, want 404", status)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "not_found" {
		t.Errorf("error code = %v, want not_found", errObj["code"])
	}

	// Wrong method on a proxy route → 405 (after auth; no auth set here).
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/messages", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/messages status = %d, want 405", resp.StatusCode)
	}
}

// Proxy auth: 401 without token, 200 with Bearer or X-Proxy-Token.
func TestProxyAuth(t *testing.T) {
	up := mockUpstream(t)
	_, base := startServer(t, up, func(c *config.Config) { c.ProxyAuthToken = "sekrit" })

	// No token → 401.
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(`{"model":"m1"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401 (body %s)", resp.StatusCode, body)
	}

	// Wrong token → 401.
	req, _ = http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(`{"model":"m1"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want 401", resp.StatusCode)
	}

	// Bearer token → 200.
	req, _ = http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(`{"model":"m1"}`))
	req.Header.Set("Authorization", "Bearer sekrit")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer status = %d, want 200 (body %s)", resp.StatusCode, raw)
	}

	// X-Proxy-Token → 200.
	req, _ = http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(`{"model":"m1"}`))
	req.Header.Set("X-Proxy-Token", "sekrit")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("x-proxy-token status = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
}

// Health and models stay open even when auth is enabled.
func TestHealthOpenWithoutAuth(t *testing.T) {
	up := mockUpstream(t)
	_, base := startServer(t, up, func(c *config.Config) { c.ProxyAuthToken = "sekrit" })

	status, body := getJSON(t, base+"/health")
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("/health with auth set: status=%d body=%v", status, body)
	}
	status, _ = getJSON(t, base+"/v1/models")
	if status != http.StatusOK {
		t.Fatalf("/v1/models with auth set: status=%d, want 200", status)
	}
}

// Proxy request end-to-end through the real server.
func TestProxyPassthrough(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ignore the warmup scheduler's GET / so it cannot race with the
		// assertion below.
		if r.Method == http.MethodPost {
			gotPath = r.URL.Path
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)
	_, base := startServer(t, up, nil)

	resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(`{"model":"m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	if string(raw) != `{"ok":true}` {
		t.Errorf("body = %q", raw)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}
}

// /messages rewrites to /v1/messages upstream.
func TestProxyMessagesRewrite(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			gotPath = r.URL.Path
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)
	_, base := startServer(t, up, nil)

	resp, err := http.Post(base+"/messages", "application/json", strings.NewReader(`{"model":"m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages (rewrite)", gotPath)
	}
}

// Shutdown closes cleanly: connections stop being served afterward.
func TestShutdown(t *testing.T) {
	up := mockUpstream(t)
	_, base := startServer(t, up, nil)

	// Prove the server is up first.
	status, _ := getJSON(t, base+"/health")
	if status != http.StatusOK {
		t.Fatalf("health before shutdown: %d", status)
	}

	// srv is shutdown via t.Cleanup in startServer; test the explicit method
	// path here by constructing our own and shutting it down mid-flight.
	addr := up.Listener.Addr().(*net.TCPAddr)
	cfg := &config.Config{
		ListenAddr: "127.0.0.1", ListenPort: 0,
		TargetProto: "http", TargetHost: "127.0.0.1", TargetPort: addr.Port,
		RequestTimeoutMs: 300000, ResponseTimeoutMs: 30000,
		SSEIdleTimeoutMs: 600000, SSEChunkTimeoutMs: 30000,
		BodyUploadTimeoutMs: 60000, SlowResponseMs: 30000,
		WarmupIntervalMs: 180000, DiscoveryIntervalMs: 600000,
		MaxRetries: 2, RetryDelayMs: 1, RetryOn5xx: true,
		StripThinkingTags: true, ModelsCSV: "m1", LogLevel: "info",
	}
	srv := New(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// After shutdown, new connections are refused.
	_, err = http.Get("http://" + ln.Addr().String() + "/health")
	if err == nil {
		t.Error("request after Shutdown succeeded, want connection error")
	}
}
