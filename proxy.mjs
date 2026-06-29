import http from "node:http";
import https from "node:https";
import { setTimeout as sleep } from "node:timers/promises";

const {
  LISTEN_PORT = "8318",
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
const TIMEOUT = parseInt(REQUEST_TIMEOUT_MS, 10);
const WARMUP_INTERVAL = parseInt(WARMUP_INTERVAL_MS, 10);
const MAX_RETRIES_NUM = parseInt(MAX_RETRIES, 10);
const RETRY_DELAY = parseInt(RETRY_DELAY_MS, 10);
const DISCOVERY_INTERVAL = parseInt(DISCOVERY_INTERVAL_MS, 10);

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
      const req = https.request(
        {
          hostname: TARGET_HOST,
          port: parseInt(TARGET_PORT, 10),
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

// ── Connection pooling ──

const AGENT = new https.Agent({
  keepAlive: true,
  keepAliveMsecs: 60000,
  maxSockets: 64,
  maxFreeSockets: 16,
  timeout: 120000,
  scheduling: "lifo",
});

// ── DNS cache ──

let cachedIps = [];
let dnsExpiry = 0;

async function resolveDns() {
  const ts = new Date().toISOString();
  try {
    const { Resolver } = await import("node:dns/promises");
    const resolver = new Resolver();
    const addresses = await resolver.resolve4(TARGET_HOST);
    cachedIps = addresses;
    dnsExpiry = Date.now() + 300000;
    log(ts, `DNS resolved ${TARGET_HOST} → ${addresses.join(", ")}`);
  } catch {
    if (!cachedIps.length) {
      log(ts, `DNS resolution failed for ${TARGET_HOST}`);
    }
  }
}

function getTargetIp() {
  if (Date.now() > dnsExpiry) return null;
  return cachedIps.length ? cachedIps[0] : null;
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
        const req = https.request(
          {
            hostname: TARGET_HOST,
            port: parseInt(TARGET_PORT, 10),
            path: "/",
            method: "GET",
            headers: WARMUP_HEADERS,
            agent: AGENT,
            rejectUnauthorized: true,
            timeout: 10000,
          },
          (res) => {
            const waf = extractWafCookies(res);
            res.resume();
            resolve(waf);
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

// ── Helpers ──

function log(ts, msg) {
  console.log(`[${ts}] ${msg}`);
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
  req.on("data", (c) => body.push(c));
  req.on("end", () => {
    const path = rewritePath(rawPath);

    const upstreamHeaders = {
      ...SPOOF_HEADERS,
      "Content-Type": "application/json",
      ...(req.headers["authorization"] ? { Authorization: req.headers["authorization"] } : {}),
      ...(req.headers["x-api-key"] ? { "x-api-key": req.headers["x-api-key"] } : {}),
      ...(req.headers["anthropic-version"] ? { "anthropic-version": req.headers["anthropic-version"] } : {}),
    };

    if (wafCookieStr) upstreamHeaders["Cookie"] = wafCookieStr;

    let cleanBody = null;
    if (body.length) {
      try {
        const parsed = JSON.parse(Buffer.concat(body).toString());
        if (parsed.stream_options && !parsed.stream) delete parsed.stream_options;
        cleanBody = JSON.stringify(parsed);
      } catch {
        cleanBody = Buffer.concat(body);
      }
    }

    if (isCircuitOpen()) {
      log(ts, `${method} ${rawPath} -> REJECTED (circuit open)`);
      respondJson(res, 503, {
        error: { code: "circuit_open", message: "Upstream circuit breaker open, retry later", type: "proxy_error" },
      });
      return;
    }

    async function doRequest(attempt) {
      // Refresh DNS if needed
      const ip = getTargetIp();
      if (!ip && cachedIps.length) {
        resolveDns().catch(() => {});
      }

      log(ts, `${method} ${rawPath} -> ${path} (attempt ${attempt + 1})`);

      return new Promise((resolveProxy) => {
        const opts = {
          hostname: TARGET_HOST,
          port: parseInt(TARGET_PORT, 10),
          path,
          method,
          headers: upstreamHeaders,
          agent: AGENT,
          rejectUnauthorized: true,
          timeout: TIMEOUT,
        };

        const upstreamReq = https.request(opts, (upstreamRes) => {
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
              // Not a WAF block, pass through
              log(ts, `${method} ${rawPath} <- ${statusCode} (${raw.length}b)`);
              log(ts, `RESPONSE BODY: ${raw.toString("utf8").slice(0, 2000)}`);
              recordFailure();
              res.writeHead(statusCode, upstreamRes.headers);
              res.end(raw);
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
          res.writeHead(statusCode, upstreamRes.headers);
          const isError = statusCode !== 200;
          if (isError) {
            const errChunks = [];
            upstreamRes.on("data", (c) => errChunks.push(c));
            upstreamRes.on("end", () => {
              const raw = Buffer.concat(errChunks);
              log(ts, `${method} ${rawPath} <- ${statusCode} (${raw.length}b)`);
              log(ts, `RESPONSE BODY: ${raw.toString("utf8").slice(0, 2000)}`);
              res.end(raw);
              resolveProxy();
            });
          } else {
            upstreamRes.on("data", (chunk) => res.write(chunk));
            upstreamRes.on("end", () => {
              res.end();
              log(ts, `${method} ${rawPath} <- ${statusCode} (stream complete)`);
              resolveProxy();
            });
          }
        });

        upstreamReq.on("timeout", () => {
          upstreamReq.destroy();
          handleError(new Error("timeout"));
        });

        upstreamReq.on("error", (e) => handleError(e));

        async function handleError(e) {
          if (attempt < MAX_RETRIES_NUM && isRetryable(null, e.message)) {
            log(ts, `${method} ${rawPath} -> ERROR: ${e.message}, retrying (${attempt + 1}/${MAX_RETRIES_NUM})...`);
            const delay = RETRY_DELAY * Math.pow(2, attempt);
            await sleep(delay);
            const result = await doRequest(attempt + 1);
            resolveProxy(result);
            return;
          }

          recordFailure();
          log(ts, `${method} ${rawPath} -> ERROR: ${e.message} (final)`);
          if (!res.headersSent) {
            if (e.message === "timeout") {
              respondJson(res, 504, {
                error: { code: "timeout", message: "Upstream request timed out", type: "proxy_error" },
              });
            } else {
              respondJson(res, 502, {
                error: { code: "proxy_error", message: e.message, type: "proxy_error" },
              });
            }
          } else {
            res.destroy(e);
          }
          resolveProxy();
        }

        if (cleanBody) upstreamReq.write(cleanBody);
        upstreamReq.end();
      });
    }

    doRequest(0).catch(() => {});
  });
});

// ── Start ──

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
