package proxy

import (
	"encoding/json"
	"testing"
)

func intPtr(v int) *int { return &v }

// sameSummary compares two summaries value-wise (the *int fields would compare
// by address with `!=`).
func sameSummary(a, b RequestSummary) bool {
	if a.Method != b.Method || a.Path != b.Path || a.BodyBytes != b.BodyBytes ||
		a.ParseOK != b.ParseOK || a.Model != b.Model || a.Stream != b.Stream {
		return false
	}
	switch {
	case a.MaxTokens == nil && b.MaxTokens == nil:
	case a.MaxTokens == nil || b.MaxTokens == nil:
		return false
	case *a.MaxTokens != *b.MaxTokens:
		return false
	}
	switch {
	case a.MessageCount == nil && b.MessageCount == nil:
	case a.MessageCount == nil || b.MessageCount == nil:
		return false
	case *a.MessageCount != *b.MessageCount:
		return false
	}
	return true
}

func TestSummarizeRequest(t *testing.T) {
	fullBody := `{"model":"gpt-5.6-sol","stream":true,"max_tokens":100,"messages":[{"a":1},{"b":2}]}`
	cases := []struct {
		name string
		raw  string
		path string
		meth string
		want RequestSummary
	}{
		{
			name: "full summary",
			raw:  fullBody,
			path: "/v1/messages",
			meth: "POST",
			want: RequestSummary{
				Method: "POST", Path: "/v1/messages", BodyBytes: len(fullBody),
				ParseOK: true, Model: "gpt-5.6-sol", Stream: true,
				MaxTokens: intPtr(100), MessageCount: intPtr(2),
			},
		},
		{
			name: "stream false is false",
			raw:  `{"stream":false}`,
			path: "/v1/messages",
			meth: "POST",
			want: RequestSummary{Method: "POST", Path: "/v1/messages",
				BodyBytes: len(`{"stream":false}`), ParseOK: true, Stream: false},
		},
		{
			name: "string stream value is not true",
			raw:  `{"stream":"true"}`,
			path: "/v1/messages",
			meth: "POST",
			want: RequestSummary{Method: "POST", Path: "/v1/messages",
				BodyBytes: len(`{"stream":"true"}`), ParseOK: true, Stream: false},
		},
		{
			name: "absent fields stay nil",
			raw:  `{}`,
			path: "/v1/chat/completions",
			meth: "GET",
			want: RequestSummary{Method: "GET", Path: "/v1/chat/completions",
				BodyBytes: 2, ParseOK: true},
		},
		{
			name: "non-string model ignored",
			raw:  `{"model":42}`,
			path: "/messages",
			meth: "POST",
			want: RequestSummary{Method: "POST", Path: "/messages",
				BodyBytes: len(`{"model":42}`), ParseOK: true},
		},
		{
			name: "non-number max_tokens ignored",
			raw:  `{"max_tokens":"100"}`,
			path: "/v1/messages",
			meth: "POST",
			want: RequestSummary{Method: "POST", Path: "/v1/messages",
				BodyBytes: len(`{"max_tokens":"100"}`), ParseOK: true},
		},
		{
			name: "non-array messages ignored",
			raw:  `{"messages":"oops"}`,
			path: "/v1/messages",
			meth: "POST",
			want: RequestSummary{Method: "POST", Path: "/v1/messages",
				BodyBytes: len(`{"messages":"oops"}`), ParseOK: true},
		},
		{
			name: "invalid JSON not parsed",
			raw:  `{not json`,
			path: "/v1/messages",
			meth: "POST",
			want: RequestSummary{Method: "POST", Path: "/v1/messages",
				BodyBytes: len(`{not json`), ParseOK: false},
		},
		{
			name: "empty body not parsed",
			raw:  ``,
			path: "/v1/messages",
			meth: "POST",
			want: RequestSummary{Method: "POST", Path: "/v1/messages",
				BodyBytes: 0, ParseOK: false},
		},
		{
			name: "max_tokens zero is a value",
			raw:  `{"max_tokens":0}`,
			path: "/v1/messages",
			meth: "POST",
			want: RequestSummary{Method: "POST", Path: "/v1/messages",
				BodyBytes: len(`{"max_tokens":0}`), ParseOK: true, MaxTokens: intPtr(0)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SummarizeRequest([]byte(tc.raw), tc.path, tc.meth)
			if !sameSummary(got, tc.want) {
				t.Errorf("SummarizeRequest(%q, %q, %q) = %+v, want %+v",
					tc.raw, tc.path, tc.meth, got, tc.want)
			}
		})
	}
}

func TestResponseHasEmptyOutput(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"anthropic empty content array", 200, `{"content":[]}`, true},
		{"anthropic all-empty text parts", 200, `{"content":[{"type":"text","text":""}]}`, true},
		{"anthropic thinking only", 200, `{"content":[{"type":"thinking","text":"secret"}]}`, true},
		{"anthropic mixed thinking and empty text", 200, `{"content":[{"type":"thinking","text":"x"},{"type":"text","text":""}]}`, true},
		{"anthropic non-empty text", 200, `{"content":[{"type":"text","text":"hi"}]}`, false},
		{"anthropic empty text plus non-empty text", 200, `{"content":[{"type":"text","text":""},{"type":"text","text":"x"}]}`, false},
		{"anthropic null part passes", 200, `{"content":[null,{"type":"text","text":""}]}`, true},
		{"anthropic primitive part passes", 200, `{"content":[42,{"type":"text","text":""}]}`, true},
		{"anthropic missing text is empty", 200, `{"content":[{"type":"text"}]}`, true},
		{"openai empty content", 200, `{"choices":[{"message":{"content":""}}]}`, true},
		{"openai non-empty content", 200, `{"choices":[{"message":{"content":"hi"}}]}`, false},
		{"openai no choices", 200, `{}`, false},
		{"openai empty choices", 200, `{"choices":[]}`, false},
		{"openai message missing", 200, `{"choices":[{}]}`, false},
		{"openai content missing", 200, `{"choices":[{"message":{}}]}`, false},
		{"non-200 status", 201, `{"content":[]}`, false},
		{"empty body", 200, ``, false},
		{"unparseable body", 200, `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResponseHasEmptyOutput(tc.status, []byte(tc.body)); got != tc.want {
				t.Errorf("ResponseHasEmptyOutput(%d, %q) = %v, want %v",
					tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// asMap re-parses an InjectPrompt result so assertions are order-independent
// (Go marshals maps with sorted keys; Node preserves insertion order).
func asMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("InjectPrompt returned invalid JSON %q: %v", raw, err)
	}
	return m
}

func TestInjectPrompt(t *testing.T) {
	const prompt = "You are a helpful assistant."

	t.Run("empty prompt returns original bytes", func(t *testing.T) {
		raw := []byte(`{"model":"x"}`)
		if got := InjectPrompt(raw, "/v1/messages", ""); string(got) != string(raw) {
			t.Errorf("got %q, want %q", got, raw)
		}
	})
	t.Run("empty body returns original bytes", func(t *testing.T) {
		if got := InjectPrompt(nil, "/v1/messages", prompt); got != nil {
			t.Errorf("got %q, want nil", got)
		}
	})
	t.Run("invalid JSON passes through unchanged", func(t *testing.T) {
		raw := []byte(`{not json`)
		if got := InjectPrompt(raw, "/v1/messages", prompt); string(got) != string(raw) {
			t.Errorf("got %q, want %q", got, raw)
		}
	})
	t.Run("null body passes through unchanged", func(t *testing.T) {
		raw := []byte(`null`)
		if got := InjectPrompt(raw, "/v1/messages", prompt); string(got) != string(raw) {
			t.Errorf("got %q, want %q", got, raw)
		}
	})

	t.Run("anthropic string system is prefixed", func(t *testing.T) {
		got := asMap(t, InjectPrompt([]byte(`{"system":"Be concise.","model":"x"}`), "/v1/messages", prompt))
		want := prompt + "\n\n" + "Be concise."
		if s, _ := got["system"].(string); s != want {
			t.Errorf("system = %q, want %q", s, want)
		}
	})
	t.Run("anthropic array system gets text part unshifted", func(t *testing.T) {
		got := asMap(t, InjectPrompt([]byte(`{"system":[{"type":"text","text":"old"}]}`), "/v1/messages", prompt))
		sys, ok := got["system"].([]any)
		if !ok || len(sys) != 2 {
			t.Fatalf("system = %v, want 2-element array", got["system"])
		}
		first, ok := sys[0].(map[string]any)
		if !ok || first["type"] != "text" || first["text"] != prompt {
			t.Errorf("first system part = %v, want {type:text text:%q}", sys[0], prompt)
		}
	})
	t.Run("anthropic absent system is created", func(t *testing.T) {
		got := asMap(t, InjectPrompt([]byte(`{"model":"x"}`), "/v1/messages", prompt))
		sys, ok := got["system"].([]any)
		if !ok || len(sys) != 1 {
			t.Fatalf("system = %v, want 1-element array", got["system"])
		}
		first, ok := sys[0].(map[string]any)
		if !ok || first["type"] != "text" || first["text"] != prompt {
			t.Errorf("system part = %v, want {type:text text:%q}", sys[0], prompt)
		}
	})
	t.Run("anthropic null system is created", func(t *testing.T) {
		got := asMap(t, InjectPrompt([]byte(`{"system":null}`), "/v1/messages", prompt))
		if _, ok := got["system"].([]any); !ok {
			t.Errorf("system = %v, want array", got["system"])
		}
	})
	t.Run("chat messages get system message unshifted", func(t *testing.T) {
		got := asMap(t, InjectPrompt([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "/v1/chat/completions", prompt))
		msgs, ok := got["messages"].([]any)
		if !ok || len(msgs) != 2 {
			t.Fatalf("messages = %v, want 2-element array", got["messages"])
		}
		first, ok := msgs[0].(map[string]any)
		if !ok || first["role"] != "system" || first["content"] != prompt {
			t.Errorf("first message = %v, want {role:system content:%q}", msgs[0], prompt)
		}
		if second, ok := msgs[1].(map[string]any); !ok || second["role"] != "user" {
			t.Errorf("second message = %v, want the original user message", msgs[1])
		}
	})
	t.Run("chat without messages array unchanged", func(t *testing.T) {
		raw := []byte(`{"model":"x"}`)
		got := asMap(t, InjectPrompt(raw, "/v1/chat/completions", prompt))
		if _, has := got["messages"]; has {
			t.Errorf("messages should be absent, got %v", got["messages"])
		}
	})
	t.Run("unrelated path unchanged", func(t *testing.T) {
		raw := []byte(`{"system":"keep","model":"x"}`)
		got := asMap(t, InjectPrompt(raw, "/health", prompt))
		if s, _ := got["system"].(string); s != "keep" {
			t.Errorf("system = %q, want unchanged", s)
		}
	})
	t.Run("anthropic system untouched on chat path", func(t *testing.T) {
		raw := []byte(`{"system":"keep"}`)
		got := asMap(t, InjectPrompt(raw, "/v1/chat/completions", prompt))
		if s, _ := got["system"].(string); s != "keep" {
			t.Errorf("system = %q, want unchanged", s)
		}
	})
}
