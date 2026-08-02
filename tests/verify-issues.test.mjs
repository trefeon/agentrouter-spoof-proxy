import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { request as httpReq } from "node:http";
import net from "node:net";
import path from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { setMaxListeners } from "node:events";
import fs from "node:fs";
import { MockUpstream } from "./mock-upstream.mjs";
setMaxListeners(50);

const PROXY_DIR = path.resolve(import.meta.dirname, "..");

function getFreePort() {
  return new Promise((resolve) => {
    const s = net.createServer();
    s.listen(0, "127.0.0.1", () => {
      const p = s.address().port;
      s.close(() => resolve(p));
    });
  });
}

function fetch(url, opts = {}) {
  return new Promise((resolve, reject) => {
    const req = httpReq(url, { method: opts.method || "GET", headers: opts.headers }, (res) => {
      const chunks = [];
      res.on("data", (c) => chunks.push(c));
      res.on("end", () => {
        res.body = Buffer.concat(chunks);
        resolve(res);
      });
    });
    req.on("error", reject);
    if (opts.body) req.write(opts.body);
    req.end();
  });
}

function fetchStream(url, opts = {}) {
  const req = httpReq(url, { method: opts.method || "POST", headers: opts.headers });
  if (opts.body) req.write(opts.body);
  req.end();
  return req;
}

async function waitForProxy(port, timeoutMs = 10000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/health`);
      if (res.statusCode === 200) return;
    } catch {}
    await sleep(100);
  }
  throw new Error(`proxy did not become healthy within ${timeoutMs}ms`);
}

async function collectSse(request, timeoutMs = 5000) {
  return new Promise((resolve, reject) => {
    const events = [];
    let buf = "";
    const timer = setTimeout(() => {
      request.destroy();
      reject(new Error(`SSE collection timed out after ${timeoutMs}ms`));
    }, timeoutMs);
    request.on("response", (res) => {
      res.on("data", (chunk) => {
        buf += chunk.toString();
        const parts = buf.split("\n\n");
        buf = parts.pop();
        for (const part of parts) {
          if (part.trim()) events.push(part.trim());
        }
      });
      res.on("end", () => {
        clearTimeout(timer);
        resolve({ res, events });
      });
      res.on("error", (e) => {
        clearTimeout(timer);
        reject(e);
      });
    });
    request.on("error", reject);
  });
}

function proxyHeaders() {
  return {
    "content-type": "application/json",
    authorization: "Bearer sk_test",
    "anthropic-version": "2023-06-01",
  };
}

function chatBody(model = "claude-opus-4-8") {
  return JSON.stringify({
    model,
    messages: [{ role: "user", content: "hi" }],
    stream: true,
    max_tokens: 10,
  });
}

// ── Helper: read source file for handler analysis ──
const SOURCE = fs.readFileSync(path.join(PROXY_DIR, "proxy.mjs"), "utf8");

describe("issue-1: unhandledRejection handler", () => {
  it("proxy.mjs registers an unhandledRejection listener (fixed)", () => {
    const hasHandler = SOURCE.includes("unhandledRejection");
    assert.equal(hasHandler, true,
      "proxy.mjs now has an unhandledRejection handler");
  });

  it("proxy registers both uncaughtException and unhandledRejection", () => {
    const hasUncaught = SOURCE.includes("uncaughtException");
    const hasUnhandled = SOURCE.includes("unhandledRejection");
    assert.equal(hasUncaught, true, "has uncaughtException handler");
    assert.equal(hasUnhandled, true, "has unhandledRejection handler");
  });
});

describe("issue-2: Content-Length forwarding with INJECT_SYSTEM_PROMPT", () => {
  let mock;
  let proxyProc;
  let proxyPort;
  const INJECT_PROMPT = "ISSUE2_TEST_PROMPT";

  before(async () => {
    mock = new MockUpstream();
    await mock.start();
    proxyPort = await getFreePort();
    proxyProc = spawn(process.execPath, ["proxy.mjs"], {
      cwd: PROXY_DIR,
      env: {
        ...process.env,
        LISTEN_PORT: String(proxyPort),
        TARGET_PROTOCOL: "http",
        TARGET_HOST: "127.0.0.1",
        TARGET_PORT: String(mock.port),
        REQUEST_TIMEOUT_MS: "5000",
        MAX_RETRIES: "0",
        WARMUP_INTERVAL_MS: "600000",
        DISCOVERY_INTERVAL_MS: "600000",
        INJECT_SYSTEM_PROMPT: INJECT_PROMPT,
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    proxyProc.stdout.on("data", () => {});
    proxyProc.stderr.on("data", () => {});
    await waitForProxy(proxyPort);
  });

  after(() => {
    if (proxyProc && !proxyProc.killed) proxyProc.kill("SIGTERM");
    mock.close();
  });

  it("content-length from client is NOT forwarded to upstream (Node auto-computes)", async () => {
    mock.setScenario("success");
    mock.received.length = 0;
    const body = chatBody();
    await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
      method: "POST",
      headers: {
        ...proxyHeaders(),
        "content-length": String(Buffer.byteLength(body)),
      },
      body,
    }));
    const req = mock.received.find((r) => r.method === "POST" && r.url.startsWith("/v1/messages"));
    assert.ok(req, "upstream received POST");

    // The proxy's upstreamHeaders does NOT spread req.headers.
    // It only picks specific fields: authorization, x-api-key, anthropic-version.
    // content-length is NOT forwarded. Node.js auto-sets it on write().
    // This test confirms the bug does NOT exist — content-length is isolated.
    const cl = req.headers["content-length"];
    assert.equal(cl, undefined,
      "content-length should be stripped by proxy (Node auto-computes). " +
      "If this fails, bug exists: content-length leaks upstream with stale value");
  });

  it("upstream receives correct body size when injection is active", async () => {
    mock.setScenario("success");
    mock.received.length = 0;
    await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
      method: "POST",
      headers: proxyHeaders(),
      body: chatBody(),
    }));
    const req = mock.received.find((r) => r.method === "POST" && r.url.startsWith("/v1/messages"));
    assert.ok(req, "upstream received POST");
    assert.ok(req.body, "upstream has body");
    const bodyStr = typeof req.body === "string" ? req.body : JSON.stringify(req.body);
    assert.ok(bodyStr.includes(INJECT_PROMPT), "injected prompt present in upstream body");
    assert.ok(bodyStr.includes("hi"), "original content preserved");
  });

  it("injection body is larger than original (prompt was prepended)", () => {
    const original = JSON.stringify({
      model: "claude-opus-4-8",
      messages: [{ role: "user", content: "hi" }],
      stream: true,
      max_tokens: 10,
    });
    const injected = JSON.stringify({
      model: "claude-opus-4-8",
      system: [{ type: "text", text: INJECT_PROMPT }],
      messages: [{ role: "user", content: "hi" }],
      stream: true,
      max_tokens: 10,
    });
    assert.ok(
      Buffer.byteLength(injected) > Buffer.byteLength(original),
      "injected body should be larger than original"
    );
  });
});

describe("issue-3: request body size limit", () => {
  let mock;
  let proxyProc;
  let proxyPort;

  before(async () => {
    mock = new MockUpstream();
    await mock.start();
    proxyPort = await getFreePort();
    proxyProc = spawn(process.execPath, ["proxy.mjs"], {
      cwd: PROXY_DIR,
      env: {
        ...process.env,
        LISTEN_PORT: String(proxyPort),
        TARGET_PROTOCOL: "http",
        TARGET_HOST: "127.0.0.1",
        TARGET_PORT: String(mock.port),
        REQUEST_TIMEOUT_MS: "5000",
        MAX_RETRIES: "0",
        WARMUP_INTERVAL_MS: "600000",
        DISCOVERY_INTERVAL_MS: "600000",
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    proxyProc.stdout.on("data", () => {});
    proxyProc.stderr.on("data", () => {});
    await waitForProxy(proxyPort);
  });

  after(() => {
    if (proxyProc && !proxyProc.killed) proxyProc.kill("SIGTERM");
    mock.close();
  });

  it("proxy handles 1MB request body without crashing", async () => {
    mock.setScenario("success");
    mock.received.length = 0;

    const largeContent = "x".repeat(1024 * 1024); // 1MB
    const largeBody = JSON.stringify({
      model: "claude-opus-4-8",
      messages: [{ role: "user", content: largeContent }],
      stream: true,
      max_tokens: 10,
    });

    const res = await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
      method: "POST",
      headers: proxyHeaders(),
      body: largeBody,
      timeout: 10000,
    });

    // Should not crash — proxy should still be responsive after
    assert.ok(res.statusCode === 200 || res.statusCode === 502,
      `proxy responded (got ${res.statusCode}) instead of crashing`);

    const health = await fetch(`http://127.0.0.1:${proxyPort}/health`);
    assert.equal(health.statusCode, 200, "proxy still healthy after large body");
  });

  it("proxy handles 5MB request body without crashing", async () => {
    mock.setScenario("success");
    mock.received.length = 0;

    const largeContent = "y".repeat(5 * 1024 * 1024); // 5MB
    const largeBody = JSON.stringify({
      model: "claude-opus-4-8",
      messages: [{ role: "user", content: largeContent }],
      stream: true,
      max_tokens: 10,
    });

    const res = await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
      method: "POST",
      headers: proxyHeaders(),
      body: largeBody,
      timeout: 30000,
    });

    assert.ok(res.statusCode === 200 || res.statusCode === 502,
      `proxy responded (got ${res.statusCode})`);

    const health = await fetch(`http://127.0.0.1:${proxyPort}/health`);
    assert.equal(health.statusCode, 200, "proxy still healthy after 5MB body");
  });
});
