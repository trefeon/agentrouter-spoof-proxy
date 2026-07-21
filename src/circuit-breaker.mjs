import { log } from "./logger.mjs";

let consecutiveFails = 0;
let circuitOpenUntil = 0;

export function isCircuitOpen() {
  return Date.now() <= circuitOpenUntil;
}

export function recordSuccess() {
  consecutiveFails = 0;
}

export function recordFailure() {
  consecutiveFails++;
  if (consecutiveFails >= 5) {
    circuitOpenUntil = Date.now() + Math.min(60000 * Math.pow(2, consecutiveFails - 5), 600000);
    log(new Date().toISOString(), `CIRCUIT OPEN for ${(circuitOpenUntil - Date.now()) / 1000}s (${consecutiveFails} consecutive failures)`);
  }
}

export function getConsecutiveFails() {
  return consecutiveFails;
}
