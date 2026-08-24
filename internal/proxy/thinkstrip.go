package proxy

import "bytes"

var (
	thinkOpenTag  = []byte("<think>")
	thinkCloseTag = []byte("</think>")
)

// ThinkStripper removes `<think>…</think>` spans at the byte level, ported
// verbatim from the stream.mjs state machine (l.251-300). It is only used
// when stripThinkingTags && streamFormat == openai (the caller decides):
// OpenAI clients cannot render thinking blocks, while Anthropic-protocol
// harness clients support thinking natively.
//
// Raw bytes are never decoded and re-encoded, so multi-byte UTF-8 is never
// corrupted. Spans split across TCP chunks are detected via an up-to-6-byte
// trailing holdback (tagPrefixPending); a span still open at stream end is
// unterminated and the caller must fail with 502 without forwarding the
// withheld bytes. Resolved spans may be split across multiple Process calls.
type ThinkStripper struct {
	insideThinkTag   bool
	thinkBuf         []byte
	tagPrefixPending []byte
}

// NewThinkStripper returns a stripper with no pending state.
func NewThinkStripper() *ThinkStripper {
	return &ThinkStripper{}
}

// Process feeds one upstream chunk through the state machine and returns the
// bytes that may be forwarded to the client. Chunks without a tag boundary
// pass through untouched (possibly minus a held-back tag prefix); the returned
// slice must not be retained past the next call.
func (t *ThinkStripper) Process(chunk []byte) []byte {
	buf := chunk
	if len(t.tagPrefixPending) > 0 {
		buf = make([]byte, 0, len(t.tagPrefixPending)+len(chunk))
		buf = append(buf, t.tagPrefixPending...)
		buf = append(buf, chunk...)
		t.tagPrefixPending = nil
	}
	if t.insideThinkTag || bytes.Contains(buf, thinkOpenTag) {
		t.thinkBuf = append(t.thinkBuf, buf...)
		out := make([][]byte, 0, 2)
		pos := 0
		for {
			if t.insideThinkTag {
				endIdx := bytes.Index(t.thinkBuf[pos:], thinkCloseTag)
				if endIdx == -1 {
					break
				}
				endIdx += pos
				t.insideThinkTag = false
				pos = endIdx + len(thinkCloseTag)
			} else {
				startIdx := bytes.Index(t.thinkBuf[pos:], thinkOpenTag)
				if startIdx == -1 {
					out = append(out, t.thinkBuf[pos:])
					pos = len(t.thinkBuf)
					break
				}
				startIdx += pos
				out = append(out, t.thinkBuf[pos:startIdx])
				t.insideThinkTag = true
				pos = startIdx + len(thinkOpenTag)
			}
		}
		if t.insideThinkTag {
			// Unresolved span: retain only the unclosed `<think>` bytes;
			// forward the clean prefix (text before the span).
			spanStart := bytes.Index(t.thinkBuf, thinkOpenTag)
			if spanStart == -1 {
				spanStart = 0
			}
			t.thinkBuf = append([]byte(nil), t.thinkBuf[spanStart:]...)
			if len(out) > 0 {
				return joinBytes(out)
			}
			return nil
		}
		t.thinkBuf = nil
		return joinBytes(out)
	}
	// No tag boundary in this chunk: forward it, but hold back up to 6
	// trailing bytes that could be the prefix of a `<think>` tag split across
	// TCP chunks (e.g. `...<thi` | `nk>SECRET response...`) so the tag is
	// still detected and stripped on the next chunk. A held prefix that never
	// completes materializes on the following chunk unchanged.
	hold := 0
	for l := 6; l >= 1; l-- {
		if len(buf) >= l && bytes.Equal(buf[len(buf)-l:], thinkOpenTag[:l]) {
			hold = l
			break
		}
	}
	if hold > 0 {
		t.tagPrefixPending = append([]byte(nil), buf[len(buf)-hold:]...)
		return buf[:len(buf)-hold]
	}
	return buf
}

// Unterminated reports whether an unclosed `<think>` span is still buffered
// (thinkBuf non-empty): the stream must not be reported as a clean 200 while
// withheld bytes were never forwarded.
func (t *ThinkStripper) Unterminated() bool {
	return len(t.thinkBuf) > 0
}

// Flush handles a clean stream end. If a `<think>` span is still open it
// returns (nil, true), the caller must 502 and never leak the withheld
// bytes. Otherwise it returns any held-back tag prefix (which never completed
// into a real tag, so it is legitimate content) and false.
func (t *ThinkStripper) Flush() (forward []byte, unterminated bool) {
	if len(t.thinkBuf) > 0 {
		return nil, true
	}
	if len(t.tagPrefixPending) > 0 {
		f := t.tagPrefixPending
		t.tagPrefixPending = nil
		return f, false
	}
	return nil, false
}

// joinBytes concatenates the forwarded parts into one fresh slice.
func joinBytes(parts [][]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
