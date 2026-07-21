# AgentRouter Spoof Proxy

Lightweight Node.js reverse proxy that bypasses AgentRouter WAF by spoofing Claude Code headers. Zero dependencies, ~600 lines orchestration + 8 small modules, 120MB Docker image.

> 🇮🇩 **[Panduan 9Router Bahasa Indonesia](docs/panduan-9router.md)** — Tutorial lengkap untuk teman-teman Indonesia.

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
{"ok":true,"upstream":"agentrouter.org:443","availableModels":5,"activeStreams":0,"wafCookie":true,"circuitOpen":false}
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
4. **Add API Key** → paste your AgentRouter API key (simpan di 9Router aja, bukan di proxy)
5. Model will appear as `AG-claude-opus-4-8`, `AG-glm-5.2`, etc.

> Windows Docker Desktop: use `http://host.docker.internal:8318/v1`

---

## Features

| Feature | How |
|---------|-----|
| **WAF bypass** | Spoofs Claude Code CLI headers + maintains `acw_tc` cookies |
| **SSE streaming** | Pipes with backpressure, keepalive pings, idle/chunk timeouts |
| **Retry logic** | Auto-retry on 5xx/timeout/ECONNRESET with exponential backoff |
| **Circuit breaker** | Opens after 5 consecutive failures, progressive cooldown |
| **Auto model health** | Failing models removed from `/v1/models` → 9Router falls back instantly |
| **Model recovery** | Background probe every 60s with spoof headers + WAF cookie |
| **Prompt injection** | Optional system prompt injection (`INJECT_SYSTEM_PROMPT`) |
| **Model discovery** | Optional dynamic model list via `AR_API_KEY` |
| **Graceful shutdown** | Drains active streams, destroys agent pool |

---

## Endpoints

| Path | Method | Description |
|------|--------|-------------|
| `/health` | GET | Status, WAF cookie, circuit breaker, streams |
| `/v1/models` | GET | Available models (unhealthy ones auto-filtered) |
| `/v1/messages` | POST | Anthropic Messages API → proxied |
| `/messages` | POST | Auto-rewritten to `/v1/messages` |
| `/v1/chat/completions` | POST | OpenAI Chat Completions → proxied |

---

## Configuration (`.env`)

All values have defaults — copy `.env.example` to `.env` only if you need to change something.

| Variable | Default | What it does |
|----------|---------|-------------|
| `LISTEN_PORT` | `8318` | Proxy port |
| `TARGET_HOST` | `agentrouter.org` | Upstream host |
| `REQUEST_TIMEOUT_MS` | `300000` | Request timeout (5min) |
| `MAX_RETRIES` | `2` | Retry count |
| `AR_API_KEY` | _(empty)_ | Enable dynamic model discovery |
| `INJECT_SYSTEM_PROMPT` | _(empty)_ | System prompt injected into requests |
| `SLOW_RESPONSE_MS` | `15000` | Temporarily degrades models with slow successful streams |
| `LOG_LEVEL` | `info` | `info` or `debug` |

See `.env.example` for the full list (14 variables).

---

## Models

| Model | Context | Input/Output per MTok | Provider |
|-------|---------|----------------------|----------|
| `claude-opus-4-8` | 1M | $5 / $25 | [Anthropic](https://docs.anthropic.com/en/docs/about-claude/models) |
| `claude-opus-4-7` | 1M | $5 / $25 | [Anthropic](https://docs.anthropic.com/en/docs/about-claude/models) |
| `claude-opus-4-6` | 1M | $5 / $25 | [Anthropic](https://docs.anthropic.com/en/docs/about-claude/models) |
| `gpt-5.5` | 1M | $5 / $30 | [OpenAI](https://openai.com/index/introducing-gpt-5-5/) |
| `glm-5.2` | 1M | ~$0.84 / ~$2.63 | [ZhipuAI](https://docs.bigmodel.cn/cn/guide/start/model-overview) |

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
proxy.mjs (~600 lines, orchestration)
├── src/config.mjs              — env + constants + agent pool
├── src/logger.mjs              — logging
├── src/utils.mjs               — pure functions (path, headers, injection, etc.)
├── src/auth/spoof.mjs          — Claude Code header spoofing
├── src/auth/waf.mjs            — WAF cookie warmup
├── src/models/discovery.mjs    — static/dynamic model discovery
├── src/models/health.mjs       — auto-detect failing models, recovery probe
├── src/models/stats.mjs        — model success metrics
└── src/resilience/circuit-breaker.mjs — circuit breaker state
```

---

## Running Tests

```bash
# Fast unit tests — pure functions, no network (51 tests, <500ms)
node --test tests/unit/utils.test.mjs

# E2E tests — spawns proxy + mock upstream (30 tests, ~12s)
node --test tests/proxy.test.mjs
```

---

## Tanya-Jawab (FAQ)

**Q: Kenapa Claude model kadang error 500?**
A: Bukan bug proxy. Upstream agentrouter.org kadang Go panic buat Claude models. Proxy punya **auto model health** — model error langsung dihapus dari `/v1/models` dan 9Router otomatis fallback ke model lain. Recovery probe tiap 60 detik. Progressive cooldown: 30s → 1m → 2m → 5m → 10m.

**Q: API key disimpen dimana?**
A: **Di 9Router aja**, bukan di proxy. Proxy cuma spoof header, ga nyimpen credential.

**Q: WAF cookie expired?**
A: Proxy refresh otomatis tiap 3 menit via warmup. Kalau kena 403 WAF mid-request, auto re-warmup & retry.

**Q: Bisa tanpa 9Router?**
A: Bisa. `curl` langsung ke `http://localhost:8318/v1/messages` dengan API key agentrouter.

---

## License

MIT
