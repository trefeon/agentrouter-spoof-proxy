# AgentRouter Spoof Proxy — Full Setup Guide for AI Agents

This guide walks through the entire setup from zero to working proxy, designed for AI coding agents (opencode, Claude Code, etc.).

## Overview

Three components deployed in order:

```
agentrouter-spoof-proxy (this repo) → 9Router → AgentRouter (upstream)
```

1. **This proxy** — injects spoof headers, maintains WAF cookies
2. **9Router** — OpenAI-compatible router, calls this proxy internally
3. **opencode** — connects to 9Router via `@ai-sdk/openai-compatible`

---

## Step 1 — Clone & Check Dependencies

```bash
git clone https://github.com/trefeon/agentrouter-spoof-proxy.git
cd agentrouter-spoof-proxy
```

Check that these are installed. Install missing ones with the user's package manager:

| Tool | Check command | Install (Ubuntu/Debian) | Install (Arch) | Install (macOS) |
|------|---------------|-------------------------|----------------|-----------------|
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

## Step 2 — Deploy the Proxy

```bash
docker compose up -d --build
```

Wait a few seconds, then verify:

```bash
curl http://localhost:8318/health
```

Expected response shows the proxy is alive and WAF cookie acquired:
```json
{
  "ok": true,
  "wafCookie": true,
  "circuitOpen": false,
  "modelSource": "static",
  "availableModels": 5
}
```

If `wafCookie` is `false`, wait 5 more seconds and retry.

---

## Step 3 — Set Up 9Router

### 3a. Clone & deploy 9Router

```bash
git clone https://github.com/<user>/9router.git
cd 9router
# follow 9Router's own setup instructions
```

9Router should be running on port `20128`. Verify:

```bash
curl http://localhost:20128/v1/models
```

### 3b. Connect proxy to 9Router network

Both containers must be on the same Docker network. The proxy's `docker-compose.yml` already specifies `9router-net` as an external network and uses `agentrouter-proxy` as the service name for DNS resolution.

Check that 9Router is on `9router-net`:
```bash
docker network inspect 9router-net
```

If 9Router is not on `9router-net`, connect it:
```bash
docker network connect 9router-net 9router
```

### 3c. Add provider to 9Router config

In 9Router's configuration, add this provider:

```yaml
providers:
  - name: agentrouter
    type: anthropic-compatible
    base_url: http://agentrouter-proxy:8318
    api_key: sk_9router_test
    models:
      - AG/claude-opus-4-6
      - AG/claude-opus-4-7
      - AG/claude-opus-4-8
      - AG/glm-5.2
```

The proxy is reachable as `agentrouter-proxy` via Docker DNS on `9router-net`.

### 3d. Test the full proxy chain

```bash
curl http://localhost:20128/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk_9router_test" \
  -d '{"model": "AG/claude-opus-4-8", "messages": [{"role": "user", "content": "say hello"}], "stream": true}'
```

You should see a streaming SSE response. If you get a 503 `NoChannelError`, retry — channels fluctuate.

---

## Step 4 — Configure opencode

### 4a. Add provider

Edit `~/.config/opencode/opencode.jsonc` and add the `"9router"` block under `"provider"`:

```jsonc
"9router": {
  "npm": "@ai-sdk/openai-compatible",
  "name": "9Router",
  "options": {
    "baseURL": "http://<SERVER_LAN_IP>:20128/v1"
  },
  "models": {
    "claude-opus-4-6": {
      "id": "AG/claude-opus-4-6",
      "name": "Claude Opus 4.6",
      "vision": true, "reasoning": true, "tool_call": true,
      "cost": { "input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25 },
      "limit": { "context": 1000000, "output": 128000 }
    },
    "claude-opus-4-7": {
      "id": "AG/claude-opus-4-7",
      "name": "Claude Opus 4.7",
      "vision": true, "reasoning": true, "tool_call": true,
      "cost": { "input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25 },
      "limit": { "context": 1000000, "output": 128000 }
    },
    "claude-opus-4-8": {
      "id": "AG/claude-opus-4-8",
      "name": "Claude Opus 4.8",
      "vision": true, "reasoning": true, "tool_call": true,
      "cost": { "input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25 },
      "limit": { "context": 1000000, "output": 128000 }
    },
    "glm-5.2": {
      "id": "AG/glm-5.2",
      "name": "GLM 5.2",
      "reasoning": true, "tool_call": true,
      "cost": { "input": 1.4, "output": 4.4, "cache_read": 0.26, "cache_write": 1.75 },
      "limit": { "context": 1000000, "output": 131072 }
    }
  }
}
```

Replace `<SERVER_LAN_IP>`:
- `localhost` if opencode is on the same machine as 9Router
- The LAN IP (e.g. `192.168.123.11`) if opencode is on a different machine

### 4b. Set API key

In opencode TUI:
```
/connect 9router
```
Enter: `sk_9router_test`

### 4c. Activate model

Switch to `9router/claude-opus-4-8` in opencode and test with a simple question.

---

## Step 5 — Optional: Enable Model Auto-Discovery

AgentRouter's available models change periodically. To auto-discover them, set `AR_API_KEY` in `docker-compose.yml`:

```yaml
environment:
  - AR_API_KEY=<your-agentrouter-api-key>
  - DISCOVERY_INTERVAL_MS=600000
```

Then recreate:
```bash
docker compose up -d
```

The health endpoint will show `modelSource: "dynamic"` when discovery is active.

---

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `wafCookie: false` | WAF warmup failed | Wait a few seconds, check network connectivity to `agentrouter.org` |
| `circuitOpen: true` | 5+ consecutive upstream failures | Wait for backoff to expire, check `agentrouter.org` availability |
| 503 `NoChannelError` | No upstream channel for that model | Retry or use a different model ID |
| 403 on request | WAF blocking or upstream quota | For WAF: the proxy auto-retries. For quota: model is unavailable |
| 502/504 | Upstream timeout or connection error | Check network, increase `REQUEST_TIMEOUT_MS` |
| 429 | TPM rate limit (common with GLM-5.2) | Wait and retry |
| `Host` header mismatch | Rare, fixed in this proxy | Ensure `TARGET_HOST` is `agentrouter.org` (not an IP) |

## Architecture Reference

```
┌──────────────┐     ┌──────────┐     ┌─────────────────────┐     ┌──────────────────┐
│  opencode     │ ──→ │ 9Router  │ ──→ │ agentrouter-proxy   │ ──→ │ agentrouter.org  │
│  (any client) │     │ :20128   │     │ :8318 (spoof proxy) │     │ (upstream)       │
└──────────────┘     └──────────┘     └─────────────────────┘     └──────────────────┘
                     OpenAI-format      Anthropic-format
                     AG/ prefix         spoof headers + WAF
```

## Key Files

| File | Purpose |
|------|---------|
| `proxy.mjs` | Main proxy source (single file, ~480 lines) |
| `Dockerfile` | `FROM node:22-alpine`, HEALTHCHECK on `/health` |
| `docker-compose.yml` | Service config, networking, env vars |
| `AGENTS.md` | This file — AI agent setup guide |

## Env Vars

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN_PORT` | `8318` | Listen port |
| `TARGET_HOST` | `agentrouter.org` | Upstream host |
| `TARGET_PORT` | `443` | Upstream port |
| `MODELS_CSV` | `claude-opus-4-6,...` | Static model fallback list |
| `WARMUP_INTERVAL_MS` | `180000` | WAF cookie refresh (3 min) |
| `MAX_RETRIES` | `2` | Retry attempts on failure |
| `RETRY_DELAY_MS` | `1000` | Base retry delay (doubles) |
| `AR_API_KEY` | `""` | API key for model auto-discovery |
| `DISCOVERY_INTERVAL_MS` | `600000` | Model list refresh (10 min) |

## Model Notes

- All Claude Opus models: 1M context, 128k output, vision, reasoning, tool calls
- GLM-5.2: 1M context, 131k output, reasoning, tool calls (vision untested)
- `gpt-5.5` always returns 403 (upstream quota)
- `NoChannelError` (503) is normal — upstream channels are pooled and fluctuate
- Opus 4.8 uses ~35% fewer output tokens than 4.7 at same effort level
