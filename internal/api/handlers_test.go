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
			Backlogs: db.Uint64Array{i, 100}, ConstraintBips: pq.Int64Array{int64(i), 0}, MinBaseFee: db.NullWeiFromUint64(20_000_000), ExponentBips: int64(i), Anchored: i == 30, PricingVersion: db.PricingFull,
		})
	}
	must(s.UpsertBlocks(ctx, blocks))
	must(s.UpsertBlocks(ctx, []db.Block{{ChainID: testnet, Number: 500, TS: now.Add(-3 * time.Second), GasUsed: 5, BaseFee: db.WeiFromUint64(10_000_000), PredictedBaseFee: db.WeiFromUint64(10_000_000), Backlogs: db.Uint64Array{7}, ExponentBips: 42}}))

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
		Constraints: db.JSONB(`[]`), Legacy: db.JSONB(`{"speedLimit":7000000,"inertia":102,"tolerance":10,"backlog":80000000}`), Prices: prices}))

	for _, res := range []string{db.Resolution1m, db.Resolution15m, db.Resolution1h} {
		width := db.Resolutions[res]
		for i := 0; i < 3; i++ {
			start := now.Add(-time.Duration(i+1) * width).Truncate(width)
			must(s.FoldBuckets(ctx, []db.Bucket{{ChainID: robinhood, Resolution: res, BucketStart: start, Blocks: 10, GasUsed: 100, FeesWei: db.WeiFromUint64(1000),
				BaseFeeMin: db.WeiFromUint64(1), BaseFeeAvg: db.WeiFromUint64(2), BaseFeeMax: db.WeiFromUint64(3), BaseFeeSum: db.NullWeiFromUint64(25), ExponentEndBips: 5,
				BacklogsEnd: db.Uint64Array{1, 2}, BacklogsMax: db.Uint64Array{3, 4}, ConstraintBipsEnd: pq.Int64Array{5, 0}, MinBaseFee: db.NullWeiFromUint64(7), PricingVersion: db.PricingFull,
				FloorFeesWei: db.NullWeiFromUint64(700), SurplusFeesWei: db.NullWeiFromUint64(300), ConstraintSetID: sql.NullInt64{Int64: 2, Valid: true}, ReplayErrorBips: 9, LastBlock: 10}}))
		}
	}
	_, err := s.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: robinhood, EffectiveBlock: 28, EffectiveAt: now.Add(-48 * time.Hour), Source: model.SourceGenesis, Constraints: db.JSONB(`[{"target":60000000,"window":9,"startingBacklog":0},{"target":20000000,"window":86400,"startingBacklog":0}]`)})
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
		{ChainID: robinhood, BlockNumber: 1016, BatchNumber: 3, BatchTS: now.Add(-9 * time.Minute), Poster: "0xp", CalldataLen: 8, GasSpent: 7, WeiSpent: db.WeiFromUint64(77), L1BaseFee: db.WeiFromUint64(9)},
	}))
	must(s.SetState(ctx, robinhood, db.StateRateLimitEvents, "4"))
	must(s.SetState(ctx, robinhood, db.StateLast429At, "2026-09-06T07:00:00Z"))
	must(s.SetState(ctx, robinhood, db.StateArbOSVersion, "61"))
	must(s.SetState(ctx, robinhood, db.StateBackfillCursor, `{"done":true}`))
	must(s.SetState(ctx, robinhood, db.StateEndpoints, `{"activeEndpoint":1,"failovers":3,"endpoints":[{"index":0,"ws":false,"archive":false,"disabled":true,"error":"reports chain id 1, configured 4663"},{"index":1,"ws":true,"archive":true,"disabled":false,"error":null}]}`))
	must(s.SetState(ctx, testnet, db.StateEndpoints, `not json`))
	// The ETH/USD spot the collector recorded: fresh for robinhood, older
	// than the default max age for the testnet.
	must(s.SetState(ctx, robinhood, db.StateEthUsd, `{"price":"4523.40","at":"2026-09-06T07:18:00Z","source":"coinbase"}`))
	must(s.SetState(ctx, testnet, db.StateEthUsd, `{"price":"4523.40","at":"2026-09-06T07:09:00Z","source":"coinbase"}`))
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
			if len(nets) != 3 || nets[0].Name != "robinhood" || nets[0].Model != model.ModelConstraints || nets[0].HeadBlock != 1030 || nets[0].LagSeconds == nil || *nets[0].LagSeconds != 1 {
				t.Fatalf("networks: %+v", nets)
			}
			if nets[1].Name != "arbitrum-one" || nets[1].Model != model.ModelUnknown || nets[1].HeadAt != nil || nets[1].LagSeconds != nil || nets[2].Model != model.ModelLegacy {
				t.Fatalf("networks models: %+v", nets)
			}
			// Timestamps of a network without a head are JSON null, never "".
			if !strings.Contains(string(b), `"headAt":null`) || !strings.Contains(string(b), `"lagSeconds":null`) {
				t.Fatalf("null timestamps expected: %s", b)
			}
		}},
		{"/api/v1/networks/robinhood", 200, cacheNetwork, func(t *testing.T, b []byte) {
			var n model.Network
			decode(t, b, &n)
			if n.ChainID != robinhood || n.HeadAt == nil || *n.HeadAt != now.Add(-time.Second).Format(time.RFC3339) || n.ExplorerURL != "https://x" {
				t.Fatalf("network: %+v", n)
			}
		}},
		{"/api/v1/networks/4663", 200, cacheNetwork, nil},
		// A decimal beyond BIGINT is looked up as a name and is unknown,
		// not an internal error.
		{"/api/v1/networks/9223372036854775808", 404, cacheNone, nil},
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
			if s.EthUsd == nil || s.EthUsd.Price != "4523.40" || s.EthUsd.Source != "coinbase" || s.EthUsd.At != "2026-09-06T07:18:00Z" {
				t.Fatalf("live eth/usd: %+v", s.EthUsd)
			}
		}},
		{"/api/v1/networks/robinhood-testnet/live", 200, cacheNone, func(t *testing.T, b []byte) {
			var s model.LiveSnapshot
			decode(t, b, &s)
			// The exponent is what the sampled legacy backlog yields (the
			// collector's tick value), not the block row's replay exponent.
			if s.Model != model.ModelLegacy || s.Legacy == nil || s.Legacy.Backlog != 80_000_000 || s.ExponentBips != legacyExponent(s.Legacy) || s.ExponentBips <= 0 || len(s.Constraints) != 0 || s.L1 != nil {
				t.Fatalf("legacy live: %+v", s)
			}
			if !strings.Contains(string(b), `"constraints":[]`) {
				t.Fatalf("constraints must be an empty array: %s", b)
			}
			// A quote older than eth_usd_max_age is null, not stale data.
			if s.EthUsd != nil || !strings.Contains(string(b), `"ethUsd":null`) {
				t.Fatalf("a stale spot must be null: %s", b)
			}
		}},
		{"/api/v1/networks/arbitrum-one/live", 404, cacheNone, nil},
		{"/api/v1/networks/robinhood/blocks?limit=5", 200, cacheShort, func(t *testing.T, b []byte) {
			var pts []model.BlockPoint
			decode(t, b, &pts)
			if len(pts) != 5 || pts[0].Number != 1030 || !pts[0].Anchored || pts[0].Backlogs[0] != 30 || pts[4].Number != 1026 {
				t.Fatalf("blocks: %+v", pts)
			}
			if pts[0].ConstraintBips[0] != 30 || len(pts[0].ConstraintBips) != 2 || *pts[0].MinBaseFee != "20000000" {
				t.Fatalf("block point contract fields: %+v", pts[0])
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
			if *p.MinBaseFee != "20000000" || *p.FloorFeesWei != "20000000000000" || *p.SurplusFeesWei != "30000000" || p.ConstraintBips[0] != 30 {
				t.Fatalf("series point fee split: %+v", p)
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
			// The average derives from the exact sum (25 / 10), not the
			// stored rounded average.
			if s.Resolution != "1m" || len(s.Points) != 3 || s.Points[0].Blocks != 10 || s.Points[0].GasPerSecond != 1 || s.Points[0].ConstraintSetID != 2 || s.Points[0].BaseFeeAvg != "2" {
				t.Fatalf("series 24h: %+v", s)
			}
			// The window is reported whatever the points cover, so a chart
			// draws the whole day and shows the rest as not indexed.
			if s.From != now.Add(-24*time.Hour).Unix() || s.To != now.Add(time.Second).Unix() {
				t.Fatalf("series 24h window: %d..%d", s.From, s.To)
			}
			if p := s.Points[0]; *p.MinBaseFee != "7" || *p.FloorFeesWei != "700" || *p.SurplusFeesWei != "300" || p.ConstraintBips[0] != 5 {
				t.Fatalf("bucket point contract fields: %+v", p)
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
			// The all range starts where the data does, never in 1970.
			if s.From != s.Points[0].T || s.To != now.Add(time.Second).Unix() {
				t.Fatalf("series all window: %d..%d (first point %d)", s.From, s.To, s.Points[0].T)
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
			if s.Range != "24h" || s.Resolution != "1m" || len(s.Points) != 2 || s.Points[0].Batches != 1 || s.Points[0].WeiSpent != "5000" || s.Points[0].L1BaseFeeAvg != "5" || s.Points[0].CalldataBytes != 100 || s.Points[1].Batches != 2 {
				t.Fatalf("batches: %+v", s)
			}
			if s.From != now.Add(-24*time.Hour).Unix() || s.To != now.Add(time.Second).Unix() {
				t.Fatalf("batches window: %d..%d", s.From, s.To)
			}
		}},
		{"/api/v1/networks/robinhood/batches?range=1h", 200, cacheHour, func(t *testing.T, b []byte) {
			var s model.BatchSeries
			decode(t, b, &s)
			// Exactly one point per report, even for reports sharing a
			// second, ordered by time then block.
			if s.Resolution != "batch" || len(s.Points) != 3 || s.Points[0].Batches != 1 || s.Points[1].Batches != 1 || s.Points[2].Batches != 1 {
				t.Fatalf("batches 1h: %+v", s)
			}
			if s.Points[1].T != s.Points[2].T || s.Points[1].WeiSpent != "1500" || s.Points[2].WeiSpent != "77" || s.Points[2].L1BaseFeeAvg != "9" || s.Points[2].CalldataBytes != 8 {
				t.Fatalf("batches per report: %+v", s.Points)
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
			if len(s.Points) != 1 || s.From != s.Points[0].T || s.To != now.Add(time.Second).Unix() {
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
			if rh.RateLimitEvents != 4 || rh.Last429At == nil || *rh.Last429At != "2026-09-06T07:00:00Z" || rh.ArbOSVersion == nil || rh.BackfillCursor == nil || rh.LagSeconds == nil || *rh.LagSeconds != 1 || rh.LastSampleAt == nil {
				t.Fatalf("status robinhood: %+v", rh)
			}
			if arb := s.Networks[1]; arb.LastError == nil || *arb.LastError != "rpc down" || arb.Enabled || arb.Last429At != nil || arb.HeadAt != nil || arb.LagSeconds != nil || arb.LastSampleAt != nil {
				t.Fatalf("status arbitrum: %+v", arb)
			}
			if !strings.Contains(string(b), `"lastSampleAt":null`) {
				t.Fatalf("null lastSampleAt expected: %s", b)
			}
			if rh.ActiveEndpoint != 1 || rh.Failovers != 3 || len(rh.Endpoints) != 2 || !rh.Endpoints[0].Disabled || rh.Endpoints[0].Index != 0 || !rh.Endpoints[1].WS || !rh.Endpoints[1].Archive {
				t.Fatalf("status endpoints: %+v", rh.EndpointsStatus)
			}
			// A disabled endpoint carries why, sanitized to chain ids: no
			// URL, no credential. A usable one carries null.
			if rh.Endpoints[0].Error == nil || *rh.Endpoints[0].Error != "reports chain id 1, configured 4663" || rh.Endpoints[1].Error != nil {
				t.Fatalf("endpoint error: %+v", rh.Endpoints)
			}
			if !strings.Contains(string(b), `"error":null`) {
				t.Fatalf("a usable endpoint reports a null error: %s", b)
			}
			// Networks without (or with an unreadable) routing state report
			// the primary alone and an empty, never null, endpoint list.
			for _, n := range s.Networks[1:] {
				if n.ActiveEndpoint != 0 || n.Failovers != 0 || n.Endpoints == nil || len(n.Endpoints) != 0 {
					t.Fatalf("status endpoints default: %+v", n.EndpointsStatus)
				}
			}
			if strings.Count(string(b), `"endpoints":[]`) != 2 || strings.Contains(string(b), "rpc_url") {
				t.Fatalf("endpoints json: %s", b)
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
			BaseFee: db.WeiFromUint64(100 + i%3), PredictedBaseFee: db.WeiFromUint64(100), Backlogs: db.Uint64Array{i % 5, 9}, ConstraintBips: pq.Int64Array{int64(i), 0}, MinBaseFee: db.NullWeiFromUint64(50), ExponentBips: int64(i), PricingVersion: db.PricingFull})
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
	if p.ReplayErrorBips == 0 || p.BaseFeeAvg == "" || len(p.Backlogs) != 2 || *p.MinBaseFee != "50" || *p.FloorFeesWei != "25000" || p.SurplusFeesWei == nil {
		t.Fatalf("stepped point aggregates: %+v", p)
	}
}

// TestUnknownHistoryIsNull: buckets and blocks written before the fee
// split and the exponents were recorded (NULL after migration 000006)
// report them as null, never as fabricated zeros or empty arrays; the
// average of such a bucket comes from its stored value.
func TestUnknownHistoryIsNull(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood"}); err != nil {
		t.Fatal(err)
	}
	start := now.Add(-2 * time.Minute).Truncate(time.Minute)
	if err := store.FoldBuckets(ctx, []db.Bucket{{ChainID: robinhood, Resolution: db.Resolution1m, BucketStart: start, Blocks: 3, GasUsed: 30, FeesWei: db.WeiFromUint64(90),
		BaseFeeMin: db.WeiFromUint64(2), BaseFeeAvg: db.WeiFromUint64(3), BaseFeeMax: db.WeiFromUint64(4), BacklogsEnd: db.Uint64Array{1}, BacklogsMax: db.Uint64Array{1}, LastBlock: 3}}); err != nil {
		t.Fatal(err)
	}
	// Two old blocks (no exponents) and one written after the migration.
	if err := store.UpsertBlocks(ctx, []db.Block{
		{ChainID: robinhood, Number: 1, TS: now.Add(-30 * time.Second), GasUsed: 10, BaseFee: db.WeiFromUint64(5), PredictedBaseFee: db.WeiFromUint64(5), Backlogs: db.Uint64Array{1}},
		{ChainID: robinhood, Number: 2, TS: now.Add(-29 * time.Second), GasUsed: 10, BaseFee: db.WeiFromUint64(5), PredictedBaseFee: db.WeiFromUint64(5), Backlogs: db.Uint64Array{1}},
		{ChainID: robinhood, Number: 3, TS: now.Add(-20 * time.Second), GasUsed: 10, BaseFee: db.WeiFromUint64(6), PredictedBaseFee: db.WeiFromUint64(6), Backlogs: db.Uint64Array{1}, ConstraintBips: pq.Int64Array{}, MinBaseFee: db.NullWeiFromUint64(2), PricingVersion: db.PricingFull},
	}); err != nil {
		t.Fatal(err)
	}
	ts := newServer(t, store)
	_, body := get(t, ts, "/api/v1/networks/robinhood/series?range=24h")
	var s model.Series
	decode(t, body, &s)
	if len(s.Points) != 1 || s.Points[0].BaseFeeAvg != "3" || s.Points[0].FloorFeesWei != nil || s.Points[0].SurplusFeesWei != nil || s.Points[0].ConstraintBips != nil {
		t.Fatalf("unknown bucket fields: %+v", s.Points)
	}
	if !strings.Contains(string(body), `"floorFeesWei":null`) || !strings.Contains(string(body), `"constraintBips":null`) {
		t.Fatalf("null expected in json: %s", body)
	}
	_, body = get(t, ts, "/api/v1/networks/robinhood/series?range=1h")
	decode(t, body, &s)
	if len(s.Points) != 3 || s.Points[0].FloorFeesWei != nil || s.Points[0].ConstraintBips != nil || s.Points[2].FloorFeesWei == nil || *s.Points[2].FloorFeesWei != "20" || len(s.Points[2].ConstraintBips) != 0 {
		t.Fatalf("per-block unknown fields: %+v", s.Points)
	}
	if !strings.Contains(string(body), `"constraintBips":[]`) {
		t.Fatalf("a recorded empty array stays an array: %s", body)
	}
	_, body = get(t, ts, "/api/v1/networks/robinhood/blocks")
	var pts []model.BlockPoint
	decode(t, body, &pts)
	if len(pts) != 3 || pts[0].ConstraintBips == nil || pts[2].ConstraintBips != nil {
		t.Fatalf("block points: %+v", pts)
	}
	// Stepping down folds unknown blocks into an unknown split.
	if p := stepDown([]db.Block{{TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.WeiFromUint64(1), ConstraintBips: pq.Int64Array{}}, {TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.WeiFromUint64(1)}}, nil, 5*time.Second); len(p) != 1 || p[0].FloorFeesWei != nil {
		t.Fatalf("step down with an unknown block: %+v", p)
	}
}

// TestLiveEthUsd: /live applies the collector's staleness rule to the
// recorded spot, serves null when there is none and refuses to invent one
// from a row it cannot read.
func TestLiveEthUsd(t *testing.T) {
	ctx := context.Background()
	liveSpot := func(t *testing.T, ts *httptest.Server) (*model.EthUsd, int) {
		t.Helper()
		resp, body := get(t, ts, "/api/v1/networks/robinhood/live")
		if resp.StatusCode != 200 {
			return nil, resp.StatusCode
		}
		var snap model.LiveSnapshot
		decode(t, body, &snap)
		return snap.EthUsd, resp.StatusCode
	}
	// Exactly at the default cutoff the quote is still live; a second past
	// it, it is null.
	for _, tc := range []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"at the cutoff", config.DefaultEthUsdMaxAge, true},
		{"past the cutoff", config.DefaultEthUsdMaxAge + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := seed(t)
			row := `{"price":"4523.40","at":"` + now.Add(-tc.age).Format(time.RFC3339) + `","source":"coinbase"}`
			if err := store.SetState(ctx, robinhood, db.StateEthUsd, row); err != nil {
				t.Fatal(err)
			}
			spot, code := liveSpot(t, newServer(t, store))
			if code != 200 || (spot != nil) != tc.want {
				t.Fatalf("%d %+v", code, spot)
			}
		})
	}
	// No row at all: null, not an error.
	store := seed(t)
	if err := store.DeleteState(ctx, robinhood, db.StateEthUsd); err != nil {
		t.Fatal(err)
	}
	if spot, code := liveSpot(t, newServer(t, store)); code != 200 || spot != nil {
		t.Fatalf("missing row: %d %+v", code, spot)
	}
	// A row that cannot be read is an error rather than a silent null.
	for _, row := range []string{`not json`, `{"price":"4523.40","at":"yesterday","source":"coinbase"}`} {
		store := seed(t)
		if err := store.SetState(ctx, robinhood, db.StateEthUsd, row); err != nil {
			t.Fatal(err)
		}
		if _, code := liveSpot(t, newServer(t, store)); code != 500 {
			t.Fatalf("unreadable row %q served %d", row, code)
		}
	}
	// A read failure is reported too.
	store = seed(t)
	store.SetFailure("GetState", true)
	if _, code := liveSpot(t, newServer(t, store)); code != 500 {
		t.Fatalf("state read failure served %d", code)
	}
}

// TestEthUsdMaxAgeOption: the API's cutoff follows the collector's
// configuration, and a non-positive value keeps the default.
func TestEthUsdMaxAgeOption(t *testing.T) {
	store := seed(t)
	cfg := config.ServerConfig{RateLimitPerSecond: 1000, RateLimitBurst: 1000}
	// The seeded robinhood quote is two minutes old: a one-minute cutoff
	// drops it, the default keeps it.
	short := New(store, cfg, nil, logger.Nop(), WithClock(func() time.Time { return now }), WithEthUsdMaxAge(time.Minute))
	snap, err := short.buildLive(context.Background(), robinhood)
	if err != nil || snap.EthUsd != nil {
		t.Fatalf("a one-minute cutoff should drop a two-minute-old quote: %+v %v", snap.EthUsd, err)
	}
	deflt := New(store, cfg, nil, logger.Nop(), WithClock(func() time.Time { return now }), WithEthUsdMaxAge(0))
	if deflt.ethUsdMaxAge != config.DefaultEthUsdMaxAge {
		t.Fatalf("max age = %v, want the default", deflt.ethUsdMaxAge)
	}
	if snap, err = deflt.buildLive(context.Background(), robinhood); err != nil || snap.EthUsd == nil {
		t.Fatalf("the default cutoff should keep it: %+v %v", snap.EthUsd, err)
	}
}

// TestLiveIsOneSnapshot: /live reads the sample, its block, the slow
// sample and the gas rates inside one snapshot transaction.
func TestLiveIsOneSnapshot(t *testing.T) {
	store := seed(t)
	ts := newServer(t, store)
	if resp, _ := get(t, ts, "/api/v1/networks/robinhood/live"); resp.StatusCode != 200 {
		t.Fatalf("live: %d", resp.StatusCode)
	}
	store.SetFailure("WithSnapshotTx", true)
	if resp, _ := get(t, ts, "/api/v1/networks/robinhood/live"); resp.StatusCode != 500 {
		t.Fatalf("live must run inside the snapshot transaction: %d", resp.StatusCode)
	}
}

func TestStoreFailures(t *testing.T) {
	paths := []string{
		"/ready", "/api/v1/networks", "/api/v1/networks/robinhood", "/api/v1/networks/robinhood/live", "/api/v1/networks/robinhood/blocks",
		"/api/v1/networks/robinhood/series", "/api/v1/networks/robinhood/series?range=24h", "/api/v1/networks/robinhood/constraints",
		"/api/v1/networks/robinhood/owner-actions", "/api/v1/networks/robinhood/batches", "/api/v1/networks/robinhood/l1", "/api/v1/status",
	}
	methods := []string{"Networks", "NetworkByRef", "LatestStateSample", "RecentBlocks", "BlocksBetween", "Buckets", "ConstraintSets", "OwnerActions", "BatchBuckets", "BatchReports", "L1Samples", "States", "BlockByNumber", "GasUsedBetween"}
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
	// Constraints: a sample lookup failure is an internal error.
	store = seed(t)
	store.SetFailure("LatestStateSample", true)
	ts = newServer(t, store)
	if resp, _ := get(t, ts, "/api/v1/networks/robinhood/constraints"); resp.StatusCode != 500 {
		t.Fatalf("constraints sample failure: %d", resp.StatusCode)
	}
}

// TestConstraintsCurrentIsModelGated: a network with recorded sets whose
// latest sample is the legacy model reports current as null while keeping
// the history.
func TestConstraintsCurrentIsModelGated(t *testing.T) {
	store := dbtest.New()
	ctx := context.Background()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: arbOne, Name: "arbitrum-one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: arbOne, EffectiveBlock: 1, EffectiveAt: now.Add(-time.Hour), Source: model.SourceObserved, Constraints: db.JSONB(`[{"target":1,"window":1,"startingBacklog":0}]`)}); err != nil {
		t.Fatal(err)
	}
	ts := newServer(t, store)
	// No sample yet: null.
	if _, b := get(t, ts, "/api/v1/networks/arbitrum-one/constraints"); !strings.Contains(string(b), `"current":null`) || !strings.Contains(string(b), `"source":"observed"`) {
		t.Fatalf("no sample: %s", b)
	}
	if err := store.InsertStateSample(ctx, db.StateSample{ChainID: arbOne, SampledAt: now, BlockNumber: 5, Constraints: db.JSONB(`[]`), Legacy: db.JSONB(`{"speedLimit":1,"inertia":1,"tolerance":1,"backlog":0}`), Prices: db.JSONB(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, b := get(t, ts, "/api/v1/networks/arbitrum-one/constraints"); !strings.Contains(string(b), `"current":null`) {
		t.Fatalf("legacy sample: %s", b)
	}
	if err := store.InsertStateSample(ctx, db.StateSample{ChainID: arbOne, SampledAt: now.Add(time.Second), BlockNumber: 6, Constraints: db.JSONB(`[{"target":1,"window":1,"backlog":0,"exponentBips":0}]`), Prices: db.JSONB(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, b := get(t, ts, "/api/v1/networks/arbitrum-one/constraints"); !strings.Contains(string(b), `"current":{"id":1`) {
		t.Fatalf("constraints sample: %s", b)
	}
}

// TestLiveIsOneTick: /live describes the block the latest sample was taken
// at, even when the collector stored a newer block meanwhile.
func TestLiveIsOneTick(t *testing.T) {
	store := seed(t)
	ctx := context.Background()
	if err := store.UpsertBlocks(ctx, []db.Block{{ChainID: robinhood, Number: 1031, TS: now.Add(time.Second), GasUsed: 9, BaseFee: db.WeiFromUint64(5), PredictedBaseFee: db.WeiFromUint64(5), Backlogs: db.Uint64Array{1, 1}}}); err != nil {
		t.Fatal(err)
	}
	ts := newServer(t, store)
	resp, body := get(t, ts, "/api/v1/networks/robinhood/live")
	var snap model.LiveSnapshot
	decode(t, body, &snap)
	if resp.StatusCode != 200 || snap.Block.Number != 1030 || snap.Block.BaseFee != "20000030" || snap.ReplayErrorBips != 15 {
		t.Fatalf("live must use the sample's block: %d %+v", resp.StatusCode, snap)
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
	clock := now
	rl.now = func() time.Time { return clock }
	if !rl.allow("a") || rl.allow("a") {
		t.Fatal("limiter")
	}
	// IPv6 remote addresses keep the whole address.
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	req.RemoteAddr = "[::1]:1234"
	rec := httptest.NewRecorder()
	rl.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(rec, req)
	if rec.Code != 204 || rl.clients["[::1]"] == nil {
		t.Fatalf("ipv6 key: %d %v", rec.Code, rl.clients)
	}
	if clientIP("1.2.3.4") != "1.2.3.4" || clientIP("1.2.3.4:5") != "1.2.3.4" || clientIP("[::1]:5") != "[::1]" {
		t.Fatal("clientIP")
	}
}

// TestRateLimiterBounded: the limiter never holds more than the cap (the
// least recently seen address is evicted), idle addresses expire on a
// timer independent of new clients, and a returning address keeps its
// bucket.
func TestRateLimiterBounded(t *testing.T) {
	rl := newRateLimiter(1, 1)
	rl.maxEntry = 3
	clock := now
	rl.now = func() time.Time { return clock }
	for _, ip := range []string{"a", "b", "c"} {
		rl.allow(ip)
	}
	clock = clock.Add(time.Second)
	rl.allow("a") // a is now the most recently seen
	rl.allow("d") // over the cap: b, the least recently seen, is evicted
	if rl.size() != 3 || rl.clients["b"] != nil || rl.clients["a"] == nil || rl.clients["d"] == nil {
		t.Fatalf("eviction: size %d clients %v", rl.size(), rl.clients)
	}
	// b comes back with a fresh bucket, evicting c.
	if !rl.allow("b") || rl.clients["c"] != nil {
		t.Fatal("re-admission")
	}
	// a's bucket is drained (one token, used twice); it refills with time.
	if rl.allow("a") {
		t.Fatal("a should be out of tokens")
	}
	clock = clock.Add(2 * time.Second)
	if !rl.allow("a") {
		t.Fatal("a should refill")
	}
	// Idle entries expire on the sweep even when no new address arrives.
	clock = clock.Add(rateEntryTTL + rateSweepEvery)
	rl.allow("a")
	if rl.size() != 1 || rl.clients["b"] != nil || rl.clients["d"] != nil {
		t.Fatalf("sweep: size %d clients %v", rl.size(), rl.clients)
	}
	// The sweep is rate limited: an entry that goes stale less than a sweep
	// interval after the last sweep survives until the next one.
	rl.allow("e")
	clock = clock.Add(rateEntryTTL - 30*time.Second)
	rl.allow("a") // sweeps; e is still fresh
	if rl.size() != 2 {
		t.Fatalf("fresh entry swept: %d", rl.size())
	}
	clock = clock.Add(31 * time.Second)
	rl.allow("a") // e is stale now but the last sweep was 31 s ago
	if rl.size() != 2 {
		t.Fatalf("sweep should wait for its interval: %d", rl.size())
	}
	clock = clock.Add(rateSweepEvery)
	rl.allow("a")
	if rl.size() != 1 {
		t.Fatalf("stale entry should be swept: %d", rl.size())
	}
	// Ten thousand distinct addresses stay within the cap.
	full := newRateLimiter(1, 1)
	full.now = func() time.Time { return clock }
	for i := range maxRateEntries * 2 {
		full.allow(strings.Repeat("x", 1) + string(rune(i)))
	}
	if full.size() != maxRateEntries {
		t.Fatalf("cap: %d", full.size())
	}
}

// TestRealIPTrustedProxies: forwarding headers only count when the peer is
// a configured proxy, and the chain is walked right to left past trusted
// hops, so a client cannot mint addresses through X-Forwarded-For.
func TestRealIPTrustedProxies(t *testing.T) {
	trusted, err := (config.ServerConfig{TrustedProxies: []string{"10.0.0.0/8", "127.0.0.1"}}).TrustedProxyNets()
	if err != nil {
		t.Fatal(err)
	}
	seen := ""
	handler := realIP(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.RemoteAddr }))
	call := func(remote string, headers map[string]string) string {
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.RemoteAddr = remote
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)
		return seen
	}
	cases := []struct {
		name    string
		remote  string
		headers map[string]string
		want    string
	}{
		{"untrusted peer keeps its address", "203.0.113.5:1", map[string]string{"X-Forwarded-For": "198.51.100.7"}, "203.0.113.5:1"},
		{"private but untrusted peer keeps its address", "192.168.1.9:1", map[string]string{"X-Forwarded-For": "198.51.100.7"}, "192.168.1.9:1"},
		{"trusted proxy, one hop", "10.1.1.1:1", map[string]string{"X-Forwarded-For": "198.51.100.7"}, "198.51.100.7:0"},
		{"trusted proxy, spoofed prefix is skipped", "10.1.1.1:1", map[string]string{"X-Forwarded-For": "1.1.1.1, 198.51.100.7, 10.2.2.2"}, "198.51.100.7:0"},
		{"all hops trusted keeps the leftmost", "10.1.1.1:1", map[string]string{"X-Forwarded-For": "10.3.3.3, 10.2.2.2"}, "10.3.3.3:0"},
		{"two headers", "127.0.0.1:1", map[string]string{"X-Forwarded-For": "198.51.100.9"}, "198.51.100.9:0"},
		{"garbage hops are ignored", "10.1.1.1:1", map[string]string{"X-Forwarded-For": "nope, 198.51.100.7"}, "198.51.100.7:0"},
		{"x-real-ip fallback", "10.1.1.1:1", map[string]string{"X-Real-IP": "198.51.100.8"}, "198.51.100.8:0"},
		{"nothing forwarded", "10.1.1.1:1", nil, "10.1.1.1:1"},
		{"bad remote", "nonsense", map[string]string{"X-Forwarded-For": "198.51.100.7"}, "nonsense"},
	}
	for _, tc := range cases {
		if got := call(tc.remote, tc.headers); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
	// Without trusted proxies the peer is always the client.
	none := realIP(nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.RemoteAddr }))
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	none.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "127.0.0.1:1" {
		t.Fatalf("no trusted proxies: %q", seen)
	}
	// An invalid configuration is ignored with a warning, not fatal.
	s := New(seed(t), config.ServerConfig{TrustedProxies: []string{"bad"}}, nil, nil)
	if s.Handler() == nil {
		t.Fatal("handler")
	}
	// The limiter keys on the forwarded client behind a trusted proxy.
	srv := New(seed(t), config.ServerConfig{RateLimitPerSecond: 1, RateLimitBurst: 1, TrustedProxies: []string{"127.0.0.1", "::1"}}, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for i, want := range []int{200, 200, 429} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/health", http.NoBody)
		req.Header.Set("X-Forwarded-For", []string{"198.51.100.1", "198.51.100.2", "198.51.100.2"}[i])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("request %d: %d want %d", i, resp.StatusCode, want)
		}
	}
}

func TestHelpers(t *testing.T) {
	if got := originPatterns([]string{"http://localhost:3000", "*", "x"}); len(got) != 1 || got[0] != "*" {
		t.Fatalf("wildcard: %v", got)
	}
	if got := originPatterns([]string{"http://localhost:3000", "bad host"}); len(got) != 2 || got[0] != "localhost:3000" || got[1] != "bad host" {
		t.Fatalf("patterns: %v", got)
	}
	if setIDAt(nil, 5, 1) != 0 {
		t.Fatal("setIDAt")
	}
	// A block is only tagged with a set whose constraint count matches its
	// backlogs; an unreadable set matches nothing.
	shaped := []db.ConstraintSet{{ID: 1, EffectiveBlock: 1, Constraints: db.JSONB(`[{"target":1}]`)}, {ID: 2, EffectiveBlock: 2, Constraints: db.JSONB(`[{"target":1},{"target":2}]`)}, {ID: 3, EffectiveBlock: 3, Constraints: db.JSONB(`{bad`)}}
	if setIDAt(shaped, 5, 1) != 1 || setIDAt(shaped, 5, 2) != 2 || setIDAt(shaped, 5, 3) != 0 || setIDAt(shaped, 1, 2) != 0 {
		t.Fatal("setIDAt shape")
	}
	if int64s(nil) != nil || int64s([]int64{}) == nil {
		t.Fatal("int64s keeps null and empty apart")
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
	if p := (&acc{blocks: 1, sum: bigInt(0), fees: bigInt(0), minFee: bigInt(1), maxFee: bigInt(1)}).point(5); p.BacklogsMax == nil || p.Backlogs == nil || p.ConstraintBips != nil || p.MinBaseFee != nil || *p.FloorFeesWei != "0" {
		t.Fatalf("point: %+v", p)
	}
	if legacyExponent(&model.LegacyParams{SpeedLimit: 1, Inertia: 1, Tolerance: 1, Backlog: 0}) != 0 || legacyExponent(&model.LegacyParams{SpeedLimit: 10, Inertia: 10, Tolerance: 1, Backlog: 110}) != 10_000 {
		t.Fatal("legacyExponent")
	}
}

// TestRangeBounds: a bounded range reports its window; the all range
// reports the first indexed point, or an empty window when nothing is.
func TestRangeBounds(t *testing.T) {
	from, to := ranges[rangeDay].window(now)
	if f, tt := ranges[rangeDay].bounds(from, to, 5, true); f != from.Unix() || tt != to.Unix() {
		t.Fatalf("day: %d..%d", f, tt)
	}
	from, to = ranges[rangeAll].window(now)
	if f, tt := ranges[rangeAll].bounds(from, to, 1_700_000_000, true); f != 1_700_000_000 || tt != to.Unix() {
		t.Fatalf("all: %d..%d", f, tt)
	}
	if f, tt := ranges[rangeAll].bounds(from, to, 0, false); f != to.Unix() || tt != to.Unix() {
		t.Fatalf("all, nothing indexed: %d..%d", f, tt)
	}
	if firstPoint(nil) != 0 || firstBatch(nil) != 0 || firstL1(nil) != 0 {
		t.Fatal("first of nothing is zero")
	}
}
