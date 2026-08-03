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
// Terminal marker and injected EOM are format-aware (see utils.eomTail) so
// Anthropic clients get `message_stop` and OpenAI clients get `data: [DONE]`.

import { eomTail } from "../utils.mjs";

const KEEPALIVE_INTERVAL = 10000;

export function pipeSse({
  upstreamRes, upstreamReq, res, isSse, streamFormat = "anthropic",
  chunkTimeoutMs, idleTimeoutMs, slowResponseMs, keepaliveIntervalMs = KEEPALIVE_INTERVAL,
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

  function endWithEom() {
    if (isSse && !sawMessageStop) safeWrite(eomTail(streamFormat));
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
    for (const line of block.split("\n")) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      if (trimmed.startsWith(":")) continue;
      hasField = true;
      const lower = trimmed.toLowerCase();
      if (lower.startsWith("data:")) {
        if (trimmed.slice(5).trim() === "[DONE]") {
          sawMessageStop = true;
          onMessageStop();
        }
      } else if (lower.startsWith("event:")) {
        if (trimmed.slice(6).trim() === "message_stop") {
          sawMessageStop = true;
          onMessageStop();
        }
      }
    }
    if (hasField) sawDataEvent = true;
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
        endWithEom();
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
      endWithEom();
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
    analyze(chunk.toString("utf8"));
    logDebug(`CHUNK #${chunkCount} ${chunk.length}b, elapsed ${Date.now() - reqStart}ms`);
    resetChunkTimer();
    resetIdleTimer();
    const canContinue = safeWrite(chunk);
    if (canContinue === false && !res.writableEnded) {
      if (res.socket?.destroyed || res.destroyed) {
        endWithEom();
        finishStream();
      } else {
        upstreamRes.pause();
        res.once("drain", () => { if (!streamFinished) upstreamRes.resume(); });
      }
    }
  });

  upstreamRes.on("end", () => {
    if (streamFinished) return;
    flushCarry();
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
    const causeCode = e.cause?.code ? ` (cause: ${e.cause.code})` : "";
    log(`UPSTREAM STREAM ERROR: ${e.message}${causeCode}`);
    onResult({ statusCode: 502, durationMs: Date.now() - reqStart, chunks: chunkCount, error: e.message });
    endWithEom();
    finishStream();
  });

  upstreamRes.on("close", () => {
    if (streamFinished) return;
    flushCarry();
    if (!res.writableEnded) {
      log(`UPSTREAM CLOSED (connection terminated prematurely, ${Date.now() - reqStart}ms, ${chunkCount} chunks)`);
      onResult({ statusCode: 502, durationMs: Date.now() - reqStart, chunks: chunkCount, error: "upstream_closed" });
      endWithEom();
    }
    finishStream();
  });
}
