// Package auth provides the request-header spoofing and WAF cookie handling
// that make the upstream (agentrouter.org) treat this proxy as a first-party
// Claude Code client.
//
// Port of src/auth/spoof.mjs and src/auth/waf.mjs; behavior is the spec.
package auth

// AnthropicHeaders returns a fresh copy of the Anthropic-* spoof headers
// (src/auth/spoof.mjs ANTHROPIC_SPOOF_HEADERS, byte-identical values).
// Callers get their own map on every call so they can never mutate the
// shared values.
func AnthropicHeaders() map[string]string {
	return map[string]string{
		"Anthropic-Version":                         "2023-06-01",
		"Anthropic-Beta":                            "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advanced-tool-use-2025-11-20,effort-2025-11-24,structured-outputs-2025-12-15,fast-mode-2026-02-01,token-efficient-tools-2026-03-28",
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
	}
}

// GenericHeaders returns a fresh copy of the claude-cli client fingerprint
// headers (src/auth/spoof.mjs GENERIC_SPOOF_HEADERS, byte-identical values).
func GenericHeaders() map[string]string {
	return map[string]string{
		"User-Agent":                  "claude-cli/2.1.92 (external, sdk-cli)",
		"X-App":                       "cli",
		"X-Stainless-Helper-Method":   "stream",
		"X-Stainless-Retry-Count":     "0",
		"X-Stainless-Runtime-Version": "v24.14.0",
		"X-Stainless-Package-Version": "0.80.0",
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Arch":            "arm64",
		"X-Stainless-Os":              "Linux",
		"X-Stainless-Timeout":         "600",
	}
}

// SpoofHeaders returns the merged spoof header set: generic first, Anthropic
// wins on any overlap (src/auth/spoof.mjs SPOOF_HEADERS).
func SpoofHeaders() map[string]string {
	m := GenericHeaders()
	for k, v := range AnthropicHeaders() {
		m[k] = v
	}
	return m
}
