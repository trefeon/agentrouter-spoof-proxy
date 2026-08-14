package proxy

import (
	"net/http"
	"reflect"
	"testing"
)

func TestFilterHeaders(t *testing.T) {
	cases := []struct {
		name string
		in   http.Header
		want http.Header
	}{
		{
			name: "strips hop-by-hop, keeps everything else",
			in: http.Header{
				"Transfer-Encoding":   {"chunked"},
				"Connection":          {"keep-alive"},
				"Keep-Alive":          {"timeout=5"},
				"Proxy-Authenticate":  {"Basic"},
				"Proxy-Authorization": {"Basic abc"},
				"Te":                  {"trailers"},
				"Trailer":             {"X-Foo"},
				"Upgrade":             {"websocket"},
				"Content-Type":        {"application/json"},
				"X-Custom":            {"v1"},
			},
			want: http.Header{
				"Content-Type": {"application/json"},
				"X-Custom":     {"v1"},
			},
		},
		{
			name: "case-insensitive hop-by-hop match",
			in: http.Header{
				"CONNECTION":   {"close"},
				"Upgrade":      {"h2c"},
				"Set-Cookie":   {"a=1"},
				"Content-Type": {"text/plain"},
			},
			want: http.Header{
				"Set-Cookie":   {"a=1"},
				"Content-Type": {"text/plain"},
			},
		},
		{
			name: "multiple set-cookie values preserved",
			in: http.Header{
				"Set-Cookie": {"a=1", "b=2; Path=/", "c=3"},
				"Connection": {"close"},
				"X-Multi":    {"x", "y", "z"},
			},
			want: http.Header{
				"Set-Cookie": {"a=1", "b=2; Path=/", "c=3"},
				"X-Multi":    {"x", "y", "z"},
			},
		},
		{
			name: "all-hop-by-hop input yields empty header",
			in: http.Header{
				"Transfer-Encoding": {"chunked"},
				"Connection":        {"close"},
			},
			want: http.Header{},
		},
		{
			name: "nil input yields empty non-nil header",
			in:   nil,
			want: http.Header{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FilterHeaders(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FilterHeaders() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRewritePath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/messages", "/v1/messages"},
		{"/messages?stream=true", "/v1/messages?stream=true"},
		{"/messages?", "/v1/messages?"},
		{"/v1/messages", "/v1/messages"},
		{"/v1/messages?model=gpt-5", "/v1/messages?model=gpt-5"},
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"/v1/chat/completions?stream=true", "/v1/chat/completions?stream=true"},
		{"/v1/chat/completions/extra", "/v1/chat/completions/extra"},
		{"/messages-extra", "/messages-extra"},
		{"/other", "/other"},
		{"/health", "/health"},
		{"/", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := RewritePath(tc.in); got != tc.want {
				t.Errorf("RewritePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactSensitive(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"sk-abc123", "[redacted]"},
		{"sk_abc-def", "[redacted]"},
		{"prefix sk-123 and more", "prefix [redacted] and more"},
		{"two keys sk-a1 and sk_b2", "two keys [redacted] and [redacted]"},
		{"sk-", "sk-"},
		{"sk-1", "[redacted]"},
		{"SK-ABC is uppercase, not matched", "SK-ABC is uppercase, not matched"},
		{"no match here", "no match here"},
		{"sk-4B7x-z9Q", "[redacted]"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := RedactSensitive(tc.in); got != tc.want {
				t.Errorf("RedactSensitive(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short string unchanged", "hello", 500, "hello"},
		{"exact boundary unchanged", "hello", 5, "hello"},
		{"empty unchanged", "", 500, ""},
		{"long string gets suffix", "abcdefghij", 3, "abc... (7 more bytes)"},
		{"max zero keeps suffix only", "hello", 0, "... (5 more bytes)"},
		{"negative max clamped to zero", "hello", -3, "... (5 more bytes)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Truncate(tc.in, tc.max); got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

func TestConstantTimeEqual(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"equal", "sk-secret-key", "sk-secret-key", true},
		{"equal empty", "", "", true},
		{"different length", "sk-abc", "sk-abcdef", false},
		{"one empty", "sk-abc", "", false},
		{"same length different content", "sk-aaaa", "sk-bbbb", false},
		{"equal length one char diff", "sk-aaa1", "sk-aaa2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConstantTimeEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("ConstantTimeEqual(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
