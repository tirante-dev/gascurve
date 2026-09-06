package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

func legacyParams() nitro.LegacyParams {
	return nitro.LegacyParams{SpeedLimit: 7_000_000, Inertia: 102, Tolerance: 10, Backlog: 0}
}

// quickSleep caps every sleep at 2ms so loops spin fast in tests.
func quickSleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(min(d, 2*time.Millisecond)):
		return nil
	}
}

func fastConfig() config.CollectorConfig {
	return config.CollectorConfig{
		TickInterval: 5 * time.Millisecond, SlowInterval: 10 * time.Millisecond, HeaderBatchSize: 10,
		BlockRetention: time.Hour, SampleRetention: time.Hour, BackfillDepth: 30 * time.Second,
	}
}

func TestFollowerRun(t *testing.T) {
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := NewFollower(Options{
		Network:   config.NetworkConfig{Name: "robinhood", ChainID: 4663, Enabled: true},
		Collector: fastConfig(),
		RPC:       rpc,
		Store:     store,
		Log:       logger.Nop(),
		Now:       func() time.Time { return baseTime.Add(100 * time.Second) },
		Sleep:     quickSleep,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	// Make the RPC fail for a while so the error paths in the loops run.
	rpc.errs["FastSample"] = errRPC
	rpc.errs["L1Sample"] = errRPC
	go func() {
		time.Sleep(50 * time.Millisecond)
		rpc.mu.Lock()
		delete(rpc.errs, "FastSample")
		delete(rpc.errs, "L1Sample")
		rpc.mu.Unlock()
	}()
	if err := f.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v", err)
	}
	if f.Head() != 1000 {
		t.Fatalf("head = %d", f.Head())
	}
	if _, ok, _ := store.GetState(context.Background(), 4663, db.StateArbOSVersion); !ok {
		t.Fatal("slow loop did not run")
	}
	c, err := f.loadCursor(context.Background())
	if err != nil || !c.Done {
		t.Fatalf("backfill did not complete: %+v %v", c, err)
	}
}

func TestFollowerRunInitFailure(t *testing.T) {
	store := dbtest.New()
	store.FailOn["UpsertNetwork"] = true
	f := NewFollower(Options{Network: config.NetworkConfig{ChainID: 1}, Collector: fastConfig(), RPC: newFakeRPC(1), Store: store})
	if err := f.Run(context.Background()); err == nil {
		t.Fatal("expected init error")
	}
}

func TestRunManager(t *testing.T) {
	cfg := &config.Config{
		Collector: fastConfig(),
		Networks: []config.NetworkConfig{
			{Name: "a", ChainID: 1, Enabled: true, RPCURL: "http://a"},
			{Name: "b", ChainID: 2, Enabled: false},
		},
	}
	store := dbtest.New()
	// The follower fails to initialize until the store recovers, which
	// exercises the restart path.
	store.FailOn["UpsertNetwork"] = true
	rpcs := map[string]*fakeRPC{}
	newRPC := func(n config.NetworkConfig) RPC {
		r := newFakeRPC(50)
		rpcs[n.Name] = r
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go func() {
		time.Sleep(20 * time.Millisecond)
		store.SetFailure("UpsertNetwork", false)
	}()
	Run(ctx, cfg, store, newRPC, logger.Nop(), func(o *Options) { o.Sleep = quickSleep })
	if len(rpcs) != 1 || rpcs["a"] == nil {
		t.Fatalf("expected one follower, got %v", rpcs)
	}
	if rpcs["a"].calledTimes("FastSample") == 0 {
		t.Fatal("follower never ticked")
	}
	if clockNow == nil {
		t.Fatal("clock")
	}
}
