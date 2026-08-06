// Request handler: buffers the client body, then proxies to upstream with
// retry, WAF cookie refresh, model-health marking, and SSE streaming.
//
// Invariants:
//   proxyDone     — set once via finishProxy(); guards double-decrement of
//                   the active-stream counter and double terminal responses
//   errorHandled  — per-attempt; prevents request/response timeouts and the
//                   error listener from acting twice on the same attempt
//   upstreamResponded — response received; disarms the adaptive timeout
//   sawMessageStop    — message_stop seen on the wire (synced from pipeSse);
//                   suppresses EOM injection in handleError after partial data

import { setTimeout as sleep } from "node:timers/promises";
import * as cfg from "../config.mjs";
import { log, logDebug } from "../logger.mjs";
import { buildError, isOurError, E_TIMEOUT, E_CIRCUIT, E_UPSTREAM, E_INTERNAL } from "../errors.mjs";
import { codeToStatus } from "../status-code.mjs";
import { isCircuitOpen, recordSuccess, recordFailure } from "../resilience/circuit-breaker.mjs";
import { getWafCookie, warmup, captureWafCookies } from "../auth/waf.mjs";
import { SPOOF_HEADERS, GENERIC_SPOOF_HEADERS } from "../auth/spoof.mjs";
import { markModelFailed, markModelExhausted, markModelDegraded } from "../models/health.mjs";
import { recordModelStart, recordModelResult } from "../models/stats.mjs";
import {
  MAX_BODY_SIZE, sseErrorFrame, abnormalFinish,
  truncate, filterHeaders, rewritePath, respondJson,
  isWafBlock, isRetryable, getRetryDelay, getResponseTimeout, injectPrompt, summarizeRequest, responseHasEmptyOutput, redactSensitive,
} from "../utils.mjs";
import { pipeSse } from "./stream.mjs";

export function handleProxyRequest(req, res, streams) {
  const ts = new Date().toISOString();
  const rawPath = req.url;
  const method = req.method;

  const body = [];
  let bodySize = 0;
  let bodyRejected = false;
  let uploadTimer = null;
  let currentUpstreamReq = null;
  let proxyDone = false;
  let hasEnded = false;

  function finishProxy() {
    if (proxyDone) return;
    proxyDone = true;
    streams.count--;
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

  function safeRespondJson(status, data) {
    if (res.headersSent || res.writableEnded) return;
    try { respondJson(res, status, data); } catch (e) { log(ts, `safeRespondJson error: ${e.message}`); }
  }

  // Oversized body: send one clean 413, then drain the remaining upload without
  // buffering it (the socket closes after a short grace deadline). Never forward
  // the oversized body upstream and never reset the client before the 413.
  function rejectOversized() {
    rejectOversizedWithStatus(413, "payload_too_large", "Request body exceeds 20MB limit");
  }

  // Client stopped uploading: bounded body-upload deadline so a stalled/slow
  // upload cannot hold a socket open indefinitely.
  function startUploadTimer() {
    uploadTimer = setTimeout(() => {
      if (hasEnded || bodyRejected) return;
      log(ts, `${method} ${rawPath} -> REJECTED 408 (body upload timed out after ${cfg.BODY_UPLOAD_TIMEOUT / 1000}s)`);
      rejectOversizedWithStatus(408, "request_timeout", "Request body upload timed out");
    }, cfg.BODY_UPLOAD_TIMEOUT);
    uploadTimer.unref();
  }

  function rejectOversizedWithStatus(status, code, message) {
    if (bodyRejected || res.headersSent || res.writableEnded) return;
    bodyRejected = true;
    if (uploadTimer) { clearTimeout(uploadTimer); uploadTimer = null; }
    log(ts, `${method} ${rawPath} -> REJECTED ${status} (${code})`);
    respondJson(res, status, { error: { code, message, type: "proxy_error" } });
    // Drain the remaining upload (never buffer, never forward) so the client
    // can finish reading the rejection over a live connection — destroying the
    // socket here would deliver ECONNRESET instead of the 413. A short grace
    // deadline still bounds a never-ending upload.
    req.removeAllListeners("data");
    req.removeAllListeners("end");
    req.on("data", () => {});
    const drainDeadline = setTimeout(() => {
      if (req.socket && !req.socket.destroyed) req.socket.destroy();
    }, 5000);
    drainDeadline.unref();
  }

  // Early Content-Length check: reject before buffering any bytes.
  const contentLength = Number(req.headers["content-length"]);
  if (Number.isFinite(contentLength) && contentLength > MAX_BODY_SIZE) {
    rejectOversized();
    return;
  }

  startUploadTimer();

  req.on("data", (c) => {
    if (bodyRejected) return;
    bodySize += c.length;
    if (bodySize > MAX_BODY_SIZE) {
      rejectOversized();
      return;
    }
    body.push(c);
  });

  req.on("end", () => {
    hasEnded = true;
    if (uploadTimer) { clearTimeout(uploadTimer); uploadTimer = null; }
    const path = rewritePath(rawPath);
    // Anthropic and OpenAI streams use different terminal events; the stream
    // pump and the partial-response EOM path must inject the right one.
    const streamFormat = path.startsWith("/v1/chat/completions") ? "openai" : "anthropic";
    const fullBody = Buffer.concat(body);
    body.length = 0;
    const requestSummary = summarizeRequest(fullBody, path, method);
    logDebug(ts, `${method} ${rawPath} -> REQUEST ${JSON.stringify(requestSummary)}`);

    // Adaptive response timeout — larger bodies need more upstream processing time
    const adaptiveResponseTimeout = getResponseTimeout(fullBody.length, cfg.RESPONSE_TIMEOUT);

    // Extract model from body for reactive health marking
    let requestModel = null;
    requestModel = requestSummary.model || null;
    if (requestModel) recordModelStart(requestModel);

    const isAnthropic = path.startsWith("/v1/messages");
    const spoofHeaders = isAnthropic ? SPOOF_HEADERS : GENERIC_SPOOF_HEADERS;

    // The proxy's spoof owns `Anthropic-Version` / `Anthropic-Beta` — clients
    // supply only `Authorization` / `x-api-key`. A client-supplied
    // `anthropic-version` is intentionally NOT forwarded: Node lowercases
    // outgoing header names, so a pass-through entry would silently overwrite
    // the spoofed value (same normalized key, last one set wins).
    const upstreamHeaders = {
      ...spoofHeaders,
      "Content-Type": "application/json",
      ...(req.headers["authorization"] ? { Authorization: req.headers["authorization"] } : {}),
      ...(req.headers["x-api-key"] ? { "x-api-key": req.headers["x-api-key"] } : {}),
    };

    const wafCookie = getWafCookie();
    if (wafCookie) upstreamHeaders["Cookie"] = wafCookie;

    if (isCircuitOpen()) {
      log(ts, `${method} ${rawPath} -> REJECTED (circuit open)`);
      safeRespondJson(codeToStatus(E_CIRCUIT), { error: { code: "circuit_open", message: "Upstream circuit breaker open, retry later", type: "proxy_error" } });
      return;
    }

    streams.count++;

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
        let reqTimer = null;
        let responseTimer = null;
        let isSse = false;
        let sawMessageStop = false;
        let upstreamResponded = false;

        function clearIdleTimer() {
          if (reqTimer) { clearTimeout(reqTimer); reqTimer = null; }
          if (responseTimer) { clearTimeout(responseTimer); responseTimer = null; }
        }

        const upstreamReq = cfg.UPSTREAM_MODULE.request(opts, (upstreamRes) => {
          upstreamResponded = true;
          clearIdleTimer();
          // Every upstream response (200, non-200, WAF block) may rotate WAF
          // session cookies — capture them so the next request carries the
          // freshest values instead of waiting for the next warmup cycle.
          captureWafCookies(upstreamRes.headers);
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
                const newCookie = getWafCookie();
                if (newCookie) upstreamHeaders["Cookie"] = newCookie;
                const delay = getRetryDelay(attempt, cfg.RETRY_DELAY);
                await sleep(delay);
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
          if (isRetryable(statusCode, null, cfg.RETRY_ON_5XX_VAL) && attempt < cfg.MAX_RETRIES_NUM) {
            upstreamRes.resume();
            errorHandled = true;
            log(ts, `${method} ${rawPath} <- ${statusCode}, retrying (${attempt + 1}/${cfg.MAX_RETRIES_NUM})...`);
            const delay = getRetryDelay(attempt, cfg.RETRY_DELAY);
            setTimeout(async () => {
              const result = await doRequest(attempt + 1);
              resolveProxy(result);
            }, delay).unref();
            return;
          }

          // Immediately remove rate-limited models so 9Router falls back
          if (statusCode === 429) markModelExhausted(requestModel);

          if (statusCode >= 500) markModelFailed(requestModel, statusCode);

          // Circuit accounting for a *final* upstream response.
          //   5xx  → provider-side outage: count toward opening the circuit.
          //   429  → per-model rate limit, upstream is healthy and reachable.
          //          The model is locked out above (markModelExhausted) so the
          //          client falls back; the global circuit is left untouched so
          //          one throttled model cannot black out every other model.
          //   4xx  → caller's fault, not an outage: neither fails nor resets.
          //   2xx/3xx → genuine success: reset the consecutive failure run.
          if (statusCode >= 500) {
            recordFailure();
          } else if (statusCode < 400) {
            recordSuccess();
          }

          const filteredHeaders = filterHeaders(upstreamRes.headers);
          // SSE detection: trust the upstream Content-Type first; fall back to
          // the request intent (`"stream": true`) so a missing/mislabeled
          // content-type does not silently disable keepalives, EOM injection
          // and the idle/stall watchdogs (client would hang waiting for a
          // terminal marker).
          const sseByContentType = (upstreamRes.headers["content-type"] || "").includes("text/event-stream");
          isSse = sseByContentType || (statusCode === 200 && requestSummary.stream === true);
          if (filteredHeaders["set-cookie"]) {
            const v = filteredHeaders["set-cookie"];
            filteredHeaders["set-cookie"] = Array.isArray(v) ? v : [v];
          }
          if (isSse) {
            filteredHeaders["X-Accel-Buffering"] = "no";
            filteredHeaders["Cache-Control"] = "no-cache";
            filteredHeaders["Connection"] = "keep-alive";
            filteredHeaders["Pragma"] = "no-cache";
            filteredHeaders["Expires"] = "0";
            if (req.socket && !req.socket.destroyed) {
              req.socket.setKeepAlive(true);
              req.socket.setNoDelay(true);
              req.socket.setTimeout(0);
            }
          }

          if (statusCode !== 200) {
            if (!safeWriteHead(statusCode, filteredHeaders)) {
              upstreamRes.resume();
              if (!upstreamReq.destroyed) upstreamReq.destroy();
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

          if (!safeWriteHead(200, filteredHeaders)) {
            upstreamRes.resume();
            if (!upstreamReq.destroyed) upstreamReq.destroy();
            finishProxy();
            resolveProxy();
            return;
          }

          logDebug(ts, `${method} ${rawPath} <- TTFB ${Date.now() - reqStart}ms, status 200, SSE=${isSse}`);

          pipeSse({
            upstreamRes, upstreamReq, res, isSse, streamFormat,
            chunkTimeoutMs: cfg.SSE_CHUNK_TIMEOUT,
            idleTimeoutMs: cfg.SSE_IDLE,
            slowResponseMs: cfg.SLOW_RESPONSE_MS_INT,
            stripThinkingTags: cfg.STRIP_THINKING_TAGS_VAL,
            log: (msg) => log(ts, `${method} ${rawPath} <- ${msg}`),
            logDebug: (msg) => logDebug(ts, `${method} ${rawPath} <- ${msg}`),
            onResult: (r) => recordModelResult(requestModel, r),
            onDegrade: (reason) => markModelDegraded(requestModel, reason),
            onMessageStop: () => { sawMessageStop = true; },
            onFinish: () => { finishProxy(); resolveProxy(); },
          });
        });

        currentUpstreamReq = upstreamReq;

        // Response timeout: adaptive based on body size.
        // Must drive the retry/error path directly — destroying the upstream
        // request with an error would be swallowed by the `error` listener
        // (errorHandled is already set), leaving the client without a response
        // and streams.count permanently incremented.
        responseTimer = setTimeout(() => {
          if (upstreamResponded) return;
          if (errorHandled) return;
          errorHandled = true;
          clearIdleTimer();
          if (!upstreamReq.destroyed) upstreamReq.destroy();
          handleError(buildError("upstream response timeout", E_TIMEOUT));
        }, adaptiveResponseTimeout);
        responseTimer.unref();

        upstreamReq.on("timeout", () => {
          if (errorHandled) return;
          errorHandled = true;
          clearIdleTimer();
          upstreamReq.destroy();
          handleError(buildError("timeout", E_TIMEOUT));
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
            if (isSse && !sawMessageStop) {
              // Error frame first (strict SDKs surface it), then the synthetic
              // finisher so harness clients terminate cleanly.
              try { res.write(sseErrorFrame(streamFormat, "upstream_error")); } catch {}
              try { res.write(abnormalFinish(streamFormat)); } catch {}
            }
            try { res.end(); } catch {}
            finishProxy();
            resolveProxy();
            return;
          }

          if (attempt < cfg.MAX_RETRIES_NUM && isRetryable(null, e.message, cfg.RETRY_ON_5XX_VAL)) {
            log(ts, `${method} ${rawPath} -> ERROR: ${e.message}, retrying (${attempt + 1}/${cfg.MAX_RETRIES_NUM})...`);
            const delay = getRetryDelay(attempt, cfg.RETRY_DELAY);
            await sleep(delay);
            if (proxyDone) { resolveProxy(); return; }
            const result = await doRequest(attempt + 1);
            resolveProxy(result);
            return;
          }

          recordFailure();
          markModelFailed(requestModel, 503);
          log(ts, `${method} ${rawPath} -> ERROR: ${e.message} (final)`);
          recordModelResult(requestModel, { statusCode: isOurError(e) && e.code === E_TIMEOUT ? codeToStatus(E_TIMEOUT) : codeToStatus(E_UPSTREAM), error: e.message });
          if (isOurError(e) && e.code === E_TIMEOUT) {
            safeRespondJson(codeToStatus(E_TIMEOUT), { error: { code: "timeout", message: "Upstream request timed out", type: "proxy_error" } });
          } else {
            safeRespondJson(codeToStatus(E_UPSTREAM), { error: { code: "proxy_error", message: e.message, type: "proxy_error" } });
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
          handleError(buildError("timeout", E_TIMEOUT));
        }, cfg.TIMEOUT);
        reqTimer.unref();

        const rawBody = injectPrompt(fullBody, path, cfg.INJECT_SYSTEM_PROMPT_VAL);
        if (rawBody.length) upstreamReq.write(rawBody);
        upstreamReq.end();
      });
    }

    doRequest(0).catch((e) => {
      log(ts, `${method} ${rawPath} -> UNHANDLED PROXY ERROR: ${e.message}`);
      safeRespondJson(codeToStatus(E_INTERNAL), { error: { code: "internal_error", message: "Proxy internal error", type: "proxy_error" } });
      finishProxy();
    });
  });

  req.on("close", () => {
    if (uploadTimer) { clearTimeout(uploadTimer); uploadTimer = null; }
    if (proxyDone) return;
    const shouldAbort = !hasEnded || req.socket?.destroyed;
    if (shouldAbort && currentUpstreamReq && !currentUpstreamReq.destroyed) {
      currentUpstreamReq.destroy();
    }
  });
  req.on("error", () => {
    if (uploadTimer) { clearTimeout(uploadTimer); uploadTimer = null; }
    if (proxyDone) return;
    if (currentUpstreamReq && !currentUpstreamReq.destroyed) {
      currentUpstreamReq.destroy();
    }
  });
}