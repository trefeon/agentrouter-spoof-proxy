package auth

import (
	"reflect"
	"testing"
)

var wantAnthropic = map[string]string{
	"Anthropic-Version":                         "2023-06-01",
	"Anthropic-Beta":                            "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advanced-tool-use-2025-11-20,effort-2025-11-24,structured-outputs-2025-12-15,fast-mode-2026-02-01,token-efficient-tools-2026-03-28",
	"Anthropic-Dangerous-Direct-Browser-Access": "true",
}

var wantGeneric = map[string]string{
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

func TestAnthropicHeadersExact(t *testing.T) {
	got := AnthropicHeaders()
	if !reflect.DeepEqual(got, wantAnthropic) {
		t.Fatalf("AnthropicHeaders() = %v, want %v", got, wantAnthropic)
	}
}

func TestGenericHeadersExact(t *testing.T) {
	got := GenericHeaders()
	if !reflect.DeepEqual(got, wantGeneric) {
		t.Fatalf("GenericHeaders() = %v, want %v", got, wantGeneric)
	}
}

func TestSpoofHeadersMerged(t *testing.T) {
	got := SpoofHeaders()
	if len(got) != len(wantGeneric)+len(wantAnthropic) {
		t.Fatalf("SpoofHeaders() has %d entries, want %d (generic %d + anthropic %d)", len(got), len(wantGeneric)+len(wantAnthropic), len(wantGeneric), len(wantAnthropic))
	}
	for k, v := range wantGeneric {
		if got[k] != v {
			t.Errorf("SpoofHeaders()[%q] = %q, want generic value %q", k, got[k], v)
		}
	}
	for k, v := range wantAnthropic {
		if got[k] != v {
			t.Errorf("SpoofHeaders()[%q] = %q, want anthropic value %q", k, got[k], v)
		}
	}
}

func TestHeadersReturnFreshCopies(t *testing.T) {
	// Mutating a returned map must never affect the next call.
	a := AnthropicHeaders()
	a["Anthropic-Version"] = "MUTATED"
	a["injected"] = "x"
	if got := AnthropicHeaders()["Anthropic-Version"]; got != "2023-06-01" {
		t.Fatalf("AnthropicHeaders() aliases caller-mutated map: got %q", got)
	}
	if _, ok := AnthropicHeaders()["injected"]; ok {
		t.Fatal("AnthropicHeaders() aliases caller-mutated map (extra key leaked)")
	}

	g := GenericHeaders()
	g["User-Agent"] = "MUTATED"
	if got := GenericHeaders()["User-Agent"]; got != "claude-cli/2.1.92 (external, sdk-cli)" {
		t.Fatalf("GenericHeaders() aliases caller-mutated map: got %q", got)
	}

	s := SpoofHeaders()
	s["User-Agent"] = "MUTATED"
	if got := SpoofHeaders()["User-Agent"]; got != "claude-cli/2.1.92 (external, sdk-cli)" {
		t.Fatalf("SpoofHeaders() aliases caller-mutated map: got %q", got)
	}
}
