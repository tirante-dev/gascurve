package config

import (
	"math"
	"net"
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
  - name: dedicated
    chain_id: 7
    rpc_url: https://node.example
    ws_url: wss://node.example/ws
    calls_per_second: 0
    archive: true
    tick_interval: 250ms
    enabled: true
    fallbacks:
      - rpc_url: https://spare.example
        calls_per_second: 2
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
	if cfg.Collector.BackfillAnchorInterval != 1000 || cfg.Collector.MaxCatchUpBatches != 10 || cfg.Collector.FailoverCooldown != time.Minute {
		t.Fatalf("collector defaults not applied: %+v", cfg.Collector)
	}
	if cfg.Collector.EthUsdSource != "coinbase" || cfg.Collector.EthUsdMaxAge != DefaultEthUsdMaxAge {
		t.Fatalf("eth/usd defaults not applied: %+v", cfg.Collector)
	}
	if cfg.Server.WSMaxPerIP != 8 || cfg.Server.WSMaxTotal != 2000 || len(cfg.Server.TrustedProxies) != 0 {
		t.Fatalf("server defaults not applied: %+v", cfg.Server)
	}
	if len(cfg.EnabledNetworks()) != 2 || cfg.EnabledNetworks()[0].Name != "robinhood" {
		t.Fatalf("enabled networks: %+v", cfg.EnabledNetworks())
	}
	rh := cfg.Networks[0]
	if rh.WSURL != "" || rh.Archive || rh.Unlimited() || rh.TickInterval != 0 || rh.EffectiveTickInterval(cfg.Collector) != 2*time.Second {
		t.Fatalf("optional fields should default off: %+v", rh)
	}
	ded := cfg.Networks[2]
	if ded.WSURL != "wss://node.example/ws" || !ded.Archive || !ded.Unlimited() || ded.TickInterval != 250*time.Millisecond || ded.EffectiveTickInterval(cfg.Collector) != 250*time.Millisecond {
		t.Fatalf("dedicated network: %+v", ded)
	}
	if len(rh.Endpoints()) != 1 || rh.Endpoints()[0] != rh.Primary() || rh.Primary().RPCURL != "https://rpc.example" {
		t.Fatalf("endpoints without fallbacks: %+v", rh.Endpoints())
	}
	eps := ded.Endpoints()
	if len(eps) != 2 || eps[0] != (EndpointConfig{RPCURL: "https://node.example", WSURL: "wss://node.example/ws", Archive: true}) || eps[1] != (EndpointConfig{RPCURL: "https://spare.example", CallsPerSecond: 2}) {
		t.Fatalf("endpoints with a yaml fallback: %+v", eps)
	}
}

// TestFallbackEnv: the positional lists create fallbacks as needed, empty
// items leave a position's field alone and YAML values survive where the
// environment says nothing.
func TestFallbackEnv(t *testing.T) {
	p := writeYAML(t, sampleYAML)
	env := envOf(map[string]string{
		"NETWORK_ROBINHOOD_FALLBACK_RPC_URLS":         "https://quick.example/key, https://spare.example",
		"NETWORK_ROBINHOOD_FALLBACK_WS_URLS":          "wss://quick.example/key",
		"NETWORK_ROBINHOOD_FALLBACK_ARCHIVE":          "true,",
		"NETWORK_ROBINHOOD_FALLBACK_CALLS_PER_SECOND": ",8",
		// The dedicated network keeps its YAML fallback URL and only gains
		// a WS URL at position 0 and a second fallback.
		"NETWORK_DEDICATED_FALLBACK_RPC_URLS": ",https://third.example",
		"NETWORK_DEDICATED_FALLBACK_WS_URLS":  "wss://spare.example/ws,",
	})
	cfg, err := LoadWith(Options{Path: p, RequireRPC: true, Getenv: env})
	if err != nil {
		t.Fatal(err)
	}
	rh := cfg.Networks[0]
	want := []EndpointConfig{
		{RPCURL: "https://quick.example/key", WSURL: "wss://quick.example/key", Archive: true},
		{RPCURL: "https://spare.example", CallsPerSecond: 8},
	}
	if len(rh.Fallbacks) != 2 || rh.Fallbacks[0] != want[0] || rh.Fallbacks[1] != want[1] {
		t.Fatalf("robinhood fallbacks: %+v", rh.Fallbacks)
	}
	if eps := rh.Endpoints(); len(eps) != 3 || eps[0] != rh.Primary() || eps[2] != want[1] {
		t.Fatalf("robinhood endpoints: %+v", eps)
	}
	ded := cfg.Networks[2]
	if len(ded.Fallbacks) != 2 || ded.Fallbacks[0] != (EndpointConfig{RPCURL: "https://spare.example", WSURL: "wss://spare.example/ws", CallsPerSecond: 2}) || ded.Fallbacks[1] != (EndpointConfig{RPCURL: "https://third.example"}) {
		t.Fatalf("dedicated fallbacks: %+v", ded.Fallbacks)
	}
	// Unset and empty variables change nothing.
	cfg, err = LoadWith(Options{Path: p, RequireRPC: true, Getenv: envOf(map[string]string{"NETWORK_ROBINHOOD_FALLBACK_RPC_URLS": ""})})
	if err != nil || len(cfg.Networks[0].Fallbacks) != 0 {
		t.Fatalf("empty list: %+v %v", cfg.Networks[0].Fallbacks, err)
	}
	// Bad values and incomplete fallbacks are rejected.
	for _, m := range []map[string]string{
		{"NETWORK_ROBINHOOD_FALLBACK_ARCHIVE": "yes,please"},
		{"NETWORK_ROBINHOOD_FALLBACK_CALLS_PER_SECOND": "1,fast"},
		{"NETWORK_ROBINHOOD_FALLBACK_RPC_URLS": "https://a", "NETWORK_ROBINHOOD_FALLBACK_CALLS_PER_SECOND": "-1"},
		{"NETWORK_ROBINHOOD_FALLBACK_RPC_URLS": "https://a", "NETWORK_ROBINHOOD_FALLBACK_WS_URLS": "https://not-a-socket"},
		{"NETWORK_ROBINHOOD_FALLBACK_WS_URLS": "wss://a"}, // a fallback without rpc_url
	} {
		if _, err := LoadWith(Options{Path: p, RequireRPC: true, Getenv: envOf(m)}); err == nil {
			t.Fatalf("expected error for %v", m)
		}
	}
	// The API does not need fallback RPC URLs either.
	if _, err := LoadWith(Options{Path: p, RequireRPC: false, Getenv: envOf(map[string]string{"NETWORK_ROBINHOOD_FALLBACK_WS_URLS": "wss://a"})}); err != nil {
		t.Fatalf("api load should not require fallback rpc urls: %v", err)
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
		"NETWORK_ARBITRUM_ONE_WS_URL":           "wss://arb.example/ws",
		"NETWORK_ARBITRUM_ONE_ARCHIVE":          "true",
		"NETWORK_ARBITRUM_ONE_TICK_INTERVAL":    "500ms",
		"NETWORK_ARBITRUM_ONE_HISTORY_EPOCH":    "3",
		"NETWORK_DEDICATED_ARCHIVE":             "false",
		"NETWORK_DEDICATED_TICK_INTERVAL":       "1s",
	})
	cfg, err := LoadWith(Options{Path: p, RequireRPC: true, Getenv: env})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.URL != "postgres://override" || cfg.Server.Port != 9090 || cfg.LogLevel != "debug" || !cfg.Server.DevMode {
		t.Fatalf("env overrides not applied: %+v", cfg)
	}
	arb := cfg.Networks[1]
	if arb.RPCURL != "https://arb.example" || !arb.Enabled || arb.CallsPerSecond != 7.5 || arb.WSURL != "wss://arb.example/ws" || !arb.Archive || arb.TickInterval != 500*time.Millisecond {
		t.Fatalf("network overrides not applied: %+v", arb)
	}
	// The history epoch is an operator's request to rebuild the
	// reconstructed history once, so it has to be settable per network
	// without editing the mounted config.
	if arb.HistoryEpoch != 3 || cfg.Networks[0].HistoryEpoch != 0 {
		t.Fatalf("history epoch overrides: %d %d", arb.HistoryEpoch, cfg.Networks[0].HistoryEpoch)
	}
	if !arb.HasArchive() || cfg.Networks[2].HasArchive() {
		t.Fatalf("HasArchive follows the configured endpoints: %+v %+v", arb, cfg.Networks[2])
	}
	if cfg.Networks[2].Archive || cfg.Networks[2].TickInterval != time.Second {
		t.Fatalf("NETWORK_DEDICATED_ARCHIVE=false and NETWORK_DEDICATED_TICK_INTERVAL=1s not applied: %+v", cfg.Networks[2])
	}
	if arb.EnvKey() != "NETWORK_ARBITRUM_ONE" {
		t.Fatalf("EnvKey = %q", arb.EnvKey())
	}
}

// TestEthUsdEnv: the source and the max age come from the environment, and
// an empty ETH_USD_SOURCE disables the fetch rather than being ignored.
func TestEthUsdEnv(t *testing.T) {
	p := writeYAML(t, sampleYAML)
	cfg, err := LoadWith(Options{Path: p, Getenv: envOf(map[string]string{
		"ETH_USD_SOURCE":  " coingecko ",
		"ETH_USD_MAX_AGE": "90s",
	})})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collector.EthUsdSource != "coingecko" || cfg.Collector.EthUsdMaxAge != 90*time.Second {
		t.Fatalf("eth/usd env not applied: %+v", cfg.Collector)
	}
	cfg, err = LoadWith(Options{Path: p, Getenv: envOf(map[string]string{"ETH_USD_SOURCE": ""})})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collector.EthUsdSource != "" {
		t.Fatalf("an empty ETH_USD_SOURCE must disable the fetch, got %q", cfg.Collector.EthUsdSource)
	}
	for _, m := range []map[string]string{
		{"ETH_USD_MAX_AGE": "soon"},
		{"ETH_USD_MAX_AGE": "-1m"},
		{"ETH_USD_SOURCE": "kraken"},
		{"ETH_USD_SOURCE": "http://insecure.example"},
	} {
		if _, err := LoadWith(Options{Path: p, Getenv: envOf(m)}); err == nil {
			t.Fatalf("expected error for %v", m)
		}
	}
}

func TestEnvErrors(t *testing.T) {
	p := writeYAML(t, sampleYAML)
	for _, m := range []map[string]string{
		{"PORT": "abc"},
		{"DEV_MODE": "maybe"},
		{"NETWORK_ROBINHOOD_ENABLED": "nah"},
		{"NETWORK_ROBINHOOD_CALLS_PER_SECOND": "fast"},
		{"NETWORK_ROBINHOOD_ARCHIVE": "sometimes"},
		{"NETWORK_ROBINHOOD_WS_URL": "https://not-a-socket"},
		{"NETWORK_ROBINHOOD_CALLS_PER_SECOND": "-1"},
		{"NETWORK_ROBINHOOD_CALLS_PER_SECOND": "NaN"},
		{"NETWORK_ROBINHOOD_CALLS_PER_SECOND": "+Inf"},
		{"NETWORK_ROBINHOOD_CALLS_PER_SECOND": "Infinity"},
		{"NETWORK_ROBINHOOD_CALLS_PER_SECOND": "10001"},
		{"METRICS_PORT": "not-a-port"},
		{"METRICS_PORT": "70000"},
		{"NETWORK_ROBINHOOD_TICK_INTERVAL": "soon"},
		{"NETWORK_ROBINHOOD_TICK_INTERVAL": "-1s"},
		{"NETWORK_ROBINHOOD_HISTORY_EPOCH": "once"},
		{"NETWORK_ROBINHOOD_HISTORY_EPOCH": "-1"},
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
			Server:   ServerConfig{Port: 8080, RateLimitPerSecond: 1, RateLimitBurst: 1, WSMaxPerIP: 8, WSMaxTotal: 100, TrustedProxies: []string{"10.0.0.0/8", "::1", " "}},
			Database: DatabaseConfig{URL: "postgres://x"},
			Collector: CollectorConfig{
				TickInterval: time.Second, SlowInterval: time.Second, HeaderBatchSize: 10,
				BlockRetention: time.Hour, SampleRetention: time.Hour, BackfillDepth: time.Hour,
				BackfillAnchorInterval: 1000, MaxCatchUpBatches: 10, FailoverCooldown: time.Minute,
				EthUsdSource: "coinbase", EthUsdMaxAge: DefaultEthUsdMaxAge,
			},
			Networks: []NetworkConfig{{Name: "a", ChainID: 1, CallsPerSecond: 1, Enabled: true, RPCURL: "http://x", Fallbacks: []EndpointConfig{{RPCURL: "http://y", WSURL: "wss://y", Archive: true, CallsPerSecond: 2}}}},
		}
	}
	if err := ptr(base()).Validate(true); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	unlimited := base()
	unlimited.Networks[0].CallsPerSecond = 0
	unlimited.Networks[0].WSURL = "ws://x/ws"
	unlimited.Networks[0].Archive = true
	if err := unlimited.Validate(true); err != nil {
		t.Fatalf("unlimited archive network rejected: %v", err)
	}
	cases := map[string]func(*Config){
		"port":                  func(c *Config) { c.Server.Port = 0 },
		"port high":             func(c *Config) { c.Server.Port = 70000 },
		"rate limit":            func(c *Config) { c.Server.RateLimitBurst = 0 },
		"db url":                func(c *Config) { c.Database.URL = "" },
		"tick":                  func(c *Config) { c.Collector.TickInterval = 0 },
		"batch size":            func(c *Config) { c.Collector.HeaderBatchSize = 101 },
		"metrics port negative": func(c *Config) { c.Collector.MetricsPort = -1 },
		"metrics port high":     func(c *Config) { c.Collector.MetricsPort = 70000 },
		"empty name":            func(c *Config) { c.Networks[0].Name = "" },
		"dup name":              func(c *Config) { c.Networks = append(c.Networks, c.Networks[0]) },
		"zero chain":            func(c *Config) { c.Networks[0].ChainID = 0 },
		"dup chain":             func(c *Config) { n := c.Networks[0]; n.Name = "b"; c.Networks = append(c.Networks, n) },
		"calls":                 func(c *Config) { c.Networks[0].CallsPerSecond = -4 },
		"calls nan":             func(c *Config) { c.Networks[0].CallsPerSecond = math.NaN() },
		"calls inf":             func(c *Config) { c.Networks[0].CallsPerSecond = math.Inf(1) },
		"calls huge":            func(c *Config) { c.Networks[0].CallsPerSecond = MaxCallsPerSecond + 1 },
		// Below one call every ten seconds a token takes longer than any
		// request timeout: that is a mistake, not a budget (use 0).
		"calls tiny":          func(c *Config) { c.Networks[0].CallsPerSecond = MinCallsPerSecond / 2 },
		"fallback calls tiny": func(c *Config) { c.Networks[0].Fallbacks[0].CallsPerSecond = 0.01 },
		"ws per ip":           func(c *Config) { c.Server.WSMaxPerIP = 0 },
		"ws total":            func(c *Config) { c.Server.WSMaxTotal = 0 },
		"proxy cidr":          func(c *Config) { c.Server.TrustedProxies = []string{"10.0.0.0/33"} },
		"proxy ip":            func(c *Config) { c.Server.TrustedProxies = []string{"not-an-ip"} },
		"ws url scheme":       func(c *Config) { c.Networks[0].WSURL = "http://x" },
		// An RPC URL with the WebSocket scheme, or one with no scheme at
		// all, fails every call: catch it before the first one.
		"rpc url scheme":      func(c *Config) { c.Networks[0].RPCURL = "wss://x" },
		"rpc url bare":        func(c *Config) { c.Networks[0].RPCURL = "node.example" },
		"rpc url file":        func(c *Config) { c.Networks[0].RPCURL = "file:///etc/passwd" },
		"rpc url no host":     func(c *Config) { c.Networks[0].RPCURL = "http://" },
		"ws url no host":      func(c *Config) { c.Networks[0].WSURL = "wss://" },
		"fallback rpc ws":     func(c *Config) { c.Networks[0].Fallbacks[0].RPCURL = "ws://y" },
		"rpc url unparsed":    func(c *Config) { c.Networks[0].RPCURL = "http://a b.example" },
		"anchor interval":     func(c *Config) { c.Collector.BackfillAnchorInterval = 0 },
		"catch up":            func(c *Config) { c.Collector.MaxCatchUpBatches = 0 },
		"missing rpc url":     func(c *Config) { c.Networks[0].RPCURL = "" },
		"cooldown":            func(c *Config) { c.Collector.FailoverCooldown = 0 },
		"fallback calls":      func(c *Config) { c.Networks[0].Fallbacks[0].CallsPerSecond = math.Inf(1) },
		"fallback huge":       func(c *Config) { c.Networks[0].Fallbacks[0].CallsPerSecond = MaxCallsPerSecond + 1 },
		"fallback ws":         func(c *Config) { c.Networks[0].Fallbacks[0].WSURL = "http://y" },
		"fallback rpc":        func(c *Config) { c.Networks[0].Fallbacks[0].RPCURL = "" },
		"network tick":        func(c *Config) { c.Networks[0].TickInterval = -time.Second },
		"eth usd max age":     func(c *Config) { c.Collector.EthUsdMaxAge = 0 },
		"eth usd source":      func(c *Config) { c.Collector.EthUsdSource = "http://insecure.example" },
		"eth usd source name": func(c *Config) { c.Collector.EthUsdSource = "kraken" },
	}
	for name, mutate := range cases {
		c := base()
		mutate(&c)
		if err := c.Validate(true); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	// A rejected URL is never quoted back: it can carry the API key.
	keyed := base()
	keyed.Networks[0].RPCURL = "ftp://node.example/s3cret-key"
	err := keyed.Validate(true)
	if err == nil {
		t.Fatal("an ftp rpc_url must be rejected")
	}
	if strings.Contains(err.Error(), "s3cret-key") {
		t.Fatalf("a rejection must not quote the URL: %v", err)
	}
	// The minimum itself is accepted.
	c := base()
	c.Networks[0].CallsPerSecond = MinCallsPerSecond
	if err := c.Validate(true); err != nil {
		t.Errorf("the minimum budget must be accepted: %v", err)
	}
	// Every supported spot source, and the empty one that disables it.
	for _, source := range []string{"", "coinbase", "coingecko", "https://prices.example/eth"} {
		c := base()
		c.Collector.EthUsdSource = source
		if err := c.Validate(true); err != nil {
			t.Errorf("eth_usd_source %q rejected: %v", source, err)
		}
	}
}

func ptr(c Config) *Config { return &c }

func TestTrustedProxyNets(t *testing.T) {
	nets, err := (ServerConfig{TrustedProxies: []string{"10.0.0.0/8", "192.168.1.5", "fd00::1", ""}}).TrustedProxyNets()
	if err != nil || len(nets) != 3 {
		t.Fatalf("nets: %v %v", nets, err)
	}
	if !nets[0].Contains(net.ParseIP("10.1.2.3")) || nets[0].Contains(net.ParseIP("11.0.0.1")) {
		t.Fatal("cidr")
	}
	if !nets[1].Contains(net.ParseIP("192.168.1.5")) || nets[1].Contains(net.ParseIP("192.168.1.6")) {
		t.Fatal("single ipv4 is a /32")
	}
	if !nets[2].Contains(net.ParseIP("fd00::1")) || nets[2].Contains(net.ParseIP("fd00::2")) {
		t.Fatal("single ipv6 is a /128")
	}
}

// TestMetricsPort: the collector's metrics server has a default port, the
// environment overrides it, and zero switches it off.
func TestMetricsPort(t *testing.T) {
	p := writeYAML(t, sampleYAML)
	cfg, err := LoadWith(Options{Path: p, Getenv: envOf(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collector.MetricsPort != DefaultMetricsPort || !cfg.Collector.MetricsEnabled() {
		t.Fatalf("default metrics port = %d, enabled = %v", cfg.Collector.MetricsPort, cfg.Collector.MetricsEnabled())
	}
	cfg, err = LoadWith(Options{Path: p, Getenv: envOf(map[string]string{"METRICS_PORT": "9111"})})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collector.MetricsPort != 9111 {
		t.Fatalf("METRICS_PORT not applied: %d", cfg.Collector.MetricsPort)
	}
	cfg, err = LoadWith(Options{Path: p, Getenv: envOf(map[string]string{"METRICS_PORT": "0"})})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collector.MetricsEnabled() {
		t.Fatal("METRICS_PORT=0 must disable the metrics server")
	}
}
