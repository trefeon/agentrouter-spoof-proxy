package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FrameClassifier is the carry-over SSE frame parser ported from stream.mjs
// l.100-165. SSE blocks are blank-line separated; each block is classified as
// terminal (`event: message_stop` / `data: [DONE]`), data (at least one
// non-comment field line) or noise (comment lines `: …` only, or `event: ping`
// liveness frames). Raw chunks are forwarded verbatim regardless of this
// analysis — the classifier only tracks what has been seen, so short events
// and markers split across TCP chunks still count.
type FrameClassifier struct {
	carry          string
	sawDataEvent   bool
	sawMessageStop bool
	onMessageStop  func()
	logDebug       func(format string, args ...any)
}

// NewFrameClassifier returns a classifier with empty carry. The callbacks may
// be nil: onMessageStop fires each time a terminal marker is classified;
// logDebug receives the token-usage line for data frames carrying usage
// fields.
func NewFrameClassifier(onMessageStop func(), logDebug func(format string, args ...any)) *FrameClassifier {
	return &FrameClassifier{onMessageStop: onMessageStop, logDebug: logDebug}
}

// Analyze ingests raw SSE text (chunk boundaries are irrelevant) and classifies
// every complete block. \r\n and lone \r are normalized to \n first, exactly
// like the Node regex replace.
func (c *FrameClassifier) Analyze(text []byte) {
	s := strings.ReplaceAll(string(text), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	c.carry += s
	for {
		idx := strings.Index(c.carry, "\n\n")
		if idx == -1 {
			break
		}
		block := c.carry[:idx]
		c.carry = c.carry[idx+2:]
		c.classifyBlock(block)
	}
}

// Flush classifies the remaining partial block at a clean stream end (the
// Node flushCarry appends a synthetic blank line first).
func (c *FrameClassifier) Flush() {
	if c.carry == "" {
		return
	}
	pending := c.carry
	c.carry = ""
	c.classifyBlock(pending + "\n\n")
}

// SawDataEvent reports whether at least one real SSE event block arrived
// (comment-only keepalive lines and `event: ping` frames are NOT data).
func (c *FrameClassifier) SawDataEvent() bool {
	return c.sawDataEvent
}

// SawMessageStop reports whether a terminal marker was seen (`event:
// message_stop` or `data: [DONE]`), across chunk boundaries.
func (c *FrameClassifier) SawMessageStop() bool {
	return c.sawMessageStop
}

func (c *FrameClassifier) classifyBlock(block string) {
	if strings.TrimSpace(block) == "" {
		return
	}
	hasField := false
	isPing := false
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, ":") {
			continue
		}
		hasField = true
		if strings.HasPrefix(strings.ToLower(trimmed), "event:") &&
			strings.TrimSpace(trimmed[6:]) == "ping" {
			// Liveness frames (`event: ping`) are noise, not model data: they
			// keep the connection alive but must not skew empty-stream
			// detection or the stall-watchdog reason.
			isPing = true
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "data:") {
			dataStr := strings.TrimSpace(trimmed[5:])
			if dataStr == "[DONE]" {
				c.sawMessageStop = true
				if c.onMessageStop != nil {
					c.onMessageStop()
				}
			} else if dataStr != "" {
				c.tokenUsage(dataStr)
			}
		} else if strings.HasPrefix(lower, "event:") {
			if strings.TrimSpace(trimmed[6:]) == "message_stop" {
				c.sawMessageStop = true
				if c.onMessageStop != nil {
					c.onMessageStop()
				}
			}
		}
	}
	if hasField && !isPing {
		c.sawDataEvent = true
	}
}

// tokenUsage mirrors the debug log of stream.mjs: parse the data: payload and,
// when usage tokens are present, emit
// `TOKEN USAGE: input_tokens=X, output_tokens=Y` (X/Y are "N/A" when only the
// other side was found). Null values are treated as absent, like the Node
// nullish-coalescing chain.
func (c *FrameClassifier) tokenUsage(dataStr string) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(dataStr), &parsed); err != nil {
		return
	}
	input, inputOK := tokenAt(parsed, []string{"message", "usage", "input_tokens"})
	if !inputOK {
		input, inputOK = tokenAt(parsed, []string{"usage", "prompt_tokens"})
	}
	if !inputOK {
		input, inputOK = tokenAt(parsed, []string{"usage", "input_tokens"})
	}
	output, outputOK := tokenAt(parsed, []string{"message", "usage", "output_tokens"})
	if !outputOK {
		output, outputOK = tokenAt(parsed, []string{"usage", "completion_tokens"})
	}
	if !outputOK {
		output, outputOK = tokenAt(parsed, []string{"usage", "output_tokens"})
	}
	if !inputOK && !outputOK {
		return
	}
	inputS, outputS := "N/A", "N/A"
	if inputOK {
		inputS = fmt.Sprint(input)
	}
	if outputOK {
		outputS = fmt.Sprint(output)
	}
	if c.logDebug != nil {
		c.logDebug("TOKEN USAGE: input_tokens=%s, output_tokens=%s", inputS, outputS)
	}
}

// tokenAt walks a JSON path, treating any missing key or null value (JS
// nullish) as absent.
func tokenAt(m map[string]any, path []string) (any, bool) {
	cur := any(m)
	for _, key := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := mm[key]
		if !ok || v == nil {
			return nil, false
		}
		cur = v
	}
	return cur, true
}
