// Package resilience implements the upstream circuit breaker. Ported from
// src/resilience/circuit-breaker.mjs, behavior is the spec.
package resilience

import (
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// Circuit thresholds. Mirrors Node values: open after 5 consecutive
// failures, cooldown min(60s << (fails-5), 600s).
const (
	failThreshold  = 5
	baseCooldownMs = 60_000  // 60s for the 5th consecutive failure
	maxCooldownMs  = 600_000 // 10m cap
)

// Breaker tracks consecutive failures and opens the circuit at
// failThreshold. Only final 5xx and transport failures count. A success
// resets the count but does not close an already open circuit, the cooldown
// must still elapse, same as Node.
type Breaker struct {
	consecutiveFails atomic.Int64
	openUntilMs      atomic.Int64
}

// NewBreaker returns a closed breaker.
func NewBreaker() *Breaker {
	return &Breaker{}
}

// IsOpen reports whether the circuit is open and cooldown has not elapsed.
func (b *Breaker) IsOpen() bool {
	return time.Now().UnixMilli() <= b.openUntilMs.Load()
}

// RecordSuccess resets the failure count.
func (b *Breaker) RecordSuccess() {
	b.consecutiveFails.Store(0)
}

// RecordFailure adds one failure. At failThreshold the circuit opens for
// min(60s << (fails-5), 600s), growing with the count.
func (b *Breaker) RecordFailure() {
	fails := b.consecutiveFails.Add(1)
	if fails >= failThreshold {
		cooldownMs := int64(baseCooldownMs) << (fails - failThreshold)
		if cooldownMs > maxCooldownMs {
			cooldownMs = maxCooldownMs
		}
		b.openUntilMs.Store(time.Now().UnixMilli() + cooldownMs)
		slog.Warn(fmt.Sprintf("CIRCUIT OPEN for %ds (%d consecutive failures)", cooldownMs/1000, fails))
	}
}

// ConsecutiveFails returns the current failure count.
func (b *Breaker) ConsecutiveFails() int64 {
	return b.consecutiveFails.Load()
}
