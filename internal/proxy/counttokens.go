package proxy

import (
	"encoding/json"
	"unicode/utf8"
)

// EstimateInputTokens approximates the input token count of an Anthropic
// /v1/messages body without calling the upstream. Deterministic and
// stdlib-only: text tokens are ceil(runeCount/4), plus a fixed overhead per
// message. Non-JSON or empty bodies count as 0.
func EstimateInputTokens(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0
	}
	tokens := 0
	if system, ok := body["system"]; ok {
		tokens += countContent(system)
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			tokens += 4 // per-message overhead
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if role, ok := msg["role"].(string); ok {
				tokens += textTokens(role)
			}
			if content, ok := msg["content"]; ok {
				tokens += countContent(content)
			}
		}
	}
	if tools, ok := body["tools"].([]any); ok {
		for _, t := range tools {
			tool, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if name, ok := tool["name"].(string); ok {
				tokens += textTokens(name)
			}
			if desc, ok := tool["description"].(string); ok {
				tokens += textTokens(desc)
			}
		}
	}
	return tokens
}

// countContent counts text inside a system or message content value: a plain
// string, or an array of blocks where text blocks and image blocks (url or
// base64 data) contribute text.
func countContent(v any) int {
	switch t := v.(type) {
	case string:
		return textTokens(t)
	case []any:
		total := 0
		for _, block := range t {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			total += countBlock(b)
		}
		return total
	default:
		return 0
	}
}

// countBlock counts the text-bearing fields of one content block, including
// the nested source of an image block.
func countBlock(b map[string]any) int {
	total := 0
	if txt, ok := b["text"].(string); ok {
		total += textTokens(txt)
	}
	if url, ok := b["url"].(string); ok {
		total += textTokens(url)
	}
	if data, ok := b["data"].(string); ok {
		total += textTokens(data)
	}
	if src, ok := b["source"].(map[string]any); ok {
		total += countBlock(src)
	}
	return total
}

// textTokens approximates tokens as ceil(runeCount/4).
func textTokens(s string) int {
	return (utf8.RuneCountInString(s) + 3) / 4
}
