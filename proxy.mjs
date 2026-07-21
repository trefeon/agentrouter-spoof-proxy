import http from "node:http";
import { resolve4 } from "node:dns/promises";
import { setTimeout as sleep } from "node:timers/promises";
import * as cfg from "./src/config.mjs";
import { log, logDebug } from "./src/logger.mjs";
import { isCircuitOpen, recordSuccess, recordFailure, getConsecutiveFails } from "./src/resilience/circuit-breaker.mjs";
import { getWafCookie, warmup } from "./src/auth/waf.mjs";
import { SPOOF_HEADERS } from "./src/auth/spoof.mjs";
import { getModelsList, getModelSource, fetchModels } from "./src/models/discovery.mjs";
import { getHealthyModels, startProbeLoop, stopProbeLoop, markModelFailed, markModelExhausted, markModelDegraded } from "./src/models/health.mjs";
import { recordModelStart, recordModelResult, getModelStats } from "./src/models/stats.mjs";
import {
  SSE_EOM, KEEPALIVE_THRESHOLD, MAX_BODY_SIZE,
  truncate, filterHeaders, rewritePath, respondJson,
  isWafBlock, isRetryable, injectPrompt, summarizeRequest, responseHasEmptyOutput, redactSensitive,
} from "./src/utils.mjs";

// ── Shared state ──

let activeStreams = 0;

// ── DNS ──

async function resolveDns() {
  const ts = new Date().toISOString();
  try {
    const addresses = await resolve4(cfg.TARGET_HOST_VAL);
    log(ts, `DNS resolved ${cfg.TARGET_HOST_VAL} → ${addresses.join(", ")}`);
  } catch {
    log(ts, `DNS resolution failed for ${cfg.TARGET_HOST_VAL}`);
  }
}

// ── Warmup scheduler ──

function scheduleWarmup() {
  warmup();
  setInterval(warmup, cfg.WARMUP_INTERVAL);
}

// ── Model discovery scheduler ──

function scheduleDiscovery() {
  if (!cfg.AR_API_KEY_VAL) {
    console.log(`Model discovery disabled (no AR_API_KEY set), using static list`);
    return;
  }
  fetchModels();
  setInterval(fetchModels, cfg.DISCOVERY_INTERVAL);
}

// ── Server ──

const server = http.createServer((req, res) => {
  const ts = new Date().toISOString();
  const rawPath = req.url;
  const method = req.method;

  // Health check
  if (method === "GET" && (rawPath === "/health" || rawPath === "/api/health")) {
    respondJson(res, 200, {
      ok: true,
      upstream: `${cfg.TARGET_HOST_VAL}:${cfg.TARGET_PORT_INT}`,
      modelSource: getModelSource(),
      staticModels: cfg.MODELS_CSV_VAL.split(",").length,
      availableModels: getModelsList().length,
      activeStreams,
      wafCookie: !!getWafCookie(),
      circuitOpen: isCircuitOpen(),
      consecutiveFails: getConsecutiveFails(),
      modelHealth: getModelStats(),
    });
    return;
  }

  // Model list — filter unhealthy models so 9Router falls back instantly
  if (method === "GET" && (rawPath === "/v1/models" || rawPath === "/models")) {
    respondJson(res, 200, { data: getHealthyModels(getModelsList()), object: "list" });
    return;
  }

  // ── Proxy request ──

  const body = [];
  let bodySize = 0;
  let bodyRejected = false;
  let currentUpstreamReq = null;
  let proxyDone = false;
  let hasEnded = false;

  function finishProxy() {
    if (proxyDone) return;
    proxyDone = true;
    activeStreams--;
  }

  function safeWriteHead(statusCode, headers) {
    if (res.headersSent) return false;
    try { res.writeHead(statusCode, headers); return true; }
    catch (e) { log(ts, `safeWriteHead error: ${e.message}`); return false; }
  }

  function safeEnd(data) {
    if (res.writableEnded) return;
    try { res.end(data); } catch {}
  }

  function safeWrite(data) {
    if (res.writableEnded) return false;
    try { return res.write(data); } catch { return false; }
  }

  function safeRespondJson(status, data) {
    if (res.headersSent || res.writableEnded) return;
    try { respondJson(res, status, data); } catch (e) { log(ts, `safeRespondJson error: ${e.message}`); }
  }

  req.on("data", (c) => {
    bodySize += c.length;
    if (bodySize > MAX_BODY_SIZE && !bodyRejected) {
      bodyRejected = true;
      req.destroy();
      respondJson(res, 413, { error: { code: "payload_too_large", message: "Request body exceeds 20MB limit", type: "proxy_error" } });
      return;
    }
    if (!bodyRejected) body.push(c);
  });

  req.on("end", () => {
    hasEnded = true;
    const path = rewritePath(rawPath);
    const fullBody = Buffer.concat(body);
    const requestSummary = summarizeRequest(fullBody, path, method);
    logDebug(ts, `${method} ${rawPath} -> REQUEST ${JSON.stringify(requestSummary)}`);

    // Adaptive response timeout — larger bodies need more upstream processing time
    const adaptiveResponseTimeout = (() => {
      const mb = fullBody.length / (1024 * 1024);
      if (mb > 5) return 300000;     // 5min for >5MB
      if (mb > 2) return 180000;     // 3min for 2-5MB
      if (mb > 1) return 120000;     // 2min for 1-2MB
      if (mb > 0.5) return 90000;    // 90s for 500KB-1MB
      return cfg.RESPONSE_TIMEOUT;   // default 30s for small payloads
    })();

    // Extract model from body for reactive health marking
    let requestModel = null;
    try { requestModel = JSON.parse(fullBody.toString("utf8"))?.model || null; } catch {}
    if (requestModel) recordModelStart(requestModel);

    const upstreamHeaders = {
      ...SPOOF_HEADERS,
      "Content-Type": "application/json",
      ...(req.headers["authorization"] ? { Authorization: req.headers["authorization"] } : {}),
      ...(req.headers["x-api-key"] ? { "x-api-key": req.headers["x-api-key"] } : {}),
      ...(req.headers["anthropic-version"] ? { "anthropic-version": req.headers["anthropic-version"] } : {}),
    };

    const wafCookie = getWafCookie();
    if (wafCookie) upstreamHeaders["Cookie"] = wafCookie;

    if (isCircuitOpen()) {
      log(ts, `${method} ${rawPath} -> REJECTED (circuit open)`);
      safeRespondJson(503, { error: { code: "circuit_open", message: "Upstream circuit breaker open, retry later", type: "proxy_error" } });
      return;
    }

    activeStreams++;

    async function doRequest(attempt) {
      log(ts, `${method} ${rawPath} -> ${path} (attempt ${attempt + 1})`);

      return new Promise((resolveProxy) => {
        if (proxyDone) { resolveProxy(); return; }

        const opts = {
          hostname: cfg.TARGET_HOST_VAL,
          port: cfg.TARGET_PORT_INT,
          path,
          method,
          headers: upstreamHeaders,
          agent: cfg.AGENT,
          rejectUnauthorized: true,
          timeout: cfg.TIMEOUT,
        };

        let errorHandled = false;
        let idleTimer = null;
        let reqTimer = null;
        let responseTimer = null;
        let chunkTimer = null;
        let keepaliveTimer = null;
        let isSse = false;
        let sawMessageStop = false;
        let upstreamResponded = false;

        function clearIdleTimer() {
          if (idleTimer) { clearTimeout(idleTimer); idleTimer = null; }
          if (reqTimer) { clearTimeout(reqTimer); reqTimer = null; }
          if (responseTimer) { clearTimeout(responseTimer); responseTimer = null; }
          if (chunkTimer) { clearTimeout(chunkTimer); chunkTimer = null; }
          if (keepaliveTimer) { clearInterval(keepaliveTimer); keepaliveTimer = null; }
        }

        const upstreamReq = cfg.UPSTREAM_MODULE.request(opts, (upstreamRes) => {
          upstreamResponded = true;
          clearIdleTimer();
          const statusCode = upstreamRes.statusCode;
          const upstreamStart = Date.now();

          // WAF block → re-warmup & retry once
          if ((statusCode === 405 || statusCode === 403) && attempt === 0) {
            let chunks = [];
            upstreamRes.on("data", (c) => chunks.push(c));
            upstreamRes.on("end", async () => {
              const raw = Buffer.concat(chunks);
              const emptyOutput = responseHasEmptyOutput(statusCode, raw);
              if (emptyOutput) markModelDegraded(requestModel, "empty_output");
              if (isWafBlock(statusCode, raw)) {
                log(ts, `WAF ${statusCode} detected, refreshing cookie and retrying...`);
                await warmup();
                recordModelResult(requestModel, { statusCode, durationMs: Date.now() - upstreamStart, wafBlock: true, error: "waf_block", emptyOutput });
                const newCookie = getWafCookie();
                if (newCookie) upstreamHeaders["Cookie"] = newCookie;
                const result = await doRequest(attempt + 1);
                resolveProxy(result);
                return;
              }
              log(ts, `${method} ${rawPath} <- ${statusCode} (${raw.length}b)`);
              log(ts, `RESPONSE BODY: ${redactSensitive(truncate(raw.toString("utf8"), 1000))}`);
              recordModelResult(requestModel, { statusCode, durationMs: Date.now() - upstreamStart, error: `http_${statusCode}`, emptyOutput });
              recordFailure();
              safeWriteHead(statusCode, filterHeaders(upstreamRes.headers));
              safeEnd(raw);
              finishProxy();
              resolveProxy();
            });
            return;
          }

          // 5xx retry — if exhausted, mark model unhealthy
          if (isRetryable(statusCode, null) && attempt < cfg.MAX_RETRIES_NUM) {
            upstreamRes.resume();
            errorHandled = true;
            recordModelResult(requestModel, { statusCode, durationMs: Date.now() - upstreamStart, error: `retry_${statusCode}` });
            log(ts, `${method} ${rawPath} <- ${statusCode}, retrying (${attempt + 1}/${cfg.MAX_RETRIES_NUM})...`);
            const delay = cfg.RETRY_DELAY * Math.pow(2, attempt);
            setTimeout(async () => {
              const result = await doRequest(attempt + 1);
              resolveProxy(result);
            }, delay).unref();
            return;
          }

          // Immediately remove rate-limited models so 9Router falls back
          if (statusCode === 429) markModelExhausted(requestModel);

          if (statusCode >= 500) markModelFailed(requestModel, statusCode);

          recordSuccess();

          const filteredHeaders = filterHeaders(upstreamRes.headers);
          isSse = (upstreamRes.headers["content-type"] || "").includes("text/event-stream");
          if (filteredHeaders["set-cookie"]) {
            const v = filteredHeaders["set-cookie"];
            filteredHeaders["set-cookie"] = Array.isArray(v) ? v : [v];
          }
          if (isSse) {
            filteredHeaders["X-Accel-Buffering"] = "no";
            filteredHeaders["Cache-Control"] = "no-cache";
            filteredHeaders["Connection"] = "keep-alive";
            filteredHeaders["Pragma"] = "no-cache";
            filteredHeaders["Expire"] = "0";
            if (req.socket && !req.socket.destroyed) {
              req.socket.setKeepAlive(true);
              req.socket.setNoDelay(true);
              req.socket.setTimeout(0);
            }
          }

          if (statusCode !== 200) {
            if (!safeWriteHead(statusCode, filteredHeaders)) {
              upstreamRes.resume();
              finishProxy();
              resolveProxy();
              return;
            }
            const errChunks = [];
            upstreamRes.on("data", (c) => errChunks.push(c));
            upstreamRes.on("end", () => {
              const raw = Buffer.concat(errChunks);
              const emptyOutput = responseHasEmptyOutput(statusCode, raw);
              if (emptyOutput) markModelDegraded(requestModel, "empty_output");
              log(ts, `${method} ${rawPath} <- ${statusCode} (${raw.length}b)`);
              log(ts, `RESPONSE BODY: ${redactSensitive(truncate(raw.toString("utf8"), 1000))}`);
              recordModelResult(requestModel, { statusCode, durationMs: Date.now() - upstreamStart, error: `http_${statusCode}`, emptyOutput });
              safeEnd(raw);
              finishProxy();
              resolveProxy();
            });
            upstreamRes.on("error", () => {
              recordModelResult(requestModel, { statusCode, durationMs: Date.now() - upstreamStart, error: "upstream_response_error" });
              safeEnd();
              finishProxy();
              resolveProxy();
            });
            return;
          }

          // 200 streaming response
          const reqStart = Date.now();
          let streamFinished = false;
          let chunkCount = 0;
          let sawDataEvent = false;
          let maxChunkSize = 0;

          function finishStream() {
            if (streamFinished) return;
            streamFinished = true;
            clearIdleTimer();
            finishProxy();
            resolveProxy();
          }

          if (!safeWriteHead(200, filteredHeaders)) {
            upstreamRes.resume();
            finishStream();
            return;
          }

          logDebug(ts, `${method} ${rawPath} <- TTFB ${Date.now() - reqStart}ms, status 200, SSE=${isSse}`);

          function resetChunkTimer() {
            if (chunkTimer && sawDataEvent) { clearTimeout(chunkTimer); chunkTimer = null; }
            if (!sawDataEvent && chunkTimer) return;
            if (isSse && !streamFinished) {
              const timeout = sawDataEvent ? cfg.SSE_CHUNK_TIMEOUT : Math.min(60000, cfg.SSE_CHUNK_TIMEOUT);
              chunkTimer = setTimeout(() => {
                if (streamFinished) return;
                if (!sawDataEvent) {
                  log(ts, `${method} ${rawPath} <- KEEPALIVE-ONLY STREAM (${chunkCount} pings, no real data in ${timeout / 1000}s)`);
                } else {
                  log(ts, `${method} ${rawPath} <- SSE CHUNK TIMEOUT (${timeout / 1000}s no data)`);
                }
                recordModelResult(requestModel, { statusCode: 504, durationMs: Date.now() - reqStart, chunks: chunkCount, error: sawDataEvent ? "sse_chunk_timeout" : "keepalive_only" });
                markModelDegraded(requestModel, sawDataEvent ? "sse_chunk_timeout" : "keepalive_only");
                if (!sawMessageStop) { safeWrite(`\n${SSE_EOM}\ndata: {}\n\n`); }
                safeEnd();
                if (!upstreamReq.destroyed) upstreamReq.destroy();
                finishStream();
              }, timeout);
              chunkTimer.unref();
            }
          }

          function startKeepalive() {
            if (keepaliveTimer) return;
            keepaliveTimer = setInterval(() => {
              if (streamFinished || res.writableEnded) { clearInterval(keepaliveTimer); keepaliveTimer = null; return; }
              const canContinue = res.write(":\n\n");
              if (canContinue === false && cfg.IS_DEBUG) { logDebug(ts, "keepalive backpressure, skipping tick"); }
            }, 10000);
            keepaliveTimer.unref();
          }

          function resetIdleTimer() {
            clearIdleTimer();
            if (isSse && !streamFinished) {
              idleTimer = setTimeout(() => {
                if (streamFinished) return;
                log(ts, `${method} ${rawPath} <- SSE IDLE TIMEOUT (${cfg.SSE_IDLE / 1000}s no data)`);
                recordModelResult(requestModel, { statusCode: 504, durationMs: Date.now() - reqStart, chunks: chunkCount, error: "sse_idle_timeout" });
                markModelDegraded(requestModel, "sse_idle_timeout");
                if (!sawMessageStop) { safeWrite(`\n${SSE_EOM}\ndata: {}\n\n`); }
                safeEnd();
                if (!upstreamReq.destroyed) upstreamReq.destroy();
                finishStream();
              }, cfg.SSE_IDLE);
              idleTimer.unref();
            }
          }

          resetIdleTimer();
          resetChunkTimer();
          if (isSse) startKeepalive();

          upstreamRes.on("data", (chunk) => {
            if (streamFinished) return;
            resetIdleTimer();
            resetChunkTimer();
            chunkCount++;
            maxChunkSize = Math.max(maxChunkSize, chunk.length);
            logDebug(ts, `${method} ${rawPath} <- CHUNK #${chunkCount} ${chunk.length}b, elapsed ${Date.now() - reqStart}ms`);
            if (chunk.length > KEEPALIVE_THRESHOLD) sawDataEvent = true;
            if (chunk.includes(SSE_EOM)) sawMessageStop = true;
            const canContinue = safeWrite(chunk);
            if (canContinue === false && !res.writableEnded) {
              if (res.socket?.destroyed || res.destroyed) {
                safeEnd();
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
              log(ts, `${method} ${rawPath} <- EMPTY SSE STREAM (${chunkCount} chunks, max ${maxChunkSize}b)`);
              markModelDegraded(requestModel, "empty_sse");
            }
            safeEnd();
            const durationMs = Date.now() - reqStart;
            recordModelResult(requestModel, { statusCode: 200, durationMs, chunks: chunkCount, emptyOutput: isSse && !sawDataEvent });
            if (durationMs >= cfg.SLOW_RESPONSE_MS_INT) markModelDegraded(requestModel, `slow_${durationMs}ms`);
            log(ts, `${method} ${rawPath} <- 200 (stream complete, ${durationMs}ms, ${chunkCount} chunks)`);
            finishStream();
          });

          upstreamRes.on("error", (e) => {
            if (streamFinished) return;
            const causeCode = e.cause?.code ? ` (cause: ${e.cause.code})` : "";
            log(ts, `${method} ${rawPath} <- UPSTREAM STREAM ERROR: ${e.message}${causeCode}`);
            recordModelResult(requestModel, { statusCode: 502, durationMs: Date.now() - reqStart, chunks: chunkCount, error: e.message });
            if (isSse && !sawMessageStop) { safeWrite(`\n${SSE_EOM}\ndata: {}\n\n`); }
            safeEnd();
            finishStream();
          });

          upstreamRes.on("close", () => {
            if (streamFinished) return;
            if (!res.writableEnded) {
                log(ts, `${method} ${rawPath} <- UPSTREAM CLOSED (connection terminated prematurely, ${Date.now() - reqStart}ms, ${chunkCount} chunks)`);
                recordModelResult(requestModel, { statusCode: 502, durationMs: Date.now() - reqStart, chunks: chunkCount, error: "upstream_closed" });
                if (isSse && !sawMessageStop) { safeWrite(`\n${SSE_EOM}\ndata: {}\n\n`); }
              safeEnd();
            }
            finishStream();
          });
        });

        currentUpstreamReq = upstreamReq;

        // Response timeout: adaptive based on body size
        responseTimer = setTimeout(() => {
          if (upstreamResponded) return;
          if (errorHandled) return;
          errorHandled = true;
          clearIdleTimer();
          if (!upstreamReq.destroyed) upstreamReq.destroy(new Error('upstream response timeout'));
        }, adaptiveResponseTimeout);
        responseTimer.unref();

        upstreamReq.on("timeout", () => {
          if (errorHandled) return;
          errorHandled = true;
          clearIdleTimer();
          upstreamReq.destroy();
          handleError(new Error("timeout"));
        });

        upstreamReq.on("error", (e) => {
          if (errorHandled) return;
          errorHandled = true;
          clearIdleTimer();
          handleError(e);
        });

        async function handleError(e) {
          if (proxyDone) { resolveProxy(); return; }

          if (res.headersSent) {
            recordFailure();
            log(ts, `${method} ${rawPath} -> STREAM ERROR after partial response: ${e.message}`);
            recordModelResult(requestModel, { statusCode: 502, error: e.message });
            if (isSse && !sawMessageStop) { try { res.write(`\n${SSE_EOM}\ndata: {}\n\n`); } catch {} }
            try { res.end(); } catch {}
            finishProxy();
            resolveProxy();
            return;
          }

          if (attempt < cfg.MAX_RETRIES_NUM && isRetryable(null, e.message)) {
            log(ts, `${method} ${rawPath} -> ERROR: ${e.message}, retrying (${attempt + 1}/${cfg.MAX_RETRIES_NUM})...`);
            const delay = cfg.RETRY_DELAY * Math.pow(2, attempt);
            await sleep(delay);
            if (proxyDone) { resolveProxy(); return; }
            const result = await doRequest(attempt + 1);
            resolveProxy(result);
            return;
          }

          recordFailure();
          markModelFailed(requestModel, 503);
          log(ts, `${method} ${rawPath} -> ERROR: ${e.message} (final)`);
          recordModelResult(requestModel, { statusCode: e.message === "timeout" || e.message === "upstream response timeout" ? 504 : 502, error: e.message });
          if (e.message === "timeout" || e.message === "upstream response timeout") {
            safeRespondJson(504, { error: { code: "timeout", message: "Upstream request timed out", type: "proxy_error" } });
          } else {
            safeRespondJson(502, { error: { code: "proxy_error", message: e.message, type: "proxy_error" } });
          }
          finishProxy();
          resolveProxy();
        }

        // Manual request timeout
        reqTimer = setTimeout(() => {
          if (errorHandled) return;
          errorHandled = true;
          clearIdleTimer();
          upstreamReq.destroy();
          handleError(new Error("timeout"));
        }, cfg.TIMEOUT);
        reqTimer.unref();

        const fullBody = Buffer.concat(body);
        const rawBody = injectPrompt(fullBody, path, cfg.INJECT_SYSTEM_PROMPT_VAL);
        if (rawBody.length) upstreamReq.write(rawBody);
        upstreamReq.end();
      });
    }

    doRequest(0).catch((e) => {
      log(ts, `${method} ${rawPath} -> UNHANDLED PROXY ERROR: ${e.message}`);
      safeRespondJson(500, { error: { code: "internal_error", message: "Proxy internal error", type: "proxy_error" } });
      finishProxy();
    });
  });

  req.on("close", () => {
    if (proxyDone) return;
    const shouldAbort = !hasEnded || req.socket?.destroyed;
    if (shouldAbort && currentUpstreamReq && !currentUpstreamReq.destroyed) {
      currentUpstreamReq.destroy();
    }
  });
  req.on("error", () => {
    if (proxyDone) return;
    if (currentUpstreamReq && !currentUpstreamReq.destroyed) {
      currentUpstreamReq.destroy();
    }
  });
  res.on("close", () => {
    if (proxyDone) return;
    if (!res.writableFinished && currentUpstreamReq && !currentUpstreamReq.destroyed) {
      currentUpstreamReq.destroy();
    }
  });
});

server.headersTimeout = 30000;
server.requestTimeout = 0;

// ── Start & Shutdown ──

server.listen(cfg.PORT, "0.0.0.0", async () => {
  console.log(`AgentRouter proxy listening on port ${cfg.PORT}, target=${cfg.TARGET_HOST_VAL}:${cfg.TARGET_PORT_INT}`);
  await resolveDns();
  scheduleWarmup();
  scheduleDiscovery();
  startProbeLoop(getModelsList, getWafCookie, SPOOF_HEADERS);
});

function shutdown(signal) {
  console.log(`\n[${new Date().toISOString()}] ${signal} received — draining ${activeStreams} active streams...`);
  stopProbeLoop();
  server.close(() => {
    cfg.AGENT.destroy();
    console.log(`[${new Date().toISOString()}] Server closed, exiting.`);
    process.exit(0);
  });
  setTimeout(() => {
    cfg.AGENT.destroy();
    console.error(`[${new Date().toISOString()}] Forced exit after timeout`);
    process.exit(1);
  }, 15000).unref();
}

process.on("SIGTERM", () => shutdown("SIGTERM"));
process.on("SIGINT", () => shutdown("SIGINT"));
process.on("uncaughtException", (err, origin) => {
  console.error(`[${new Date().toISOString()}] UNCAUGHT EXCEPTION (${origin}): ${err.message}`);
  console.error(err.stack);
});
process.on("unhandledRejection", (reason) => {
  const ts = new Date().toISOString();
  const msg = reason instanceof Error ? reason.message : String(reason);
  console.error(`[${ts}] UNHANDLED REJECTION: ${msg}`);
  if (reason instanceof Error && reason.stack) console.error(reason.stack);
});
