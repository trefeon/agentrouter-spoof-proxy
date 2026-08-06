# AgentRouter Spoof Proxy

Lightweight Node.js reverse proxy that bypasses AgentRouter WAF by spoofing Claude Code headers. Zero runtime dependencies — thin modular entry + 13 focused modules, 120MB Docker image.

> 🇮🇩 **[Panduan 9Router (Bahasa Indonesia)](docs/panduan-9router.md)** — Indonesian tutorial for integrating with 9Router.

---

## Quick Install

### Linux — One command

```bash
curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash
```

Non-interactive Docker install:

```bash
curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash -s -- --yes --docker
```

Dry-run without changing the system:

```bash
curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash -s -- --dry-run --docker
```

### Windows — One command (PowerShell as Admin)

```powershell
iwr -useb https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.ps1 | iex
```

Both scripts auto-detect your setup and guide you through Docker, PM2, or direct Node.js. Linux can install missing dependencies through supported package managers when you pass `--yes`; Windows can attempt Node.js install through `winget` and opens Docker Desktop installation if needed.

---

## Verify

```bash
curl http://localhost:8318/health
```

```json
{"ok":true,"upstream":"agentrouter.org:443","availableModels":3,"activeStreams":0,"wafCookie":true,"circuitOpen":false}
```

Wait 5 seconds if `wafCookie: false` — WAF warmup runs at startup.

---

## Connect to 9Router

1. Dashboard → Providers → **Add Provider** → **Add OpenAI Compatible**
2. Fill in:
   - **Name:** `AgentRouter`
   - **Prefix:** `AG`
   - **API Type:** `chat completions`
   - **Base URL:** `http://localhost:8318/v1` (or `http://172.18.0.3:8318/v1` if Docker-to-Docker)
3. Click **Import from /models**
4. **Add API Key** → paste your AgentRouter API key (store it only in 9Router, not in the proxy)
5. Model will appear as `AG-gpt-5.6-sol`, `AG-claude-opus-5`, etc.

> Windows Docker Desktop: use `http://host.docker.internal:8318/v1`

---

## Features

| Feature | How |
|---------|-----|
| **WAF bypass** | Spoofs Claude Code CLI headers + maintains `acw_tc` cookies |
| **SSE streaming** | Pipes with backpressure + keepalives; format-aware EOM; never cuts live streams |
| **Retry logic** | Auto-retry on transport errors with exponential backoff; 5xx retry configurable (`RETRY_ON_5XX`) |
| **Thinking tag stripping** | Strips `<think>...</think>` tags from Claude responses (`STRIP_THINKING_TAGS`) |
| **Model-aware headers** | Anthropic headers for `/v1/messages`, generic headers for `/v1/chat/completions` |
| **Circuit breaker** | Opens after 5 consecutive final 5xx/transport failures, progressive cooldown |
| **Auto model health** | Failing models removed from `/v1/models` → 9Router falls back instantly |
| **Model recovery** | Background probe every 60s with spoof headers + WAF cookie |
| **Prompt injection** | Optional system prompt injection (`INJECT_SYSTEM_PROMPT`) |
| **Model discovery** | Optional dynamic model list via `AR_API_KEY` |
| **Bounded bodies** | 20MB limit → clean `413`; stalled uploads → `408` |
| **Narrow proxy surface** | Only 3 API routes proxied; localhost bind by default; optional token auth |
| **Graceful shutdown** | Drains active streams, destroys agent pool |

---

## Endpoints

| Path | Method | Description |
|------|--------|-------------|
| `/health`, `/api/health` | GET | Status, WAF cookie, circuit breaker, streams (no auth required) |
| `/v1/models`, `/models` | GET | Available models (unhealthy ones auto-filtered) |
| `/v1/messages` | POST | Anthropic Messages API → proxied |
| `/messages` | POST | Auto-rewritten to `/v1/messages` |
| `/v1/chat/completions` | POST | OpenAI Chat Completions → proxied |

Only the three proxy routes above are ever forwarded upstream. Unknown paths
return a local `404`, unsupported methods return a local `405`, and — when
`PROXY_AUTH_TOKEN` is set — missing/invalid credentials return `401` before any
upstream work. SSE terminal events are format-aware: Anthropic streams end with
`event: message_stop`, OpenAI `chat.completions` streams end with `data: [DONE]`,
and long-lived streams are never cut while the upstream connection is alive.

---

## Configuration (`.env`)

All values have defaults — copy `.env.example` to `.env` only if you need to change something.

| Variable | Default | What it does |
|----------|---------|-------------|
| `LISTEN_PORT` | `8318` | Proxy port |
| `LISTEN_ADDRESS` | `127.0.0.1` | Bind address. Set `0.0.0.0` only for Docker-to-Docker/remote (with `PROXY_AUTH_TOKEN`) |
| `PROXY_AUTH_TOKEN` | _(empty)_ | Optional inbound auth — required header `Authorization: Bearer` or `X-Proxy-Token` |
| `TARGET_HOST` | `agentrouter.org` | Upstream host |
| `REQUEST_TIMEOUT_MS` | `300000` | Request timeout (5min) |
| `RESPONSE_TIMEOUT_MS` | `30000` | Wait for upstream response headers before 504/retry |
| `SSE_IDLE_TIMEOUT_MS` | `600000` | Terminate stream after no SSE events (dead upstream) |
| `SSE_CHUNK_TIMEOUT_MS` | `30000` | Stall watchdog — reports slow streams, keeps alive while connected |
| `BODY_UPLOAD_TIMEOUT_MS` | `60000` | Reject stalled uploads with 408 |
| `MAX_RETRIES` | `2` | Retry count for transport errors |
| `RETRY_ON_5XX` | `false` | Also retry on 5xx responses (⚠️ causes double token billing) |
| `STRIP_THINKING_TAGS` | `true` | Strip `<think>...</think>` from SSE text content |
| `AR_API_KEY` | _(empty)_ | Enable dynamic model discovery |
| `INJECT_SYSTEM_PROMPT` | _(empty)_ | System prompt injected into requests |
| `SLOW_RESPONSE_MS` | `30000` | Temporarily degrades models with slow successful streams |
| `LOG_LEVEL` | `info` | `info` or `debug` |

See `.env.example` for the full list.

### Retry behavior and token billing

By default (`RETRY_ON_5XX=false`), the proxy only retries on **transport-level errors**
(timeout, ECONNRESET, socket hang up, ETIMEDOUT). HTTP 5xx responses from the upstream
are forwarded to the client immediately.

When `RETRY_ON_5XX=true`, the proxy also retries on 5xx responses. **Warning:** each
retry re-sends the full request body to the upstream, which means the upstream counts
tokens for every attempt. If a request is retried once, 9Router will show ~2× the input
tokens. With `MAX_RETRIES=2`, up to 3× is possible.

WAF cookie retries (403/405) always occur regardless of `RETRY_ON_5XX`.

### Thinking tag stripping

The proxy spoofs the `interleaved-thinking-2025-05-14` Anthropic beta header, which
causes Claude to return thinking content blocks. When downstream clients don't understand
these blocks, thinking content leaks as raw `<think>...</think>` tags in text output.

With `STRIP_THINKING_TAGS=true` (default), the proxy strips `<think>...</think>` tags
from SSE response text before forwarding to the client. Tags that span multiple SSE
chunks are handled correctly. Set to `false` if your client supports thinking content.

### Model-aware headers

The proxy applies different spoof headers based on the request format:
- **`/v1/messages`** (Anthropic): full Claude Code headers including `Anthropic-Beta`,
  `Anthropic-Version`, etc.
- **`/v1/chat/completions`** (OpenAI): generic headers only (User-Agent, Content-Type,
  Authorization) — no Anthropic-specific headers are sent

The proxy owns the canonical `Anthropic-Version`/`Anthropic-Beta` spoof values — a
client-supplied `anthropic-version` on `/v1/messages` is intentionally ignored so the
spoofed identity can't be distorted. WAF cookies are also refreshed from API responses,
not just the warmup loop, so rotated session cookies are picked up immediately.

---

## Models

| Model | Context | Max output | Input/Output per MTok | Provider |
|-------|---------|------------|----------------------|----------|
| `gpt-5.6-sol` | 1.05M | 128K | $5 / $30 | [OpenAI](https://developers.openai.com/api/docs/models/gpt-5.6-sol) |
| `claude-opus-5` | 1M | 128K | $5 / $25 | [Anthropic](https://docs.anthropic.com/en/docs/about-claude/models) |
| `claude-opus-4-8` | 1M | 128K | $5 / $25 | [Anthropic](https://docs.anthropic.com/en/docs/about-claude/models) |

---

## Architecture

```
Client → 9Router → agentrouter-proxy:8318 → agentrouter.org (upstream)
                        ├── Spoof headers (Claude Code CLI)
                        ├── WAF cookie management
                        ├── Model health monitoring
                        └── SSE streaming + backpressure
```

```
proxy.mjs (~166 lines, thin entry: routing allowlist + inbound auth + lifecycle)
├── src/config.mjs                     — env + constants + agent pool
├── src/logger.mjs                     — logging
├── src/utils.mjs                      — pure functions (path, headers, retry, adaptive timeout)
├── src/errors.mjs                     — buildError + isOurError marker
├── src/status-code.mjs                — error code → HTTP status mapping
├── src/auth/spoof.mjs                 — Claude Code header spoofing
├── src/auth/waf.mjs                   — WAF cookie warmup
├── src/models/discovery.mjs           — static/dynamic model discovery
├── src/models/health.mjs              — auto-detect failing models, recovery probe
├── src/models/stats.mjs               — model success metrics
├── src/resilience/circuit-breaker.mjs — circuit breaker state
├── src/proxy/handler.mjs              — request handler (buffering, telemetry, retry loop)
└── src/proxy/stream.mjs               — SSE streaming pump (keepalive, timeouts, backpressure)
```

---

## Running Tests

> Runtime is zero-dependency. The scripts below use `oxlint` + the built-in `node:test` runner; run `npm install` once (devDependencies only) to use them.

```bash
# Everything (121 unit + 55 E2E + 7 issue-verification = 183 tests, exits cleanly)
npm test

# Fast unit tests — pure functions + SSE pump, no long-lived processes (121 tests)
npm run test:unit

# E2E tests — spawns proxy + mock upstream (55 tests, ~75s)
npm run test:e2e

# Issue-verification regression tests (7 tests)
npm run test:verify

# Lint + syntax gate
npm run lint
npm run check

# Zero-dep coverage (node built-in) — config, resilience, stats, stream, utils, models
npm run coverage
```

## Local Debugging

| Symptom | Command |
|---------|---------|
| Hang / who holds the socket | `NODE_DEBUG=http,net,stream,tls node proxy.mjs` |
| Stack of warnings (listener leaks) | `node --trace-warnings --stack-trace-limit=50 proxy.mjs` |
| Verbose proxy logs | `LOG_LEVEL=debug node proxy.mjs` |
| Attach debugger | `node --inspect proxy.mjs` → Chrome `chrome://inspect` |
| Diagnostic report on demand | `node --report-on-signal proxy.mjs` then `kill -USR2` → JSON report |
| Stuck shutdown (diagnostic only) | `node --test --test-force-exit tests/proxy.test.mjs` |

Zero runtime dependencies — everything above is built-in Node (or `oxlint` as dev-only).

---

## FAQ

**Q: Why do Claude models sometimes return 500?**
A: Not a proxy bug. The agentrouter.org upstream occasionally Go-panics for Claude models. The proxy has **auto model health** — a failing model is removed from `/v1/models` immediately and 9Router falls back to another model automatically. Recovery probe every 60 seconds. Progressive cooldown: 30s → 1m → 2m → 5m → 10m.

**Q: Streaming keeps disconnecting / getting cut mid-answer?**
A: Since the hardening, the proxy never cuts a stream that is still alive — as long as the upstream is connected, the stall watchdog only resets (safe for long thinking / tool use). A stream only ends when: the upstream finishes, an error occurs, the client disconnects, or the stream is truly idle (no events at all) beyond `SSE_IDLE_TIMEOUT_MS` (default 10 minutes). If streams still get cut, check the proxy logs for `SLOW STREAM` / `IDLE TIMEOUT` and adjust `SSE_CHUNK_TIMEOUT_MS` / `SSE_IDLE_TIMEOUT_MS`.

**Q: OpenAI / chat completions gives error "Expected 'id' to be a string"?**
A: Fixed. Terminal events are now format-aware: `/v1/chat/completions` streams end with `data: [DONE]`, not the Anthropic-format `event: message_stop`.

**Q: Request body too large (>20MB)?**
A: Rejected cleanly with HTTP `413 payload_too_large` — the body is never forwarded upstream. Stalled uploads are also cut with `408` after `BODY_UPLOAD_TIMEOUT_MS`.

**Q: Where is the API key stored?**
A: **Only in 9Router**, not in the proxy. The proxy only spoofs headers and stores no credentials. If the proxy is exposed (not localhost), set `PROXY_AUTH_TOKEN` and fill it in 9Router as a Bearer token.

**Q: WAF cookie expired?**
A: The proxy refreshes it automatically every 3 minutes via warmup. If a 403 WAF block happens mid-request, it auto re-warms and retries.

**Q: Can I use it without 9Router?**
A: Yes. `curl` directly to `http://localhost:8318/v1/messages` with your agentrouter API key.

---

## License

MIT
