package resilience

import (
	"math"
	"testing"
	"time"
)

// checkCooldown asserts that the circuit opened for approximately the expected
// cooldown (measured from openUntilMs minus now, within a 2s tolerance).
func checkCooldown(t *testing.T, b *Breaker, want time.Duration) {
	t.Helper()
	until := b.openUntilMs.Load()
	now := time.Now().UnixMilli()
	got := time.Duration(until-now) * time.Millisecond
	if math.Abs(float64(got-want)) > float64(2*time.Second) {
		t.Fatalf("cooldown = %v, want ~%v", got, want)
	}
}

func TestBreakerClosedInitially(t *testing.T) {
	b := NewBreaker()
	b.RecordSuccess() // reset any cross-test contamination
	if b.IsOpen() {
		t.Fatal("breaker must start closed")
	}
	if b.ConsecutiveFails() != 0 {
		t.Fatalf("ConsecutiveFails() = %d, want 0", b.ConsecutiveFails())
	}
}

func TestBreakerCountsWhileClosed(t *testing.T) {
	b := NewBreaker()
	b.RecordSuccess()
	b.RecordFailure()
	b.RecordFailure()
	if got := b.ConsecutiveFails(); got != 2 {
		t.Fatalf("ConsecutiveFails() = %d, want 2", got)
	}
	if b.IsOpen() {
		t.Fatal("breaker must stay closed below the threshold")
	}
}

func TestBreakerOpensAfterFive(t *testing.T) {
	b := NewBreaker()
	b.RecordSuccess()
	for i := 0; i < 5; i++ {
		b.RecordFailure()
	}
	if got := b.ConsecutiveFails(); got != 5 {
		t.Fatalf("ConsecutiveFails() = %d, want 5", got)
	}
	if !b.IsOpen() {
		t.Fatal("breaker must open after 5 consecutive failures")
	}
}

func TestBreakerCooldownGrowsThenCaps(t *testing.T) {
	b := NewBreaker()
	b.RecordSuccess()
	for i := 0; i < 5; i++ {
		b.RecordFailure()
	}
	checkCooldown(t, b, 60*time.Second) // 60s << 0

	b.RecordFailure()
	checkCooldown(t, b, 120*time.Second) // 60s << 1

	b.RecordFailure()
	checkCooldown(t, b, 240*time.Second) // 60s << 2

	b.RecordFailure()
	checkCooldown(t, b, 480*time.Second) // 60s << 3

	b.RecordFailure() // 9th: 60s << 4 = 960s → capped at 600s
	checkCooldown(t, b, 600*time.Second)

	b.RecordFailure() // 10th: capped too
	checkCooldown(t, b, 600*time.Second)
}

func TestBreakerSuccessResetsRun(t *testing.T) {
	b := NewBreaker()
	b.RecordSuccess()
	for i := 0; i < 3; i++ {
		b.RecordFailure()
	}
	if b.ConsecutiveFails() != 3 {
		t.Fatalf("ConsecutiveFails() = %d, want 3", b.ConsecutiveFails())
	}
	b.RecordSuccess()
	if got := b.ConsecutiveFails(); got != 0 {
		t.Fatalf("RecordSuccess must reset the run, got %d", got)
	}
	if b.IsOpen() {
		t.Fatal("breaker must not be open after only 3 failures + reset")
	}
}

func TestBreakerOpenUntilBoundary(t *testing.T) {
	b := NewBreaker()
	now := time.Now().UnixMilli()
	b.openUntilMs.Store(now)
	if !b.IsOpen() {
		t.Fatal("IsOpen must be true while now <= openUntilMs")
	}
	b.openUntilMs.Store(now - 1)
	if b.IsOpen() {
		t.Fatal("IsOpen must be false once the cooldown elapsed")
	}
}
