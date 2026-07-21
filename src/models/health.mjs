import { UPSTREAM_MODULE, TARGET_HOST_VAL, TARGET_PORT_INT, AGENT } from "../config.mjs";
import { log } from "../logger.mjs";

const PROBE_INTERVAL = 60000;     // recovery probe every 60s
const PROBE_TIMEOUT = 8000;       // probe deadline
const BASE_COOLDOWN = 30000;      // 30s first offence
const MAX_COOLDOWN = 600000;      // 10min max lock

const failedUntil = new Map();    // model_id → timestamp
const failCounts = new Map();     // model_id → consecutive failure count

const BACKOFF = [30000, 60000, 120000, 300000, 600000]; // 30s, 1m, 2m, 5m, 10m

export function isModelHealthy(modelId) {
  const ts = failedUntil.get(modelId);
  if (!ts) return true;
  return Date.now() > ts;
}

export function getHealthyModels(models) {
  return models.filter((m) => isModelHealthy(m.id));
}

export function markModelFailed(modelId, statusCode) {
  if (!modelId) return;
  if (statusCode >= 500) {
    const count = (failCounts.get(modelId) || 0) + 1;
    failCounts.set(modelId, count);
    const idx = Math.min(count - 1, BACKOFF.length - 1);
    const cooldown = BACKOFF[idx];
    failedUntil.set(modelId, Date.now() + cooldown);
    const ts = new Date().toISOString();
    log(ts, `MODEL UNHEALTHY: ${modelId} (${statusCode}, #${count}) — locked for ${cooldown / 1000}s`);
  }
}

export function markModelExhausted(modelId) {
  if (!modelId) return;
  failedUntil.set(modelId, Date.now() + 120000);
  const ts = new Date().toISOString();
  log(ts, `MODEL EXHAUSTED (429): ${modelId} — locked for 120s`);
}

export function markModelDegraded(modelId, reason = "degraded") {
  if (!modelId) return;
  failedUntil.set(modelId, Date.now() + 60000);
  const ts = new Date().toISOString();
  log(ts, `MODEL DEGRADED: ${modelId} (${reason}) — locked for 60s`);
}

function clearModelLock(modelId) {
  failedUntil.delete(modelId);
  failCounts.delete(modelId);
  log(new Date().toISOString(), `MODEL RECOVERED: ${modelId}`);
}

async function probeModel(modelId, wafCookie, spoofHeaders) {
  return new Promise((resolve) => {
    const headers = { ...spoofHeaders, "Content-Type": "application/json" };
    if (wafCookie) headers["Cookie"] = wafCookie;

    const body = JSON.stringify({
      model: modelId, max_tokens: 1, stream: false,
      messages: [{ role: "user", content: "." }],
    });

    const req = UPSTREAM_MODULE.request(
      {
        hostname: TARGET_HOST_VAL,
        port: TARGET_PORT_INT,
        path: "/v1/messages",
        method: "POST",
        headers,
        agent: AGENT,
        rejectUnauthorized: true,
        timeout: PROBE_TIMEOUT,
      },
      (res) => {
        res.resume();
        res.on("end", () => resolve(res.statusCode === 200));
        res.on("error", () => resolve(false));
      }
    );
    req.on("error", () => resolve(false));
    req.on("timeout", () => { req.destroy(); resolve(false); });
    req.write(body);
    req.end();
  });
}

let probeTimer = null;

export function startProbeLoop(getModelsFn, getWafFn, spoofHeaders) {
  if (probeTimer) return;
  probeTimer = setInterval(async () => {
    if (failedUntil.size === 0) return;
    for (const [modelId, until] of failedUntil) {
      if (Date.now() > until) {
        const ok = await probeModel(modelId, getWafFn(), spoofHeaders);
        if (ok) {
          clearModelLock(modelId);
        } else {
          // Recovery probe still failing — extend lock
          const count = (failCounts.get(modelId) || 0) + 1;
          failCounts.set(modelId, count);
          const idx = Math.min(count - 1, BACKOFF.length - 1);
          failedUntil.set(modelId, Date.now() + BACKOFF[idx]);
          log(new Date().toISOString(), `MODEL STILL DOWN: ${modelId} (#${count}) — extended for ${BACKOFF[idx] / 1000}s`);
        }
      }
    }
  }, PROBE_INTERVAL);
  probeTimer.unref();
}

export function stopProbeLoop() {
  if (probeTimer) { clearInterval(probeTimer); probeTimer = null; }
}
