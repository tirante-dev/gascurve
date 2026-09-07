package collector

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/prices"
)

// testClock is a clock the tests move by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeFetcher counts fetches and answers with a fixed quote or an error.
type fakeFetcher struct {
	mu    sync.Mutex
	calls int
	price string
	at    time.Time
	err   error
}

func (f *fakeFetcher) Fetch(context.Context) (*prices.Price, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &prices.Price{Price: f.price, At: f.at, Source: prices.SourceCoinbase}, nil
}

func (f *fakeFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeFetcher) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// observedLog returns a logger recording everything at debug and above.
func observedLog() (*logger.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	return logger.FromZap(zap.New(core)), logs
}

// ethUsdFollower builds a follower for one chain wired to a given cache and
// clock, without touching the process-wide cache.
func ethUsdFollower(t *testing.T, store *dbtest.MemStore, chainID uint64, cache *ethUsdCache, now func() time.Time, log *logger.Logger) *Follower {
	t.Helper()
	f := NewFollower(Options{
		Network:   config.NetworkConfig{Name: "network-" + strconv.FormatUint(chainID, 10), ChainID: chainID, CallsPerSecond: 4, Enabled: true},
		Collector: testConfig(),
		RPC:       newFakeRPC(1000),
		Store:     store,
		Log:       log,
		Now:       now,
		Sleep:     func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	})
	f.ethUsd = cache
	return f
}

// stateEthUsd reads the recorded spot of a chain.
func stateEthUsd(t *testing.T, store *dbtest.MemStore, chainID uint64) (model.EthUsd, bool) {
	t.Helper()
	raw, ok, err := store.GetState(context.Background(), chainID, db.StateEthUsd)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return model.EthUsd{}, false
	}
	var v model.EthUsd
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("recorded spot is not the documented shape: %v", err)
	}
	return v, true
}

// TestEthUsdOneFetchPerInterval: the price is the same for every network,
// so several followers sharing the process-wide cache cause one fetch per
// interval, and each of them records it for its own chain.
func TestEthUsdOneFetchPerInterval(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{t: baseTime}
	fetch := &fakeFetcher{price: "4523.40", at: baseTime}
	cache := newEthUsdCache(fetch, time.Minute)
	store := dbtest.New()
	chains := []uint64{4663, 46630, 42161, 421614}
	followers := make([]*Follower, 0, len(chains))
	for _, id := range chains {
		followers = append(followers, ethUsdFollower(t, store, id, cache, clock.now, logger.Nop()))
	}
	for _, f := range followers {
		if err := f.sampleEthUsd(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if fetch.count() != 1 {
		t.Fatalf("%d fetches for %d followers, want 1", fetch.count(), len(followers))
	}
	for _, id := range chains {
		v, ok := stateEthUsd(t, store, id)
		if !ok || v.Price != "4523.40" || v.Source != prices.SourceCoinbase || v.At != baseTime.Format(time.RFC3339) {
			t.Fatalf("chain %d recorded %+v (found %v)", id, v, ok)
		}
	}
	// Inside the interval nothing is fetched again.
	clock.advance(59 * time.Second)
	for _, f := range followers {
		if err := f.sampleEthUsd(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if fetch.count() != 1 {
		t.Fatalf("%d fetches within one interval, want 1", fetch.count())
	}
	// The interval elapsed: exactly one more fetch for the whole process.
	clock.advance(time.Second)
	fetch.mu.Lock()
	fetch.price, fetch.at = "4600.00", baseTime.Add(time.Minute)
	fetch.mu.Unlock()
	for _, f := range followers {
		if err := f.sampleEthUsd(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if fetch.count() != 2 {
		t.Fatalf("%d fetches after the interval, want 2", fetch.count())
	}
	if v, _ := stateEthUsd(t, store, chains[0]); v.Price != "4600.00" {
		t.Fatalf("the new quote was not recorded: %+v", v)
	}
}

// TestEthUsdFailureKeepsLastValue: a provider blip leaves the last quote in
// place and is logged at debug at most once a minute.
func TestEthUsdFailureKeepsLastValue(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{t: baseTime}
	fetch := &fakeFetcher{price: "4523.40", at: baseTime}
	cache := newEthUsdCache(fetch, 10*time.Second)
	log, logs := observedLog()
	if p := cache.value(ctx, clock.now(), log); p == nil || p.Price != "4523.40" {
		t.Fatalf("first fetch: %+v", p)
	}
	fetch.setErr(errRPC)
	for i := 0; i < 5; i++ { // t+10s .. t+50s
		clock.advance(10 * time.Second)
		p := cache.value(ctx, clock.now(), log)
		if p == nil || p.Price != "4523.40" {
			t.Fatalf("failure %d dropped the last value: %+v", i, p)
		}
	}
	if fetch.count() != 6 {
		t.Fatalf("%d fetches, want one per interval", fetch.count())
	}
	if n := logs.FilterMessageSnippet("eth/usd fetch failed").Len(); n != 1 {
		t.Fatalf("%d failure logs in the first minute, want 1", n)
	}
	if lvl := logs.All()[0].Level; lvl != zap.DebugLevel {
		t.Fatalf("failure logged at %v, want debug", lvl)
	}
	clock.advance(30 * time.Second) // t+80s, over a minute since the log
	cache.value(ctx, clock.now(), log)
	if n := logs.FilterMessageSnippet("eth/usd fetch failed").Len(); n != 2 {
		t.Fatalf("%d failure logs after a minute, want 2", n)
	}
}

// TestEthUsdNeverFetched: before the first success there is nothing to
// publish and nothing is recorded.
func TestEthUsdNeverFetched(t *testing.T) {
	ctx := context.Background()
	fetch := &fakeFetcher{err: errRPC}
	store := dbtest.New()
	f := ethUsdFollower(t, store, 4663, newEthUsdCache(fetch, time.Minute), func() time.Time { return baseTime }, logger.Nop())
	if err := f.sampleEthUsd(ctx); err != nil {
		t.Fatalf("a failed fetch must not fail the slow tick: %v", err)
	}
	if _, ok := stateEthUsd(t, store, 4663); ok {
		t.Fatal("nothing should be recorded before the first successful fetch")
	}
}

// TestEthUsdDisabled: an empty source means no fetcher, no state key and a
// null ethUsd in every snapshot.
func TestEthUsdDisabled(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if f.ethUsd != nil {
		t.Fatal("an empty eth_usd_source must leave the cache unset")
	}
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := stateEthUsd(t, store, 4663); ok {
		t.Fatal("a disabled source must not record a spot")
	}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if snap := f.Snapshot(); snap == nil || snap.EthUsd != nil {
		t.Fatalf("snapshot should carry a null spot: %+v", snap)
	}
	n, _ := store.LastNotification(db.ChannelLive)
	if !strings.Contains(n.Payload, `"ethUsd":null`) {
		t.Fatalf("notify payload should carry a null spot: %s", n.Payload)
	}
}

// TestEthUsdStaleness: the tick publishes a quote while it is younger than
// eth_usd_max_age and null once it is not, in the snapshot and in the
// NOTIFY payload alike.
func TestEthUsdStaleness(t *testing.T) {
	ctx := context.Background()
	now := baseTime.Add(2 * time.Hour)
	for _, tc := range []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"fresh", 0, true},
		{"at the cutoff", 10 * time.Minute, true},
		{"stale", 10*time.Minute + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := dbtest.New()
			f := ethUsdFollower(t, store, 4663, nil, func() time.Time { return now }, logger.Nop())
			f.ethUsdPrice = &prices.Price{Price: "4523.40", At: now.Add(-tc.age), Source: prices.SourceCoinbase}
			if err := f.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			snap := f.Snapshot()
			n, _ := store.LastNotification(db.ChannelLive)
			if !tc.want {
				if snap.EthUsd != nil || !strings.Contains(n.Payload, `"ethUsd":null`) {
					t.Fatalf("a quote %v old must be dropped: %+v %s", tc.age, snap.EthUsd, n.Payload)
				}
				return
			}
			if snap.EthUsd == nil || snap.EthUsd.Price != "4523.40" || snap.EthUsd.Source != prices.SourceCoinbase {
				t.Fatalf("snapshot spot: %+v", snap.EthUsd)
			}
			if snap.EthUsd.At != now.Add(-tc.age).UTC().Format(time.RFC3339) {
				t.Fatalf("snapshot spot time: %+v", snap.EthUsd)
			}
			if !strings.Contains(n.Payload, `"ethUsd":{"price":"4523.40"`) {
				t.Fatalf("notify payload: %s", n.Payload)
			}
		})
	}
}

// TestEthUsdRestoredAtStart: a restarted collector keeps publishing the
// recorded quote until it ages out, and survives an unreadable row.
func TestEthUsdRestoredAtStart(t *testing.T) {
	ctx := context.Background()
	cache := newEthUsdCache(&fakeFetcher{price: "1", at: baseTime}, time.Minute)
	for _, tc := range []struct {
		name  string
		row   string
		price string
	}{
		{"recorded", `{"price":"4523.40","at":"2026-09-06T07:00:00Z","source":"coinbase"}`, "4523.40"},
		{"unreadable", `not json`, ""},
		{"bad timestamp", `{"price":"4523.40","at":"yesterday","source":"coinbase"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := dbtest.New()
			if err := store.SetState(ctx, 4663, db.StateEthUsd, tc.row); err != nil {
				t.Fatal(err)
			}
			log, logs := observedLog()
			f := ethUsdFollower(t, store, 4663, cache, func() time.Time { return baseTime }, log)
			if err := f.ensureInit(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.price == "" {
				if f.ethUsdPrice != nil {
					t.Fatalf("an unreadable row must be ignored: %+v", f.ethUsdPrice)
				}
				if logs.FilterMessageSnippet("unreadable eth/usd").Len() != 1 {
					t.Fatal("an unreadable row should be logged")
				}
				return
			}
			if f.ethUsdPrice == nil || f.ethUsdPrice.Price != tc.price || !f.ethUsdPrice.At.Equal(baseTime) {
				t.Fatalf("restored quote: %+v", f.ethUsdPrice)
			}
		})
	}
	// A database failure is reported rather than swallowed.
	failing := dbtest.New()
	failing.SetFailure("GetState", true)
	f := ethUsdFollower(t, failing, 4663, cache, func() time.Time { return baseTime }, logger.Nop())
	if err := f.loadEthUsdLocked(ctx); err == nil {
		t.Fatal("expected the state read failure to be reported")
	}
	// A disabled source restores nothing, even with a row in place.
	store := dbtest.New()
	if err := store.SetState(ctx, 4663, db.StateEthUsd, `{"price":"4523.40","at":"2026-09-06T07:00:00Z","source":"coinbase"}`); err != nil {
		t.Fatal(err)
	}
	disabled := ethUsdFollower(t, store, 4663, nil, func() time.Time { return baseTime }, logger.Nop())
	if err := disabled.ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if disabled.ethUsdPrice != nil {
		t.Fatalf("a disabled source must restore nothing: %+v", disabled.ethUsdPrice)
	}
}

// TestEthUsdSlowTickPersists: the spot is a step of the slow loop, and a
// database failure there is reported like any other step.
func TestEthUsdSlowTickPersists(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	f := ethUsdFollower(t, store, 4663, newEthUsdCache(&fakeFetcher{price: "4523.40", at: baseTime}, time.Minute),
		func() time.Time { return baseTime }, logger.Nop())
	if err := f.SlowTick(ctx); err != nil {
		t.Fatal(err)
	}
	if v, ok := stateEthUsd(t, store, 4663); !ok || v.Price != "4523.40" {
		t.Fatalf("slow tick did not record the spot: %+v %v", v, ok)
	}
	store.SetFailure("SetState", true)
	err := f.SlowTick(ctx)
	if err == nil || !strings.Contains(err.Error(), "eth usd") {
		t.Fatalf("a failed write should be reported by the step: %v", err)
	}
}

// TestSharedEthUsdCache: the cache is process-wide, built once, and a
// source that cannot be used disables the spot instead of failing the
// follower.
func TestSharedEthUsdCache(t *testing.T) {
	reset := func() {
		sharedEthUsdMu.Lock()
		sharedEthUsd = nil
		sharedEthUsdMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	first := ethUsdCacheFor(prices.SourceCoinbase, time.Minute, logger.Nop())
	if first == nil {
		t.Fatal("coinbase should build a cache")
	}
	if again := ethUsdCacheFor(prices.SourceCoinGecko, time.Minute, logger.Nop()); again != first {
		t.Fatal("every follower must share one cache")
	}
	reset()
	log, logs := observedLog()
	if c := ethUsdCacheFor("kraken", time.Minute, log); c != nil {
		t.Fatal("an unusable source must disable the spot")
	}
	if logs.FilterMessageSnippet("eth/usd source unusable").Len() != 1 {
		t.Fatal("an unusable source should be logged")
	}
	// A configured source wires the follower to the shared cache.
	reset()
	cfg := testConfig()
	cfg.EthUsdSource = prices.SourceCoinbase
	f := NewFollower(Options{
		Network: config.NetworkConfig{Name: "robinhood", ChainID: 4663, Enabled: true}, Collector: cfg,
		RPC: newFakeRPC(1), Store: dbtest.New(), Log: logger.Nop(),
	})
	if f.ethUsd == nil {
		t.Fatal("a configured source should wire the follower to the cache")
	}
	if f.cfg.EthUsdMaxAge != defaultEthUsdMaxAge {
		t.Fatalf("eth_usd_max_age default = %v", f.cfg.EthUsdMaxAge)
	}
}

// TestEthUsdPublishedAfterItsCheckpoint: the quote a tick publishes and the
// one /live reads are the same row, so a checkpoint write that fails must
// leave the in-memory quote alone. Publishing first would put a quote in
// the snapshots and the WebSocket that /live cannot see.
func TestEthUsdPublishedAfterItsCheckpoint(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{t: baseTime}
	fetch := &fakeFetcher{price: "4523.40", at: baseTime}
	store := dbtest.New()
	f := ethUsdFollower(t, store, 4663, newEthUsdCache(fetch, time.Minute), clock.now, logger.Nop())
	store.FailOn["SetState"] = true
	if err := f.sampleEthUsd(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("a checkpoint failure must surface: %v", err)
	}
	f.mu.Lock()
	published := f.ethUsdPrice
	f.mu.Unlock()
	if published != nil {
		t.Fatalf("no quote may be published before its checkpoint: %+v", published)
	}
	// Once the write succeeds the quote is published and the two agree.
	store.FailOn["SetState"] = false
	clock.advance(time.Minute)
	if err := f.sampleEthUsd(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	published = f.ethUsdPrice
	f.mu.Unlock()
	recorded, ok := stateEthUsd(t, store, 4663)
	if !ok || published == nil || published.Price != recorded.Price {
		t.Fatalf("the published quote and the checkpoint must agree: %+v %+v", published, recorded)
	}
}
