# Audit: Auth — Spoof Header Injection & WAF Cookie Handling

Resolves [issue #2](https://github.com/trefeon/agentrouter-spoof-proxy/issues/2).

Scope: `src/auth/spoof.mjs`, `src/auth/waf.mjs`, `src/proxy/handler.mjs`,
`src/utils.mjs`, all related tests, and the model-health probes that share the
spoof/WAF headers.

---

## 1. What the spoof/WAF layer does

The proxy stands between 9Router and `agentrouter.org` (which is fronted by an
Alibaba-Cloud-style WAF). Two mechanisms keep traffic flowing:

- **Spoof headers** — every upstream request carries headers that make the
  upstream + WAF fingerprint the client as Claude Code CLI (UA, `x-app`,
  `x-stainless-*`, `anthropic-beta/-version`, the browser-access opt-in header).
  Anthropic-format requests (`/v1/messages`) get the full set; OpenAI-format
  requests (`/v1/chat/completions`) get only the generic set.
- **WAF cookie** — a warmup loop `GET /` against the target and captures the
  session cookies (`acw_tc`, `cdn_sec_tc`) the WAF sets, then re-sends them on
  every upstream API request. On a 403/405 WAF block the proxy re-warms and
  retries once.

## Research performed (evidence)

1. **Real Claude Code header capture** — an actual Claude Code request log
   (github.com/anthropics/claude-code issue #2182) shows the exact header set:
   `anthropic-beta: claude-code-20250219`, `anthropic-version: 2023-06-01`,
   `anthropic-dangerous-direct-browser-access: true`, `x-app: cli`,
   `user-agent: claude-cli/<v> (external, ...)`, `x-stainless-*`. The spoof set
   matches this shape. Semantic of the "dangerous" header (CORS/CSRF opt-in
   that real Claude Code sends by default, also used by the SDK's
   `dangerouslyAllowBrowser`).
2. **Alibaba WAF cookie contract** (Cloud docs, WAF 3.0 + ESA):
   - `acw_tc` — WAF session/CC-protection cookie, set via `Set-Cookie` the first
     time a client shows up without one. This is the cookie a plain GET can obtain. ✅
   - `cdn_sec_tc` — Edge-Security-ACCE (ESA) equivalent, same contract. ✅
   - `acw_sc__v2` — JavaScript-validation cookie, set **only after the client
     passes the JS challenge** (value is computed by the challenge script). A
     server-side plain HTTP GET **cannot** obtain it. When verification fails,
     the WAF *clears* it with `Set-Cookie: acw_sc__v2=; max-age=0`. ⚠️
   - `acw_sc__v3` — slider-CAPTCHA cookie, same model. ⚠️
3. **Node outgoing-header normalization** — empirically proven: `http.request`
   lowercases header names, so when both `"Anthropic-Version"` (spoof) and
   `"anthropic-version"` (client pass-through) are present, the client value
   **silently wins** (last one set). This is the spoof-injection hole.

## Changes applied

### CHG-1 (bug) WAF: reject empty/expired cookie values
`extractWafCookies` now skips cookies whose name or value is empty (`acw_tc=;` /
`acw_sc__v2=; max-age=0`). An empty value must never be shipped inside the
`Cookie` header — it makes the upstream WAF treat the client as a *failed*
challenge (worse than no cookie). Also added `acw_sc__v3` to the known set and
made name matching defensive (no `=` → skip; trim).

### CHG-2 (robustness) — WAF cookie refresh on live traffic
New `captureWafCookies(headers)` + shared `mergeWafCookies(current, fresh)` in
`src/auth/waf.mjs`. The request handler captures `Set-Cookie` from every
upstream response; valid WAF cookies are merged into the store **by name**
(union), so a rotated `acw_tc` on an API response is picked up immediately
instead of waiting for the 3‑min warmup, and a fresh `cdn_sec_tc` from a proxy
response does not wipe the warmup `acw_tc`. `warmup()` uses the same merge
(instead of wiping the store), so traffic-captured cookies survive the next
warmup cycle.

### CHG-3 (security/correctness) — spoof authority: drop client `anthropic-version`
`src/proxy/handler.mjs` no longer forwards the client's `anthropic-version` on
`/v1/messages`. The spoofed `Anthropic-Version: 2023-06-01` (and the beta list)
is the source of truth on the wire, matching real Claude Code. Prevents a
buggy/malicious client from distorting the spoof identity. Client-supplied
`Authorization` / `x-api-key` remain forwarded (the actual API credentials).

### CHG-4 (classification) — `isWafBlock` markers
Added `waf.js` (the static challenge script referenced by Alibaba block pages)
to the marker set (`alicdn`, `block_message`, `renderData`, now `waf.js`).
Only evaluated on 403/405 upstream bodies. Retry-after-warmup on a *false
positive* is harmless (one extra attempt); a true positive now retries instead
of passing a block page to the client.

### Not changed (with reason)
- `x-stainless-timeout: 600` — plausible for the current SDK/CLI defaults;
  observed logs range 60–600; not worth disturbing a working bypass.
- Hardcoded `X-Stainless-Os: Linux` / `X-Stainless-Arch: arm64` — a *consistent*
  fingerprint across backends is deliberate; deriving from the host would make
  the spoof vary per machine and trip fingerprint checks.
- Bearer forward rule — when `PROXY_AUTH_TOKEN` is configured, 9Router supplies
  the same token as `Bearer` for the proxy **and** upstream auth
  (documented same-token setup) — forwarding it upstream is intentional.
  The dedicated `X-Proxy-Token` credential is **never** forwarded upstream
  (already the case; now locked with a test).

## Test coverage added (with the fix)

- `tests/unit/utils.test.mjs` — `extractWafCookies` rejects empty value on expired cookies, keeps non-empty WAF cookies (`acw_tc`/`acw_sc`/`acw_sc__v2`/`acw_sc__v3`), skips malformed entries, ignores decided.
- `tests/unit/waf-traffic.test.mjs` — `captureWafCookies` merges by name, doesn't
  wipe the warmup cookie, ignores empty/non-WAF values; `mergeWafCookies` keeps
  traffic-captured cookies across a simulated warmup (warmup value wins for the
  names it returns).
- `tests/unit/utils.test.mjs` — `isWafBlock` true on a `waf.js`-only body.
- `tests/proxy.test.mjs` (e2e): client `anthropic-version` override is ignored →
  upstream sees the spoofed `2023-06-01`; a `Set-Cookie: cdn_sec_tc=...` on an API
  response updates the cookie carried on the next request (merge preserves the
  warmup cookie too).
- auth isolate e2e: `X-Proxy-Token` credential never reaches upstream and
  `Authorization: Bearer` <real-key> still forwarded.

## Verification

- `npm run test:unit && npm run lint && npm run test:e2e (clean exit, no hanging proxies mock leftover).
</content>