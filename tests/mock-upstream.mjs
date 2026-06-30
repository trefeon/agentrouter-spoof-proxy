import http from "node:http";
import { setTimeout as sleep } from "node:timers/promises";

const SSE_CHUNKS = [
  `event: message_start\ndata: {"type":"message_start","message":{"id":"msg_test","content":[],"model":"claude-opus-4-8","role":"assistant","stop_reason":null,"stop_sequence":null,"type":"message","usage":{"input_tokens":10,"output_tokens":1}}}\n\n`,
  `event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"block_type":"text","text":""}}\n\n`,
  `event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello from mock upstream"}}\n\n`,
  `event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}\n\n`,
  `event: message_stop\ndata: {}\n\n`,
];

export class MockUpstream {
  constructor() {
    this._scenario = "success";
    this.received = [];
    this._server = null;
    this._port = null;
    this.reqDestroyed = false;
    this._req = null;
  }

  setScenario(s) { this._scenario = s; }
  get port() { return this._port; }

  async start() {
    this._server = http.createServer((req, res) => {
      this.received.push({ method: req.method, url: req.url, headers: req.headers });

      const body = [];
      req.on("data", (c) => body.push(c));
      req.on("end", () => this._route(req, res, Buffer.concat(body)));
    });

    return new Promise((resolve) => {
      this._server.listen(0, "127.0.0.1", () => {
        this._port = this._server.address().port;
        resolve();
      });
    });
  }

  async close() {
    if (this._server) {
      await new Promise((r) => this._server.close(r));
      this._server = null;
    }
  }

  reset() {
    this.received = [];
    this._scenario = "success";
  }

  _route(req, res, body) {
    this._req = req;
    this.reqDestroyed = false;
    req.socket.on("close", () => {
      if (!res.writableEnded) this.reqDestroyed = true;
    });
    if (req.method === "GET") {
      return this._get(req, res, body);
    }
    if (req.url.startsWith("/v1/chat/completions") || req.url.startsWith("/v1/messages")) {
      return this._chat(req, res, body);
    }
    res.writeHead(404);
    res.end("not found");
  }

  _get(req, res, body) {
    res.writeHead(200, {
      "content-type": "text/html",
      "set-cookie": ["acw_tc=test_mock_cookie; Path=/; Secure"],
    });
    res.end("<html><body>mock ok</body></html>");
  }

  async _chat(req, res, body) {
    switch (this._scenario) {
      case "success":
        this._sse(res, () => { for (const c of SSE_CHUNKS) res.write(c); });
        break;

      case "success_streaming":
        this._sse(res, async () => {
          for (const c of SSE_CHUNKS) { res.write(c); await sleep(10); }
        });
        break;

      case "waf_405":
        res.writeHead(405, {
          "content-type": "text/html",
          connection: "close",
        });
        res.end(`<html><body><script src="//alicdn.com/waf.js"></script><p>block_message</p></body></html>`);
        break;

      case "non_waf_405":
        res.writeHead(405, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: { message: "method not allowed" } }));
        break;

      case "error_500":
        res.writeHead(500, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: { message: "internal error" } }));
        break;

      case "error_502":
        res.writeHead(502, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: { message: "bad gateway" } }));
        break;

      case "error_503":
        res.writeHead(503, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: { message: "NoChannelError" } }));
        break;

      case "timeout":
        await sleep(60000);
        break;

      case "partial_close":
        res.writeHead(200, {
          "content-type": "text/event-stream",
          "cache-control": "no-cache",
        });
        res.write(SSE_CHUNKS[0]);
        res.write(SSE_CHUNKS[1]);
        await sleep(1);
        req.socket.destroy();
        break;

      case "slow_stream":
        this._sse(res, async () => {
          for (const c of SSE_CHUNKS) { res.write(c); await sleep(50); }
        });
        break;

      case "hang":
        res.writeHead(200, {
          "content-type": "text/event-stream",
          "cache-control": "no-cache",
        });
        res.write(SSE_CHUNKS[0]);
        res.write(SSE_CHUNKS[1]);
        break;

      case "connection_error":
        req.socket.destroy();
        break;

      default:
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ ok: true }));
    }
  }

  _sse(res, writeFn) {
    res.writeHead(200, {
      "content-type": "text/event-stream",
      "cache-control": "no-cache",
      connection: "keep-alive",
    });
    const result = writeFn();
    if (result instanceof Promise) {
      result.then(() => { try { res.end(); } catch {} });
    } else {
      res.end();
    }
  }
}
