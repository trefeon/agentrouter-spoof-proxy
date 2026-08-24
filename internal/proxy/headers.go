package proxy

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// MaxBodySize is the accepted request body ceiling (utils.mjs MAX_BODY_SIZE):
// 20 MiB. Larger bodies are rejected with 413 before buffering.
const MaxBodySize = 20 << 20

// hopByHop lists the HTTP/1.1 hop-by-hop headers stripped before forwarding
// (utils.mjs HOP_BY_HOP, lowercase keys; matched case-insensitively).
var hopByHop = map[string]struct{}{
	"transfer-encoding":   {},
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"upgrade":             {},
}

// FilterHeaders returns a copy of h without hop-by-hop headers (matched
// case-insensitively). All values of the remaining headers are preserved,
// including multiple Set-Cookie values.
func FilterHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vv := range h {
		if _, ok := hopByHop[strings.ToLower(k)]; ok {
			continue
		}
		vals := make([]string, len(vv))
		copy(vals, vv)
		out[k] = vals
	}
	return out
}

// RewritePath maps the public `/messages` route to the upstream Anthropic
// route `/v1/messages` (utils.mjs rewritePath). Query strings are preserved;
// `/v1/messages` and `/v1/chat/completions` pass through unchanged.
func RewritePath(path string) string {
	if path == "/messages" || strings.HasPrefix(path, "/messages?") {
		return "/v1" + path
	}
	if path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages?") {
		return path
	}
	if path == "/v1/chat/completions" || strings.HasPrefix(path, "/v1/chat/completions?") {
		return path
	}
	return path
}

// sensitiveKey matches API keys of the form sk-… / sk_… (utils.mjs
// redactSensitive regex `sk[-_][A-Za-z0-9_-]+`, case-sensitive lowercase sk).
var sensitiveKey = regexp.MustCompile(`sk[-_][A-Za-z0-9_-]+`)

// RedactSensitive replaces API-key-shaped substrings with "[redacted]".
func RedactSensitive(s string) string {
	return sensitiveKey.ReplaceAllString(s, "[redacted]")
}

// Truncate cuts s to max bytes, appending the `... (N more bytes)` suffix like
// utils.mjs truncate(). Byte counts match Node's code-unit counting for ASCII;
// like the Node version, a multi-byte character straddling the cut point may
// be split.
func Truncate(s string, max int) string {
	if max < 0 {
		max = 0
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("... (%d more bytes)", len(s)-max)
}

// ConstantTimeEqual compares two strings in constant time (utils.mjs
// safeTokenEqual): a length check first, then crypto/subtle on the UTF-8
// bytes, so a naive timing attack cannot recover the secret by
// length-prefixed probing.
func ConstantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

