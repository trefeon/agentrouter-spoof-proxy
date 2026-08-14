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

The script auto-detects Docker/systemd and guides you through setup. It downloads
the prebuilt Go binary from GitHub Releases, or falls back to building from
source (Go 1.26+).

---

## Manual Install (3 steps)

```bash
git clone https://github.com/trefeon/agentrouter-spoof-proxy.git
cd agentrouter-spoof-proxy
cp .env.example .env

# Pick one:
docker compose up -d --build                      # Docker (recommended)
go build -trimpath -ldflags="-s -w" -o dist/proxy ./cmd/proxy   # then ./dist/proxy
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
| `cmd/proxy/main.go` | Thin entry: config validation, signal shutdown, `-healthcheck` flag |
| `internal/config/config.go` | Env config (caarlos0/env, 25 vars) + `Validate()` startup checks |
| `internal/proxy/handler.go` | Request handler: body limits (413/408), retry loop (invariant semantics documented), WAF/5xx/transport retries, SSE detection |
| `internal/proxy/sse.go` | SSE streaming pump: single-writer goroutines, keepalive, idle/stall watchdogs, format-aware EOM |
| `internal/proxy/thinkstrip.go` | Byte-level `<think>` stripping (pure, split-chunk + UTF-8 safe) |
| `internal/proxy/sseparse.go` | Carry-over SSE frame classifier (pure) |
| `internal/proxy/{eom,headers,retry,body,errors}.go` | Pure helpers (EOM strings, hop-by-hop filter, retry math, prompt injection, sentinel errors) |
| `internal/auth/spoof.go` | Claude Code header spoofing (Anthropic + generic sets) |
| `internal/auth/waf.go` | WAF cookie store + warmup (acw_tc/cdn_sec_tc rotation) |
| `internal/models/discovery.go` | Static/dynamic model discovery |
| `internal/models/health.go` | Auto-detect failing models, recovery probe loop |
| `internal/models/stats.go` | Model success metrics (mutex-guarded) |
| `internal/resilience/breaker.go` | Circuit breaker (atomics, final-5xx accounting) |
| `internal/server/server.go` | Routing (ServeMux), auth, warmup/discovery/probe schedulers, graceful shutdown |
| `testutil/mockupstream/mockupstream.go` | Scripted mock upstream (23 scenarios) for tests |
| `e2e/proxy_e2e_test.go` | 62 E2E tests (in-process proxy + mock upstream) |
| `e2e/verify_test.go` | 7 issue-verification regression tests |
| `deploy/Dockerfile` | Multi-stage Go → distroless static (~15-25MB), multi-arch |
| `deploy/agentrouter-proxy.service` | systemd unit (replaces PM2) |
| `docs/panduan-9router.md` | 🇮🇩 Indonesian tutorial (Bahasa Indonesia) |

---

## Build & Test Commands

```bash
go build ./...            # compile check
go vet ./...              # static analysis
go test ./...             # all tests (~222 across 8 packages)
go test ./e2e/            # E2E only
go test -race ./...       # race detector (needs cgo toolchain)
gofmt -l .                # formatting check
make check                # vet + lint + test (lint needs golangci-lint)
make cross                # cross-compile matrix → dist/
```

---

## Troubleshooting

| Problem | Fix |
|---------|-----|
| `wafCookie: false` | Wait 5s, warmup in progress |
| Model keeps getting 500 | **Auto model health** removes it from list → 9Router falls back |
| Docker: 9Router can't reach proxy | `docker network connect 9router-net agentrouter-proxy` |
| Windows: 9Router can't reach proxy | Use `host.docker.internal` in Base URL |
| SSE streams cut mid-answer | Check logs for `SLOW STREAM`/`IDLE TIMEOUT`; raise `SSE_CHUNK_TIMEOUT_MS`/`SSE_IDLE_TIMEOUT_MS` |
| Installer falls back to source build | No GitHub release published yet — publish one, or install Go 1.26+ |
