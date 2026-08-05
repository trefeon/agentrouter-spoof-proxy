import { timingSafeEqual } from "node:crypto";

export const HOP_BY_HOP = new Set([
  "transfer-encoding", "connection", "keep-alive",
  "proxy-authenticate", "proxy-authorization",
  "te", "trailer", "upgrade",
]);

export const SSE_EOM = "event: message_stop";
export const SSE_DONE = "data: [DONE]";
export const MAX_BODY_SIZE = 20 * 1024 * 1024;

// Terminal SSE event for the active stream format. Anthropic `messages` streams
// end with `event: message_stop\ndata: {}`; OpenAI `chat.completions` streams
// end with `data: [DONE]`. Injecting the wrong one corrupts the client's
// parser (e.g. openai-node throws `Expected 'id' to be a string` when it reads
// the Anthropic `data: {}` frame as a chat-completion chunk).
export function eomTail(streamFormat = "anthropic") {
  return streamFormat === "openai" ? `\ndata: [DONE]\n\n` : `\n${SSE_EOM}\ndata: {}\n\n`;
}

// ── Pure functions ──

export function truncate(str, max = 500) {
  if (!str || str.length <= max) return str;
  return str.slice(0, max) + `... (${str.length - max} more bytes)`;
}

export function redactSensitive(value) {
  if (typeof value !== "string") return value;
  return value.replace(/sk[-_][A-Za-z0-9_-]+/g, "[redacted]");
}

export function summarizeRequest(rawBody, path, method = "POST") {
  const summary = { method, path, bodyBytes: rawBody.length, parseOk: false };
  try {
    const body = JSON.parse(rawBody.toString("utf8"));
    summary.parseOk = true;
    summary.model = typeof body.model === "string" ? body.model : null;
    summary.stream = body.stream === true;
    summary.maxTokens = typeof body.max_tokens === "number" ? body.max_tokens : null;
    summary.messageCount = Array.isArray(body.messages) ? body.messages.length : null;
  } catch {}
  return summary;
}

export function responseHasEmptyOutput(statusCode, body) {
  if (statusCode !== 200 || !body?.length) return false;
  try {
    const parsed = JSON.parse(body.toString("utf8"));
    if (Array.isArray(parsed.content)) {
      return parsed.content.every((part) => part?.type !== "text" || !part.text);
    }
    const content = parsed.choices?.[0]?.message?.content;
    return typeof content === "string" && content.length === 0;
  } catch {
    return false;
  }
}

export function filterHeaders(headers) {
  if (!headers) return {};
  const out = {};
  for (const [k, v] of Object.entries(headers)) {
    if (!HOP_BY_HOP.has(k.toLowerCase())) out[k] = v;
  }
  return out;
}

export function normalizeSetCookie(headers) {
  if (!headers || !headers["set-cookie"]) return headers;
  const v = headers["set-cookie"];
  headers["set-cookie"] = Array.isArray(v) ? v : [v];
  return headers;
}

export function rewritePath(path) {
  if (path === "/messages" || path.startsWith("/messages?"))
    return path.replace("/messages", "/v1/messages");
  if (path === "/v1/messages" || path.startsWith("/v1/messages?")) return path;
  if (path === "/v1/chat/completions" || path.startsWith("/v1/chat/completions?")) return path;
  return path;
}

// Proxy allowlist: only these API paths may be forwarded upstream. Everything
// else (including any undocumented path) is answered locally with a 404.
const PROXY_ROUTES = new Set(["/v1/messages", "/messages", "/v1/chat/completions"]);

export function isProxyRoute(rawPath) {
  const base = String(rawPath || "").split("?")[0];
  return PROXY_ROUTES.has(base);
}

// Constant-time comparison for the inbound proxy token so a naive timing
// attack cannot recover the secret by length-prefixed probing.
export function safeTokenEqual(a, b) {
  const x = Buffer.from(String(a ?? ""));
  const y = Buffer.from(String(b ?? ""));
  if (x.length !== y.length) return false;
  return timingSafeEqual(x, y);
}

export function respondJson(res, status, data) {
  res.writeHead(status, {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
  });
  res.end(JSON.stringify(data));
}

// WAF block-page markers (Alibaba-Cloud-style). `waf.js` is the static
// challenge script referenced by block pages; it catches pages that strip the
// alicdn/block_message markers. Only evaluated on 403/405 upstream bodies.
const WAF_BLOCK_MARKERS = ["alicdn", "block_message", "renderData", "waf.js"];

export function isWafBlock(statusCode, body) {
  if (statusCode !== 405 && statusCode !== 403) return false;
  const html = typeof body === "string" ? body : body.toString("utf8");
  return WAF_BLOCK_MARKERS.some((m) => html.includes(m));
}

export function isRetryable(statusCode, errorMessage, retryOn5xx = false) {
  if (typeof statusCode === "number" && statusCode >= 500 && statusCode <= 599) {
    return Boolean(retryOn5xx);
  }
  if (
    errorMessage &&
    typeof errorMessage === "string" &&
    (errorMessage.includes("socket hang up") ||
      errorMessage.includes("timeout") ||
      errorMessage.includes("ECONNRESET") ||
      errorMessage.includes("ETIMEDOUT") ||
      errorMessage.includes("ENETUNREACH"))
  ) {
    return true;
  }
  return false;
}

export function getRetryDelay(attempt, baseMs) {
  return baseMs * Math.pow(2, attempt);
}

// Adaptive response timeout — larger bodies need more upstream processing time.
export function getResponseTimeout(bodyBytes, defaultMs) {
  const mb = bodyBytes / (1024 * 1024);
  if (mb > 5) return 300000;     // 5min for >5MB
  if (mb > 2) return 180000;     // 3min for 2-5MB
  if (mb > 1) return 120000;     // 2min for 1-2MB
  if (mb > 0.5) return 90000;    // 90s for 500KB-1MB
  return defaultMs;              // default 30s for small payloads
}

export function injectPrompt(rawBody, path, prompt) {
  if (!prompt || !rawBody.length) return rawBody;
  try {
    const body = JSON.parse(rawBody.toString("utf8"));
    if (!body) return rawBody;
    if (path.startsWith("/v1/messages")) {
      if (typeof body.system === "string") {
        body.system = prompt + "\n\n" + body.system;
      } else if (Array.isArray(body.system)) {
        body.system.unshift({ type: "text", text: prompt });
      } else {
        body.system = [{ type: "text", text: prompt }];
      }
    }
    if (path.startsWith("/v1/chat/completions") && Array.isArray(body.messages)) {
      body.messages.unshift({ role: "system", content: prompt });
    }
    return Buffer.from(JSON.stringify(body), "utf8");
  } catch {
    return rawBody;
  }
}
