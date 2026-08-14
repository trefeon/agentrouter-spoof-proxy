# Go Migration Plan — agentrouter-spoof-proxy

> **STATUS: EXECUTED ✅** — This migration is complete (commit `3418ea1`, Aug 2026).
> The Node.js codebase was removed in commit `4a76d62`; the repository is now
> 100% Go. This document is retained as the historical port map / behavioral
> contract reference. See `README.md` and `AGENTS.md` for the current facts.

Full rewrite of the Node.js proxy in **Go**. Goal: a *fast* cross-platform proxy —
small binary, tiny Docker image, long-lived SSE streaming without framework
overhead. Current code: zero-dep Node ESM, 13 modules + thin entry, 194 tests
(132 unit + 55 E2E + 7 issue-verification).

This document is the port map. **Behavior is the spec** — the existing test
suite is ported first and kept green as the regression gate. We are porting
semantics, not code.

---

## 1. Target stack (verified Aug 2026)

| Concern | Choice | Version / note |
|---|---|---|
| Language | Go | **1.26.x** (current stable; `go 1.25` in go.mod) |
| HTTP server | stdlib `net/http` | fasthttp rejected (HTTP/1.1-only, manual mem mgmt, its own README says net/http wins for most cases) |
| Routing | stdlib `http.ServeMux` (1.22+ patterns) | 6 routes; no framework needed |
| Upstream client | stdlib `net/http` `Client` + `Transport` | mirrors Node agent (keep-alive, maxConns 64, idle 16) |
| Config | `caarlos0/env` v11 | struct tags, typed parsing, `required`/`notEmpty`; zero deps |
| Logging | stdlib `log/slog` | JSON handler + request IDs; `slog.Attr`/`LogAttrs` |
| Tests | stdlib `testing` + `httptest` + `testing/synctest` (1.25+) | real sockets; `google/go-cmp` for deep equality; `testify` assert only (no mock/suite) |
| Lint | `golangci-lint` v2.x | one binary + `.golangci.yml` |
| Docker | multi-stage → `distroless/static-debian13:nonroot` | `CGO_ENABLED=0`, `-ldflags="-s -w" -trimpath`, ~15–25MB |
| Process mgmt | systemd (bare metal) / `restart: unless-stopped` (Docker) | PM2 is Node-only and goes away |
| Cross-compile | `GOOS/GOARCH` env vars | pure Go; trivial from Windows |

**Deliberately excluded**: fasthttp, Hono/Fastify-class frameworks, Bun, gRPC,
koanf (overkill for env-only), zerolog (slog is fine at proxy scale — the
slog↔zerolog bridge kills zerolog's perf advantage, don't mix).

---

## 2. Repository layout

```
agentrouter-spoof-proxy/
├── cmd/proxy/main.go            ← proxy.mjs (entry, server, schedulers, shutdown)
├── internal/
│   ├── config/config.go         ← src/config.mjs (env struct + Validate)
│   ├── logging/logger.go        ← src/logger.mjs (slog wrapper, debug level)
│   ├── auth/spoof.go            ← src/auth/spoof.mjs (header maps)
│   ├── auth/waf.go              ← src/auth/waf.mjs (cookie store + warmup)
│   ├── models/discovery.go      ← src/models/discovery.mjs
│   ├── models/health.go         ← src/models/health.mjs (probe loop)
│   ├── models/stats.go          ← src/models/stats.mjs
│   ├── resilience/breaker.go    ← src/resilience/circuit-breaker.mjs
│   └── proxy/
│       ├── handler.go           ← src/proxy/handler.mjs
│       ├── sse.go               ← src/proxy/stream.mjs (pump, keepalive, watchdogs)
│       ├── thinkstrip.go        ← byte-level <think> stripping (pure)
│       ├── sseparse.go          ← carry-over SSE frame classifier (pure)
│       ├── eom.go               ← utils: eomTail/sseErrorFrame/abnormalFinish (pure)
│       ├── headers.go           ← utils: filterHeaders, HOP_BY_HOP, cookie merge
│       ├── retry.go             ← utils: isRetryable, getRetryDelay, adaptive timeout
│       ├── body.go              ← utils: injectPrompt, summarizeRequest, empty-output
│       └── errors.go            ← src/errors.mjs + src/status-code.mjs (typed)
├── testutil/
│   └── mockupstream.go          ← tests/mock-upstream.mjs
├── e2e/
│   ├── proxy_e2e_test.go        ← tests/proxy.test.mjs (55 tests)
│   └── verify_test.go           ← tests/verify-issues.test.mjs (7 tests)
├── Dockerfile, docker-compose.yml, .env.example   ← env names unchanged
└── Makefile                     ← build / test / lint / cross-compile / image
```

---

## 3. Module-by-module port map

### 3.1 Entry & server — `proxy.mjs` → `cmd/proxy/main.go`

| Node behavior | Go equivalent |
|---|---|
| `validateConfig()` fail-fast before listen | `config.Load()` + `Validate()`; `log.Fatal` on error |
| `resolve4(TARGET_HOST)` log at startup | `net.DefaultResolver.LookupHost` (background, non-fatal) |
| `setInterval(warmup)` | `time.Ticker` goroutine + stop channel |
| `fetchModels()` + discovery interval (only if `AR_API_KEY`) | same pattern |
| `startProbeLoop(...)` | goroutine + `time.Ticker` (60s) |
| Route dispatch (health/models/proxy allowlist/401/405) | `http.ServeMux` patterns + manual allowlist |
| `server.headersTimeout = 30000` | `http.Server{ReadHeaderTimeout: 30s}` |
| `server.requestTimeout = 0` | **do not set** `WriteTimeout` (kills SSE); set `IdleTimeout` |
| `rejectLocally` drain body (keep-alive hygiene) | `io.Copy(io.Discard, r.Body)` before responding, or `r.Body.Close()` |
| Graceful shutdown: drain streams, 15s force-exit | `server.Shutdown(ctx 15s)` + `sync.WaitGroup` on active streams + cancel upstream contexts |
| `uncaughtException`/`unhandledRejection` handlers | `recover()` middleware + `server.ErrorLog` |

**Route table (identical surface):**

```
GET  /health, /api/health     → JSON status (no auth)
GET  /v1/models, /models      → healthy model list (no auth)
POST /v1/messages             → proxied (Anthropic format)
POST /messages                → rewrite to /v1/messages
POST /v1/chat/completions     → proxied (OpenAI format)
anything else                 → 404; wrong method → 405; auth fail → 401
```

ServeMux note: Go 1.26 changed trailing-slash redirects 301→307. We register
exact paths only (no sub-routes), so this is irrelevant — but the ported E2E
tests must not assume redirect behavior.

### 3.2 Config — `src/config.mjs` → `internal/config/config.go`

`caarlos0/env` struct with **identical env names** so `.env` and
`docker-compose.yml` work unchanged. All 25 vars (see §6 parity table).
`Validate()` ported check-for-check from the 16-entry validation table (port
range, positive ints, `"true"/"false"` booleans, `http|https` protocol,
non-empty listen address).

Upstream pool (Node `Agent{keepAlive, maxSockets:64, maxFreeSockets:16, lifo}`)
→ `http.Transport{MaxConnsPerHost: 64, MaxIdleConnsPerHost: 16,
IdleConnTimeout: 90s, ForceAttemptHTTP2: false}`. TLS `rejectUnauthorized: true`
is Go's default.

### 3.3 Pure helpers — `src/utils.mjs` → `internal/proxy/*.go`

Direct 1:1 ports, all pure and unit-tested:

- `eom.go` — `eomTail`, `sseErrorFrame`, `abnormalFinish` (exact strings, both
  formats). Keep as byte constants, not templates.
- `headers.go` — `HOP_BY_HOP` set, `filterHeaders` (Go: build `http.Header`),
  `normalizeSetCookie` (Go `Add` handles multi Set-Cookie natively),
  `rewritePath` (`/messages` → `/v1/messages`), `redactSensitive` (regex
  `sk[-_][A-Za-z0-9_-]+`).
- `retry.go` — `isRetryable(status, msg, retryOn5xx)` (transport-error keywords:
  socket hang up / timeout / ECONNRESET / ETIMEDOUT / ENETUNREACH),
  `getRetryDelay(attempt, base) = base << attempt`, `getResponseTimeout(bodyBytes)`.
- `body.go` — `injectPrompt` (both `/v1/messages` system rewrite and
  `/v1/chat/completions` messages.unshift), `summarizeRequest` (model/stream/
  max_tokens/messageCount), `responseHasEmptyOutput` (Anthropic content array /
  OpenAI choices[0].message.content).
- `wafmarkers.go` — `isWafBlock` (403/405 body markers: alicdn, block_message,
  renderData, waf.js).
- `safetoken.go` — `safeTokenEqual`: length check + `crypto/subtle.ConstantTimeCompare`.

`MAX_BODY_SIZE = 20MB`, `SSE_EOM`, `SSE_DONE` → package constants.

### 3.4 Errors + status — `src/errors.mjs` + `src/status-code.mjs` → `internal/proxy/errors.go`

```go
var ErrTimeout = errors.New("EUPSTREAM_TIMEOUT") // → 504
    ErrUpstream = ...                            // → 502
    ErrCircuit  = ...                            // → 503
    ErrInternal = ...                            // → 500
```
`sentinel errors.Is/As` replaces the `isOurError` symbol marker. Keep the
`{error: {code, message, type: "proxy_error"}}` JSON shape byte-compatible with
9Router.

### 3.5 Spoof headers — `src/auth/spoof.mjs` → `internal/auth/spoof.go`

Three exported maps, byte-identical values: `AnthropicSpoofHeaders`,
`GenericSpoofHeaders`, `SpoofHeaders` (merged). Go canonicalizes header names on
write (`Anthropic-Version` stays `Anthropic-Version`) — no lowercase-overwrite
hazard the Node code had to defend against, but keep the same
client-supplied-`anthropic-version`-ignored rule anyway (defense in depth).

### 3.6 WAF cookies — `src/auth/waf.mjs` → `internal/auth/waf.go`

Shared state becomes a **thread-safe store**:

```go
type Store struct { mu sync.RWMutex; cookies []string }
```

- `extractWafCookies` — same name allowlist (`acw_tc`, `acw_sc__v2/3`,
  `cdn_sec_tc`), skip empty values (empty cookie = failed challenge, worse than
  none). Go: `resp.Cookies()` + `Cookie.Valid()`.
- `mergeWafCookies` — keyed by cookie name, fresh wins, preserves unrelated
  names (pure, unit-tested).
- `captureWafCookies` — called on every upstream response (rotated cookies
  picked up immediately, not only on warmup).
- `warmup` — goroutine: GET `/` with the browser header set, `agent: false`
  equivalent = a **fresh** `http.Client{Timeout: 10s}` (no shared pool), 3
  attempts with 1s/2s backoff, merge-don't-replace. Returns early on success.

### 3.7 Models — discovery / health / stats → `internal/models/*.go`

- `discovery.go` — static CSV list + optional dynamic fetch (GET /v1/models with
  `AR_API_KEY`), 15s timeout, fall back to static on any error. `modelSource`
  guarded by mutex.
- `health.go` — `failedUntil` + `failCounts` maps → `sync.RWMutex`-guarded.
  `markModelFailed` (5xx, BACKOFF ladder 30s/1m/2m/5m/10m), `markModelExhausted`
  (429, 120s), `markModelDegraded` (60s), recovery probe goroutine (60s ticker,
  8s probe timeout, probe = POST /v1/messages `max_tokens:1` with spoof headers
  + WAF cookie).
- `stats.go` — per-model stats map (requests/successes/failures/emptyOutputs/
  slowResponses/wafBlocks/rateLimits/upstreamErrors/totalMs/maxMs/totalChunks/
  lastStatus/lastError/lastSeen) + `avgMs`/`avgChunks` derivation + sort by
  lastSeen desc. Mutex-guarded; `recordModelStart`/`recordModelResult` called
  from the handler.

### 3.8 Circuit breaker — `src/resilience/circuit-breaker.mjs` → `internal/resilience/breaker.go`

Atomics, not mutexes:

```go
var consecutiveFails atomic.Int64
var openUntilMs atomic.Int64
```

Open after 5 consecutive final 5xx/transport failures; cooldown
`min(60s << (fails-5), 600s)`; `recordSuccess` resets the run; final-5xx
accounting rules preserved (429 never opens the circuit, 4xx neither fails nor
resets).

### 3.9 Request handler — `src/proxy/handler.mjs` → `internal/proxy/handler.go`

The critical port. The Node state machine's invariant flags map to Go
primitives:

| Node invariant | Go equivalent |
|---|---|
| `proxyDone` (finishProxy once) | `sync.Once` / `atomic.Bool` + ctx; active-stream `WaitGroup.Done` once |
| `errorHandled` per attempt | **disappears** — Go's sequential retry loop returns one error per attempt; no multiple-listener double-fire possible |
| `upstreamResponded` disarms adaptive timeout | `context.WithCancel` + `time.AfterFunc(adaptive)` → `cancel()` on headers arrival (mirrors `responseTimer`) |
| `req.on("close")` → destroy upstream | `r.Context().Done()` → cancel upstream request (server cancels ctx on client disconnect) |
| `req.on("error")` | HTTP/2/1 read error on `r.Body` |

Flow, ported step for step:

1. **Body**: early `Content-Length > 20MB` → 413 before buffering; else buffer
   with `http.MaxBytesReader` + 60s upload deadline (custom: `time.AfterFunc`
   that aborts the read and writes 408); oversized → 413, then drain rest of
   body (don't reset socket).
2. Parse body once → `summarizeRequest` (model, stream flag), `recordModelStart`.
3. `rewritePath` → `streamFormat` (`openai` if `/v1/chat/completions` else
   `anthropic`); pick spoof header set; add client `Authorization`/`x-api-key`;
   add WAF cookie if present; `injectPrompt`.
4. Circuit open → 503 JSON immediately.
5. `streams.count++` (atomic) → enters retry loop:

```
for attempt := 0; ; attempt++ {
    resp, err := doUpstream(ctx, ...)   // one call, one error — no listener races
    if err → if retryable && attempt < MAX_RETRIES → backoff, continue
            else → recordFailure, markModelFailed, respond (504 timeout / 502)
    // WAF 403/405 on attempt 0 → warmup(), refresh cookie, retry once
    // 5xx retry (if RETRY_ON_5XX) while attempt < MAX_RETRIES
    // 429 → markModelExhausted
    // 5xx → markModelFailed + recordFailure; 2xx/3xx → recordSuccess
    captureWafCookies(resp)              // every response
    if SSE (content-type OR status 200 && stream==true) → pumpSse(...)
    else → copy body, filter headers, write
    break
}
```

6. Every response is recorded via `recordModelResult` with the same
   statusCode/durationMs/chunks/error/emptyOutput/wafBlock fields.

**Client response writes must not use `io.Copy` blindly**: headers must be
filtered first (strip hop-by-hop), `set-cookie` kept as array, then
`w.WriteHeader(status)` + streaming body with flush (see §3.10).

### 3.10 SSE pump — `src/proxy/stream.mjs` → `internal/proxy/sse.go`

Port of `pipeSse` as a function `pumpSse(ctx, w, upstreamResp, opts) -> Result`.
Structure:

- **Read loop** (one goroutine): `bufio.Reader` on upstream body → for each
  chunk: run `thinkstrip` + `openaiPending` frame filter → `w.Write(chunk)` →
  `http.NewResponseController(w).Flush()` → re-arm chunk/idle timers.
  **Backpressure is automatic**: `w.Write` blocks when the client's buffer is
  full, which stalls the upstream read — the Node pause/resume dance is
  unnecessary.
- **Keepalive** (goroutine + `time.Ticker`, 10s): write `:\n\n` while
  stream-alive; skip on backpressure.
- **Idle watchdog** (`SSE_IDLE_TIMEOUT_MS`, default 10m): `time.AfterFunc` —
  terminates a genuinely silent stream → error frame + abnormal-finish EOM +
  destroy upstream. `time.After` in a `select` is the idiomatic equivalent.
- **Chunk stall watchdog** (`SSE_CHUNK_TIMEOUT_MS`, default 30s): re-arms on
  every chunk; if it fires while the connection is alive → log SLOW STREAM,
  keep alive, re-arm (never cut a live stream); if dead → terminate.
- **`isStreamAlive`** → `ctx.Err() == nil && !wClosed && upstream body alive`
  (client ctx + `ResponseController` status).
- **Format-aware EOM**: `eom.go` strings — inject on abnormal end only if
  `!sawMessageStop`; error frame first, then `abnormalFinish`.
- **Terminal detection**: `sseparse.go` carry-over frame classifier detects
  `event: message_stop` / `data: [DONE]` across chunk boundaries; comment-only
  (`:`) and `event: ping` lines are noise, not data (`sawDataEvent` semantics).
- **Callbacks** → a `Result` struct returned on finish + `onDegrade` /
  `onMessageStop` closures or an interface; stats recorder called once.
- **Upstream close paths** (`end`/`error`/`close`): preserve the three distinct
  terminal reasons (`upstream_ended_mid_frame` when `thinkBuf` non-empty →
  502; `upstream_closed` → 502; clean end → flush pending buffers, check
  `empty_sse` degrade, 200 with `emptyOutput` flag).

**WriteTimeout is the #1 SSE killer** — never set `http.Server.WriteTimeout`.
Set only `ReadHeaderTimeout` + `IdleTimeout`.

### 3.11 `thinkstrip.go` — byte-level `<think>` stripping (pure, new module)

`stream.mjs` lines 251–300 port verbatim as a pure `[]byte` function with
explicit state (`insideThinkTag`, `thinkBuf`, `tagPrefixPending`), so it can be
table-tested without sockets: chunk-in → (forward bytes, held prefix, state).
Preserve: multi-byte UTF-8 never corrupted (no decode/re-encode), split-tag
detection via up-to-6-byte trailing holdback, `</think>`-terminated spans,
upstream-end-mid-span → 502 path.

---

## 4. Test porting strategy (194 tests as the spec)

| Current | Go port | Notes |
|---|---|---|
| `tests/unit/utils.test.mjs` (320) | `internal/proxy/{eom,headers,retry,body,thinkstrip,sseparse}_test.go` | table-driven; pure functions |
| `tests/unit/stream.test.mjs` (420) | `internal/proxy/sse_test.go` | **`testing/synctest` (Go 1.25+)** replaces real-sleep waits — deterministic fake clocks for keepalive/idle/stall timers; real `net.Pipe`/`httptest` for socket-level framing |
| `tests/unit/errors.test.mjs` (28) | `internal/proxy/errors_test.go` | sentinel errors + status mapping |
| `tests/unit/resilience.test.mjs` (65) | `internal/resilience/breaker_test.go` | atomic state, cooldown ladder |
| `tests/unit/models.test.mjs` (98) | `internal/models/*_test.go` | health/stats/discovery |
| `tests/unit/waf-traffic.test.mjs` (49) | `internal/auth/waf_test.go` | cookie extract/merge/capture |
| `tests/mock-upstream.mjs` (196) | `testutil/mockupstream.go` | `httptest.NewServer` with scripted SSE/error/WAF responses |
| `tests/proxy.test.mjs` (55 E2E) | `e2e/proxy_e2e_test.go` | real proxy on random port + mock upstream; abrupt disconnect via raw `net.Dial` |
| `tests/verify-issues.test.mjs` (7) | `e2e/verify_test.go` | the 7 historical regression scenarios |

Gate: `go test ./...` green + `golangci-lint run` clean at every phase. Use
`go-cmp` for deep struct compares; `testify` only for `assert`/`require`
convenience, never mock/suite.

---

## 5. Config parity table (env names unchanged)

All names/defaults identical to `.env.example` — docker-compose and install
scripts keep working. `caarlos0/env` handles `parseInt`/`"true"` typing;
`Validate()` reproduces the 16 checks.

| Env var | Default | Go type | Validation |
|---|---|---|---|
| LISTEN_PORT | 8318 | int | 1–65535 |
| LISTEN_ADDRESS | 127.0.0.1 | string | non-empty |
| TARGET_PROTOCOL | https | enum | http\|https |
| TARGET_HOST | agentrouter.org | string | non-empty |
| TARGET_PORT | 443 | int | 1–65535 |
| REQUEST_TIMEOUT_MS | 300000 | time.Duration | > 0 |
| RESPONSE_TIMEOUT_MS | 30000 | time.Duration | > 0 |
| SSE_IDLE_TIMEOUT_MS | 600000 | time.Duration | > 0 |
| SSE_CHUNK_TIMEOUT_MS | 30000 | time.Duration | > 0 |
| BODY_UPLOAD_TIMEOUT_MS | 60000 | time.Duration | > 0 |
| SLOW_RESPONSE_MS | 30000 | time.Duration | > 0 |
| WARMUP_INTERVAL_MS | 180000 | time.Duration | > 0 |
| DISCOVERY_INTERVAL_MS | 600000 | time.Duration | > 0 |
| MAX_RETRIES | 2 | int | ≥ 0 |
| RETRY_DELAY_MS | 1000 | time.Duration | ≥ 0 |
| RETRY_ON_5XX | false | bool | "true"/"false" |
| STRIP_THINKING_TAGS | true | bool | "true"/"false" |
| MODELS_CSV | gpt-5.6-sol,… | []string (csv) | non-empty |
| AR_API_KEY | "" | string | optional |
| INJECT_SYSTEM_PROMPT | "" | string | optional |
| PROXY_AUTH_TOKEN | "" | string | optional |
| LOG_LEVEL | info | string | info\|debug |

---

## 6. Deployment changes

### Dockerfile (multi-stage, multi-arch)

```dockerfile
# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/proxy ./cmd/proxy

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/proxy /proxy
EXPOSE 8318
ENTRYPOINT ["/proxy"]
```

Build from Windows: `docker buildx build --platform linux/amd64,linux/arm64
--push .` (native cross-compile via TARGETOS/TARGETARCH — no QEMU emulation).
Image lands at **~15–25MB** vs the current 120MB.

**Healthcheck**: distroless has no shell/`wget`. Add a `-healthcheck` flag mode
to the binary (GET /health, exit 0/1) used by
`HEALTHCHECK CMD ["/proxy", "-healthcheck"]` — zero extra files in the image.

### Process management

- **Linux bare metal**: systemd unit `Type=simple`, `Restart=always`,
  `EnvironmentFile=/etc/agentrouter-proxy.env`. (PM2 is Node-only; removed.)
- **Docker**: existing `restart: unless-stopped` unchanged.
- **Windows dev**: `go run ./cmd/proxy`; production Windows (optional):
  `kardianos/service` to embed systemd + Windows-service + launchd support in
  the binary, or wrap with NSSM/WinSW.
- **Windows dev server quirk**: `localhost` may resolve `::1`; bind
  `127.0.0.1` explicitly (LISTEN_ADDRESS default already does).

### Cross-compile matrix (from Windows host)

```bash
GOOS=linux  GOARCH=amd64 go build -o dist/proxy-linux-amd64  ./cmd/proxy
GOOS=linux  GOARCH=arm64 go build -o dist/proxy-linux-arm64  ./cmd/proxy
GOOS=darwin GOARCH=arm64 go build -o dist/proxy-darwin-arm64 ./cmd/proxy
GOOS=windows GOARCH=amd64 go build -o dist/proxy.exe         ./cmd/proxy
```

Notes: `windows/arm` (32-bit) removed in Go 1.26; Go 1.27 requires macOS 13+;
pure Go (no cgo) cross-compiles without toolchains.

---

## 7. Phased execution plan

Each phase ends green: `go test ./...` + `golangci-lint run` + (phase 3+)
`go vet ./...`. Ported tests land in the same phase as their target module.

| Phase | Deliverable | Gate |
|---|---|---|
| **0. Scaffold** | `go.mod`, `cmd/proxy` skeleton, `Makefile`, `.golangci.yml`, Dockerfile, config package | `go build ./...`, lint clean |
| **1. Pure core** | config, errors, eom/headers/retry/body/thinkstrip/sseparse + their unit tests | utils/errors/config unit tests green |
| **2. SSE pump** | `sse.go` with synctest-driven timers + keepalive + EOM injection | stream.test.mjs equivalents green |
| **3. Handler + server** | `handler.go`, routing, WAF capture, auth, breaker, stats wiring; mock upstream; E2E harness | 55 E2E equivalents green |
| **4. Schedulers + shutdown** | warmup/discovery/probe loops, graceful drain, signal handling | verify-issues equivalents green |
| **5. Deploy** | Dockerfile, systemd unit, cross-compile matrix, install scripts | buildx multi-arch image boots; /health parity |
| **6. Parity run** | Old + new proxy side by side against mock upstream; diff /health payloads, stream bytes, error shapes | byte-identical for the 55 E2E scenarios |

---

## 8. Risks & mitigations

| Risk | Mitigation |
|---|---|
| **Concurrency bugs** (Node is single-threaded; Go is not) | Mutex/atomic discipline listed per module (§3); race detector on in tests: `go test -race` |
| **`http.Server.WriteTimeout` silently kills SSE** | Never set it; use `ReadHeaderTimeout` + `IdleTimeout` (Tyk's SSE tests as reference) |
| ResponseWriter wrapper breaking streaming | We don't wrap; if middleware added, must implement `Unwrap()` (go#64045) |
| `httputil.ReverseProxy` limitations | Not used — manual copy-with-flush because of custom pumps; ReverseProxy only as a future passthrough baseline |
| ServeMux 301→307 trailing-slash change (Go 1.26) | Exact-path routes only; E2E tests must not assume redirects |
| `http.Client.Timeout` covers body reads — would kill SSE | Don't set Client.Timeout; use context deadline for headers + pump's own idle/chunk timers for body |
| Backpressure regression (Node paused upstream; Go writes block) | Loop `read → write → flush` naturally throttles upstream; verify with a slow-reader test client |
| distroless has no shell/wget | `-healthcheck` mode in binary |
| golangci-lint v1→v2 config format | Fresh `.golangci.yml` for v2.x at Phase 0 |
| Behavior drift in error JSON shapes | 9Router-compat fixtures from the old test suite ported verbatim |

---

## 9. What we deliberately do NOT port

- `src/logger.mjs` console-string logging → slog structured (log *shape* is not
  a contract; messages keep their `WARMUP →`, `CIRCUIT OPEN for`, `MODEL
  UNHEALTHY:` prefixes for grep parity)
- Node `Agent` scheduling `lifo` (no Go equivalent, irrelevant at these
  volumes)
- `resolve4` startup DNS log (kept as LookupHost, informational only)
- PM2-specific install script paths (replaced by systemd unit)
