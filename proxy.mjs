import http from "node:http";
import { resolve4 } from "node:dns/promises";
import * as cfg from "./src/config.mjs";
import { log } from "./src/logger.mjs";
import { isCircuitOpen, getConsecutiveFails } from "./src/resilience/circuit-breaker.mjs";
import { getWafCookie, warmup } from "./src/auth/waf.mjs";
import { SPOOF_HEADERS } from "./src/auth/spoof.mjs";
import { getModelsList, getModelSource, fetchModels } from "./src/models/discovery.mjs";
import { getHealthyModels, startProbeLoop, stopProbeLoop } from "./src/models/health.mjs";
import { getModelStats } from "./src/models/stats.mjs";
import { respondJson } from "./src/utils.mjs";
import { handleProxyRequest } from "./src/proxy/handler.mjs";

// ── Shared state ──

const streams = { count: 0 };

// ── DNS ──

async function resolveDns() {
  const ts = new Date().toISOString();
  try {
    const addresses = await resolve4(cfg.TARGET_HOST_VAL);
    log(ts, `DNS resolved ${cfg.TARGET_HOST_VAL} → ${addresses.join(", ")}`);
  } catch {
    log(ts, `DNS resolution failed for ${cfg.TARGET_HOST_VAL}`);
  }
}

// ── Warmup scheduler ──

function scheduleWarmup() {
  warmup();
  setInterval(warmup, cfg.WARMUP_INTERVAL);
}

// ── Model discovery scheduler ──

function scheduleDiscovery() {
  if (!cfg.AR_API_KEY_VAL) {
    console.log(`Model discovery disabled (no AR_API_KEY set), using static list`);
    return;
  }
  fetchModels();
  setInterval(fetchModels, cfg.DISCOVERY_INTERVAL);
}

// ── Server ──

const server = http.createServer((req, res) => {
  const rawPath = req.url;
  const method = req.method;

  // Health check
  if (method === "GET" && (rawPath === "/health" || rawPath === "/api/health")) {
    respondJson(res, 200, {
      ok: true,
      upstream: `${cfg.TARGET_HOST_VAL}:${cfg.TARGET_PORT_INT}`,
      modelSource: getModelSource(),
      staticModels: cfg.MODELS_CSV_VAL.split(",").length,
      availableModels: getModelsList().length,
      activeStreams: streams.count,
      wafCookie: !!getWafCookie(),
      circuitOpen: isCircuitOpen(),
      consecutiveFails: getConsecutiveFails(),
      modelHealth: getModelStats(),
    });
    return;
  }

  // Model list — filter unhealthy models so 9Router falls back instantly
  if (method === "GET" && (rawPath === "/v1/models" || rawPath === "/models")) {
    respondJson(res, 200, { data: getHealthyModels(getModelsList()), object: "list" });
    return;
  }

  handleProxyRequest(req, res, streams);
});

server.headersTimeout = 30000;
server.requestTimeout = 0;

// ── Start & Shutdown ──

server.listen(cfg.PORT, "0.0.0.0", async () => {
  console.log(`AgentRouter proxy listening on port ${cfg.PORT}, target=${cfg.TARGET_HOST_VAL}:${cfg.TARGET_PORT_INT}`);
  await resolveDns();
  scheduleWarmup();
  scheduleDiscovery();
  startProbeLoop(getModelsList, getWafCookie, SPOOF_HEADERS);
});

function shutdown(signal) {
  console.log(`\n[${new Date().toISOString()}] ${signal} received — draining ${streams.count} active streams...`);
  stopProbeLoop();
  server.close(() => {
    cfg.AGENT.destroy();
    console.log(`[${new Date().toISOString()}] Server closed, exiting.`);
    process.exit(0);
  });
  setTimeout(() => {
    cfg.AGENT.destroy();
    console.error(`[${new Date().toISOString()}] Forced exit after timeout`);
    process.exit(1);
  }, 15000).unref();
}

process.on("SIGTERM", () => shutdown("SIGTERM"));
process.on("SIGINT", () => shutdown("SIGINT"));
process.on("uncaughtException", (err, origin) => {
  console.error(`[${new Date().toISOString()}] UNCAUGHT EXCEPTION (${origin}): ${err.message}`);
  console.error(err.stack);
});
process.on("unhandledRejection", (reason) => {
  const ts = new Date().toISOString();
  const msg = reason instanceof Error ? reason.message : String(reason);
  console.error(`[${ts}] UNHANDLED REJECTION: ${msg}`);
  if (reason instanceof Error && reason.stack) console.error(reason.stack);
});