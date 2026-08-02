import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { buildError, isOurError, E_TIMEOUT, E_UPSTREAM, E_CIRCUIT, E_INTERNAL } from "../../src/errors.mjs";
import { codeToStatus } from "../../src/status-code.mjs";

describe("unit: buildError", () => {
  it("sets code and ourError marker", () => {
    const err = buildError("timeout", E_TIMEOUT);
    assert.ok(err instanceof Error);
    assert.equal(err.message, "timeout");
    assert.equal(err.code, E_TIMEOUT);
    assert.equal(isOurError(err), true);
  });
  it("isOurError false for plain errors", () => {
    assert.equal(isOurError(new Error("boom")), false);
    assert.equal(isOurError(null), false);
  });
});

describe("unit: codeToStatus", () => {
  it("maps known codes", () => {
    assert.equal(codeToStatus(E_TIMEOUT), 504);
    assert.equal(codeToStatus(E_UPSTREAM), 502);
    assert.equal(codeToStatus(E_CIRCUIT), 503);
    assert.equal(codeToStatus(E_INTERNAL), 500);
  });
  it("falls back to 500 for unknown code", () => {
    assert.equal(codeToStatus("BOGUS"), 500);
  });
});