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

// Graceful, awaited proxy shutdown: send SIGTERM, wait for the process to
// exit on its own (it drains and closes), and destroy any leftover handle.
// Used by every teardown so `npm test` exits without --test-force-exit.
async function stopProxy(proc, timeoutMs = 8000) {
  if (!proc || proc.exitCode !== null) return;
  const exited = new Promise((resolve) => proc.once("exit", resolve));
  proc.kill("SIGTERM");
  await Promise.race([
    exited,
    sleep(timeoutMs).then(() => {
      if (proc.exitCode === null) proc.kill("SIGKILL");
    }),
  ]);
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

  after(async () => {
    await stopProxy(proxyProc);
    await mock.close();
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
      assert.ok(Array.isArray(body.modelHealth));
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

  describe("model reliability telemetry", () => {
    it("records model success metrics without prompt content", async () => {
      mock.setScenario("success");
      await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: JSON.stringify({
          model: "claude-opus-4-8",
          messages: [{ role: "user", content: "super secret prompt text" }],
          stream: true,
          max_tokens: 10,
        }),
      }));

      const res = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const body = JSON.parse(res.body);
      const stat = body.modelHealth.find((m) => m.model === "claude-opus-4-8");
      assert.ok(stat, "model stat exists");
      assert.ok(stat.successes >= 1);
      assert.equal(JSON.stringify(stat).includes("super secret prompt text"), false);
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

      await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
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

  // ── Format-aware streaming (OpenAI vs Anthropic) ──
  describe("streaming format awareness", () => {
    it("Anthropic /v1/messages terminates with event: message_stop", async () => {
      mock.setScenario("success");
      const { res, events } = await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));
      assert.equal(res.statusCode, 200);
      assert.ok(events.some((e) => e.includes("event: message_stop")), "anthropic terminal present");
    });

    it("OpenAI /v1/chat/completions streams [DONE], never Anthropic event: message_stop", async () => {
      mock.setScenario("openai_stream");
      const { res, events } = await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/chat/completions`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody("gpt-5.6-sol"),
      }));
      assert.equal(res.statusCode, 200);
      const raw = events.join("\n");
      assert.ok(raw.includes("chatcmpl-9router-test"), "OpenAI chunk ids forwarded");
      assert.ok(raw.includes("data: [DONE]"), "OpenAI stream must end with [DONE]");
      assert.equal(raw.includes("event: message_stop"), false, "no Anthropic EOM may leak into OpenAI stream");
      assert.equal(raw.includes("data: {}"), false, "no empty {} chunk corruption");
    });

    it("Anthropic thinking blocks stream cleanly and are not empty/degraded", async () => {
      mock.setScenario("thinking_stream");
      const { res, events } = await collectSse(fetchStream(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));
      assert.equal(res.statusCode, 200);
      const joined = events.join("\n");
      assert.ok(joined.includes("thinking_delta"), "thinking delta forwarded");
      assert.ok(joined.includes("text_delta"), "text delta forwarded");
      assert.ok(joined.includes("event: message_stop"), "message_stop terminates");

      await sleep(100);
      const health = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const body = JSON.parse(health.body);
      const stat = body.modelHealth.find((m) => m.model === "claude-opus-4-8");
      if (stat) {
        assert.ok(stat.successes >= 1, "thinking stream counts as a success");
      }
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

  // ── Adaptive response timeout ──
  describe("adaptive response timeout", () => {
    let rtMock;
    let rtProxy;
    let rtPort;

    before(async () => {
      rtMock = new MockUpstream();
      await rtMock.start();
      rtPort = await getFreePort();
      rtProxy = spawn(process.execPath, ["proxy.mjs"], {
        cwd: PROXY_DIR,
        env: {
          ...process.env,
          LISTEN_PORT: String(rtPort),
          TARGET_PROTOCOL: "http",
          TARGET_HOST: "127.0.0.1",
          TARGET_PORT: String(rtMock.port),
          // Response timeout must fire well before the request timeout so the
          // test proves which of the two paths produced the 504.
          RESPONSE_TIMEOUT_MS: "600",
          REQUEST_TIMEOUT_MS: "30000",
          MAX_RETRIES: "0",
          RETRY_DELAY_MS: "10",
          WARMUP_INTERVAL_MS: "600000",
          DISCOVERY_INTERVAL_MS: "600000",
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      rtProxy.stdout.on("data", () => {});
      rtProxy.stderr.on("data", () => {});
      await waitForProxy(rtPort);
    });

    after(async () => {
      await stopProxy(rtProxy);
      await rtMock.close();
    });

    it("returns 504 when upstream withholds response headers", { timeout: 15000 }, async () => {
      rtMock.setScenario("no_response_headers");
      const started = Date.now();
      const res = await fetch(`http://127.0.0.1:${rtPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      });
      const elapsed = Date.now() - started;

      assert.equal(res.statusCode, 504, "response timeout must return 504");
      const body = JSON.parse(res.body);
      assert.equal(body.error.code, "timeout");
      // Proves the adaptive response timer fired, not the 30s request timer.
      assert.ok(elapsed < 10000, `should time out via RESPONSE_TIMEOUT_MS, took ${elapsed}ms`);
    });

    it("does not leak activeStreams after a response timeout", { timeout: 15000 }, async () => {
      rtMock.setScenario("no_response_headers");
      const h1 = await fetch(`http://127.0.0.1:${rtPort}/health`);
      const before = JSON.parse(h1.body).activeStreams;

      const res = await fetch(`http://127.0.0.1:${rtPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      });
      assert.equal(res.statusCode, 504);

      const after = await waitActiveStreams(rtPort, before);
      assert.equal(after, before, "activeStreams must return to baseline after response timeout");
    });

    it("retries a response timeout when MAX_RETRIES allows it", { timeout: 20000 }, async () => {
      const retryMock = new MockUpstream();
      await retryMock.start();
      const retryPort = await getFreePort();
      const retryProxy = spawn(process.execPath, ["proxy.mjs"], {
        cwd: PROXY_DIR,
        env: {
          ...process.env,
          LISTEN_PORT: String(retryPort),
          TARGET_PROTOCOL: "http",
          TARGET_HOST: "127.0.0.1",
          TARGET_PORT: String(retryMock.port),
          RESPONSE_TIMEOUT_MS: "500",
          REQUEST_TIMEOUT_MS: "30000",
          MAX_RETRIES: "1",
          RETRY_DELAY_MS: "10",
          WARMUP_INTERVAL_MS: "600000",
          DISCOVERY_INTERVAL_MS: "600000",
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      retryProxy.stdout.on("data", () => {});
      retryProxy.stderr.on("data", () => {});
      try {
        await waitForProxy(retryPort);
        retryMock.setScenario("no_response_headers");
        retryMock.received.length = 0;

        const res = await fetch(`http://127.0.0.1:${retryPort}/v1/messages`, {
          method: "POST",
          headers: proxyHeaders(),
          body: chatBody(),
        });

        assert.equal(res.statusCode, 504, "exhausted retries still return 504");
        const posts = retryMock.received.filter((r) => r.method === "POST");
        assert.ok(posts.length >= 2, `response timeout should retry, got ${posts.length} upstream attempts`);
      } finally {
        await stopProxy(retryProxy);
        await retryMock.close();
      }
    });
  });

  // ── Circuit breaker accounting ──
  describe("circuit breaker accounting", () => {
    let cbMock;
    let cbProxy;
    let cbPort;

    before(async () => {
      cbMock = new MockUpstream();
      await cbMock.start();
      cbPort = await getFreePort();
      cbProxy = spawn(process.execPath, ["proxy.mjs"], {
        cwd: PROXY_DIR,
        env: {
          ...process.env,
          LISTEN_PORT: String(cbPort),
          TARGET_PROTOCOL: "http",
          TARGET_HOST: "127.0.0.1",
          TARGET_PORT: String(cbMock.port),
          REQUEST_TIMEOUT_MS: "5000",
          // No retries: every upstream response is a *final* response, so the
          // breaker accounting under test is unambiguous.
          MAX_RETRIES: "0",
          RETRY_DELAY_MS: "10",
          WARMUP_INTERVAL_MS: "600000",
          DISCOVERY_INTERVAL_MS: "600000",
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      cbProxy.stdout.on("data", () => {});
      cbProxy.stderr.on("data", () => {});
      await waitForProxy(cbPort);
    });

    after(async () => {
      await stopProxy(cbProxy);
      await cbMock.close();
    });

    async function fails(port) {
      const h = await fetch(`http://127.0.0.1:${port}/health`);
      return JSON.parse(h.body).consecutiveFails;
    }

    async function send(port, model) {
      return fetch(`http://127.0.0.1:${port}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(model),
      });
    }

    it("counts a final 5xx response as a circuit failure", async () => {
      cbMock.setScenario("success");
      await send(cbPort, "cb-reset-5xx");
      const base = await fails(cbPort);
      assert.equal(base, 0, "success should zero the counter");

      cbMock.setScenario("error_500");
      const res = await send(cbPort, "cb-500");
      assert.equal(res.statusCode, 500, "final 5xx is forwarded to the client");
      assert.equal(await fails(cbPort), 1, "a final 5xx must increment consecutiveFails");
    });

    it("opens the circuit after 5 consecutive final 500 responses", async () => {
      cbMock.setScenario("success");
      await send(cbPort, "cb-open-reset");
      assert.equal(await fails(cbPort), 0);

      cbMock.setScenario("error_500");
      for (let i = 0; i < 5; i++) await send(cbPort, `cb-open-${i}`);

      const h = await fetch(`http://127.0.0.1:${cbPort}/health`);
      const body = JSON.parse(h.body);
      assert.ok(body.consecutiveFails >= 5, `expected >=5 fails, got ${body.consecutiveFails}`);
      assert.equal(body.circuitOpen, true, "5 consecutive final 5xx must open the circuit");

      const blocked = await send(cbPort, "cb-open-blocked");
      assert.equal(blocked.statusCode, 503, "open circuit returns 503");
    });

    it("a successful response resets consecutive failures", async () => {
      // Circuit may be open from the previous test — wait it out is too slow,
      // so use a dedicated proxy-free assertion via a fresh failure sequence.
      const freshMock = new MockUpstream();
      await freshMock.start();
      const freshPort = await getFreePort();
      const freshProxy = spawn(process.execPath, ["proxy.mjs"], {
        cwd: PROXY_DIR,
        env: {
          ...process.env,
          LISTEN_PORT: String(freshPort),
          TARGET_PROTOCOL: "http",
          TARGET_HOST: "127.0.0.1",
          TARGET_PORT: String(freshMock.port),
          REQUEST_TIMEOUT_MS: "5000",
          MAX_RETRIES: "0",
          RETRY_DELAY_MS: "10",
          WARMUP_INTERVAL_MS: "600000",
          DISCOVERY_INTERVAL_MS: "600000",
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      freshProxy.stdout.on("data", () => {});
      freshProxy.stderr.on("data", () => {});
      try {
        await waitForProxy(freshPort);

        freshMock.setScenario("error_500");
        await send(freshPort, "cb-reset-a");
        await send(freshPort, "cb-reset-b");
        assert.equal(await fails(freshPort), 2, "two 5xx accumulate");

        freshMock.setScenario("success");
        await collectSse(fetchStream(`http://127.0.0.1:${freshPort}/v1/messages`, {
          method: "POST",
          headers: proxyHeaders(),
          body: chatBody("cb-reset-ok"),
        }));
        assert.equal(await fails(freshPort), 0, "a success must reset the breaker");
      } finally {
        await stopProxy(freshProxy);
        await freshMock.close();
      }
    });

    it("does not count ordinary 4xx client errors as upstream outages", async () => {
      const c4Mock = new MockUpstream();
      await c4Mock.start();
      const c4Port = await getFreePort();
      const c4Proxy = spawn(process.execPath, ["proxy.mjs"], {
        cwd: PROXY_DIR,
        env: {
          ...process.env,
          LISTEN_PORT: String(c4Port),
          TARGET_PROTOCOL: "http",
          TARGET_HOST: "127.0.0.1",
          TARGET_PORT: String(c4Mock.port),
          REQUEST_TIMEOUT_MS: "5000",
          MAX_RETRIES: "0",
          WARMUP_INTERVAL_MS: "600000",
          DISCOVERY_INTERVAL_MS: "600000",
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      c4Proxy.stdout.on("data", () => {});
      c4Proxy.stderr.on("data", () => {});
      try {
        await waitForProxy(c4Port);
        c4Mock.setScenario("error_400");
        for (let i = 0; i < 6; i++) await send(c4Port, `cb-400-${i}`);

        const h = await fetch(`http://127.0.0.1:${c4Port}/health`);
        const body = JSON.parse(h.body);
        assert.equal(body.consecutiveFails, 0, "4xx must not increment the breaker");
        assert.equal(body.circuitOpen, false, "4xx must never open the circuit");
      } finally {
        await stopProxy(c4Proxy);
        await c4Mock.close();
      }
    });

    it("429 does not open the global circuit (upstream is reachable)", async () => {
      const rlMock = new MockUpstream();
      await rlMock.start();
      const rlPort = await getFreePort();
      const rlProxy = spawn(process.execPath, ["proxy.mjs"], {
        cwd: PROXY_DIR,
        env: {
          ...process.env,
          LISTEN_PORT: String(rlPort),
          TARGET_PROTOCOL: "http",
          TARGET_HOST: "127.0.0.1",
          TARGET_PORT: String(rlMock.port),
          REQUEST_TIMEOUT_MS: "5000",
          MAX_RETRIES: "0",
          WARMUP_INTERVAL_MS: "600000",
          DISCOVERY_INTERVAL_MS: "600000",
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      rlProxy.stdout.on("data", () => {});
      rlProxy.stderr.on("data", () => {});
      try {
        await waitForProxy(rlPort);
        rlMock.setScenario("error_429");
        for (let i = 0; i < 6; i++) await send(rlPort, `cb-429-${i}`);

        const h = await fetch(`http://127.0.0.1:${rlPort}/health`);
        const body = JSON.parse(h.body);
        // Policy: 429 is per-model rate limiting, not a provider outage. The
        // model is locked out via markModelExhausted so 9Router falls back,
        // but the global circuit stays closed for other models.
        assert.equal(body.circuitOpen, false, "429 must not open the global circuit");
        assert.equal(body.consecutiveFails, 0, "429 must not count as an outage failure");
      } finally {
        await stopProxy(rlProxy);
        await rlMock.close();
      }
    });
  });

  // ── Route allowlist ──
  describe("route allowlist", () => {
    let rlMock;
    let rlProxy;
    let rlPort;

    before(async () => {
      rlMock = new MockUpstream();
      await rlMock.start();
      rlPort = await getFreePort();
      rlProxy = spawn(process.execPath, ["proxy.mjs"], {
        cwd: PROXY_DIR,
        env: {
          ...process.env,
          LISTEN_PORT: String(rlPort),
          TARGET_PROTOCOL: "http",
          TARGET_HOST: "127.0.0.1",
          TARGET_PORT: String(rlMock.port),
          REQUEST_TIMEOUT_MS: "5000",
          MAX_RETRIES: "0",
          RETRY_DELAY_MS: "10",
          WARMUP_INTERVAL_MS: "600000",
          DISCOVERY_INTERVAL_MS: "600000",
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      rlProxy.stdout.on("data", () => {});
      rlProxy.stderr.on("data", () => {});
      await waitForProxy(rlPort);
    });

    after(async () => {
      await stopProxy(rlProxy);
      await rlMock.close();
    });

    it("returns 404 for unknown paths without reaching upstream", async () => {
      rlMock.setScenario("success");
      rlMock.received.length = 0;
      for (const p of ["/unknown", "/v1/unknown", "/healthz", "/", "/v1/models/extra"]) {
        const res = await fetch(`http://127.0.0.1:${rlPort}${p}`, {
          method: "POST",
          headers: proxyHeaders(),
          body: chatBody(),
        });
        assert.equal(res.statusCode, 404, `${p} should be rejected locally, got ${res.statusCode}`);
        const body = JSON.parse(res.body);
        assert.equal(body.error.code, "not_found");
      }
      await sleep(100);
      const posts = rlMock.received.filter((r) => r.method === "POST");
      assert.equal(posts.length, 0, "no unknown-path request may reach upstream");
    });

    it("rejects unsupported methods on proxy routes with 405", async () => {
      rlMock.setScenario("success");
      rlMock.received.length = 0;
      const res = await fetch(`http://127.0.0.1:${rlPort}/v1/messages`, {
        method: "GET",
        headers: proxyHeaders(),
      });
      assert.equal(res.statusCode, 405);
      await sleep(100);
      assert.equal(rlMock.received.filter((r) => r.method === "GET").length, 0);
    });

    it("preserves query strings on supported routes", async () => {
      rlMock.setScenario("success");
      rlMock.received.length = 0;
      await collectSse(fetchStream(`http://127.0.0.1:${rlPort}/v1/messages?beta=true`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));
      const req = rlMock.received.find((r) => r.method === "POST");
      assert.ok(req, "upstream received POST");
      assert.ok(req.url.startsWith("/v1/messages?beta=true"), `query preserved, got ${req.url}`);
    });
  });

  // ── Inbound proxy authentication ──
  describe("inbound proxy authentication", () => {
    let authMock;
    let authProxy;
    let authPort;
    const AUTH_TOKEN = "test-proxy-secret-token";

    before(async () => {
      authMock = new MockUpstream();
      await authMock.start();
      authPort = await getFreePort();
      authProxy = spawn(process.execPath, ["proxy.mjs"], {
        cwd: PROXY_DIR,
        env: {
          ...process.env,
          LISTEN_PORT: String(authPort),
          TARGET_PROTOCOL: "http",
          TARGET_HOST: "127.0.0.1",
          TARGET_PORT: String(authMock.port),
          REQUEST_TIMEOUT_MS: "5000",
          MAX_RETRIES: "0",
          RETRY_DELAY_MS: "10",
          WARMUP_INTERVAL_MS: "600000",
          DISCOVERY_INTERVAL_MS: "600000",
          PROXY_AUTH_TOKEN: AUTH_TOKEN,
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      authProxy.stdout.on("data", () => {});
      authProxy.stderr.on("data", () => {});
      await waitForProxy(authPort);
    });

    after(async () => {
      await stopProxy(authProxy);
      await authMock.close();
    });

    it("rejects proxy requests without credentials", async () => {
      authMock.setScenario("success");
      authMock.received.length = 0;
      const res = await fetch(`http://127.0.0.1:${authPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      });
      assert.equal(res.statusCode, 401);
      await sleep(100);
      assert.equal(authMock.received.filter((r) => r.method === "POST").length, 0,
        "no upstream work before auth");
    });

    it("rejects invalid credentials", async () => {
      const res = await fetch(`http://127.0.0.1:${authPort}/v1/messages`, {
        method: "POST",
        headers: { ...proxyHeaders(), "x-proxy-token": "wrong-token" },
        body: chatBody(),
      });
      assert.equal(res.statusCode, 401);
    });

    it("accepts valid Bearer token", async () => {
      authMock.setScenario("success");
      const { res } = await collectSse(fetchStream(`http://127.0.0.1:${authPort}/v1/messages`, {
        method: "POST",
        headers: { ...proxyHeaders(), authorization: `Bearer ${AUTH_TOKEN}` },
        body: chatBody(),
      }));
      assert.equal(res.statusCode, 200);
    });

    it("accepts valid X-Proxy-Token header", async () => {
      authMock.setScenario("success");
      const { res } = await collectSse(fetchStream(`http://127.0.0.1:${authPort}/v1/messages`, {
        method: "POST",
        headers: { ...proxyHeaders(), "x-proxy-token": AUTH_TOKEN },
        body: chatBody(),
      }));
      assert.equal(res.statusCode, 200);
    });

    it("health endpoint stays reachable without auth", async () => {
      const res = await fetch(`http://127.0.0.1:${authPort}/health`);
      assert.equal(res.statusCode, 200);
    });
  });

  // ── Oversized request bodies → 413 ──
  describe("oversized request bodies", () => {
    let bigMock;
    let bigProxy;
    let bigPort;

    before(async () => {
      bigMock = new MockUpstream();
      await bigMock.start();
      bigPort = await getFreePort();
      bigProxy = spawn(process.execPath, ["proxy.mjs"], {
        cwd: PROXY_DIR,
        env: {
          ...process.env,
          LISTEN_PORT: String(bigPort),
          TARGET_PROTOCOL: "http",
          TARGET_HOST: "127.0.0.1",
          TARGET_PORT: String(bigMock.port),
          REQUEST_TIMEOUT_MS: "5000",
          MAX_RETRIES: "0",
          RETRY_DELAY_MS: "10",
          WARMUP_INTERVAL_MS: "600000",
          DISCOVERY_INTERVAL_MS: "600000",
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      bigProxy.stdout.on("data", () => {});
      bigProxy.stderr.on("data", () => {});
      await waitForProxy(bigPort);
    });

    after(async () => {
      await stopProxy(bigProxy);
      await bigMock.close();
    });

    it("returns 413 for a Content-Length body over the limit", async () => {
      bigMock.setScenario("success");
      bigMock.received.length = 0;
      const big = JSON.stringify({
        model: "claude-opus-4-8",
        messages: [{ role: "user", content: "x".repeat(21 * 1024 * 1024) }],
      });
      const res = await fetch(`http://127.0.0.1:${bigPort}/v1/messages`, {
        method: "POST",
        headers: { ...proxyHeaders(), "content-length": String(Buffer.byteLength(big)) },
        body: big,
        timeout: 30000,
      });
      assert.equal(res.statusCode, 413, `expected 413, got ${res.statusCode}`);
      const body = JSON.parse(res.body);
      assert.equal(body.error.code, "payload_too_large");
      await sleep(150);
      assert.equal(bigMock.received.filter((r) => r.method === "POST").length, 0,
        "oversized body must never reach upstream");
    });

    it("returns 413 for a chunked body that exceeds the limit mid-upload", { timeout: 30000 }, async () => {
      bigMock.setScenario("success");
      bigMock.received.length = 0;

      const status = await new Promise((resolve, reject) => {
        const req = httpReq(
          { host: "127.0.0.1", port: bigPort, path: "/v1/messages", method: "POST", headers: proxyHeaders() },
          (res) => {
            const chunks = [];
            res.on("data", (c) => chunks.push(c));
            res.on("end", () => { res.body = Buffer.concat(chunks); resolve(res); });
          }
        );
        req.on("error", reject);
        const chunk = Buffer.alloc(2 * 1024 * 1024, "z");
        for (let i = 0; i < 12; i++) req.write(chunk);
        req.end();
      });

      assert.equal(status.statusCode, 413, `chunked oversized got ${status.statusCode}`);
      await sleep(150);
      assert.equal(bigMock.received.filter((r) => r.method === "POST").length, 0);
    });
  });

  // ── Bounded upload timeouts ──
  describe("bounded uploads", () => {
    let upMock;
    let upProxy;
    let upPort;

    before(async () => {
      upMock = new MockUpstream();
      await upMock.start();
      upPort = await getFreePort();
      upProxy = spawn(process.execPath, ["proxy.mjs"], {
        cwd: PROXY_DIR,
        env: {
          ...process.env,
          LISTEN_PORT: String(upPort),
          TARGET_PROTOCOL: "http",
          TARGET_HOST: "127.0.0.1",
          TARGET_PORT: String(upMock.port),
          REQUEST_TIMEOUT_MS: "5000",
          MAX_RETRIES: "0",
          RETRY_DELAY_MS: "10",
          WARMUP_INTERVAL_MS: "600000",
          DISCOVERY_INTERVAL_MS: "600000",
          BODY_UPLOAD_TIMEOUT_MS: "800",
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      upProxy.stdout.on("data", () => {});
      upProxy.stderr.on("data", () => {});
      await waitForProxy(upPort);
    });

    after(async () => {
      await stopProxy(upProxy);
      await upMock.close();
    });

    it("terminates a stalled upload with 408", { timeout: 15000 }, async () => {
      const result = await new Promise((resolve, reject) => {
        const req = httpReq(
          { host: "127.0.0.1", port: upPort, path: "/v1/messages", method: "POST", headers: proxyHeaders() },
          (res) => {
            const chunks = [];
            res.on("data", (c) => chunks.push(c));
            res.on("end", () => { res.body = Buffer.concat(chunks); resolve(res); });
          }
        );
        req.on("error", reject);
        req.write(Buffer.alloc(1024, "a"));
        // Intentionally never end the request body — the upload deadline fires.
      });
      assert.equal(result.statusCode, 408, `stalled upload got ${result.statusCode}`);
      const body = JSON.parse(result.body);
      assert.equal(body.error.code, "request_timeout");
    });
  });

  // ── Circuit breaker ──
  describe("circuit breaker", () => {
    it("opens after consecutive failures and blocks requests", async () => {
      mock.setScenario("connection_error");

      // Get current circuit state
      let h1;
      for (let retry = 0; retry < 5; retry++) {
        try {
          h1 = await fetch(`http://127.0.0.1:${proxyPort}/health`);
          JSON.parse(h1.body);
          break;
        } catch { await sleep(100); }
      }
      const priorFails = JSON.parse(h1.body).consecutiveFails;

      // Send enough failures to reach 5 consecutive
      const needed = Math.max(0, 6 - priorFails);
      for (let i = 0; i < needed; i++) {
        await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
          method: "POST",
          headers: proxyHeaders(),
          body: chatBody(),
        }).catch(() => {});
      }

      await sleep(300);
      const h2 = await fetch(`http://127.0.0.1:${proxyPort}/health`);
      const body = JSON.parse(h2.body);
      assert.equal(body.circuitOpen, true, "circuit should be open after 5+ failures");

      const res = await fetch(`http://127.0.0.1:${proxyPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      });
      assert.equal(res.statusCode, 503, "open circuit should return 503");
    });
  });

  // ── Prompt injection ──
  describe("prompt injection", () => {
    let injMock;
    let injProxy;
    let injPort;

    const INJECT_PROMPT = "TEST_INJECTION_SYSTEM_PROMPT";

    before(async () => {
      injMock = new MockUpstream();
      await injMock.start();
      injPort = await getFreePort();
      injProxy = spawn(process.execPath, ["proxy.mjs"], {
        cwd: PROXY_DIR,
        env: {
          ...process.env,
          LISTEN_PORT: String(injPort),
          TARGET_PROTOCOL: "http",
          TARGET_HOST: "127.0.0.1",
          TARGET_PORT: String(injMock.port),
          REQUEST_TIMEOUT_MS: "5000",
          MAX_RETRIES: "1",
          RETRY_DELAY_MS: "10",
          WARMUP_INTERVAL_MS: "600000",
          DISCOVERY_INTERVAL_MS: "600000",
          INJECT_SYSTEM_PROMPT: INJECT_PROMPT,
        },
        stdio: ["ignore", "pipe", "pipe"],
      });
      injProxy.stdout.on("data", () => {});
      injProxy.stderr.on("data", () => {});
      await waitForProxy(injPort);
    });

    after(async () => {
      await stopProxy(injProxy);
      await injMock.close();
    });

    it("injects system prompt for Anthropic /v1/messages format", async () => {
      injMock.received.length = 0;
      await collectSse(fetchStream(`http://127.0.0.1:${injPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));
      const req = injMock.received.find((r) => r.method === "POST" && r.url.startsWith("/v1/messages"));
      assert.ok(req, "upstream received POST");
      assert.ok(req.body, "upstream body should be captured");
      const sysOk =
        (typeof req.body.system === "string" && req.body.system.includes(INJECT_PROMPT)) ||
        (Array.isArray(req.body.system) && req.body.system.some((b) => b.text && b.text.includes(INJECT_PROMPT)));
      assert.ok(sysOk, `system field should contain injected prompt, got: ${JSON.stringify(req.body.system)}`);
    });

    it("injects system prompt for Anthropic /messages format (rewritten)", async () => {
      injMock.received.length = 0;
      await collectSse(fetchStream(`http://127.0.0.1:${injPort}/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: chatBody(),
      }));
      const req = injMock.received.find((r) => r.method === "POST" && r.url.startsWith("/v1/messages"));
      assert.ok(req, "upstream received POST");
      const sysOk =
        (typeof req.body.system === "string" && req.body.system.includes(INJECT_PROMPT)) ||
        (Array.isArray(req.body.system) && req.body.system.some((b) => b.text && b.text.includes(INJECT_PROMPT)));
      assert.ok(sysOk, `system should contain injected prompt for rewritten /messages, got: ${JSON.stringify(req.body.system)}`);
    });

    it("injects system message for OpenAI /v1/chat/completions format", async () => {
      injMock.received.length = 0;
      const openaiBody = JSON.stringify({
        model: "claude-opus-4-8",
        messages: [{ role: "user", content: "hello" }],
        stream: true,
        max_tokens: 10,
      });
      await collectSse(fetchStream(`http://127.0.0.1:${injPort}/v1/chat/completions`, {
        method: "POST",
        headers: { ...proxyHeaders(), "content-type": "application/json" },
        body: openaiBody,
      }));
      const req = injMock.received.find((r) => r.method === "POST" && r.url.startsWith("/v1/chat/completions"));
      assert.ok(req, "upstream received POST");
      assert.ok(Array.isArray(req.body.messages), "messages should be an array");
      assert.equal(req.body.messages[0].role, "system", "first message should be system role");
      assert.ok(
        req.body.messages[0].content.includes(INJECT_PROMPT),
        "first message should contain injected prompt"
      );
    });

    it("appends to existing Anthropic system field", async () => {
      injMock.received.length = 0;
      const bodyWithSystem = JSON.stringify({
        model: "claude-opus-4-8",
        messages: [{ role: "user", content: "hi" }],
        system: "original system prompt",
        stream: true,
        max_tokens: 10,
      });
      await collectSse(fetchStream(`http://127.0.0.1:${injPort}/v1/messages`, {
        method: "POST",
        headers: proxyHeaders(),
        body: bodyWithSystem,
      }));
      const req = injMock.received.find((r) => r.method === "POST" && r.url.startsWith("/v1/messages"));
      assert.ok(req, "upstream received POST");
      assert.ok(typeof req.body.system === "string", "system should be string");
      assert.ok(req.body.system.startsWith(INJECT_PROMPT), "injected prompt should be prepended");
      assert.ok(req.body.system.includes("original system prompt"), "original system should be preserved");
    });
  });
});
