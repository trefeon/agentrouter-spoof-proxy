# AgentRouter Spoof Proxy

Cross-platform reverse proxy in Go that bypasses the AgentRouter WAF by spoofing Claude Code headers. Ships as a single static binary with no runtime dependencies. Docker image is about 15 to 25 MB.

> 🇮🇩 **[Panduan 9Router (Bahasa Indonesia)](docs/panduan-9router.md):** tutorial in Indonesian for integrating with 9Router.

---

## Quick Install

### Linux: one command

```bash
curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash
```

Non-interactive Docker install:

```bash
curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash -s -- --yes --docker
```

Host install as a systemd service (`--pm2` is a deprecated alias):

```bash
curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash -s -- --systemd
```

Dry-run without changing the system:

```bash
curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash -s -- --dry-run --docker
```

### Windows: one command (PowerShell as Admin)

```powershell
iwr -useb https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.ps1 | iex
```

Use `-Service` to register a Windows service, `-Docker` for Docker Desktop
(`-PM2` is a deprecated alias).

The installer downloads the prebuilt binary from GitHub Releases, or falls back
to building from source (Go 1.26+). It auto-detects Docker/systemd and guides
you through the rest.

### Manual / Docker

```bash
git clone https://github.com/trefeon/agentrouter-spoof-proxy.git
cd agentrouter-spoof-proxy
cp .env.example .env

# Pick one:
docker compose up -d --build                 # Docker (recommended)
go build -o agentrouter-proxy ./cmd/proxy && ./agentrouter-proxy   # Direct
make build && ./dist/proxy                    # via Makefile
```

---

## Verify

```bash
curl http://localhost:8318/health
```

```json
{"ok":true,"upstream":"agentrouter.org:443","modelSource":"static","staticModels":3,"availableModels":3,"activeStreams":0,"wafCookie":true,"circuitOpen":false,"consecutiveFails":0,"modelHealth":[]}
```

Wait 5 seconds if `wafCookie: false`. WAF warmup runs at startup.

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
| **WAF bypass** | Spoofs Claude Code CLI headers and maintains `acw_tc` cookies |
| **SSE streaming** | Proxies with backpressure and keepalives, handles end of message per format, never cuts a live stream |
| **Retry logic** | Retries transport errors with exponential backoff, 5xx retry is configurable (`RETRY_ON_5XX`) |
| **Thinking tag stripping** | Removes `<think>...</think>` from OpenAI-format SSE streams, Anthropic thinking blocks pass through (`STRIP_THINKING_TAGS`) |
| **Model-aware headers** | Sends Anthropic headers for `/v1/messages`, generic headers for `/v1/chat/completions` |
| **Circuit breaker** | Opens after 5 consecutive final 5xx or transport failures, exponential cooldown capped at 600s |
| **Auto model health** | Removes failing models from `/v1/models` so 9Router can fall back right away |
| **Model recovery** | Background probe every 60s with spoof headers and WAF cookie |
| **Prompt injection** | Optional system prompt injection (`INJECT_SYSTEM_PROMPT`) |
| **Model discovery** | Optional dynamic model list via `AR_API_KEY` |
| **Bounded bodies** | 20MB limit gives clean `413`, stalled uploads give `408` |
| **Narrow proxy surface** | Only 3 API routes are proxied, binds to localhost by default, optional token auth |
| **Graceful shutdown** | Drains active streams, cancels schedulers, 15s force-exit bound |

---

## Endpoints

| Path | Method | Description |
|------|--------|-------------|
| `/health`, `/api/health` | GET | Status, WAF cookie, circuit breaker, streams (no auth required) |
| `/v1/models`, `/models` | GET | Available models (unhealthy ones auto-filtered) |
| `/v1/messages` | POST | Anthropic Messages API, proxied |
| `/messages` | POST | Auto-rewritten to `/v1/messages` |
| `/v1/chat/completions` | POST | OpenAI Chat Completions, proxied |

Only the three proxy routes above are ever forwarded upstream. Unknown paths
return a local `404`, unsupported methods return a local `405`, and when
`PROXY_AUTH_TOKEN` is set, missing or invalid credentials return `401` before any
upstream work. SSE terminal events depend on the format. Anthropic streams end with
`event: message_stop`, OpenAI `chat.completions` streams end with `data: [DONE]`,
and long-lived streams stay open while the upstream connection is alive.

---

## Configuration (`.env`)

All values have defaults. Copy `.env.example` to `.env` only if you need to change something. Env names match the Node.js version, so existing `.env` files keep working.

| Variable | Default | What it does |
|----------|---------|-------------|
| `LISTEN_PORT` | `8318` | Proxy port |
| `LISTEN_ADDRESS` | `127.0.0.1` | Bind address. Set `0.0.0.0` only for Docker-to-Docker or remote access (use with `PROXY_AUTH_TOKEN`) |
| `PROXY_AUTH_TOKEN` | _(empty)_ | Optional inbound auth. Requires header `Authorization: Bearer` or `X-Proxy-Token` |
| `TARGET_PROTOCOL` | `https` | Upstream protocol |
| `TARGET_HOST` | `agentrouter.org` | Upstream host |
| `TARGET_PORT` | `443` | Upstream port |
| `WARMUP_INTERVAL_MS` | `180000` | WAF cookie warmup interval (3 min) |
| `REQUEST_TIMEOUT_MS` | `300000` | Request timeout (5 min) |
| `RESPONSE_TIMEOUT_MS` | `30000` | Wait for upstream response headers before 504 or retry |
| `SSE_IDLE_TIMEOUT_MS` | `600000` | Kill stream after no SSE events (dead upstream). OpenAI-format upstreams send no liveness pings, so a genuinely silent reasoning pause near this timeout will be cut. Raise it for long-thinking OpenAI models |
| `SSE_CHUNK_TIMEOUT_MS` | `30000` | Stall watchdog. Reports slow streams and keeps the connection alive |
| `BODY_UPLOAD_TIMEOUT_MS` | `60000` | Reject stalled uploads with 408 |
| `MAX_RETRIES` | `2` | Retry count for transport errors (3 attempts total) |
| `RETRY_DELAY_MS` | `1000` | Base backoff delay. Actual delay is `RETRY_DELAY_MS × 2^attempt` |
| `RETRY_ON_5XX` | `false` | Also retry on 5xx responses (warning: causes double token billing) |
| `STRIP_THINKING_TAGS` | `true` | Remove `<think>...</think>` from OpenAI-format SSE text. Anthropic thinking blocks pass through |
| `MODELS_CSV` | `gpt-5.6-sol,claude-opus-5,claude-opus-4-8` | Static fallback model list (used when `AR_API_KEY` is not set) |
| `AR_API_KEY` | _(empty)_ | Enable dynamic model discovery |
| `DISCOVERY_INTERVAL_MS` | `600000` | Dynamic model discovery refresh interval |
| `INJECT_SYSTEM_PROMPT` | _(empty)_ | System prompt injected into requests |
| `SLOW_RESPONSE_MS` | `30000` | Temporarily marks slow successful streams as degraded |
| `LOG_LEVEL` | `info` | `info` or `debug` |

See `.env.example` for the full list.

### Retry behavior and token billing

By default (`RETRY_ON_5XX=false`), the proxy only retries **transport-level errors**
(timeout, connection reset, unreachable), up to `MAX_RETRIES=2` (3 attempts total)
with an exponential delay of `RETRY_DELAY_MS × 2^attempt`. HTTP 5xx responses
from the upstream are forwarded to the client right away.

When `RETRY_ON_5XX=true`, the proxy also retries on 5xx responses. **Warning:** each
retry re-sends the full request body to the upstream, so the upstream counts
tokens for every attempt. If a request is retried once, 9Router will show about 2x the input
tokens. With `MAX_RETRIES=2`, up to 3x is possible.

WAF cookie retries (403/405) always happen regardless of `RETRY_ON_5XX`.

### Thinking tag stripping

The proxy spoofs the `interleaved-thinking-2025-05-14` Anthropic beta header, so
Claude returns thinking blocks. When a downstream client does not understand
those blocks, thinking content leaks as raw `<think>...</think>` tags in the text.

With `STRIP_THINKING_TAGS=true` (default), the proxy removes `<think>...</think>` tags
from **OpenAI-format** (`/v1/chat/completions`) SSE text before forwarding.
OpenAI clients cannot render thinking blocks. Stripping works at the byte level, so tags
and their content are handled correctly even when they span multiple SSE chunks or split
mid-tag at a chunk boundary, and multi-byte UTF-8 content is never corrupted. If the
upstream ends while a thinking span is still open, the stream ends with a `502` error
frame instead of leaking raw tags. **Anthropic-format** (`/v1/messages`) thinking
blocks always pass through unchanged. Harness clients (opencode, OpenClaw, claude-code)
render thinking natively, and stripping there would remove reasoning and create
silent gaps that trigger client-side idle watchdogs. Set to `false` if your OpenAI
client supports thinking content.

### Model-aware headers

The proxy applies different spoof headers based on the request format:
- **`/v1/messages`** (Anthropic): full Claude Code headers including `Anthropic-Beta`,
  `Anthropic-Version`, etc.
- **`/v1/chat/completions`** (OpenAI): generic `claude-cli` User-Agent and `X-Stainless-*`
  headers only. No `anthropic-*` headers are sent.

The proxy owns the canonical `Anthropic-Version` and `Anthropic-Beta` spoof values.
A client-supplied `anthropic-version` on `/v1/messages` is ignored so the
spoofed identity stays intact. WAF cookies are also refreshed from API responses,
not just the warmup loop, so rotated session cookies are picked up right away.

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
cmd/proxy/main.go: thin entry, config validation, signal shutdown, -healthcheck flag
├── internal/config: env config (caarlos0/env) and Validate() (slog wired in server/main)
├── internal/auth: spoof headers and WAF cookie store/warmup
├── internal/models: discovery, health probing, stats
├── internal/resilience: circuit breaker (atomic)
├── internal/proxy: handler (retry loop), SSE pump, pure helpers (think-strip, frame parser)
├── internal/server: routing, schedulers, graceful shutdown
├── testutil/mockupstream: scripted mock upstream for tests
└── e2e: 62 E2E and 7 issue-regression tests
```

---

## Building & Testing

> Runtime is zero-dependency. Dev tooling: Go 1.26+, golangci-lint (optional).

```bash
# Everything (~222 tests, all packages)
go test ./...

# Fast unit tests (pure core + pump + handler)
go test ./internal/...

# E2E: in-process proxy + mock upstream (62 tests)
go test ./e2e/

# Issue-verification regression tests (7 tests)
go test ./e2e/ -run TestIssue

# Race detector (needs a C toolchain for cgo)
go test -race ./...

# Lint + vet + build
make check        # vet + lint + test
go vet ./...
go build ./...

# Cross-compile all platforms
make cross        # → dist/ (linux amd64/arm64, darwin arm64, windows amd64)

# Multi-arch Docker image
make image        # buildx, linux/amd64 + linux/arm64
```

## Local Debugging

| Symptom | Command |
|---------|---------|
| Verbose proxy logs | `LOG_LEVEL=debug go run ./cmd/proxy` |
| Attach debugger | `dlv debug ./cmd/proxy` (or GoLand/VSCode Go) |
| Health probe for Docker | `/proxy -healthcheck` (exit 0/1) |
| Graceful shutdown | `SIGTERM`/`SIGINT` drains streams, 15s force-exit bound |

---

## FAQ

**Q: Why do Claude models sometimes return 500?**
A: Not a proxy bug. The agentrouter.org upstream sometimes panics in Go for Claude models. The proxy has **auto model health**. A failing model is removed from `/v1/models` right away and 9Router falls back to another model automatically. Recovery probe runs every 60 seconds. Progressive cooldown: 30s, 1m, 2m, 5m, 10m.

**Q: Streaming keeps disconnecting or getting cut mid-answer?**
A: The proxy never cuts a stream that is still alive. As long as the upstream is connected, the stall watchdog only resets. This is safe for long thinking or tool use. A stream only ends when the upstream finishes, an error occurs, the client disconnects, or the stream is truly idle (no events at all) past `SSE_IDLE_TIMEOUT_MS` (default 10 minutes). If streams still get cut, check the logs for `SLOW STREAM` or `IDLE TIMEOUT` and raise `SSE_CHUNK_TIMEOUT_MS` or `SSE_IDLE_TIMEOUT_MS`.

**Q: OpenAI / chat completions gives error "Expected 'id' to be a string"?**
A: Fixed. Terminal events are now format-aware. `/v1/chat/completions` streams end with `data: [DONE]`, not the Anthropic `event: message_stop`.

**Q: Request body too large (>20MB)?**
A: Rejected with HTTP `413 payload_too_large`. The body is never forwarded upstream. Stalled uploads are cut with `408` after `BODY_UPLOAD_TIMEOUT_MS`.

**Q: Where is the API key stored?**
A: **Only in 9Router**, not in the proxy. The proxy only spoofs headers and stores no credentials. If the proxy is exposed (not localhost), set `PROXY_AUTH_TOKEN` and use it as a Bearer token in 9Router.

**Q: WAF cookie expired?**
A: The proxy refreshes it automatically every 3 minutes via warmup. If a 403 WAF block happens mid-request, it re-warms and retries automatically.

**Q: Can I use it without 9Router?**
A: Yes. `curl` directly to `http://localhost:8318/v1/messages` with your agentrouter API key.

---

## License

MIT
