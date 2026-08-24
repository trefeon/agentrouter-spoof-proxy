package proxy

import (
	"bytes"
	"testing"
)

// run feeds all chunks through a fresh stripper and concatenates the output.
func run(t *testing.T, chunks ...string) string {
	t.Helper()
	s := NewThinkStripper()
	var out bytes.Buffer
	for _, c := range chunks {
		out.Write(s.Process([]byte(c)))
	}
	if s.Unterminated() {
		t.Fatalf("stripper left an unclosed <think> span after %q", chunks)
	}
	return out.String()
}

func TestThinkStripperNoTags(t *testing.T) {
	if got := run(t, "Hello, world!"); got != "Hello, world!" {
		t.Errorf("got %q, want %q", got, "Hello, world!")
	}
	if got := run(t, "chunk one ", "chunk two"); got != "chunk one chunk two" {
		t.Errorf("got %q", got)
	}
}

func TestThinkStripperSingleChunkSpan(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"middle span", "a<think>secret</think>b", "ab"},
		{"leading span", "<think>secret</think>b", "b"},
		{"trailing span", "a<think>secret</think>", "a"},
		{"whole span", "<think>secret</think>", ""},
		{"multiple spans", "a<think>1</think>b<think>2</think>c", "abc"},
		{"adjacent spans", "<think>1</think><think>2</think>", ""},
		{"empty span", "a<think></think>b", "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(t, tc.in); got != tc.want {
				t.Errorf("run(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestThinkStripperSplitOpenTag(t *testing.T) {
	// "<thi" plus "nk>SECRET response...", tag split across TCP chunks.
	if got := run(t, "a<thi", "nk>SECRET response</think>tail"); got != "atail" {
		t.Errorf("got %q, want %q", got, "atail")
	}
	// Single-character splits.
	if got := run(t, "<", "t", "h", "i", "n", "k>hidden</think>out"); got != "out" {
		t.Errorf("got %q, want %q", got, "out")
	}
}

func TestThinkStripperSplitCloseTag(t *testing.T) {
	// "<think>abc</th" plus "ink>rest", close tag split across chunks.
	s := NewThinkStripper()
	if got := string(s.Process([]byte("x<think>abc</th"))); got != "x" {
		t.Errorf("first chunk forward = %q, want %q", got, "x")
	}
	if got := string(s.Process([]byte("ink>rest"))); got != "rest" {
		t.Errorf("second chunk forward = %q, want %q", got, "rest")
	}
	if s.Unterminated() {
		t.Error("unterminated after close tag completed")
	}
}

func TestThinkStripperMultiByteUTF8(t *testing.T) {
	// Single chunk: CJK around a span, bytes must pass through untouched.
	if got := run(t, "你好<think>x</think>世界"); got != "你好世界" {
		t.Errorf("got %q, want %q", got, "你好世界")
	}
	// Emoji adjacency.
	if got := run(t, "👋<think>x</think>🚀"); got != "👋🚀" {
		t.Errorf("got %q, want %q", got, "👋🚀")
	}
	// Multibyte char in its own chunk, then a tag prefix holdback after it:
	// the held bytes are pure ASCII, so the CJK rune is never split.
	s := NewThinkStripper()
	if got := string(s.Process([]byte("abc你<th"))); got != "abc你" {
		t.Errorf("forward before holdback = %q, want %q", got, "abc你")
	}
	if got := string(s.Process([]byte("ink>SEC</think>end"))); got != "end" {
		t.Errorf("forward after split tag = %q, want %q", got, "end")
	}
}

func TestThinkStripperUnterminatedAtEnd(t *testing.T) {
	s := NewThinkStripper()
	if got := string(s.Process([]byte("a<think>secret"))); got != "a" {
		t.Errorf("clean prefix = %q, want %q", got, "a")
	}
	if !s.Unterminated() {
		t.Fatal("expected Unterminated() == true")
	}
	forward, unterminated := s.Flush()
	if unterminated != true || forward != nil {
		t.Errorf("Flush() = (%q, %v), want (nil, true)", forward, unterminated)
	}
}

func TestThinkStripperSplitSpanNeverForwardsContent(t *testing.T) {
	// Span opened in one chunk and closed in another: only clean prefixes
	// forward, the span content is never leaked.
	s := NewThinkStripper()
	if got := string(s.Process([]byte("<think>part1 "))); got != "" {
		t.Errorf("chunk 1 forward = %q, want empty", got)
	}
	if got := string(s.Process([]byte("part2 </think>done"))); got != "done" {
		t.Errorf("chunk 2 forward = %q, want %q", got, "done")
	}
	if s.Unterminated() {
		t.Error("unterminated after clean close")
	}
}

func TestThinkStripperTagPrefixPendingFlush(t *testing.T) {
	// Stream ends while a "<thi" prefix is held: never completed into a real
	// tag, so it is legitimate content and must be forwarded on Flush.
	s := NewThinkStripper()
	if got := string(s.Process([]byte("hello<thi"))); got != "hello" {
		t.Errorf("forward = %q, want %q", got, "hello")
	}
	if s.Unterminated() {
		t.Error("held prefix is not an open span")
	}
	forward, unterminated := s.Flush()
	if unterminated != false || string(forward) != "<thi" {
		t.Errorf("Flush() = (%q, %v), want (%q, false)", forward, unterminated, "<thi")
	}
}

func TestThinkStripperPrefixMaterializesOnNextChunk(t *testing.T) {
	// Held "<thi" + a next chunk that does not continue the tag: bytes come
	// through unchanged.
	if got := run(t, "abc<thi", "ZZZ"); got != "abc<thiZZZ" {
		t.Errorf("got %q, want %q", got, "abc<thiZZZ")
	}
}

func TestThinkStripperNestedOpenTags(t *testing.T) {
	// A second <think> inside a span does not reopen: everything up to the
	// first </think> is stripped.
	if got := run(t, "<think>a<think>b</think>c"); got != "c" {
		t.Errorf("got %q, want %q", got, "c")
	}
}

func TestThinkStripperEmptyChunks(t *testing.T) {
	s := NewThinkStripper()
	if got := s.Process(nil); len(got) != 0 {
		t.Errorf("Process(nil) = %q, want empty", got)
	}
	if got := s.Process([]byte("")); len(got) != 0 {
		t.Errorf("Process(empty) = %q, want empty", got)
	}
	if s.Unterminated() {
		t.Error("empty chunks must not open a span")
	}
	if forward, unterminated := s.Flush(); forward != nil || unterminated {
		t.Errorf("Flush() = (%q, %v), want (nil, false)", forward, unterminated)
	}
}

func TestThinkStripperInputNotMutated(t *testing.T) {
	in := []byte("a<think>x</think>b")
	cp := bytes.Clone(in)
	s := NewThinkStripper()
	if got := string(s.Process(in)); got != "ab" {
		t.Errorf("got %q", got)
	}
	if !bytes.Equal(in, cp) {
		t.Errorf("input chunk was mutated: %q -> %q", cp, in)
	}
}
