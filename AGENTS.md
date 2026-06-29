# AgentRouter Spoof Proxy — AI Agent Setup Guide

Step-by-step guide for AI coding agents (opencode, Claude Code, Cursor, etc.) to deploy and configure from zero.

## Overview

```
agentrouter-spoof-proxy:8318 → agentrouter.org (upstream)
```

Optionally sits behind a router:
```
client → router → agentrouter-proxy:8318 → agentrouter.org
```

---

## Step 1 — Check Dependencies

| Tool | Check | Install (Ubuntu/Debian) | Install (Arch) | Install (macOS) |
|------|-------|-------------------------|----------------|-----------------|
| Docker | `docker --version` | `apt install docker.io` | `pacman -S docker` | `brew install docker` |
| Docker Compose | `docker compose version` | `apt install docker-compose-v2` | included with docker | included with Docker Desktop |
| git | `git --version` | `apt install git` | `pacman -S git` | `brew install git` |
| curl | `curl --version` | `apt install curl` | `pacman -S curl` | preinstalled |

If Docker is not running:
```bash
sudo systemctl enable --now docker
sudo usermod -aG docker $USER
# log out and back in, or use `newgrp docker`
```

---

## Step 2 — Clone & Deploy

```bash
git clone https://github.com/trefeon/agentrouter-spoof-proxy.git
cd agentrouter-spoof-proxy
```

Optional: copy `.env.example` to `.env` and edit settings:
```bash
cp .env.example .env
# edit .env if needed (all values have sensible defaults)
```

Build and start:
```bash
docker compose up -d --build
```

Verify (wait a few seconds for WAF warmup):
```bash
curl http://localhost:8318/health
```

Expected:
```json
{
  "ok": true,
  "wafCookie": true,
  "circuitOpen": false,
  "modelSource": "static",
  "availableModels": 5
}
```

If `wafCookie` is `false`, wait 5 seconds and retry.

---

## Step 3 — Test Directly

```bash
curl http://localhost:8318/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -d '{
    "model": "claude-opus-4-8",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "say hello"}],
    "stream": true
  }'
```

Replace `YOUR_API_KEY` with your AgentRouter API key.

---

## Step 4 — (Optional) Connect to a Router

If using a router (9Router, LiteLLM, etc.) in Docker:

### 4a. Join the router's network

```bash
# Option A: use docker-compose.override.yml
cp docker-compose.override.yml.example docker-compose.override.yml
# Edit the network name in docker-compose.override.yml
docker compose up -d

# Option B: connect manually
docker network connect ROUTER_NETWORK agentrouter-proxy
```

### 4b. Configure the router

Add the proxy as an upstream provider. Example for an Anthropic-compatible router:

```yaml
providers:
  - name: agentrouter
    type: anthropic-compatible
    base_url: http://agentrouter-proxy:8318
    api_key: YOUR_ROUTER_API_KEY
    models:
      - claude-opus-4-6
      - claude-opus-4-7
      - claude-opus-4-8
      - glm-5.2
```

The proxy is reachable as `agentrouter-proxy` via Docker DNS.

### 4c. Test through the router

```bash
curl http://localhost:ROUTER_PORT/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_ROUTER_API_KEY" \
  -d '{"model": "claude-opus-4-8", "messages": [{"role": "user", "content": "say hello"}], "stream": true}'
```

---

## Step 5 — (Optional) Configure opencode

Edit `~/.config/opencode/opencode.jsonc`, add a provider:

```jsonc
"provider": {
  "agentrouter": {
    "npm": "@ai-sdk/openai-compatible",
    "name": "AgentRouter",
    "options": {
      "baseURL": "http://localhost:8318/v1"
    },
    "models": {
      "claude-opus-4-8": {
        "id": "claude-opus-4-8",
        "name": "Claude Opus 4.8",
        "vision": true, "reasoning": true, "tool_call": true,
        "cost": { "input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25 },
        "limit": { "context": 1000000, "output": 128000 }
      }
    }
  }
}
```

- Replace `localhost` with the host's LAN IP if opencode is on a different machine
- If routing through 9Router, point `baseURL` at the router instead (e.g. `http://localhost:20128/v1`)
- Set API key via `/connect agentrouter` in opencode TUI

---

## Step 6 — (Optional) Enable Model Auto-Discovery

Set `AR_API_KEY` in `.env`:
```bash
AR_API_KEY=your-agentrouter-api-key
```

Recreate:
```bash
docker compose up -d
```

Health endpoint shows `modelSource: "dynamic"` when active.

---

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `wafCookie: false` | WAF warmup failed | Wait a few seconds, check network to `agentrouter.org` |
| `circuitOpen: true` | 5+ consecutive upstream failures | Wait for backoff, check upstream availability |
| 503 `NoChannelError` | No upstream channel for model | Retry or use different model |
| 403 on request | WAF or upstream quota | WAF: auto-retried. Quota: model unavailable |
| 502/504 | Upstream timeout/error | Check network, increase `REQUEST_TIMEOUT_MS` |
| 429 | TPM rate limit | Wait and retry |

## Architecture

```
┌────────────┐     ┌─────────────────────┐     ┌──────────────────┐
│  client     │ ──→ │ agentrouter-proxy   │ ──→ │ agentrouter.org  │
│             │     │ :8318 (spoof proxy) │     │ (upstream)       │
└────────────┘     └─────────────────────┘     └──────────────────┘
```

## Key Files

| File | Purpose |
|------|---------|
| `proxy.mjs` | Main proxy (~680 lines, zero dependencies) |
| `Dockerfile` | `FROM node:22-alpine`, HEALTHCHECK on `/health` |
| `docker-compose.yml` | Service config with `.env` support |
| `.env.example` | All configurable environment variables |
| `docker-compose.override.yml.example` | Network integration template |
| `tests/` | Test suite (`node --test tests/proxy.test.mjs`) |

## Env Vars

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN_PORT` | `8318` | Listen port |
| `TARGET_HOST` | `agentrouter.org` | Upstream host |
| `TARGET_PORT` | `443` | Upstream port |
| `REQUEST_TIMEOUT_MS` | `120000` | Request timeout (ms) |
| `MODELS_CSV` | `claude-opus-4-6,...` | Static model list |
| `WARMUP_INTERVAL_MS` | `180000` | WAF cookie refresh (ms) |
| `MAX_RETRIES` | `2` | Retry attempts |
| `RETRY_DELAY_MS` | `1000` | Base retry delay (ms) |
| `AR_API_KEY` | _(empty)_ | API key for model discovery |
| `DISCOVERY_INTERVAL_MS` | `600000` | Model list refresh (ms) |

## Model Notes

- All Claude Opus models: 1M context, 128k output, vision, reasoning, tool calls
- GLM-5.2: 1M context, 131k output, reasoning, tool calls (vision untested)
- `gpt-5.5` always returns 403 (upstream quota)
- `NoChannelError` (503) is normal — upstream channels fluctuate
- Opus 4.8 uses ~35% fewer output tokens than 4.7 at same effort level
