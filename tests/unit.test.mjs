import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";

const HOP_BY_HOP = new Set([
  "transfer-encoding", "connection", "keep-alive",
  "proxy-authenticate", "proxy-authorization",
  "te", "trailer", "upgrade",
]);

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

function injectPrompt(rawBody, path, prompt) {
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

function truncate(str, max = 500) {
  if (!str || str.length <= max) return str;
  return str.slice(0, max) + `... (${str.length - max} more bytes)`;
}

// ════════════════ TESTS ════════════════

describe("unit: rewritePath", () => {
  it("/messages -> /v1/messages", () => {
    assert.equal(rewritePath("/messages"), "/v1/messages");
  });
  it("/messages?foo=bar -> /v1/messages?foo=bar", () => {
    assert.equal(rewritePath("/messages?foo=bar"), "/v1/messages?foo=bar");
  });
  it("/v1/messages unchanged", () => {
    assert.equal(rewritePath("/v1/messages"), "/v1/messages");
  });
  it("/v1/chat/completions unchanged", () => {
    assert.equal(rewritePath("/v1/chat/completions"), "/v1/chat/completions");
  });
  it("/health unchanged", () => {
    assert.equal(rewritePath("/health"), "/health");
  });
  it("/v1/models unchanged", () => {
    assert.equal(rewritePath("/v1/models"), "/v1/models");
  });
});

describe("unit: filterHeaders", () => {
  it("strips hop-by-hop headers", () => {
    const input = { "content-type": "text/html", connection: "keep-alive", "transfer-encoding": "chunked" };
    const out = filterHeaders(input);
    assert.equal(out["content-type"], "text/html");
    assert.equal(out["connection"], undefined);
    assert.equal(out["transfer-encoding"], undefined);
  });
  it("preserves normal headers", () => {
    const input = { authorization: "Bearer x", "x-custom": "val" };
    const out = filterHeaders(input);
    assert.equal(out["authorization"], "Bearer x");
    assert.equal(out["x-custom"], "val");
  });
  it("case insensitive for hop-by-hop", () => {
    const input = { Connection: "close", "Proxy-Authorization": "x" };
    const out = filterHeaders(input);
    assert.equal(out["Connection"], undefined);
    assert.equal(out["Proxy-Authorization"], undefined);
  });
  it("handles null input", () => {
    assert.deepStrictEqual(filterHeaders(null), {});
  });
});

describe("unit: isWafBlock", () => {
  it("false for 200", () => {
    assert.equal(isWafBlock(200, ""), false);
  });
  it("true for 403 with alicdn", () => {
    assert.equal(isWafBlock(403, '<script src="//alicdn.com/waf.js"></script>'), true);
  });
  it("true for 405 with block_message", () => {
    assert.equal(isWafBlock(405, '<p>block_message</p>'), true);
  });
  it("true for 403 with renderData", () => {
    assert.equal(isWafBlock(403, 'renderData("x")'), true);
  });
  it("false for 405 without WAF markers", () => {
    assert.equal(isWafBlock(405, '{"error":"not found"}'), false);
  });
  it("works with buffer body", () => {
    assert.equal(isWafBlock(405, Buffer.from('<script src="//alicdn.com/waf.js"></script>')), true);
  });
});

describe("unit: isRetryable", () => {
  it("500 is retryable", () => assert.equal(isRetryable(500, null), true));
  it("503 is retryable", () => assert.equal(isRetryable(503, null), true));
  it("200 is NOT retryable", () => assert.equal(isRetryable(200, null), false));
  it("403 is NOT retryable", () => assert.equal(isRetryable(403, null), false));
  it("socket hang up is retryable", () => assert.equal(isRetryable(null, "socket hang up"), true));
  it("timeout is retryable", () => assert.equal(isRetryable(null, "timeout"), true));
  it("ECONNRESET is retryable", () => assert.equal(isRetryable(null, "ECONNRESET"), true));
  it("ETIMEDOUT is retryable", () => assert.equal(isRetryable(null, "ETIMEDOUT"), true));
  it("no status no message is retryable", () => assert.equal(isRetryable(null, null), true));
  it("known error with no status is retryable", () => assert.equal(isRetryable(null, "ENETUNREACH"), true));
});

describe("unit: extractWafCookies", () => {
  it("extracts acw_tc cookie", () => {
    const res = { headers: { "set-cookie": ["acw_tc=abc123; Path=/"] } };
    const cookies = extractWafCookies(res);
    assert.ok(cookies.includes("acw_tc=abc123"));
  });
  it("extracts acw_sc__v2 cookie", () => {
    const res = { headers: { "set-cookie": ["acw_sc__v2=xyz; Secure"] } };
    const cookies = extractWafCookies(res);
    assert.ok(cookies.includes("acw_sc__v2=xyz"));
  });
  it("extracts cdn_sec_tc cookie", () => {
    const res = { headers: { "set-cookie": ["cdn_sec_tc=789; Path=/"] } };
    const cookies = extractWafCookies(res);
    assert.ok(cookies.includes("cdn_sec_tc=789"));
  });
  it("ignores non-WAF cookies", () => {
    const res = { headers: { "set-cookie": ["session=abc; Path=/", "acw_tc=xyz"] } };
    const cookies = extractWafCookies(res);
    assert.equal(cookies.length, 1);
    assert.equal(cookies[0], "acw_tc=xyz");
  });
  it("handles missing set-cookie header", () => {
    const res = { headers: {} };
    assert.deepStrictEqual(extractWafCookies(res), []);
  });
});

describe("unit: injectPrompt", () => {
  const PROMPT = "INJECTED_SYSTEM_PROMPT_TEST";

  it("/v1/messages — injects into string system", () => {
    const body = JSON.stringify({ model: "x", system: "hello", messages: [{ role: "user", content: "hi" }] });
    const result = injectPrompt(Buffer.from(body), "/v1/messages", PROMPT);
    const parsed = JSON.parse(result.toString());
    assert.ok(parsed.system.startsWith(PROMPT), "prompt prepended to string system");
    assert.ok(parsed.system.includes("hello"), "original system preserved");
  });

  it("/v1/messages — injects into array system", () => {
    const body = JSON.stringify({ model: "x", system: [{ type: "text", text: "orig" }], messages: [{ role: "user", content: "hi" }] });
    const result = injectPrompt(Buffer.from(body), "/v1/messages", PROMPT);
    const parsed = JSON.parse(result.toString());
    assert.equal(parsed.system[0].text, PROMPT);
    assert.equal(parsed.system[1].text, "orig");
  });

  it("/v1/messages — injects when no system field", () => {
    const body = JSON.stringify({ model: "x", messages: [{ role: "user", content: "hi" }] });
    const result = injectPrompt(Buffer.from(body), "/v1/messages", PROMPT);
    const parsed = JSON.parse(result.toString());
    assert.ok(Array.isArray(parsed.system));
    assert.equal(parsed.system[0].text, PROMPT);
  });

  it("/v1/messages — returns original if prompt is empty string", () => {
    const body = JSON.stringify({ model: "x", system: "hello", messages: [{ role: "user", content: "hi" }] });
    const result = injectPrompt(Buffer.from(body), "/v1/messages", "");
    assert.equal(result.toString(), body);
  });

  it("/v1/chat/completions — prepends system message", () => {
    const body = JSON.stringify({ model: "x", messages: [{ role: "user", content: "hi" }] });
    const result = injectPrompt(Buffer.from(body), "/v1/chat/completions", PROMPT);
    const parsed = JSON.parse(result.toString());
    assert.equal(parsed.messages[0].role, "system");
    assert.equal(parsed.messages[0].content, PROMPT);
    assert.equal(parsed.messages[1].role, "user");
  });

  it("/v1/chat/completions — returns original if prompt is empty", () => {
    const body = JSON.stringify({ model: "x", messages: [{ role: "user", content: "hi" }] });
    const result = injectPrompt(Buffer.from(body), "/v1/chat/completions", "");
    assert.equal(result.toString(), body);
  });

  it("returns original on invalid JSON", () => {
    const raw = Buffer.from("not json");
    const result = injectPrompt(raw, "/v1/messages", PROMPT);
    assert.ok(result.equals(raw));
  });

  it("injected body is larger than original", () => {
    const body = Buffer.from(JSON.stringify({ model: "x", messages: [{ role: "user", content: "hi" }] }));
    const result = injectPrompt(body, "/v1/messages", PROMPT);
    assert.ok(result.length > body.length);
  });
});

describe("unit: truncate", () => {
  it("returns short strings unchanged", () => {
    assert.equal(truncate("hello", 10), "hello");
  });
  it("truncates long strings with byte count", () => {
    const long = "x".repeat(1000);
    const result = truncate(long, 500);
    assert.equal(result.length, 500 + `... (500 more bytes)`.length);
    assert.ok(result.startsWith("x".repeat(500)));
    assert.ok(result.includes("500 more bytes"));
  });
  it("handles null", () => {
    assert.equal(truncate(null, 100), null);
  });
  it("handles empty", () => {
    assert.equal(truncate("", 10), "");
  });
});
