package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

// Run drives the fast loop, the slow loop and the backfill until ctx ends.
// It returns early when the follower cannot initialize or when the RPC
// reports a different chain id than the one configured: nothing from the
// wrong chain may be written under this network's identity.
func (f *Follower) Run(ctx context.Context) error {
	if err := f.ensureInit(ctx); err != nil {
		return err
	}
	if err := f.verifyChainID(ctx); err != nil {
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

// verifyChainID refuses to run against an RPC whose eth_chainId differs
// from the configured chain id, recording the mismatch on the network row.
// With a pool every endpoint is verified (a mismatching one is disabled
// and shown in /status) and the follower runs as long as one is usable;
// capabilities are then routed among the usable endpoints.
func (f *Follower) verifyChainID(ctx context.Context) error {
	if f.pool != nil {
		if err := f.pool.Verify(ctx); err != nil {
			return f.refuse(ctx, fmt.Errorf("%w: refusing to run", err))
		}
		f.bindPool()
		return nil
	}
	id, err := f.rpc.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("eth_chainId: %w", err)
	}
	if id != f.chainID {
		f.log.Error("chain id mismatch", "rpcChainId", id, "configured", f.chainID)
		return f.refuse(ctx, fmt.Errorf("rpc reports chain id %d, configured %d: refusing to run", id, f.chainID))
	}
	return nil
}

// refuse records why the follower will not run on the network row.
func (f *Follower) refuse(ctx context.Context, err error) error {
	if serr := f.store.SetNetworkError(ctx, f.chainID, err.Error()); serr != nil {
		f.log.Warn("record error", "err", serr.Error())
	}
	return err
}

// runFast drives the fast loop: on the timer when polling, on newHeads
// events when a head source is configured. The timer runs at the
// network's effective tick interval (its own tick_interval, else
// collector.tick_interval).
func (f *Follower) runFast(ctx context.Context) {
	if f.heads == nil {
		f.runPolling(ctx)
		return
	}
	f.runOnHeads(ctx)
}

func (f *Follower) runPolling(ctx context.Context) {
	for ctx.Err() == nil {
		start := f.now()
		if err := f.Tick(ctx); err != nil && ctx.Err() == nil {
			f.log.Debug("tick error", "err", err.Error())
		}
		wait := f.tickInterval - f.now().Sub(start)
		if wait < f.tickInterval/10 {
			wait = f.tickInterval / 10
		}
		if err := f.sleep(ctx, wait); err != nil {
			return
		}
	}
}

// runOnHeads ticks at every head the subscription delivers, sampling state
// at that block number. Heads that arrive while a tick is running collapse
// into one tick at the newest of them (the catch-up fetches the rest). The
// timer keeps firing at the effective tick interval but only ticks while
// the subscription is down, so the follower degrades to polling and picks the
// subscription back up on its own; the first timer beat after the
// subscription comes up runs one explicit tick, so a quiet chain is
// sampled as soon as the subscription is acknowledged rather than at its
// next head. After a failed head tick (say the HTTP node has not seen that
// block yet) the timer polls once so a quiet chain cannot leave the
// follower stuck on a stale head.
func (f *Follower) runOnHeads(ctx context.Context) {
	var mu sync.Mutex
	var latest uint64
	signal := make(chan struct{}, 1)
	go f.heads.Run(ctx, func(h nitro.Head) {
		mu.Lock()
		latest = max(latest, h.Number)
		mu.Unlock()
		select {
		case signal <- struct{}{}:
		default:
		}
	})
	ticker := time.NewTicker(f.tickInterval)
	defer ticker.Stop()
	polling, retry := true, false
	for {
		select {
		case <-ctx.Done():
			return
		case <-signal:
			mu.Lock()
			n := latest
			mu.Unlock()
			if polling {
				polling = false
				f.log.Info("following newHeads, timer polling paused")
			}
			retry = false
			if err := f.TickAt(ctx, n); err != nil && ctx.Err() == nil {
				f.log.Debug("tick error", "head", n, "err", err.Error())
				retry = true
			}
		case <-ticker.C:
			connected := f.heads.Connected()
			switch {
			case connected && polling:
				// Acknowledged since the last beat: one explicit tick now.
				polling = false
				f.log.Info("following newHeads, timer polling paused")
			case connected && !retry:
				continue
			case !connected && !polling:
				polling = true
				f.log.Warn("newHeads subscription down, polling on the timer")
			}
			retry = false
			if err := f.Tick(ctx); err != nil && ctx.Err() == nil {
				f.log.Debug("tick error", "err", err.Error())
			}
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
