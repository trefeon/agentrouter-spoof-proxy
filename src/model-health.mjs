import { UPSTREAM_MODULE, TARGET_HOST_VAL, TARGET_PORT_INT, AGENT } from "./config.mjs";
import { log } from "./logger.mjs";

const PROBE_INTERVAL = 60000;     // check every 60s
const COOLDOWN_MS = 120000;       // block failed model for 2min
const PROBE_TIMEOUT = 8000;       // probe deadline

const failedUntil = new Map();    // model_id → timestamp

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
    failedUntil.set(modelId, Date.now() + COOLDOWN_MS);
    const ts = new Date().toISOString();
    log(ts, `MODEL UNHEALTHY: ${modelId} (${statusCode}) — removed for ${COOLDOWN_MS / 1000}s`);
  }
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
  // Background probing: only recover models, don't mark healthy ones as failed
  probeTimer = setInterval(async () => {
    // Check only models that were previously failed — try to recover them
    if (failedUntil.size === 0) return;
    const models = getModelsFn();
    // Re-probe only still-failed models
    for (const [modelId, until] of failedUntil) {
      if (Date.now() > until) {
        const waf = getWafFn();
        const ok = await probeModel(modelId, waf, spoofHeaders);
        if (ok) {
          failedUntil.delete(modelId);
          log(new Date().toISOString(), `MODEL RECOVERED: ${modelId}`);
        }
      }
    }
  }, PROBE_INTERVAL);
  probeTimer.unref();
}

export function stopProbeLoop() {
  if (probeTimer) { clearInterval(probeTimer); probeTimer = null; }
}
