// Command proxy is the AgentRouter spoof proxy. A fast, cross-platform
// reverse proxy that spoofs Claude Code headers, keeps WAF cookies and
// streams SSE responses from the upstream gateway.
//
// Thin entry point, Go equivalent of proxy.mjs. It validates config,
// starts the HTTP server and schedulers and handles graceful shutdown.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/trefeon/agentrouter-spoof-proxy/internal/config"
	"github.com/trefeon/agentrouter-spoof-proxy/internal/server"
)

func main() {
	// -healthcheck probes local /health and exits 0 or 1. Used as Docker
	// HEALTHCHECK, distroless has no shell or wget.
	healthcheck := flag.Bool("healthcheck", false, "probe /health and exit 0 on 200")
	flag.Parse()

	// Fail fast on bad env values before starting server or schedulers.
	// Mirrors proxy.mjs validateConfig.
	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	if *healthcheck {
		os.Exit(runHealthcheck(cfg))
	}

	srv := server.New(cfg)
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.ListenAddr, strconv.Itoa(cfg.ListenPort)))
	if err != nil {
		slog.Error("listen failed", "addr", net.JoinHostPort(cfg.ListenAddr, strconv.Itoa(cfg.ListenPort)), "error", err)
		os.Exit(1)
	}
	slog.Info(fmt.Sprintf("AgentRouter proxy listening on %s:%d, target=%s",
		cfg.ListenAddr, cfg.ListenPort, cfg.Upstream()))

	// Graceful shutdown on SIGINT or SIGTERM. Mirrors proxy.mjs shutdown,
	// drains active streams and forces exit after 15s.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		slog.Info("signal received, draining active streams...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("forced exit after shutdown timeout", "error", err)
			os.Exit(1)
		}
		slog.Info("server closed, exiting")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}
}

// runHealthcheck GETs local /health and returns 0 on 200, 1 otherwise.
// Used by Docker HEALTHCHECK.
func runHealthcheck(cfg *config.Config) int {
	url := fmt.Sprintf("http://%s/health", net.JoinHostPort(cfg.ListenAddr, strconv.Itoa(cfg.ListenPort)))
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		slog.Error("healthcheck failed", "url", url, "error", err)
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		slog.Error("healthcheck: unexpected status", "status", resp.StatusCode)
		return 1
	}
	return 0
}
