package models

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// assertLocked checks that a model is locked for approximately wantCooldown
// with the expected consecutive-failure count.
func assertLocked(t *testing.T, h *Health, id string, wantCount int, wantCooldown time.Duration) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if got := h.failCounts[id]; got != wantCount {
		t.Fatalf("failCounts[%q] = %d, want %d", id, got, wantCount)
	}
	until, ok := h.failedUntil[id]
	if !ok {
		t.Fatalf("model %q not locked", id)
	}
	now := time.Now().UnixMilli()
	got := time.Duration(until-now) * time.Millisecond
	if math.Abs(float64(got-wantCooldown)) > float64(2*time.Second) {
		t.Fatalf("cooldown for %q = %v, want ~%v", id, got, wantCooldown)
	}
}

func TestHealthUnknownModelIsHealthy(t *testing.T) {
	h := NewHealth()
	if !h.IsHealthy("never-marked-model") {
		t.Fatal("an unknown model must be healthy")
	}
}

func TestHealthMarkFailedLadder(t *testing.T) {
	h := NewHealth()
	h.MarkFailed("m", 500)
	assertLocked(t, h, "m", 1, 30*time.Second)
	if h.IsHealthy("m") {
		t.Fatal("model must be unhealthy after a 5xx")
	}

	h.MarkFailed("m", 503)
	assertLocked(t, h, "m", 2, 60*time.Second)

	h.MarkFailed("m", 500)
	assertLocked(t, h, "m", 3, 120*time.Second)

	h.MarkFailed("m", 500)
	assertLocked(t, h, "m", 4, 300*time.Second)

	h.MarkFailed("m", 502)
	assertLocked(t, h, "m", 5, 600*time.Second)

	// Sixth failure stays capped on the last ladder rung.
	h.MarkFailed("m", 500)
	assertLocked(t, h, "m", 6, 600*time.Second)
}

func TestHealthMarkFailedIgnores4xx(t *testing.T) {
	h := NewHealth()
	h.MarkFailed("m", 400)
	h.MarkFailed("m", 429)
	if !h.IsHealthy("m") {
		t.Fatal("4xx must never lock a model")
	}
	h.mu.Lock()
	count := h.failCounts["m"]
	h.mu.Unlock()
	if count != 0 {
		t.Fatalf("failCounts = %d, want 0", count)
	}
}

func TestHealthMarkFailedEmptyID(t *testing.T) {
	h := NewHealth()
	h.MarkFailed("", 500)
	h.mu.Lock()
	locks := len(h.failedUntil)
	h.mu.Unlock()
	if locks != 0 {
		t.Fatalf("MarkFailed with empty id created %d locks", locks)
	}
}

func TestHealthIsHealthyAfterExpiry(t *testing.T) {
	h := NewHealth()
	h.MarkFailed("m", 500)
	if h.IsHealthy("m") {
		t.Fatal("model should be unhealthy while locked")
	}
	// Force the lock to expire.
	h.mu.Lock()
	h.failedUntil["m"] = time.Now().UnixMilli() - 1
	h.mu.Unlock()
	if !h.IsHealthy("m") {
		t.Fatal("model should recover once the lock expires")
	}
}

func TestHealthHealthyModelsFilters(t *testing.T) {
	h := NewHealth()
	h.MarkFailed("bad", 503)
	models := []Model{{ID: "bad"}, {ID: "good"}}
	got := h.HealthyModels(models)
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("HealthyModels() = %+v, want only the good model", got)
	}
}

func TestHealthMarkExhausted(t *testing.T) {
	h := NewHealth()
	h.MarkExhausted("m")
	assertLocked(t, h, "m", 0, 120*time.Second)
	if h.IsHealthy("m") {
		t.Fatal("model must be locked after MarkExhausted")
	}
}

func TestHealthMarkDegraded(t *testing.T) {
	h := NewHealth()
	h.MarkDegraded("m", "empty_output")
	assertLocked(t, h, "m", 0, 60*time.Second)
	if h.IsHealthy("m") {
		t.Fatal("model must be locked after MarkDegraded")
	}

	// Default reason.
	h2 := NewHealth()
	h2.MarkDegraded("m2", "")
	assertLocked(t, h2, "m2", 0, 60*time.Second)
}

// probeSrv spins up a mock upstream that records probe requests and responds
// with the given status.
func probeSrv(t *testing.T, status int) (*httptest.Server, *probeLog) {
	t.Helper()
	log := &probeLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.mu.Lock()
		log.count++
		log.path = r.URL.Path
		log.method = r.Method
		log.authVersion = r.Header.Get("Anthropic-Version")
		log.cookie = r.Header.Get("Cookie")
		log.ct = r.Header.Get("Content-Type")
		log.mu.Unlock()
		w.WriteHeader(status)
		fmt.Fprint(w, `{"id":"probe","type":"message","model":"probe","content":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

type probeLog struct {
	mu          sync.Mutex
	count       int
	path        string
	method      string
	authVersion string
	cookie      string
	ct          string
}

func splitProbeHostPort(t *testing.T, u string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(stripScheme(u))
	if err != nil {
		t.Fatalf("split %q: %v", u, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return host, port
}

func TestProbeModelSuccess(t *testing.T) {
	srv, log := probeSrv(t, http.StatusOK)
	host, port := splitProbeHostPort(t, srv.URL)
	headers := map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": "beta-set"}
	ok := probeModel(context.Background(), srv.Client(), host, port, "m1", "acw_tc=probe", headers)
	if !ok {
		t.Fatal("probeModel must return true on HTTP 200")
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.count != 1 || log.path != "/v1/messages" || log.method != http.MethodPost {
		t.Fatalf("probe request = %s %s (count %d), want POST /v1/messages", log.method, log.path, log.count)
	}
	if log.authVersion != "2023-06-01" || log.ct != "application/json" {
		t.Fatalf("probe headers wrong: Anthropic-Version=%q Content-Type=%q", log.authVersion, log.ct)
	}
	if log.cookie != "acw_tc=probe" {
		t.Fatalf("probe Cookie = %q, want acw_tc=probe", log.cookie)
	}
}

func TestProbeModelNon200(t *testing.T) {
	srv, _ := probeSrv(t, http.StatusServiceUnavailable)
	host, port := splitProbeHostPort(t, srv.URL)
	if probeModel(context.Background(), srv.Client(), host, port, "m1", "", nil) {
		t.Fatal("probeModel must return false on non-200")
	}
}

func TestProbeModelNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	host, port := splitProbeHostPort(t, url)
	if probeModel(context.Background(), &http.Client{}, host, port, "m1", "", nil) {
		t.Fatal("probeModel must return false on transport error")
	}
}

func TestProbeModelNoCookieHeader(t *testing.T) {
	srv, log := probeSrv(t, http.StatusOK)
	host, port := splitProbeHostPort(t, srv.URL)
	if !probeModel(context.Background(), srv.Client(), host, port, "m1", "", map[string]string{"Anthropic-Version": "2023-06-01"}) {
		t.Fatal("probe must succeed")
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.cookie != "" {
		t.Fatalf("probe with empty cookie must not set a Cookie header, got %q", log.cookie)
	}
}

func TestProbeLoopRecoversModel(t *testing.T) {
	oldInterval := probeInterval
	probeInterval = 5 * time.Millisecond
	t.Cleanup(func() { probeInterval = oldInterval })

	srv, log := probeSrv(t, http.StatusOK)
	host, port := splitProbeHostPort(t, srv.URL)

	h := NewHealth()
	h.MarkFailed("probe-model", 500)
	// Expire the lock so the first probe tick picks it up. NOTE: this makes
	// IsHealthy true immediately, so recovery is signaled by clearLock
	// deleting the failCounts entry (pre-expiry alone never deletes it).
	h.mu.Lock()
	h.failedUntil["probe-model"] = time.Now().UnixMilli() - 1
	h.mu.Unlock()

	headers := func() map[string]string { return map[string]string{"Anthropic-Version": "2023-06-01"} }
	cookie := func() string { return "acw_tc=probe" }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.ProbeLoop(ctx, srv.Client(), host, port, headers, cookie)
		close(done)
	}()

	// A successful probe clears the lock (deletes the failCounts entry).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		_, locked := h.failCounts["probe-model"]
		h.mu.Unlock()
		if !locked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.mu.Lock()
	_, stillLocked := h.failCounts["probe-model"]
	h.mu.Unlock()
	if stillLocked {
		t.Fatal("probe loop did not recover the failed model (lock not cleared)")
	}
	if !h.IsHealthy("probe-model") {
		t.Fatal("recovered model must be healthy")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ProbeLoop did not stop after ctx cancel")
	}

	log.mu.Lock()
	defer log.mu.Unlock()
	if log.count == 0 {
		t.Fatal("probe loop never probed the upstream")
	}
	if log.path != "/v1/messages" {
		t.Fatalf("probe hit path %q, want /v1/messages", log.path)
	}
	if log.cookie != "acw_tc=probe" {
		t.Fatalf("probe Cookie = %q, want acw_tc=probe", log.cookie)
	}
	if log.authVersion != "2023-06-01" {
		t.Fatalf("probe missing spoof headers, Anthropic-Version=%q", log.authVersion)
	}
}

func TestProbeLoopExtendsStillDown(t *testing.T) {
	oldInterval := probeInterval
	probeInterval = 5 * time.Millisecond
	t.Cleanup(func() { probeInterval = oldInterval })

	srv, _ := probeSrv(t, http.StatusServiceUnavailable) // upstream keeps failing
	host, port := splitProbeHostPort(t, srv.URL)

	h := NewHealth()
	h.MarkFailed("m", 500)
	h.mu.Lock()
	h.failedUntil["m"] = time.Now().UnixMilli() - 1
	h.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.ProbeLoop(ctx, srv.Client(), host, port, nil, nil)
		close(done)
	}()

	// Wait for at least one extend: failCounts climbs past the initial mark.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		count := h.failCounts["m"]
		h.mu.Unlock()
		if count >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.IsHealthy("m") {
		t.Fatal("model must still be locked while the upstream is down")
	}
	h.mu.Lock()
	count := h.failCounts["m"]
	h.mu.Unlock()
	if count < 2 {
		t.Fatalf("failCounts = %d, want >= 2 after failed probes extended the lock", count)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ProbeLoop did not stop after ctx cancel")
	}
}

func TestProbeLoopSkipsWhenNoLocks(t *testing.T) {
	oldInterval := probeInterval
	probeInterval = 5 * time.Millisecond
	t.Cleanup(func() { probeInterval = oldInterval })

	srv, log := probeSrv(t, http.StatusOK)
	host, port := splitProbeHostPort(t, srv.URL)

	h := NewHealth() // no locks at all
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.ProbeLoop(ctx, srv.Client(), host, port, nil, nil)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond) // several ticks with nothing to do
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ProbeLoop did not stop after ctx cancel")
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.count != 0 {
		t.Fatalf("probe loop must skip probing when there are no locks, made %d probes", log.count)
	}
}
