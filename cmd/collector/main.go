// Command collector follows the configured Arbitrum Nitro chains, replays
// the pricer, writes PostgreSQL and publishes live snapshots with NOTIFY.
// It also serves Prometheus metrics on collector.metrics_port, from a
// server of its own that shares nothing with the followers.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/tirante-dev/gascurve/internal/collector"
	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/metrics"
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

	reg := metrics.NewRegistry()
	collectorMetrics := metrics.NewCollector(reg)
	var wg sync.WaitGroup
	if cfg.Collector.MetricsEnabled() {
		// Bound before the followers start, so a port already in use is a
		// startup failure rather than a process nobody can scrape. The
		// server reads the registry alone: a follower stuck on an RPC call
		// or on the database still answers a scrape.
		srv, err := metrics.NewServer(ctx, metrics.Addr(cfg.Collector.MetricsPort), reg, log)
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Run(ctx); err != nil {
				log.Error("metrics server stopped", "err", err.Error())
			}
		}()
	} else {
		log.Info("metrics server disabled", "reason", "collector.metrics_port is 0")
	}

	newRPC := func(n config.NetworkConfig) collector.RPC {
		return nitro.NewPool(nitro.PoolConfig{
			ChainID:   n.ChainID,
			Endpoints: n.Endpoints(),
			BatchSize: cfg.Collector.HeaderBatchSize,
			Cooldown:  cfg.Collector.FailoverCooldown,
		}, nitro.WithPoolLogger(log.With("network", n.Name)))
	}
	collector.Run(ctx, cfg, store, newRPC, log, collector.WithMetrics(collectorMetrics))
	wg.Wait()
	log.Info("collector stopped")
	return nil
}
