# Implementation Plan: Proxy Reliability and Security Hardening

## Overview

This plan addresses the current-state review findings for AgentRouter Spoof Proxy. The implementation should preserve the project's zero-runtime-dependency Node.js design, existing Anthropic/OpenAI-compatible endpoints, WAF warmup behavior, retry behavior, and SSE streaming contract while making failure handling deterministic and deployment safer.

The highest-priority work is the response-timeout deadlock and circuit-breaker accounting. Security and request-boundary hardening follow, then SSE correctness and test infrastructure. Each task is intended to be independently verifiable and small enough for one focused implementation session.

## Goals

- Always terminate a request exactly once with a response or a documented client disconnect path.
- Ensure configured upstream response timeouts return a 504, retry when configured, and decrement `activeStreams`.
- Make the circuit breaker reflect the failure classes the proxy already treats as retryable/upstream failures.
- Prevent unintended proxy surface area and make exposure/authentication behavior explicit.
- Return a real 413 for oversized requests without resetting the client first.
- Preserve SSE keepalives and correctly recognize events independent of TCP chunk boundaries.
- Make the test command terminate cleanly and make coverage include the critical handler paths.

## Non-goals

- Replacing the Node.js built-in HTTP client/server.
- Rewriting the WAF spoofing strategy or changing upstream model behavior.
- Adding a runtime framework or a required production dependency.
- Changing model names, provider routing semantics, or prompt-injection semantics.

## Architecture Decisions

1. **Single terminal lifecycle:** every request path must converge on one idempotent completion function that clears timers, aborts the active upstream request when appropriate, and decrements `streams.count` exactly once.
2. **Explicit timeout handling:** manually destroying an upstream request must not depend on a later `error` event to complete the client response. The timeout handler should invoke the error/retry path directly.
3. **Circuit semantics:** count final upstream 5xx responses as failures if they are retried or used to mark a model unhealthy; do not count ordinary client errors as upstream outages. Document the selected treatment of 429 responses.
4. **Narrow routing surface:** only documented API routes should be proxied. Health and model discovery remain separate local endpoints; unknown paths should receive a local 404.
5. **Deployment security:** host-mode operation should default to local exposure. Docker-to-Docker operation must explicitly opt into container-network exposure, and an optional proxy authentication token should be supported for shared or remote deployments.
6. **SSE parsing over chunk heuristics:** stream health must be based on parsed/carry-over SSE content, not on the size or boundaries of individual `data` chunks.
7. **Built-in tooling only:** tests and fixes should use `node:test`, Node streams, and existing project utilities; `oxlint` remains a development-only dependency.

## Dependency Graph

```text
Request lifecycle + regression tests
        |
        +--> timeout and retry correctness
        |
        +--> circuit-breaker accounting
        |
        +--> request boundary/auth hardening

SSE framing state
        |
        +--> keepalive lifecycle
        +--> stream health/degradation behavior

Test teardown + coverage configuration
        |
        +--> reliable final verification for all changes
```

## Task List

### Phase 1: Failure Lifecycle Foundation

#### Task 1: Add regression tests for adaptive response timeout

**Description:** Add an end-to-end test with `RESPONSE_TIMEOUT_MS` shorter than `REQUEST_TIMEOUT_MS` and an upstream that accepts the request but never sends response headers.

**Acceptance criteria:**
- [ ] The client receives HTTP 504 within the configured response-timeout window.
- [ ] The upstream request is destroyed.
- [ ] `/health` reports the original `activeStreams` value after the request finishes.
- [ ] The test distinguishes response timeout from request timeout.

**Verification:**
- [ ] Run the focused E2E test.
- [ ] Run the complete E2E suite.

**Dependencies:** None.

**Files likely touched:**
- `tests/proxy.test.mjs`
- `tests/mock-upstream.mjs`

**Estimated scope:** Small.

#### Task 2: Fix the adaptive response-timeout completion path

**Description:** Make the adaptive response timer invoke the existing retry/error path directly and keep the completion guards consistent with request timeout and socket-error paths.

**Acceptance criteria:**
- [ ] A response timeout returns 504 when retries are exhausted.
- [ ] A response timeout retries when `MAX_RETRIES` permits it.
- [ ] No response timeout can leave `streams.count` incremented.
- [ ] A late upstream `error` or `close` event cannot produce a second terminal response.

**Verification:**
- [ ] Focused adaptive-timeout test passes.
- [ ] Unit/E2E suites pass.
- [ ] `npm run lint` and `npm run check` pass.

**Dependencies:** Task 1.

**Files likely touched:**
- `src/proxy/handler.mjs`
- `tests/proxy.test.mjs`

**Estimated scope:** Small.

#### Task 3: Correct circuit-breaker accounting

**Description:** Align circuit-breaker updates with retry/failure semantics. Ensure final 5xx responses and exhausted retry sequences are counted as failures when appropriate, while successful upstream responses reset the breaker.

**Acceptance criteria:**
- [ ] Five consecutive final 5xx responses open the circuit.
- [ ] A successful upstream response resets consecutive failures.
- [ ] Ordinary 4xx client errors do not falsely open the circuit.
- [ ] 429 behavior is explicitly tested and documented.

**Verification:**
- [ ] Add unit or E2E coverage for final 5xx responses.
- [ ] Run the circuit-breaker and full E2E tests.

**Dependencies:** Task 2.

**Files likely touched:**
- `src/proxy/handler.mjs`
- `src/resilience/circuit-breaker.mjs`
- `tests/proxy.test.mjs`

**Estimated scope:** Medium.

### Checkpoint: Failure Lifecycle

- [ ] Adaptive timeout returns a bounded response and leaves no active-stream leak.
- [ ] Final 5xx responses exercise the circuit breaker as designed.
- [ ] Existing connection-error, upstream-timeout, retry, and WAF tests still pass.
- [ ] No uncaught rejection is emitted by retry or timeout paths.

### Phase 2: Request Boundary and Deployment Security

#### Task 4: Restrict the proxy route surface

**Description:** Make routing explicit: proxy only `/v1/messages`, `/messages`, and `/v1/chat/completions` (including query strings), while retaining `/health`, `/api/health`, `/models`, and `/v1/models` as local endpoints. Return a local 404 for unknown routes.

**Acceptance criteria:**
- [ ] Documented routes retain their current behavior.
- [ ] Unknown paths are not sent upstream.
- [ ] Unsupported methods receive a deterministic local response where appropriate.
- [ ] Query strings on supported routes remain intact.

**Verification:**
- [ ] Add route allowlist tests.
- [ ] Confirm the mock upstream receives no request for an unknown path.

**Dependencies:** None.

**Files likely touched:**
- `proxy.mjs`
- `src/utils.mjs`
- `tests/proxy.test.mjs`

**Estimated scope:** Small.

#### Task 5: Define and enforce inbound proxy authentication

**Description:** Choose and implement the deployment contract for shared/remote access. Recommended design: add an optional `PROXY_AUTH_TOKEN`, compare it against a dedicated inbound header or bearer token using a constant-time comparison, and document that host-mode defaults to local exposure while remote/container-network exposure requires the token.

**Acceptance criteria:**
- [ ] The chosen authentication behavior is documented in `.env.example`, README, and the Indonesian guide.
- [ ] Unauthorized proxy requests are rejected before upstream work begins.
- [ ] Health-check and Docker behavior remain usable under the selected deployment defaults.
- [ ] Credentials are never logged.

**Verification:**
- [ ] Test missing, invalid, and valid inbound credentials.
- [ ] Test direct local operation and Docker/9Router configuration expectations.

**Dependencies:** Task 4.

**Files likely touched:**
- `src/config.mjs`
- `proxy.mjs`
- `src/utils.mjs` or a new `src/auth/inbound.mjs`
- `.env.example`
- `README.md`

**Estimated scope:** Medium.

#### Task 6: Make request body limits return 413 cleanly

**Description:** Replace the current destroy-before-response behavior with a controlled rejection. Prefer an early `Content-Length` check plus streaming enforcement; send 413 once, stop buffering, and close/drain the request only after the response is committed safely.

**Acceptance criteria:**
- [ ] Requests over 20 MB receive HTTP 413 rather than `ECONNRESET`.
- [ ] Oversized requests never reach the upstream.
- [ ] The request cannot trigger duplicate responses or unhandled errors.
- [ ] Normal requests remain stream-safe and preserve the existing body limit.

**Verification:**
- [ ] Add tests for both `Content-Length` and chunked oversized requests.
- [ ] Run the issue-verification tests and E2E suite.

**Dependencies:** Task 4.

**Files likely touched:**
- `src/proxy/handler.mjs`
- `tests/proxy.test.mjs`
- `tests/verify-issues.test.mjs`

**Estimated scope:** Medium.

#### Task 7: Add bounded request-upload and configuration validation

**Description:** Prevent slow-body socket exhaustion and invalid environment values from producing undefined timer/server behavior. Add a body-upload deadline or idle timer, validate numeric settings at startup, and fail with a clear message for invalid values.

**Acceptance criteria:**
- [ ] A client that stops uploading is terminated within a documented bound.
- [ ] Invalid port, timeout, retry, and interval values fail fast with actionable errors.
- [ ] Valid zero/disabled settings, if supported, are explicitly documented.
- [ ] Startup failures exit non-zero instead of relying on `uncaughtException` logging.

**Verification:**
- [ ] Add configuration unit tests.
- [ ] Add a slow-upload integration test.
- [ ] Run startup checks with valid and invalid environments.

**Dependencies:** Task 5.

**Files likely touched:**
- `src/config.mjs`
- `proxy.mjs`
- `src/proxy/handler.mjs`
- `.env.example`

**Estimated scope:** Medium.

### Checkpoint: Request Boundary

- [ ] Unknown paths are rejected locally.
- [ ] Remote/shared access behavior is covered by tests and documentation.
- [ ] Oversized and slow uploads terminate predictably.
- [ ] Existing 9Router-compatible routes still pass.

### Phase 3: SSE Correctness

#### Task 8: Preserve keepalive timers across data events

**Description:** Separate idle-timer reset from global timer cleanup so receiving upstream data does not cancel the keepalive interval or unintentionally discard unrelated timers.

**Acceptance criteria:**
- [ ] Keepalive comments continue after real upstream data arrives.
- [ ] Keepalives stop when the stream finishes, errors, times out, or the client disconnects.
- [ ] Chunk and idle timers retain their current semantics.

**Verification:**
- [ ] Add a stream unit test that emits data, waits beyond the keepalive interval, and observes a keepalive comment.
- [ ] Run all stream tests.

**Dependencies:** None.

**Files likely touched:**
- `src/proxy/stream.mjs`
- `tests/unit/stream.test.mjs`

**Estimated scope:** Small.

#### Task 9: Replace chunk-size SSE heuristics with framing state

**Description:** Track a small carry-over buffer for SSE event framing. Detect real events and `message_stop` even when an event is short or split across Node stream chunks; keep the existing EOM behavior for genuinely abnormal termination.

**Acceptance criteria:**
- [ ] A short valid `message_stop` is not marked as empty output.
- [ ] A marker split across chunks is detected exactly once.
- [ ] Keepalive comments do not count as model data.
- [ ] Premature close still synthesizes one terminal event when needed.

**Verification:**
- [ ] Add tests for short events, split markers, comments, empty streams, and partial close.
- [ ] Run unit and E2E streaming tests.

**Dependencies:** Task 8.

**Files likely touched:**
- `src/proxy/stream.mjs`
- `tests/unit/stream.test.mjs`
- `tests/mock-upstream.mjs`

**Estimated scope:** Medium.

### Checkpoint: Streaming

- [ ] Long streams receive keepalives after real data.
- [ ] Short/split SSE frames are classified correctly.
- [ ] EOM injection remains idempotent.
- [ ] Active stream counts return to baseline on every stream termination path.

### Phase 4: Test and Documentation Quality

#### Task 10: Make test teardown deterministic

**Description:** Await mock-server shutdown, wait for spawned proxy children to exit, and make cleanup idempotent. Keep `--test-force-exit` as a diagnostic fallback rather than a required normal workflow.

**Acceptance criteria:**
- [ ] `npm test` exits without manual process termination.
- [ ] No proxy or mock-upstream child remains after the suite.
- [ ] Cleanup still runs when a test fails.

**Verification:**
- [ ] Run `npm test` twice consecutively.
- [ ] Confirm no repository test processes remain after completion.

**Dependencies:** Tasks 1-9.

**Files likely touched:**
- `tests/proxy.test.mjs`
- `tests/verify-issues.test.mjs`
- `tests/mock-upstream.mjs`
- `package.json`

**Estimated scope:** Medium.

#### Task 11: Expand coverage and include issue verification

**Description:** Update scripts so the normal test command includes `tests/verify-issues.test.mjs`, and add a coverage command or suite that exercises handler, health, model, discovery, and circuit-breaker paths rather than reporting only utility/stream coverage.

**Acceptance criteria:**
- [ ] `npm test` includes all maintained test files.
- [ ] Coverage includes the request handler and resilience/model modules.
- [ ] README test counts and commands match actual scripts.
- [ ] Coverage thresholds or documented expectations are reproducible.

**Verification:**
- [ ] Run `npm test`, `npm run coverage`, `npm run lint`, and `npm run check`.
- [ ] Compare reported files against `src/**/*.mjs`.

**Dependencies:** Task 10.

**Files likely touched:**
- `package.json`
- `README.md`
- `AGENTS.md`
- `tests/verify-issues.test.mjs`

**Estimated scope:** Medium.

#### Task 12: Update operational/security documentation

**Description:** Document the final route allowlist, inbound authentication, timeout behavior, body limits, Docker exposure requirements, and test commands after implementation.

**Acceptance criteria:**
- [ ] English and Indonesian deployment instructions agree.
- [ ] Security defaults and remote-exposure warnings are prominent.
- [ ] Environment variables and defaults are complete and accurate.
- [ ] Troubleshooting covers timeout, 413, circuit-open, and SSE termination behavior.

**Verification:**
- [ ] Review all changed documentation against the implementation.
- [ ] Run documented verification commands from a clean checkout.

**Dependencies:** Tasks 5, 7, 10, and 11.

**Files likely touched:**
- `README.md`
- `AGENTS.md`
- `docs/panduan-9router.md`
- `.env.example`

**Estimated scope:** Medium.

### Final Checkpoint: Ready for Review

- [ ] All tasks are complete or explicitly deferred.
- [ ] Full tests exit cleanly.
- [ ] Lint and syntax checks pass.
- [ ] Timeout, breaker, auth, body-limit, and SSE regressions are covered.
- [ ] Documentation matches production defaults.
- [ ] Git diff contains only intended changes.

## Risks and Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Changing circuit semantics causes more 503 responses during upstream 5xx bursts | High | Add explicit 5xx/4xx/429 tests and document the policy before implementation |
| Adding inbound auth breaks existing 9Router setups | High | Decide compatibility mode first; provide clear Docker/host configuration and migration guidance |
| Route allowlisting breaks an undocumented upstream endpoint | Medium | Confirm supported endpoints from README and integration tests; return a clear local 404 |
| SSE parser changes alter client-visible event timing | High | Preserve raw chunk forwarding and test message_stop/EOM behavior with split frames |
| Cleanup changes mask real open-handle bugs | Medium | Run repeated test suites and inspect child processes after normal completion |
| Configuration validation rejects existing deployments | Medium | Document accepted ranges and test current `.env.example` defaults |

## Open Questions (resolved)

- **Inbound authentication mandatory or compatibility mode?** → **Compatibility mode.** `PROXY_AUTH_TOKEN` is optional. When unset, proxied routes require no auth, and safety comes from the default `LISTEN_ADDRESS=127.0.0.1` bind. When set, `Authorization: Bearer <token>` or `X-Proxy-Token: <token>` is required (constant-time compare), while `/health` and `/v1/models` stay open for Docker/9Router probes. Docker-to-Docker exposure opts in via `LISTEN_ADDRESS=0.0.0.0` in `docker-compose.yml`.
- **Should `429` reset or trip the global circuit?** → Neither. A 429 means the upstream is reachable, so it never counts toward the global circuit; the affected model is locked out via `markModelExhausted` so 9Router falls back to other models.
- **Unknown methods on supported paths?** → Rejected locally with `405`. The three proxy routes are POST-only.
- **Acceptable upload idle timeout?** → `BODY_UPLOAD_TIMEOUT_MS` default 60000 ms; the stalled upload is rejected with `408`.
- **Should `uncaughtException` terminate the process?** → Startup misconfiguration now fails fast (`validateConfig()` + `process.exit(1)`) instead of relying on the logging handler. Runtime `uncaughtException`/`unhandledRejection` remain logged so the process manager can decide.
- **Streaming cut by stall watchdog?** → User-driven change: the watchdog now *extends* while the upstream connection is still open, so long thinking/tool pauses never cut a live stream; `SSE_IDLE_TIMEOUT_MS` (no events at all) is the ultimate bound. OpenAI streams end with `data: [DONE]` (fixes openai-node `Expected 'id' to be a string`), Anthropic streams keep `event: message_stop`.

## Completion Definition

The work is complete when the final checkpoint is satisfied, the highest-priority regressions have automated tests, the default deployment behavior is documented, and `npm test` exits cleanly without `--test-force-exit`.
