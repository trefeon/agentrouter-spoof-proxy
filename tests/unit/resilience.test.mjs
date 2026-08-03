import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import path from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import {
  isCircuitOpen, getConsecutiveFails, recordSuccess, recordFailure,
} from "../../src/resilience/circuit-breaker.mjs";
import { validateConfig } from "../../src/config.mjs";

const PROXY_DIR = path.resolve(import.meta.dirname, "../..");

describe("unit: circuit breaker", () => {
  it("is closed with zero failures initially", () => {
    recordSuccess();
    assert.equal(isCircuitOpen(), false);
    assert.equal(getConsecutiveFails(), 0);
  });

  it("failure counting increments monotonically while closed", () => {
    recordSuccess();
    recordFailure();
    recordFailure();
    assert.equal(getConsecutiveFails(), 2);
    assert.equal(isCircuitOpen(), false);
  });

  it("opens after 5 consecutive failures", () => {
    recordSuccess();
    for (let i = 0; i < 5; i++) recordFailure();
    assert.equal(getConsecutiveFails(), 5);
    assert.equal(isCircuitOpen(), true);
  });

  it("success resets the consecutive-failure run", () => {
    const before = getConsecutiveFails();
    assert.ok(before >= 5, "failure run accumulated by the previous test");
    recordSuccess();
    assert.equal(getConsecutiveFails(), 0);
  });
});

describe("unit: config validation", () => {
  it("accepts the repository's default/valid environment", () => {
    assert.doesNotThrow(() => validateConfig());
  });

  it("proxy.mjs exits non-zero with a clear message on invalid config", async () => {
    const child = spawn(process.execPath, ["proxy.mjs"], {
      cwd: PROXY_DIR,
      env: { ...process.env, LISTEN_PORT: "not-a-port" },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stderr = "";
    child.stderr.on("data", (d) => { stderr += d; });
    const code = await new Promise((resolve) => child.once("exit", resolve));
    assert.equal(code, 1, `invalid config must exit non-zero, got ${code}`);
    assert.match(stderr, /Invalid configuration: LISTEN_PORT/, `expected actionable message, got: ${stderr.slice(0, 200)}`);
  });

  it("exit is immediate (no server started) for invalid configuration", async () => {
    const started = Date.now();
    const child = spawn(process.execPath, ["proxy.mjs"], {
      cwd: PROXY_DIR,
      env: { ...process.env, SSE_CHUNK_TIMEOUT_MS: "-5" },
      stdio: ["ignore", "ignore", "pipe"],
    });
    const code = await new Promise((resolve) => child.once("exit", resolve));
    assert.equal(code, 1);
    assert.ok(Date.now() - started < 5000, "invalid config should fail fast");
    await sleep(10);
  });
});
