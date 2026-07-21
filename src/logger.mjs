import { IS_DEBUG } from "./config.mjs";

export function log(ts, msg) {
  console.log(`[${ts}] ${msg}`);
}

export function logDebug(ts, msg) {
  if (IS_DEBUG) console.log(`[${ts}] [DEBUG] ${msg}`);
}
