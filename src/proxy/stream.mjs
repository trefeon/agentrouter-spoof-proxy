// SSE streaming pump: wires the upstream response into the client response with
// keepalive pings, idle/stall guards, backpressure, and a synthesized terminal
// SSE event on abnormal end. Modeled on 9router's open-sse streamHandler
// (createStreamController + pipeWithDisconnect), adapted to node:http streams.
//
// Invariant flags:
//   streamFinished  — terminal; guards all timers/handlers against double-fire
//   sawDataEvent    — at least one real SSE event block arrived. Comment-only
//                     keepalive lines (`: ...`) are NOT data. Detected from a
//                     carry-over frame parser, never from chunk size, so short
//                     events and markers split across TCP chunks still count.
//   sawMessageStop  — terminal marker seen (Anthropic `event: message_stop` or
//                     OpenAI `data: [DONE]`); skips synthetic EOM injection
//
// Terminal marker and injected EOM are format-aware (see utils.eomTail /
// abnormalFinish) so Anthropic clients get `message_stop` and OpenAI clients
// get `data: [DONE]`; abnormal ends additionally carry a protocol error frame.

import { sseErrorFrame, abnormalFinish } from "../utils.mjs";

const KEEPALIVE_INTERVAL = 10000;

export function pipeSse({
  upstreamRes, upstreamReq, res, isSse, streamFormat = "anthropic",
  chunkTimeoutMs, idleTimeoutMs, slowResponseMs, keepaliveIntervalMs = KEEPALIVE_INTERVAL,
  stripThinkingTags = false,
  log = () => {}, logDebug = () => {},
  onResult = () => {}, onDegrade = () => {}, onFinish = () => {}, onMessageStop = () => {},
} = {}) {
  const reqStart = Date.now();
  let streamFinished = false;
  let chunkCount = 0;
  let sawDataEvent = false;
  let sawMessageStop = false;
  let stallLogged = false;
  let idleTimer = null;
  let chunkTimer = null;
  let keepaliveTimer = null;
  let carry = "";
  let insideThinkTag = false;
  // Byte-level residue for transforms that must not corrupt multi-byte UTF-8:
  //   thinkBuf   — bytes of an unresolved `<think>` span awaiting `</think>`
  //   openaiPending — incomplete SSE frame (no trailing `\n\n` yet) held back
  //                   so `data: null` keepalives split across TCP chunks are
  //                   still filtered at the frame level
  let thinkBuf = Buffer.alloc(0);
  let openaiPending = Buffer.alloc(0);
  // Trailing bytes of a chunk that could be the prefix of a `<think>` tag
  // split across TCP chunks (at most 6 bytes: "<think>"). Held back until
  // the next chunk arrives so the tag is detected and stripped, not leaked.
  let tagPrefixPending = Buffer.alloc(0);

  function safeWrite(chunk) {
    if (res.writableEnded) return false;
    try { return res.write(chunk); } catch { return false; }
  }

  function safeEnd(data) {
    if (res.writableEnded) return;
    try { res.end(data); } catch {}
  }

  function clearTimers() {
    for (const t of [idleTimer, chunkTimer]) if (t) clearTimeout(t);
    idleTimer = chunkTimer = null;
  }

  function finishStream() {
    if (streamFinished) return;
    streamFinished = true;
    clearTimers();
    if (keepaliveTimer) clearInterval(keepaliveTimer);
    keepaliveTimer = null;
    onFinish();
  }

  // Abnormal end: emit a protocol error frame first so strict SDK clients
  // surface the failure instead of treating a truncated stream as a clean
  // completion, then a synthetic finisher + terminal marker so every client
  // class (opencode finalizes on finish_reason/message_delta, Anthropic SDK
  // needs message_stop, lax parsers need [DONE]) terminates cleanly.
  function endWithEom(reason) {
    if (isSse && !sawMessageStop) {
      if (reason) safeWrite(sseErrorFrame(streamFormat, reason));
      safeWrite(abnormalFinish(streamFormat));
    }
    safeEnd();
  }

  // "Still streaming" = both ends of the pipe are open. A slow-but-live
  // upstream (long thinking pause, tool-call gap, agentic turn) must never be
  // cut by a stall watchdog; only a genuinely dead connection is terminated.
  function isStreamAlive() {
    if (res.writableEnded || res.destroyed) return false;
    if (res.socket && res.socket.destroyed) return false;
    if (upstreamRes.destroyed || upstreamRes.readableEnded) return false;
    return true;
  }

  // ── SSE frame classification (carry-over parser) ──
  //
  // SSE blocks are blank-line separated. Each block is classified as:
  //   - terminal: contains `event: message_stop` or `data: [DONE]`
  //   - data: contains at least one non-comment field line
  //   - noise: comment lines only (`: ...`)
  // Raw chunks are forwarded verbatim regardless of this analysis.

  function classifyBlock(block) {
    if (!block.trim()) return;
    let hasField = false;
    let isPing = false;
    for (const line of block.split("\n")) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      if (trimmed.startsWith(":")) continue;
      hasField = true;
      if (trimmed.toLowerCase().startsWith("event:") && trimmed.slice(6).trim() === "ping") {
        // Liveness frames (`event: ping`) are noise, not model data: they keep
        // the connection alive but must not skew empty-stream detection or the
        // stall-watchdog reason.
        isPing = true;
      }
      const lower = trimmed.toLowerCase();
      if (lower.startsWith("data:")) {
        const dataStr = trimmed.slice(5).trim();
        if (dataStr === "[DONE]") {
          sawMessageStop = true;
          onMessageStop();
        } else if (dataStr) {
          try {
            const json = JSON.parse(dataStr);
            const inputTokens = json.message?.usage?.input_tokens ?? json.usage?.prompt_tokens ?? json.usage?.input_tokens;
            const outputTokens = json.message?.usage?.output_tokens ?? json.usage?.completion_tokens ?? json.usage?.output_tokens;
            if (inputTokens !== undefined || outputTokens !== undefined) {
              logDebug(`TOKEN USAGE: input_tokens=${inputTokens ?? "N/A"}, output_tokens=${outputTokens ?? "N/A"}`);
            }
          } catch {}
        }
      } else if (lower.startsWith("event:")) {
        if (trimmed.slice(6).trim() === "message_stop") {
          sawMessageStop = true;
          onMessageStop();
        }
      }
    }
    if (hasField && !isPing) sawDataEvent = true;
  }

  function flushCarry() {
    if (!carry) return;
    const pending = carry;
    carry = "";
    classifyBlock(pending + "\n\n");
  }

  function analyze(text) {
    carry += text.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
    let idx;
    while ((idx = carry.indexOf("\n\n")) !== -1) {
      const block = carry.slice(0, idx);
      carry = carry.slice(idx + 2);
      classifyBlock(block);
    }
  }

  // ── Timers ──

  function resetIdleTimer() {
    if (idleTimer) { clearTimeout(idleTimer); idleTimer = null; }
    if (isSse && !streamFinished) {
      idleTimer = setTimeout(() => {
        if (streamFinished) return;
        log(`SSE IDLE TIMEOUT (${idleTimeoutMs / 1000}s no events)`);
        onResult({ statusCode: 504, durationMs: Date.now() - reqStart, chunks: chunkCount, error: "sse_idle_timeout" });
        onDegrade("sse_idle_timeout");
        endWithEom("sse_idle_timeout");
        if (!upstreamReq.destroyed) upstreamReq.destroy();
        finishStream();
      }, idleTimeoutMs);
      idleTimer.unref();
    }
  }

  function resetChunkTimer() {
    if (!isSse || streamFinished) return;
    if (chunkTimer) { clearTimeout(chunkTimer); chunkTimer = null; }
    chunkTimer = setTimeout(() => {
      if (streamFinished) return;
      if (isStreamAlive()) {
        // Slow but alive: keep the connection and re-arm the stall watchdog
        // instead of cutting. The idle timer is the ultimate bound for a
        // truly silent (half-open) connection.
        if (!stallLogged) {
          stallLogged = true;
          log(`SLOW STREAM (${sawDataEvent ? "data gap" : "no data yet"} > ${chunkTimeoutMs / 1000}s) — keeping stream alive`);
        }
        resetChunkTimer();
        return;
      }
      const reason = sawDataEvent ? "sse_chunk_timeout" : "keepalive_only";
      log(`${reason.toUpperCase()} (${chunkCount} chunks)`);
      onResult({ statusCode: 504, durationMs: Date.now() - reqStart, chunks: chunkCount, error: reason });
      onDegrade(reason);
      endWithEom(reason);
      if (!upstreamReq.destroyed) upstreamReq.destroy();
      finishStream();
    }, chunkTimeoutMs);
    chunkTimer.unref();
  }

  function startKeepalive() {
    if (!isSse) return;
    keepaliveTimer = setInterval(() => {
      if (streamFinished || res.writableEnded || res.destroyed) {
        clearInterval(keepaliveTimer);
        keepaliveTimer = null;
        return;
      }
      const canContinue = res.write(":\n\n");
      if (canContinue === false && logDebug) logDebug("keepalive backpressure, skipping tick");
    }, keepaliveIntervalMs);
    keepaliveTimer.unref();
  }

  resetIdleTimer();
  resetChunkTimer();
  startKeepalive();

  upstreamRes.on("data", (chunk) => {
    if (streamFinished) return;
    chunkCount++;
    // Analyze BEFORE re-arming timers so the stall watchdog uses the fresh
    // framing state (previously the timer was armed with the pre-chunk
    // sawDataEvent, killing streams the moment their first real chunk and a
    // thinking pause overlapped).
    let chunkText = chunk.toString("utf8");
    analyze(chunkText);
    logDebug(`CHUNK #${chunkCount} ${chunk.length}b, elapsed ${Date.now() - reqStart}ms`);
    resetChunkTimer();
    resetIdleTimer();

    // Byte-level `<think>` span stripping. OpenAI-format streams only: OpenAI
    // clients cannot render thinking blocks and would show raw tags, while
    // Anthropic-protocol harness clients (opencode, OpenClaw, claude-code)
    // support thinking natively — stripping it there would remove reasoning AND
    // create silent gaps that trigger client-side idle watchdogs. Unlike the
    // old string-based stripper, raw bytes are never decoded and re-encoded:
    // chunks without a tag boundary pass through untouched, and spans split
    // across TCP chunks (or adjacent to multi-byte UTF-8) are resolved
    // byte-faithfully, so CJK/emoji content is never corrupted.
    if (stripThinkingTags && isSse && streamFormat === "openai") {
      let buf = chunk;
      if (tagPrefixPending.length) { buf = Buffer.concat([tagPrefixPending, chunk]); tagPrefixPending = Buffer.alloc(0); }
      if (insideThinkTag || buf.includes(Buffer.from("<think>"))) {
        thinkBuf = Buffer.concat([thinkBuf, buf]);
        const out = [];
        let pos = 0;
        while (true) {
          if (insideThinkTag) {
            const endIdx = thinkBuf.indexOf(Buffer.from("</think>"), pos);
            if (endIdx === -1) break;
            insideThinkTag = false;
            pos = endIdx + 8;
          } else {
            const startIdx = thinkBuf.indexOf(Buffer.from("<think>"), pos);
            if (startIdx === -1) {
              out.push(thinkBuf.subarray(pos));
              pos = thinkBuf.length;
              break;
            }
            out.push(thinkBuf.subarray(pos, startIdx));
            insideThinkTag = true;
            pos = startIdx + 7;
          }
        }
        if (insideThinkTag) {
          // Unresolved span: retain only the unclosed `<think>` bytes; forward
          // the clean prefix (text before the span).
          const spanStart = thinkBuf.indexOf(Buffer.from("<think>"));
          thinkBuf = thinkBuf.subarray(spanStart === -1 ? 0 : spanStart);
          if (out.length) chunk = Buffer.concat(out);
          else chunk = Buffer.alloc(0);
        } else {
          chunk = Buffer.concat(out);
          thinkBuf = Buffer.alloc(0);
        }
      } else {
        // No tag boundary in this chunk: forward it, but hold back up to 6
        // trailing bytes that could be the prefix of a `<think>` tag split
        // across TCP chunks (e.g. `...<thi` | `nk>SECRET response...`) so the
        // tag is still detected and stripped on the next chunk. A held prefix
        // that never completes materializes on the following chunk unchanged.
        let hold = 0;
        for (let len = 6; len >= 1; len--) {
          if (buf.subarray(buf.length - len).equals(Buffer.from("<think>".slice(0, len), "latin1"))) { hold = len; break; }
        }
        if (hold > 0) { tagPrefixPending = buf.subarray(buf.length - hold); chunk = buf.subarray(0, buf.length - hold); }
        else { chunk = buf; }
      }
    }

    // Frame-level `data: null` / bare `data:` keepalive filtering (OpenAI
    // format). AgentRouter (new-api) emits bare `data: null` frames as
    // keepalives; schema-validating clients (AI SDK, opencode llm layer) fail
    // on `null` and empty payloads. Hold the incomplete frame tail so frames
    // split across TCP chunks are still filtered; untouched frames are
    // forwarded as raw bytes (no decode/encode, no corruption).
    if (isSse && streamFormat === "openai") {
      const combined = openaiPending.length ? Buffer.concat([openaiPending, chunk]) : chunk;
      const lastIdx = combined.lastIndexOf("\n\n");
      if (lastIdx === -1) {
        openaiPending = combined;
        chunk = Buffer.alloc(0);
      } else {
        const readyEnd = lastIdx + 2;
        const ready = combined.subarray(0, readyEnd);
        openaiPending = combined.subarray(readyEnd);
        const hasBadFrame =
          ready.includes(Buffer.from("data: null")) ||
          ready.includes(Buffer.from("data:null")) ||
          ready.includes(Buffer.from("data:\n"));
        if (hasBadFrame) {
          const readyText = ready.toString("utf8");
          const cleaned = readyText
            .split("\n")
            .filter((line) => {
              const t = line.trim();
              return t !== "data: null" && t !== "data:null" && t !== "data:";
            })
            .join("\n");
          if (cleaned !== readyText) {
            logDebug(`dropped invalid data: keepalive frame(s), ${ready.length} -> ${cleaned.length} bytes`);
            chunk = Buffer.from(cleaned, "utf8");
          } else {
            chunk = ready;
          }
        } else {
          chunk = ready;
        }
      }
    }

    const canContinue = safeWrite(chunk);
    if (canContinue === false && !res.writableEnded) {
      if (res.socket?.destroyed || res.destroyed) {
        endWithEom("client_disconnected");
        if (!upstreamReq.destroyed) upstreamReq.destroy();
        finishStream();
      } else {
        upstreamRes.pause();
        res.once("drain", () => { if (!streamFinished) upstreamRes.resume(); });
      }
    }
  });

  // Client aborted: tear down the upstream immediately instead of waiting for
  // the next upstream chunk or the stall watchdog.
  res.on("close", () => {
    if (streamFinished) return;
    if (!upstreamReq.destroyed) upstreamReq.destroy();
  });

  upstreamRes.on("end", () => {
    if (streamFinished) return;
    // Upstream ended while a `<think>` span was still open: those bytes were
    // withheld from the client (never forwarded), so silently reporting 200
    // would be silent truncation. Surface it as a 502 with an error frame.
    if (thinkBuf.length) {
      thinkBuf = Buffer.alloc(0);
      log(`UPSTREAM ENDED MID-SPAN (unterminated thinking tag, ${chunkCount} chunks)`);
      onResult({ statusCode: 502, durationMs: Date.now() - reqStart, chunks: chunkCount, error: "upstream_ended_mid_frame" });
      endWithEom("upstream_ended_mid_frame");
      finishStream();
      return;
    }
    flushCarry();
    // Upstream ended cleanly with a trailing `<think>` tag prefix still held
    // back: forward it so no bytes are lost (verbatim-streaming contract).
    // It never completed into a real tag, so it is legitimate content.
    if (tagPrefixPending.length) {
      safeWrite(tagPrefixPending);
      tagPrefixPending = Buffer.alloc(0);
    }
    // Upstream ended cleanly while an OpenAI tail frame was pending: forward it
    // so no bytes are lost (verbatim-streaming contract). Abnormal paths below
    // drop the partial frame instead — a half frame before a synthetic
    // finisher would corrupt the client's parser.
    if (openaiPending.length) {
      safeWrite(openaiPending);
      openaiPending = Buffer.alloc(0);
    }
    if (isSse && !sawDataEvent) {
      log(`EMPTY SSE STREAM (${chunkCount} chunks)`);
      onDegrade("empty_sse");
    }
    safeEnd();
    const durationMs = Date.now() - reqStart;
    onResult({ statusCode: 200, durationMs, chunks: chunkCount, emptyOutput: isSse && !sawDataEvent });
    if (durationMs >= slowResponseMs) onDegrade(`slow_${durationMs}ms`);
    log(`200 (stream complete, ${durationMs}ms, ${chunkCount} chunks)`);
    finishStream();
  });

  upstreamRes.on("error", (e) => {
    if (streamFinished) return;
    flushCarry();
    openaiPending = Buffer.alloc(0);
    const causeCode = e.cause?.code ? ` (cause: ${e.cause.code})` : "";
    log(`UPSTREAM STREAM ERROR: ${e.message}${causeCode}`);
    onResult({ statusCode: 502, durationMs: Date.now() - reqStart, chunks: chunkCount, error: e.message });
    endWithEom("upstream_stream_error");
    finishStream();
  });

  upstreamRes.on("close", () => {
    if (streamFinished) return;
    flushCarry();
    openaiPending = Buffer.alloc(0);
    if (!res.writableEnded) {
      log(`UPSTREAM CLOSED (connection terminated prematurely, ${Date.now() - reqStart}ms, ${chunkCount} chunks)`);
      onResult({ statusCode: 502, durationMs: Date.now() - reqStart, chunks: chunkCount, error: "upstream_closed" });
      endWithEom("upstream_closed");
    }
    finishStream();
  });
}
