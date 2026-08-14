// Package config loads and validates the proxy's environment configuration.
//
// Env var names and defaults are identical to the Node.js version (src/config.mjs)
// so .env, docker-compose.yml and the install scripts keep working unchanged.
package config

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config mirrors src/config.mjs 1:1. Every field maps to one env var.
// *_Ms fields keep the raw integer millisecond values so .env files written
// for the Node version parse unchanged; use the Duration() helpers or
// RequestTimeout()/etc. accessors where a time.Duration is needed.
type Config struct {
	ListenPort  int    `env:"LISTEN_PORT" envDefault:"8318"`
	ListenAddr  string `env:"LISTEN_ADDRESS" envDefault:"127.0.0.1"`
	TargetProto string `env:"TARGET_PROTOCOL" envDefault:"https"`
	TargetHost  string `env:"TARGET_HOST" envDefault:"agentrouter.org"`
	TargetPort  int    `env:"TARGET_PORT" envDefault:"443"`

	RequestTimeoutMs    int `env:"REQUEST_TIMEOUT_MS" envDefault:"300000"`
	ResponseTimeoutMs   int `env:"RESPONSE_TIMEOUT_MS" envDefault:"30000"`
	SSEIdleTimeoutMs    int `env:"SSE_IDLE_TIMEOUT_MS" envDefault:"600000"`
	SSEChunkTimeoutMs   int `env:"SSE_CHUNK_TIMEOUT_MS" envDefault:"30000"`
	BodyUploadTimeoutMs int `env:"BODY_UPLOAD_TIMEOUT_MS" envDefault:"60000"`
	SlowResponseMs      int `env:"SLOW_RESPONSE_MS" envDefault:"30000"`
	WarmupIntervalMs    int `env:"WARMUP_INTERVAL_MS" envDefault:"180000"`
	DiscoveryIntervalMs int `env:"DISCOVERY_INTERVAL_MS" envDefault:"600000"`
	MaxRetries          int `env:"MAX_RETRIES" envDefault:"2"`
	RetryDelayMs        int `env:"RETRY_DELAY_MS" envDefault:"1000"`

	RetryOn5xx        bool `env:"RETRY_ON_5XX" envDefault:"false"`
	StripThinkingTags bool `env:"STRIP_THINKING_TAGS" envDefault:"true"`

	ModelsCSV          string `env:"MODELS_CSV" envDefault:"gpt-5.6-sol,claude-opus-5,claude-opus-4-8"`
	ARAPIKey           string `env:"AR_API_KEY"`
	InjectSystemPrompt string `env:"INJECT_SYSTEM_PROMPT"`
	ProxyAuthToken     string `env:"PROXY_AUTH_TOKEN"`
	LogLevel           string `env:"LOG_LEVEL" envDefault:"info"`
}

// Load parses the environment into a Config. It does NOT validate — call
// Validate() explicitly (cmd/proxy does both before the server starts).
func Load() (*Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return nil, fmt.Errorf("parse environment config: %w", err)
	}
	return &c, nil
}

// Validate reproduces every check from src/config.mjs validateConfig() with
// the same messages: fail fast before the server or any scheduler starts.
func (c *Config) Validate() error {
	checks := []struct {
		name, expected string
		value          any
		ok             func() bool
	}{
		{"LISTEN_PORT", "integer 1-65535", c.ListenPort, func() bool { return c.ListenPort >= 1 && c.ListenPort <= 65535 }},
		{"TARGET_PORT", "integer 1-65535", c.TargetPort, func() bool { return c.TargetPort >= 1 && c.TargetPort <= 65535 }},
		{"REQUEST_TIMEOUT_MS", "positive integer (ms)", c.RequestTimeoutMs, func() bool { return c.RequestTimeoutMs > 0 }},
		{"RESPONSE_TIMEOUT_MS", "positive integer (ms)", c.ResponseTimeoutMs, func() bool { return c.ResponseTimeoutMs > 0 }},
		{"SSE_IDLE_TIMEOUT_MS", "positive integer (ms)", c.SSEIdleTimeoutMs, func() bool { return c.SSEIdleTimeoutMs > 0 }},
		{"SSE_CHUNK_TIMEOUT_MS", "positive integer (ms)", c.SSEChunkTimeoutMs, func() bool { return c.SSEChunkTimeoutMs > 0 }},
		{"BODY_UPLOAD_TIMEOUT_MS", "positive integer (ms)", c.BodyUploadTimeoutMs, func() bool { return c.BodyUploadTimeoutMs > 0 }},
		{"SLOW_RESPONSE_MS", "positive integer (ms)", c.SlowResponseMs, func() bool { return c.SlowResponseMs > 0 }},
		{"WARMUP_INTERVAL_MS", "positive integer (ms)", c.WarmupIntervalMs, func() bool { return c.WarmupIntervalMs > 0 }},
		{"DISCOVERY_INTERVAL_MS", "positive integer (ms)", c.DiscoveryIntervalMs, func() bool { return c.DiscoveryIntervalMs > 0 }},
		{"MAX_RETRIES", "integer >= 0", c.MaxRetries, func() bool { return c.MaxRetries >= 0 }},
		{"RETRY_DELAY_MS", "integer >= 0 (ms)", c.RetryDelayMs, func() bool { return c.RetryDelayMs >= 0 }},
		{"TARGET_PROTOCOL", `"http" or "https"`, c.TargetProto, func() bool { return c.TargetProto == "http" || c.TargetProto == "https" }},
		{"LISTEN_ADDRESS", "non-empty IP/hostname", c.ListenAddr, func() bool { return c.ListenAddr != "" }},
	}
	for _, ch := range checks {
		if !ch.ok() {
			return fmt.Errorf("Invalid configuration: %s=%q — expected %s. Check your .env / environment variables and restart.", ch.name, fmt.Sprint(ch.value), ch.expected)
		}
	}
	return nil
}

// Upstream string used by the /health payload: "host:port".
func (c *Config) Upstream() string {
	return fmt.Sprintf("%s:%d", c.TargetHost, c.TargetPort)
}

// StaticModelIDs splits MODELS_CSV into trimmed ids (used when AR_API_KEY is
// unset or dynamic discovery fails). Mirrors discovery.mjs STATIC_MODELS.
func (c *Config) StaticModelIDs() []string {
	var ids []string
	for _, id := range strings.Split(c.ModelsCSV, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// Debug reports whether LOG_LEVEL == "debug" (mirrors src/logger.mjs IS_DEBUG).
func (c *Config) Debug() bool { return c.LogLevel == "debug" }

// Duration accessors for the millisecond fields.

func (c *Config) RequestTimeout() time.Duration  { return ms(c.RequestTimeoutMs) }
func (c *Config) ResponseTimeout() time.Duration { return ms(c.ResponseTimeoutMs) }
func (c *Config) SSEIdleTimeout() time.Duration  { return ms(c.SSEIdleTimeoutMs) }
func (c *Config) SSEChunkTimeout() time.Duration { return ms(c.SSEChunkTimeoutMs) }
func (c *Config) BodyUploadTimeout() time.Duration {
	return ms(c.BodyUploadTimeoutMs)
}
func (c *Config) SlowResponse() time.Duration   { return ms(c.SlowResponseMs) }
func (c *Config) WarmupInterval() time.Duration { return ms(c.WarmupIntervalMs) }
func (c *Config) DiscoveryInterval() time.Duration {
	return ms(c.DiscoveryIntervalMs)
}
func (c *Config) RetryDelay() time.Duration { return ms(c.RetryDelayMs) }

func ms(v int) time.Duration { return time.Duration(v) * time.Millisecond }

// Transport mirrors the Node http.Agent pool (src/config.mjs AGENT):
// keepAlive, maxSockets 64, maxFreeSockets 16. HTTP/1.1 only (like Node's
// http.Agent) — no HTTP/2, which avoids SSE buffering/streaming surprises.
func (c *Config) Transport() *http.Transport {
	return &http.Transport{
		MaxConnsPerHost:     64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   false, // HTTP/1.1, mirroring Node http.Agent
	}
}
