package proxy

import (
	"fmt"
	"strings"
	"testing"
)

// classifierRecorder captures the classifier callbacks.
type classifierRecorder struct {
	stopCount int
	logs      []string
}

func newTestClassifier(t *testing.T) (*FrameClassifier, *classifierRecorder) {
	t.Helper()
	rec := &classifierRecorder{}
	c := NewFrameClassifier(func() { rec.stopCount++ }, func(format string, args ...any) {
		rec.logs = append(rec.logs, fmt.Sprintf(format, args...))
	})
	return c, rec
}

func TestFrameClassifierMessageStop(t *testing.T) {
	c, rec := newTestClassifier(t)
	c.Analyze([]byte("event: message_stop\ndata: {}\n\n"))
	if !c.SawMessageStop() {
		t.Error("SawMessageStop() = false, want true")
	}
	if !c.SawDataEvent() {
		t.Error("SawDataEvent() = false, want true (message_stop is a field line)")
	}
	if rec.stopCount != 1 {
		t.Errorf("onMessageStop called %d times, want 1", rec.stopCount)
	}
}

func TestFrameClassifierDone(t *testing.T) {
	c, rec := newTestClassifier(t)
	c.Analyze([]byte("data: [DONE]\n\n"))
	if !c.SawMessageStop() {
		t.Error("SawMessageStop() = false, want true")
	}
	if !c.SawDataEvent() {
		t.Error("SawDataEvent() = false, want true")
	}
	if rec.stopCount != 1 {
		t.Errorf("onMessageStop called %d times, want 1", rec.stopCount)
	}
}

func TestFrameClassifierPingIsNoise(t *testing.T) {
	c, _ := newTestClassifier(t)
	c.Analyze([]byte("event: ping\ndata: null\n\n"))
	if c.SawDataEvent() {
		t.Error("SawDataEvent() = true for event: ping, want false")
	}
	if c.SawMessageStop() {
		t.Error("SawMessageStop() = true for ping, want false")
	}
}

func TestFrameClassifierPingMixedBlockStillNoise(t *testing.T) {
	// A block containing event: ping is noise even when it has other lines.
	c, _ := newTestClassifier(t)
	c.Analyze([]byte("event: ping\ndata: {\"x\":1}\n\n"))
	if c.SawDataEvent() {
		t.Error("SawDataEvent() = true for a ping-containing block, want false")
	}
}

func TestFrameClassifierCommentOnlyIsNoise(t *testing.T) {
	c, _ := newTestClassifier(t)
	c.Analyze([]byte(": keepalive tick\n\n"))
	if c.SawDataEvent() {
		t.Error("SawDataEvent() = true for comment-only block, want false")
	}
	if c.SawMessageStop() {
		t.Error("SawMessageStop() = true for comment-only block, want false")
	}
}

func TestFrameClassifierRealDataIsData(t *testing.T) {
	c, _ := newTestClassifier(t)
	c.Analyze([]byte("data: {\"type\":\"content_block_delta\"}\n\n"))
	if !c.SawDataEvent() {
		t.Error("SawDataEvent() = false for a real data block, want true")
	}
}

func TestFrameClassifierBlockSplitAcrossCalls(t *testing.T) {
	c, rec := newTestClassifier(t)
	c.Analyze([]byte("event: mess"))
	if c.SawMessageStop() {
		t.Error("SawMessageStop() true before the block completed")
	}
	c.Analyze([]byte("age_stop\n\ndata: {}"))
	if !c.SawMessageStop() {
		t.Error("SawMessageStop() = false after the marker completed across calls")
	}
	if rec.stopCount != 1 {
		t.Errorf("onMessageStop called %d times, want 1", rec.stopCount)
	}
}

func TestFrameClassifierCRLFNormalization(t *testing.T) {
	c, _ := newTestClassifier(t)
	c.Analyze([]byte("data: [DONE]\r\n\r\n"))
	if !c.SawMessageStop() {
		t.Error("SawMessageStop() = false for CRLF-terminated frame, want true")
	}
	// Lone \r is also a line break (second replace in the Node regex chain).
	c2, _ := newTestClassifier(t)
	c2.Analyze([]byte("data: [DONE]\r\r"))
	if !c2.SawMessageStop() {
		t.Error("SawMessageStop() = false for \\r-terminated frame, want true")
	}
}

func TestFrameClassifierFlushPartialCarry(t *testing.T) {
	c, _ := newTestClassifier(t)
	c.Analyze([]byte("data: hello")) // no trailing blank line yet
	if c.SawDataEvent() {
		t.Error("SawDataEvent() = true before Flush")
	}
	c.Flush()
	if !c.SawDataEvent() {
		t.Error("SawDataEvent() = false after Flush of partial block, want true")
	}
	if c.SawMessageStop() {
		t.Error("SawMessageStop() = true for a non-terminal partial")
	}
}

func TestFrameClassifierFlushPartialTerminal(t *testing.T) {
	// A complete terminal frame missing only its trailing blank line is
	// classified by Flush.
	c, rec := newTestClassifier(t)
	c.Analyze([]byte("event: message_stop"))
	if c.SawMessageStop() {
		t.Error("SawMessageStop() = true before Flush")
	}
	c.Flush()
	if !c.SawMessageStop() {
		t.Error("SawMessageStop() = false after Flush of partial terminal, want true")
	}
	if rec.stopCount != 1 {
		t.Errorf("onMessageStop called %d times, want 1", rec.stopCount)
	}
}

func TestFrameClassifierFlushTruncatedTerminalIsNotTerminal(t *testing.T) {
	// A genuinely truncated marker (missing a character) is never terminal,
	// exactly as Node's exact-value compare behaves.
	c, _ := newTestClassifier(t)
	c.Analyze([]byte("event: message_sto"))
	c.Flush()
	if c.SawMessageStop() {
		t.Error("SawMessageStop() = true for truncated marker, want false")
	}
}

func TestFrameClassifierFlushNoCarry(t *testing.T) {
	c, _ := newTestClassifier(t)
	c.Flush() // no-op
	if c.SawDataEvent() || c.SawMessageStop() {
		t.Error("Flush with no carry must not classify anything")
	}
}

func TestFrameClassifierTokenUsageOpenAI(t *testing.T) {
	c, rec := newTestClassifier(t)
	c.Analyze([]byte("data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n"))
	if len(rec.logs) != 1 {
		t.Fatalf("got %d log lines, want 1: %v", len(rec.logs), rec.logs)
	}
	if rec.logs[0] != "TOKEN USAGE: input_tokens=10, output_tokens=5" {
		t.Errorf("log = %q", rec.logs[0])
	}
}

func TestFrameClassifierTokenUsageAnthropic(t *testing.T) {
	c, rec := newTestClassifier(t)
	c.Analyze([]byte("data: {\"message\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"))
	if len(rec.logs) != 1 || rec.logs[0] != "TOKEN USAGE: input_tokens=1, output_tokens=2" {
		t.Errorf("logs = %v, want [TOKEN USAGE: input_tokens=1, output_tokens=2]", rec.logs)
	}
}

func TestFrameClassifierTokenUsagePrecedence(t *testing.T) {
	// message.usage.input_tokens wins over usage.prompt_tokens.
	c, rec := newTestClassifier(t)
	c.Analyze([]byte("data: {\"message\":{\"usage\":{\"input_tokens\":7}},\"usage\":{\"prompt_tokens\":99}}\n\n"))
	if len(rec.logs) != 1 || rec.logs[0] != "TOKEN USAGE: input_tokens=7, output_tokens=N/A" {
		t.Errorf("logs = %v", rec.logs)
	}
}

func TestFrameClassifierTokenUsageOneSided(t *testing.T) {
	c, rec := newTestClassifier(t)
	c.Analyze([]byte("data: {\"usage\":{\"completion_tokens\":3}}\n\n"))
	if len(rec.logs) != 1 || rec.logs[0] != "TOKEN USAGE: input_tokens=N/A, output_tokens=3" {
		t.Errorf("logs = %v", rec.logs)
	}
}

func TestFrameClassifierTokenUsageAbsent(t *testing.T) {
	c, rec := newTestClassifier(t)
	c.Analyze([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n"))
	if len(rec.logs) != 0 {
		t.Errorf("logs = %v, want none", rec.logs)
	}
}

func TestFrameClassifierTokenUsageInvalidJSON(t *testing.T) {
	c, rec := newTestClassifier(t)
	c.Analyze([]byte("data: not-json\n\n"))
	if len(rec.logs) != 0 {
		t.Errorf("logs = %v, want none", rec.logs)
	}
	// But the block still counts as a data event.
	if !c.SawDataEvent() {
		t.Error("SawDataEvent() = false for a data block with invalid JSON")
	}
}

func TestFrameClassifierTokenUsageNullTokens(t *testing.T) {
	// null values are treated as absent (JS nullish coalescing).
	c, rec := newTestClassifier(t)
	c.Analyze([]byte("data: {\"usage\":{\"prompt_tokens\":null,\"input_tokens\":4}}\n\n"))
	if len(rec.logs) != 1 || rec.logs[0] != "TOKEN USAGE: input_tokens=4, output_tokens=N/A" {
		t.Errorf("logs = %v", rec.logs)
	}
}

func TestFrameClassifierMultipleBlocksOneCall(t *testing.T) {
	c, rec := newTestClassifier(t)
	c.Analyze([]byte("event: ping\n\n: comment\n\n" +
		"event: message_stop\n\ndata: [DONE]\n\n"))
	if !c.SawMessageStop() {
		t.Error("SawMessageStop() = false, want true")
	}
	if !c.SawDataEvent() {
		t.Error("SawDataEvent() = false, want true (message_stop/[DONE] blocks)")
	}
	if rec.stopCount != 2 {
		t.Errorf("onMessageStop called %d times, want 2", rec.stopCount)
	}
}

func TestFrameClassifierNilCallbacks(t *testing.T) {
	c := NewFrameClassifier(nil, nil)
	c.Analyze([]byte("event: message_stop\n\ndata: {\"usage\":{\"prompt_tokens\":1}}\n\n"))
	if !c.SawMessageStop() {
		t.Error("SawMessageStop() = false, want true")
	}
}

func TestFrameClassifierEmptyInput(t *testing.T) {
	c, _ := newTestClassifier(t)
	c.Analyze(nil)
	c.Analyze([]byte(""))
	if c.SawDataEvent() || c.SawMessageStop() {
		t.Error("empty input must not classify anything")
	}
}

func TestFrameClassifierBlockWithOnlyEventLineCountsAsData(t *testing.T) {
	// A bare `event: message_stop` line (no data:) is still a field line.
	c, _ := newTestClassifier(t)
	c.Analyze([]byte("event: message_stop\n\n"))
	if !c.SawDataEvent() {
		t.Error("SawDataEvent() = false, want true")
	}
}

// Sanity check: partial-frame carry must be retained across analyzes.
func TestFrameClassifierCarryAcrossManyCalls(t *testing.T) {
	c, _ := newTestClassifier(t)
	for _, piece := range strings.Split("data: {\"usage\":{\"prompt_tokens\":1}}\n\nrest", "") {
		c.Analyze([]byte(piece))
	}
	if !c.SawDataEvent() {
		t.Error("SawDataEvent() = false after byte-at-a-time feeding")
	}
	c.Flush()
	if c.carry != "" {
		t.Errorf("carry not drained after Flush: %q", c.carry)
	}
}

