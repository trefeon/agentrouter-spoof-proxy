import { E_TIMEOUT, E_UPSTREAM, E_CIRCUIT, E_INTERNAL } from "./errors.mjs";

const CODE_TO_STATUS = {
  [E_TIMEOUT]: 504,
  [E_UPSTREAM]: 502,
  [E_CIRCUIT]: 503,
  [E_INTERNAL]: 500,
};

export function codeToStatus(code) {
  return CODE_TO_STATUS[code] || 500;
}