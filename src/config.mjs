import http from "node:http";
import https from "node:https";

const {
  LISTEN_PORT = "8318",
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
  LOG_LEVEL = "info",
  SLOW_RESPONSE_MS = "30000",
} = process.env;

export const PORT = parseInt(LISTEN_PORT, 10);
export const TARGET_PORT_INT = parseInt(TARGET_PORT, 10);
export const TIMEOUT = parseInt(REQUEST_TIMEOUT_MS, 10);
export const WARMUP_INTERVAL = parseInt(WARMUP_INTERVAL_MS, 10);
export const MAX_RETRIES_NUM = parseInt(MAX_RETRIES, 10);
export const RETRY_DELAY = parseInt(RETRY_DELAY_MS, 10);
export const DISCOVERY_INTERVAL = parseInt(DISCOVERY_INTERVAL_MS, 10);
export const SSE_IDLE = parseInt(SSE_IDLE_TIMEOUT_MS, 10);
export const SSE_CHUNK_TIMEOUT = parseInt(SSE_CHUNK_TIMEOUT_MS, 10);
export const RESPONSE_TIMEOUT = parseInt(RESPONSE_TIMEOUT_MS, 10);
export const IS_DEBUG = LOG_LEVEL === "debug";
export const SLOW_RESPONSE_MS_INT = parseInt(SLOW_RESPONSE_MS, 10);

export const TARGET_HOST_VAL = TARGET_HOST;
export const MODELS_CSV_VAL = MODELS_CSV;
export const AR_API_KEY_VAL = AR_API_KEY;
export const INJECT_SYSTEM_PROMPT_VAL = INJECT_SYSTEM_PROMPT;

export const UPSTREAM_MODULE = TARGET_PROTOCOL === "http" ? http : https;

export const AGENT = new UPSTREAM_MODULE.Agent({
  keepAlive: true,
  keepAliveMsecs: 1000,
  maxSockets: 64,
  maxFreeSockets: 16,
  scheduling: "lifo",
});
