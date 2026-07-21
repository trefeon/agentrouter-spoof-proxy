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

Expected: `"ok":true,"wafCookie":true`

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
| `proxy.mjs` | Main orchestration (core module) |
| `src/config.mjs` | Env config + agent pool |
| `src/utils.mjs` | Pure functions (path, headers, injection) |
| `src/logger.mjs` | Logging |
| `src/auth/spoof.mjs` | Claude Code header spoofing |
| `src/auth/waf.mjs` | WAF cookie warmup |
| `src/models/discovery.mjs` | Static/dynamic model discovery |
| `src/models/health.mjs` | Auto-detect failing models |
| `src/models/stats.mjs` | Model success metrics |
| `src/resilience/circuit-breaker.mjs` | Circuit breaker |
| `tests/unit/utils.test.mjs` | 51 unit tests (<500ms) |
| `tests/proxy.test.mjs` | 30 E2E tests |
| `docs/panduan-9router.md` | 🇮🇩 Panduan Bahasa Indonesia |

---

## Troubleshooting

| Problem | Fix |
|---------|-----|
| `wafCookie: false` | Wait 5s, warmup in progress |
| Model keeps getting 500 | **Auto model health** removes it from list → 9Router falls back |
| Docker: 9Router can't reach proxy | `docker network connect 9router-net agentrouter-proxy` |
| Windows: 9Router can't reach proxy | Use `host.docker.internal` in Base URL |
