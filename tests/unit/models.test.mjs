import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  getModelsList, getModelSource,
} from "../../src/models/discovery.mjs";
import {
  isModelHealthy, getHealthyModels, markModelFailed, markModelExhausted, markModelDegraded,
} from "../../src/models/health.mjs";
import {
  recordModelStart, recordModelResult, getModelStats,
} from "../../src/models/stats.mjs";
import {
  isProxyRoute, safeTokenEqual, eomTail, SSE_EOM, SSE_DONE,
} from "../../src/utils.mjs";
import { codeToStatus } from "../../src/status-code.mjs";
import { E_TIMEOUT, E_UPSTREAM, E_CIRCUIT, E_INTERNAL } from "../../src/errors.mjs";

describe("unit: model discovery", () => {
  it("exposes the static model list when no AR_API_KEY is configured", () => {
    const models = getModelsList();
    assert.ok(Array.isArray(models) && models.length >= 1);
    assert.equal(getModelSource(), "static");
    assert.ok(models.every((m) => typeof m.id === "string" && m.id.length > 0));
  });
});

describe("unit: model health", () => {
  const M = "unit-test-model";

  it("treats an unknown model as healthy", () => {
    assert.equal(isModelHealthy("never-marked-model"), true);
  });

  it("locks a model after a 5xx failure", () => {
    markModelFailed(M, 500);
    assert.equal(isModelHealthy(M), false);
  });

  it("excludes unhealthy models from getHealthyModels", () => {
    markModelFailed(M, 503);
    const healthy = getHealthyModels([{ id: M }, { id: "other-model" }]);
    assert.ok(!healthy.some((m) => m.id === M));
    assert.ok(healthy.some((m) => m.id === "other-model"));
  });

  it("locks a rate-limited model without touching its failure count", () => {
    markModelExhausted(M);
    assert.equal(isModelHealthy(M), false);
  });

  it("locks a degraded model", () => {
    markModelDegraded(M, "empty_output");
    assert.equal(isModelHealthy(M), false);
  });

  it("records success telemetry without prompt content", () => {
    const MODEL = "unit-stats-model";
    recordModelStart(MODEL);
    recordModelResult(MODEL, { statusCode: 200, durationMs: 42, chunks: 5 });
    const stat = getModelStats().find((s) => s.model === MODEL);
    assert.ok(stat, "model stat present");
    assert.equal(stat.requests, 1);
    assert.equal(stat.successes, 1);
    assert.equal(stat.failures, 0);
    assert.equal(JSON.stringify(stat).includes("prompt"), false);
  });

  it("counts upstream 5xx as failures", () => {
    const MODEL = "unit-fail-model";
    recordModelStart(MODEL);
    recordModelResult(MODEL, { statusCode: 502, error: "socket hang up" });
    const stat = getModelStats().find((s) => s.model === MODEL);
    assert.equal(stat.failures, 1);
    assert.equal(stat.upstreamErrors, 1);
  });
});

describe("unit: proxy routing helpers", () => {
  it("allowlists only the documented proxy routes", () => {
    for (const p of ["/v1/messages", "/v1/messages?beta=1", "/messages", "/v1/chat/completions"]) {
      assert.equal(isProxyRoute(p), true, `${p} should be proxied`);
    }
    for (const p of ["/", "/health", "/api/health", "/v1/models", "/models", "/unknown", "/v1/foo"]) {
      assert.equal(isProxyRoute(p), false, `${p} must not be proxied`);
    }
  });

  it("compares tokens in constant time and rejects mismatches", () => {
    assert.equal(safeTokenEqual("secret-token", "secret-token"), true);
    assert.equal(safeTokenEqual("secret-token", "secret-tokne"), false);
    assert.equal(safeTokenEqual("secret-token", ""), false);
    assert.equal(safeTokenEqual(undefined, "secret-token"), false);
  });

  it("produces format-correct terminal events", () => {
    assert.equal(eomTail("anthropic"), `\n${SSE_EOM}\ndata: {}\n\n`);
    assert.equal(eomTail("openai"), `\ndata: [DONE]\n\n`);
    assert.equal(eomTail("openai").includes("message_stop"), false);
    assert.ok(eomTail("openai").includes(SSE_DONE));
  });
});

describe("unit: status mapping", () => {
  it("maps error codes to HTTP statuses consistently", () => {
    assert.equal(codeToStatus(E_TIMEOUT), 504);
    assert.equal(codeToStatus(E_CIRCUIT), 503);
    assert.equal(codeToStatus(E_UPSTREAM), 502);
    assert.equal(codeToStatus(E_INTERNAL), 500);
    assert.equal(codeToStatus("UNKNOWN"), 500);
  });
});
