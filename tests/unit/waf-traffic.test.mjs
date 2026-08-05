import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { getWafCookie, captureWafCookies, mergeWafCookies } from "../../src/auth/waf.mjs";

// ── WAF cookie refresh on live traffic (CHG-2) ──
//
// captureWafCookies merges Set-Cookie values from upstream API responses into
// the module store keyed by cookie NAME. Each test uses its OWN distinctive
// cookie values so assertions are order-independent: the singleton store may
// already hold warmup cookies from earlier tests, but a value like `acw_tc=u_a`
// can only ever be written by the test that asserts it.
describe("unit: captureWafCookies traffic refresh", () => {
  it("merges new cookie names into the store (union)", () => {
    captureWafCookies({ "set-cookie": ["acw_tc=u_a; Path=/"] });
    captureWafCookies({ "set-cookie": ["cdn_sec_tc=u_b; Path=/"] });
    const cookie = getWafCookie();
    assert.ok(cookie.includes("acw_tc=u_a"), `store should contain acw_tc=u_a, got: ${cookie}`);
    assert.ok(cookie.includes("cdn_sec_tc=u_b"), `store should contain cdn_sec_tc=u_b, got: ${cookie}`);
  });

  it("replaces a cookie by name, preserving unrelated names", () => {
    captureWafCookies({ "set-cookie": ["acw_tc=u_c; Path=/"] });
    const cookie = getWafCookie();
    assert.ok(cookie.includes("acw_tc=u_c"), `store should contain acw_tc=u_c, got: ${cookie}`);
    assert.ok(!cookie.includes("acw_tc=u_a"), `old acw_tc=u_a should be replaced, got: ${cookie}`);
    assert.ok(cookie.includes("cdn_sec_tc=u_b"), `cdn_sec_tc=u_b should be preserved, got: ${cookie}`);
  });

  it("ignores expired/empty-value cookies, leaving the store unchanged", () => {
    const before = getWafCookie();
    captureWafCookies({ "set-cookie": ["acw_tc=; max-age=0"] });
    assert.equal(getWafCookie(), before, "an empty acw_tc must not touch the store");
  });

  it("ignores non-WAF cookies, leaving the store unchanged", () => {
    const before = getWafCookie();
    captureWafCookies({ "set-cookie": ["session=u_d; Path=/"] });
    assert.equal(getWafCookie(), before, "a session cookie must not touch the store");
  });

  it("handles a single string set-cookie value", () => {
    captureWafCookies({ "set-cookie": "acw_tc=u_e; Path=/" });
    assert.ok(getWafCookie().includes("acw_tc=u_e"), "string-form set-cookie should be captured");
  });

  it("warmup merge keeps traffic-captured cookies that warmup did not return", () => {
    // warmup() uses the same merge: a warmup GET "/" returning only acw_tc must
    // not wipe a cdn_sec_tc that was captured from a live API response.
    const store = ["acw_tc=w_a", "cdn_sec_tc=w_b"];
    const merged = mergeWafCookies(store, ["acw_tc=w_fresh"]);
    assert.ok(merged.includes("acw_tc=w_fresh"), "fresh warmup value wins for acw_tc");
    assert.ok(merged.includes("cdn_sec_tc=w_b"), "traffic-only cdn_sec_tc survives warmup");
    assert.ok(!merged.includes("acw_tc=w_a"), "old acw_tc replaced");
  });
});