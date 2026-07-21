import { SLOW_RESPONSE_MS_INT } from "../config.mjs";

const stats = new Map();

function get(modelId) {
  const model = modelId || "unknown";
  if (!stats.has(model)) {
    stats.set(model, {
      model,
      requests: 0,
      successes: 0,
      failures: 0,
      emptyOutputs: 0,
      slowResponses: 0,
      wafBlocks: 0,
      rateLimits: 0,
      upstreamErrors: 0,
      totalMs: 0,
      maxMs: 0,
      totalChunks: 0,
      lastStatus: null,
      lastError: null,
      lastSeen: null,
    });
  }
  return stats.get(model);
}

export function recordModelStart(modelId) {
  const s = get(modelId);
  s.requests++;
  s.lastSeen = new Date().toISOString();
}

export function recordModelResult(modelId, { statusCode, durationMs = 0, chunks = 0, error = null, emptyOutput = false, wafBlock = false } = {}) {
  const s = get(modelId);
  s.lastStatus = statusCode || null;
  s.lastError = error || null;
  s.lastSeen = new Date().toISOString();
  s.totalMs += durationMs;
  s.maxMs = Math.max(s.maxMs, durationMs);
  s.totalChunks += chunks;

  if (statusCode >= 200 && statusCode < 300 && !error) s.successes++;
  if (error || statusCode >= 400 || emptyOutput) s.failures++;
  if (emptyOutput) s.emptyOutputs++;
  if (wafBlock) s.wafBlocks++;
  if (statusCode === 429) s.rateLimits++;
  if (statusCode >= 500) s.upstreamErrors++;
  if (durationMs >= SLOW_RESPONSE_MS_INT) s.slowResponses++;
}

export function getModelStats() {
  return [...stats.values()].map((s) => ({
    ...s,
    avgMs: s.requests ? Math.round(s.totalMs / s.requests) : 0,
    avgChunks: s.requests ? Math.round(s.totalChunks / s.requests) : 0,
  })).sort((a, b) => b.lastSeen?.localeCompare(a.lastSeen || "") || 0);
}
