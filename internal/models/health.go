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

// Health timing mirrors src/models/health.mjs.
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

// Health tracks per-model failure locks. Ported from src/models/health.mjs.
// failedUntil holds expiry timestamps in unix millis, failCounts tracks
// consecutive failures that drive the BACKOFF ladder.
type Health struct {
	mu          sync.Mutex
	failedUntil map[string]int64
	failCounts  map[string]int
}

// NewHealth returns an empty tracker, all models healthy.
func NewHealth() *Health {
	return &Health{
		failedUntil: make(map[string]int64),
		failCounts:  make(map[string]int),
	}
}

// IsHealthy reports whether the model is not locked or its lock expired.
func (h *Health) IsHealthy(modelID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	until, ok := h.failedUntil[modelID]
	if !ok {
		return true
	}
	return time.Now().UnixMilli() > until
}

// HealthyModels returns only the currently healthy models.
func (h *Health) HealthyModels(models []Model) []Model {
	var out []Model
	for _, m := range models {
		if h.IsHealthy(m.ID) {
			out = append(out, m)
		}
	}
	return out
}

// MarkFailed locks a model after a 5xx. 4xx never locks, mirrors Node
// statusCode >= 500. Each consecutive failure steps up the BACKOFF ladder,
// capped at 600s.
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
	slog.Info(fmt.Sprintf("MODEL UNHEALTHY: %s (%d, #%d), locked for %ds", modelID, statusCode, count, int(cooldown.Seconds())))
}

// MarkExhausted locks a 429 rate-limited model for 120s. Failure count
// is not changed (src/health.mjs markModelExhausted).
func (h *Health) MarkExhausted(modelID string) {
	if modelID == "" {
		return
	}
	h.mu.Lock()
	h.failedUntil[modelID] = time.Now().UnixMilli() + exhaustLockMs.Milliseconds()
	h.mu.Unlock()
	slog.Info(fmt.Sprintf("MODEL EXHAUSTED (429): %s, locked for %ds", modelID, int(exhaustLockMs.Seconds())))
}

// MarkDegraded locks a slow, empty or SSE-timeout model for 60s.
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
	slog.Info(fmt.Sprintf("MODEL DEGRADED: %s (%s), locked for %ds", modelID, reason, int(degradeLockMs.Seconds())))
}

// ProbeLoop runs recovery probes until ctx is canceled. Every
// probeInterval it probes expired locks with POST /v1/messages (8s timeout,
// spoof headers and current WAF cookie). 200 clears the lock, anything else
// extends it. Cancel ctx to stop, there is no separate Stop method.
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

// probeOnce finds expired locks and probes each one.
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

// extendLock extends a still-failing lock with the next BACKOFF step.
// This is the probe path, separate from MarkFailed which is for live failures.
func (h *Health) extendLock(modelID string) {
	h.mu.Lock()
	count := h.failCounts[modelID] + 1
	h.failCounts[modelID] = count
	cooldown := backoff[ladderIndex(count)]
	h.failedUntil[modelID] = time.Now().UnixMilli() + cooldown.Milliseconds()
	h.mu.Unlock()
	slog.Warn(fmt.Sprintf("MODEL STILL DOWN: %s (#%d), extended for %ds", modelID, count, int(cooldown.Seconds())))
}

// ladderIndex clamps a 1-based count to the BACKOFF ladder.
func ladderIndex(count int) int {
	if count-1 >= len(backoff) {
		return len(backoff) - 1
	}
	return count - 1
}

// probeModel sends one recovery probe with POST /v1/messages, minimal
// payload, spoof headers and WAF cookie. Returns true only on 200. Uses
// a fresh 8s client that shares the caller's Transport.
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
