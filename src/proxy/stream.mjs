// SSE streaming pump: wires the upstream response into the client response with
// keepalive pings, idle/chunk timeout guards, backpressure, and a synthesized
// terminal SSE event on abnormal end. Modeled on 9router's open-sse
// streamHandler (createStreamController + pipeWithDisconnect), adapted to
// node:http streams.
//
// Invariant flags:
//   streamFinished  — terminal; guards all timers/handlers against double-fire
//   sawDataEvent    — a real data chunk (> KEEPALIVE_THRESHOLD bytes) arrived;
//                     distinguishes live streams from keepalive-only noise
//   sawMessageStop  — upstream emitted event: message_stop; skip EOM injection

import { SSE_EOM, KEEPALIVE_THRESHOLD } from "../utils.mjs";

const KEEPALIVE_INTERVAL = 10000;

export function pipeSse({
  upstreamRes, upstreamReq, res, isSse,
  chunkTimeoutMs, idleTimeoutMs, slowResponseMs,
  log = () => {}, logDebug = () => {},
  onResult = () => {}, onDegrade = () => {}, onFinish = () => {}, onMessageStop = () => {},
} = {}) {
  const reqStart = Date.now();
  let streamFinished = false;
  let chunkCount = 0;
  let sawDataEvent = false;
  let sawMessageStop = false;
  let maxChunkSize = 0;
  let idleTimer = null;
  let chunkTimer = null;
  let keepaliveTimer = null;

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
    if (keepaliveTimer) clearInterval(keepaliveTimer);
    idleTimer = chunkTimer = keepaliveTimer = null;
  }

  function finishStream() {
    if (streamFinished) return;
    streamFinished = true;
    clearTimers();
    onFinish();
  }

  function endWithEom() {
    if (isSse && !sawMessageStop) safeWrite(`\n${SSE_EOM}\ndata: {}\n\n`);
    safeEnd();
  }

  function resetChunkTimer() {
    if (!isSse || streamFinished) return;
    if (sawDataEvent && chunkTimer) {
      clearTimeout(chunkTimer);
      chunkTimer = null;
    }
    if (!sawDataEvent && chunkTimer) return;
    const timeout = sawDataEvent ? chunkTimeoutMs : Math.min(60000, chunkTimeoutMs);
    chunkTimer = setTimeout(() => {
      if (streamFinished) return;
      const reason = sawDataEvent ? "sse_chunk_timeout" : "keepalive_only";
      if (sawDataEvent) {
        log(`CHUNK TIMEOUT (${timeout / 1000}s no data)`);
      } else {
        log(`KEEPALIVE-ONLY STREAM (${chunkCount} pings, no real data in ${timeout / 1000}s)`);
      }
      onResult({ statusCode: 504, durationMs: Date.now() - reqStart, chunks: chunkCount, error: reason });
      onDegrade(reason);
      endWithEom();
      if (!upstreamReq.destroyed) upstreamReq.destroy();
      finishStream();
    }, timeout);
    chunkTimer.unref();
  }

  function resetIdleTimer() {
    clearTimers();
    if (isSse && !streamFinished) {
      idleTimer = setTimeout(() => {
        if (streamFinished) return;
        log(`SSE IDLE TIMEOUT (${idleTimeoutMs / 1000}s no data)`);
        onResult({ statusCode: 504, durationMs: Date.now() - reqStart, chunks: chunkCount, error: "sse_idle_timeout" });
        onDegrade("sse_idle_timeout");
        endWithEom();
        if (!upstreamReq.destroyed) upstreamReq.destroy();
        finishStream();
      }, idleTimeoutMs);
      idleTimer.unref();
    }
  }

  function startKeepalive() {
    if (!isSse) return;
    keepaliveTimer = setInterval(() => {
      if (streamFinished || res.writableEnded) {
        clearInterval(keepaliveTimer);
        keepaliveTimer = null;
        return;
      }
      const canContinue = res.write(":\n\n");
      if (canContinue === false && logDebug) logDebug("keepalive backpressure, skipping tick");
    }, KEEPALIVE_INTERVAL);
    keepaliveTimer.unref();
  }

  resetIdleTimer();
  resetChunkTimer();
  startKeepalive();

  upstreamRes.on("data", (chunk) => {
    if (streamFinished) return;
    resetIdleTimer();
    resetChunkTimer();
    chunkCount++;
    maxChunkSize = Math.max(maxChunkSize, chunk.length);
    logDebug(`CHUNK #${chunkCount} ${chunk.length}b, elapsed ${Date.now() - reqStart}ms`);
    if (chunk.length > KEEPALIVE_THRESHOLD) sawDataEvent = true;
    if (chunk.includes(SSE_EOM)) {
      sawMessageStop = true;
      onMessageStop();
    }
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
    if (isSse && !sawDataEvent) {
      log(`EMPTY SSE STREAM (${chunkCount} chunks, max ${maxChunkSize}b)`);
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
    const causeCode = e.cause?.code ? ` (cause: ${e.cause.code})` : "";
    log(`UPSTREAM STREAM ERROR: ${e.message}${causeCode}`);
    onResult({ statusCode: 502, durationMs: Date.now() - reqStart, chunks: chunkCount, error: e.message });
    endWithEom();
    finishStream();
  });

  upstreamRes.on("close", () => {
    if (streamFinished) return;
    if (!res.writableEnded) {
      log(`UPSTREAM CLOSED (connection terminated prematurely, ${Date.now() - reqStart}ms, ${chunkCount} chunks)`);
      onResult({ statusCode: 502, durationMs: Date.now() - reqStart, chunks: chunkCount, error: "upstream_closed" });
      endWithEom();
    }
    finishStream();
  });
}