package proxy

import "testing"

func TestEstimateInputTokens(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "empty body", body: "", want: 0},
		{name: "non-JSON body", body: "not json", want: 0},
		// One message: 4 overhead + role "user" (4 runes, ceil(4/4)=1) +
		// content "hello" (5 runes, ceil(5/4)=2) = 7.
		{name: "single message", body: `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello"}]}`, want: 7},
		// System "Be brief" (8 runes, 2) + message: 4 + 1 + "hi" (1) = 8.
		{name: "system string plus messages", body: `{"system":"Be brief","messages":[{"role":"user","content":"hi"}]}`, want: 8},
		// Message: 4 + 1 + text block "hello world" (11 runes, 3) = 8.
		{name: "content as block array", body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hello world"}]}]}`, want: 8},
		// First: 4 + 1 + 1 = 6; second: 4 + "assistant" (3) + 1 = 8; total 14.
		{name: "multiple messages grow count", body: `{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`, want: 14},
		// name "get_weather" (11 runes, 3) + description "Gets weather" (12 runes, 3).
		{name: "tool definitions", body: `{"tools":[{"name":"get_weather","description":"Gets weather"}]}`, want: 6},
		// Message: 4 + 1 + image block url "https://x.io/i.png" (18 runes, 5) = 10.
		{name: "image block", body: `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://x.io/i.png"}}]}]}`, want: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EstimateInputTokens([]byte(tt.body)); got != tt.want {
				t.Fatalf("EstimateInputTokens(%q) = %d, want %d", tt.body, got, tt.want)
			}
		})
	}
}

func TestEstimateInputTokensDeterministic(t *testing.T) {
	body := []byte(`{"system":"Be brief","messages":[{"role":"user","content":"hello"}]}`)
	first := EstimateInputTokens(body)
	for i := 0; i < 10; i++ {
		if got := EstimateInputTokens(body); got != first {
			t.Fatalf("non-deterministic estimate: got %d, then %d", first, got)
		}
	}
}
