import { describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { PassThrough } from "node:stream";
import { setTimeout as sleep } from "node:timers/promises";
import { pipeSse } from "../../src/proxy/stream.mjs";
import { SSE_EOM } from "../../src/utils.mjs";

const EOM_TAIL = `\n${SSE_EOM}\ndata: {}\n\n`;

function makeHarness(overrides = {}) {
  const upstreamRes = new PassThrough();
  const res = new PassThrough();
  const upstreamReq = { destroyed: false, destroy() { this.destroyed = true; } };
  const events = { results: [], degrades: [], finished: 0, messageStops: 0 };
  let body = "";
  res.on("data", (c) => { body += c.toString("utf8"); });
  pipeSse({
    upstreamRes, upstreamReq, res,
    isSse: true,
    chunkTimeoutMs: 5000,
    idleTimeoutMs: 5000,
    slowResponseMs: 100000,
    log: () => {}, logDebug: () => {},
    onResult: (r) => events.results.push(r),
    onDegrade: (d) => events.degrades.push(d),
    onFinish: () => events.finished++,
    onMessageStop: () => events.messageStops++,
    ...overrides,
  });
  return { upstreamRes, res, upstreamReq, events, get body() { return body; } };
}

describe("unit: pipeSse", () => {
  it("passes chunks through and records completion", async () => {
    const h = makeHarness();
    h.upstreamRes.write(`event: message_start\ndata: ${"y".repeat(200)}\n\n`);
    h.upstreamRes.end();
    await sleep(20);
    assert.equal(h.body.includes("event: message_start"), true);
    assert.deepStrictEqual(h.events.results, [{ statusCode: 200, durationMs: h.events.results[0].durationMs, chunks: 1, emptyOutput: false }]);
    assert.equal(h.events.finished, 1);
  });

  it("injects EOM on abrupt upstream close", async () => {
    const h = makeHarness();
    h.upstreamRes.write("event: message_start\ndata: {}\n\n");
    await sleep(10);
    h.upstreamRes.destroy();
    await sleep(20);
    assert.equal(h.body.endsWith(EOM_TAIL), true);
    assert.equal(h.events.results[0].statusCode, 502);
    assert.equal(h.events.results[0].error, "upstream_closed");
    assert.equal(h.events.finished, 1);
  });

  it("does not inject EOM after message_stop seen", async () => {
    const h = makeHarness();
    h.upstreamRes.write("event: message_start\ndata: {}\n\n");
    h.upstreamRes.write(`\n${SSE_EOM}\n`);
    await sleep(10);
    h.upstreamRes.destroy();
    await sleep(20);
    assert.equal(h.body.endsWith(EOM_TAIL), false);
    assert.equal(h.events.messageStops, 1);
    assert.equal(h.events.finished, 1);
  });

  it("idle timeout records 504 sse_idle_timeout, ends and destroys upstream", async () => {
    const h = makeHarness({ idleTimeoutMs: 150 });
    await sleep(300);
    assert.equal(h.events.results[0].statusCode, 504);
    assert.equal(h.events.results[0].error, "sse_idle_timeout");
    assert.deepStrictEqual(h.events.degrades, ["sse_idle_timeout"]);
    assert.equal(h.upstreamReq.destroyed, true);
    assert.equal(h.events.finished, 1);
  });

  it("chunk timeout after real data records 504 sse_chunk_timeout", async () => {
    const h = makeHarness({ chunkTimeoutMs: 150 });
    h.upstreamRes.write("x".repeat(100));
    await sleep(300);
    assert.equal(h.events.results[0].statusCode, 504);
    assert.equal(h.events.results[0].error, "sse_chunk_timeout");
    assert.deepStrictEqual(h.events.degrades, ["sse_chunk_timeout"]);
    assert.equal(h.upstreamReq.destroyed, true);
    assert.equal(h.body.endsWith(EOM_TAIL), true);
  });

  it("keepalive-only stream records 504 keepalive_only", async () => {
    const h = makeHarness({ chunkTimeoutMs: 150, idleTimeoutMs: 5000 });
    h.upstreamRes.write(":\n\n");
    h.upstreamRes.write(":\n\n");
    await sleep(350);
    assert.equal(h.events.results[0].statusCode, 504);
    assert.equal(h.events.results[0].error, "keepalive_only");
    assert.deepStrictEqual(h.events.degrades, ["keepalive_only"]);
  });

  it("empty SSE stream (pings only) degrades empty_sse on normal end", async () => {
    const h = makeHarness();
    h.upstreamRes.write(":\n\n");
    h.upstreamRes.write(":\n\n");
    h.upstreamRes.end();
    await sleep(20);
    assert.deepStrictEqual(h.events.degrades, ["empty_sse"]);
    assert.equal(h.events.results[0].emptyOutput, true);
    assert.equal(h.events.results[0].statusCode, 200);
  });

  it("marks slow successful stream", async () => {
    const h = makeHarness({ slowResponseMs: 50 });
    h.upstreamRes.write("x".repeat(100));
    await sleep(100);
    h.upstreamRes.end();
    await sleep(20);
    assert.equal(h.events.degrades[0].startsWith("slow_"), true);
    assert.equal(h.events.results[0].statusCode, 200);
  });

  it("pauses upstream on client backpressure and resumes on drain", async () => {
    const upstream = http.createServer((req, res) => {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write(Buffer.alloc(1024 * 1024, "a"));
    });
    await new Promise((r) => upstream.listen(0, "127.0.0.1", r));

    let captured = null;
    const proxy = http.createServer((req, res) => {
      const uReq = http.request(
        { host: "127.0.0.1", port: upstream.address().port, path: "/", method: "GET" },
        (uRes) => {
          captured = { uRes, uReq };
          pipeSse({
            upstreamRes: uRes, upstreamReq: uReq, res, isSse: true,
            chunkTimeoutMs: 5000, idleTimeoutMs: 5000, slowResponseMs: 100000,
            log: () => {}, logDebug: () => {},
          });
        }
      );
      uReq.end();
    });
    await new Promise((r) => proxy.listen(0, "127.0.0.1", r));

    const clientRes = await new Promise((resolve, reject) => {
      const req = http.get({ host: "127.0.0.1", port: proxy.address().port, path: "/" }, resolve);
      req.on("error", reject);
    });
    clientRes.pause();
    await sleep(150);
    assert.equal(captured.uRes.isPaused(), true);
    clientRes.resume();
    clientRes.on("data", () => {});
    await sleep(150);
    assert.equal(captured.uRes.isPaused(), false);

    clientRes.destroy();
    upstream.closeAllConnections();
    proxy.closeAllConnections();
    await new Promise((r) => upstream.close(r));
    await new Promise((r) => proxy.close(r));
  });

  it("non-SSE stream never injects EOM", async () => {
    const h = makeHarness({ isSse: false });
    h.upstreamRes.write("plain body");
    h.upstreamRes.destroy();
    await sleep(20);
    assert.equal(h.body.includes(SSE_EOM), false);
    assert.equal(h.events.finished, 1);
  });
});