package collector

import (
	"context"
	"sync"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/logger"
)

// Run drives the fast loop, the slow loop and the backfill until ctx ends.
// It returns early only when the follower cannot initialize.
func (f *Follower) Run(ctx context.Context) error {
	if err := f.ensureInit(ctx); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		f.runSlow(ctx)
	}()
	go func() {
		defer wg.Done()
		f.runBackfill(ctx)
	}()
	f.runFast(ctx)
	wg.Wait()
	return ctx.Err()
}

func (f *Follower) runFast(ctx context.Context) {
	for ctx.Err() == nil {
		start := f.now()
		if err := f.Tick(ctx); err != nil && ctx.Err() == nil {
			f.log.Debug("tick error", "err", err.Error())
		}
		wait := f.cfg.TickInterval - f.now().Sub(start)
		if wait < f.cfg.TickInterval/10 {
			wait = f.cfg.TickInterval / 10
		}
		if err := f.sleep(ctx, wait); err != nil {
			return
		}
	}
}

func (f *Follower) runSlow(ctx context.Context) {
	for ctx.Err() == nil {
		if err := f.SlowTick(ctx); err != nil && ctx.Err() == nil {
			f.log.Debug("slow tick error", "err", err.Error())
		}
		if err := f.sleep(ctx, f.cfg.SlowInterval); err != nil {
			return
		}
	}
}

func (f *Follower) runBackfill(ctx context.Context) {
	for ctx.Err() == nil {
		status, err := f.BackfillStep(ctx)
		if err != nil && ctx.Err() == nil {
			f.log.Warn("backfill error", "err", err.Error())
			if err := f.sleep(ctx, restartDelay); err != nil {
				return
			}
			continue
		}
		switch status {
		case BackfillDone:
			return
		case BackfillIdle:
			if err := f.sleep(ctx, backfillIdle); err != nil {
				return
			}
		case BackfillProgressed:
		}
	}
}

// Run starts one follower per enabled network and blocks until ctx ends.
// A follower that fails to initialize is restarted after restartDelay.
func Run(ctx context.Context, cfg *config.Config, store db.Store, newRPC func(config.NetworkConfig) RPC, log *logger.Logger, opts ...func(*Options)) {
	var wg sync.WaitGroup
	for _, n := range cfg.EnabledNetworks() {
		o := Options{Network: n, Collector: cfg.Collector, RPC: newRPC(n), Store: store, Log: log}
		for _, apply := range opts {
			apply(&o)
		}
		f := NewFollower(o)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := f.Run(ctx); err != nil && ctx.Err() == nil {
					f.log.Error("follower stopped, restarting", "err", err.Error())
					if err := f.sleep(ctx, restartDelay); err != nil {
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

// clockNow is the default clock, exposed for tests to compare against.
var clockNow = time.Now
