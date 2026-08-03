import http from "node:http";
import https from "node:https";

const {
  LISTEN_PORT = "8318",
  LISTEN_ADDRESS = "127.0.0.1",
  TARGET_PROTOCOL = "https",
  TARGET_HOST = "agentrouter.org",
  TARGET_PORT = "443",
  REQUEST_TIMEOUT_MS = "300000",
  MODELS_CSV = "gpt-5.6-sol,claude-opus-5,claude-opus-4-8",
  WARMUP_INTERVAL_MS = "180000",
  MAX_RETRIES = "2",
  RETRY_DELAY_MS = "1000",
  AR_API_KEY = "",
  DISCOVERY_INTERVAL_MS = "600000",
  INJECT_SYSTEM_PROMPT = "",
  SSE_IDLE_TIMEOUT_MS = "600000",
  SSE_CHUNK_TIMEOUT_MS = "30000",
  RESPONSE_TIMEOUT_MS = "30000",
  BODY_UPLOAD_TIMEOUT_MS = "60000",
  PROXY_AUTH_TOKEN = "",
  LOG_LEVEL = "info",
  SLOW_RESPONSE_MS = "30000",
} = process.env;

export const PORT = parseInt(LISTEN_PORT, 10);
export const LISTEN_ADDRESS_VAL = LISTEN_ADDRESS;
export const TARGET_PORT_INT = parseInt(TARGET_PORT, 10);
export const TIMEOUT = parseInt(REQUEST_TIMEOUT_MS, 10);
export const WARMUP_INTERVAL = parseInt(WARMUP_INTERVAL_MS, 10);
export const MAX_RETRIES_NUM = parseInt(MAX_RETRIES, 10);
export const RETRY_DELAY = parseInt(RETRY_DELAY_MS, 10);
export const DISCOVERY_INTERVAL = parseInt(DISCOVERY_INTERVAL_MS, 10);
export const SSE_IDLE = parseInt(SSE_IDLE_TIMEOUT_MS, 10);
export const SSE_CHUNK_TIMEOUT = parseInt(SSE_CHUNK_TIMEOUT_MS, 10);
export const RESPONSE_TIMEOUT = parseInt(RESPONSE_TIMEOUT_MS, 10);
export const BODY_UPLOAD_TIMEOUT = parseInt(BODY_UPLOAD_TIMEOUT_MS, 10);
export const PROXY_AUTH_TOKEN_VAL = PROXY_AUTH_TOKEN;
export const IS_DEBUG = LOG_LEVEL === "debug";
export const SLOW_RESPONSE_MS_INT = parseInt(SLOW_RESPONSE_MS, 10);

export const TARGET_HOST_VAL = TARGET_HOST;
export const MODELS_CSV_VAL = MODELS_CSV;
export const AR_API_KEY_VAL = AR_API_KEY;
export const INJECT_SYSTEM_PROMPT_VAL = INJECT_SYSTEM_PROMPT;

export const UPSTREAM_MODULE = TARGET_PROTOCOL === "http" ? http : https;

// ── Startup validation ──
//
// Fail fast with an actionable message instead of letting NaN timers, a bad
// port, or an unbound protocol drift into undefined behavior. Thrown errors are
// caught by proxy.mjs which exits non-zero.
export function validateConfig() {
  const checks = [
    ["LISTEN_PORT", PORT, "integer 1-65535", (v) => Number.isInteger(v) && v >= 1 && v <= 65535],
    ["TARGET_PORT", TARGET_PORT_INT, "integer 1-65535", (v) => Number.isInteger(v) && v >= 1 && v <= 65535],
    ["REQUEST_TIMEOUT_MS", TIMEOUT, "positive integer (ms)", (v) => Number.isInteger(v) && v > 0],
    ["RESPONSE_TIMEOUT_MS", RESPONSE_TIMEOUT, "positive integer (ms)", (v) => Number.isInteger(v) && v > 0],
    ["SSE_IDLE_TIMEOUT_MS", SSE_IDLE, "positive integer (ms)", (v) => Number.isInteger(v) && v > 0],
    ["SSE_CHUNK_TIMEOUT_MS", SSE_CHUNK_TIMEOUT, "positive integer (ms)", (v) => Number.isInteger(v) && v > 0],
    ["BODY_UPLOAD_TIMEOUT_MS", BODY_UPLOAD_TIMEOUT, "positive integer (ms)", (v) => Number.isInteger(v) && v > 0],
    ["SLOW_RESPONSE_MS", SLOW_RESPONSE_MS_INT, "positive integer (ms)", (v) => Number.isInteger(v) && v > 0],
    ["WARMUP_INTERVAL_MS", WARMUP_INTERVAL, "positive integer (ms)", (v) => Number.isInteger(v) && v > 0],
    ["DISCOVERY_INTERVAL_MS", DISCOVERY_INTERVAL, "positive integer (ms)", (v) => Number.isInteger(v) && v > 0],
    ["MAX_RETRIES", MAX_RETRIES_NUM, "integer >= 0", (v) => Number.isInteger(v) && v >= 0],
    ["RETRY_DELAY_MS", RETRY_DELAY, "integer >= 0 (ms)", (v) => Number.isInteger(v) && v >= 0],
    ["TARGET_PROTOCOL", TARGET_PROTOCOL, "\"http\" or \"https\"", (v) => v === "http" || v === "https"],
    ["LISTEN_ADDRESS", LISTEN_ADDRESS, "non-empty IP/hostname", (v) => typeof v === "string" && v.length > 0],
  ];
  for (const [name, value, expected, ok] of checks) {
    if (!ok(value)) {
      throw new Error(
        `Invalid configuration: ${name}="${value}" — expected ${expected}. ` +
        `Check your .env / environment variables and restart.`
      );
    }
  }
}

export const AGENT = new UPSTREAM_MODULE.Agent({
  keepAlive: true,
  keepAliveMsecs: 1000,
  maxSockets: 64,
  maxFreeSockets: 16,
  scheduling: "lifo",
});
