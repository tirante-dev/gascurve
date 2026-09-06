package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/model"
)

var now = time.Date(2026, 9, 6, 7, 20, 0, 0, time.UTC)

const (
	robinhood = uint64(4663)
	testnet   = uint64(46630)
	arbOne    = uint64(42161)
)

func seed(t *testing.T) *dbtest.MemStore {
	t.Helper()
	ctx := context.Background()
	s := dbtest.New()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", DisplayName: "Robinhood Chain", ExplorerURL: "https://x", Enabled: true}))
	must(s.UpsertNetwork(ctx, db.Network{ChainID: testnet, Name: "robinhood-testnet", DisplayName: "Testnet", Enabled: true}))
	must(s.UpsertNetwork(ctx, db.Network{ChainID: arbOne, Name: "arbitrum-one", DisplayName: "Arbitrum One", Enabled: false}))
	must(s.UpdateNetworkHead(ctx, robinhood, 1030, now.Add(-time.Second), now))
	must(s.UpdateNetworkHead(ctx, testnet, 500, now.Add(-3*time.Second), now))
	must(s.SetNetworkError(ctx, arbOne, "rpc down"))

	var blocks []db.Block
	for i := uint64(1); i <= 30; i++ {
		blocks = append(blocks, db.Block{
			ChainID: robinhood, Number: 1000 + i, TS: now.Add(-time.Duration(31-i) * time.Second), GasUsed: 1_000_000,
			BaseFee: db.WeiFromUint64(20_000_000 + i), PredictedBaseFee: db.WeiFromUint64(19_970_000), L1Block: 5, TxCount: 3,
			Backlogs: pq.Int64Array{int64(i), 100}, ExponentBips: int64(i), Anchored: i == 30,
		})
	}
	must(s.UpsertBlocks(ctx, blocks))
	must(s.UpsertBlocks(ctx, []db.Block{{ChainID: testnet, Number: 500, TS: now.Add(-3 * time.Second), GasUsed: 5, BaseFee: db.WeiFromUint64(10_000_000), PredictedBaseFee: db.WeiFromUint64(10_000_000), Backlogs: pq.Int64Array{7}, ExponentBips: 42}}))

	constraints := db.JSONB(`[{"target":60000000,"window":15,"backlog":3111506,"exponentBips":34},{"target":40000000,"window":86400,"backlog":11194391810886,"exponentBips":32391}]`)
	prices := db.JSONB(`{"perL2Tx":"1","perL1CalldataByte":"2","perL2Storage":"3","perArbGasBase":"4","perArbGasCongestion":"5","perArbGasTotal":"6"}`)
	must(s.InsertStateSample(ctx, db.StateSample{ChainID: robinhood, SampledAt: now.Add(-time.Minute), BlockNumber: 1000, BaseFee: db.WeiFromUint64(399_726_000), MinBaseFee: db.WeiFromUint64(20_000_000),
		Constraints: constraints, Prices: prices, L1: db.JSONB(`{"baseFeeEstimate":"2369608","surplus":"-1","feesAvailable":"190","unitsSinceUpdate":7,"lastUpdateAt":"2026-09-06T07:00:00Z","equilibrationUnits":160000000,"perBatchGasCharge":210000,"rewardRate":10}`),
		Accounts: db.JSONB(`{"infra":{"address":"0x1","balance":"402"},"network":{"address":"0x2","balance":"10706"},"l1Reward":{"address":"0x3","balance":"0"}}`)}))
	must(s.InsertStateSample(ctx, db.StateSample{ChainID: robinhood, SampledAt: now.Add(-15 * time.Minute), BlockNumber: 900, BaseFee: db.WeiFromUint64(1), MinBaseFee: db.WeiFromUint64(1),
		Constraints: constraints, Prices: prices, L1: db.JSONB(`{"baseFeeEstimate":"1"}`)}))
	must(s.InsertStateSample(ctx, db.StateSample{ChainID: robinhood, SampledAt: now, BlockNumber: 1030, BaseFee: db.WeiFromUint64(399_726_000), MinBaseFee: db.WeiFromUint64(20_000_000),
		Constraints: constraints, Prices: prices}))
	must(s.InsertStateSample(ctx, db.StateSample{ChainID: testnet, SampledAt: now, BlockNumber: 500, BaseFee: db.WeiFromUint64(10_000_000), MinBaseFee: db.WeiFromUint64(10_000_000),
		Constraints: db.JSONB(`[]`), Legacy: db.JSONB(`{"speedLimit":7000000,"inertia":102,"tolerance":10,"backlog":7}`), Prices: prices}))

	for _, res := range []string{db.Resolution1m, db.Resolution15m, db.Resolution1h} {
		width := db.Resolutions[res]
		for i := 0; i < 3; i++ {
			start := now.Add(-time.Duration(i+1) * width).Truncate(width)
			must(s.FoldBuckets(ctx, []db.Bucket{{ChainID: robinhood, Resolution: res, BucketStart: start, Blocks: 10, GasUsed: 100, FeesWei: db.WeiFromUint64(1000),
				BaseFeeMin: db.WeiFromUint64(1), BaseFeeAvg: db.WeiFromUint64(2), BaseFeeMax: db.WeiFromUint64(3), ExponentEndBips: 5,
				BacklogsEnd: pq.Int64Array{1, 2}, BacklogsMax: pq.Int64Array{3, 4}, ConstraintSetID: sql.NullInt64{Int64: 2, Valid: true}, ReplayErrorBips: 9, LastBlock: 10}}))
		}
	}
	_, err := s.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: robinhood, EffectiveBlock: 28, EffectiveAt: now.Add(-48 * time.Hour), Source: model.SourceGenesis, Constraints: db.JSONB(`[{"target":60000000,"window":9,"startingBacklog":0}]`)})
	must(err)
	_, err = s.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: robinhood, EffectiveBlock: 1010, EffectiveAt: now.Add(-20 * time.Second), Source: model.SourceOwnerAction, Constraints: db.JSONB(`[{"target":60000000,"window":15,"startingBacklog":0},{"target":40000000,"window":86400,"startingBacklog":9989222400000}]`)})
	must(err)
	_, err = s.InsertOwnerActions(ctx, []db.OwnerAction{
		{ChainID: robinhood, BlockNumber: 1010, TxHash: "0xaa", LogIndex: 1, TS: now.Add(-20 * time.Second), Method: "setGasPricingConstraints", Selector: "0xcc0d556a", Args: db.JSONB(`{"constraints":[]}`)},
		{ChainID: robinhood, BlockNumber: 174150, TxHash: "0xbb", LogIndex: 0, TS: now.Add(-3 * 24 * time.Hour), Method: "setMinimumL2BaseFee", Selector: "0xa0188cdb"},
	})
	must(err)
	must(s.UpsertBatchReports(ctx, []db.BatchReport{
		{ChainID: robinhood, BlockNumber: 1005, BatchNumber: 1, BatchTS: now.Add(-10 * time.Minute), Poster: "0xp", CalldataLen: 100, GasSpent: 1000, WeiSpent: db.WeiFromUint64(5000), L1BaseFee: db.WeiFromUint64(5)},
		{ChainID: robinhood, BlockNumber: 1015, BatchNumber: 2, BatchTS: now.Add(-9 * time.Minute), Poster: "0xp", CalldataLen: 50, GasSpent: 500, WeiSpent: db.WeiFromUint64(1500), L1BaseFee: db.WeiFromUint64(3)},
	}))
	must(s.SetState(ctx, robinhood, db.StateRateLimitEvents, "4"))
	must(s.SetState(ctx, robinhood, db.StateLast429At, "2026-09-06T07:00:00Z"))
	must(s.SetState(ctx, robinhood, db.StateArbOSVersion, "61"))
	must(s.SetState(ctx, robinhood, db.StateBackfillCursor, `{"done":true}`))
	return s
}

func newServer(t *testing.T, store db.Store) *httptest.Server {
	t.Helper()
	cfg := config.ServerConfig{CORSOrigins: []string{"http://localhost:3000"}, RateLimitPerSecond: 1000, RateLimitBurst: 1000}
	hub := NewHub(store, logger.Nop(), WithPingInterval(50*time.Millisecond), WithOrigins(nil))
	s := New(store, cfg, hub, logger.Nop(), WithClock(func() time.Time { return now }), WithVersion("test"))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// reply is a fully read HTTP response.
type reply struct {
	StatusCode int
	Header     http.Header
}

func get(t *testing.T, ts *httptest.Server, path string) (res reply, body []byte) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, body
}

func decode(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func TestEndpoints(t *testing.T) {
	ts := newServer(t, seed(t))
	cases := []struct {
		path   string
		status int
		cache  string
		check  func(t *testing.T, body []byte)
	}{
		{"/health", 200, cacheNone, func(t *testing.T, b []byte) {
			var m map[string]string
			decode(t, b, &m)
			if m["status"] != "ok" || m["version"] != "test" {
				t.Fatalf("health: %v", m)
			}
		}},
		{"/ready", 200, cacheNone, nil},
		{"/api/v1/health", 200, cacheNone, nil},
		{"/api/v1/ready", 200, cacheNone, nil},
		{"/api/v1/networks", 200, cacheNetwork, func(t *testing.T, b []byte) {
			var nets []model.Network
			decode(t, b, &nets)
			if len(nets) != 3 || nets[0].Name != "robinhood" || nets[0].Model != model.ModelConstraints || nets[0].HeadBlock != 1030 || nets[0].LagSeconds != 1 {
				t.Fatalf("networks: %+v", nets)
			}
			if nets[1].Name != "arbitrum-one" || nets[1].Model != model.ModelUnknown || nets[1].HeadAt != "" || nets[2].Model != model.ModelLegacy {
				t.Fatalf("networks models: %+v", nets)
			}
		}},
		{"/api/v1/networks/robinhood", 200, cacheNetwork, func(t *testing.T, b []byte) {
			var n model.Network
			decode(t, b, &n)
			if n.ChainID != robinhood || n.HeadAt != now.Add(-time.Second).Format(time.RFC3339) || n.ExplorerURL != "https://x" {
				t.Fatalf("network: %+v", n)
			}
		}},
		{"/api/v1/networks/4663", 200, cacheNetwork, nil},
		{"/api/v1/networks/nope", 404, cacheNone, func(t *testing.T, b []byte) {
			var e model.ErrorBody
			decode(t, b, &e)
			if e.Error.Code != "not_found" || !strings.Contains(e.Error.Message, "nope") {
				t.Fatalf("error: %+v", e)
			}
		}},
		{"/api/v1/networks/robinhood/live", 200, cacheNone, func(t *testing.T, b []byte) {
			var s model.LiveSnapshot
			decode(t, b, &s)
			if s.ChainID != robinhood || s.Block.Number != 1030 || s.BaseFee != "399726000" || s.MultiplierBips != 199_863 || s.ExponentBips != 32_425 {
				t.Fatalf("live: %+v", s)
			}
			if s.L1 == nil || s.L1.BaseFeeEstimate != "2369608" || s.Accounts == nil || s.Accounts.Network.Balance != "10706" || s.Legacy != nil {
				t.Fatalf("live slow data: %+v", s)
			}
			if s.GasPerSecond.S10 != 1_000_000 || s.GasPerSecond.S60 != 500_000 || s.ReplayErrorBips != 15 || s.Prices.PerArbGasTotal != "6" {
				t.Fatalf("live gps/error: %+v", s)
			}
		}},
		{"/api/v1/networks/robinhood-testnet/live", 200, cacheNone, func(t *testing.T, b []byte) {
			var s model.LiveSnapshot
			decode(t, b, &s)
			if s.Model != model.ModelLegacy || s.Legacy == nil || s.Legacy.Backlog != 7 || s.ExponentBips != 42 || len(s.Constraints) != 0 || s.L1 != nil {
				t.Fatalf("legacy live: %+v", s)
			}
			if !strings.Contains(string(b), `"constraints":[]`) {
				t.Fatalf("constraints must be an empty array: %s", b)
			}
		}},
		{"/api/v1/networks/arbitrum-one/live", 404, cacheNone, nil},
		{"/api/v1/networks/robinhood/blocks?limit=5", 200, cacheShort, func(t *testing.T, b []byte) {
			var pts []model.BlockPoint
			decode(t, b, &pts)
			if len(pts) != 5 || pts[0].Number != 1030 || !pts[0].Anchored || pts[0].Backlogs[0] != 30 || pts[4].Number != 1026 {
				t.Fatalf("blocks: %+v", pts)
			}
		}},
		{"/api/v1/networks/robinhood/blocks?limit=abc", 200, cacheShort, func(t *testing.T, b []byte) {
			var pts []model.BlockPoint
			decode(t, b, &pts)
			if len(pts) != 30 {
				t.Fatalf("default limit: %d", len(pts))
			}
		}},
		{"/api/v1/networks/robinhood/blocks?limit=0", 200, cacheShort, func(t *testing.T, b []byte) {
			var pts []model.BlockPoint
			decode(t, b, &pts)
			if len(pts) != 1 {
				t.Fatalf("clamped limit: %d", len(pts))
			}
		}},
		{"/api/v1/networks/robinhood/series", 200, cacheHour, func(t *testing.T, b []byte) {
			var s model.Series
			decode(t, b, &s)
			if s.Range != "1h" || s.Resolution != "block" || len(s.Points) != 30 || s.Points[0].T != now.Add(-30*time.Second).Unix() {
				t.Fatalf("series 1h: %+v", s)
			}
			p := s.Points[29]
			if p.GasPerSecond != 1_000_000 || p.FeesWei != "20000030000000" || p.ExponentBips != 30 || p.ConstraintSetID != 2 || p.ReplayErrorBips != 15 || p.BacklogsMax[1] != 100 {
				t.Fatalf("series point: %+v", p)
			}
			if s.Points[5].ConstraintSetID != 1 {
				t.Fatalf("set before block 1010: %+v", s.Points[5])
			}
			if len(s.ConstraintSets) != 2 || s.ConstraintSets[0].Source != model.SourceGenesis || len(s.OwnerActions) != 1 || s.OwnerActions[0].TxHash != "0xaa" {
				t.Fatalf("series sets/actions: %+v %+v", s.ConstraintSets, s.OwnerActions)
			}
		}},
		{"/api/v1/networks/robinhood/series?range=24h", 200, cacheDay, func(t *testing.T, b []byte) {
			var s model.Series
			decode(t, b, &s)
			if s.Resolution != "1m" || len(s.Points) != 3 || s.Points[0].Blocks != 10 || s.Points[0].GasPerSecond != 1 || s.Points[0].ConstraintSetID != 2 || s.Points[0].BaseFeeAvg != "2" {
				t.Fatalf("series 24h: %+v", s)
			}
		}},
		{"/api/v1/networks/robinhood/series?range=30d", 200, cacheMonth, func(t *testing.T, b []byte) {
			var s model.Series
			decode(t, b, &s)
			if s.Resolution != "15m" || len(s.Points) != 3 || len(s.OwnerActions) != 2 {
				t.Fatalf("series 30d: %+v", s)
			}
		}},
		{"/api/v1/networks/robinhood/series?range=all", 200, cacheAll, func(t *testing.T, b []byte) {
			var s model.Series
			decode(t, b, &s)
			if s.Resolution != "1h" || len(s.Points) != 3 || len(s.ConstraintSets) != 2 {
				t.Fatalf("series all: %+v", s)
			}
		}},
		{"/api/v1/networks/robinhood/series?range=2y", 400, cacheNone, nil},
		{"/api/v1/networks/robinhood-testnet/series", 200, cacheHour, func(t *testing.T, b []byte) {
			var s model.Series
			decode(t, b, &s)
			if len(s.ConstraintSets) != 0 || len(s.Points) != 1 || s.Points[0].ConstraintSetID != 0 {
				t.Fatalf("testnet series: %+v", s)
			}
			if !strings.Contains(string(b), `"constraintSets":[]`) || !strings.Contains(string(b), `"ownerActions":[]`) {
				t.Fatalf("empty arrays expected: %s", b)
			}
		}},
		{"/api/v1/networks/robinhood/constraints", 200, cacheDay, func(t *testing.T, b []byte) {
			var out struct {
				Current *model.ConstraintSet  `json:"current"`
				History []model.ConstraintSet `json:"history"`
			}
			decode(t, b, &out)
			if out.Current == nil || out.Current.ID != 2 || len(out.History) != 2 || out.History[1].Source != model.SourceGenesis || len(out.Current.Constraints) != 2 || out.Current.Constraints[1].StartingBacklog != 9_989_222_400_000 {
				t.Fatalf("constraints: %+v", out)
			}
		}},
		{"/api/v1/networks/robinhood-testnet/constraints", 200, cacheDay, func(t *testing.T, b []byte) {
			if !strings.Contains(string(b), `"current":null`) || !strings.Contains(string(b), `"history":[]`) {
				t.Fatalf("empty constraints: %s", b)
			}
		}},
		{"/api/v1/networks/robinhood/owner-actions?limit=1", 200, cacheDay, func(t *testing.T, b []byte) {
			var acts []model.OwnerAction
			decode(t, b, &acts)
			if len(acts) != 1 || acts[0].Block != 174150 || acts[0].Method != "setMinimumL2BaseFee" || string(acts[0].Args) != "{}" {
				t.Fatalf("owner actions: %+v", acts)
			}
		}},
		{"/api/v1/networks/robinhood/batches?range=24h", 200, cacheDay, func(t *testing.T, b []byte) {
			var s model.BatchSeries
			decode(t, b, &s)
			if s.Range != "24h" || s.Resolution != "1m" || len(s.Points) != 2 || s.Points[0].Batches != 1 || s.Points[0].WeiSpent != "5000" || s.Points[0].L1BaseFeeAvg != "5" || s.Points[0].CalldataBytes != 100 {
				t.Fatalf("batches: %+v", s)
			}
		}},
		{"/api/v1/networks/robinhood/batches?range=1h", 200, cacheHour, func(t *testing.T, b []byte) {
			var s model.BatchSeries
			decode(t, b, &s)
			if s.Resolution != "batch" || len(s.Points) != 2 {
				t.Fatalf("batches 1h: %+v", s)
			}
		}},
		{"/api/v1/networks/robinhood/batches?range=x", 400, cacheNone, nil},
		{"/api/v1/networks/robinhood/l1?range=24h", 200, cacheDay, func(t *testing.T, b []byte) {
			var s model.L1Series
			decode(t, b, &s)
			if s.Range != "24h" || len(s.Points) != 2 || s.Points[1].BaseFeeEstimate != "2369608" || s.Points[1].UnitsSinceUpdate != 7 || s.Points[1].Surplus != "-1" {
				t.Fatalf("l1: %+v", s)
			}
		}},
		{"/api/v1/networks/robinhood/l1?range=all", 200, cacheAll, func(t *testing.T, b []byte) {
			var s model.L1Series
			decode(t, b, &s)
			if len(s.Points) != 1 {
				t.Fatalf("l1 hourly: %+v", s)
			}
		}},
		{"/api/v1/networks/robinhood/l1?range=x", 400, cacheNone, nil},
		{"/api/v1/status", 200, cacheNone, func(t *testing.T, b []byte) {
			var s model.Status
			decode(t, b, &s)
			if s.Version != "test" || len(s.Networks) != 3 {
				t.Fatalf("status: %+v", s)
			}
			rh := s.Networks[0]
			if rh.RateLimitEvents != 4 || rh.Last429At == nil || *rh.Last429At != "2026-09-06T07:00:00Z" || rh.ArbOSVersion == nil || rh.BackfillCursor == nil || rh.LagSeconds != 1 || rh.LastSampleAt == "" {
				t.Fatalf("status robinhood: %+v", rh)
			}
			if arb := s.Networks[1]; arb.LastError == nil || *arb.LastError != "rpc down" || arb.Enabled || arb.Last429At != nil {
				t.Fatalf("status arbitrum: %+v", arb)
			}
		}},
		{"/api/v1/nothing", 404, cacheNone, nil},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, body := get(t, ts, tc.path)
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tc.status, body)
			}
			if cc := resp.Header.Get("Cache-Control"); cc != tc.cache {
				t.Fatalf("cache-control %q, want %q", cc, tc.cache)
			}
			if resp.Header.Get("X-Request-Id") == "" {
				t.Fatal("missing request id header")
			}
			if tc.check != nil {
				tc.check(t, body)
			}
		})
	}
}

func TestMethodNotAllowedAndCORS(t *testing.T) {
	ts := newServer(t, seed(t))
	resp, err := http.Post(ts.URL+"/api/v1/networks", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/networks", http.NoBody)
	req.Header.Set("Origin", "http://localhost:3000")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatalf("cors header = %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestSeriesStepDown(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood"}); err != nil {
		t.Fatal(err)
	}
	var blocks []db.Block
	for i := uint64(0); i < 2500; i++ {
		blocks = append(blocks, db.Block{ChainID: robinhood, Number: i, TS: now.Add(-time.Duration(2500-i) * 100 * time.Millisecond).Truncate(time.Second), GasUsed: 10,
			BaseFee: db.WeiFromUint64(100 + i%3), PredictedBaseFee: db.WeiFromUint64(100), Backlogs: pq.Int64Array{int64(i % 5), 9}, ExponentBips: int64(i)})
	}
	if err := store.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	ts := newServer(t, store)
	resp, body := get(t, ts, "/api/v1/networks/robinhood/series?range=1h")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var s model.Series
	decode(t, body, &s)
	if s.Resolution != "5s" || len(s.Points) < 49 || len(s.Points) > 51 {
		t.Fatalf("step down: %s %d", s.Resolution, len(s.Points))
	}
	p := s.Points[1]
	if p.Blocks != 50 || p.GasUsed != 500 || p.GasPerSecond != 100 || p.BaseFeeMin != "100" || p.BaseFeeMax != "102" || p.BacklogsMax[0] != 4 || p.T%5 != 0 {
		t.Fatalf("stepped point: %+v", p)
	}
	if p.ReplayErrorBips == 0 || p.BaseFeeAvg == "" || len(p.Backlogs) != 2 {
		t.Fatalf("stepped point aggregates: %+v", p)
	}
}

func TestStoreFailures(t *testing.T) {
	paths := []string{
		"/ready", "/api/v1/networks", "/api/v1/networks/robinhood", "/api/v1/networks/robinhood/live", "/api/v1/networks/robinhood/blocks",
		"/api/v1/networks/robinhood/series", "/api/v1/networks/robinhood/series?range=24h", "/api/v1/networks/robinhood/constraints",
		"/api/v1/networks/robinhood/owner-actions", "/api/v1/networks/robinhood/batches", "/api/v1/networks/robinhood/l1", "/api/v1/status",
	}
	methods := []string{"Networks", "NetworkByRef", "LatestStateSample", "RecentBlocks", "BlocksBetween", "Buckets", "ConstraintSets", "OwnerActions", "BatchBuckets", "L1Samples", "States", "LatestBlock", "GasUsedBetween"}
	for _, m := range methods {
		store := seed(t)
		store.SetFailure(m, true)
		store.PingErr = dbtest.ErrInjected
		ts := newServer(t, store)
		failed := 0
		for _, p := range paths {
			resp, _ := get(t, ts, p)
			if resp.StatusCode >= 500 {
				failed++
			}
		}
		if failed == 0 {
			t.Errorf("%s: no endpoint reported a failure", m)
		}
		ts.Close()
	}
	// Decode failures in stored JSON are internal errors too.
	store := seed(t)
	ctx := context.Background()
	for _, sample := range []db.StateSample{
		{ChainID: robinhood, SampledAt: now.Add(time.Second), Constraints: db.JSONB(`{bad`), Prices: db.JSONB(`{}`)},
		{ChainID: robinhood, SampledAt: now.Add(2 * time.Second), Constraints: db.JSONB(`[]`), Legacy: db.JSONB(`{bad`), Prices: db.JSONB(`{}`)},
		{ChainID: robinhood, SampledAt: now.Add(3 * time.Second), Constraints: db.JSONB(`[]`), Prices: db.JSONB(`{bad`)},
		{ChainID: robinhood, SampledAt: now.Add(4 * time.Second), Constraints: db.JSONB(`[]`), Prices: db.JSONB(`{}`), L1: db.JSONB(`{bad`)},
		{ChainID: robinhood, SampledAt: now.Add(5 * time.Second), Constraints: db.JSONB(`[]`), Prices: db.JSONB(`{}`), L1: db.JSONB(`{}`), Accounts: db.JSONB(`{bad`)},
	} {
		if err := store.InsertStateSample(ctx, sample); err != nil {
			t.Fatal(err)
		}
		ts := newServer(t, store)
		if resp, _ := get(t, ts, "/api/v1/networks/robinhood/live"); resp.StatusCode != 500 {
			t.Fatalf("expected 500 for %s, got %d", sample.SampledAt, resp.StatusCode)
		}
		ts.Close()
	}
	// A broken constraint set document breaks /constraints and /series.
	store = seed(t)
	if _, err := store.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: robinhood, EffectiveBlock: 5, EffectiveAt: now, Source: "x", Constraints: db.JSONB(`{bad`)}); err != nil {
		t.Fatal(err)
	}
	ts := newServer(t, store)
	if resp, _ := get(t, ts, "/api/v1/networks/robinhood/constraints"); resp.StatusCode != 500 {
		t.Fatalf("constraints: %d", resp.StatusCode)
	}
	if resp, _ := get(t, ts, "/api/v1/networks/robinhood/series?range=all"); resp.StatusCode != 500 {
		t.Fatalf("series: %d", resp.StatusCode)
	}
	// Unreadable L1 documents are skipped in /l1.
	store = seed(t)
	if err := store.InsertStateSample(ctx, db.StateSample{ChainID: robinhood, SampledAt: now.Add(-2 * time.Minute), Constraints: db.JSONB(`[]`), Prices: db.JSONB(`{}`), L1: db.JSONB(`{bad`)}); err != nil {
		t.Fatal(err)
	}
	ts = newServer(t, store)
	resp, body := get(t, ts, "/api/v1/networks/robinhood/l1?range=1h")
	var l1 model.L1Series
	decode(t, body, &l1)
	if resp.StatusCode != 200 || len(l1.Points) != 2 {
		t.Fatalf("l1 skip: %d %+v", resp.StatusCode, l1)
	}
	// Live without a block row falls back to the sample's block number.
	store = seed(t)
	store.BlockRows = map[uint64]map[uint64]db.Block{}
	ts = newServer(t, store)
	resp, body = get(t, ts, "/api/v1/networks/robinhood/live")
	var snap model.LiveSnapshot
	decode(t, body, &snap)
	if resp.StatusCode != 200 || snap.Block.Number != 1030 || snap.GasPerSecond.S10 != 0 {
		t.Fatalf("live without blocks: %d %+v", resp.StatusCode, snap)
	}
}

func TestRateLimit(t *testing.T) {
	store := seed(t)
	cfg := config.ServerConfig{RateLimitPerSecond: 1, RateLimitBurst: 2}
	s := New(store, cfg, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	// Without a hub the websocket route does not exist.
	if resp, _ := get(t, ts, "/api/v1/ws?network=robinhood"); resp.StatusCode != 404 {
		t.Fatalf("ws without hub: %d", resp.StatusCode)
	}
	codes := make([]int, 0, 2)
	for range 2 {
		resp, _ := get(t, ts, "/health")
		codes = append(codes, resp.StatusCode)
	}
	if codes[0] != 200 || codes[1] != 429 {
		t.Fatalf("codes = %v", codes)
	}
	// Defaults apply when the config is zero.
	s2 := New(store, config.ServerConfig{}, nil, nil)
	if s2.Handler() == nil {
		t.Fatal("handler")
	}
	rl := newRateLimiter(1, 1)
	rl.now = func() time.Time { return now }
	if !rl.allow("a") || rl.allow("a") {
		t.Fatal("limiter")
	}
	for i := range maxRateEntries {
		rl.clients[strings.Repeat("x", 1)+string(rune(i))] = &rateEntry{limiter: nil, seen: now.Add(-time.Hour)}
	}
	if !rl.allow("fresh") || len(rl.clients) > 2 {
		t.Fatalf("stale entries should be evicted: %d", len(rl.clients))
	}
	// IPv6 remote addresses keep the whole address.
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	req.RemoteAddr = "[::1]:1234"
	rec := httptest.NewRecorder()
	rl.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(rec, req)
	if rec.Code != 204 || rl.clients["[::1]"] == nil {
		t.Fatalf("ipv6 key: %d %v", rec.Code, rl.clients)
	}
}

func TestHelpers(t *testing.T) {
	if got := originPatterns([]string{"http://localhost:3000", "*", "x"}); len(got) != 1 || got[0] != "*" {
		t.Fatalf("wildcard: %v", got)
	}
	if got := originPatterns([]string{"http://localhost:3000", "bad host"}); len(got) != 2 || got[0] != "localhost:3000" || got[1] != "bad host" {
		t.Fatalf("patterns: %v", got)
	}
	if setIDAt(nil, 5) != 0 {
		t.Fatal("setIDAt")
	}
	sets := []db.ConstraintSet{{ID: 1, EffectiveAt: now.Add(-2 * time.Hour)}, {ID: 2, EffectiveAt: now.Add(-time.Hour)}, {ID: 3, EffectiveAt: now}}
	if got := setsInForce(sets, now.Add(-90*time.Minute)); len(got) != 3 || got[0].ID != 1 {
		t.Fatalf("in force: %+v", got)
	}
	if got := setsInForce(sets, now.Add(time.Hour)); len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("latest only: %+v", got)
	}
	if got := setsInForce(sets, now.Add(-3*time.Hour)); len(got) != 3 {
		t.Fatalf("all: %+v", got)
	}
	if multiplierBips(bigInt(30), bigInt(0)) != 0 || multiplierBips(bigInt(30), bigInt(10)) != 30_000 {
		t.Fatal("multiplierBips")
	}
	if truncate(strings.Repeat("a", 200)) != strings.Repeat("a", 120)+"..." || truncate("b") != "b" {
		t.Fatal("truncate")
	}
	if string(mustJSON(make(chan int))) != "null" {
		t.Fatal("mustJSON")
	}
	if sampleModel(&db.StateSample{Constraints: db.JSONB(`{bad`)}) != model.ModelUnknown {
		t.Fatal("sampleModel")
	}
	if intParam(httptest.NewRequest(http.MethodGet, "/?limit=5000", http.NoBody), "limit", 1, 1, 10) != 10 {
		t.Fatal("intParam clamp")
	}
	if b := (&acc{blocks: 1, sum: bigInt(0), fees: bigInt(0), minFee: bigInt(1), maxFee: bigInt(1)}); b.point(5).BacklogsMax == nil {
		t.Fatal("point")
	}
}
