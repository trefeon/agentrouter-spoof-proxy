import { UPSTREAM_MODULE, TARGET_HOST_VAL, TARGET_PORT_INT } from "../config.mjs";
import { log } from "../logger.mjs";

const WARMUP_HEADERS = {
  "User-Agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
  Accept: "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
  "Accept-Language": "en-US,en;q=0.9",
  "Accept-Encoding": "gzip, deflate, br",
  Connection: "keep-alive",
  "Upgrade-Insecure-Requests": "1",
  "Sec-Fetch-Dest": "document",
  "Sec-Fetch-Mode": "navigate",
  "Sec-Fetch-Site": "none",
  "Sec-Fetch-User": "?1",
};

// Known WAF/edge session-cookie names. Set-Cookie entries with any other name
// are treated as non-WAF (browser/app cookies) and never shipped upstream.
const WAF_COOKIE_NAMES = new Set(["acw_tc", "acw_sc__v2", "acw_sc__v3", "cdn_sec_tc"]);

let wafCookies = [];

export function getWafCookie() {
  return wafCookies.join("; ");
}

// Parse a `set-cookie` header (string or array) and return the valid WAF
// cookies as `name=value` strings. Expired/cleared cookies (`name=; max-age=0`)
// and entries without a `=` are skipped: an empty value must never be shipped
// inside the `Cookie` header — the upstream WAF would read it as a *failed*
// challenge (worse than no cookie at all).
export function extractWafCookies(res) {
  const raw = res.headers["set-cookie"] || [];
  const cookies = Array.isArray(raw) ? raw : [raw];
  const waf = [];
  for (const c of cookies) {
    const pair = c.split(";")[0];
    const eq = pair.indexOf("=");
    if (eq < 1) continue; // no `=` or empty name → skip
    const name = pair.slice(0, eq).trim();
    const value = pair.slice(eq + 1).trim();
    if (!WAF_COOKIE_NAMES.has(name) || !value) continue;
    waf.push(`${name}=${value}`);
  }
  return waf;
}

// Merge cookie lists keyed by cookie NAME: a fresh value replaces the old one
// for the same name while unrelated names are preserved. Both captureWafCookies
// and warmup use this so traffic-captured cookies survive the next warmup.
export function mergeWafCookies(current, fresh) {
  const out = [...current];
  for (const c of fresh) {
    const eq = c.indexOf("=");
    const name = c.slice(0, eq);
    const i = out.findIndex((old) => old.slice(0, old.indexOf("=")) === name);
    if (i === -1) out.push(c);
    else out[i] = c;
  }
  return out;
}

// Merge WAF cookies seen on a live upstream response into the store, keyed by
// cookie NAME: a fresh value replaces the old one for the same name while
// unrelated names are preserved. No-op when the response carries no valid WAF
// cookies. This lets a rotated `acw_tc`/`cdn_sec_tc` on an API response be
// picked up immediately instead of waiting for the next warmup cycle.
export function captureWafCookies(headers = {}) {
  const fresh = extractWafCookies({ headers });
  if (!fresh.length) return;
  wafCookies = mergeWafCookies(wafCookies, fresh);
}

export async function warmup() {
  const ts = new Date().toISOString();
  const { setTimeout: sleep } = await import("node:timers/promises");
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      const cookie = await new Promise((resolve, reject) => {
        const req = UPSTREAM_MODULE.request(
          {
            hostname: TARGET_HOST_VAL,
            port: TARGET_PORT_INT,
            path: "/",
            method: "GET",
            headers: WARMUP_HEADERS,
            agent: false,
            rejectUnauthorized: true,
            timeout: 10000,
          },
          (res) => {
            const waf = extractWafCookies(res);
            res.resume();
            res.on("end", () => resolve(waf));
          }
        );
        req.on("error", reject);
        req.on("timeout", () => { req.destroy(); reject(new Error("timeout")); });
        req.end();
      });

      if (cookie.length) {
        // Merge, not replace: the warmup GET "/" may not return every WAF
        // cookie the traffic path captured (e.g. `cdn_sec_tc` seen on an API
        // response) — dropping them on the next warmup cycle would regress the
        // traffic-time refresh. Fresh warmup values win for the names it
        // returns; traffic-only names are preserved.
        wafCookies = mergeWafCookies(wafCookies, cookie);
        log(ts, `WARMUP → 200 cookies: ${cookie.length}`);
        return;
      }
    } catch (e) {
      if (attempt >= 2) log(ts, `WARMUP attempt 3/3 failed: ${e.message}`);
    }
    if (attempt < 2) await sleep(1000 * (attempt + 1));
  }
  log(ts, `WARMUP failed after 3 attempts`);
}
