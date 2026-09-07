// Command collector follows the configured Arbitrum Nitro chains, replays
// the pricer, writes PostgreSQL and publishes live snapshots with NOTIFY.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tirante-dev/gascurve/internal/collector"
	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "collector:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log, err := logger.New(cfg.LogLevel, cfg.Server.DevMode)
	if err != nil {
		return err
	}
	defer log.Sync()
	log.Info("starting collector", "version", version.Version, "networks", len(cfg.EnabledNetworks()))

	pool, err := db.Open(cfg.Database.URL, cfg.Database.MaxOpen, cfg.Database.MaxIdle)
	if err != nil {
		return err
	}
	defer pool.Close()
	if cfg.Database.RunMigrations {
		if err := db.RunMigrations(pool.DB); err != nil {
			return err
		}
	}
	store := db.NewPostgres(pool)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := store.Ping(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}

	newRPC := func(n config.NetworkConfig) collector.RPC {
		return nitro.NewPool(nitro.PoolConfig{
			ChainID:   n.ChainID,
			Endpoints: n.Endpoints(),
			BatchSize: cfg.Collector.HeaderBatchSize,
			Cooldown:  cfg.Collector.FailoverCooldown,
		}, nitro.WithPoolLogger(log.With("network", n.Name)))
	}
	collector.Run(ctx, cfg, store, newRPC, log)
	log.Info("collector stopped")
	return nil
}
