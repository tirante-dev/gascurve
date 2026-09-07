package metrics

import (
	"strings"
	"testing"
	"time"
)

var (
	sampledAt = time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)
	headAt    = sampledAt.Add(-4 * time.Second)
)

func TestCollectorNetworkIsStable(t *testing.T) {
	reg := NewRegistry()
	c := NewCollector(reg)
	first := c.Network("robinhood", 4663)
	if second := c.Network("robinhood", 4663); second != first {
		t.Fatal("a chain id must always get the same instruments")
	}
	if other := c.Network("arbitrum-one", 42161); other == first {
		t.Fatal("two networks must not share instruments")
	}
}

func TestCollectorReportsEveryNetworkSeries(t *testing.T) {
	reg := NewRegistry()
	n := NewCollector(reg).Network("robinhood", 4663)

	n.ObserveHead(1000, headAt, sampledAt, sampledAt)
	n.ObserveTick(250 * time.Millisecond)
	n.GapSkipped()
	n.GapSkipped()
	n.HoleFilled()
	n.ObserveHoles(HolesState{Pending: 2, Blocks: 512, Unfillable: 1})
	n.ObserveBackfill(BackfillState{Cursor: 900, Floor: 100, Remaining: 400})
	n.ObservePool(PoolState{
		Active: 1, Failovers: 3, RateLimitEvents: 7, FastCalls: 12, BulkCalls: 40,
		Endpoints: []EndpointState{
			{Index: 0, Disabled: true, RateLimitEvents: 5},
			{Index: 1, WSCooling: true, RateLimitEvents: 2},
		},
	})

	body := scrape(t, reg)
	const labels = `{chain_id="4663",network="robinhood"}`
	for _, want := range []string{
		"gascurve_collector_head_block" + labels + " 1000",
		"gascurve_collector_head_lag_seconds" + labels + " 4",
		"gascurve_collector_last_sample_timestamp_seconds" + labels + " 1.788678e+09",
		"gascurve_collector_tick_duration_seconds_count" + labels + " 1",
		"gascurve_collector_catch_up_gaps_skipped_total" + labels + " 2",
		"gascurve_collector_holes_filled_total" + labels + " 1",
		"gascurve_collector_holes_pending" + labels + " 2",
		"gascurve_collector_holes_blocks" + labels + " 512",
		"gascurve_collector_holes_unfillable" + labels + " 1",
		"gascurve_collector_backfill_cursor_block" + labels + " 900",
		"gascurve_collector_backfill_floor_block" + labels + " 100",
		"gascurve_collector_backfill_blocks_remaining" + labels + " 400",
		"gascurve_collector_backfill_done" + labels + " 0",
		"gascurve_collector_active_endpoint" + labels + " 1",
		"gascurve_collector_endpoint_failovers_total" + labels + " 3",
		"gascurve_collector_rate_limit_events_total" + labels + " 7",
		`gascurve_collector_rpc_calls_total{chain_id="4663",class="fast",network="robinhood"} 12`,
		`gascurve_collector_rpc_calls_total{chain_id="4663",class="bulk",network="robinhood"} 40`,
		`gascurve_collector_endpoint_disabled{chain_id="4663",endpoint="0",network="robinhood"} 1`,
		`gascurve_collector_endpoint_disabled{chain_id="4663",endpoint="1",network="robinhood"} 0`,
		`gascurve_collector_endpoint_ws_cooling{chain_id="4663",endpoint="0",network="robinhood"} 0`,
		`gascurve_collector_endpoint_ws_cooling{chain_id="4663",endpoint="1",network="robinhood"} 1`,
		`gascurve_collector_endpoint_rate_limit_events_total{chain_id="4663",endpoint="0",network="robinhood"} 5`,
		`gascurve_collector_endpoint_rate_limit_events_total{chain_id="4663",endpoint="1",network="robinhood"} 2`,
	} {
		hasSeries(t, body, want)
	}
	// No label anywhere may look like a URL: endpoints are indexes.
	if strings.Contains(body, "http") {
		t.Fatalf("an endpoint URL reached the exposition text:\n%s", body)
	}
}

func TestCollectorHeadLagNeverGoesNegative(t *testing.T) {
	reg := NewRegistry()
	n := NewCollector(reg).Network("robinhood", 4663)
	// A node whose clock is ahead of ours reports a block from the future.
	n.ObserveHead(1, sampledAt.Add(5*time.Second), sampledAt, sampledAt)
	hasSeries(t, scrape(t, reg), `gascurve_collector_head_lag_seconds{chain_id="4663",network="robinhood"} 0`)
}

func TestCollectorPoolCountersAreCumulative(t *testing.T) {
	reg := NewRegistry()
	n := NewCollector(reg).Network("robinhood", 4663)
	n.ObservePool(PoolState{RateLimitEvents: 4, FastCalls: 1, BulkCalls: 2, Failovers: 1,
		Endpoints: []EndpointState{{Index: 0, RateLimitEvents: 4}}})
	n.ObservePool(PoolState{RateLimitEvents: 9, FastCalls: 6, BulkCalls: 8, Failovers: 2,
		Endpoints: []EndpointState{{Index: 0, RateLimitEvents: 9}}})
	body := scrape(t, reg)
	const labels = `{chain_id="4663",network="robinhood"}`
	for _, want := range []string{
		"gascurve_collector_rate_limit_events_total" + labels + " 9",
		"gascurve_collector_endpoint_failovers_total" + labels + " 2",
		`gascurve_collector_rpc_calls_total{chain_id="4663",class="fast",network="robinhood"} 6`,
		`gascurve_collector_rpc_calls_total{chain_id="4663",class="bulk",network="robinhood"} 8`,
		`gascurve_collector_endpoint_rate_limit_events_total{chain_id="4663",endpoint="0",network="robinhood"} 9`,
	} {
		hasSeries(t, body, want)
	}
}

func TestCollectorPoolWithoutEndpoints(t *testing.T) {
	reg := NewRegistry()
	n := NewCollector(reg).Network("robinhood", 4663)
	// A plain client has no routing state: the aggregate counters are all
	// there is, and no endpoint series is created for it.
	n.ObservePool(PoolState{RateLimitEvents: 2})
	body := scrape(t, reg)
	hasSeries(t, body, `gascurve_collector_rate_limit_events_total{chain_id="4663",network="robinhood"} 2`)
	lacksSeries(t, body, "gascurve_collector_endpoint_disabled")
}

func TestCollectorBackfillDone(t *testing.T) {
	reg := NewRegistry()
	n := NewCollector(reg).Network("robinhood", 4663)
	n.ObserveBackfill(BackfillState{Cursor: 100, Floor: 100, Done: true})
	hasSeries(t, scrape(t, reg), `gascurve_collector_backfill_done{chain_id="4663",network="robinhood"} 1`)
}
