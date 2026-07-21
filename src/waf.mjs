import { UPSTREAM_MODULE, TARGET_HOST_VAL, TARGET_PORT_INT } from "./config.mjs";
import { log } from "./logger.mjs";

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

let wafCookieStr = "";

export function getWafCookie() {
  return wafCookieStr;
}

function extractWafCookies(res) {
  const cookies = res.headers["set-cookie"] || [];
  const waf = [];
  for (const c of cookies) {
    const name = c.split("=")[0];
    if (name === "acw_tc" || name === "acw_sc__v2" || name === "cdn_sec_tc") {
      waf.push(c.split(";")[0]);
    }
  }
  return waf;
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
        wafCookieStr = cookie.join("; ");
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
