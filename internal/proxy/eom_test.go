package proxy

import (
	"strings"
	"testing"
)

func TestTerminalMarkers(t *testing.T) {
	if SSE_EOM != "event: message_stop" {
		t.Errorf("SSE_EOM = %q", SSE_EOM)
	}
	if SSE_DONE != "data: [DONE]" {
		t.Errorf("SSE_DONE = %q", SSE_DONE)
	}
	if MaxBodySize != 20*1024*1024 {
		t.Errorf("MaxBodySize = %d, want %d", MaxBodySize, 20*1024*1024)
	}
}

// EomTail strings are byte-for-byte contracts, assert against literals.
func TestEomTail(t *testing.T) {
	cases := []struct {
		name   string
		format StreamFormat
		want   string
	}{
		{"anthropic", StreamAnthropic, "\nevent: message_stop\ndata: {}\n\n"},
		{"openai", StreamOpenAI, "\ndata: [DONE]\n\n"},
		{"unknown defaults to anthropic", StreamFormat("bogus"), "\nevent: message_stop\ndata: {}\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EomTail(tc.format); got != tc.want {
				t.Errorf("EomTail(%q) = %q, want %q", tc.format, got, tc.want)
			}
		})
	}
}

func TestSSEErrorFrame(t *testing.T) {
	cases := []struct {
		name    string
		format  StreamFormat
		message string
		want    string
	}{
		{
			"anthropic default message",
			StreamAnthropic,
			"proxy stream error",
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"proxy_error\",\"message\":\"proxy stream error\"}}\n\n",
		},
		{
			"openai default message",
			StreamOpenAI,
			"proxy stream error",
			"data: {\"error\":{\"message\":\"proxy stream error\",\"type\":\"proxy_error\"}}\n\n",
		},
		{
			"anthropic custom reason",
			StreamAnthropic,
			"sse_idle_timeout",
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"proxy_error\",\"message\":\"sse_idle_timeout\"}}\n\n",
		},
		{
			"openai custom reason",
			StreamOpenAI,
			"upstream_closed",
			"data: {\"error\":{\"message\":\"upstream_closed\",\"type\":\"proxy_error\"}}\n\n",
		},
		{
			"explicit empty message stays empty",
			StreamOpenAI,
			"",
			"data: {\"error\":{\"message\":\"\",\"type\":\"proxy_error\"}}\n\n",
		},
		{
			"message with JSON-escapable chars",
			StreamOpenAI,
			"say \"hi\"\n<tag> & done",
			"data: {\"error\":{\"message\":\"say \\\"hi\\\"\\n\\u003ctag\\u003e \\u0026 done\",\"type\":\"proxy_error\"}}\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SSEErrorFrame(tc.format, tc.message); got != tc.want {
				t.Errorf("SSEErrorFrame(%q, %q) =\n  %q\nwant\n  %q", tc.format, tc.message, got, tc.want)
			}
		})
	}
}

func TestAbnormalFinish(t *testing.T) {
	cases := []struct {
		name   string
		format StreamFormat
		want   string
	}{
		{
			"anthropic",
			StreamAnthropic,
			"\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":0}}\n\nevent: message_stop\ndata: {}\n\n",
		},
		{
			"openai",
			StreamOpenAI,
			"\ndata: {\"id\":\"proxy-eom\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AbnormalFinish(tc.format); got != tc.want {
				t.Errorf("AbnormalFinish(%q) =\n  %q\nwant\n  %q", tc.format, got, tc.want)
			}
		})
	}
}

// Abnormal finish must end with the terminal marker for the format.
func TestAbnormalFinishTerminalTail(t *testing.T) {
	if !strings.HasSuffix(AbnormalFinish(StreamAnthropic), EomTail(StreamAnthropic)) {
		t.Errorf("anthropic finisher does not end with the EOM tail")
	}
	if !strings.HasSuffix(AbnormalFinish(StreamOpenAI), EomTail(StreamOpenAI)) {
		t.Errorf("openai finisher does not end with the EOM tail")
	}
}
