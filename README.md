# AgentRouter Spoof Proxy

A lightweight Node.js reverse proxy that injects Claude Code spoof headers and maintains WAF cookies to bypass AgentRouter restrictions. Zero dependencies, single-file, ~730 lines, 120MB Docker image.

## Architecture

```
Any OpenAI/Anthropic client → agentrouter-proxy:8318 → agentrouter.org (upstream)
```

The proxy:
- Rewrites `/messages` → `/v1/messages` (Anthropic API format)
- Injects all spoof headers (`User-Agent`, `X-Stainless-*`, `Anthropic-Beta`, etc.)
- Maintains `acw_tc` WAF cookies via periodic warmup
- Pipes SSE streaming responses with backpressure handling
- Retries on timeouts/5xx with exponential backoff
- Circuit breaker on consecutive failures
- Graceful shutdown with active stream draining
- Configurable system prompt injection (`INJECT_SYSTEM_PROMPT`) for content filter bypass

## Quick Start

```bash
git clone https://github.com/trefeon/agentrouter-spoof-proxy.git
cd agentrouter-spoof-proxy
docker compose up -d --build
```

Verify:
```bash
curl http://localhost:8318/health
```

```json
{
  "ok": true,
  "upstream": "agentrouter.org:443",
  "modelSource": "static",
  "availableModels": 5,
  "activeStreams": 0,
  "wafCookie": true,
  "circuitOpen": false
}
```

If `wafCookie` is `false`, wait a few seconds and retry — the WAF warmup runs on startup.

## Configuration

Copy `.env.example` to `.env` and edit as needed:

```bash
cp .env.example .env
```

All settings have sensible defaults. Only set what you need to change.

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN_PORT` | `8318` | Proxy listen port |
| `TARGET_HOST` | `agentrouter.org` | Upstream hostname |
| `TARGET_PORT` | `443` | Upstream port |
| `REQUEST_TIMEOUT_MS` | `300000` | Max time to wait for upstream first byte (ms). Raised from 120s for large-context tasks. |
| `SSE_IDLE_TIMEOUT_MS` | `600000` | SSE idle timeout: if no streaming data arrives within this window, stream is terminated (ms) |
| `MODELS_CSV` | `claude-opus-4-6,...` | Static model fallback list |
| `WARMUP_INTERVAL_MS` | `180000` | WAF cookie refresh interval (ms) |
| `MAX_RETRIES` | `2` | Retry attempts on failure |
| `RETRY_DELAY_MS` | `1000` | Base retry delay, doubles per attempt (ms) |
| `AR_API_KEY` | _(empty)_ | AgentRouter API key for auto model discovery |
| `DISCOVERY_INTERVAL_MS` | `600000` | Model list refresh interval (ms) |
| `INJECT_SYSTEM_PROMPT` | _(empty)_ | System prompt injected into every request (Anthropic `system` field / OpenAI `system` message). Empty = disabled. |

## Endpoints

| Path | Method | Description |
|------|--------|-------------|
| `/health`, `/api/health` | GET | Status, WAF cookie state, circuit breaker, active streams |
| `/v1/models`, `/models` | GET | List available models |
| `/v1/messages` | POST | Proxied to upstream (Anthropic format) |
| `/messages` | POST | Rewritten to `/v1/messages`, then proxied |
| `/v1/chat/completions` | POST | Proxied to upstream (pass-through) |

## Usage

### Standalone (direct requests)

Send Anthropic-format requests directly:

```bash
curl http://localhost:8318/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -d '{
    "model": "claude-opus-4-8",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'
```

### With a router (9Router, LiteLLM, etc.)

If your router runs in Docker, both containers need to be on the same network:

```bash
# Option A: copy and edit the override example
cp docker-compose.override.yml.example docker-compose.override.yml
# Edit docker-compose.override.yml to set your network name
docker compose up -d --build

# Option B: connect manually
docker network connect YOUR_NETWORK agentrouter-proxy
```

Then configure the router to use `http://agentrouter-proxy:8318` as the upstream URL. The proxy is reachable by container name via Docker DNS.

### With opencode

Add a provider to your `opencode.jsonc`:

```jsonc
"provider": {
  "your-provider-name": {
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

If opencode is on a different machine, replace `localhost` with the host's LAN IP.

## Model Auto-Discovery

Set `AR_API_KEY` in your `.env` to enable dynamic model listing:

```bash
AR_API_KEY=your-agentrouter-api-key
```

The proxy queries `agentrouter.org/v1/models` on startup and every 10 minutes. The health endpoint shows `modelSource: "dynamic"` when active.

Without `AR_API_KEY`, the static `MODELS_CSV` list is used. Unknown model IDs in requests are always forwarded as-is.

## Prompt Injection

Set `INJECT_SYSTEM_PROMPT` in your `.env` to inject a system prompt into every proxied request:

```bash
INJECT_SYSTEM_PROMPT=Your system prompt here
```

The prompt is injected differently depending on the API format:

| Format | Path | Injection target |
|--------|------|-----------------|
| Anthropic | `/v1/messages` | `system` field (string or content block array) |
| OpenAI | `/v1/chat/completions` | System message prepended to `messages` array |

Recreate the container after changing the variable:

```bash
docker compose up -d --build
```

> **Note:** The upstream (agentrouter.org) enforces its own server-side content policies. The injected prompt may not bypass upstream filtering.

## Model Reference

| Model | Context | Output | Vision | Cost (in/out $/MTok) |
|-------|---------|--------|--------|---------------------|
| `claude-opus-4-6` | 1M | 128k | yes (1568px) | 5 / 25 |
| `claude-opus-4-7` | 1M | 128k | yes (2576px) | 5 / 25 |
| `claude-opus-4-8` | 1M | 128k | yes (2576px) | 5 / 25 |
| `glm-5.2` | 1M | 131k | untested | 1.4 / 4.4 |

Opus 4.8 uses ~35% fewer output tokens than 4.7 at the same effort level.

## Running Tests

Tests use Node.js 22+ built-in test runner (zero dependencies):

```bash
node --test tests/proxy.test.mjs
```

## Known Limitations

| Issue | Cause | Workaround |
|-------|-------|------------|
| `NoChannelError` (503) | No available upstream channel | Retry or switch model |
| `content-blocked` (400) | Upstream content moderation | Rephrase request |
| Alibaba ALB 503 | Transient WAF issue | Proxy retry handles it |
| `gpt-5.5` always 403 | Insufficient upstream quota | Omit from config |
| `glm-5.2` 429 | TPM rate limit | Wait and retry |

## License

MIT
