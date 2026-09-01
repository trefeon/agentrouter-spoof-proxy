package proxy

import "encoding/json"

// Usage holds the token accounting extracted from one response payload.
// Missing or null fields count as 0.
type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

// UsageFromFrame parses ONE SSE data payload (JSON) for usage tokens:
// Anthropic message_start -> message.usage.input_tokens plus the
// cache_creation/cache_read fields; Anthropic message_delta ->
// usage.output_tokens plus cache fields; OpenAI chunk ->
// usage.input_tokens/output_tokens plus
// usage.prompt_tokens_details.cached_tokens. Missing or null fields count 0.
func UsageFromFrame(data []byte) Usage {
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil || parsed == nil {
		return Usage{}
	}
	var u Usage
	if msg, ok := parsed["message"].(map[string]any); ok {
		if usage, ok := msg["usage"].(map[string]any); ok {
			u.add(usage)
		}
	}
	if usage, ok := parsed["usage"].(map[string]any); ok {
		u.add(usage)
	}
	return u
}

// UsageFromBody parses a full non-SSE JSON response body: the top-level
// Anthropic message usage, or the OpenAI completion usage. Missing or null
// fields count 0.
func UsageFromBody(body []byte) Usage {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil || parsed == nil {
		return Usage{}
	}
	var u Usage
	if usage, ok := parsed["usage"].(map[string]any); ok {
		u.add(usage)
	}
	return u
}

// add accumulates one usage object. Anthropic names the cache fields
// cache_creation/cache_read_input_tokens; OpenAI uses prompt/completion_tokens
// with prompt_tokens_details.cached_tokens. Presence, not value, decides the
// fallback so a genuine zero is respected.
func (u *Usage) add(usage map[string]any) {
	if v, ok := tokenAt(usage, []string{"input_tokens"}); ok {
		u.Input += intOf(v)
	} else if v, ok := tokenAt(usage, []string{"prompt_tokens"}); ok {
		u.Input += intOf(v)
	}
	if v, ok := tokenAt(usage, []string{"output_tokens"}); ok {
		u.Output += intOf(v)
	} else if v, ok := tokenAt(usage, []string{"completion_tokens"}); ok {
		u.Output += intOf(v)
	}
	if v, ok := tokenAt(usage, []string{"cache_read_input_tokens"}); ok {
		u.CacheRead += intOf(v)
	}
	if v, ok := tokenAt(usage, []string{"cache_creation_input_tokens"}); ok {
		u.CacheWrite += intOf(v)
	}
	if v, ok := tokenAt(usage, []string{"prompt_tokens_details", "cached_tokens"}); ok {
		u.CacheRead += intOf(v)
	}
}

// intOf converts a JSON number to int; non-numbers count as 0.
func intOf(v any) int {
	f, ok := v.(float64)
	if !ok {
		return 0
	}
	return int(f)
}
