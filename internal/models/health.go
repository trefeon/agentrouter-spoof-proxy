package models

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Health timing constants, mirroring src/models/health.mjs.
const (
	probeTimeout  = 8 * time.Second   // per-probe deadline
	exhaustLockMs = 120 * time.Second // 429 rate-limit lock
	degradeLockMs = 60 * time.Second  // degraded (slow/empty/sse-timeout) lock
)

var (
	// probeInterval is the recovery-probe cadence (60s). A variable so tests
	// can shorten it.
	probeInterval = 60 * time.Second
	// backoff is the consecutive-failure cooldown ladder:
	// 30s, 1m, 2m, 5m, 10m.
	backoff = []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 300 * time.Second, 600 * time.Second}
)

// Health tracks per-model failure locks, ported from src/models/health.mjs.
// failedUntil holds unix-millisecond expiry timestamps; failCounts holds the
// consecutive-failure count that drives the BACKOFF ladder.
type Health struct {
	mu          sync.Mutex
	failedUntil map[string]int64
	failCounts  map[string]int
}

// NewHealth returns an empty health tracker (all models healthy).
func NewHealth() *Health {
	return &Health{
		failedUntil: make(map[string]int64),
		failCounts:  make(map[string]int),
	}
}

// IsHealthy reports whether the model has no lock or its lock has expired.
func (h *Health) IsHealthy(modelID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	until, ok := h.failedUntil[modelID]
	if !ok {
		return true
	}
	return time.Now().UnixMilli() > until
}

// HealthyModels filters the given models down to the currently healthy ones.
func (h *Health) HealthyModels(models []Model) []Model {
	var out []Model
	for _, m := range models {
		if h.IsHealthy(m.ID) {
			out = append(out, m)
		}
	}
	return out
}

// MarkFailed locks a model after a 5xx failure (4xx never locks, mirroring the
// Node `statusCode >= 500` guard). Each consecutive failure walks up the
// BACKOFF ladder; the cooldown caps at the last rung (600s).
func (h *Health) MarkFailed(modelID string, statusCode int) {
	if modelID == "" || statusCode < 500 {
		return
	}
	h.mu.Lock()
	count := h.failCounts[modelID] + 1
	h.failCounts[modelID] = count
	cooldown := backoff[ladderIndex(count)]
	h.failedUntil[modelID] = time.Now().UnixMilli() + cooldown.Milliseconds()
	h.mu.Unlock()
	slog.Info(fmt.Sprintf("MODEL UNHEALTHY: %s (%d, #%d) — locked for %ds", modelID, statusCode, count, int(cooldown.Seconds())))
}

// MarkExhausted locks a rate-limited model for 120s (429, src/health.mjs
// markModelExhausted). The failure count is left untouched.
func (h *Health) MarkExhausted(modelID string) {
	if modelID == "" {
		return
	}
	h.mu.Lock()
	h.failedUntil[modelID] = time.Now().UnixMilli() + exhaustLockMs.Milliseconds()
	h.mu.Unlock()
	slog.Info(fmt.Sprintf("MODEL EXHAUSTED (429): %s — locked for %ds", modelID, int(exhaustLockMs.Seconds())))
}

// MarkDegraded locks a degraded model (slow/empty/sse-timeout) for 60s.
func (h *Health) MarkDegraded(modelID, reason string) {
	if modelID == "" {
		return
	}
	if reason == "" {
		reason = "degraded"
	}
	h.mu.Lock()
	h.failedUntil[modelID] = time.Now().UnixMilli() + degradeLockMs.Milliseconds()
	h.mu.Unlock()
	slog.Info(fmt.Sprintf("MODEL DEGRADED: %s (%s) — locked for %ds", modelID, reason, int(degradeLockMs.Seconds())))
}

// ProbeLoop runs the recovery-probe goroutine until ctx is canceled: every
// probeInterval it probes each model whose lock has expired with a POST
// /v1/messages (8s timeout, spoof headers + current WAF cookie). A 200 clears
// the lock; anything else extends it with the next BACKOFF step.
//
// The loop is driven entirely by ctx — cancel it to stop (no separate Stop
// method, mirroring the plan's "keep simple" note).
func (h *Health) ProbeLoop(ctx context.Context, client *http.Client, host string, port int, getHeaders func() map[string]string, getCookie func() string) {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.probeOnce(ctx, client, host, port, getHeaders, getCookie)
		}
	}
}

// probeOnce snapshots the expired locks and probes each one.
func (h *Health) probeOnce(ctx context.Context, client *http.Client, host string, port int, getHeaders func() map[string]string, getCookie func() string) {
	h.mu.Lock()
	if len(h.failedUntil) == 0 {
		h.mu.Unlock()
		return
	}
	now := time.Now().UnixMilli()
	var due []string
	for id, until := range h.failedUntil {
		if now > until {
			due = append(due, id)
		}
	}
	h.mu.Unlock()

	for _, id := range due {
		var headers map[string]string
		if getHeaders != nil {
			headers = getHeaders()
		}
		cookie := ""
		if getCookie != nil {
			cookie = getCookie()
		}
		if probeModel(ctx, client, host, port, id, cookie, headers) {
			h.clearLock(id)
		} else {
			h.extendLock(id)
		}
	}
}

func (h *Health) clearLock(modelID string) {
	h.mu.Lock()
	delete(h.failedUntil, modelID)
	delete(h.failCounts, modelID)
	h.mu.Unlock()
	slog.Info("MODEL RECOVERED: " + modelID)
}

// extendLock extends a still-failing model's lock using the next BACKOFF step
// (the probe-loop path from src/models/health.mjs — distinct from MarkFailed,
// which is only called on live request failures).
func (h *Health) extendLock(modelID string) {
	h.mu.Lock()
	count := h.failCounts[modelID] + 1
	h.failCounts[modelID] = count
	cooldown := backoff[ladderIndex(count)]
	h.failedUntil[modelID] = time.Now().UnixMilli() + cooldown.Milliseconds()
	h.mu.Unlock()
	slog.Warn(fmt.Sprintf("MODEL STILL DOWN: %s (#%d) — extended for %ds", modelID, count, int(cooldown.Seconds())))
}

// ladderIndex clamps a 1-based failure count to the BACKOFF ladder.
func ladderIndex(count int) int {
	if count-1 >= len(backoff) {
		return len(backoff) - 1
	}
	return count - 1
}

// probeModel sends one recovery probe: POST /v1/messages with a minimal
// Anthropic payload, spoof headers and the current WAF cookie. Returns true
// only on HTTP 200. A fresh client (8s timeout) sharing the caller's
// Transport is used per probe.
func probeModel(ctx context.Context, client *http.Client, host string, port int, modelID, cookie string, headers map[string]string) bool {
	body, err := json.Marshal(map[string]any{
		"model":      modelID,
		"max_tokens": 1,
		"stream":     false,
		"messages":   []map[string]string{{"role": "user", "content": "."}},
	})
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL(host, port, "/v1/messages"), bytes.NewReader(body))
	if err != nil {
		return false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := clientWithTimeout(client, probeTimeout).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
