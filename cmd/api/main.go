// Command api serves the gascurve REST API, the WebSocket and Prometheus
// metrics from PostgreSQL, all on server.port.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/tirante-dev/gascurve/internal/api"
	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/metrics"
	"github.com/tirante-dev/gascurve/internal/version"
)

const (
	listenerStartTimeout = 30 * time.Second
	shutdownTimeout      = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadForAPI()
	if err != nil {
		return err
	}
	log, err := logger.New(cfg.LogLevel, cfg.Server.DevMode)
	if err != nil {
		return err
	}
	defer log.Sync()
	log.Info("starting api", "version", version.Version, "port", cfg.Server.Port)

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

	lctx, lcancel := context.WithTimeout(ctx, listenerStartTimeout)
	listener, err := db.NewListener(lctx, cfg.Database.URL, []string{db.ChannelLive, db.ChannelOwnerAction}, log)
	lcancel()
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()

	hub := api.NewHub(store, log, api.WithOrigins(cfg.Server.CORSOrigins))
	go hub.Run(ctx, listener)

	reg := metrics.NewRegistry()
	server := api.New(store, cfg.Server, hub, log,
		api.WithVersion(version.Version),
		api.WithEthUsdMaxAge(cfg.Collector.EthUsdMaxAge),
		api.WithMetrics(metrics.NewAPI(reg), reg))
	httpServer := &http.Server{
		Addr:              net.JoinHostPort("", strconv.Itoa(cfg.Server.Port)),
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		if err != nil {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(sctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
