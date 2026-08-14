package proxy

import "encoding/json"

// Terminal SSE markers (utils.mjs SSE_EOM / SSE_DONE).
const (
	SSE_EOM  = "event: message_stop"
	SSE_DONE = "data: [DONE]"
)

// StreamFormat selects which protocol's terminal/finisher frames are emitted.
type StreamFormat string

const (
	StreamAnthropic StreamFormat = "anthropic"
	StreamOpenAI    StreamFormat = "openai"
)

// EomTail returns the terminal SSE tail for the active stream format
// (utils.mjs eomTail). Anthropic `messages` streams end with
// `event: message_stop\ndata: {}`; OpenAI `chat.completions` streams end with
// `data: [DONE]`. Any unrecognized format gets the Anthropic tail.
func EomTail(format StreamFormat) string {
	if format == StreamOpenAI {
		return "\ndata: [DONE]\n\n"
	}
	return "\n" + SSE_EOM + "\ndata: {}\n\n"
}

// anthropicAbnormalFinish / openaiAbnormalFinish are the synthetic "stream is
// over" sequences written on ABNORMAL ends (utils.mjs abnormalFinish,
// byte-for-byte). A finish-reason-bearing frame followed by the protocol
// terminal marker so every client class (opencode finalizes on finish_reason /
// message_delta, the Anthropic SDK needs message_stop, lax parsers need
// [DONE]) terminates cleanly. The error frame written before it surfaces the
// failure to strict SDKs.
const anthropicAbnormalFinish = "\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":0}}` + "\n\n" +
	"event: message_stop\n" +
	"data: {}\n\n"

const openaiAbnormalFinish = "\n" +
	`data: {"id":"proxy-eom","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// AbnormalFinish returns the synthetic "stream is over" sequence for abnormal
// ends (utils.mjs abnormalFinish).
func AbnormalFinish(format StreamFormat) string {
	if format == StreamOpenAI {
		return openaiAbnormalFinish
	}
	return anthropicAbnormalFinish
}

// openaiErrFrame is the OpenAI protocol error frame body, matching
// JSON.stringify({error:{message,type}}) key order: message before type.
type openaiErrFrame struct {
	Error openaiErrBody `json:"error"`
}

type openaiErrBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// anthropicErrFrame is the Anthropic protocol error frame body, matching
// JSON.stringify({type:"error",error:{type,message}}) key order.
type anthropicErrFrame struct {
	Type  string           `json:"type"`
	Error anthropicErrBody `json:"error"`
}

type anthropicErrBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// SSEErrorFrame returns the protocol error frame written BEFORE the terminal
// EOM when a stream ends abnormally (utils.mjs sseErrorFrame). Strict SDKs
// surface these as stream errors instead of treating a truncated answer as a
// clean completion; the EOM tail still terminates lax parsers.
//
// The message is embedded as-is (JSON-escaped like JSON.stringify, including
// `<`, `>`, `&`). An empty message yields `"message":""` exactly as the Node
// version would for an explicit "" argument.
func SSEErrorFrame(format StreamFormat, message string) string {
	if format == StreamOpenAI {
		return "data: " + jsonString(openaiErrFrame{
			Error: openaiErrBody{Message: message, Type: "proxy_error"},
		}) + "\n\n"
	}
	return "event: error\ndata: " + jsonString(anthropicErrFrame{
		Type:  "error",
		Error: anthropicErrBody{Type: "proxy_error", Message: message},
	}) + "\n\n"
}

// jsonString marshals v as compact JSON with HTML escaping enabled (matching
// JSON.stringify's escaping of `<`, `>`, `&`). Marshal cannot fail for the
// static shapes used above.
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
