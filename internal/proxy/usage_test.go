package proxy

import "testing"

func TestUsageFromFrame(t *testing.T) {
	cases := []struct {
		name string
		data string
		want Usage
	}{
		{
			"anthropic message_start",
			`{"type":"message_start","message":{"usage":{"input_tokens":25,"cache_creation_input_tokens":100,"cache_read_input_tokens":200}}}`,
			Usage{Input: 25, CacheWrite: 100, CacheRead: 200},
		},
		{
			"anthropic message_start null cache",
			`{"type":"message_start","message":{"usage":{"input_tokens":25,"cache_creation_input_tokens":null,"cache_read_input_tokens":null}}}`,
			Usage{Input: 25},
		},
		{
			"anthropic message_delta",
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
			Usage{Output: 42},
		},
		{
			"anthropic message_delta with cache read",
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42,"cache_read_input_tokens":7}}`,
			Usage{Output: 42, CacheRead: 7},
		},
		{
			"openai chunk",
			`{"choices":[{"delta":{"content":"hi"}}],"usage":{"input_tokens":10,"output_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}}`,
			Usage{Input: 10, Output: 5, CacheRead: 3},
		},
		{
			"openai chunk cached only",
			`{"usage":{"prompt_tokens_details":{"cached_tokens":7}}}`,
			Usage{CacheRead: 7},
		},
		{
			"openai chunk null usage",
			`{"choices":[{"delta":{}}],"usage":null}`,
			Usage{},
		},
		{
			"no usage fields",
			`{"type":"content_block_delta","delta":{"text":"x"}}`,
			Usage{},
		},
		{
			"empty usage object",
			`{"usage":{}}`,
			Usage{},
		},
		{
			"non-number fields ignored",
			`{"usage":{"input_tokens":"many","output_tokens":null}}`,
			Usage{},
		},
		{
			"not json",
			`not json`,
			Usage{},
		},
		{
			"empty payload",
			``,
			Usage{},
		},
		{
			"null top level",
			`null`,
			Usage{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UsageFromFrame([]byte(tc.data))
			if got != tc.want {
				t.Errorf("UsageFromFrame(%q) = %+v, want %+v", tc.data, got, tc.want)
			}
		})
	}
}

func TestUsageFromBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Usage
	}{
		{
			"anthropic message",
			`{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":15,"output_tokens":9,"cache_creation_input_tokens":50,"cache_read_input_tokens":25}}`,
			Usage{Input: 15, Output: 9, CacheWrite: 50, CacheRead: 25},
		},
		{
			"anthropic message no cache fields",
			`{"content":[],"usage":{"input_tokens":15,"output_tokens":9}}`,
			Usage{Input: 15, Output: 9},
		},
		{
			"openai completion",
			`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":11,"completion_tokens":6,"prompt_tokens_details":{"cached_tokens":4}}}`,
			Usage{Input: 11, Output: 6, CacheRead: 4},
		},
		{
			"openai input_tokens style",
			`{"usage":{"input_tokens":3,"output_tokens":2}}`,
			Usage{Input: 3, Output: 2},
		},
		{
			"no usage",
			`{"choices":[]}`,
			Usage{},
		},
		{
			"null usage",
			`{"usage":null}`,
			Usage{},
		},
		{
			"malformed json",
			`{`,
			Usage{},
		},
		{
			"empty object",
			`{}`,
			Usage{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UsageFromBody([]byte(tc.body))
			if got != tc.want {
				t.Errorf("UsageFromBody(%q) = %+v, want %+v", tc.body, got, tc.want)
			}
		})
	}
}
