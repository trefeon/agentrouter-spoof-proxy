package mockupstream

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestMockSuccessStream ensures the default scenario streams the full
// Anthropic sequence and records the request.
func TestMockSuccessStream(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	resp, err := http.Post("http://127.0.0.1:"+itoa(m.Port())+"/v1/messages",
		"application/json", strings.NewReader(`{"model":"m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(raw), "message_stop") {
		t.Errorf("stream missing message_stop: %q", raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	posts := m.Received()
	if len(posts) != 1 || posts[0].URL != "/v1/messages" {
		t.Fatalf("received = %+v", posts)
	}
	if posts[0].Body == nil {
		t.Fatal("request body not recorded")
	}
}

// TestMockWAFPage verifies the WAF block-page scenario markers.
func TestMockWAFPage(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	m.SetScenario(ScenarioWAF405)
	resp, err := http.Post("http://127.0.0.1:"+itoa(m.Port())+"/v1/messages",
		"application/json", strings.NewReader(`{"model":"m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	body := string(raw)
	for _, marker := range []string{"alicdn", "block_message", "waf.js"} {
		if !strings.Contains(body, marker) {
			t.Errorf("WAF page missing marker %q: %q", marker, body)
		}
	}
}

// TestMockWarmupGet verifies GET / returns the challenge cookie used by the
// warmup scheduler.
func TestMockWarmupGet(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	resp, err := http.Get("http://127.0.0.1:" + itoa(m.Port()) + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if c := resp.Header.Get("Set-Cookie"); !strings.Contains(c, "acw_tc=test_mock_cookie") {
		t.Errorf("Set-Cookie = %q", c)
	}
}

// TestMockFailThenSuccess verifies the fail_then_success sequencing.
func TestMockFailThenSuccess(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	m.SetFailThenSuccess(1)

	for i := 0; i < 2; i++ {
		resp, err := http.Post("http://127.0.0.1:"+itoa(m.Port())+"/v1/messages",
			"application/json", strings.NewReader(`{"model":"m1"}`))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if i == 0 && resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("first call = %d, want 500", resp.StatusCode)
		}
		if i == 1 && (resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "message_stop")) {
			t.Fatalf("second call = %d body %q, want 200 SSE", resp.StatusCode, raw)
		}
	}
	if n := m.PostCount(); n != 2 {
		t.Errorf("PostCount = %d, want 2", n)
	}
}

func itoa(v int) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v%10]
		v /= 10
	}
	return string(buf[i:])
}

