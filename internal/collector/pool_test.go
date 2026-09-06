package collector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

// fakePool is an EndpointPool over a fakeRPC with scripted verification,
// capabilities and status.
type fakePool struct {
	*fakeRPC
	verifyErr error
	verified  int
	status    nitro.PoolStatus
	ws        *nitro.Endpoint
	archive   *nitro.Endpoint
}

func (p *fakePool) Verify(context.Context) error {
	p.verified++
	return p.verifyErr
}
func (p *fakePool) WS() *nitro.Endpoint      { return p.ws }
func (p *fakePool) Archive() *nitro.Endpoint { return p.archive }
func (p *fakePool) Status() nitro.PoolStatus { return p.status }

// chainIDServer is a JSON-RPC server that only answers eth_chainId.
func chainIDServer(t *testing.T, id string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqs []struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		out := make([]map[string]any, 0, len(reqs))
		for _, req := range reqs {
			resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
			if req.Method == "eth_chainId" {
				resp["result"] = id
			} else {
				resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
			}
			out = append(out, resp)
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFollowerWithPool: a pool is verified instead of a single eth_chainId,
// its routing state is persisted for /status, and a pool without a usable
// endpoint stops the follower with the reason recorded.
func TestFollowerWithPool(t *testing.T) {
	ctx := context.Background()
	pool := &fakePool{fakeRPC: newFakeRPC(1000), status: nitro.PoolStatus{
		Active: 1, Failovers: 2,
		Endpoints: []nitro.EndpointStatus{{Index: 0, Disabled: true}, {Index: 1, WS: true, Archive: true}},
	}}
	store := dbtest.New()
	f := NewFollower(Options{
		Network:   config.NetworkConfig{Name: "robinhood", ChainID: 4663, Enabled: true, WSURL: "wss://ignored", Archive: true},
		Collector: testConfig(), RPC: pool, Store: store, Log: logger.Nop(),
	})
	// With a pool the network's own ws_url and archive flags describe the
	// primary endpoint; nothing is bound before verification.
	if f.pool == nil || f.heads != nil || f.archive != nil {
		t.Fatalf("pool follower before verification: heads=%v archive=%v", f.heads, f.archive)
	}
	if err := f.verifyChainID(ctx); err != nil || pool.verified != 1 || pool.calledTimes("ChainID") != 0 {
		t.Fatalf("verify: %v verified=%d", err, pool.verified)
	}
	// The fake pool offers no capabilities: polling, pure replay.
	if f.heads != nil || f.archive != nil {
		t.Fatal("no capabilities to bind")
	}
	if err := f.persistStats(ctx); err != nil {
		t.Fatal(err)
	}
	raw, ok, _ := store.GetState(ctx, 4663, db.StateEndpoints)
	want := `{"activeEndpoint":1,"failovers":2,"endpoints":[{"index":0,"ws":false,"archive":false,"disabled":true},{"index":1,"ws":true,"archive":true,"disabled":false}]}`
	if !ok || raw != want {
		t.Fatalf("endpoints state = %s", raw)
	}
	// Nothing usable: the follower refuses to run and records why.
	pool.verifyErr = errors.New("no usable endpoint: endpoint 0: reports chain id 1, configured 4663")
	err := f.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "refusing to run") {
		t.Fatalf("expected refusal, got %v", err)
	}
	n, _ := store.NetworkByRef(ctx, "robinhood")
	if n == nil || !n.LastError.Valid || !strings.Contains(n.LastError.String, "chain id 1") || pool.calledTimes("FastSample") != 0 {
		t.Fatalf("refusal not recorded: %+v", n)
	}
	store.FailOn["SetNetworkError"] = true
	if err := f.Run(ctx); err == nil {
		t.Fatal("refusal must still fail when it cannot be recorded")
	}
	// A plain client keeps the single eth_chainId path and the network's
	// own capabilities.
	rpc := newFakeRPC(1000)
	plain := NewFollower(Options{Network: config.NetworkConfig{ChainID: 4663, Archive: true}, Collector: testConfig(), RPC: rpc, Store: dbtest.New()})
	if plain.pool != nil || plain.archive != ArchiveRPC(rpc) {
		t.Fatal("plain client archive routing")
	}
}

// TestFollowerBindsPoolCapabilities: with a real pool whose primary has
// neither a ws_url nor archive, heads are followed on the fallback's
// WebSocket and anchors are routed to the fallback's archive state.
func TestFollowerBindsPoolCapabilities(t *testing.T) {
	ctx := context.Background()
	primary, fallback := chainIDServer(t, "0x1237"), chainIDServer(t, "0x1237")
	pool := nitro.NewPool(nitro.PoolConfig{ChainID: 4663, Endpoints: []config.EndpointConfig{
		{RPCURL: primary.URL, CallsPerSecond: 4},
		{RPCURL: fallback.URL, WSURL: "ws://127.0.0.1:1/ws", Archive: true},
	}, BatchSize: 10, Cooldown: time.Minute})
	f := NewFollower(Options{Network: config.NetworkConfig{Name: "robinhood", ChainID: 4663, Enabled: true}, Collector: testConfig(), RPC: pool, Store: dbtest.New(), Log: logger.Nop()})
	if err := f.verifyChainID(ctx); err != nil {
		t.Fatal(err)
	}
	sub, ok := f.heads.(*nitro.HeadSubscriber)
	if !ok || sub == nil {
		t.Fatalf("heads should follow the fallback's WebSocket, got %T", f.heads)
	}
	if f.archive != ArchiveRPC(pool.Archive()) || pool.Archive().Index() != 1 {
		t.Fatalf("archive should be the fallback endpoint, got %v", f.archive)
	}
	if st := pool.Status(); st.Active != 0 || st.Endpoints[1].Disabled || !st.Endpoints[1].WS || !st.Endpoints[1].Archive {
		t.Fatalf("status: %+v", st)
	}
	// Re-verification (a restarted follower) keeps the same bindings.
	if err := f.verifyChainID(ctx); err != nil || f.heads != HeadSource(sub) {
		t.Fatalf("rebinding: %v", err)
	}
	// Explicit options win over the pool's routing.
	heads, archive := newFakeHeads(), newFakeRPC(1)
	g := NewFollower(Options{Network: config.NetworkConfig{ChainID: 4663}, Collector: testConfig(), RPC: pool, Store: dbtest.New(), Heads: heads, Archive: archive})
	if err := g.verifyChainID(ctx); err != nil || g.heads != HeadSource(heads) || g.archive != ArchiveRPC(archive) {
		t.Fatalf("overrides: %v", err)
	}
	// A pool whose only WS and archive endpoint is disabled offers neither.
	wrong := chainIDServer(t, "0x1")
	bad := nitro.NewPool(nitro.PoolConfig{ChainID: 4663, Endpoints: []config.EndpointConfig{
		{RPCURL: primary.URL},
		{RPCURL: wrong.URL, WSURL: "ws://127.0.0.1:1/ws", Archive: true},
	}})
	h := NewFollower(Options{Network: config.NetworkConfig{ChainID: 4663}, Collector: testConfig(), RPC: bad, Store: dbtest.New()})
	if err := h.verifyChainID(ctx); err != nil || h.heads != nil || h.archive != nil {
		t.Fatalf("disabled capabilities: %v heads=%v archive=%v", err, h.heads, h.archive)
	}
}
