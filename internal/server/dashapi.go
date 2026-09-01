package server

// Dashboard API: the admin web UI reads these endpoints and the embedded page
// is served at the root path. Every route is gated behind proxy auth exactly
// like the proxy routes.

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"

	"github.com/trefeon/agentrouter-spoof-proxy/internal/dashboard"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/logstore"
)

// dashAuth verifies proxy auth for a dashboard route and writes the 401
// response when it fails.
func (s *Server) dashAuth(w http.ResponseWriter, r *http.Request) bool {
	if requireProxyAuth(r, s.cfg.ProxyAuthToken) {
		return true
	}
	rejectLocally(w, r, http.StatusUnauthorized, "unauthorized", "Invalid or missing proxy auth token")
	return false
}

// handleDashboard serves the embedded admin UI at the exact root path.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if !s.dashAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(dashboard.HTML()))
}

// handleDashStatus serves GET /api/status with live proxy state.
func (s *Server) handleDashStatus(w http.ResponseWriter, r *http.Request) {
	if !s.dashAuth(w, r) {
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"upstream":         s.cfg.Upstream(),
		"modelSource":      s.discovery.Source(),
		"staticModels":     len(s.cfg.StaticModelIDs()),
		"availableModels":  len(s.discovery.List()),
		"activeStreams":    s.active.Load(),
		"wafCookie":        s.wafStore.Get() != "",
		"circuitOpen":      s.breaker.IsOpen(),
		"consecutiveFails": s.breaker.ConsecutiveFails(),
		"mode":             s.Mode(),
	})
}

// handleDashConfig serves GET /api/config with connection, mode and model
// settings plus the check-in configuration.
func (s *Server) handleDashConfig(w http.ResponseWriter, r *http.Request) {
	if !s.dashAuth(w, r) {
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"baseUrl":    "http://" + net.JoinHostPort(s.cfg.ListenAddr, strconv.Itoa(s.cfg.ListenPort)),
		"listenAddr": s.cfg.ListenAddr,
		"listenPort": s.cfg.ListenPort,
		"upstream":   s.cfg.Upstream(),
		"mode":       s.Mode(),
		"proxyAuth":  s.cfg.ProxyAuthToken != "",
		"models":     s.health.HealthyModels(s.discovery.List()),
		"checkin": map[string]any{
			"configured":      s.cfg.CheckinWorkdir != "",
			"cmd":             s.cfg.CheckinCmd,
			"args":            s.cfg.CheckinArgs,
			"workdir":         s.cfg.CheckinWorkdir,
			"scheduleEnabled": s.cfg.CheckinSchedule,
			"windowStart":     s.cfg.CheckinWindowStart,
			"windowEnd":       s.cfg.CheckinWindowEnd,
		},
	})
}

// handleDashModeGet serves GET /api/mode.
func (s *Server) handleDashModeGet(w http.ResponseWriter, r *http.Request) {
	if !s.dashAuth(w, r) {
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"mode": s.Mode()})
}

// handleDashModeSet serves POST /api/mode with {"mode": "..."}. Invalid modes
// get the fixed 400 error shape.
func (s *Server) handleDashModeSet(w http.ResponseWriter, r *http.Request) {
	if !s.dashAuth(w, r) {
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		rejectLocally(w, r, http.StatusBadRequest, "bad_request", "Failed to read request body")
		return
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			rejectLocally(w, r, http.StatusBadRequest, "bad_request", "Invalid JSON body")
			return
		}
	}
	if s.SetMode(body.Mode) != nil {
		respondJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "invalid mode", "type": "proxy_error"},
		})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"mode": s.Mode()})
}

// handleDashLogs serves GET /api/logs with the most recent proxy log entries,
// newest first. level filters to "error" or "info" ("all" or empty means no
// filter); limit clamps to 1..500 with a default of 200.
func (s *Server) handleDashLogs(w http.ResponseWriter, r *http.Request) {
	if !s.dashAuth(w, r) {
		return
	}
	level := r.URL.Query().Get("level")
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	entries := s.logs.List()
	out := make([]logstore.Entry, 0, len(entries))
	for _, e := range entries {
		if (level == "error" || level == "info") && string(e.Level) != level {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"entries": out})
}
