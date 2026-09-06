package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

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

// TestFollowerRunChainIDMismatch: a follower whose RPC reports another
// chain refuses to run and records why, and an eth_chainId failure is
// retried rather than trusted.
func TestFollowerRunChainIDMismatch(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	rpc.chainID = 46630
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	err := f.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "chain id 46630") {
		t.Fatalf("expected chain id mismatch, got %v", err)
	}
	n, _ := store.NetworkByRef(ctx, "robinhood")
	if n == nil || !n.LastError.Valid || !strings.Contains(n.LastError.String, "refusing to run") {
		t.Fatalf("mismatch not recorded: %+v", n)
	}
	if rpc.calledTimes("FastSample") != 0 || len(store.BlockRows[4663]) != 0 {
		t.Fatal("nothing may be sampled or written for the wrong chain")
	}
	rpc.chainID = 4663
	rpc.errs["ChainID"] = errRPC
	if err := f.Run(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("eth_chainId failure: %v", err)
	}
	delete(rpc.errs, "ChainID")
	store.FailOn["SetNetworkError"] = true
	rpc.chainID = 1
	if err := f.Run(ctx); err == nil {
		t.Fatal("mismatch must still fail when it cannot be recorded")
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
		r.chainID = n.ChainID
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

// fakeHeads is a scripted HeadSource: the test flips connected and pushes
// head numbers.
type fakeHeads struct {
	connected atomic.Bool
	heads     chan uint64
	started   chan struct{}
}

func newFakeHeads() *fakeHeads {
	return &fakeHeads{heads: make(chan uint64, 16), started: make(chan struct{})}
}

func (h *fakeHeads) Run(ctx context.Context, fn func(nitro.Head)) {
	close(h.started)
	for {
		select {
		case <-ctx.Done():
			return
		case n := <-h.heads:
			fn(nitro.Head{Number: n})
		}
	}
}

func (h *fakeHeads) Connected() bool { return h.connected.Load() }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFollowerRunOnHeads(t *testing.T) {
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	heads := newFakeHeads()
	f := NewFollower(Options{
		Network:   config.NetworkConfig{Name: "robinhood", ChainID: 4663, Enabled: true, CallsPerSecond: 4, WSURL: "ws://ignored"},
		Collector: fastConfig(),
		RPC:       rpc,
		Store:     store,
		Log:       logger.Nop(),
		Now:       func() time.Time { return baseTime.Add(100 * time.Second) },
		Sleep:     quickSleep,
		Heads:     heads,
	})
	if f.heads != heads {
		t.Fatal("Options.Heads must override the ws_url subscriber")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = f.Run(ctx)
		close(done)
	}()
	<-heads.started
	// Disconnected: the timer polls with FastSample.
	waitFor(t, "timer polling", func() bool { return rpc.calledTimes("FastSample") >= 2 })
	if f.Head() != 1000 || len(rpc.sampleAt) != 0 {
		t.Fatalf("polling phase: head %d, sampleAt %v", f.Head(), rpc.sampleAt)
	}
	// Connected: the first timer beat runs one explicit tick (a quiet chain
	// is sampled as soon as the subscription is acknowledged), then heads
	// drive ticks pinned to their block numbers and the timer stops
	// sampling.
	polled := rpc.calledTimes("FastSample")
	heads.connected.Store(true)
	waitFor(t, "explicit tick after the subscription", func() bool { return rpc.calledTimes("FastSample") == polled+1 })
	polled++
	rpc.setHead(1002)
	heads.heads <- 1001
	heads.heads <- 1002
	waitFor(t, "head ticks", func() bool { return f.Head() == 1002 })
	rpc.mu.Lock()
	sampled := append([]uint64(nil), rpc.sampleAt...)
	rpc.mu.Unlock()
	if len(sampled) == 0 || sampled[len(sampled)-1] != 1002 {
		t.Fatalf("sampleAt = %v", sampled)
	}
	time.Sleep(20 * time.Millisecond)
	if rpc.calledTimes("FastSample") != polled {
		t.Fatalf("timer must not poll while connected: %d != %d", rpc.calledTimes("FastSample"), polled)
	}
	// A head the node cannot serve yet is logged and dropped; the timer
	// then polls once as a safety net, which keeps the head where it was.
	polled = rpc.calledTimes("FastSample")
	heads.heads <- 1010
	waitFor(t, "failed head", func() bool {
		rpc.mu.Lock()
		defer rpc.mu.Unlock()
		return len(rpc.sampleAt) > len(sampled)
	})
	waitFor(t, "retry poll", func() bool { return rpc.calledTimes("FastSample") == polled+1 })
	time.Sleep(20 * time.Millisecond)
	if f.Head() != 1002 || rpc.calledTimes("FastSample") != polled+1 {
		t.Fatalf("after failed head: head %d, polls %d (want %d)", f.Head(), rpc.calledTimes("FastSample"), polled+1)
	}
	// Disconnected again: polling resumes and catches up on its own.
	heads.connected.Store(false)
	rpc.setHead(1010)
	waitFor(t, "polling resumed", func() bool { return f.Head() == 1010 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestFollowerRunOnRealSocket(t *testing.T) {
	// End to end: a newHeads WebSocket server drives ticks through the
	// real nitro.HeadSubscriber.
	stall := make(chan struct{})
	defer close(stall)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var req struct {
			ID uint64 `json:"id"`
		}
		_ = json.Unmarshal(data, &req)
		_ = conn.Write(r.Context(), websocket.MessageText, fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%d,"result":"0xabc"}`, req.ID))
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"0xabc","result":{"number":"0x3ea","hash":"0x1","timestamp":"0x1"}}}`))
		<-stall
	}))
	defer srv.Close()
	rpc := newFakeRPC(1002)
	store := dbtest.New()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	byURL := NewFollower(Options{Network: config.NetworkConfig{ChainID: 1, WSURL: wsURL}, Collector: fastConfig(), RPC: rpc, Store: store})
	if _, ok := byURL.heads.(*nitro.HeadSubscriber); !ok {
		t.Fatalf("ws_url should build a nitro.HeadSubscriber, got %T", byURL.heads)
	}
	f := NewFollower(Options{
		Network:   config.NetworkConfig{Name: "robinhood", ChainID: 4663, Enabled: true, CallsPerSecond: 4},
		Collector: fastConfig(),
		RPC:       rpc,
		Store:     store,
		Log:       logger.Nop(),
		Now:       func() time.Time { return baseTime.Add(100 * time.Second) },
		Sleep:     quickSleep,
		Heads:     nitro.NewHeadSubscriber(wsURL, nitro.WithHeadBackoff(time.Millisecond, 4*time.Millisecond)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = f.Run(ctx)
		close(done)
	}()
	waitFor(t, "head from the socket", func() bool {
		rpc.mu.Lock()
		defer rpc.mu.Unlock()
		return len(rpc.sampleAt) > 0 && rpc.sampleAt[0] == 1002
	})
	waitFor(t, "connected", f.heads.Connected)
	cancel()
	<-done
	if f.Head() != 1002 {
		t.Fatalf("head = %d", f.Head())
	}
}
