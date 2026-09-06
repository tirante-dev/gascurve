package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sampleYAML = `
server:
  port: 8081
  cors_origins: [http://localhost:3000]
database:
  url: postgres://x
collector:
  tick_interval: 2s
  slow_interval: 30s
  header_batch_size: 50
  block_retention: 1h
  sample_retention: 2h
  backfill_depth: 3h
networks:
  - name: robinhood
    display_name: Robinhood Chain
    chain_id: 4663
    rpc_url: https://rpc.example
    explorer_url: https://explorer.example
    calls_per_second: 4
    enabled: true
  - name: arbitrum-one
    display_name: Arbitrum One
    chain_id: 42161
    calls_per_second: 2
    enabled: false
`

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadWith(t *testing.T) {
	p := writeYAML(t, sampleYAML)
	cfg, err := LoadWith(Options{Path: p, RequireRPC: true, Getenv: envOf(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 8081 || cfg.Collector.TickInterval != 2*time.Second || cfg.Collector.HeaderBatchSize != 50 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.LogLevel != "info" || cfg.Database.MaxOpen != 25 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if len(cfg.EnabledNetworks()) != 1 || cfg.EnabledNetworks()[0].Name != "robinhood" {
		t.Fatalf("enabled networks: %+v", cfg.EnabledNetworks())
	}
}

func TestEnvOverrides(t *testing.T) {
	p := writeYAML(t, sampleYAML)
	env := envOf(map[string]string{
		"DB_URL":                                "postgres://override",
		"PORT":                                  "9090",
		"LOG_LEVEL":                             "debug",
		"DEV_MODE":                              "true",
		"NETWORK_ARBITRUM_ONE_RPC_URL":          "https://arb.example",
		"NETWORK_ARBITRUM_ONE_ENABLED":          "true",
		"NETWORK_ARBITRUM_ONE_CALLS_PER_SECOND": "7.5",
	})
	cfg, err := LoadWith(Options{Path: p, RequireRPC: true, Getenv: env})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.URL != "postgres://override" || cfg.Server.Port != 9090 || cfg.LogLevel != "debug" || !cfg.Server.DevMode {
		t.Fatalf("env overrides not applied: %+v", cfg)
	}
	arb := cfg.Networks[1]
	if arb.RPCURL != "https://arb.example" || !arb.Enabled || arb.CallsPerSecond != 7.5 {
		t.Fatalf("network overrides not applied: %+v", arb)
	}
	if arb.EnvKey() != "NETWORK_ARBITRUM_ONE" {
		t.Fatalf("EnvKey = %q", arb.EnvKey())
	}
}

func TestEnvErrors(t *testing.T) {
	p := writeYAML(t, sampleYAML)
	for _, m := range []map[string]string{
		{"PORT": "abc"},
		{"DEV_MODE": "maybe"},
		{"NETWORK_ROBINHOOD_ENABLED": "nah"},
		{"NETWORK_ROBINHOOD_CALLS_PER_SECOND": "fast"},
	} {
		if _, err := LoadWith(Options{Path: p, Getenv: envOf(m)}); err == nil {
			t.Fatalf("expected error for %v", m)
		}
	}
}

func TestRequireRPC(t *testing.T) {
	p := writeYAML(t, sampleYAML)
	env := envOf(map[string]string{"NETWORK_ARBITRUM_ONE_ENABLED": "true"})
	if _, err := LoadWith(Options{Path: p, RequireRPC: true, Getenv: env}); err == nil || !strings.Contains(err.Error(), "rpc_url is required") {
		t.Fatalf("expected rpc_url error, got %v", err)
	}
	if _, err := LoadWith(Options{Path: p, RequireRPC: false, Getenv: env}); err != nil {
		t.Fatalf("api load should not require rpc: %v", err)
	}
}

func TestLoadAndLoadForAPI(t *testing.T) {
	p := writeYAML(t, sampleYAML)
	t.Setenv("CONFIG_PATH", p)
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadForAPI(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", "")
	if _, err := LoadWith(Options{Path: "", Getenv: envOf(nil)}); err == nil {
		t.Fatal("expected error for missing default config.yaml in temp cwd")
	}
}

func TestBadFiles(t *testing.T) {
	if _, err := LoadWith(Options{Path: filepath.Join(t.TempDir(), "missing.yaml"), Getenv: envOf(nil)}); err == nil {
		t.Fatal("expected read error")
	}
	p := writeYAML(t, "server:\n  port: notanumber\n")
	if _, err := LoadWith(Options{Path: p, Getenv: envOf(nil)}); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestValidate(t *testing.T) {
	base := func() Config {
		return Config{
			Server:   ServerConfig{Port: 8080, RateLimitPerSecond: 1, RateLimitBurst: 1},
			Database: DatabaseConfig{URL: "postgres://x"},
			Collector: CollectorConfig{
				TickInterval: time.Second, SlowInterval: time.Second, HeaderBatchSize: 10,
				BlockRetention: time.Hour, SampleRetention: time.Hour, BackfillDepth: time.Hour,
			},
			Networks: []NetworkConfig{{Name: "a", ChainID: 1, CallsPerSecond: 1, Enabled: true, RPCURL: "http://x"}},
		}
	}
	if err := ptr(base()).Validate(true); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]func(*Config){
		"port":            func(c *Config) { c.Server.Port = 0 },
		"port high":       func(c *Config) { c.Server.Port = 70000 },
		"rate limit":      func(c *Config) { c.Server.RateLimitBurst = 0 },
		"db url":          func(c *Config) { c.Database.URL = "" },
		"tick":            func(c *Config) { c.Collector.TickInterval = 0 },
		"batch size":      func(c *Config) { c.Collector.HeaderBatchSize = 101 },
		"empty name":      func(c *Config) { c.Networks[0].Name = "" },
		"dup name":        func(c *Config) { c.Networks = append(c.Networks, c.Networks[0]) },
		"zero chain":      func(c *Config) { c.Networks[0].ChainID = 0 },
		"dup chain":       func(c *Config) { n := c.Networks[0]; n.Name = "b"; c.Networks = append(c.Networks, n) },
		"calls":           func(c *Config) { c.Networks[0].CallsPerSecond = 0 },
		"missing rpc url": func(c *Config) { c.Networks[0].RPCURL = "" },
	}
	for name, mutate := range cases {
		c := base()
		mutate(&c)
		if err := c.Validate(true); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func ptr(c Config) *Config { return &c }
