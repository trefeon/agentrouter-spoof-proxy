# AgentRouter Spoof Proxy

A lightweight Node.js reverse proxy that injects Claude Code spoof headers and maintains WAF cookies to bypass AgentRouter restrictions. Designed to sit between 9Router and AgentRouter as an Anthropic-compatible provider.

## Architecture

```
opencode/LLM client → 9Router → agentrouter-proxy:8318 → agentrouter.org (upstream)
```

The proxy:
- Rewrites `/messages` → `/v1/messages` (Anthropic API format)
- Injects all spoof headers (`User-Agent`, `X-Stainless-*`, `Anthropic-Beta`, etc.)
- Maintains `acw_tc` WAF cookies via periodic warmup
- Pipes SSE streaming responses through without buffering
- Retries on timeouts/5xx with exponential backoff
- Circuit breaker on consecutive failures

## Prerequisites

- Docker & Docker Compose
- A running [9Router](https://github.com/your-org/9router) instance
- An AgentRouter API key

## Setup

### 1. Deploy the proxy

```bash
docker compose up -d --build
```

This starts the proxy on port `8318` and attaches it to the `9router-net` Docker network so 9Router can reach it internally.

### 2. Verify it's running

```bash
curl http://localhost:8318/health
```

Expected response:
```json
{
  "ok": true,
  "upstream": "agentrouter.org:443",
  "models": 5,
  "wafCookie": true,
  "circuitOpen": false,
  "consecutiveFails": 0,
  "cachedIps": 2
}
```

If `wafCookie` is `false`, the warmup hasn't completed yet — wait a few seconds and retry.

---

## Connecting to 9Router

9Router needs to know about this proxy as an upstream provider.

### Add provider to 9Router config

In your 9Router configuration, add an `anthropic-compatible` provider pointing to the proxy:

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

The proxy is reachable as `agentrouter-proxy` (Docker DNS on `9router-net`).

### Send requests through 9Router

```bash
curl http://localhost:20128/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk_9router_test" \
  -d '{
    "model": "AG/claude-opus-4-8",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'
```

---

## Connecting to opencode

### 1. Add the provider to `opencode.jsonc`

Edit `~/.config/opencode/opencode.jsonc`:

```json
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
      "reasoning": true, "tool_call": true,
      "cost": { "input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25 },
      "limit": { "context": 1000000, "output": 128000 }
    },
    "claude-opus-4-7": {
      "id": "AG/claude-opus-4-7",
      "name": "Claude Opus 4.7",
      "reasoning": true, "tool_call": true,
      "cost": { "input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25 },
      "limit": { "context": 1000000, "output": 128000 }
    },
    "claude-opus-4-8": {
      "id": "AG/claude-opus-4-8",
      "name": "Claude Opus 4.8",
      "reasoning": true, "tool_call": true,
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

Replace `<SERVER_LAN_IP>` with the IP of the machine running 9Router (e.g. `192.168.123.11`). Use `localhost` if opencode is on the same machine.

### 2. Set the API key

In opencode TUI, run:

```
/connect 9router
```

Enter: `sk_9router_test`

### 3. Switch model

Select `9router/claude-opus-4-8` as your active model and start chatting.

---

## Model Modalities & opencode Config

### Capabilities per model

| Model | Input | Output | Vision | Reasoning | Tool Call |
|-------|-------|--------|--------|-----------|-----------|
| `claude-opus-4-6` | text, image (1568px cap) | text | ✅ | ✅ | ✅ |
| `claude-opus-4-7` | text, image (2576px high-res) | text | ✅ | ✅ | ✅ |
| `claude-opus-4-8` | text, image (2576px high-res) | text | ✅ | ✅ | ✅ |
| `glm-5.2` | text | text | untested | ✅ | ✅ |

All Claude Opus models accept `image` content blocks via Anthropic Messages API (base64 or URL). The proxy passes the request body through unchanged — image data reaches the upstream as-is.

### opencode config reference

Each model entry supports these fields:

```jsonc
"claude-opus-4-8": {
  "id": "AG/claude-opus-4-8",       // model ID sent to 9Router
  "name": "Claude Opus 4.8",         // display name in TUI
  "reasoning": true,                 // enables extended/adaptive thinking
  "tool_call": true,                 // enables tool/function calling
  "vision": true,                    // enables image input support
  "cost": {
    "input": 5,                      // $/MTok input
    "output": 25,                    // $/MTok output
    "cache_read": 0.5,              // $/MTok cache read
    "cache_write": 6.25             // $/MTok cache write
  },
  "limit": {
    "context": 1000000,             // max context window (tokens)
    "output": 128000                // max output tokens
  }
}
```

Set `"vision": false` on models that don't support images to prevent opencode from sending image content blocks. All three Claude Opus models support vision; GLM-5.2 vision capability is untested.

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN_PORT` | `8318` | Proxy listen port |
| `TARGET_HOST` | `agentrouter.org` | Upstream hostname |
| `TARGET_PORT` | `443` | Upstream port |
| `REQUEST_TIMEOUT_MS` | `120000` | Request timeout |
| `MODELS_CSV` | `claude-opus-4-6,claude-opus-4-7,claude-opus-4-8,glm-5.2,gpt-5.5` | Static model fallback list |
| `WARMUP_INTERVAL_MS` | `180000` | WAF cookie refresh interval |
| `MAX_RETRIES` | `2` | Retry attempts on failure |
| `RETRY_DELAY_MS` | `1000` | Base retry delay (doubles per attempt) |
| `AR_API_KEY` | `""` | AgentRouter API key for auto model discovery (optional) |
| `DISCOVERY_INTERVAL_MS` | `600000` | How often to refresh model list from upstream (10 min) |

## Endpoints

| Path | Method | Description |
|------|--------|-------------|
| `/v1/models`, `/models` | GET | List available models |
| `/health`, `/api/health` | GET | Upstream status, WAF cookie state, circuit breaker |
| `/v1/messages`, `/v1/chat/completions` | POST | Proxied to upstream |
| `/messages` | POST | Rewritten to `/v1/messages` |

## Model Auto-Discovery

AgentRouter's available models change periodically without notice. The proxy can dynamically discover them:

1. Set `AR_API_KEY` to your AgentRouter API key
2. On startup and every 10 minutes (`DISCOVERY_INTERVAL_MS`), the proxy queries `agentrouter.org/v1/models`
3. If discovery succeeds, the returned model list replaces the static `MODELS_CSV`
4. The health endpoint shows `modelSource: "dynamic"` vs `"static"`

Without `AR_API_KEY`, the proxy uses the static `MODELS_CSV` list. No model is ever blocked — unknown model IDs in requests are forwarded as-is to the upstream.

## Known Limitations

| Issue | Cause | Workaround |
|-------|-------|------------|
| `NoChannelError` (503) | AgentRouter has no available channel for the model | Retry or switch to a different model |
| `content-blocked` (400) | Upstream content moderation | Rephrase the request |
| Alibaba ALB 503 | Transient infrastructure issue | Proxy retry logic handles it |
| `gpt-5.5` always 403 | Insufficient upstream quota | Omit from config |
| `glm-5.2` 429 | TPM rate limit | Wait and retry |

## License

MIT
