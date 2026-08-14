package proxy

import (
	"encoding/json"
	"strings"
)

// RequestSummary mirrors the shape returned by summarizeRequest().
type RequestSummary struct {
	Method       string
	Path         string
	BodyBytes    int
	ParseOK      bool
	Model        string
	Stream       bool
	MaxTokens    *int
	MessageCount *int
}

// SummarizeRequest parses the raw JSON body once and extracts the routing /
// telemetry fields used by the handler (utils.mjs summarizeRequest). MaxTokens
// and MessageCount are nil when the field is absent or of the wrong type; a
// non-string model yields "". ParseOK is false for any unparseable body.
func SummarizeRequest(raw []byte, path, method string) RequestSummary {
	s := RequestSummary{Method: method, Path: path, BodyBytes: len(raw)}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return s
	}
	s.ParseOK = true
	if m, ok := body["model"].(string); ok {
		s.Model = m
	}
	if b, ok := body["stream"].(bool); ok {
		s.Stream = b // body.stream === true only; other types are false
	}
	if mt, ok := body["max_tokens"].(float64); ok {
		v := int(mt)
		s.MaxTokens = &v
	}
	if msgs, ok := body["messages"].([]any); ok {
		n := len(msgs)
		s.MessageCount = &n
	}
	return s
}

// ResponseHasEmptyOutput reports whether a 200 response carries no usable
// model output (utils.mjs responseHasEmptyOutput): an Anthropic content array
// with no text-carrying part, or an OpenAI choices[0].message.content == "".
// Non-200 statuses, empty bodies and unparseable JSON are never "empty".
func ResponseHasEmptyOutput(statusCode int, body []byte) bool {
	if statusCode != 200 || len(body) == 0 {
		return false
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	if parts, ok := parsed["content"].([]any); ok {
		// content.every(part => part?.type !== "text" || !part.text): empty
		// arrays are vacuously "empty"; a part only fails when it is a text
		// part carrying a truthy (JS) text value.
		for _, part := range parts {
			pm, ok := part.(map[string]any)
			if !ok {
				continue // primitives / null: part?.type is undefined ≠ "text"
			}
			if t, _ := pm["type"].(string); t != "text" {
				continue
			}
			if jsFalsy(pm["text"]) {
				continue
			}
			return false
		}
		return true
	}
	choices, ok := parsed["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return false
	}
	msg, ok := first["message"].(map[string]any)
	if !ok {
		return false
	}
	content, ok := msg["content"].(string)
	return ok && content == ""
}

// jsFalsy mirrors JavaScript truthiness for JSON values (the `!part.text`
// check): nil, false, "", 0 are falsy; everything else is truthy. (NaN cannot
// appear in JSON.)
func jsFalsy(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case bool:
		return !t
	case string:
		return t == ""
	case float64:
		return t == 0
	default:
		return false
	}
}

// InjectPrompt prepends prompt into the request body (utils.mjs injectPrompt):
// `/v1/messages` bodies get the system prompt merged (a string system is
// prefixed with `prompt\n\n`, an array system has a text part unshifted, an
// absent system is created), `/v1/chat/completions` bodies get a system
// message unshifted. On parse failure, an empty prompt or an empty body the
// original bytes are returned unchanged.
//
// Known deviation: the re-marshalled body is compact JSON with keys sorted
// alphabetically (encoding/json), whereas Node's JSON.stringify preserves the
// original key order. The JSON is semantically identical.
func InjectPrompt(raw []byte, path, prompt string) []byte {
	if prompt == "" || len(raw) == 0 {
		return raw
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return raw
	}
	if strings.HasPrefix(path, "/v1/messages") {
		switch system := body["system"].(type) {
		case string:
			body["system"] = prompt + "\n\n" + system
		case []any:
			body["system"] = append([]any{
				map[string]any{"type": "text", "text": prompt},
			}, system...)
		default:
			body["system"] = []any{
				map[string]any{"type": "text", "text": prompt},
			}
		}
	}
	if strings.HasPrefix(path, "/v1/chat/completions") {
		if msgs, ok := body["messages"].([]any); ok {
			body["messages"] = append([]any{
				map[string]any{"role": "system", "content": prompt},
			}, msgs...)
		}
	}
	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return out
}
