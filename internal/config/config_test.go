package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Load parses with defaults when no env vars are set.
func TestLoadDefaults(t *testing.T) {
	clear := clearEnv(t)
	defer clear()

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ListenPort != 8318 {
		t.Errorf("ListenPort = %d, want 8318", c.ListenPort)
	}
	if c.ListenAddr != "127.0.0.1" {
		t.Errorf("ListenAddr = %q, want 127.0.0.1", c.ListenAddr)
	}
	if c.TargetHost != "agentrouter.org" {
		t.Errorf("TargetHost = %q", c.TargetHost)
	}
	if c.TargetPort != 443 {
		t.Errorf("TargetPort = %d, want 443", c.TargetPort)
	}
	if c.MaxRetries != 2 {
		t.Errorf("MaxRetries = %d, want 2", c.MaxRetries)
	}
	if c.RetryOn5xx {
		t.Error("RetryOn5xx default should be false")
	}
	if !c.StripThinkingTags {
		t.Error("StripThinkingTags default should be true")
	}
	if c.ARAPIKey != "" {
		t.Errorf("ARAPIKey = %q, want empty", c.ARAPIKey)
	}
	if c.RequestTimeout() != 300*time.Second {
		t.Errorf("RequestTimeout = %v, want 5m", c.RequestTimeout())
	}
	if c.SSEChunkTimeout() != 30*time.Second {
		t.Errorf("SSEChunkTimeout = %v, want 30s", c.SSEChunkTimeout())
	}
}

// Load honors explicitly set env vars (integer + bool + string).
func TestLoadExplicit(t *testing.T) {
	clear := clearEnv(t)
	defer clear()

	t.Setenv("LISTEN_PORT", "9000")
	t.Setenv("MAX_RETRIES", "5")
	t.Setenv("RETRY_ON_5XX", "true")
	t.Setenv("STRIP_THINKING_TAGS", "false")
	t.Setenv("AR_API_KEY", "sk-test-key")
	t.Setenv("PROXY_AUTH_TOKEN", "secret")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ListenPort != 9000 {
		t.Errorf("ListenPort = %d, want 9000", c.ListenPort)
	}
	if c.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d, want 5", c.MaxRetries)
	}
	if !c.RetryOn5xx {
		t.Error("RetryOn5xx should be true")
	}
	if c.StripThinkingTags {
		t.Error("StripThinkingTags should be false")
	}
	if c.ARAPIKey != "sk-test-key" || c.ProxyAuthToken != "secret" {
		t.Errorf("keys not parsed: api=%q token=%q", c.ARAPIKey, c.ProxyAuthToken)
	}
}

func TestValidate(t *testing.T) {
	clear := clearEnv(t)
	defer clear()

	valid := &Config{
		ListenPort: 8318, ListenAddr: "127.0.0.1", TargetProto: "https",
		TargetHost: "agentrouter.org", TargetPort: 443,
		RequestTimeoutMs: 1, ResponseTimeoutMs: 1, SSEIdleTimeoutMs: 1,
		SSEChunkTimeoutMs: 1, BodyUploadTimeoutMs: 1, SlowResponseMs: 1,
		WarmupIntervalMs: 1, DiscoveryIntervalMs: 1, MaxRetries: 0,
		RetryDelayMs: 0,
		ExposureMode: "auto",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := []struct {
		name  string
		mut   func(*Config)
		field string
	}{
		{"port zero", func(c *Config) { c.ListenPort = 0 }, "LISTEN_PORT"},
		{"port too big", func(c *Config) { c.ListenPort = 70000 }, "LISTEN_PORT"},
		{"target port zero", func(c *Config) { c.TargetPort = 0 }, "TARGET_PORT"},
		{"request timeout zero", func(c *Config) { c.RequestTimeoutMs = 0 }, "REQUEST_TIMEOUT_MS"},
		{"response timeout negative", func(c *Config) { c.ResponseTimeoutMs = -5 }, "RESPONSE_TIMEOUT_MS"},
		{"sse idle zero", func(c *Config) { c.SSEIdleTimeoutMs = 0 }, "SSE_IDLE_TIMEOUT_MS"},
		{"sse chunk zero", func(c *Config) { c.SSEChunkTimeoutMs = 0 }, "SSE_CHUNK_TIMEOUT_MS"},
		{"body upload zero", func(c *Config) { c.BodyUploadTimeoutMs = 0 }, "BODY_UPLOAD_TIMEOUT_MS"},
		{"slow response zero", func(c *Config) { c.SlowResponseMs = 0 }, "SLOW_RESPONSE_MS"},
		{"warmup zero", func(c *Config) { c.WarmupIntervalMs = 0 }, "WARMUP_INTERVAL_MS"},
		{"discovery zero", func(c *Config) { c.DiscoveryIntervalMs = 0 }, "DISCOVERY_INTERVAL_MS"},
		{"max retries negative", func(c *Config) { c.MaxRetries = -1 }, "MAX_RETRIES"},
		{"retry delay negative", func(c *Config) { c.RetryDelayMs = -1 }, "RETRY_DELAY_MS"},
		{"bad protocol", func(c *Config) { c.TargetProto = "ftp" }, "TARGET_PROTOCOL"},
		{"empty listen address", func(c *Config) { c.ListenAddr = "" }, "LISTEN_ADDRESS"},
		{"bad exposure mode", func(c *Config) { c.ExposureMode = "weird" }, "EXPOSURE_MODE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *valid // copy
			tc.mut(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not mention %s", err, tc.field)
			}
		})
	}
}

func TestStaticModelIDs(t *testing.T) {
	clear := clearEnv(t)
	defer clear()

	c := &Config{ModelsCSV: "claude-opus-4-8, claude-opus-5 , deepseek-v4-flash ,glm-5.3,gpt-5.6-sol"}
	ids := c.StaticModelIDs()
	want := []string{"claude-opus-4-8", "claude-opus-5", "deepseek-v4-flash", "glm-5.3", "gpt-5.6-sol"}
	if len(ids) != len(want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

func TestUpstream(t *testing.T) {
	c := &Config{TargetHost: "agentrouter.org", TargetPort: 443}
	if got := c.Upstream(); got != "agentrouter.org:443" {
		t.Errorf("Upstream() = %q", got)
	}
}

// clearEnv removes every env var the config reads, so tests are hermetic.
func clearEnv(t *testing.T) func() {
	t.Helper()
	vars := []string{
		"LISTEN_PORT", "LISTEN_ADDRESS", "TARGET_PROTOCOL", "TARGET_HOST", "TARGET_PORT",
		"REQUEST_TIMEOUT_MS", "RESPONSE_TIMEOUT_MS", "SSE_IDLE_TIMEOUT_MS", "SSE_CHUNK_TIMEOUT_MS",
		"BODY_UPLOAD_TIMEOUT_MS", "SLOW_RESPONSE_MS", "WARMUP_INTERVAL_MS", "DISCOVERY_INTERVAL_MS",
		"MAX_RETRIES", "RETRY_DELAY_MS", "RETRY_ON_5XX", "STRIP_THINKING_TAGS",
		"MODELS_CSV", "AR_API_KEY", "INJECT_SYSTEM_PROMPT", "PROXY_AUTH_TOKEN", "LOG_LEVEL",
		"EXPOSURE_MODE", "CHECKIN_CMD", "CHECKIN_ARGS", "CHECKIN_WORKDIR", "CHECKIN_SCHEDULE",
		"CHECKIN_RANDOM_WINDOW_START", "CHECKIN_RANDOM_WINDOW_END",
	}
	saved := map[string]string{}
	for _, v := range vars {
		if val, ok := os.LookupEnv(v); ok {
			saved[v] = val
			t.Setenv(v, "")
		}
	}
	return func() {
		for v, val := range saved {
			t.Setenv(v, val)
		}
	}
}
