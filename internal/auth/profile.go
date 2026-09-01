// Package auth — profile-aware spoof header registry.
//
// Each spoof profile impersonates a different first-party client that
// AgentRouter's WAF allows. The proxy sends these headers upstream so
// agentrouter.org sees a legitimate CLI, not a generic proxy.
//
// Default profile is "opencode" (env SPOOF_PROFILE). Legacy callers
// continue to use GenericHeaders/AnthropicHeaders/SpoofHeaders which are
// pinned to the claude-code profile for backwards compatibility.
package auth

import (
	"strings"
)

// SupportedProfiles is the canonical list shown in config validation and
// the dashboard. Keep sorted for stable error messages.
var SupportedProfiles = []string{
	"opencode",
	"claude-code",
	"codex",
	"qwen",
	"cline",
	"roo",
	"kilo",
	"cursor",
	"trae",
	"pi",
	"openclaw",
	"hermes",
	"droid",
	"copilot",
	"gemini",
	"generic",
}

// profileAliases maps user-facing aliases to canonical names. Kept in sync
// with config.NormalizedSpoofProfile.
var profileAliases = map[string]string{
	"claude":         "claude-code",
	"qwen-code":      "qwen",
	"roocode":        "roo",
	"roo-code":       "roo",
	"kilocode":       "kilo",
	"kilo-code":      "kilo",
	"factory":        "droid",
	"factory-droid":  "droid",
	"github-copilot": "copilot",
	"none":           "generic",
}

// NormalizeProfile lowercases, trims, resolves aliases and falls back to
// "opencode" for empty input. Exported for config validation reuse.
func NormalizeProfile(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "opencode"
	}
	if canon, ok := profileAliases[p]; ok {
		return canon
	}
	return p
}

// IsValidProfile reports whether p is a known profile or alias.
func IsValidProfile(p string) bool {
	n := NormalizeProfile(p)
	for _, s := range SupportedProfiles {
		if n == s {
			return true
		}
	}
	return false
}

// GenericHeadersForProfile returns the non-Anthropic (OpenAI-path) headers
// for the given profile. A new map each call.
func GenericHeadersForProfile(profile string) map[string]string {
	switch NormalizeProfile(profile) {
	case "opencode":
		// Real opencode session User-Agent is `opencode/${InstallationVersion}`
		// (packages/opencode/src/session/llm/request.ts:18, USER_AGENT).
		// No X-Stainless headers; WAF now allows this UA directly.
		return map[string]string{
			"User-Agent": "opencode/1.18.25",
		}
	case "claude-code":
		return GenericHeaders()
	case "codex":
		return map[string]string{
			"User-Agent": "codex-cli/0.52.0 (external, codex)",
			"X-App":      "codex",
		}
	case "qwen":
		return map[string]string{
			"User-Agent": "qwen-code/0.2.1 (external, qwen)",
			"X-App":      "qwen-code",
		}
	case "cline":
		return map[string]string{
			"User-Agent": "Cline/3.18.0 (external, cline)",
			"X-App":      "cline",
		}
	case "roo":
		return map[string]string{
			"User-Agent": "Roo-Code/3.20.0 (external, roo)",
			"X-App":      "roo-code",
		}
	case "kilo":
		return map[string]string{
			"User-Agent": "Kilo-Code/4.0.0 (external, kilo)",
			"X-App":      "kilo-code",
		}
	case "cursor":
		return map[string]string{
			"User-Agent": "Cursor/1.5.0 (external, cursor)",
			"X-App":      "cursor",
		}
	case "trae":
		return map[string]string{
			"User-Agent": "Trae/2.0.5 (external, trae)",
			"X-App":      "trae",
		}
	case "pi":
		return map[string]string{
			"User-Agent": "pi/0.5.0 (external, pi)",
			"X-App":      "pi",
		}
	case "openclaw":
		return map[string]string{
			"User-Agent": "openclaw/1.0.0 (external, openclaw)",
			"X-App":      "openclaw",
		}
	case "hermes":
		return map[string]string{
			"User-Agent": "hermes/1.0.0 (external, hermes)",
			"X-App":      "hermes",
		}
	case "droid":
		return map[string]string{
			"User-Agent": "factory-droid/0.8.0 (external, droid)",
			"X-App":      "droid",
		}
	case "copilot":
		return map[string]string{
			"User-Agent": "GitHubCopilotChat/0.26.0 (external, copilot)",
			"X-App":      "copilot",
		}
	case "gemini":
		return map[string]string{
			"User-Agent": "GeminiCLI/0.10.0 (external, gemini)",
			"X-App":      "gemini",
		}
	case "generic":
		return map[string]string{
			"User-Agent": "agentrouter-spoof-proxy/1.0.0",
			"X-App":      "generic",
		}
	default:
		// Unknown profile → fall back to opencode (the default).
		return map[string]string{
			"User-Agent": "opencode/1.18.25",
		}
	}
}

// AnthropicHeadersForProfile returns the Anthropic-specific headers for the
// given profile (Anthropic-Version, Anthropic-Beta, etc.). Generic UA is
// NOT included here; callers should merge via SpoofHeadersForProfile.
// A new map each call. For OpenAI-only profiles this still returns a valid
// Anthropic set so /v1/messages can be proxied regardless of profile choice.
func AnthropicHeadersForProfile(profile string) map[string]string {
	switch NormalizeProfile(profile) {
	case "claude-code":
		return AnthropicHeaders()
	case "opencode":
		return map[string]string{
			"Anthropic-Version":                         "2023-06-01",
			"Anthropic-Beta":                            "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14",
			"Anthropic-Dangerous-Direct-Browser-Access": "true",
		}
	default:
		// Other CLIs that occasionally hit /v1/messages get the opencode
		// minimal beta; it is the smallest set that still enables thinking
		// blocks without over-claiming Claude Code capabilities.
		return map[string]string{
			"Anthropic-Version":                         "2023-06-01",
			"Anthropic-Beta":                            "interleaved-thinking-2025-05-14",
			"Anthropic-Dangerous-Direct-Browser-Access": "true",
		}
	}
}

// SpoofHeadersForProfile merges GenericHeadersForProfile + AnthropicHeadersForProfile.
// Generic first, Anthropic wins on overlap (no overlap today, but keep the rule).
func SpoofHeadersForProfile(profile string) map[string]string {
	g := GenericHeadersForProfile(profile)
	a := AnthropicHeadersForProfile(profile)
	m := make(map[string]string, len(g)+len(a))
	for k, v := range g {
		m[k] = v
	}
	for k, v := range a {
		m[k] = v
	}
	return m
}

// HeadersForProfile returns the headers the handler should send upstream
// for the given profile and path type. If isAnthropic is true (e.g.
// /v1/messages) it returns SpoofHeadersForProfile, otherwise
// GenericHeadersForProfile.
func HeadersForProfile(profile string, isAnthropic bool) map[string]string {
	if isAnthropic {
		return SpoofHeadersForProfile(profile)
	}
	return GenericHeadersForProfile(profile)
}
