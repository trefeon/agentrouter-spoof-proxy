# Task Checklist: Proxy Reliability and Security Hardening

Use this file as the execution checklist for [`PLAN.md`](PLAN.md). Check items only after the implementation and its verification have completed. Keep each task focused; do not combine unrelated fixes in one commit.

## Phase 1 — Failure Lifecycle Foundation

### Task 1 — Adaptive timeout regression test

- [x] Add a mock-upstream scenario that accepts a request and withholds response headers (`no_response_headers`).
- [x] Configure `RESPONSE_TIMEOUT_MS` below `REQUEST_TIMEOUT_MS`.
- [x] Assert the client receives 504 within the response timeout.
- [x] Assert `activeStreams` returns to its baseline.

Verification: focused E2E test, then full E2E suite. ✅ 3 tests in `tests/proxy.test.mjs` (adaptive response timeout).

### Task 2 — Fix adaptive response timeout

- [x] Route adaptive timeout directly through the existing error/retry handler.
- [x] Ensure the timeout timer is cleared on every terminal path.
- [x] Ensure late upstream events cannot double-complete the request.
- [x] Add retry and exhausted-retry assertions.

Verification: timeout tests, `npm run test:unit`, `npm run lint`, `npm run check`. ✅

### Task 3 — Correct circuit-breaker accounting

- [x] Decide and document 5xx, 4xx, and 429 breaker semantics.
- [x] Count final 5xx failure sequences consistently.
- [x] Preserve reset-on-success behavior.
- [x] Add tests for 5xx opening and success resetting the circuit.

**Decisions:** final 5xx → `recordFailure()`; 4xx → neither fail nor reset; 429 → per-model lock only (never opens the global circuit, upstream is reachable); 2xx/3xx → `recordSuccess()`. ✅ 5 tests in `tests/proxy.test.mjs` (circuit breaker accounting).

### Checkpoint 1

- [x] Timeout requests always terminate.
- [x] No active-stream leak remains after timeout/retry paths.
- [x] 5xx circuit behavior is covered and predictable.
- [x] Existing WAF, retry, connection-error, and streaming tests pass.

## Phase 2 — Request Boundary and Deployment Security

### Task 4 — Route allowlist

- [x] Allow only the documented proxy API routes.
- [x] Preserve `/health`, `/api/health`, `/models`, and `/v1/models`.
- [x] Return local 404 for unknown paths.
- [x] Add tests proving unknown routes never reach upstream.

Unsupported methods on proxy routes → local `405`. Query strings preserved. ✅ 3 tests (route allowlist).

### Task 5 — Inbound proxy authentication

- [x] Decide mandatory versus compatibility-mode authentication.
- [x] Add configuration and constant-time token comparison.
- [x] Reject missing/invalid credentials before upstream work.
- [x] Document host and Docker/9Router setup.
- [x] Add valid/invalid/missing credential tests.

**Decision:** compatibility mode — `PROXY_AUTH_TOKEN` is optional; when unset, no auth (safe because `LISTEN_ADDRESS` defaults to `127.0.0.1`). When set, `Authorization: Bearer <token>` or `X-Proxy-Token: <token>` required on proxied routes; `/health` and `/v1/models` stay open. ✅ 5 tests (inbound proxy authentication).

### Task 6 — Correct 413 handling

- [x] Check `Content-Length` before buffering when available.
- [x] Enforce the limit for chunked requests.
- [x] Send 413 before closing the connection.
- [x] Ensure oversized bodies never reach upstream.
- [x] Add fixed-length and chunked oversized-body tests.

Oversized bodies are drained (not buffered/forwarded) and the socket is closed after a bounded grace period so the client reads a clean 413 (no `ECONNRESET`). ✅ 2 tests (oversized request bodies).

### Task 7 — Upload timeout and config validation

- [x] Add an upload idle/body deadline.
- [x] Validate port, timeout, retry, and interval environment values.
- [x] Define valid disabled/zero settings.
- [x] Fail startup clearly and non-zero on invalid configuration.
- [x] Add valid/invalid configuration tests.

`BODY_UPLOAD_TIMEOUT_MS` (default 60000) → stalled uploads rejected with `408`. `validateConfig()` runs before the server starts; invalid env exits `1` with an actionable message. Zero/disabled settings: `MAX_RETRIES=0` and `RETRY_DELAY_MS=0` are valid; timeouts/ports must be positive. ✅ 1 E2E (bounded uploads) + 3 unit (config validation).

### Checkpoint 2

- [x] Unknown routes are rejected locally.
- [x] Remote exposure has an explicit authentication story.
- [x] Oversized and stalled uploads terminate predictably.
- [x] Existing 9Router routes remain compatible.

## Phase 3 — SSE Correctness

### Task 8 — Keepalive timer lifecycle

- [x] Separate idle timer clearing from keepalive timer clearing.
- [x] Confirm keepalives continue after real data.
- [x] Stop keepalives on completion, error, timeout, and disconnect.
- [x] Add a unit test spanning the keepalive interval.

`keepaliveIntervalMs` is injectable for tests (default 10s). ✅ stream unit tests.

### Task 9 — Chunk-independent SSE framing

- [x] Add carry-over state for partial SSE events.
- [x] Detect short valid events without a size threshold.
- [x] Detect `message_stop` split across chunks.
- [x] Ignore comment-only keepalives as model data.
- [x] Preserve single EOM injection on abnormal termination.

**Superseded decision (user-driven):** the stall watchdog now *extends* while the upstream connection is still open (`isStreamAlive()`), so long thinking/tool pauses never cut a live stream — instead of terminating at `SSE_CHUNK_TIMEOUT_MS`. The `SSE_IDLE_TIMEOUT_MS` idle timer remains the ultimate bound for a truly silent/half-open connection. Related unit tests were updated to assert the extend behavior. ✅ stream unit tests + 3 E2E (streaming format awareness).

**New (user-reported):** terminal events are format-aware. OpenAI `/v1/chat/completions` streams end with `data: [DONE]` and never receive Anthropic `event: message_stop` (fixes openai-node `Expected 'id' to be a string`). Anthropic thinking blocks stream cleanly and count as model data, not empty output.

### Checkpoint 3

- [x] Long streams remain alive with keepalives.
- [x] Short and split events are classified correctly.
- [x] EOM behavior remains idempotent.
- [x] Stream counters return to baseline in every termination case.

## Phase 4 — Test and Documentation Quality

### Task 10 — Deterministic test teardown

- [x] Await mock-server shutdown in every teardown.
- [x] Wait for spawned proxy processes to exit.
- [x] Make cleanup safe when setup partially fails.
- [x] Confirm normal test execution needs no force-exit flag.

All `after()` hooks use an awaited `stopProxy()` (SIGTERM → wait for exit, SIGKILL fallback) + `await mock.close()`. `npm test` exits cleanly with `0` and no leftover proxy/mock processes. ✅

### Task 11 — Complete test and coverage scripts

- [x] Include `tests/verify-issues.test.mjs` in maintained test execution.
- [x] Include handler/resilience/model modules in meaningful coverage.
- [x] Update README and AGENTS test counts.
- [x] Ensure coverage output matches the intended source set.

`npm test` = 97 unit + 51 E2E + 7 verify-issues = 155 tests. New `tests/unit/resilience.test.mjs` and `tests/unit/models.test.mjs` give in-process coverage of config (100%), resilience (100%), stats (100%), stream (97%), utils (95%), models (health/discovery). `handler.mjs` runs in spawned children, so its in-process coverage is low — documented limitation. ✅

### Task 12 — Update documentation

- [x] Document route allowlist and rejection behavior.
- [x] Document inbound auth and exposure defaults.
- [x] Document timeout, body-limit, circuit, and SSE behavior.
- [x] Synchronize README, AGENTS, `.env.example`, and Indonesian docs.
- [x] Verify every documented command against the final scripts.

`docker-compose.yml` sets `LISTEN_ADDRESS=0.0.0.0` inside the container (Docker-to-Docker opt-in); host mode defaults to `127.0.0.1`. ✅

### Final Checklist

- [x] All high-priority findings are fixed.
- [x] All new regression tests pass.
- [x] `npm test` exits cleanly.
- [x] `npm run lint` passes.
- [x] `npm run check` passes.
- [x] Coverage includes the critical handler and resilience paths.
- [x] No secrets or generated artifacts are committed.
- [x] `git status` and `git diff` contain only intended changes.
