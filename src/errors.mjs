// Error construction + classification for the proxy.
// Codes map to HTTP statuses via status-code.mjs; the {error.code} string
// sent in API responses stays as-is for 9Router compat.

export const E_TIMEOUT = "EUPSTREAM_TIMEOUT";
export const E_UPSTREAM = "EUPSTREAM";
export const E_CIRCUIT = "ECIRCUIT_OPEN";
export const E_INTERNAL = "EINTERNAL";

const OUR_ERROR = Symbol("ourError");

export function buildError(message, code) {
  const err = new Error(message);
  err.code = code;
  err[OUR_ERROR] = true;
  return err;
}

export function isOurError(err) {
  return !!(err && err[OUR_ERROR]);
}