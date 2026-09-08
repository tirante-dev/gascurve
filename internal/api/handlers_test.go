package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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
			ChainID: robinhood, Number: 1000 + i, TS: now.Add(-time.Duration(31-i) * time.Second), GasUsed: 1_000_000, PosterGas: sql.NullInt64{Valid: true},
			BaseFee: db.WeiFromUint64(20_000_000 + i), PredictedBaseFee: db.NullWeiFromUint64(19_970_000), L1Block: 5, TxCount: 3,
			Backlogs: db.Uint64Array{i, 100}, ConstraintBips: pq.Int64Array{int64(i), 0}, MinBaseFee: db.NullWeiFromUint64(20_000_000), ExponentBips: int64(i), Anchored: i == 30, PricingVersion: db.PricingFull,
		})
	}
	must(s.UpsertBlocks(ctx, blocks))
	must(s.UpsertBlocks(ctx, []db.Block{{ChainID: testnet, Number: 500, TS: now.Add(-3 * time.Second), GasUsed: 5, BaseFee: db.WeiFromUint64(10_000_000), PredictedBaseFee: db.NullWeiFromUint64(10_000_000), Backlogs: db.Uint64Array{7}, ExponentBips: 42}}))

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
			must(s.FoldBuckets(ctx, []db.Bucket{{ChainID: robinhood, Resolution: res, BucketStart: start, Blocks: 10, GasUsed: 100, PosterGas: sql.NullInt64{Valid: true}, FeesWei: db.WeiFromUint64(1000), PosterFeesWei: db.NullWeiFromUint64(0),
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
		{ChainID: robinhood, BlockNumber: 1005, BatchNumber: 1, BatchTS: now.Add(-10 * time.Minute), Poster: "0xp", CalldataLen: 100, GasSpent: 1000, WeiSpent: db.WeiFromUint64(5000), L1BaseFee: db.WeiFromUint64(5), CostCalculationVersion: 1},
		{ChainID: robinhood, BlockNumber: 1015, BatchNumber: 2, BatchTS: now.Add(-9 * time.Minute), Poster: "0xp", CalldataLen: 50, GasSpent: 500, WeiSpent: db.WeiFromUint64(1500), L1BaseFee: db.WeiFromUint64(3), CostCalculationVersion: 1},
		{ChainID: robinhood, BlockNumber: 1016, BatchNumber: 3, BatchTS: now.Add(-9 * time.Minute), Poster: "0xp", CalldataLen: 8, GasSpent: 7, WeiSpent: db.WeiFromUint64(77), L1BaseFee: db.WeiFromUint64(9), CostCalculationVersion: 1},
	}))
	must(s.SetState(ctx, robinhood, db.StateRateLimitEvents, "4"))
	must(s.SetState(ctx, robinhood, db.StateLast429At, "2026-09-06T07:00:00Z"))
	must(s.SetState(ctx, robinhood, db.StateArbOSVersion, "61"))
	must(s.SetState(ctx, robinhood, db.StateBackfillCursor, `{"done":true}`))
	must(s.SetState(ctx, robinhood, db.StateRPCCapacity, `{"configuredCallsPerSecond":4,"requiredCallsPerSecond":7.5,"observedCallsPerSecond":4,"headroomCallsPerSecond":-3.5,"saturated":true,"at":"2026-09-06T07:19:59Z","checkpointError":false}`))
	must(s.SetState(ctx, robinhood, db.StateEndpoints, `{"activeEndpoint":1,"failovers":3,"endpoints":[{"index":0,"ws":false,"archive":false,"disabled":true,"error":"reports chain id 1, configured 4663"},{"index":1,"ws":true,"archive":true,"disabled":false,"error":null}]}`))
	must(s.SetState(ctx, testnet, db.StateEndpoints, `not json`))
	must(s.SetState(ctx, robinhood, db.StateHoles, `[{"from":1001,"to":1100,"at":"2026-09-06T07:00:00Z","next":1051},{"from":0,"to":499,"at":"2026-09-06T07:00:00Z","reason":"no state"}]`))
	must(s.SetState(ctx, testnet, db.StateHoles, `not json`))
	must(s.SetState(ctx, robinhood, db.StateTelemetry, `{"heartbeatAt":"2026-09-06T07:19:58Z","heartbeatStaleAfterSeconds":30,"observedHead":1030,"indexedHead":1030,"headLagBlocks":0,"loops":{"fast":{"lastSuccessAt":"2026-09-06T07:19:59Z","lastErrorAt":"2026-09-06T07:20:00Z","lastError":"429 from endpoint 0","errorStreak":1,"lastDurationMs":20,"staleAfterSeconds":30},"slow":{"lastSuccessAt":"2026-09-06T07:19:30Z","lastErrorAt":null,"lastError":null,"lastDurationMs":40,"staleAfterSeconds":180},"history":{"lastSuccessAt":"2026-09-06T07:19:59Z","lastErrorAt":null,"lastError":null,"lastDurationMs":10,"staleAfterSeconds":180}},"rpc":{"calls":100,"requests":20,"errors":2,"callsLast10Seconds":7,"rateLimitEvents":4,"last429At":"2026-09-06T07:00:00Z","averageLatencyMs":12.5},"database":{"operations":80,"errors":1,"averageLatencyMs":3.5,"lastLatencyMs":2}}`))
	must(s.SetState(ctx, testnet, db.StateTelemetry, `{"heartbeatAt":"2026-09-06T07:19:58Z","heartbeatStaleAfterSeconds":30,"observedHead":502,"indexedHead":500,"headLagBlocks":2,"loops":{"fast":{"lastSuccessAt":"2026-09-06T07:19:40Z","lastErrorAt":"2026-09-06T07:19:59Z","lastError":"sample failed","errorStreak":4,"lastDurationMs":20,"staleAfterSeconds":30},"slow":{"lastSuccessAt":"2026-09-06T07:19:30Z","lastErrorAt":null,"lastError":null,"lastDurationMs":40,"staleAfterSeconds":180},"history":{"lastSuccessAt":"2026-09-06T07:19:59Z","lastErrorAt":null,"lastError":null,"lastDurationMs":10,"staleAfterSeconds":180}},"rpc":{"calls":10,"requests":5,"errors":1,"callsLast10Seconds":2,"rateLimitEvents":1,"last429At":null,"averageLatencyMs":8},"database":{"operations":80,"errors":1,"averageLatencyMs":3.5,"lastLatencyMs":2}}`))
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
	listener := &fakeListener{ch: make(chan db.Notification), status: db.ListenerStatus{Ready: true}}
	s := New(store, cfg, hub, logger.Nop(), WithListener(listener), WithClock(func() time.Time { return now }), WithVersion("test"))
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
			// arbitrum-one is seeded disabled, so the list holds the other two.
			if len(nets) != 2 || nets[0].Name != "robinhood" || nets[0].Model != model.ModelConstraints || nets[0].HeadBlock != 1030 || nets[0].LagSeconds == nil || *nets[0].LagSeconds != 1 {
				t.Fatalf("networks: %+v", nets)
			}
			if nets[1].Name != "robinhood-testnet" || nets[1].Model != model.ModelLegacy {
				t.Fatalf("networks models: %+v", nets)
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
			if s.GasPerSecond.S10 != 1_000_000 || s.GasPerSecond.S60 != 500_000 || s.ComputeGasPerSecond.S10 == nil || *s.ComputeGasPerSecond.S10 != 1_000_000 || s.ReplayErrorBips != 15 || s.Prices.PerArbGasTotal != "6" {
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
		{"/api/v1/networks/robinhood/series?range=24h", 200, cacheHistory, func(t *testing.T, b []byte) {
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
		{"/api/v1/networks/robinhood/series?range=30d", 200, cacheHistory, func(t *testing.T, b []byte) {
			var s model.Series
			decode(t, b, &s)
			if s.Resolution != "15m" || len(s.Points) != 3 || len(s.OwnerActions) != 2 {
				t.Fatalf("series 30d: %+v", s)
			}
		}},
		{"/api/v1/networks/robinhood/series?range=all", 200, cacheHistory, func(t *testing.T, b []byte) {
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
		{"/api/v1/networks/robinhood/constraints", 200, cacheOwner, func(t *testing.T, b []byte) {
			var out struct {
				Current *model.ConstraintSet  `json:"current"`
				History []model.ConstraintSet `json:"history"`
			}
			decode(t, b, &out)
			if out.Current == nil || out.Current.ID != 2 || len(out.History) != 2 || out.History[1].Source != model.SourceGenesis || len(out.Current.Constraints) != 2 || out.Current.Constraints[1].StartingBacklog != 9_989_222_400_000 {
				t.Fatalf("constraints: %+v", out)
			}
		}},
		{"/api/v1/networks/robinhood-testnet/constraints", 200, cacheOwner, func(t *testing.T, b []byte) {
			if !strings.Contains(string(b), `"current":null`) || !strings.Contains(string(b), `"history":[]`) {
				t.Fatalf("empty constraints: %s", b)
			}
		}},

		{"/api/v1/networks/robinhood/owner-actions?limit=1", 200, cacheOwner, func(t *testing.T, b []byte) {
			var acts []model.OwnerAction
			decode(t, b, &acts)
			if len(acts) != 1 || acts[0].Block != 174150 || acts[0].Method != "setMinimumL2BaseFee" || string(acts[0].Args) != "{}" {
				t.Fatalf("owner actions: %+v", acts)
			}
		}},
		{"/api/v1/networks/robinhood/batches?range=24h", 200, cacheHistory, func(t *testing.T, b []byte) {
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
			// The per-report resolution reports its window like every other.
			if s.From != now.Add(-time.Hour).Unix() || s.To != now.Add(time.Second).Unix() {
				t.Fatalf("batches 1h window: %d..%d", s.From, s.To)
			}
		}},
		{"/api/v1/networks/robinhood/batches?range=x", 400, cacheNone, nil},
		{"/api/v1/networks/robinhood/l1?range=24h", 200, cacheHistory, func(t *testing.T, b []byte) {
			var s model.L1Series
			decode(t, b, &s)
			if s.Range != "24h" || len(s.Points) != 2 || s.Points[1].BaseFeeEstimate != "2369608" || s.Points[1].UnitsSinceUpdate != 7 || s.Points[1].Surplus != "-1" {
				t.Fatalf("l1: %+v", s)
			}
		}},
		{"/api/v1/networks/robinhood/l1?range=all", 200, cacheHistory, func(t *testing.T, b []byte) {
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
			if s.Version != "test" || s.Status != model.StatusDegraded || s.Listener == nil || !s.Listener.Ready || s.Listener.Reconnects != 0 || s.Listener.LastError != nil || len(s.Networks) != 3 {
				t.Fatalf("status: %+v", s)
			}
			rh := s.Networks[0]
			if rh.RateLimitEvents != 4 || rh.Last429At == nil || *rh.Last429At != "2026-09-06T07:00:00Z" || rh.ArbOSVersion == nil || rh.BackfillCursor == nil || rh.LagSeconds == nil || *rh.LagSeconds != 1 || rh.LastSampleAt == nil {
				t.Fatalf("status robinhood: %+v", rh)
			}
			if !rh.Capacity.Saturated || rh.Capacity.RequiredCallsPerSecond != 7.5 || rh.Capacity.HeadroomCallsPerSecond == nil || *rh.Capacity.HeadroomCallsPerSecond != -3.5 {
				t.Fatalf("status capacity: %+v", rh.Capacity)
			}
			if arb := s.Networks[1]; arb.LastError == nil || *arb.LastError != "rpc down" || arb.Enabled || arb.Last429At != nil || arb.HeadAt != nil || arb.LagSeconds != nil || arb.LastSampleAt != nil {
				t.Fatalf("status arbitrum: %+v", arb)
			}
			if !strings.Contains(string(b), `"lastSampleAt":null`) {
				t.Fatalf("null lastSampleAt expected: %s", b)
			}
			// Holes are summarized: what is queued for the gap filler, how
			// many blocks are still missing and what can never be filled.
			if rh.Holes.Pending != 1 || rh.Holes.Unfillable != 1 || rh.Holes.Blocks != 50+500 {
				t.Fatalf("status holes: %+v", rh.Holes)
			}
			if rh.Holes.PendingBlocks != 50 || rh.Holes.OldestPendingAgeSeconds == nil || *rh.Holes.OldestPendingAgeSeconds != 1200 {
				t.Fatalf("status pending hole age: %+v", rh.Holes)
			}
			// A network without ranges reports zeros. An unreadable legacy
			// checkpoint is an explicit degradation instead of looking empty.
			if s.Networks[1].Holes != (model.HolesStatus{}) || !s.Networks[2].Holes.CheckpointError || !s.Networks[2].Degraded {
				t.Fatalf("status holes default: %+v %+v", s.Networks[1].Holes, s.Networks[2].Holes)
			}
			if !rh.Degraded || rh.Holes.OldestAgeSeconds != 20*60 || !strings.Contains(string(b), `"holes":{"pending":1,"blocks":550,"unfillable":1,"retrying":0,"oldestAgeSeconds":1200,"checkpointError":false,"pendingBlocks":50`) {
				t.Fatalf("holes json: %s", b)
			}
			if rh.Collector == nil || rh.Collector.HeartbeatAgeSeconds == nil || *rh.Collector.HeartbeatAgeSeconds != 2 || rh.Collector.RPC.AverageLatencyMS != 12.5 || rh.Collector.Database.LastLatencyMS != 2 {
				t.Fatalf("collector telemetry: %+v", rh.Collector)
			}
			if rh.Status != model.StatusDegraded || !slices.Contains(rh.DegradedReasons, "pending gaps are stale") {
				t.Fatalf("robinhood degradation: %+v", rh)
			}
			// One 429 since the last success is what a metered public RPC
			// does. Only a streak of them is a failing loop.
			if slices.Contains(rh.DegradedReasons, "fast loop failing") {
				t.Fatalf("a single loop error must not degrade: %+v", rh.DegradedReasons)
			}
			tn := s.Networks[2]
			if tn.Status != model.StatusDegraded || !slices.Contains(tn.DegradedReasons, "fast loop failing") || !slices.Contains(tn.DegradedReasons, "collector behind observed head") {
				t.Fatalf("testnet degradation: %+v", tn)
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

func TestHolesStatusReportsMalformedDurableReplayState(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	rangeRow := db.MissingRange{
		ChainID: robinhood, From: 100, To: 109, DetectedAt: now.Add(-time.Minute),
		Lifecycle: model.MissingRangePending, ReplayState: db.JSONB(`[]`),
	}
	if err := store.ReplaceMissingRanges(ctx, robinhood, []db.MissingRange{rangeRow}); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.ServerConfig{}, nil, logger.Nop(), WithClock(func() time.Time { return now }))
	status, err := s.holesStatus(ctx, robinhood, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !status.CheckpointError || status.Pending != 1 || status.Blocks != 10 || status.OldestAgeSeconds != 60 {
		t.Fatalf("durable range status: %+v", status)
	}
	rows, err := store.MissingRanges(ctx, robinhood)
	if err != nil || len(rows) != 1 || string(rows[0].ReplayState) != `[]` {
		t.Fatalf("durable range must remain visible: %+v %v", rows, err)
	}
}

func TestReadyAndStatusExposeListenerFailure(t *testing.T) {
	store := dbtest.New()
	cfg := config.ServerConfig{RateLimitPerSecond: 1000, RateLimitBurst: 1000}
	listener := &fakeListener{ch: make(chan db.Notification), status: db.ListenerStatus{
		Reconnects: 2,
		Error:      "connection lost",
	}}
	s := New(store, cfg, nil, logger.Nop(), WithListener(listener), WithVersion("test"))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, body := get(t, ts, "/ready")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "notification listener unavailable") {
		t.Fatalf("readiness during listener outage: %d %s", resp.StatusCode, body)
	}
	resp, body = get(t, ts, "/api/v1/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status during listener outage: %d %s", resp.StatusCode, body)
	}
	var status model.Status
	decode(t, body, &status)
	if status.Status != model.StatusDegraded || status.Listener == nil || status.Listener.Ready || status.Listener.Reconnects != 2 || status.Listener.LastError == nil || *status.Listener.LastError != "connection lost" {
		t.Fatalf("listener status: %+v", status.Listener)
	}
}

func TestStatusHealthyAfterRecovery(t *testing.T) {
	store := seed(t)
	ctx := context.Background()
	if err := store.SetState(ctx, robinhood, db.StateHoles, `[]`); err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(ctx, robinhood, db.StateRPCCapacity, `{"configuredCallsPerSecond":12,"requiredCallsPerSecond":7.5,"observedCallsPerSecond":4,"headroomCallsPerSecond":4.5,"saturated":false,"at":"2026-09-06T07:19:59Z","checkpointError":false}`); err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(ctx, robinhood, db.StateEndpoints, `{"activeEndpoint":0,"failovers":0,"endpoints":[{"index":0,"ws":false,"archive":false,"disabled":false,"error":null,"wsCooling":false,"wsError":null}]}`); err != nil {
		t.Fatal(err)
	}
	testnetRow, err := store.NetworkByRef(ctx, "robinhood-testnet")
	if err != nil || testnetRow == nil {
		t.Fatalf("testnet row: %+v %v", testnetRow, err)
	}
	testnetRow.Enabled = false
	if err := store.UpsertNetwork(ctx, *testnetRow); err != nil {
		t.Fatal(err)
	}
	ts := newServer(t, store)
	resp, body := get(t, ts, "/api/v1/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var status model.Status
	decode(t, body, &status)
	if status.Status != model.StatusHealthy || status.Networks[0].Status != model.StatusHealthy || len(status.Networks[0].DegradedReasons) != 0 {
		t.Fatalf("healthy status: %+v", status)
	}
}

func TestLiveThroughputKeepsTotalAndMakesComputeNullable(t *testing.T) {
	ctx := context.Background()
	store := seed(t)
	rows := make([]db.Block, 0, 30)
	for _, block := range store.BlockRows[robinhood] {
		block.PosterGas = sql.NullInt64{Int64: 100_000, Valid: true}
		rows = append(rows, block)
	}
	if err := store.UpsertBlocks(ctx, rows); err != nil {
		t.Fatal(err)
	}
	snap, err := buildLiveIn(ctx, store, robinhood, now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if snap.GasPerSecond != (model.GasPerSecond{S10: 1_000_000, S60: 500_000}) || snap.ComputeGasPerSecond.S10 == nil || *snap.ComputeGasPerSecond.S10 != 900_000 || snap.ComputeGasPerSecond.S60 == nil || *snap.ComputeGasPerSecond.S60 != 450_000 {
		t.Fatalf("known live rates: %+v", snap)
	}
	unknown := store.BlockRows[robinhood][1025]
	unknown.PosterGas = sql.NullInt64{}
	if err := store.UpsertBlocks(ctx, []db.Block{unknown}); err != nil {
		t.Fatal(err)
	}
	snap, err = buildLiveIn(ctx, store, robinhood, now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if snap.GasPerSecond != (model.GasPerSecond{S10: 1_000_000, S60: 500_000}) || snap.ComputeGasPerSecond.S10 != nil || snap.ComputeGasPerSecond.S60 != nil {
		t.Fatalf("unknown compute rates must not change total rates: %+v", snap)
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
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var blocks []db.Block
	for i := uint64(0); i < 2500; i++ {
		blocks = append(blocks, db.Block{ChainID: robinhood, Number: i, TS: now.Add(-time.Duration(2500-i) * 100 * time.Millisecond).Truncate(time.Second), GasUsed: 10,
			PosterGas: sql.NullInt64{Valid: true}, BaseFee: db.WeiFromUint64(100 + i%3), PredictedBaseFee: db.NullWeiFromUint64(100), Backlogs: db.Uint64Array{i % 5, 9}, ConstraintBips: pq.Int64Array{int64(i), 0}, MinBaseFee: db.NullWeiFromUint64(50), ExponentBips: int64(i), PricingVersion: db.PricingFull})
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

func TestSeriesPosterGasFeeDestinations(t *testing.T) {
	b := db.Block{
		TS:               now,
		GasUsed:          422_716,
		PosterGas:        sql.NullInt64{Int64: 767, Valid: true},
		BaseFee:          db.WeiFromUint64(20_036_000),
		PredictedBaseFee: db.NullWeiFromUint64(20_036_000),
		MinBaseFee:       db.NullWeiFromUint64(20_000_000),
		PricingVersion:   db.PricingFull,
	}
	points := blockPoints([]db.Block{b}, nil)
	if len(points) != 1 {
		t.Fatalf("points: %+v", points)
	}
	p := points[0]
	if p.PosterGas == nil || *p.PosterGas != 767 || p.ComputeGasPerSecond == nil || *p.ComputeGasPerSecond != 421_949 || p.FeesWei != "8469537776000" || p.FloorFeesWei == nil || *p.FloorFeesWei != "8438980000000" || *p.SurplusFeesWei != "15190164000" || *p.PosterFeesWei != "15367612000" {
		t.Fatalf("block destination split: %+v", p)
	}
	steps, _, _ := stepDown([]db.Block{b}, nil, now)
	stepped := steps[0]
	if stepped.PosterGas == nil || *stepped.PosterGas != 767 || stepped.PosterFeesWei == nil || *stepped.PosterFeesWei != "15367612000" {
		t.Fatalf("step destination split: %+v", stepped)
	}

	b.PosterGas = sql.NullInt64{}
	unknown := blockPoints([]db.Block{b}, nil)[0]
	if unknown.MinBaseFee == nil || unknown.ComputeGasPerSecond != nil || unknown.FloorFeesWei != nil || unknown.SurplusFeesWei != nil || unknown.PosterFeesWei != nil || unknown.PosterGas != nil {
		t.Fatalf("receipt availability must be independent from pricing: %+v", unknown)
	}
	legacyBucket := db.Bucket{
		BucketStart: now.Add(-time.Minute), Blocks: 1, GasUsed: 10, FeesWei: db.WeiFromUint64(70),
		BaseFeeAvg: db.WeiFromUint64(7), FloorFeesWei: db.NullWeiFromUint64(40), SurplusFeesWei: db.NullWeiFromUint64(30),
	}
	bucket, ok := bucketPoint(legacyBucket, time.Minute, now, time.Time{})
	if !ok || bucket.FloorFeesWei != nil || bucket.SurplusFeesWei != nil {
		t.Fatalf("legacy two-way sums must be hidden without poster gas: %+v", bucket)
	}
}

// TestUnknownHistoryIsNull: buckets and blocks written before the fee
// split and the exponents were recorded (pricing version 0, whose unknown
// columns are NULL) report them as null, never as fabricated zeros or
// empty arrays; the
// average of such a bucket comes from its stored value.
func TestUnknownHistoryIsNull(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	start := now.Add(-2 * time.Minute).Truncate(time.Minute)
	if err := store.FoldBuckets(ctx, []db.Bucket{{ChainID: robinhood, Resolution: db.Resolution1m, BucketStart: start, Blocks: 3, GasUsed: 30, FeesWei: db.WeiFromUint64(90),
		BaseFeeMin: db.WeiFromUint64(2), BaseFeeAvg: db.WeiFromUint64(3), BaseFeeMax: db.WeiFromUint64(4), BacklogsEnd: db.Uint64Array{1}, BacklogsMax: db.Uint64Array{1}, LastBlock: 3}}); err != nil {
		t.Fatal(err)
	}
	// Two old blocks (no exponents) and one written after the migration.
	if err := store.UpsertBlocks(ctx, []db.Block{
		{ChainID: robinhood, Number: 1, TS: now.Add(-30 * time.Second), GasUsed: 10, BaseFee: db.WeiFromUint64(5), PredictedBaseFee: db.NullWeiFromUint64(5), Backlogs: db.Uint64Array{1}},
		{ChainID: robinhood, Number: 2, TS: now.Add(-29 * time.Second), GasUsed: 10, BaseFee: db.WeiFromUint64(5), PredictedBaseFee: db.NullWeiFromUint64(5), Backlogs: db.Uint64Array{1}},
		{ChainID: robinhood, Number: 3, TS: now.Add(-20 * time.Second), GasUsed: 10, PosterGas: sql.NullInt64{Valid: true}, BaseFee: db.WeiFromUint64(6), PredictedBaseFee: db.NullWeiFromUint64(6), Backlogs: db.Uint64Array{1}, ConstraintBips: pq.Int64Array{}, MinBaseFee: db.NullWeiFromUint64(2), PricingVersion: db.PricingFull},
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
	if p, _, _ := stepDown([]db.Block{{TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.NullWeiFromUint64(1), ConstraintBips: pq.Int64Array{}}, {TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.NullWeiFromUint64(1)}}, nil, now); len(p) != 1 || p[0].FloorFeesWei != nil {
		t.Fatalf("step down with an unknown block: %+v", p)
	}
}

// TestLiveEthUsd: /live applies the collector's staleness rule to the
// recorded spot, rejects one stamped materially later than the serving
// clock, serves null when there is none and refuses to invent one from a
// row it cannot read.
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
		// A quote stamped later than the serving clock comes from a clock
		// that disagrees with this one: a small skew is tolerated, a large
		// one makes the age unknowable and the quote unusable.
		{"slightly ahead of the clock", -time.Minute, true},
		{"materially ahead of the clock", -(ethUsdFutureSkew + time.Minute), false},
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

// TestLiveAndSeriesUseSnapshots: /live reads the sample, its block, the slow
// sample and gas rates in one database moment. /series likewise reads buckets
// and their completeness evidence together.
func TestLiveAndSeriesUseSnapshots(t *testing.T) {
	store := seed(t)
	ts := newServer(t, store)
	if resp, _ := get(t, ts, "/api/v1/networks/robinhood/live"); resp.StatusCode != 200 {
		t.Fatalf("live: %d", resp.StatusCode)
	}
	if resp, _ := get(t, ts, "/api/v1/networks/robinhood/series?range=24h"); resp.StatusCode != 200 {
		t.Fatalf("series: %d", resp.StatusCode)
	}
	store.SetFailure("WithSnapshotTx", true)
	if resp, _ := get(t, ts, "/api/v1/networks/robinhood/live"); resp.StatusCode != 500 {
		t.Fatalf("live must run inside the snapshot transaction: %d", resp.StatusCode)
	}
	if resp, _ := get(t, ts, "/api/v1/networks/robinhood/series?range=24h"); resp.StatusCode != 500 {
		t.Fatalf("series must run inside the snapshot transaction: %d", resp.StatusCode)
	}
}

func TestStoreFailures(t *testing.T) {
	paths := []string{
		"/ready", "/api/v1/networks", "/api/v1/networks/robinhood", "/api/v1/networks/robinhood/live", "/api/v1/networks/robinhood/blocks",
		"/api/v1/networks/robinhood/series", "/api/v1/networks/robinhood/series?range=24h", "/api/v1/networks/robinhood/constraints",
		"/api/v1/networks/robinhood/owner-actions", "/api/v1/networks/robinhood/batches", "/api/v1/networks/robinhood/l1", "/api/v1/status",
	}
	methods := []string{"Networks", "NetworkByRef", "LatestStateSample", "RecentBlocks", "BlocksBetween", "Buckets", "ConstraintSets", "OwnerActions", "MissingRanges", "BatchBuckets", "BatchReports", "L1Samples", "States", "BlockByNumber", "GasBetween"}
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
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: arbOne, Name: "arbitrum-one", Enabled: true}); err != nil {
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
	if err := store.UpsertBlocks(ctx, []db.Block{{ChainID: robinhood, Number: 1031, TS: now.Add(time.Second), GasUsed: 9, BaseFee: db.WeiFromUint64(5), PredictedBaseFee: db.NullWeiFromUint64(5), Backlogs: db.Uint64Array{1, 1}}}); err != nil {
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
	// The same forwarded addresses are not identities when the direct peer
	// is untrusted. This is the secure default for a direct deployment.
	direct := New(seed(t), config.ServerConfig{RateLimitPerSecond: 1, RateLimitBurst: 1}, nil, nil)
	directTS := httptest.NewServer(direct.Handler())
	defer directTS.Close()
	for i, want := range []int{200, 429} {
		req, _ := http.NewRequest(http.MethodGet, directTS.URL+"/health", http.NoBody)
		req.Header.Set("X-Forwarded-For", []string{"198.51.100.10", "198.51.100.11"}[i])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("untrusted request %d: %d want %d", i, resp.StatusCode, want)
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
	if p := (&acc{blocks: 1, sum: bigInt(0), fees: bigInt(0), minFee: bigInt(1), maxFee: bigInt(1)}).point(5*time.Second, now); p.BacklogsMax == nil || p.Backlogs == nil || p.ConstraintBips != nil || p.MinBaseFee != nil || *p.FloorFeesWei != "0" {
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

// TestSeriesCoverage: a bucket still in progress, and the bucket the
// collector started inside, report their rate over the span they cover and
// the share that span is, so a chart neither dips at its right edge nor
// reads a partial bucket as a quiet one. A bucket that ends before the
// live start was written by the backfiller or the gap filler and is whole,
// and a bucket entirely in the future is not a point at all.
func TestSeriesCoverage(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	clock := now.Add(20 * time.Second) // twenty seconds into the minute that starts at now
	inProgress := now.Truncate(time.Minute)
	backfilled := inProgress.Add(-3 * time.Minute)
	first := inProgress.Add(-2 * time.Minute)
	full := inProgress.Add(-time.Minute)
	bucket := func(start time.Time) db.Bucket {
		return db.Bucket{ChainID: robinhood, Resolution: db.Resolution1m, BucketStart: start, Blocks: 3, GasUsed: 300, FeesWei: db.WeiFromUint64(90),
			BaseFeeMin: db.WeiFromUint64(1), BaseFeeAvg: db.WeiFromUint64(1), BaseFeeMax: db.WeiFromUint64(1), BacklogsEnd: db.Uint64Array{}, BacklogsMax: db.Uint64Array{}, PricingVersion: db.PricingFull}
	}
	if err := store.FoldBuckets(ctx, []db.Bucket{bucket(backfilled), bucket(first), bucket(full), bucket(inProgress)}); err != nil {
		t.Fatal(err)
	}
	// The collector started forty-five seconds into the first bucket.
	if err := store.SetState(ctx, robinhood, db.StateLiveStart, `{"block":1,"ts":`+strconv.FormatInt(first.Add(45*time.Second).Unix(), 10)+`}`); err != nil {
		t.Fatal(err)
	}
	cfg := config.ServerConfig{RateLimitPerSecond: 1000, RateLimitBurst: 1000}
	s := New(store, cfg, nil, logger.Nop(), WithClock(func() time.Time { return clock }))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, body := get(t, ts, "/api/v1/networks/robinhood/series?range=24h")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out model.Series
	decode(t, body, &out)
	if len(out.Points) != 4 {
		t.Fatalf("points: %+v", out.Points)
	}
	// A bucket that ends before the live start is backfilled history, and
	// whole: the live start must not trim it to a single second, which
	// would multiply its rate by the width.
	if p := out.Points[0]; p.GasPerSecond != 5 || p.Coverage == nil || *p.Coverage != 1 || p.Completeness != model.SeriesComplete {
		t.Fatalf("backfilled bucket: %d gas/s coverage %v", p.GasPerSecond, p.Coverage)
	}
	// The bucket the live start falls inside: fifteen seconds of sixty.
	if p := out.Points[1]; p.GasPerSecond != 20 || p.Coverage == nil || *p.Coverage != 0.25 || p.Completeness != model.SeriesPartial {
		t.Fatalf("first live bucket: %d gas/s coverage %v", p.GasPerSecond, p.Coverage)
	}
	// A whole bucket.
	if p := out.Points[2]; p.GasPerSecond != 5 || p.Coverage == nil || *p.Coverage != 1 || p.Completeness != model.SeriesComplete {
		t.Fatalf("full bucket: %d gas/s coverage %v", p.GasPerSecond, p.Coverage)
	}
	// The bucket in progress: twenty seconds covered.
	if p := out.Points[3]; p.GasPerSecond != 15 || p.Coverage == nil || *p.Coverage < 0.33 || *p.Coverage > 0.34 || p.Completeness != model.SeriesPartial {
		t.Fatalf("bucket in progress: %d gas/s coverage %v", p.GasPerSecond, p.Coverage)
	}
	// An unreadable live_start is ignored, not an error.
	if err := store.SetState(ctx, robinhood, db.StateLiveStart, "{bad"); err != nil {
		t.Fatal(err)
	}
	if resp, _ := get(t, ts, "/api/v1/networks/robinhood/series?range=24h"); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// A bucket whose whole width is at or after the serving clock has no
	// span at all and is not returned; nothing is ever clamped up to a
	// second from nothing.
	if span := coveredSpan(now, time.Minute, now, time.Time{}); span > 0 {
		t.Fatalf("a bucket starting at now has no span: %v", span)
	}
	if _, ok := bucketPoint(db.Bucket{BucketStart: now.Add(time.Minute), BaseFeeMin: db.WeiFromUint64(1), BaseFeeAvg: db.WeiFromUint64(1),
		BaseFeeMax: db.WeiFromUint64(1), FeesWei: db.WeiFromUint64(1)}, time.Minute, now, time.Time{}); ok {
		t.Fatal("a bucket in the future must not be a point")
	}
	// The span never rises above the width, and a sub-second span still
	// counts as one second so a rate is never divided by zero.
	if span := coveredSpan(now, time.Minute, now.Add(time.Hour), now.Add(-time.Hour)); span != time.Minute {
		t.Fatalf("clamped high: %v", span)
	}
	if secs, share := coverage(time.Minute, time.Minute); secs != 60 || share != 1 {
		t.Fatalf("whole bucket: %d %v", secs, share)
	}
	if secs, share := coverage(500*time.Millisecond, time.Minute); secs != 1 || share > 0.01 {
		t.Fatalf("sub-second span: %d %v", secs, share)
	}
	// A live start outside the bucket does not trim it: a bucket entirely
	// before it is backfilled history, one entirely after it cannot exist.
	if span := coveredSpan(now, time.Minute, now.Add(time.Hour), now.Add(2*time.Minute)); span != time.Minute {
		t.Fatalf("live start past the bucket: %v", span)
	}
	if span := coveredSpan(now, time.Minute, now.Add(time.Hour), now.Add(-time.Second)); span != time.Minute {
		t.Fatalf("live start before the bucket: %v", span)
	}
	// Per-block points are whole by definition.
	if p := blockPoints([]db.Block{{TS: now, BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.NullWeiFromUint64(1), MinBaseFee: db.NullWeiFromUint64(1), PricingVersion: db.PricingFull}}, nil); p[0].Coverage == nil || *p[0].Coverage != 1 || p[0].Completeness != model.SeriesComplete {
		t.Fatalf("block coverage: %v", p[0].Coverage)
	}
}

// TestSeriesCoverageIncludesHoleInsidePopulatedBucket is the regression for a
// durable block gap hidden inside a bucket that still has aggregate rows. Its
// time envelope reduces coverage and the rate divisor, while the neighboring
// populated bucket remains complete.
func TestSeriesCoverageIncludesHoleInsidePopulatedBucket(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	start := now.Add(-2 * time.Minute)
	bucket := func(at time.Time) db.Bucket {
		return db.Bucket{
			ChainID: robinhood, Resolution: db.Resolution1m, BucketStart: at, Blocks: 3, GasUsed: 300,
			PosterGas: sql.NullInt64{Int64: 50, Valid: true}, FeesWei: db.WeiFromUint64(90), PosterFeesWei: db.NullWeiFromUint64(50),
			BaseFeeMin: db.WeiFromUint64(1), BaseFeeAvg: db.WeiFromUint64(1), BaseFeeMax: db.WeiFromUint64(1),
			BacklogsEnd: db.Uint64Array{}, BacklogsMax: db.Uint64Array{}, PricingVersion: db.PricingFull,
		}
	}
	if err := store.FoldBuckets(ctx, []db.Bucket{bucket(start), bucket(start.Add(time.Minute))}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMissingRanges(ctx, robinhood, []db.MissingRange{{
		ChainID: robinhood, From: 100, To: 109, Lifecycle: model.MissingRangePending,
		PredecessorAt: sql.NullTime{Time: start.Add(20 * time.Second), Valid: true},
		SuccessorAt:   sql.NullTime{Time: start.Add(30 * time.Second), Valid: true},
	}}); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.ServerConfig{RateLimitPerSecond: 1000, RateLimitBurst: 1000}, nil, logger.Nop(), WithClock(func() time.Time { return now }))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, body := get(t, ts, "/api/v1/networks/robinhood/series?range=24h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out model.Series
	decode(t, body, &out)
	if len(out.Points) != 2 {
		t.Fatalf("points: %+v", out.Points)
	}
	holed := out.Points[0]
	if holed.Completeness != model.SeriesPartial || holed.Coverage == nil || *holed.Coverage < 0.833 || *holed.Coverage > 0.834 || holed.GasPerSecond != 6 || holed.ComputeGasPerSecond == nil || *holed.ComputeGasPerSecond != 5 {
		t.Fatalf("populated bucket with internal hole: %+v", holed)
	}
	whole := out.Points[1]
	if whole.Completeness != model.SeriesComplete || whole.Coverage == nil || *whole.Coverage != 1 || whole.GasPerSecond != 5 || whole.ComputeGasPerSecond == nil || *whole.ComputeGasPerSecond != 4 {
		t.Fatalf("neighboring whole bucket: %+v", whole)
	}
}

func TestMissingTimelineUnknownAndCursorBounds(t *testing.T) {
	start := now.Add(-time.Minute)
	point := func(at time.Time) model.SeriesPoint {
		coverage := 1.0
		return model.SeriesPoint{T: at.Unix(), GasUsed: 60, GasPerSecond: 1, Coverage: &coverage, Completeness: model.SeriesComplete}
	}

	unknown := []model.SeriesPoint{point(start)}
	newMissingTimeline([]db.MissingRange{{From: 1, To: 2}}).apply(unknown, nil, time.Minute)
	if unknown[0].Completeness != model.SeriesUnknown || unknown[0].Coverage != nil {
		t.Fatalf("unbounded range: %+v", unknown[0])
	}

	// Equal timestamps consume no wall-clock coverage but still prove blocks
	// are absent, which is why completeness is separate from coverage.
	equal := []model.SeriesPoint{point(start)}
	at := sql.NullTime{Time: start.Add(20 * time.Second), Valid: true}
	newMissingTimeline([]db.MissingRange{{From: 1, To: 2, PredecessorAt: at, SuccessorAt: at}}).apply(equal, []uint64{3}, time.Minute)
	if equal[0].Completeness != model.SeriesPartial || equal[0].Coverage == nil || *equal[0].Coverage != 1 {
		t.Fatalf("equal-time range: %+v", equal[0])
	}

	points := []model.SeriesPoint{point(start), point(start.Add(time.Minute))}
	newMissingTimeline([]db.MissingRange{{
		From: 1, To: 10, Cursor: 6,
		PredecessorAt: sql.NullTime{Time: start.Add(10 * time.Second), Valid: true},
		CursorAt:      sql.NullTime{Time: start.Add(70 * time.Second), Valid: true},
		SuccessorAt:   sql.NullTime{Time: start.Add(90 * time.Second), Valid: true},
	}}).apply(points, []uint64{20, 30}, time.Minute)
	if points[0].Completeness != model.SeriesComplete || points[1].Completeness != model.SeriesPartial || points[1].Coverage == nil || *points[1].Coverage < 0.666 || *points[1].Coverage > 0.667 {
		t.Fatalf("advanced cursor must qualify only the remaining suffix: %+v", points)
	}

	done := []model.SeriesPoint{point(start)}
	newMissingTimeline([]db.MissingRange{{From: 1, To: 2, Cursor: 3}}).apply(done, []uint64{3}, time.Minute)
	if done[0].Completeness != model.SeriesComplete || done[0].Coverage == nil || *done[0].Coverage != 1 {
		t.Fatalf("completed range: %+v", done[0])
	}
}

// TestMissingTimelineLocatesRangesWithoutTimestamps is the regression for prehistory blanking a whole
// chain. A public endpoint serves no state below its retention, so the collector records everything it
// cannot reconstruct as one range with no timestamps beside it. Placed by block number that range only
// reaches the points that can hold its blocks, instead of leaving every point of every range unknown.
// The watermarks are the highest block each point indexed, so an active range always falls between two
// of them: no point can claim a block the ledger says is missing.
func TestMissingTimelineLocatesRangesWithoutTimestamps(t *testing.T) {
	start := now.Add(-3 * time.Minute)
	points := func(n int) []model.SeriesPoint {
		out := make([]model.SeriesPoint, n)
		for i := range out {
			coverage := 1.0
			out[i] = model.SeriesPoint{
				T: start.Add(time.Duration(i) * time.Minute).Unix(), GasUsed: 60, GasPerSecond: 1,
				Coverage: &coverage, Completeness: model.SeriesComplete,
			}
		}
		return out
	}
	states := func(ps []model.SeriesPoint) []string {
		out := make([]string, len(ps))
		for i, p := range ps {
			out[i] = p.Completeness
			if p.Coverage == nil {
				out[i] += "/null"
			}
		}
		return out
	}
	complete, unknown := model.SeriesComplete, model.SeriesUnknown+"/null"
	locate := func(ps []model.SeriesPoint, numbers []uint64, r db.MissingRange) []string {
		newMissingTimeline([]db.MissingRange{r}).apply(ps, numbers, time.Minute)
		return states(ps)
	}

	// Everything below the first watermark: only the point that could hold those blocks is affected.
	if got := locate(points(3), []uint64{1_000, 1_100, 1_200}, db.MissingRange{From: 1, To: 900}); got[0] != unknown || got[1] != complete || got[2] != complete {
		t.Fatalf("prehistory below the window: %v", got)
	}

	// A gap between two watermarks reaches both points that straddle it. The blocks after the first
	// point's own last block still carry its timestamps, so that point is missing them too.
	if got := locate(points(3), []uint64{100, 300, 400}, db.MissingRange{From: 101, To: 299}); got[0] != unknown || got[1] != unknown || got[2] != complete {
		t.Fatalf("gap between two watermarks: %v", got)
	}

	// A gap above every watermark is the collector falling behind inside the last point, not a range
	// clear of the window: its blocks belong to the minute the last indexed block was in.
	if got := locate(points(3), []uint64{100, 200, 300}, db.MissingRange{From: 301, To: 400}); got[0] != complete || got[1] != complete || got[2] != unknown {
		t.Fatalf("gap above every watermark: %v", got)
	}

	// A single recorded timestamp does not bound the other end of the range; its blocks still do.
	half := db.MissingRange{From: 101, To: 299, SuccessorAt: sql.NullTime{Time: start.Add(90 * time.Second), Valid: true}}
	if got := locate(points(3), []uint64{100, 300, 400}, half); got[0] != unknown || got[1] != unknown || got[2] != complete {
		t.Fatalf("range with one timestamp: %v", got)
	}

	// A watermark inside the range contradicts the ledger, which says that block is absent. The run
	// holds the points that claim it rather than collapsing to nothing.
	if got := locate(points(3), []uint64{100, 200, 300}, db.MissingRange{From: 150, To: 250}); got[0] != unknown || got[1] != unknown || got[2] != unknown {
		t.Fatalf("watermark inside the range: %v", got)
	}

	// An advanced cursor narrows the range to the suffix that is still missing.
	filled := db.MissingRange{From: 101, To: 299, Cursor: 250}
	if got := locate(points(3), []uint64{100, 200, 300}, filled); got[0] != complete || got[1] != unknown || got[2] != unknown {
		t.Fatalf("range with an advanced cursor: %v", got)
	}

	// Points the caller could not put a watermark on cannot be placed against, and neither can an order
	// the search cannot rely on, so every point is uncertain instead.
	for name, numbers := range map[string][]uint64{
		"none":       nil,
		"short":      {1_000, 1_100},
		"descending": {1_200, 1_100, 1_000},
	} {
		if got := locate(points(3), numbers, db.MissingRange{From: 1, To: 900}); got[0] != unknown || got[1] != unknown || got[2] != unknown {
			t.Fatalf("watermarks %s: %v", name, got)
		}
	}

	// A bucket stored before last_block was written has no watermark, and no chain indexes block 0, so
	// a zero withdraws the whole set rather than placing a range against a bucket that holds genesis.
	if got := bucketWatermarks([]uint64{1_000, 0, 1_200}); got != nil {
		t.Fatalf("buckets with an unrecorded watermark: %v", got)
	}
	if got := bucketWatermarks([]uint64{1_000, 1_100}); len(got) != 2 || got[1] != 1_100 {
		t.Fatalf("buckets with every watermark recorded: %v", got)
	}
}

// TestMissingTimelineLocatesRangesAmongBlockPoints places an undated range among per-block points and
// among stepped-down ones, where the watermark is the block itself and the highest block of the step.
func TestMissingTimelineLocatesRangesAmongBlockPoints(t *testing.T) {
	base := now.Truncate(time.Second).Add(-time.Minute)
	block := func(number uint64, at time.Time) db.Block {
		return db.Block{
			Number: number, TS: at, GasUsed: 100, BaseFee: db.WeiFromUint64(1),
			PredictedBaseFee: db.NullWeiFromUint64(1), MinBaseFee: db.NullWeiFromUint64(1), PricingVersion: db.PricingFull,
		}
	}
	blocks := []db.Block{block(98, base), block(99, base.Add(time.Second)), block(110, base.Add(20*time.Second))}
	gap := []db.MissingRange{{From: 100, To: 109, Lifecycle: model.MissingRangeBlocked}}

	// A per-block point covers itself, so the range only changes completeness and never a rate.
	points := blockPoints(blocks, nil)
	newMissingTimeline(gap).mark(points, blockNumbers(blocks))
	if p := points[0]; p.Completeness != model.SeriesComplete {
		t.Fatalf("block below the range: %+v", p)
	}
	for _, p := range points[1:] {
		if p.Completeness != model.SeriesUnknown || p.Coverage == nil || *p.Coverage != 1 || p.GasPerSecond != 100 {
			t.Fatalf("block beside the range: %+v", p)
		}
	}

	// Blocks sharing a second are one window: each of their rates is the gas of every block in it, so
	// the range reaches the siblings of the two blocks its own numbers sit between, not those alone.
	shared := []db.Block{block(98, base), block(99, base), block(110, base), block(111, base)}
	siblings := blockPoints(shared, nil)
	newMissingTimeline(gap).mark(siblings, blockNumbers(shared))
	for i, p := range siblings {
		if p.Completeness != model.SeriesUnknown {
			t.Fatalf("block %d sharing the second the range falls in: %+v", i, p)
		}
	}

	// Stepping down carries the highest block of each step, so the range lands on the same two steps.
	steps, numbers, _ := stepDown(blocks, nil, now)
	if len(steps) != 2 || numbers[0] != 99 || numbers[1] != 110 {
		t.Fatalf("step watermarks: %+v %v", steps, numbers)
	}
	newMissingTimeline(gap).apply(steps, numbers, stepDownWidth)
	for i, p := range steps {
		if p.Completeness != model.SeriesUnknown || p.Coverage != nil {
			t.Fatalf("step %d beside the range: %+v", i, p)
		}
	}
}

// TestSeriesPrehistoryLeavesLaterBucketsComplete is the same regression through the endpoint: the
// bucket the unreconstructable prehistory reaches is unknown, the ones above it stay whole.
func TestSeriesPrehistoryLeavesLaterBucketsComplete(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	start := now.Add(-2 * time.Minute)
	bucket := func(at time.Time, last uint64) db.Bucket {
		return db.Bucket{
			ChainID: robinhood, Resolution: db.Resolution1m, BucketStart: at, Blocks: 3, GasUsed: 300,
			PosterGas: sql.NullInt64{Int64: 50, Valid: true}, FeesWei: db.WeiFromUint64(90), PosterFeesWei: db.NullWeiFromUint64(50),
			BaseFeeMin: db.WeiFromUint64(1), BaseFeeAvg: db.WeiFromUint64(1), BaseFeeMax: db.WeiFromUint64(1),
			BacklogsEnd: db.Uint64Array{}, BacklogsMax: db.Uint64Array{}, PricingVersion: db.PricingFull, LastBlock: last,
		}
	}
	if err := store.FoldBuckets(ctx, []db.Bucket{bucket(start, 1_000), bucket(start.Add(time.Minute), 1_100)}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMissingRanges(ctx, robinhood, []db.MissingRange{{
		ChainID: robinhood, From: 0, To: 900, Lifecycle: model.MissingRangeBlocked, Reason: "no_state",
	}}); err != nil {
		t.Fatal(err)
	}
	ts := newServer(t, store)
	resp, body := get(t, ts, "/api/v1/networks/robinhood/series?range=24h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out model.Series
	decode(t, body, &out)
	if len(out.Points) != 2 {
		t.Fatalf("points: %+v", out.Points)
	}
	if p := out.Points[0]; p.Completeness != model.SeriesUnknown || p.Coverage != nil {
		t.Fatalf("bucket the prehistory reaches: %+v", p)
	}
	if p := out.Points[1]; p.Completeness != model.SeriesComplete || p.Coverage == nil || *p.Coverage != 1 {
		t.Fatalf("bucket above the prehistory: %+v", p)
	}
}

// TestMissingTimelineKeepsBlockPointRates is the regression for applying
// bucket coverage arithmetic to per-block points. The block before a gap is
// itself indexed, so it keeps a whole coverage and the gas rate of its second
// while reporting that the series around it is not whole.
func TestMissingTimelineKeepsBlockPointRates(t *testing.T) {
	base := now.Truncate(time.Second)
	block := func(number uint64, at time.Time, gas uint64) db.Block {
		return db.Block{
			Number: number, TS: at, GasUsed: gas, BaseFee: db.WeiFromUint64(1),
			PredictedBaseFee: db.NullWeiFromUint64(1), MinBaseFee: db.NullWeiFromUint64(1), PricingVersion: db.PricingFull,
		}
	}
	points := blockPoints([]db.Block{
		block(98, base, 100), block(99, base, 100), block(110, base.Add(5*time.Second), 200),
	}, nil)
	newMissingTimeline([]db.MissingRange{{
		From: 100, To: 109, Lifecycle: model.MissingRangePending,
		PredecessorAt: sql.NullTime{Time: base, Valid: true},
		SuccessorAt:   sql.NullTime{Time: base.Add(5 * time.Second), Valid: true},
	}}).mark(points, []uint64{98, 99, 110})
	for i, p := range points[:2] {
		// gasPerSecond stays the gas of every block in the second, not the
		// point's own gas divided by a gap-shortened divisor.
		if p.Completeness != model.SeriesPartial || p.Coverage == nil || *p.Coverage != 1 || p.GasPerSecond != 200 {
			t.Fatalf("block %d beside a gap: %+v", i, p)
		}
	}
	if p := points[2]; p.Completeness != model.SeriesPartial || p.Coverage == nil || *p.Coverage != 1 || p.GasPerSecond != 200 {
		t.Fatalf("block after a gap: %+v", p)
	}

	away := blockPoints([]db.Block{block(200, base.Add(time.Minute), 300)}, nil)
	newMissingTimeline([]db.MissingRange{{
		From: 100, To: 109, Lifecycle: model.MissingRangePending,
		PredecessorAt: sql.NullTime{Time: base, Valid: true},
		SuccessorAt:   sql.NullTime{Time: base.Add(5 * time.Second), Valid: true},
	}}).mark(away, []uint64{200})
	if p := away[0]; p.Completeness != model.SeriesComplete || p.Coverage == nil || *p.Coverage != 1 || p.GasPerSecond != 300 {
		t.Fatalf("block clear of every gap: %+v", p)
	}
}

// TestBatchesOneHourBounds: the per-report batch resolution reports the
// window it was asked for, exactly as the grouped resolutions do, whether
// or not any report falls inside it.
func TestBatchesOneHourBounds(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ts := newServer(t, store)
	batches := func(t *testing.T) model.BatchSeries {
		t.Helper()
		resp, body := get(t, ts, "/api/v1/networks/robinhood/batches?range=1h")
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, body)
		}
		var out model.BatchSeries
		decode(t, body, &out)
		return out
	}
	from, to := now.Add(-time.Hour).Unix(), now.Add(time.Second).Unix()
	if s := batches(t); len(s.Points) != 0 || s.From != from || s.To != to {
		t.Fatalf("empty one hour window: %d..%d %+v", s.From, s.To, s.Points)
	}
	if err := store.UpsertBatchReports(ctx, []db.BatchReport{{ChainID: robinhood, BlockNumber: 10, BatchNumber: 1, BatchTS: now.Add(-time.Minute),
		Poster: "0x1", CalldataLen: 8, L1BaseFee: db.WeiFromUint64(9), GasSpent: 3, WeiSpent: db.WeiFromUint64(77), CostCalculationVersion: 1}}); err != nil {
		t.Fatal(err)
	}
	if s := batches(t); len(s.Points) != 1 || s.From != from || s.To != to {
		t.Fatalf("one hour window with a report: %d..%d %+v", s.From, s.To, s.Points)
	}
}

// A network switched off in the collector's configuration keeps its rows but leaves the public api:
// the list drops it and every route below /networks answers as it does for a name it never knew,
// so nothing on the site offers a chain whose history has stopped moving.
func TestDisabledNetworkLeavesThePublicAPI(t *testing.T) {
	ctx := context.Background()
	store := seed(t)
	ts := newServer(t, store)

	for _, path := range []string{"/api/v1/networks/arbitrum-one", "/api/v1/networks/42161", "/api/v1/networks/arbitrum-one/live", "/api/v1/networks/arbitrum-one/series?range=24h"} {
		if resp, body := get(t, ts, path); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s = %d: %s", path, resp.StatusCode, body)
		}
	}
	resp, body := get(t, ts, "/api/v1/networks")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("networks = %d: %s", resp.StatusCode, body)
	}
	var nets []model.Network
	decode(t, body, &nets)
	for _, n := range nets {
		if n.Name == "arbitrum-one" {
			t.Fatalf("disabled network listed: %+v", nets)
		}
	}

	row, err := store.NetworkByRef(ctx, "arbitrum-one")
	if err != nil || row == nil {
		t.Fatalf("arbitrum-one row: %+v %v", row, err)
	}
	row.Enabled = true
	if err := store.UpsertNetwork(ctx, *row); err != nil {
		t.Fatal(err)
	}
	resp, body = get(t, ts, "/api/v1/networks")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("networks after enabling = %d: %s", resp.StatusCode, body)
	}
	decode(t, body, &nets)
	if len(nets) != 3 || nets[1].Name != "arbitrum-one" || nets[1].Model != model.ModelUnknown || nets[1].HeadAt != nil || nets[1].LagSeconds != nil {
		t.Fatalf("networks after enabling: %+v", nets)
	}
	// Timestamps of a network without a head are JSON null, never "".
	if !strings.Contains(string(body), `"headAt":null`) || !strings.Contains(string(body), `"lagSeconds":null`) {
		t.Fatalf("null timestamps expected: %s", body)
	}
	if resp, body := get(t, ts, "/api/v1/networks/arbitrum-one"); resp.StatusCode != http.StatusOK {
		t.Fatalf("network after enabling = %d: %s", resp.StatusCode, body)
	}
}

// TestLoopReasonPhase: a loop that announced a long step is reported as
// being in it. The network is still incomplete, so still degraded, but an
// operator can tell a first owner scan that legitimately takes twenty
// minutes from a loop that is wedged or failing.
func TestLoopReasonPhase(t *testing.T) {
	now := time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour).Format(time.RFC3339Nano)
	fresh := now.Add(-time.Second).Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name string
		loop model.LoopStatus
		want string
	}{
		{"never succeeded", model.LoopStatus{}, "slow loop has not succeeded"},
		{"first pass", model.LoopStatus{Phase: "scanning owner actions, 3 of 9 blocks"},
			"slow loop on its first pass: scanning owner actions, 3 of 9 blocks"},
		{"failing wins over a phase", model.LoopStatus{LastSuccessAt: &fresh, ErrorStreak: degradedErrorStreak, Phase: "scanning"},
			"slow loop failing"},
		{"stale", model.LoopStatus{LastSuccessAt: &at, StaleAfterSecs: 60}, "slow loop stale"},
		{"busy", model.LoopStatus{LastSuccessAt: &at, StaleAfterSecs: 60, Phase: "scanning"}, "slow loop busy: scanning"},
		{"healthy", model.LoopStatus{LastSuccessAt: &fresh, StaleAfterSecs: 60}, ""},
	} {
		if got := loopReason("slow", tc.loop, now); got != tc.want {
			t.Fatalf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The load band: every bucketed resolution reports the spread of the unit one step finer, the
// per-block one reports none, and the unit the clock is inside is never one of them.
func TestSeriesComputeRateSpread(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	block := func(n uint64, ts time.Time, gas uint64, poster int64) db.Block {
		return db.Block{ChainID: robinhood, Number: n, TS: ts, GasUsed: gas, PosterGas: sql.NullInt64{Int64: poster, Valid: true},
			BaseFee: db.WeiFromUint64(100), PredictedBaseFee: db.NullWeiFromUint64(100), Backlogs: db.Uint64Array{1},
			ConstraintBips: pq.Int64Array{1}, MinBaseFee: db.NullWeiFromUint64(50), PricingVersion: db.PricingFull}
	}
	// Enough blocks for the hour to step down to 5s, with one busy second in every step.
	var blocks []db.Block
	for i := uint64(0); i < 2500; i++ {
		at := now.Add(-time.Duration(2500-i) * 100 * time.Millisecond).Truncate(time.Second)
		gas := uint64(10)
		if i%50 == 0 {
			gas = 400
		}
		blocks = append(blocks, block(i, at, gas, 0))
	}
	if err := store.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	hour, err := buildSeriesIn(ctx, store, robinhood, ranges[rangeHour], now)
	if err != nil {
		t.Fatal(err)
	}
	if hour.Resolution != "5s" || hour.SpreadSeconds == nil || *hour.SpreadSeconds != 1 {
		t.Fatalf("stepped spread unit: %s %v", hour.Resolution, hour.SpreadSeconds)
	}
	step := hour.Points[1]
	if step.ComputeGasPerSecondMin == nil || step.ComputeGasPerSecondMax == nil || *step.ComputeGasPerSecondMin >= *step.ComputeGasPerSecondMax {
		t.Fatalf("stepped band: %+v", step)
	}
	if *step.ComputeGasPerSecond < *step.ComputeGasPerSecondMin || *step.ComputeGasPerSecond > *step.ComputeGasPerSecondMax {
		t.Fatalf("the average must sit inside the band: %+v", step)
	}
	// A clock two seconds into the newest step leaves it filling: its average is over the elapsed
	// span while its band is over the whole seconds of it, two different populations, so it reports
	// no band at all rather than one its own average can sit outside.
	if err := store.UpsertBlocks(ctx, []db.Block{block(2500, now, 10, 0)}); err != nil {
		t.Fatal(err)
	}
	midStep, err := buildSeriesIn(ctx, store, robinhood, ranges[rangeHour], now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	filling := midStep.Points[len(midStep.Points)-1]
	if filling.Completeness == model.SeriesComplete || filling.ComputeGasPerSecondMin != nil || filling.ComputeGasPerSecondMax != nil {
		t.Fatalf("the bucket in progress carries no band: %+v", filling)
	}
	// A block whose poster gas was never recorded voids the step it lands in, and only that one.
	banded := func(s *model.Series) int {
		n := 0
		for _, p := range s.Points {
			if p.ComputeGasPerSecondMin != nil {
				n++
			}
		}
		return n
	}
	before := banded(hour)
	voided := blocks[60]
	voided.PosterGas = sql.NullInt64{}
	if err := store.UpsertBlocks(ctx, []db.Block{voided}); err != nil {
		t.Fatal(err)
	}
	if hour, err = buildSeriesIn(ctx, store, robinhood, ranges[rangeHour], now); err != nil {
		t.Fatal(err)
	}
	if banded(hour) != before-1 {
		t.Fatalf("one unrated second voided %d steps", before-banded(hour))
	}

	// Under the step-down threshold the points are the blocks themselves, which have no interior.
	few := dbtest.New()
	if err := few.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := few.UpsertBlocks(ctx, blocks[:10]); err != nil {
		t.Fatal(err)
	}
	perBlock, err := buildSeriesIn(ctx, few, robinhood, ranges[rangeHour], now)
	if err != nil {
		t.Fatal(err)
	}
	if perBlock.Resolution != resolutionBlock || perBlock.SpreadSeconds != nil || perBlock.Points[0].ComputeGasPerSecondMax != nil {
		t.Fatalf("per-block points carry no band: %s %v %+v", perBlock.Resolution, perBlock.SpreadSeconds, perBlock.Points[0])
	}

	// Two quarter hours read their spread from the minutes inside them: one with every minute
	// stored, one with three, whose twelve idle minutes carried nothing.
	minute := func(start time.Time, compute uint64, resolution string) db.Bucket {
		return db.Bucket{
			ChainID: robinhood, Resolution: resolution, BucketStart: start, Blocks: 1, GasUsed: compute + 7,
			PosterGas: sql.NullInt64{Int64: 7, Valid: true}, FeesWei: db.WeiFromUint64(1), BaseFeeMin: db.WeiFromUint64(1),
			BaseFeeAvg: db.WeiFromUint64(1), BaseFeeMax: db.WeiFromUint64(1), LastBlock: uint64(9000 + start.Unix()), PricingVersion: db.PricingFull,
		}
	}
	whole := now.Add(-time.Hour).Truncate(15 * time.Minute)
	sparse := whole.Add(15 * time.Minute)
	rows := make([]db.Bucket, 0, 20)
	for i := range 15 {
		compute := uint64(600)
		switch i {
		case 3:
			compute = 120
		case 7:
			compute = 6_000
		}
		rows = append(rows, minute(whole.Add(time.Duration(i)*time.Minute), compute, db.Resolution1m))
	}
	for i, compute := range []uint64{600, 120, 6_000} {
		rows = append(rows, minute(sparse.Add(time.Duration(i)*time.Minute), compute, db.Resolution1m))
	}
	rows = append(rows, minute(whole, 0, db.Resolution15m), minute(sparse, 0, db.Resolution15m))
	if err := store.FoldBuckets(ctx, rows); err != nil {
		t.Fatal(err)
	}
	month, err := buildSeriesIn(ctx, store, robinhood, ranges[rangeMonth], now)
	if err != nil {
		t.Fatal(err)
	}
	if month.SpreadSeconds == nil || *month.SpreadSeconds != 60 {
		t.Fatalf("month spread unit: %v", month.SpreadSeconds)
	}
	if len(month.Points) != 2 {
		t.Fatalf("quarter hours: %+v", month.Points)
	}
	if *month.Points[0].ComputeGasPerSecondMin != 2 || *month.Points[0].ComputeGasPerSecondMax != 100 {
		t.Fatalf("a quarter hour with every minute stored bands over what it measured: %+v", month.Points[0])
	}
	if *month.Points[1].ComputeGasPerSecondMin != 0 || *month.Points[1].ComputeGasPerSecondMax != 100 {
		t.Fatalf("an idle minute inside a whole quarter hour carried nothing: %+v", month.Points[1])
	}
}

// A second with no blocks at all is not an unmeasured one: it carried nothing, and a band that
// leaves it out reports a minimum above the bucket's own average.
func TestSeriesSpreadCountsIdleUnits(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Four blocks in the first second of every five, so each step of the hour holds one measured
	// second and four idle ones, and enough of them for the range to step down.
	base := now.Add(-time.Hour).Truncate(stepDownWidth)
	var blocks []db.Block
	for i := uint64(0); i < 2500; i++ {
		at := base.Add(time.Duration(i/4) * stepDownWidth)
		blocks = append(blocks, db.Block{
			ChainID: robinhood, Number: i, TS: at, GasUsed: 100, PosterGas: sql.NullInt64{Valid: true},
			BaseFee: db.WeiFromUint64(100), PredictedBaseFee: db.NullWeiFromUint64(100), Backlogs: db.Uint64Array{1},
			ConstraintBips: pq.Int64Array{1}, MinBaseFee: db.NullWeiFromUint64(50), PricingVersion: db.PricingFull,
		})
	}
	if err := store.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	hour, err := buildSeriesIn(ctx, store, robinhood, ranges[rangeHour], now)
	if err != nil {
		t.Fatal(err)
	}
	if hour.Resolution != resolutionStepped {
		t.Fatalf("resolution %q", hour.Resolution)
	}
	p := hour.Points[0]
	if p.Completeness != model.SeriesComplete || p.ComputeGasPerSecondMin == nil || *p.ComputeGasPerSecondMin != 0 || *p.ComputeGasPerSecondMax != 400 {
		t.Fatalf("four idle seconds floor the step's band: %+v", p)
	}
	// The average of a step is a mean over its whole span, so it can never leave its own band.
	for _, p := range hour.Points {
		if p.ComputeGasPerSecondMin == nil || p.ComputeGasPerSecond == nil {
			continue
		}
		if *p.ComputeGasPerSecond < *p.ComputeGasPerSecondMin || *p.ComputeGasPerSecond > *p.ComputeGasPerSecondMax {
			t.Fatalf("average outside its band: %+v", p)
		}
	}
}

// Block rows are pruned long before the buckets standing on them. A window the prune boundary falls
// inside holds seconds that are gone, not seconds that were idle, so it reports no band.
func TestSeriesSpreadStopsAtTheOldestRetainedRow(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// The oldest row lands two seconds into its step, so that step has three of its five seconds
	// behind the retention boundary.
	base := now.Add(-time.Hour).Truncate(stepDownWidth).Add(2 * time.Second)
	var blocks []db.Block
	for i := uint64(0); i < 2500; i++ {
		blocks = append(blocks, db.Block{
			ChainID: robinhood, Number: i, TS: base.Add(time.Duration(i) * time.Second), GasUsed: 100,
			PosterGas: sql.NullInt64{Valid: true}, BaseFee: db.WeiFromUint64(100), PredictedBaseFee: db.NullWeiFromUint64(100),
			Backlogs: db.Uint64Array{1}, ConstraintBips: pq.Int64Array{1}, MinBaseFee: db.NullWeiFromUint64(50), PricingVersion: db.PricingFull,
		})
	}
	if err := store.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	hour, err := buildSeriesIn(ctx, store, robinhood, ranges[rangeHour], now)
	if err != nil {
		t.Fatal(err)
	}
	if hour.Points[0].ComputeGasPerSecondMin != nil {
		t.Fatalf("the step the retention boundary falls inside carries no band: %+v", hour.Points[0])
	}
	if hour.Points[1].ComputeGasPerSecondMin == nil {
		t.Fatalf("a step wholly inside the retained rows keeps its band: %+v", hour.Points[1])
	}
}

// A step reaches back to its own five second boundary. For a window that does not start on one the
// seconds before it were never asked for, so that step is not one whose gaps are idle seconds.
func TestSeriesSpreadStopsAtTheWindowEdge(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	if err := store.UpsertNetwork(ctx, db.Network{ChainID: robinhood, Name: "robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var blocks []db.Block
	for i := uint64(0); i < 2500; i++ {
		blocks = append(blocks, db.Block{
			ChainID: robinhood, Number: i, TS: now.Add(-time.Hour).Add(time.Duration(i) * time.Second), GasUsed: 100,
			PosterGas: sql.NullInt64{Valid: true}, BaseFee: db.WeiFromUint64(100), PredictedBaseFee: db.NullWeiFromUint64(100),
			Backlogs: db.Uint64Array{1}, ConstraintBips: pq.Int64Array{1}, MinBaseFee: db.NullWeiFromUint64(50), PricingVersion: db.PricingFull,
		})
	}
	if err := store.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	// A clock three seconds off the five second grid puts the window's start inside a step.
	hour, err := buildSeriesIn(ctx, store, robinhood, ranges[rangeHour], now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if hour.Points[0].T >= hour.From {
		t.Fatalf("the first step is expected to reach back past the window: %d, window from %d", hour.Points[0].T, hour.From)
	}
	if hour.Points[0].ComputeGasPerSecondMin != nil {
		t.Fatalf("the step the window starts inside carries no band: %+v", hour.Points[0])
	}
	if hour.Points[1].ComputeGasPerSecondMin == nil {
		t.Fatalf("a step wholly inside the window keeps its band: %+v", hour.Points[1])
	}
}
