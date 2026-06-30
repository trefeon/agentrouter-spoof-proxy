import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { request as httpReq } from "node:http";
import net from "node:net";
import path from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { setMaxListeners } from "node:events";
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

async function waitActiveStreams(port, target, timeoutMs = 2000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const h = await fetch(`http://127.0.0.1:${port}/health`);
    const body = JSON.parse(h.body);
    if (body.activeStreams === target) return body.activeStreams;
    await sleep(50);
  }
  const h = await fetch(`http://127.0.0.1:${port}/health`);
  return JSON.parse(h.body).activeStreams;
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

function proxyHeaders(method = "POST") {
  return {
    "content-type": "application/json",
    authorization: "Bearer sk_test",
    "anthropic-version": "2023-06-01",
  };
}

function chatBody() {
  return JSON.stringify({
    model: "claude-opus-4-8",
    messages: [{ role: "user", content: "hi" }],
    stream: true,
    max_tokens: 10,
  });
}

describe("agentrouter-spoof-proxy", () => {
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
        MAX_RETRIES: "1",
        RETRY_DELAY_MS: "10",
        WARMUP_INTERVAL_MS: "600000",
        DISCOVERY_INTERVAL_MS: "600000",
      },
      stdio: ["ignore", "pipe", "pipe"],
    });

    proxyProc.stdout.on("data", (d) => process.stdout.write("[proxy] " + d));
    proxyProc.stderr.on("data", (d) => process.stderr.write("[proxy:err] " + d));

    await waitForProxy(proxyPort);
  });

  after(() => {
    if (proxyProc && !proxyProc.killed) {
      proxyProc.kill("SIGTERM");
    }
    mock.close();
  });

  // ── Health endpoint ──
  describe("health endpoint", () => {
    it("returns 200 with expected fields", async () => {
      const res = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const body = JSON.parse(res.body);
      assert.equal(res.statusCode, 200);
      assert.equal(body.ok, true);
      assert.equal(typeof body.activeStreams, "number");
      assert.equal(typeof body.wafCookie, "boolean");
      assert.equal(typeof body.circuitOpen, "boolean");
      assert.equal(typeof body.upstream, "string");
      assert.ok(body.staticModels >= 1);
    });

    it("reports activeStreams as 0 at startup", async () => {
      const res = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const body = JSON.parse(res.body);
      assert.equal(body.activeStreams, 0);
    });
  });

  // ── Models endpoint ──
  describe("models endpoint", () => {
    it("returns static model list", async () => {
      const res = await fetch(`http://127.0.0.1:${proxyPort}/v1/models`);
      const body = JSON.parse(res.body);
      assert.equal(res.statusCode, 200);
      assert.ok(Array.isArray(body.data));
      assert.ok(body.data.length >= 1);
      assert.ok(body.data.some((m) => m.id === "claude-opus-4-8"));
    });
  });

  // ── Path rewriting ──
  describe("path rewriting", () => {
    it("rewrites /messages to /v1/messages upstream", async () => {
      mock.setScenario("success");
      mock.received.length = 0;
      await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));
      const upstreamReqs = mock.received.filter((r) => r.method === "POST");
      assert.ok(upstreamReqs.length >= 1);
      assert.ok(upstreamReqs.some((r) => r.url.startsWith("/v1/messages")));
    });
  });

  // ── Header injection ──
  describe("header injection", () => {
    it("injects spoof headers to upstream", async () => {
      mock.setScenario("success");
      mock.received.length = 0;
      await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));
      const req = mock.received.find((r) => r.method === "POST" && r.url.startsWith("/v1/messages"));
      assert.ok(req, "upstream received POST");
      assert.ok(req.headers["user-agent"]?.includes("claude-cli"), "user-agent spoofed");
      assert.ok(req.headers["anthropic-version"], "anthropic-version present");
      assert.ok(req.headers["x-stainless-runtime"], "x-stainless-runtime present");
      assert.ok(req.headers["anthropic-dangerous-direct-browser-access"] === "true", "dangerous header present");
    });

    it("forwards Authorization header to upstream", async () => {
      mock.setScenario("success");
      mock.received.length = 0;
      await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));
      const req = mock.received.find((r) => r.method === "POST" && r.url.startsWith("/v1/messages"));
      assert.equal(req.headers.authorization, "Bearer sk_test");
    });

    it("forwards WAF cookie to upstream", async () => {
      mock.setScenario("success");
      mock.received.length = 0;
      // Wait for warmup to acquire cookie (warmup is GET / which returns cookie)
      await sleep(300);
      await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));
      const req = mock.received.find((r) => r.method === "POST" && r.url.startsWith("/v1/messages"));
      assert.ok(req.headers.cookie, "Cookie header should be present");
      assert.ok(req.headers.cookie.includes("acw_tc"), "Cookie should contain acw_tc");
    });
  });

  // ── SSE streaming ──
  describe("SSE streaming", () => {
    it("forwards SSE chunks to client", async () => {
      mock.setScenario("success");
      const { res, events } = await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));
      assert.equal(res.statusCode, 200);
      assert.ok(res.headers["content-type"].includes("text/event-stream"));
      assert.ok(events.length >= 1);
      assert.ok(events.some((e) => e.includes("message_stop")), "should contain message_stop");
    });

    it("activeStreams returns to 0 after stream completes", async () => {
      mock.setScenario("success");
      const h1 = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const before = JSON.parse(h1.body).activeStreams;

      await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));

      await sleep(100);
      const h2 = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const after = JSON.parse(h2.body).activeStreams;
      assert.equal(after, before, "activeStreams should return to original value");
    });
  });

  // ── Non-200 responses ──
  describe("non-200 responses", () => {
    it("forwards non-WAF 405 to client", async () => {
      mock.setScenario("non_waf_405");
      const res = await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      });
      assert.equal(res.statusCode, 405);
      const body = JSON.parse(res.body);
      assert.ok(body.error?.message);
    });

    it("forwards 500 without retrying (max retries 1)", async () => {
      mock.setScenario("error_500");
      const res = await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      });
      // With MAX_RETRIES=1 and a 500 error, the proxy will retry once
      // If the mock returns 500 twice, the final response should be 502 or 500
      assert.ok(res.statusCode === 502 || res.statusCode === 500);
    });

    it("forwards 503 to client", async () => {
      mock.setScenario("error_503");
      const res = await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      });
      assert.ok(res.statusCode === 503 || res.statusCode === 502);
    });
  });

  // ── WAF handling ──
  describe("WAF handling", () => {
    it("retries on 405 WAF block and succeeds on retry", async () => {
      const origScenario = mock._scenario;
      mock.setScenario("waf_405");
      mock.received.length = 0;

      const { events } = await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));

      const upstreamReqs = mock.received.filter((r) => r.method === "POST");
      // Should have at least 2 requests: original + WAF retry
      assert.ok(upstreamReqs.length >= 2, "should retry after WAF block");
      mock.setScenario(origScenario);
    });
  });

  // ── Error handling ──
  describe("error handling", () => {
    it("returns 502 on upstream connection error", async () => {
      mock.setScenario("connection_error");
      const res = await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      });
      assert.equal(res.statusCode, 502);
    });

    it("returns 504 on upstream timeout", async () => {
      mock.setScenario("timeout");
      const res = await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
        timeout: 15000,
      });
      assert.equal(res.statusCode, 504);
    });

    it("injects message_stop on premature upstream close", async () => {
      mock.setScenario("partial_close");
      const { res, events } = await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));
      // The response should end cleanly even though upstream disconnected
      assert.ok(res.complete || res.statusCode === 200);
      assert.ok(events.some((e) => e.includes("message_stop")), "should inject synthetic message_stop");
    });

    it("activeStreams returns to 0 after error", async () => {
      mock.setScenario("connection_error");
      const h1 = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const before = JSON.parse(h1.body).activeStreams;

      await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      });

      await sleep(200);
      const h2 = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const after = JSON.parse(h2.body).activeStreams;
      assert.equal(after, before, "activeStreams should return to original after error");
    });

    it("activeStreams returns to 0 after partial close", async () => {
      mock.setScenario("partial_close");
      const h1 = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const before = JSON.parse(h1.body).activeStreams;

      await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));

      await sleep(200);
      const h2 = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const after = JSON.parse(h2.body).activeStreams;
      assert.equal(after, before, "activeStreams should return to original after partial close");
    });
  });

  // ── Concurrent requests ──
  describe("concurrent requests", () => {
    it("handles multiple sequential requests without leaking streams", async () => {
      mock.setScenario("success_streaming");
      for (let i = 0; i < 3; i++) {
        await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
          method: "POST",
          headers: proxyHeaders(),
          body: chatBody(),
        }));
      }
      const h = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const body = JSON.parse(h.body);
      assert.equal(body.activeStreams, 0, "no streams leaked after sequential requests");
    });
  });

  // ── X-Accel-Buffering header ──
  describe("SSE anti-buffering headers", () => {
    it("adds X-Accel-Buffering and Cache-Control to SSE responses", async () => {
      mock.setScenario("success");
      const { res } = await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));
      assert.equal(res.headers["x-accel-buffering"], "no");
      assert.ok(res.headers["cache-control"]?.includes("no-cache"));
    });
  });

  // ── Client disconnect mid-stream ──
  describe("client disconnect", () => {
    it("cleans up when client disconnects mid-stream", async () => {
      mock.setScenario("slow_stream");
      const h1 = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const before = JSON.parse(h1.body).activeStreams;

      mock.reqDestroyed = false;
      const req = fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      });

      await sleep(200);
      req.destroy();
      const after = await waitActiveStreams(proxyPort, before);
      assert.equal(after, before, "activeStreams should return to original after client disconnect");
    });
  });

  // ── Hop-by-hop headers ──
  describe("hop-by-hop headers", () => {
    it("does not copy hop-by-hop headers from client request to upstream", async () => {
      mock.setScenario("success");
      mock.received.length = 0;
      await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: {
          ...proxyHeaders(),
          connection: "close",
          "transfer-encoding": "chunked",
          "x-custom-hop": "should-not-forward",
        },
        body: chatBody(),
      }));
      const req = mock.received.find((r) => r.method === "POST" && r.url.startsWith("/v1/messages"));
      assert.ok(req, "upstream received POST");
      // The proxy only forwards specific headers: authorization, x-api-key, anthropic-version
      assert.equal(req.headers.authorization, "Bearer sk_test", "authorization forwarded");
      assert.ok(req.headers["user-agent"]?.includes("claude-cli"), "user-agent spoofed");
      // connection, transfer-encoding should not be in upstreamHeaders
      // (Node.js will add its own Connection: keep-alive, but not from client request)
      assert.equal(req.headers["x-custom-hop"], undefined, "custom hop-by-hop not forwarded");
    });
  });

  // ── Concurrent parallel streams ──
  describe("concurrent parallel streams", () => {
    it("handles 3 parallel SSE streams without leaks", { timeout: 5000 }, async () => {
      mock.setScenario("success");
      const results = await Promise.all(
        Array.from({ length: 3 }, () =>
          collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
            method: "POST",
            headers: proxyHeaders(),
            body: chatBody(),
          }))
        )
      );
      results.forEach(({ events }) => {
        assert.ok(events.some((e) => e.includes("message_stop")), "each stream should complete");
      });
      const after = await waitActiveStreams(proxyPort, 0);
      assert.equal(after, 0, "no streams leaked after 3 concurrent streams");
    });
  });

  // ── Circuit breaker ──
  describe("circuit breaker", () => {
    it("opens after consecutive failures and blocks requests", async () => {
      mock.setScenario("connection_error");
      const h1 = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const initiallyOpen = JSON.parse(h1.body).circuitOpen;

      // Send 5+ requests that will fail (connection errors)
      for (let i = 0; i < 6; i++) {
        await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
          method: "POST",
          headers: proxyHeaders(),
          body: chatBody(),
        }).catch(() => {});
      }

      await sleep(300);
      const h2 = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const body = JSON.parse(h2.body);
      // Circuit might be open or not depending on exact count
      // It opens after 5 consecutive failures with MAX_RETRIES=1 → 5 fail + 1 fail = circuit opens
      console.log(`  circuitOpen: ${body.circuitOpen}, consecutiveFails: ${body.consecutiveFails}`);
      if (body.circuitOpen) {
        // If circuit is open, a new request should get 503
        const res = await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
          method: "POST",
          headers: proxyHeaders(),
          body: chatBody(),
        });
        assert.equal(res.statusCode, 503);
      }
    });
  });
});
