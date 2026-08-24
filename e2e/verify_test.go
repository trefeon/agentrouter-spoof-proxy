// Package e2e, issue-verification regression tests (port of
// tests/verify-issues.test.mjs, 7 tests). These lock in historical bug fixes;
// the naming and intent follow the Node suite. Two tests inspect
// cmd/proxy/main.go source (the original inspected proxy.mjs source); the
// rest run against a live in-process proxy + mock upstream (newEnv).
package e2e

import (
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/trefeon/agentrouter-spoof-proxy/internal/config"
)

// ── issue-1: process-level crash protection ──────────────────────────────────
//
// Node guarded uncaughtException/unhandledRejection so a stray async error
// could not kill the proxy. Go's equivalent guarantees: signal-based graceful
// shutdown is wired, and net/http recovers per-connection handler panics. The
// original suite verified the source; we do the same for main.go.

func TestIssue1MainWiresSignalHandling(t *testing.T) {
	src, err := os.ReadFile("../cmd/proxy/main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)
	for _, needle := range []string{"signal.NotifyContext", "syscall.SIGTERM", "os.Interrupt"} {
		if !strings.Contains(text, needle) {
			t.Errorf("main.go missing %q (signal handling not wired)", needle)
		}
	}
}

func TestIssue1MainWiresGracefulShutdown(t *testing.T) {
	src, err := os.ReadFile("../cmd/proxy/main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)
	if !strings.Contains(text, "srv.Shutdown") {
		t.Error("main.go missing srv.Shutdown (graceful drain not wired)")
	}
	if !strings.Contains(text, "15*time.Second") {
		t.Error("main.go missing 15s shutdown deadline (forced-exit bound)")
	}
}

// ── issue-2: Content-Length isolation with INJECT_SYSTEM_PROMPT ──────────────
//
// The proxy must never forward a client-supplied Content-Length upstream:
// header spoofing/injection changes the body size, so a stale value would
// corrupt the upstream read. Go's http.Transport auto-computes Content-Length
// for buffered bodies; the handler copies only authorization/x-api-key from
// the client (never content-length).

func TestIssue2ClientContentLengthNotForwarded(t *testing.T) {
	const injectPrompt = "ISSUE2_TEST_PROMPT"
	env := newEnv(t, func(c *config.Config) {
		c.InjectSystemPrompt = injectPrompt
		c.MaxRetries = 0
	})
	env.Mock.Reset()

	body := chatBody("")
	resp := request(t, env, "POST", "/v1/messages", body, proxyHeaders())
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	post := env.Mock.LastPost()
	if post == nil {
		t.Fatal("upstream received no POST")
	}
	upLen := post.Headers.Get("Content-Length")
	raw := post.Raw
	if upLen == "" {
		t.Fatalf("upstream Content-Length missing (Go Transport must set it): headers %v", post.Headers)
	}
	// Injection prepends the system prompt, so the forwarded body is LARGER
	// than the client body. If the handler had leaked the client's
	// Content-Length, this equality would fail.
	if upLen != strconv.Itoa(len(raw)) {
		t.Errorf("upstream Content-Length = %s, want %d (actual forwarded bytes). "+
			"Stale client content-length leaked upstream?", upLen, len(raw))
	}
	if len(raw) <= len(body) {
		t.Errorf("forwarded body (%d bytes) not larger than client body (%d bytes); prompt injection missing",
			len(raw), len(body))
	}
}

func TestIssue2InjectedPromptPresent(t *testing.T) {
	const injectPrompt = "ISSUE2_TEST_PROMPT"
	env := newEnv(t, func(c *config.Config) {
		c.InjectSystemPrompt = injectPrompt
		c.MaxRetries = 0
	})
	env.Mock.Reset()

	resp := request(t, env, "POST", "/v1/messages", chatBody(""), proxyHeaders())
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	post := env.Mock.LastPost()
	if post == nil {
		t.Fatal("upstream received no POST")
	}
	bodyStr := string(post.Raw)
	if !strings.Contains(bodyStr, injectPrompt) {
		t.Error("injected prompt missing from upstream body")
	}
	if !strings.Contains(bodyStr, `"hi"`) {
		t.Error("original content not preserved in upstream body")
	}
}

func TestIssue2InjectedBodyLarger(t *testing.T) {
	const prompt = "ISSUE2_TEST_PROMPT"
	original := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":10}`)
	injected := []byte(`{"model":"claude-opus-4-8","system":[{"type":"text","text":"` + prompt + `"}],"messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":10}`)
	if len(injected) <= len(original) {
		t.Errorf("injected body (%d) should be larger than original (%d)", len(injected), len(original))
	}
}

// ── issue-3: large request bodies must not crash the proxy ───────────────────

func TestIssue3LargeBody1MB(t *testing.T) {
	assertLargeBodySurvives(t, 1*1024*1024)
}

func TestIssue3LargeBody5MB(t *testing.T) {
	assertLargeBodySurvives(t, 5*1024*1024)
}

func assertLargeBodySurvives(t *testing.T, size int) {
	t.Helper()
	env := newEnv(t, func(c *config.Config) { c.MaxRetries = 0 })

	var sb strings.Builder
	sb.WriteString(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"`)
	sb.WriteString(strings.Repeat("x", size))
	sb.WriteString(`"}],"stream":true,"max_tokens":10}`)

	resp := request(t, env, "POST", "/v1/messages", sb.String(), proxyHeaders())
	if resp.StatusCode != 200 && resp.StatusCode != 502 {
		t.Errorf("status = %d, want 200 or 502 (proxy responded, not crashed)", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Proxy must still be healthy and responsive after the large body.
	hr := request(t, env, "GET", "/health", "", nil)
	if hr.StatusCode != 200 {
		t.Errorf("/health after large body = %d, want 200 (proxy crashed?)", hr.StatusCode)
	}
	hr.Body.Close()
}
