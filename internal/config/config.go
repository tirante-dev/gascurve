// Package config loads gascurve configuration from a YAML file (config.yaml
// or CONFIG_PATH) and applies environment overrides: DB_URL, PORT, LOG_LEVEL,
// DEV_MODE and per-network NETWORK_<NAME>_RPC_URL, NETWORK_<NAME>_ENABLED and
// NETWORK_<NAME>_CALLS_PER_SECOND, where NAME is the network name upper-cased
// with dashes replaced by underscores.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
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
	TickInterval    time.Duration `mapstructure:"tick_interval"`
	SlowInterval    time.Duration `mapstructure:"slow_interval"`
	HeaderBatchSize int           `mapstructure:"header_batch_size"`
	BlockRetention  time.Duration `mapstructure:"block_retention"`
	SampleRetention time.Duration `mapstructure:"sample_retention"`
	BackfillDepth   time.Duration `mapstructure:"backfill_depth"`
}

// NetworkConfig describes one Arbitrum Nitro chain.
type NetworkConfig struct {
	Name           string  `mapstructure:"name"`
	DisplayName    string  `mapstructure:"display_name"`
	ChainID        uint64  `mapstructure:"chain_id"`
	RPCURL         string  `mapstructure:"rpc_url"`
	ExplorerURL    string  `mapstructure:"explorer_url"`
	CallsPerSecond float64 `mapstructure:"calls_per_second"`
	Enabled        bool    `mapstructure:"enabled"`
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
	v.SetDefault("database.max_open", 25)
	v.SetDefault("database.max_idle", 10)
	v.SetDefault("collector.tick_interval", "1s")
	v.SetDefault("collector.slow_interval", "60s")
	v.SetDefault("collector.header_batch_size", 100)
	v.SetDefault("collector.block_retention", "48h")
	v.SetDefault("collector.sample_retention", "168h")
	v.SetDefault("collector.backfill_depth", "720h")
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
	for i := range cfg.Networks {
		n := &cfg.Networks[i]
		key := n.EnvKey()
		if s, ok := getenv(key + "_RPC_URL"); ok && s != "" {
			n.RPCURL = s
		}
		if s, ok := getenv(key + "_ENABLED"); ok && s != "" {
			b, err := strconv.ParseBool(s)
			if err != nil {
				return fmt.Errorf("%s_ENABLED: %w", key, err)
			}
			n.Enabled = b
		}
		if s, ok := getenv(key + "_CALLS_PER_SECOND"); ok && s != "" {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return fmt.Errorf("%s_CALLS_PER_SECOND: %w", key, err)
			}
			n.CallsPerSecond = f
		}
	}
	return nil
}

// Validate checks invariants: unique names and chain IDs, positive
// intervals, sane batch sizes and, when requireRPC is set, an RPC URL for
// every enabled network.
func (c *Config) Validate(requireRPC bool) error {
	var errs []error
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		errs = append(errs, fmt.Errorf("server.port %d out of range", c.Server.Port))
	}
	if c.Server.RateLimitPerSecond <= 0 || c.Server.RateLimitBurst <= 0 {
		errs = append(errs, errors.New("server.rate_limit_per_second and rate_limit_burst must be positive"))
	}
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required"))
	}
	for name, d := range map[string]time.Duration{
		"collector.tick_interval":    c.Collector.TickInterval,
		"collector.slow_interval":    c.Collector.SlowInterval,
		"collector.block_retention":  c.Collector.BlockRetention,
		"collector.sample_retention": c.Collector.SampleRetention,
		"collector.backfill_depth":   c.Collector.BackfillDepth,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", name))
		}
	}
	if c.Collector.HeaderBatchSize <= 0 || c.Collector.HeaderBatchSize > 100 {
		errs = append(errs, fmt.Errorf("collector.header_batch_size %d must be between 1 and 100", c.Collector.HeaderBatchSize))
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
		if n.CallsPerSecond <= 0 {
			errs = append(errs, fmt.Errorf("network %s: calls_per_second must be positive", n.Name))
		}
		if requireRPC && n.Enabled && n.RPCURL == "" {
			errs = append(errs, fmt.Errorf("network %s: rpc_url is required (set %s_RPC_URL)", n.Name, n.EnvKey()))
		}
	}
	return errors.Join(errs...)
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
