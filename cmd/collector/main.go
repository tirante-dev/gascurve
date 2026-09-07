// Command collector follows the configured Arbitrum Nitro chains, replays
// the pricer, writes PostgreSQL and publishes live snapshots with NOTIFY.
// It also serves health endpoints and Prometheus metrics on
// collector.metrics_port, from a server of its own.
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
	monitor := collector.NewMonitor(store, store, collectorMetrics, log)
	// The metrics server lives exactly as long as the followers do: its own
	// context is canceled when collector.Run returns, so a process with no
	// enabled network still exits instead of waiting on a server nobody
	// asked for.
	mctx, stopMetrics := context.WithCancel(ctx)
	defer stopMetrics()
	var wg sync.WaitGroup
	switch {
	case !cfg.Collector.MetricsEnabled():
		log.Info("metrics server disabled", "reason", "collector.metrics_port is 0")
	default:
		// A bind failure is a process configuration error now that Kubernetes
		// probes this listener. Dependency failures are handled by readiness
		// and never make the shallow liveness route fail.
		srv, err := metrics.NewServer(ctx, metrics.Addr(cfg.Collector.MetricsPort), reg, log, monitor.Handler(version.Version))
		if err != nil {
			return fmt.Errorf("collector observability: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Run(mctx); err != nil && ctx.Err() == nil {
				log.Error("collector observability server stopped", "err", err.Error())
			}
		}()
	}

	newRPC := func(n config.NetworkConfig) collector.RPC {
		return nitro.NewPool(nitro.PoolConfig{
			ChainID:   n.ChainID,
			Endpoints: n.Endpoints(),
			BatchSize: cfg.Collector.HeaderBatchSize,
			Cooldown:  cfg.Collector.FailoverCooldown,
		}, nitro.WithPoolLogger(log.With("network", n.Name)))
	}
	collector.Run(ctx, cfg, store, newRPC, log, collector.WithMetrics(collectorMetrics), func(o *collector.Options) { o.Monitor = monitor })
	stopMetrics()
	wg.Wait()
	log.Info("collector stopped")
	return nil
}
