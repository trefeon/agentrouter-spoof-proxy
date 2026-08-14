// Package proxy implements the pure-core helpers of the AgentRouter spoof
// proxy, ported from src/utils.mjs, src/errors.mjs and src/status-code.mjs.
package proxy

import "errors"

// Sentinel errors mirror src/errors.mjs error codes. The error text IS the
// code string, matching the {error.code} value sent in API responses for
// 9Router compatibility.
var (
	ErrTimeout  = errors.New("EUPSTREAM_TIMEOUT") // → 504
	ErrUpstream = errors.New("EUPSTREAM")         // → 502
	ErrCircuit  = errors.New("ECIRCUIT_OPEN")     // → 503
	ErrInternal = errors.New("EINTERNAL")         // → 500
)

// CodeToStatus maps a sentinel (or wrapped) error to the HTTP status used by
// status-code.mjs codeToStatus(): 504/502/503 for the timeout/upstream/circuit
// sentinels, 500 for everything else (including nil and unknown errors).
// ErrInternal intentionally lands in the 500 default branch.
func CodeToStatus(err error) int {
	switch {
	case errors.Is(err, ErrTimeout):
		return 504
	case errors.Is(err, ErrUpstream):
		return 502
	case errors.Is(err, ErrCircuit):
		return 503
	default:
		return 500
	}
}
