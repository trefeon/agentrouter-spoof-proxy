// Package server wires the proxy. It owns the HTTP server, route dispatch,
// schedulers and graceful shutdown. Go equivalent of proxy.mjs.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trefeon/agentrouter-spoof-proxy/internal/auth"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/config"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/models"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/proxy"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/resilience"
)

// Server owns the HTTP server and scheduler lifecycle. Shutdown cancels the
// internal context, which stops warmup, discovery and probe goroutines, drains
// connections and waits for the WaitGroup.
type Server struct {
	HTTP   *http.Server
	cancel context.CancelFunc
	wg     sync.WaitGroup
	log    *slog.Logger

	handler   *proxy.Handler
	cfg       *config.Config
	wafStore  *auth.Store
	breaker   *resilience.Breaker
	discovery *models.Discovery
	health    *models.Health
	recorder  *models.Recorder
	active    *atomic.Int64
}

// New builds the dependency graph, starts scheduler goroutines on an
// internal context, and returns the HTTP server. Call Serve to listen and
// Shutdown to stop. Order mirrors proxy.mjs.
func New(cfg *config.Config) *Server {
	level := slog.LevelInfo
	if cfg.Debug() {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	wafStore := auth.NewStore()
	wafStore.SetTarget(cfg.TargetProto, cfg.TargetHost, cfg.TargetPort)
	breaker := resilience.NewBreaker()
	discovery := models.NewDiscovery(cfg.StaticModelIDs())
	health := models.NewHealth()
	recorder := models.NewRecorder(cfg.SlowResponseMs)
	client := &http.Client{Transport: cfg.Transport()}
	active := &atomic.Int64{}
	handler := proxy.NewHandler(cfg, wafStore, breaker, discovery, health, recorder, client, logger)

	s := &Server{
		cfg:       cfg,
		log:       logger,
		handler:   handler,
		wafStore:  wafStore,
		breaker:   breaker,
		discovery: discovery,
		health:    health,
		recorder:  recorder,
		active:    active,
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	// Schedulers, goroutines tied to ctx and tracked by wg.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runWarmup(ctx)
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runDiscovery(ctx, client)
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		health.ProbeLoop(ctx, client, cfg.TargetHost, cfg.TargetPort, auth.SpoofHeaders, wafStore.Get)
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.resolveDNS(ctx)
	}()

	s.HTTP = &http.Server{
		Addr:              net.JoinHostPort(cfg.ListenAddr, strconv.Itoa(cfg.ListenPort)),
		Handler:           s.mux(),
		ReadHeaderTimeout: 30 * time.Second, // mirrors server.headersTimeout
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is not set, it would kill SSE streaming
		// (see docs/go-migration.md 3.10).
	}
	return s
}

// Serve runs HTTP on the listener until Shutdown.
func (s *Server) Serve(ln net.Listener) error {
	return s.HTTP.Serve(ln)
}

// Shutdown stops schedulers, drains connections and waits for goroutines.
// Context bounds the drain, mirrors proxy.mjs 15s forced exit.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cancel()
	err := s.HTTP.Shutdown(ctx)
	s.wg.Wait()
	return err
}

// Schedulers

// runWarmup runs warmup once, then every WarmupInterval. Mirrors
// scheduleWarmup.
func (s *Server) runWarmup(ctx context.Context) {
	s.wafStore.Warmup(ctx)
	t := time.NewTicker(s.cfg.WarmupInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.wafStore.Warmup(ctx)
		}
	}
}

// runDiscovery runs discovery only when AR_API_KEY is set. Fetches once,
// then every DiscoveryInterval. Mirrors scheduleDiscovery.
func (s *Server) runDiscovery(ctx context.Context, client *http.Client) {
	if s.cfg.ARAPIKey == "" {
		s.log.Info("Model discovery disabled (no AR_API_KEY set), using static list")
		return
	}
	s.discovery.Fetch(ctx, client, s.cfg.TargetHost, s.cfg.TargetPort, s.cfg.ARAPIKey)
	t := time.NewTicker(s.cfg.DiscoveryInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.discovery.Fetch(ctx, client, s.cfg.TargetHost, s.cfg.TargetPort, s.cfg.ARAPIKey)
		}
	}
}

// resolveDNS does a one-shot DNS lookup at startup. Informational only,
// not fatal. Mirrors resolveDns.
func (s *Server) resolveDNS(ctx context.Context) {
	addrs, err := net.DefaultResolver.LookupHost(ctx, s.cfg.TargetHost)
	if err != nil {
		s.log.Info(fmt.Sprintf("DNS resolution failed for %s", s.cfg.TargetHost))
		return
	}
	s.log.Info(fmt.Sprintf("DNS resolved %s → %s", s.cfg.TargetHost, strings.Join(addrs, ", ")))
}

// Routing

// proxyRoutes is the allowlist of paths that proxy upstream (utils.mjs
// PROXY_ROUTES).
var proxyRoutes = map[string]bool{
	"/v1/messages":         true,
	"/messages":            true,
	"/v1/chat/completions": true,
}

func isProxyRoute(rawPath string) bool {
	base := strings.SplitN(rawPath, "?", 2)[0]
	return proxyRoutes[base]
}

// mux builds the route table. Exact paths only, no trailing slash patterns,
// so Go 1.26 301 to 307 redirect never applies. Order matches proxy.mjs:
// health, models, allowlist 404, auth 401, method 405, then proxy.
func (s *Server) mux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /models", s.handleModels)

	// Proxy routes, auth-gated POST handlers.
	proxyH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requireProxyAuth(r, s.cfg.ProxyAuthToken) {
			rejectLocally(w, r, http.StatusUnauthorized, "unauthorized", "Invalid or missing proxy auth token")
			return
		}
		s.handler.ServeProxy(w, r)
	})
	mux.HandleFunc("POST /v1/messages", proxyH)
	mux.HandleFunc("POST /v1/chat/completions", proxyH)
	mux.HandleFunc("POST /messages", proxyH)

	// Catch-all for unknown paths, wrong methods and non-GET on health or models.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rawPath := r.URL.RequestURI()
		if !isProxyRoute(rawPath) {
			rejectLocally(w, r, http.StatusNotFound, "not_found", fmt.Sprintf("Route %s not found", rawPath))
			return
		}
		if !requireProxyAuth(r, s.cfg.ProxyAuthToken) {
			rejectLocally(w, r, http.StatusUnauthorized, "unauthorized", "Invalid or missing proxy auth token")
			return
		}
		if r.Method != http.MethodPost {
			rejectLocally(w, r, http.StatusMethodNotAllowed, "method_not_allowed", fmt.Sprintf("Method %s not allowed on %s", r.Method, rawPath))
			return
		}
		s.handler.ServeProxy(w, r)
	})

	return mux
}

// requireProxyAuth checks proxy auth. When PROXY_AUTH_TOKEN is set, the
// client must send it as Authorization Bearer or X-Proxy-Token. Mirrors
// proxy.mjs requireProxyAuth.
func requireProxyAuth(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	var bearer string
	if a := r.Header.Get("Authorization"); a != "" {
		// /^Bearer\s+(.+)$/i, Bearer (any case) followed by whitespace.
		const prefix = "bearer"
		if len(a) > len(prefix) && strings.EqualFold(a[:len(prefix)], prefix) && isSpace(a[len(prefix)]) {
			bearer = strings.TrimSpace(a[len(prefix)+1:])
		}
	}
	candidates := []string{r.Header.Get("X-Proxy-Token"), bearer}
	for _, c := range candidates {
		if proxy.ConstantTimeEqual(c, token) {
			return true
		}
	}
	return false
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\v' || b == '\f'
}

// rejectLocally sends a local error JSON and drains the request body so
// the keep-alive socket stays clean. Mirrors proxy.mjs rejectLocally.
func rejectLocally(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	respondJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message, "type": "proxy_error"},
	})
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
	}
}

// respondJSON writes JSON, mirrors utils.mjs respondJson.
func respondJSON(w http.ResponseWriter, status int, data any) {
	b, _ := json.Marshal(data)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// handleHealth serves GET /health and /api/health (proxy.mjs 80-94).
// modelHealth uses ModelStats JSON names.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
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
		"modelHealth":      s.recorder.Snapshot(),
	})
}

// handleModels serves GET /v1/models and /models (proxy.mjs 97-100).
// It filters unhealthy models so 9Router falls back quickly.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]any{
		"data":   s.health.HealthyModels(s.discovery.List()),
		"object": "list",
	})
}
