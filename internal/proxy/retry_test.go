package proxy

import (
	"testing"
	"time"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		message  string
		retry5xx bool
		want     bool
	}{
		{"500 with retryOn5xx", 500, "", true, true},
		{"500 without retryOn5xx", 500, "", false, false},
		{"502 with retryOn5xx", 502, "", true, true},
		{"503 without retryOn5xx", 503, "", false, false},
		{"599 with retryOn5xx", 599, "", true, true},
		{"499 never retries on status", 499, "", true, false},
		{"404 with keyword message", 404, "connection timeout", false, true},
		{"404 without keyword", 404, "bad request", false, false},
		{"200 with keyword", 200, "ECONNRESET", false, true},
		{"0 status with keyword", 0, "socket hang up", false, true},
		{"0 status without keyword", 0, "other error", false, false},
		{"empty message", 0, "", false, false},
		{"keyword socket hang up", 0, "socket hang up during read", false, true},
		{"keyword timeout", 0, "ETIMEDOUT-ish", false, true},
		{"keyword ECONNRESET", 0, "read ECONNRESET", false, true},
		{"keyword ETIMEDOUT", 0, "connect ETIMEDOUT", false, true},
		{"keyword ENETUNREACH", 0, "ENETUNREACH destination", false, true},
		{"case-sensitive keywords", 0, "Socket Hang Up", false, false},
		{"case-sensitive ECONNRESET", 0, "econnreset", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.status, tc.message, tc.retry5xx); got != tc.want {
				t.Errorf("IsRetryable(%d, %q, %v) = %v, want %v",
					tc.status, tc.message, tc.retry5xx, got, tc.want)
			}
		})
	}
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		name    string
		attempt int
		baseMs  int
		want    time.Duration
	}{
		{"attempt 0", 0, 1000, 1 * time.Second},
		{"attempt 1", 1, 1000, 2 * time.Second},
		{"attempt 2", 2, 1000, 4 * time.Second},
		{"attempt 3", 3, 1000, 8 * time.Second},
		{"non-power-of-two base", 2, 500, 2 * time.Second},
		{"zero base", 0, 0, 0},
		{"zero base attempt 3", 3, 0, 0},
		{"base 100 attempt 2", 2, 100, 400 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RetryDelay(tc.attempt, tc.baseMs); got != tc.want {
				t.Errorf("RetryDelay(%d, %d) = %v, want %v", tc.attempt, tc.baseMs, got, tc.want)
			}
		})
	}
}

func TestResponseTimeout(t *testing.T) {
	cases := []struct {
		name      string
		bodyBytes int
		defaultMs int
		want      time.Duration
	}{
		{"tiny body uses default", 100, 30000, 30 * time.Second},
		{"exactly 0.5MB uses default", 512 * 1024, 30000, 30 * time.Second},
		{"just over 0.5MB", 512*1024 + 1, 30000, 90 * time.Second},
		{"exactly 1MB", 1024 * 1024, 30000, 90 * time.Second},
		{"just over 1MB", 1024*1024 + 1, 30000, 120 * time.Second},
		{"exactly 2MB", 2 * 1024 * 1024, 30000, 120 * time.Second},
		{"just over 2MB", 2*1024*1024 + 1, 30000, 180 * time.Second},
		{"exactly 5MB", 5 * 1024 * 1024, 30000, 180 * time.Second},
		{"just over 5MB", 5*1024*1024 + 1, 30000, 300 * time.Second},
		{"huge body", 100 * 1024 * 1024, 30000, 300 * time.Second},
		{"custom default", 100, 5000, 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResponseTimeout(tc.bodyBytes, tc.defaultMs); got != tc.want {
				t.Errorf("ResponseTimeout(%d, %d) = %v, want %v",
					tc.bodyBytes, tc.defaultMs, got, tc.want)
			}
		})
	}
}

func TestIsWafBlock(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   []byte
		want   bool
	}{
		{"403 alicdn marker", 403, []byte("<html>alicdn block page</html>"), true},
		{"403 block_message marker", 403, []byte("var block_message = 'x'"), true},
		{"403 renderData marker", 403, []byte("renderData.push(...)"), true},
		{"403 waf.js marker", 403, []byte(`<script src="/waf.js">`), true},
		{"405 waf.js marker", 405, []byte("waf.js challenge"), true},
		{"403 non-marker body", 403, []byte("<html>rate limited</html>"), false},
		{"403 substring marker matches", 403, []byte("xalicdn2y"), true},
		{"403 near-miss marker does not match", 403, []byte("ali cdn page"), false},
		{"404 with marker is not WAF", 404, []byte("alicdn marker present"), false},
		{"200 with marker is not WAF", 200, []byte("waf.js"), false},
		{"403 empty body", 403, nil, false},
		{"500 marker body not WAF", 500, []byte("alicdn"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsWafBlock(tc.status, tc.body); got != tc.want {
				t.Errorf("IsWafBlock(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}
