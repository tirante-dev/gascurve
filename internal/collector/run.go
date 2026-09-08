package collector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/metrics"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

// WithMetrics points every follower Run starts at the process instruments.
func WithMetrics(m *metrics.Collector) func(*Options) {
	return func(o *Options) { o.Metrics = m }
}

// Run drives the fast loop, the slow loop and the history loop until ctx ends. It returns early when
// the follower cannot initialize or when the RPC reports a different chain id than the one configured:
// nothing from the wrong chain may be written under this network's identity.
func (f *Follower) Run(ctx context.Context) error {
	start := f.now()
	if err := f.ensureInit(ctx); err != nil {
		f.observeLoop(loopFast, start, err)
		return err
	}
	if err := f.verifyChainID(ctx); err != nil {
		f.observeLoop(loopFast, start, err)
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
		f.runHistory(ctx)
	}()
	f.runFast(ctx)
	wg.Wait()
	return ctx.Err()
}

// verifyChainID refuses to run against an RPC whose eth_chainId differs from the configured chain id,
// recording the mismatch on the network row. With a pool every endpoint is verified (a mismatching one
// is disabled and shown in /status) and the follower runs as long as one is usable.
func (f *Follower) verifyChainID(ctx context.Context) error {
	if f.pool != nil {
		err := f.pool.Verify(ctx)
		// Observed either way, and before the error is returned: a pool with no usable endpoint stops the
		// follower before the slow loop ever runs, so this is the only place the endpoint gauges can come
		// from when every endpoint fails at startup.
		status := f.pool.Status()
		f.metrics.ObservePool(poolMetrics(f.rpc.Stats(), &status))
		if err != nil {
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

// runFast drives the fast loop: on the timer when polling, on newHeads events when a head source is
// configured. The timer runs at the network's effective tick interval.
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
		err := f.Tick(ctx)
		f.observeLoop(loopFast, start, err)
		if err != nil && ctx.Err() == nil {
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

// runOnHeads ticks at every head the subscription delivers, sampling state at that block number. Heads
// that arrive while a tick is running collapse into one tick at the newest of them. The timer keeps
// firing but only ticks while the subscription is down, so the follower degrades to polling and picks
// the subscription back up on its own. The first timer beat after it comes up runs one explicit tick,
// so a quiet chain is sampled as soon as the subscription is acknowledged. After a failed head tick the
// timer polls once, so a quiet chain cannot leave the follower stuck on a stale head.
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
			start := f.now()
			err := f.TickAt(ctx, n)
			f.observeLoop(loopFast, start, err)
			if err != nil && ctx.Err() == nil {
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
			start := f.now()
			err := f.Tick(ctx)
			f.observeLoop(loopFast, start, err)
			if err != nil && ctx.Err() == nil {
				f.log.Debug("tick error", "err", err.Error())
			}
		}
	}
}

func (f *Follower) runSlow(ctx context.Context) {
	for ctx.Err() == nil {
		start := f.now()
		err := f.SlowTick(ctx)
		f.observeLoop(loopSlow, start, err)
		if err != nil && ctx.Err() == nil {
			f.log.Debug("slow tick error", "err", err.Error())
		}
		if err := f.sleep(ctx, f.cfg.SlowInterval); err != nil {
			return
		}
	}
}

// runHistory rebuilds history: every iteration fills one batch of the newest fillable hole, then one
// batch of the poster-gas repair, and only when neither has work does it spend the iteration on the
// backfill. The repair goes before the backfill because it alone has a deadline: it rebuilds from block
// rows, which retention removes, while the backfill can resume at any time. Completion is never cached
// here: BackfillStep answers Done from the durable cursor, so a rewind that resets it is picked up.
func (f *Follower) runHistory(ctx context.Context) {
	for ctx.Err() == nil {
		start := f.now()
		status, fillErr := f.FillStep(ctx)
		if fillErr != nil {
			if ctx.Err() != nil {
				return
			}
			// Logged and observed, but not the end of the turn: a hole that fails persistently
			// (a malformed replay state, say) must not hold the repair off for good, all the more
			// now that an unfinished repair pins retention and would then hold every row above the
			// boundary indefinitely. The repair runs, and the restart delay follows it.
			f.log.Warn("gap fill error", "err", fillErr.Error())
			status = FillIdle
		}
		// The repair runs every iteration, whatever the fill did. A progressed fill must not take the
		// turn, or a long gap would hold the repair off for hours; an idle fill must not either, or
		// the repair would count a deferral only on the one turn in thirty the filler takes, and get
		// its own turn once in nine hundred. Each counts on its own counter every time.
		repair, err := f.RepairStep(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			f.observeLoop(loopHistory, start, err)
			f.log.Warn("poster gas repair error", "err", err.Error())
			if err := f.sleep(ctx, restartDelay); err != nil {
				return
			}
			continue
		}
		// A fill error backs off whatever the repair did: it has had its one step, and retrying
		// a hole that fails persistently once per repair batch would be a tight loop.
		if fillErr != nil {
			f.observeLoop(loopHistory, start, fillErr)
			if err := f.sleep(ctx, restartDelay); err != nil {
				return
			}
			continue
		}
		if status == FillProgressed || repair == RepairProgressed {
			f.observeLoop(loopHistory, start, nil)
			continue
		}
		if status == FillIdle || repair == RepairIdle {
			f.observeLoop(loopHistory, start, nil)
			if err := f.sleep(ctx, backfillIdle); err != nil {
				return
			}
			continue
		}
		// Only an iteration in which neither had work falls through to the backfill, the one job
		// with no deadline of its own.
		back, err := f.BackfillStep(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			f.observeLoop(loopHistory, start, err)
			f.log.Warn("backfill error", "err", err.Error())
			if err := f.sleep(ctx, restartDelay); err != nil {
				return
			}
			continue
		}
		f.observeLoop(loopHistory, start, err)
		switch back {
		case BackfillDone, BackfillIdle:
			// A finished backfill is polled at the idle cadence: the cursor answers Done without a call,
			// and a rewind that resets it puts the job back to work on its own.
			if err := f.sleep(ctx, backfillIdle); err != nil {
				return
			}
		case BackfillProgressed:
		}
	}
}

func (f *Follower) observeLoop(loop string, start time.Time, err error) {
	if f.monitor == nil || (err != nil && errors.Is(err, context.Canceled)) {
		return
	}
	f.monitor.observeLoop(f.chainID, loop, start, err)
	if loop == loopFast {
		f.monitor.observeHead(f.chainID, 0, f.Head())
	}
}

// Run starts one follower per enabled network and blocks until ctx ends. A follower that fails to
// initialize is restarted after restartDelay.
func Run(ctx context.Context, cfg *config.Config, store db.Store, newRPC func(config.NetworkConfig) RPC, log *logger.Logger, opts ...func(*Options)) {
	enabled := cfg.EnabledNetworks()
	followers := make([]*Follower, 0, len(enabled))
	monitors := map[*Monitor]bool{}
	for _, n := range enabled {
		o := Options{Network: n, Collector: cfg.Collector, RPC: newRPC(n), Store: store, Log: log}
		for _, apply := range opts {
			apply(&o)
		}
		f := NewFollower(o)
		followers = append(followers, f)
		if f.monitor != nil {
			monitors[f.monitor] = true
		}
	}
	base := Options{}
	for _, apply := range opts {
		apply(&base)
	}
	// A collector with no enabled networks still has a live health server: discover its monitor from the
	// process option so startup can complete instead of entering a probe restart loop.
	if len(followers) == 0 && base.Monitor != nil {
		monitors[base.Monitor] = true
	}
	var wg sync.WaitGroup
	if disabled := cfg.DisabledNetworks(); len(disabled) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			retireNetworks(ctx, store, disabled, base.Sleep, log)
		}()
	}
	for monitor := range monitors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			monitor.Run(ctx)
		}()
	}
	for _, f := range followers {
		wg.Add(1)
		go func(f *Follower) {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := f.Run(ctx); err != nil && ctx.Err() == nil {
					f.log.Error("follower stopped, restarting", "err", err.Error())
					if err := f.sleep(ctx, restartDelay); err != nil {
						return
					}
				}
			}
		}(f)
	}
	wg.Wait()
}

// retireNetworks marks the configured networks that are switched off, so the api stops listing and
// serving them. A follower registers the network it runs; one with enabled: false has no follower, and
// without this its row would keep the flag it was last started with and the chain would stay on the
// site with a head that never moves. Registration is retried because it is the only write these
// networks ever get: a database that is briefly unavailable at start must not leave one showing.
func retireNetworks(ctx context.Context, store db.Store, nets []config.NetworkConfig, sleep func(context.Context, time.Duration) error, log *logger.Logger) {
	if sleep == nil {
		sleep = sleepContext
	}
	for _, n := range nets {
		row := db.Network{ChainID: n.ChainID, Name: n.Name, DisplayName: n.DisplayName, ExplorerURL: n.ExplorerURL, Enabled: false}
		for ctx.Err() == nil {
			err := store.UpsertNetwork(ctx, row)
			if err == nil {
				log.Info("network disabled", "network", n.Name, "chain_id", n.ChainID)
				break
			}
			if ctx.Err() != nil {
				return
			}
			log.Error("could not record disabled network, retrying", "network", n.Name, "err", err.Error())
			if err := sleep(ctx, restartDelay); err != nil {
				return
			}
		}
	}
}

// clockNow is the default clock, exposed for tests to compare against.
var clockNow = time.Now
