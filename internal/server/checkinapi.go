package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/trefeon/agentrouter-spoof-proxy/internal/checkin"
)

// handleCheckinStatus serves GET /api/checkin/status with the check-in
// manager snapshot. Auth-gated like the proxy routes.
func (s *Server) handleCheckinStatus(w http.ResponseWriter, r *http.Request) {
	if !requireProxyAuth(r, s.cfg.ProxyAuthToken) {
		rejectLocally(w, r, http.StatusUnauthorized, "unauthorized", "Invalid or missing proxy auth token")
		return
	}
	respondJSON(w, http.StatusOK, s.checkin.Status())
}

// handleCheckinRun serves POST /api/checkin/run. It starts a run asynchronously
// and decouples it from the request lifetime so a client disconnect does not
// kill the check-in.
func (s *Server) handleCheckinRun(w http.ResponseWriter, r *http.Request) {
	if !requireProxyAuth(r, s.cfg.ProxyAuthToken) {
		rejectLocally(w, r, http.StatusUnauthorized, "unauthorized", "Invalid or missing proxy auth token")
		return
	}
	err := s.checkin.RunNow(context.WithoutCancel(r.Context()))
	switch {
	case errors.Is(err, checkin.ErrNotConfigured):
		respondJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "check-in not configured", "type": "proxy_error"},
		})
	case errors.Is(err, checkin.ErrAlreadyRunning):
		respondJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{"message": "check-in already running", "type": "proxy_error"},
		})
	default:
		respondJSON(w, http.StatusOK, map[string]any{"started": true})
	}
}
