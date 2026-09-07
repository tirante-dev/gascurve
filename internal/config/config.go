// Package config loads gascurve configuration from a YAML file (config.yaml
// or CONFIG_PATH) and applies environment overrides: DB_URL, PORT, LOG_LEVEL,
// DEV_MODE, ETH_USD_SOURCE, ETH_USD_MAX_AGE and per-network
// NETWORK_<NAME>_RPC_URL, NETWORK_<NAME>_WS_URL,
// NETWORK_<NAME>_ENABLED, NETWORK_<NAME>_CALLS_PER_SECOND,
// NETWORK_<NAME>_ARCHIVE and NETWORK_<NAME>_TICK_INTERVAL, where NAME is
// the network name upper-cased with dashes replaced by underscores.
// Fallback endpoints come from the positional, comma-separated
// NETWORK_<NAME>_FALLBACK_RPC_URLS,
// NETWORK_<NAME>_FALLBACK_WS_URLS, NETWORK_<NAME>_FALLBACK_ARCHIVE and
// NETWORK_<NAME>_FALLBACK_CALLS_PER_SECOND.
package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/tirante-dev/gascurve/internal/prices"
)

// Config is the full configuration tree.
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Database  DatabaseConfig  `mapstructure:"database"`
	Collector CollectorConfig `mapstructure:"collector"`
	Networks  []NetworkConfig `mapstructure:"networks"`
	LogLevel  string          `mapstructure:"log_level"`
}

// ServerConfig configures the HTTP API.
type ServerConfig struct {
	Port        int      `mapstructure:"port"`
	CORSOrigins []string `mapstructure:"cors_origins"`
	DevMode     bool     `mapstructure:"dev_mode"`
	// RateLimitPerSecond is the per-IP request budget; RateLimitBurst the bucket size.
	RateLimitPerSecond float64 `mapstructure:"rate_limit_per_second"`
	RateLimitBurst     int     `mapstructure:"rate_limit_burst"`
	// TrustedProxies lists the CIDRs (or single addresses) of reverse
	// proxies whose X-Forwarded-For is believed. Empty means the peer
	// address is always the client address.
	TrustedProxies []string `mapstructure:"trusted_proxies"`
	// WSMaxPerIP and WSMaxTotal cap concurrent WebSocket connections per
	// client address and across the process.
	WSMaxPerIP int `mapstructure:"ws_max_per_ip"`
	WSMaxTotal int `mapstructure:"ws_max_total"`
}

// TrustedProxyNets parses TrustedProxies into networks. A bare address is
// a /32 or /128.
func (s ServerConfig) TrustedProxyNets() ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(s.TrustedProxies))
	for _, raw := range s.TrustedProxies {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "/") {
			ip := net.ParseIP(raw)
			if ip == nil {
				return nil, fmt.Errorf("server.trusted_proxies: %q is not an address or CIDR", raw)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("server.trusted_proxies: %q: %w", raw, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// DatabaseConfig configures PostgreSQL access.
type DatabaseConfig struct {
	URL           string `mapstructure:"url"`
	MaxOpen       int    `mapstructure:"max_open"`
	MaxIdle       int    `mapstructure:"max_idle"`
	RunMigrations bool   `mapstructure:"run_migrations"`
}

// CollectorConfig configures the follower loops.
type CollectorConfig struct {
	// TickInterval is the fast loop cadence when polling. With a ws_url the
	// fast loop runs on every newHeads event instead and the timer only
	// fires while the subscription is down. A network's own tick_interval
	// overrides it for that network.
	TickInterval time.Duration `mapstructure:"tick_interval"`
	// SlowInterval is the cadence of the L1, balances, owner action and
	// batch report loop.
	SlowInterval    time.Duration `mapstructure:"slow_interval"`
	HeaderBatchSize int           `mapstructure:"header_batch_size"`
	BlockRetention  time.Duration `mapstructure:"block_retention"`
	SampleRetention time.Duration `mapstructure:"sample_retention"`
	BackfillDepth   time.Duration `mapstructure:"backfill_depth"`
	// BackfillAnchorInterval is how many blocks the backfill replays
	// between two state anchors on archive: true networks (default 1000).
	// Networks without archive ignore it: their backfill is a pure replay.
	BackfillAnchorInterval int `mapstructure:"backfill_anchor_interval"`
	// MaxCatchUpBatches bounds one fast tick's catch-up to this many header
	// batches on a budgeted network (default 10); a larger gap is skipped so
	// the follower never falls behind forever on a budget below the chain's
	// block rate. Unlimited networks (calls_per_second: 0) never skip.
	MaxCatchUpBatches int `mapstructure:"max_catch_up_batches"`
	// FailoverCooldown is how long a network stays on a fallback endpoint
	// after the active one failed before the primary is probed again with
	// one eth_chainId (default 60s).
	FailoverCooldown time.Duration `mapstructure:"failover_cooldown"`
	// EthUsdSource names the ETH/USD spot provider the slow loop reads once
	// per slow_interval for the whole process (not once per network):
	// "coinbase" (default), "coingecko", or an https URL answering JSON
	// with a top-level price holding a string or a number. An empty value
	// disables the fetch and every snapshot then reports ethUsd: null.
	// Environment: ETH_USD_SOURCE.
	EthUsdSource string `mapstructure:"eth_usd_source"`
	// EthUsdMaxAge is how long a fetched spot stays in the snapshot: past
	// it the tick and /live report ethUsd: null rather than a stale price
	// (default 10m). Environment: ETH_USD_MAX_AGE.
	EthUsdMaxAge time.Duration `mapstructure:"eth_usd_max_age"`
}

// DefaultEthUsdMaxAge is the fallback for collector.eth_usd_max_age, used
// by the API when it is not configured.
const DefaultEthUsdMaxAge = 10 * time.Minute

// EndpointConfig is one JSON-RPC endpoint of a network. The primary is
// described by the network's own rpc_url, ws_url, archive and
// calls_per_second; fallbacks carry the same four fields.
type EndpointConfig struct {
	RPCURL string `mapstructure:"rpc_url"`
	// WSURL is an optional ws:// or wss:// endpoint for the newHeads
	// subscription. The first endpoint that has one serves it.
	WSURL string `mapstructure:"ws_url"`
	// Archive is true when the node serves historical eth_call. The first
	// endpoint that has it serves the backfill anchors.
	Archive bool `mapstructure:"archive"`
	// CallsPerSecond is this endpoint's own JSON-RPC budget, 0 for
	// unlimited. Every endpoint has its own token bucket.
	CallsPerSecond float64 `mapstructure:"calls_per_second"`
}

// NetworkConfig describes one Arbitrum Nitro chain.
type NetworkConfig struct {
	Name        string `mapstructure:"name"`
	DisplayName string `mapstructure:"display_name"`
	ChainID     uint64 `mapstructure:"chain_id"`
	RPCURL      string `mapstructure:"rpc_url"`
	// WSURL is an optional ws:// or wss:// endpoint. When set the follower
	// subscribes to newHeads and samples state at each head's block number
	// instead of polling every tick_interval; while the socket is down it
	// falls back to the timer.
	WSURL       string `mapstructure:"ws_url"`
	ExplorerURL string `mapstructure:"explorer_url"`
	// CallsPerSecond is the JSON-RPC budget, counting every item inside a
	// batch. 0 means unlimited (a dedicated node): the pacer never waits,
	// catch-up never skips blocks and the batch report scan reads every
	// block. Batches stay capped at 100 items and 429 back-off still applies.
	// Must be finite and at most MaxCallsPerSecond.
	CallsPerSecond float64 `mapstructure:"calls_per_second"`
	// Archive is true when the node serves historical eth_call. The backfill
	// then anchors its replay to the real backlogs every
	// collector.backfill_anchor_interval blocks.
	Archive bool `mapstructure:"archive"`
	// TickInterval overrides collector.tick_interval for this network: the
	// fast loop's polling cadence and, with a ws_url, the safety timer
	// that polls while the subscription is down. Zero means the
	// collector-wide value. A dedicated endpoint can run at 250ms; a
	// public one should stay at 1s, since every tick costs about five
	// calls against its budget.
	TickInterval time.Duration `mapstructure:"tick_interval"`
	Enabled      bool          `mapstructure:"enabled"`
	// Fallbacks are further endpoints for the same chain, tried in order
	// when the active endpoint fails (see collector.failover_cooldown).
	// Capabilities are routed independently of the order: the first
	// endpoint with a ws_url serves newHeads, the first with archive: true
	// serves the backfill anchors. URLs that carry keys belong in the
	// environment (NETWORK_<NAME>_FALLBACK_RPC_URLS and friends), never in
	// config.yaml.
	Fallbacks []EndpointConfig `mapstructure:"fallbacks"`
}

// MaxCallsPerSecond bounds calls_per_second; anything above it is a typo
// rather than a budget (set 0 for a dedicated node).
const MaxCallsPerSecond = 10_000

// MinCallsPerSecond is the smallest accepted budget, one call every ten
// seconds. Below it a single token takes longer than any request timeout
// and the pacer's waits stop being meaningful, so such a value is a
// mistake rather than a budget: set 0 for a dedicated node instead.
const MinCallsPerSecond = 0.1

// Unlimited reports whether the network has no call budget.
func (n NetworkConfig) Unlimited() bool { return n.CallsPerSecond <= 0 }

// EffectiveTickInterval returns the network's tick_interval when set,
// otherwise the collector-wide one.
func (n NetworkConfig) EffectiveTickInterval(c CollectorConfig) time.Duration {
	if n.TickInterval > 0 {
		return n.TickInterval
	}
	return c.TickInterval
}

// Primary returns the network's primary endpoint.
func (n NetworkConfig) Primary() EndpointConfig {
	return EndpointConfig{RPCURL: n.RPCURL, WSURL: n.WSURL, Archive: n.Archive, CallsPerSecond: n.CallsPerSecond}
}

// Endpoints returns the primary endpoint followed by the fallbacks.
func (n NetworkConfig) Endpoints() []EndpointConfig {
	out := make([]EndpointConfig, 0, 1+len(n.Fallbacks))
	out = append(out, n.Primary())
	return append(out, n.Fallbacks...)
}

// fallback returns the i-th fallback, growing the list with zero-valued
// entries as needed.
func (n *NetworkConfig) fallback(i int) *EndpointConfig {
	for len(n.Fallbacks) <= i {
		n.Fallbacks = append(n.Fallbacks, EndpointConfig{})
	}
	return &n.Fallbacks[i]
}

// EnvKey returns the environment variable prefix for this network, for
// example NETWORK_ARBITRUM_ONE for "arbitrum-one".
func (n NetworkConfig) EnvKey() string {
	return "NETWORK_" + strings.ToUpper(strings.ReplaceAll(n.Name, "-", "_"))
}

// Options tunes loading.
type Options struct {
	// Path of the YAML file. Empty means CONFIG_PATH or config.yaml.
	Path string
	// RequireRPC makes validation fail when an enabled network has no rpc_url.
	RequireRPC bool
	// Getenv overrides os.LookupEnv, for tests.
	Getenv func(string) (string, bool)
}

// Load reads configuration for the collector: every enabled network must
// have an RPC URL.
func Load() (*Config, error) {
	return LoadWith(Options{RequireRPC: true})
}

// LoadForAPI reads configuration for the API, which never talks to an RPC.
func LoadForAPI() (*Config, error) {
	return LoadWith(Options{RequireRPC: false})
}

// LoadWith reads configuration according to opts.
func LoadWith(opts Options) (*Config, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.LookupEnv
	}
	path := opts.Path
	if path == "" {
		if p, ok := getenv("CONFIG_PATH"); ok && p != "" {
			path = p
		} else {
			path = "config.yaml"
		}
	}

	v := viper.New()
	setDefaults(v)
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("decode config %s: %w", path, err)
	}
	if err := applyEnv(&cfg, getenv); err != nil {
		return nil, err
	}
	if err := cfg.Validate(opts.RequireRPC); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.rate_limit_per_second", 20.0)
	v.SetDefault("server.rate_limit_burst", 60)
	v.SetDefault("server.ws_max_per_ip", 8)
	v.SetDefault("server.ws_max_total", 2000)
	v.SetDefault("database.max_open", 25)
	v.SetDefault("database.max_idle", 10)
	v.SetDefault("collector.tick_interval", "3s")
	v.SetDefault("collector.slow_interval", "60s")
	v.SetDefault("collector.header_batch_size", 100)
	v.SetDefault("collector.block_retention", "48h")
	v.SetDefault("collector.sample_retention", "168h")
	v.SetDefault("collector.backfill_depth", "720h")
	v.SetDefault("collector.backfill_anchor_interval", 1000)
	v.SetDefault("collector.max_catch_up_batches", 10)
	v.SetDefault("collector.failover_cooldown", "60s")
	v.SetDefault("collector.eth_usd_source", "coinbase")
	v.SetDefault("collector.eth_usd_max_age", DefaultEthUsdMaxAge.String())
	v.SetDefault("log_level", "info")
}

func applyEnv(cfg *Config, getenv func(string) (string, bool)) error {
	if s, ok := getenv("DB_URL"); ok && s != "" {
		cfg.Database.URL = s
	}
	if s, ok := getenv("PORT"); ok && s != "" {
		p, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("PORT: %w", err)
		}
		cfg.Server.Port = p
	}
	if s, ok := getenv("LOG_LEVEL"); ok && s != "" {
		cfg.LogLevel = s
	}
	if s, ok := getenv("DEV_MODE"); ok && s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("DEV_MODE: %w", err)
		}
		cfg.Server.DevMode = b
	}
	// An empty ETH_USD_SOURCE is meaningful (it disables the spot fetch),
	// so the variable being set is enough, unlike the others.
	if s, ok := getenv("ETH_USD_SOURCE"); ok {
		cfg.Collector.EthUsdSource = strings.TrimSpace(s)
	}
	if s, ok := getenv("ETH_USD_MAX_AGE"); ok && s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("ETH_USD_MAX_AGE: %w", err)
		}
		cfg.Collector.EthUsdMaxAge = d
	}
	for i := range cfg.Networks {
		n := &cfg.Networks[i]
		key := n.EnvKey()
		if s, ok := getenv(key + "_RPC_URL"); ok && s != "" {
			n.RPCURL = s
		}
		if s, ok := getenv(key + "_WS_URL"); ok && s != "" {
			n.WSURL = s
		}
		if s, ok := getenv(key + "_ENABLED"); ok && s != "" {
			b, err := strconv.ParseBool(s)
			if err != nil {
				return fmt.Errorf("%s_ENABLED: %w", key, err)
			}
			n.Enabled = b
		}
		if s, ok := getenv(key + "_ARCHIVE"); ok && s != "" {
			b, err := strconv.ParseBool(s)
			if err != nil {
				return fmt.Errorf("%s_ARCHIVE: %w", key, err)
			}
			n.Archive = b
		}
		if s, ok := getenv(key + "_CALLS_PER_SECOND"); ok && s != "" {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return fmt.Errorf("%s_CALLS_PER_SECOND: %w", key, err)
			}
			n.CallsPerSecond = f
		}
		if s, ok := getenv(key + "_TICK_INTERVAL"); ok && s != "" {
			d, err := time.ParseDuration(s)
			if err != nil {
				return fmt.Errorf("%s_TICK_INTERVAL: %w", key, err)
			}
			n.TickInterval = d
		}
		if err := applyFallbackEnv(n, getenv); err != nil {
			return err
		}
	}
	return nil
}

// applyFallbackEnv applies the positional fallback lists. Every list is
// comma-separated and position i addresses fallbacks[i], which is created
// with zero values when the YAML has fewer; an empty item leaves that
// position's field alone.
func applyFallbackEnv(n *NetworkConfig, getenv func(string) (string, bool)) error {
	key := n.EnvKey() + "_FALLBACK_"
	for i, v := range envList(getenv, key+"RPC_URLS") {
		if v != "" {
			n.fallback(i).RPCURL = v
		}
	}
	for i, v := range envList(getenv, key+"WS_URLS") {
		if v != "" {
			n.fallback(i).WSURL = v
		}
	}
	for i, v := range envList(getenv, key+"ARCHIVE") {
		if v == "" {
			continue
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%sARCHIVE[%d]: %w", key, i, err)
		}
		n.fallback(i).Archive = b
	}
	for i, v := range envList(getenv, key+"CALLS_PER_SECOND") {
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("%sCALLS_PER_SECOND[%d]: %w", key, i, err)
		}
		n.fallback(i).CallsPerSecond = f
	}
	return nil
}

// envList splits a comma-separated variable into trimmed items; an unset
// or empty variable is no items.
func envList(getenv func(string) (string, bool), name string) []string {
	s, ok := getenv(name)
	if !ok || s == "" {
		return nil
	}
	items := strings.Split(s, ",")
	for i := range items {
		items[i] = strings.TrimSpace(items[i])
	}
	return items
}

// Validate checks invariants: unique names and chain IDs, positive
// intervals (a network's tick_interval may also be zero, meaning the
// collector-wide one), sane batch sizes, non-negative call budgets, ws://
// or wss:// ws_url values and, when requireRPC is set, an RPC URL for
// every enabled network.
func (c *Config) Validate(requireRPC bool) error {
	var errs []error
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		errs = append(errs, fmt.Errorf("server.port %d out of range", c.Server.Port))
	}
	if c.Server.RateLimitPerSecond <= 0 || c.Server.RateLimitBurst <= 0 {
		errs = append(errs, errors.New("server.rate_limit_per_second and rate_limit_burst must be positive"))
	}
	if c.Server.WSMaxPerIP <= 0 || c.Server.WSMaxTotal <= 0 {
		errs = append(errs, errors.New("server.ws_max_per_ip and ws_max_total must be positive"))
	}
	if _, err := c.Server.TrustedProxyNets(); err != nil {
		errs = append(errs, err)
	}
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required"))
	}
	for name, d := range map[string]time.Duration{
		"collector.tick_interval":     c.Collector.TickInterval,
		"collector.slow_interval":     c.Collector.SlowInterval,
		"collector.block_retention":   c.Collector.BlockRetention,
		"collector.sample_retention":  c.Collector.SampleRetention,
		"collector.backfill_depth":    c.Collector.BackfillDepth,
		"collector.failover_cooldown": c.Collector.FailoverCooldown,
		"collector.eth_usd_max_age":   c.Collector.EthUsdMaxAge,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", name))
		}
	}
	if c.Collector.HeaderBatchSize <= 0 || c.Collector.HeaderBatchSize > 100 {
		errs = append(errs, fmt.Errorf("collector.header_batch_size %d must be between 1 and 100", c.Collector.HeaderBatchSize))
	}
	if c.Collector.BackfillAnchorInterval <= 0 {
		errs = append(errs, fmt.Errorf("collector.backfill_anchor_interval %d must be positive", c.Collector.BackfillAnchorInterval))
	}
	if c.Collector.MaxCatchUpBatches <= 0 {
		errs = append(errs, fmt.Errorf("collector.max_catch_up_batches %d must be positive", c.Collector.MaxCatchUpBatches))
	}
	if err := prices.ValidateSource(c.Collector.EthUsdSource); err != nil {
		errs = append(errs, fmt.Errorf("collector.eth_usd_source: %w", err))
	}
	names := map[string]bool{}
	ids := map[uint64]bool{}
	for _, n := range c.Networks {
		if n.Name == "" {
			errs = append(errs, errors.New("network name is required"))
			continue
		}
		if names[n.Name] {
			errs = append(errs, fmt.Errorf("duplicate network name %q", n.Name))
		}
		names[n.Name] = true
		if n.ChainID == 0 {
			errs = append(errs, fmt.Errorf("network %s: chain_id is required", n.Name))
		}
		if ids[n.ChainID] {
			errs = append(errs, fmt.Errorf("duplicate chain id %d", n.ChainID))
		}
		ids[n.ChainID] = true
		errs = append(errs, validateEndpoint("network "+n.Name, n.Primary())...)
		if n.TickInterval < 0 {
			errs = append(errs, fmt.Errorf("network %s: tick_interval %v must be positive (omit it for collector.tick_interval)", n.Name, n.TickInterval))
		}
		if requireRPC && n.Enabled && n.RPCURL == "" {
			errs = append(errs, fmt.Errorf("network %s: rpc_url is required (set %s_RPC_URL)", n.Name, n.EnvKey()))
		}
		for i, e := range n.Fallbacks {
			where := fmt.Sprintf("network %s: fallbacks[%d]", n.Name, i)
			errs = append(errs, validateEndpoint(where, e)...)
			if requireRPC && n.Enabled && e.RPCURL == "" {
				errs = append(errs, fmt.Errorf("%s: rpc_url is required (set %s_FALLBACK_RPC_URLS)", where, n.EnvKey()))
			}
		}
	}
	return errors.Join(errs...)
}

// validateEndpoint checks an endpoint's budget and ws_url scheme.
func validateEndpoint(where string, e EndpointConfig) []error {
	var errs []error
	switch {
	case e.CallsPerSecond < 0 || math.IsNaN(e.CallsPerSecond) || math.IsInf(e.CallsPerSecond, 0):
		errs = append(errs, fmt.Errorf("%s: calls_per_second must be zero (unlimited) or a finite positive number", where))
	case e.CallsPerSecond > 0 && e.CallsPerSecond < MinCallsPerSecond:
		errs = append(errs, fmt.Errorf("%s: calls_per_second %v is below the minimum of %v (use 0 for a dedicated node)", where, e.CallsPerSecond, MinCallsPerSecond))
	case e.CallsPerSecond > MaxCallsPerSecond:
		errs = append(errs, fmt.Errorf("%s: calls_per_second %v exceeds %d (use 0 for a dedicated node)", where, e.CallsPerSecond, MaxCallsPerSecond))
	}
	if e.WSURL != "" && !strings.HasPrefix(e.WSURL, "ws://") && !strings.HasPrefix(e.WSURL, "wss://") {
		errs = append(errs, fmt.Errorf("%s: ws_url %q must start with ws:// or wss://", where, e.WSURL))
	}
	return errs
}

// EnabledNetworks returns the networks with enabled: true.
func (c *Config) EnabledNetworks() []NetworkConfig {
	out := make([]NetworkConfig, 0, len(c.Networks))
	for _, n := range c.Networks {
		if n.Enabled {
			out = append(out, n)
		}
	}
	return out
}
