import http from "node:http";
import https from "node:https";
import { resolve4 } from "node:dns/promises";
import { setTimeout as sleep } from "node:timers/promises";

const {
  LISTEN_PORT = "8318",
  TARGET_PROTOCOL = "https",
  TARGET_HOST = "agentrouter.org",
  TARGET_PORT = "443",
  REQUEST_TIMEOUT_MS = "120000",
  MODELS_CSV = "claude-opus-4-6,claude-opus-4-7,claude-opus-4-8,glm-5.2,gpt-5.5",
  WARMUP_INTERVAL_MS = "180000",
  MAX_RETRIES = "2",
  RETRY_DELAY_MS = "1000",
  AR_API_KEY = "",
  DISCOVERY_INTERVAL_MS = "600000",
} = process.env;

const PORT = parseInt(LISTEN_PORT, 10);
const TARGET_PORT_INT = parseInt(TARGET_PORT, 10);
const TIMEOUT = parseInt(REQUEST_TIMEOUT_MS, 10);
const WARMUP_INTERVAL = parseInt(WARMUP_INTERVAL_MS, 10);
const MAX_RETRIES_NUM = parseInt(MAX_RETRIES, 10);
const RETRY_DELAY = parseInt(RETRY_DELAY_MS, 10);
const DISCOVERY_INTERVAL = parseInt(DISCOVERY_INTERVAL_MS, 10);

const HOP_BY_HOP = new Set([
  "transfer-encoding", "connection", "keep-alive",
  "proxy-authenticate", "proxy-authorization",
  "te", "trailer", "upgrade",
]);

const SSE_EOM = "event: message_stop";

const UPSTREAM_MODULE = TARGET_PROTOCOL === "http" ? http : https;

const AGENT = new UPSTREAM_MODULE.Agent({
  keepAlive: true,
  keepAliveMsecs: 1000,
  maxSockets: 64,
  maxFreeSockets: 16,
  scheduling: "lifo",
});

let activeStreams = 0;

const SPOOF_HEADERS = {
  "User-Agent": "claude-cli/2.1.92 (external, sdk-cli)",
  "Anthropic-Version": "2023-06-01",
  "Anthropic-Beta":
    "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advanced-tool-use-2025-11-20,effort-2025-11-24,structured-outputs-2025-12-15,fast-mode-2026-02-01,redact-thinking-2026-02-12,token-efficient-tools-2026-03-28",
  "Anthropic-Dangerous-Direct-Browser-Access": "true",
  "X-App": "cli",
  "X-Stainless-Helper-Method": "stream",
  "X-Stainless-Retry-Count": "0",
  "X-Stainless-Runtime-Version": "v24.14.0",
  "X-Stainless-Package-Version": "0.80.0",
  "X-Stainless-Runtime": "node",
  "X-Stainless-Lang": "js",
  "X-Stainless-Arch": "arm64",
  "X-Stainless-Os": "Linux",
  "X-Stainless-Timeout": "600",
};

const WARMUP_HEADERS = {
  "User-Agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
  Accept: "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
  "Accept-Language": "en-US,en;q=0.9",
  "Accept-Encoding": "gzip, deflate, br",
  Connection: "keep-alive",
  "Upgrade-Insecure-Requests": "1",
  "Sec-Fetch-Dest": "document",
  "Sec-Fetch-Mode": "navigate",
  "Sec-Fetch-Site": "none",
  "Sec-Fetch-User": "?1",
};

const STATIC_MODELS = MODELS_CSV.split(",").map((id) => ({
  id: id.trim(),
  object: "model",
  created: 1626777600,
  owned_by: "agentrouter",
}));

let modelsList = [...STATIC_MODELS];
let modelSource = "static";

async function fetchModels() {
  if (!AR_API_KEY) return;
  const ts = new Date().toISOString();
  try {
    const data = await new Promise((resolve, reject) => {
      const req = UPSTREAM_MODULE.request(
        {
          hostname: TARGET_HOST,
          port: TARGET_PORT_INT,
          path: "/v1/models",
          method: "GET",
          headers: {
            Authorization: `Bearer ${AR_API_KEY}`,
            "User-Agent": "agentrouter-spoof-proxy/1.0",
            Accept: "application/json",
          },
            agent: AGENT,
            rejectUnauthorized: true,
            timeout: 15000,
          },
        (res) => {
          const chunks = [];
          res.on("data", (c) => chunks.push(c));
          res.on("end", () => {
            const raw = Buffer.concat(chunks);
            if (res.statusCode === 200) {
              try { resolve(JSON.parse(raw)); }
              catch { reject(new Error("bad json")); }
            } else {
              reject(new Error(`status ${res.statusCode}`));
            }
          });
        }
      );
      req.on("error", reject);
      req.on("timeout", () => { req.destroy(); reject(new Error("timeout")); });
      req.end();
    });

    if (data?.data && Array.isArray(data.data)) {
      modelsList = data.data.map((m) => ({
        id: m.id,
        object: "model",
        created: m.created || 1626777600,
        owned_by: m.owned_by || "agentrouter",
      }));
      modelSource = "dynamic";
      log(ts, `DISCOVERED ${modelsList.length} models from upstream`);
    }
  } catch (e) {
    log(ts, `Model discovery failed: ${e.message}, using static list`);
    modelSource = "static";
    modelsList = [...STATIC_MODELS];
  }
}

// ── DNS cache ──

let cachedIps = [];
let dnsExpiry = 0;

async function resolveDns() {
  const ts = new Date().toISOString();
  try {
    const addresses = await resolve4(TARGET_HOST);
    cachedIps = addresses;
    dnsExpiry = Date.now() + 300000;
    log(ts, `DNS resolved ${TARGET_HOST} → ${addresses.join(", ")}`);
  } catch {
    if (!cachedIps.length) {
      log(ts, `DNS resolution failed for ${TARGET_HOST}`);
    }
  }
}

// ── WAF Cookie Store ──

let wafCookieStr = "";

function extractWafCookies(res) {
  const cookies = res.headers["set-cookie"] || [];
  const waf = [];
  for (const c of cookies) {
    const name = c.split("=")[0];
    if (name === "acw_tc" || name === "acw_sc__v2" || name === "cdn_sec_tc") {
      waf.push(c.split(";")[0]);
    }
  }
  return waf;
}

async function warmup() {
  const ts = new Date().toISOString();
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      const cookie = await new Promise((resolve, reject) => {
        const req = UPSTREAM_MODULE.request(
          {
            hostname: TARGET_HOST,
            port: TARGET_PORT_INT,
            path: "/",
            method: "GET",
            headers: WARMUP_HEADERS,
            agent: false,
            rejectUnauthorized: true,
            timeout: 10000,
          },
          (res) => {
            const waf = extractWafCookies(res);
            res.resume();
            res.on("end", () => resolve(waf));
          }
        );
        req.on("error", reject);
        req.on("timeout", () => { req.destroy(); reject(new Error("timeout")); });
        req.end();
      });

      if (cookie.length) {
        wafCookieStr = cookie.join("; ");
        log(ts, `WARMUP → 200 cookies: ${cookie.length}`);
        return;
      }
    } catch {}
    if (attempt < 2) await sleep(1000 * (attempt + 1));
  }
  log(ts, `WARMUP failed after 3 attempts`);
}

function scheduleWarmup() {
  warmup();
  setInterval(warmup, WARMUP_INTERVAL);
}

// ── Circuit breaker ──

let consecutiveFails = 0;
let circuitOpenUntil = 0;

function isCircuitOpen() {
  if (Date.now() > circuitOpenUntil) return false;
  return true;
}

function recordSuccess() {
  consecutiveFails = 0;
}

function recordFailure() {
  consecutiveFails++;
  if (consecutiveFails >= 5) {
    circuitOpenUntil = Date.now() + Math.min(60000 * Math.pow(2, consecutiveFails - 5), 600000);
    log(new Date().toISOString(), `CIRCUIT OPEN for ${(circuitOpenUntil - Date.now()) / 1000}s (${consecutiveFails} consecutive failures)`);
  }
}

function log(ts, msg) {
  console.log(`[${ts}] ${msg}`);
}

function filterHeaders(headers) {
  if (!headers) return {};
  const out = {};
  for (const [k, v] of Object.entries(headers)) {
    if (!HOP_BY_HOP.has(k.toLowerCase())) out[k] = v;
  }
  return out;
}

function rewritePath(path) {
  if (path === "/messages" || path.startsWith("/messages?"))
    return path.replace("/messages", "/v1/messages");
  if (path === "/v1/messages" || path.startsWith("/v1/messages?")) return path;
  if (path === "/v1/chat/completions" || path.startsWith("/v1/chat/completions?")) return path;
  return path;
}

function respondJson(res, status, data) {
  res.writeHead(status, {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
  });
  res.end(JSON.stringify(data));
}

function isWafBlock(statusCode, body) {
  if (statusCode !== 405 && statusCode !== 403) return false;
  const html = typeof body === "string" ? body : body.toString("utf8");
  return html.includes("alicdn") || html.includes("block_message") || html.includes("renderData");
}

function isRetryable(statusCode, errorMessage) {
  if (statusCode >= 500 && statusCode <= 599) return true;
  if (!statusCode) return true;
  if (errorMessage && (errorMessage.includes("socket hang up") || errorMessage.includes("timeout") || errorMessage.includes("ECONNRESET") || errorMessage.includes("ETIMEDOUT") || errorMessage.includes("ENETUNREACH"))) return true;
  return false;
}

// ── SSE idle timeout (5 min no data = hung stream) ──
const SSE_IDLE_TIMEOUT_MS = 300000;

// ── Server ──

const server = http.createServer((req, res) => {
  const ts = new Date().toISOString();
  const rawPath = req.url;
  const method = req.method;

  // ── Health check ──
  if (method === "GET" && (rawPath === "/health" || rawPath === "/api/health")) {
    respondJson(res, 200, {
      ok: true,
      upstream: `${TARGET_HOST}:${TARGET_PORT}`,
      modelSource,
      staticModels: STATIC_MODELS.length,
      availableModels: modelsList.length,
      activeStreams,
      wafCookie: !!wafCookieStr,
      circuitOpen: isCircuitOpen(),
      consecutiveFails,
      cachedIps: cachedIps.length,
    });
    return;
  }

  // ── Model list ──
  if (method === "GET" && (rawPath === "/v1/models" || rawPath === "/models")) {
    respondJson(res, 200, { data: modelsList, object: "list" });
    return;
  }

  // ── Proxy ──
  const body = [];
  let currentUpstreamReq = null;
  let proxyDone = false;
  let hasEnded = false;

  function finishProxy() {
    if (proxyDone) return;
    proxyDone = true;
    activeStreams--;
  }

  // Safe response helpers — guard against ERR_HTTP_HEADERS_SENT
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

  req.on("data", (c) => body.push(c));
  req.on("end", () => {
    hasEnded = true;
    const path = rewritePath(rawPath);

    const upstreamHeaders = {
      ...SPOOF_HEADERS,
      "Content-Type": "application/json",
      ...(req.headers["authorization"] ? { Authorization: req.headers["authorization"] } : {}),
      ...(req.headers["x-api-key"] ? { "x-api-key": req.headers["x-api-key"] } : {}),
      ...(req.headers["anthropic-version"] ? { "anthropic-version": req.headers["anthropic-version"] } : {}),
    };

    if (wafCookieStr) upstreamHeaders["Cookie"] = wafCookieStr;

    if (isCircuitOpen()) {
      log(ts, `${method} ${rawPath} -> REJECTED (circuit open)`);
      safeRespondJson(503, {
        error: { code: "circuit_open", message: "Upstream circuit breaker open, retry later", type: "proxy_error" },
      });
      return;
    }

    activeStreams++;

    async function doRequest(attempt) {
      log(ts, `${method} ${rawPath} -> ${path} (attempt ${attempt + 1})`);

      return new Promise((resolveProxy) => {
        // Guard: if proxy already finished (client disconnected, previous attempt resolved), skip
        if (proxyDone) { resolveProxy(); return; }

        const opts = {
          hostname: TARGET_HOST,
          port: TARGET_PORT_INT,
          path,
          method,
          headers: upstreamHeaders,
          agent: AGENT,
          rejectUnauthorized: true,
          timeout: TIMEOUT,
        };

        let errorHandled = false;  // prevent double-invocation of handleError (timeout fires error)
        let idleTimer = null;      // SSE idle timeout
        let reqTimer = null;       // upstream request timeout (manual, more reliable than opts.timeout)
        let isSse = false;
        let sawMessageStop = false;

        function clearIdleTimer() {
          if (idleTimer) { clearTimeout(idleTimer); idleTimer = null; }
          if (reqTimer) { clearTimeout(reqTimer); reqTimer = null; }
        }

        const upstreamReq = UPSTREAM_MODULE.request(opts, (upstreamRes) => {
          const statusCode = upstreamRes.statusCode;

          // Handle WAF block: re-warmup and retry once
          if ((statusCode === 405 || statusCode === 403) && attempt === 0) {
            let chunks = [];
            upstreamRes.on("data", (c) => chunks.push(c));
            upstreamRes.on("end", async () => {
              const raw = Buffer.concat(chunks);
              if (isWafBlock(statusCode, raw)) {
                log(ts, `WAF ${statusCode} detected, refreshing cookie and retrying...`);
                await warmup();
                if (wafCookieStr) upstreamHeaders["Cookie"] = wafCookieStr;
                const result = await doRequest(attempt + 1);
                resolveProxy(result);
                return;
              }
              log(ts, `${method} ${rawPath} <- ${statusCode} (${raw.length}b)`);
              log(ts, `RESPONSE BODY: ${raw.toString("utf8").slice(0, 2000)}`);
              recordFailure();
              safeWriteHead(statusCode, filterHeaders(upstreamRes.headers));
              safeEnd(raw);
              finishProxy();
              resolveProxy();
            });
            return;
          }

          // Retry on 5xx
          if (isRetryable(statusCode, null) && attempt < MAX_RETRIES_NUM) {
            upstreamRes.resume();
            log(ts, `${method} ${rawPath} <- ${statusCode}, retrying (${attempt + 1}/${MAX_RETRIES_NUM})...`);
            const delay = RETRY_DELAY * Math.pow(2, attempt);
            setTimeout(async () => {
              const result = await doRequest(attempt + 1);
              resolveProxy(result);
            }, delay).unref();
            return;
          }

          recordSuccess();

          const filteredHeaders = filterHeaders(upstreamRes.headers);
          isSse = (upstreamRes.headers["content-type"] || "").includes("text/event-stream");
          if (isSse) {
            filteredHeaders["X-Accel-Buffering"] = "no";
            filteredHeaders["Cache-Control"] = "no-cache";
          }

          if (!safeWriteHead(statusCode, filteredHeaders)) {
            // Headers already sent (race condition) — drain upstream, don't double-send
            upstreamRes.resume();
            finishProxy();
            resolveProxy();
            return;
          }

          if (statusCode !== 200) {
            const errChunks = [];
            upstreamRes.on("data", (c) => errChunks.push(c));
            upstreamRes.on("end", () => {
              const raw = Buffer.concat(errChunks);
              log(ts, `${method} ${rawPath} <- ${statusCode} (${raw.length}b)`);
              log(ts, `RESPONSE BODY: ${raw.toString("utf8").slice(0, 2000)}`);
              safeEnd(raw);
              finishProxy();
              resolveProxy();
            });
            upstreamRes.on("error", () => {
              safeEnd();
              finishProxy();
              resolveProxy();
            });
            return;
          }

          // Streaming (200 + SSE)
          let streamFinished = false;  // prevent double finish from end+close

          function finishStream() {
            if (streamFinished) return;
            streamFinished = true;
            clearIdleTimer();
            finishProxy();
            resolveProxy();
          }

          // Set up SSE idle timeout — if no data for 5 min, abort
          function resetIdleTimer() {
            clearIdleTimer();
            if (isSse && !streamFinished) {
              idleTimer = setTimeout(() => {
                if (streamFinished) return;
                log(ts, `${method} ${rawPath} <- SSE IDLE TIMEOUT (${SSE_IDLE_TIMEOUT_MS / 1000}s no data)`);
                if (!sawMessageStop) {
                  safeWrite(`\n${SSE_EOM}\ndata: {}\n\n`);
                }
                safeEnd();
                if (!upstreamReq.destroyed) upstreamReq.destroy();
                finishStream();
              }, SSE_IDLE_TIMEOUT_MS);
              idleTimer.unref();
            }
          }

          resetIdleTimer();

          upstreamRes.on("data", (chunk) => {
            if (streamFinished) return;
            resetIdleTimer();
            if (isSse && chunk.includes(SSE_EOM)) sawMessageStop = true;
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
            safeEnd();
            log(ts, `${method} ${rawPath} <- ${statusCode} (stream complete)`);
            finishStream();
          });

          upstreamRes.on("error", (e) => {
            if (streamFinished) return;
            log(ts, `${method} ${rawPath} <- UPSTREAM STREAM ERROR: ${e.message}`);
            if (isSse && !sawMessageStop) {
              safeWrite(`\n${SSE_EOM}\ndata: {}\n\n`);
            }
            safeEnd();
            finishStream();
          });

          upstreamRes.on("close", () => {
            if (streamFinished) return;
            if (!res.writableEnded) {
              log(ts, `${method} ${rawPath} <- UPSTREAM CLOSED (connection terminated prematurely)`);
              if (isSse && !sawMessageStop) {
                safeWrite(`\n${SSE_EOM}\ndata: {}\n\n`);
              }
              safeEnd();
            }
            finishStream();
          });
        });

        // Track current upstream request for client-disconnect handler
        currentUpstreamReq = upstreamReq;

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
            if (isSse && !sawMessageStop) {
              try { res.write(`\n${SSE_EOM}\ndata: {}\n\n`); } catch {}
            }
            try { res.end(); } catch {}
            finishProxy();
            resolveProxy();
            return;
          }

          if (attempt < MAX_RETRIES_NUM && isRetryable(null, e.message)) {
            log(ts, `${method} ${rawPath} -> ERROR: ${e.message}, retrying (${attempt + 1}/${MAX_RETRIES_NUM})...`);
            const delay = RETRY_DELAY * Math.pow(2, attempt);
            await sleep(delay);
            if (proxyDone) { resolveProxy(); return; }
            const result = await doRequest(attempt + 1);
            resolveProxy(result);
            return;
          }

          recordFailure();
          log(ts, `${method} ${rawPath} -> ERROR: ${e.message} (final)`);
          if (e.message === "timeout") {
            safeRespondJson(504, {
              error: { code: "timeout", message: "Upstream request timed out", type: "proxy_error" },
            });
          } else {
            safeRespondJson(502, {
              error: { code: "proxy_error", message: e.message, type: "proxy_error" },
            });
          }
          finishProxy();
          resolveProxy();
        }

        // Manual request timeout (more reliable than opts.timeout with keep-alive pool)
        reqTimer = setTimeout(() => {
          if (errorHandled) return;
          errorHandled = true;
          clearIdleTimer();
          upstreamReq.destroy();
          handleError(new Error("timeout"));
        }, TIMEOUT);
        reqTimer.unref();

        const rawBody = Buffer.concat(body);
        if (rawBody.length) upstreamReq.write(rawBody);
        upstreamReq.end();
      });
    }

    doRequest(0).catch((e) => {
      log(ts, `${method} ${rawPath} -> UNHANDLED PROXY ERROR: ${e.message}`);
      safeRespondJson(500, {
        error: { code: "internal_error", message: "Proxy internal error", type: "proxy_error" },
      });
      finishProxy();
    });
  });

  req.on("close", () => {
    if (proxyDone) return;
    const trulyDisconnected = !hasEnded || req.socket?.destroyed;
    if (trulyDisconnected && currentUpstreamReq && !currentUpstreamReq.destroyed) {
      currentUpstreamReq.destroy();
    }
  });
  req.on("error", () => {
    if (proxyDone) return;
    if (currentUpstreamReq && !currentUpstreamReq.destroyed) {
      currentUpstreamReq.destroy();
    }
  });
});

// ── Start & Graceful Shutdown ──

function scheduleDiscovery() {
  if (!AR_API_KEY) {
    console.log(`Model discovery disabled (no AR_API_KEY set), using static list (${STATIC_MODELS.length} models)`);
    return;
  }
  fetchModels();
  setInterval(fetchModels, DISCOVERY_INTERVAL);
}

server.listen(PORT, "0.0.0.0", async () => {
  console.log(`AgentRouter proxy listening on port ${PORT}, target=${TARGET_HOST}:${TARGET_PORT}`);
  await resolveDns();
  scheduleWarmup();
  scheduleDiscovery();
});

function shutdown(signal) {
  console.log(`\n[${new Date().toISOString()}] ${signal} received — draining ${activeStreams} active streams...`);
  server.close(() => {
    console.log(`[${new Date().toISOString()}] Server closed, exiting.`);
    process.exit(0);
  });
  setTimeout(() => {
    console.error(`[${new Date().toISOString()}] Forced exit after timeout`);
    process.exit(1);
  }, 15000).unref();
}

process.on("SIGTERM", () => shutdown("SIGTERM"));
process.on("SIGINT", () => shutdown("SIGINT"));

// Prevent uncaught exceptions (e.g. ERR_HTTP_HEADERS_SENT race) from crashing
process.on("uncaughtException", (err) => {
  console.error(`[${new Date().toISOString()}] UNCAUGHT EXCEPTION: ${err.message}`);
  console.error(err.stack);
  // Don't exit — let Docker health check restart if truly broken
});
