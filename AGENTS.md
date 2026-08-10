# AgentRouter Spoof Proxy — AI Agent Setup Guide

Quick setup for AI coding agents (opencode, Claude Code, Cursor).

## One-Line Install

### Linux
```bash
curl -fsSL https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.sh | bash
```

### Windows (PowerShell Admin)
```powershell
iwr -useb https://raw.githubusercontent.com/trefeon/agentrouter-spoof-proxy/main/scripts/install.ps1 | iex
```

The script auto-detects Docker/PM2 and guides you through setup.

---

## Manual Install (3 steps)

```bash
git clone https://github.com/trefeon/agentrouter-spoof-proxy.git
cd agentrouter-spoof-proxy
cp .env.example .env

# Pick one:
docker compose up -d --build     # Docker (recommended)
pm2 start proxy.mjs --name agentrouter-proxy   # PM2
node proxy.mjs                   # Direct (foreground)
```

---

## Verify

```bash
curl http://localhost:8318/health
```

Expected: `{"ok":true,"upstream":"agentrouter.org:443","modelSource":"static","staticModels":3,"availableModels":3,"activeStreams":0,"wafCookie":true,"circuitOpen":false,"consecutiveFails":0,"modelHealth":[]}`

---

## Connect to 9Router

1. Dashboard → Providers → **Add OpenAI Compatible**
2. Name: `AgentRouter`, Prefix: `AG`, Type: `chat completions`
3. Base URL: `http://localhost:8318/v1`
4. **Import from /models**
5. Add API Key (AgentRouter key — stored in 9Router only)

> Windows Docker Desktop: use `http://host.docker.internal:8318/v1`

---

## Key Files

| File | Purpose |
|------|---------|
| `proxy.mjs` | Thin entry: routing allowlist, optional inbound auth, schedulers, lifecycle (~166 lines) |
| `src/proxy/handler.mjs` | Request handler: buffering, body-limit 413, upload 408, telemetry, retry loop (invariant flags documented) |
| `src/proxy/stream.mjs` | SSE streaming pump: keepalive, framing, stall watchdog, backpressure, format-aware EOM |
| `src/errors.mjs` | `buildError(msg, code)` + `isOurError` marker |
| `src/status-code.mjs` | Error code → HTTP status mapping (pure) |
| `src/config.mjs` | Env config + `validateConfig()` startup validation + agent pool |
| `src/utils.mjs` | Pure functions (path, route allowlist, auth compare, headers, retry, adaptive timeout) |
| `src/logger.mjs` | Logging |
| `src/auth/spoof.mjs` | Claude Code header spoofing |
| `src/auth/waf.mjs` | WAF cookie warmup |
| `src/models/discovery.mjs` | Static/dynamic model discovery |
| `src/models/health.mjs` | Auto-detect failing models |
| `src/models/stats.mjs` | Model success metrics |
| `src/resilience/circuit-breaker.mjs` | Circuit breaker (final-5xx accounting) |
| `tests/unit/utils.test.mjs` | Unit tests: pure functions |
| `tests/unit/errors.test.mjs` | Unit tests: errors + status mapping |
| `tests/unit/stream.test.mjs` | Unit tests: SSE pump (real sockets, framing, keepalives) |
| `tests/unit/resilience.test.mjs` | Unit tests: circuit breaker + config validation |
| `tests/unit/models.test.mjs` | Unit tests: health, stats, discovery, routing helpers |
| `tests/proxy.test.mjs` | 55 E2E tests |
| `tests/verify-issues.test.mjs` | 7 issue-verification regression tests |
| `package.json` | Scripts: `test`, `test:unit`, `test:e2e`, `test:verify`, `coverage`, `lint`, `check` (oxlint devDep only) |
| `docs/panduan-9router.md` | 🇮🇩 Indonesian tutorial (Bahasa Indonesia) |

---

## Troubleshooting

| Problem | Fix |
|---------|-----|
| `wafCookie: false` | Wait 5s, warmup in progress |
| Model keeps getting 500 | **Auto model health** removes it from list → 9Router falls back |
| Docker: 9Router can't reach proxy | `docker network connect 9router-net agentrouter-proxy` |
| Windows: 9Router can't reach proxy | Use `host.docker.internal` in Base URL |
