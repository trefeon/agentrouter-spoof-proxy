// Command proxy is the AgentRouter spoof proxy — a fast, cross-platform
// reverse proxy that spoofs Claude Code headers, maintains WAF cookies, and
// streams SSE responses from the upstream LLM gateway.
//
// This is the thin entry point (the Go equivalent of proxy.mjs): config
// validation, HTTP server, scheduler loops and graceful shutdown.
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
	// -healthcheck: probe the local /health endpoint and exit 0/1. Used as the
	// Docker HEALTHCHECK (distroless images have no shell/wget).
	healthcheck := flag.Bool("healthcheck", false, "probe /health and exit 0 on 200")
	flag.Parse()

	// Fail fast on invalid environment values before the server or any
	// scheduler starts (mirrors proxy.mjs validateConfig fail-fast).
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

	// Graceful shutdown on SIGINT/SIGTERM (proxy.mjs shutdown(): drain active
	// streams, force-exit after 15s).
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

// runHealthcheck GETs the local /health endpoint and returns an exit code:
// 0 when the proxy answers 200, 1 otherwise. Used by the Docker HEALTHCHECK.
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
