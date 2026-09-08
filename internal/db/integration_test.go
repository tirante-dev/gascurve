//go:build integration

package db

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/tirante-dev/gascurve/internal/logger"
)

// Chain ids no collector uses, so a demo running against the same database
// cannot interleave rows with the test's.
const (
	testChain  = uint64(900004663)
	otherChain = uint64(900000007)
	thirdChain = uint64(900046630)
)

func openIntegration(t *testing.T) *Postgres {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set")
	}
	d, err := Open(url, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	p := NewPostgres(d)
	resetIntegrationSchema(t, p)
	return p
}

// resetIntegrationSchema drops the schema and migrates it to head.
func resetIntegrationSchema(t *testing.T, p *Postgres) {
	t.Helper()
	retryIntegrationReset(t, func() error { return ResetSchema(context.Background(), p.DB()) })
}

// emptyIntegrationSchema drops the schema and leaves it unmigrated, so a test
// can install an older version itself.
func emptyIntegrationSchema(t *testing.T, p *Postgres) {
	t.Helper()
	retryIntegrationReset(t, func() error {
		_, err := p.DB().ExecContext(context.Background(), "DROP SCHEMA public CASCADE; CREATE SCHEMA public;")
		return err
	})
}

func retryIntegrationReset(t *testing.T, reset func() error) {
	t.Helper()
	// A collector running against the database holds locks the reset has
	// to wait for and can deadlock with; retry a few times.
	var rerr error
	for attempt := 0; attempt < 5; attempt++ {
		if rerr = reset(); rerr == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if rerr != nil {
		t.Fatal(rerr)
	}
}

// testNetworks keeps the rows of the test's chains, in chain id order.
func testNetworks(nets []Network) []Network {
	var out []Network
	for _, n := range nets {
		if n.ChainID == testChain || n.ChainID == otherChain || n.ChainID == thirdChain {
			out = append(out, n)
		}
	}
	return out
}

func TestIntegrationMigratorFreshInstall(t *testing.T) {
	p := openIntegration(t)
	m, err := NewMigrator(p.DB().DB)
	if err != nil {
		t.Fatal(err)
	}
	v, dirty, err := m.Version()
	if err != nil || dirty || v < productionSchemaVersion {
		t.Fatalf("version = %d dirty=%v err=%v", v, dirty, err)
	}
	headVersion := v
	// The poster-gas migration preserves deployed history and marks its destination split unknown
	// until receipt-backed poster gas is recomputed. Roll back to just below it, however many
	// migrations have landed since, so it runs again over the rows inserted below.
	const posterGasVersion = 6
	ctx := context.Background()
	if err := m.Down(int(headVersion) - posterGasVersion + 1); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB().ExecContext(ctx, `INSERT INTO blocks
		(chain_id, number, ts, gas_used, base_fee, backlogs, constraint_bips, min_base_fee, pricing_version)
		VALUES (2, 1, now(), 10, 7, '{5}', '{3}', 4, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB().ExecContext(ctx, `INSERT INTO buckets
		(chain_id, resolution, bucket_start, blocks, gas_used, fees_wei, base_fee_avg, backlogs_end, backlogs_max,
		 constraint_bips_end, min_base_fee, floor_fees_wei, surplus_fees_wei, pricing_version)
		VALUES (2, '1m', now(), 1, 10, 70, 7, '{5}', '{5}', '{3}', 4, 40, 30, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	var poster sql.NullInt64
	var minFee, fees, floor, surplus, posterFees sql.NullString
	var pricing int16
	if err := p.DB().QueryRowContext(ctx, `SELECT poster_gas, min_base_fee, pricing_version FROM blocks WHERE chain_id = 2`).Scan(&poster, &minFee, &pricing); err != nil || poster.Valid || minFee.String != "4" || pricing != PricingFull {
		t.Fatalf("pre-poster block after migration: poster=%+v min=%+v pricing=%d err=%v", poster, minFee, pricing, err)
	}
	if err := p.DB().QueryRowContext(ctx, `SELECT poster_gas, fees_wei, floor_fees_wei, surplus_fees_wei, poster_fees_wei FROM buckets WHERE chain_id = 2`).Scan(&poster, &fees, &floor, &surplus, &posterFees); err != nil || poster.Valid || fees.String != "70" || floor.Valid || surplus.Valid || posterFees.Valid {
		t.Fatalf("pre-poster bucket after migration: poster=%+v fees=%+v floor=%+v surplus=%+v poster fees=%+v err=%v", poster, fees, floor, surplus, posterFees, err)
	}
	if _, err := p.DB().ExecContext(ctx, `INSERT INTO blocks (chain_id, number, ts, gas_used, base_fee, backlogs, pricing_version) VALUES (1, 1, now(), 0, 0, '{18446744073709551615,5}', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB().ExecContext(ctx, `INSERT INTO buckets (chain_id, resolution, bucket_start, blocks, base_fee_avg, backlogs_end, backlogs_max, fees_wei, pricing_version, constraint_bips_end)
		VALUES (1, '1m', now(), 3, 7, '{18446744073709551615}', '{18446744073709551614,1}', 900, 0, NULL)`); err != nil {
		t.Fatal(err)
	}
	// Backlogs are uint64 and saturate above the BIGINT range, so they are
	// stored as NUMERIC(20,0) arrays and come back whole.
	b, err := p.BlockByNumber(ctx, 1, 1)
	if err != nil || b == nil || b.Backlogs[0] != math.MaxUint64 || b.Backlogs[1] != 5 || b.Hash != "" {
		t.Fatalf("block: %+v %v", b, err)
	}
	// A block written without the breakdown carries no exponents and no
	// floor, and its fee split is therefore unknown.
	if b.ConstraintBips != nil || b.MinBaseFee.Valid || b.PricingVersion != PricingUnknown || b.Known() {
		t.Fatalf("unknown block history: %+v", b)
	}
	bk, err := p.Buckets(ctx, 1, Resolution1m, time.Unix(0, 0), time.Now().Add(time.Hour))
	if err != nil || len(bk) != 1 || bk[0].BacklogsEnd[0] != math.MaxUint64 || bk[0].BacklogsMax[0] != math.MaxUint64-1 || bk[0].BacklogsMax[1] != 1 {
		t.Fatalf("bucket: %+v %v", bk, err)
	}
	if bk[0].BaseFeeSum.Valid || bk[0].FloorFeesWei.Valid || bk[0].SurplusFeesWei.Valid || bk[0].MinBaseFee.Valid || bk[0].BaseFeeAvg.Int64() != 7 {
		t.Fatalf("an unknown bucket carries no sum and no split: %+v", bk[0])
	}
	// Folding into a bucket with an unknown sum keeps it unknown and
	// derives the average from the reconstruction: (7*3 + 13) / 4 = 8.
	if err := p.FoldBuckets(ctx, []Bucket{{ChainID: 1, Resolution: Resolution1m, BucketStart: bk[0].BucketStart, Blocks: 1, GasUsed: 1, FeesWei: WeiFromUint64(13),
		BaseFeeMin: WeiFromUint64(13), BaseFeeAvg: WeiFromUint64(13), BaseFeeMax: WeiFromUint64(13), BaseFeeSum: NullWeiFromUint64(13), BacklogsEnd: Uint64Array{1}, BacklogsMax: Uint64Array{1},
		ConstraintBipsEnd: pq.Int64Array{1}, MinBaseFee: NullWeiFromUint64(1), FloorFeesWei: NullWeiFromUint64(1), SurplusFeesWei: NullWeiFromUint64(12), LastBlock: 9}}); err != nil {
		t.Fatal(err)
	}
	bk, _ = p.Buckets(ctx, 1, Resolution1m, time.Unix(0, 0), time.Now().Add(time.Hour))
	if bk[0].Blocks != 4 || bk[0].BaseFeeAvg.Int64() != 8 || bk[0].BaseFeeSum.Valid || bk[0].FloorFeesWei.Valid || bk[0].SurplusFeesWei.Valid || bk[0].FeesWei.Int64() != 913 {
		t.Fatalf("fold into an unknown bucket: %+v", bk[0])
	}
	// Folding does not promote history whose pricing inputs remain unknown.
	one, _ := p.Buckets(ctx, 1, Resolution1m, time.Unix(0, 0), time.Now().Add(time.Hour))
	if len(one) != 1 || one[0].PricingVersion != PricingUnknown {
		t.Fatalf("folding must not re-authorize unknown history: %+v", one)
	}
	// Migration 2 has the exact index order needed by block-based sample
	// lookup and deletion. Later migrations add durable missing ranges, nullable
	// batch cost metadata, and receipt-backed poster-gas accounting.
	var indexDef string
	if err := p.DB().QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'state_samples_chain_block'`).Scan(&indexDef); err != nil {
		t.Fatal(err)
	}
	if want := "(chain_id, block_number DESC, sampled_at DESC)"; !strings.Contains(indexDef, want) {
		t.Fatalf("index definition = %q, want %q", indexDef, want)
	}
	if _, err := p.DB().ExecContext(ctx, `INSERT INTO state_samples (chain_id, sampled_at, block_number, base_fee, min_base_fee) VALUES (1, now(), 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB().ExecContext(ctx, `INSERT INTO missing_ranges (chain_id, from_block, to_block, detected_at, lifecycle, reason) VALUES (1, 10, 20, now(), 'pending', 'catch up limit')`); err != nil {
		t.Fatal(err)
	}
	if err := m.Down(0); err == nil {
		t.Fatal("Down(0) should fail")
	}
	// Rolling back the prediction alignment restores the NOT NULL column and puts each pricing group
	// back on the block whose replay produced it.
	if err := m.Down(1); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Version(); err != nil || v != headVersion-1 {
		t.Fatalf("after prediction-alignment down: %d %v", v, err)
	}
	var nullable string
	if err := p.DB().QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'blocks' AND column_name = 'predicted_base_fee'`).Scan(&nullable); err != nil || nullable != "NO" {
		t.Fatalf("predicted_base_fee must be NOT NULL again: %q %v", nullable, err)
	}
	// Rolling back the poster-gas migration removes only its columns and makes
	// the old two-way destination view unknown. Total user fees remain intact.
	if err := m.Down(1); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Version(); err != nil || v != headVersion-2 {
		t.Fatalf("after poster-gas down: %d %v", v, err)
	}
	var posterColumns int
	if err := p.DB().QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND ((table_name = 'blocks' AND column_name = 'poster_gas') OR (table_name = 'buckets' AND column_name IN ('poster_gas', 'poster_fees_wei')))`).Scan(&posterColumns); err != nil || posterColumns != 0 {
		t.Fatalf("poster-gas columns after down: %d %v", posterColumns, err)
	}
	if err := p.DB().QueryRowContext(ctx, `SELECT min_base_fee, pricing_version FROM blocks WHERE chain_id = 2`).Scan(&minFee, &pricing); err != nil || minFee.Valid || pricing != PricingUnknown {
		t.Fatalf("poster-gas down must make the old two-way view unknown: min=%+v pricing=%d err=%v", minFee, pricing, err)
	}
	if err := p.DB().QueryRowContext(ctx, `SELECT fees_wei, floor_fees_wei, surplus_fees_wei FROM buckets WHERE chain_id = 2`).Scan(&fees, &floor, &surplus); err != nil || fees.String != "70" || floor.Valid || surplus.Valid {
		t.Fatalf("poster-gas down must preserve only total fees: fees=%+v floor=%+v surplus=%+v err=%v", fees, floor, surplus, err)
	}
	// Rolling back the attributed-cost migration drops only its nullable
	// columns, and the step below it preserves the basic missing range in
	// the legacy JSON checkpoint before that table is removed.
	if err := m.Down(1); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Version(); err != nil || v != headVersion-3 {
		t.Fatalf("after attributed-cost down: %d %v", v, err)
	}
	var batchCostColumns int
	if err := p.DB().QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'batch_reports' AND column_name IN ('report_version', 'arbos_version', 'per_batch_gas_charge', 'parent_gas_floor_per_token', 'cost_calculation_version', 'attributed_gas_spent', 'attributed_wei_spent')`).Scan(&batchCostColumns); err != nil || batchCostColumns != 0 {
		t.Fatalf("batch cost columns after down: %d %v", batchCostColumns, err)
	}
	if err := m.Down(1); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Version(); err != nil || v != headVersion-4 {
		t.Fatalf("after missing-ranges down: %d %v", v, err)
	}
	if raw, ok, err := p.GetState(ctx, 1, StateHoles); err != nil || !ok || !strings.Contains(raw, `"from": 10`) {
		t.Fatalf("rollback checkpoint: %q %v %v", raw, ok, err)
	}
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Version(); err != nil || v != headVersion {
		t.Fatalf("after migration up: %d %v", v, err)
	}
	// Stepping back past the owner-action transaction index removes the
	// column, and the step below that removes the state-sample index. Both
	// keep the original schema and the sample rows.
	if err := m.Down(5); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Version(); err != nil || v != headVersion-5 {
		t.Fatalf("after down: %d %v", v, err)
	}
	var txIndexes int
	if err := p.DB().QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'owner_actions' AND column_name = 'tx_index'`).Scan(&txIndexes); err != nil || txIndexes != 0 {
		t.Fatalf("transaction index after down: %d %v", txIndexes, err)
	}
	if err := m.Down(1); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Version(); err != nil || v != headVersion-6 {
		t.Fatalf("after index down: %d %v", v, err)
	}
	var indexes int
	if err := p.DB().QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'state_samples_chain_block'`).Scan(&indexes); err != nil || indexes != 0 {
		t.Fatalf("index after down: %d %v", indexes, err)
	}
	var samples int
	if err := p.DB().QueryRowContext(ctx, `SELECT count(*) FROM state_samples WHERE chain_id = 1`).Scan(&samples); err != nil || samples != 1 {
		t.Fatalf("samples after down: %d %v", samples, err)
	}
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Version(); err != nil || v != headVersion {
		t.Fatalf("after up: %d %v", v, err)
	}
	if err := p.DB().QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'batch_reports' AND column_name IN ('report_version', 'arbos_version', 'per_batch_gas_charge', 'parent_gas_floor_per_token', 'cost_calculation_version', 'attributed_gas_spent', 'attributed_wei_spent')`).Scan(&batchCostColumns); err != nil || batchCostColumns != 7 {
		t.Fatalf("batch cost columns after up: %d %v", batchCostColumns, err)
	}
	if err := p.DB().QueryRowContext(ctx, `SELECT count(*) FROM state_samples WHERE chain_id = 1`).Scan(&samples); err != nil || samples != 1 {
		t.Fatalf("samples after up: %d %v", samples, err)
	}
	if err := m.Up(); err != nil {
		t.Fatal("Up twice should be a no-op")
	}

	// Rolling all migrations back drops the schema, and applying them
	// recreates it.
	if err := m.Down(int(headVersion)); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Version(); err != nil || v != 0 {
		t.Fatalf("after full down: %d %v", v, err)
	}
	var tables int
	if err := p.DB().QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('blocks','buckets','networks')`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("down must drop the schema: %d %v", tables, err)
	}
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	if v, _, err := m.Version(); err != nil || v != headVersion {
		t.Fatalf("after up: %d %v", v, err)
	}
}

func TestIntegrationStateSampleBlockIndexPlans(t *testing.T) {
	p := openIntegration(t)
	ctx := context.Background()

	// Enough rows and fresh statistics make PostgreSQL's normal cost model
	// choose the production index without planner overrides.
	if _, err := p.DB().ExecContext(ctx, `
		INSERT INTO state_samples (chain_id, sampled_at, block_number, base_fee, min_base_fee)
		SELECT chain_id, timestamptz '2026-01-01 00:00:00+00' + block_number * interval '1 second', block_number, 1, 1
		FROM (SELECT unnest(ARRAY[$1::bigint, $2::bigint]) AS chain_id) AS chains
		CROSS JOIN generate_series(1, 10000) AS block_number`, testChain, otherChain); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB().ExecContext(ctx, `ANALYZE state_samples`); err != nil {
		t.Fatal(err)
	}

	lookupPlan := explainPlan(t, p, `EXPLAIN (COSTS OFF)
		SELECT `+sampleColumns+` FROM state_samples
		WHERE chain_id = $1 AND block_number <= $2
		ORDER BY block_number DESC, sampled_at DESC LIMIT 1`, testChain, 9000)
	if !strings.Contains(lookupPlan, "Index Scan using state_samples_chain_block") || strings.Contains(lookupPlan, "Sort") {
		t.Fatalf("StateSampleAt plan does not use the ordered index without sorting:\n%s", lookupPlan)
	}

	deletePlan := explainPlan(t, p, `EXPLAIN (COSTS OFF)
		DELETE FROM state_samples WHERE chain_id = $1 AND block_number > $2`, testChain, 9990)
	if !strings.Contains(deletePlan, "state_samples_chain_block") {
		t.Fatalf("DeleteStateSamplesAfter plan does not use the block index:\n%s", deletePlan)
	}

	t.Logf("StateSampleAt plan:\n%s", lookupPlan)
	t.Logf("DeleteStateSamplesAfter plan:\n%s", deletePlan)
}

func explainPlan(t *testing.T, p *Postgres, query string, args ...any) string {
	t.Helper()
	rows, err := p.DB().QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func TestIntegrationStore(t *testing.T) {
	p := openIntegration(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)

	if err := p.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertNetwork(ctx, Network{ChainID: testChain, Name: "robinhood", DisplayName: "Robinhood Chain", ExplorerURL: "https://x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertNetwork(ctx, Network{ChainID: testChain, Name: "robinhood", DisplayName: "Robinhood", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	n, err := p.NetworkByRef(ctx, "robinhood")
	if err != nil || n == nil || n.DisplayName != "Robinhood" {
		t.Fatalf("NetworkByRef: %+v %v", n, err)
	}
	if n, err := p.NetworkByRef(ctx, "900004663"); err != nil || n == nil {
		t.Fatalf("NetworkByRef by id: %+v %v", n, err)
	}
	if n, err := p.NetworkByRef(ctx, "missing"); err != nil || n != nil {
		t.Fatalf("NetworkByRef missing: %+v %v", n, err)
	}
	// A decimal beyond BIGINT is not an internal error, and a numeric name
	// never shadows a chain id.
	if n, err := p.NetworkByRef(ctx, "9223372036854775808"); err != nil || n != nil {
		t.Fatalf("NetworkByRef huge: %+v %v", n, err)
	}
	if err := p.UpsertNetwork(ctx, Network{ChainID: otherChain, Name: "900004663", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	rangeRow := MissingRange{
		ChainID: testChain, From: 10, To: 20, DetectedAt: base, Lifecycle: "retrying", Reason: "catch up limit",
		Cursor: 15, ReplayState: JSONB(`{"block":14}`), Folded: 15, RetryCount: 2,
		LastAttemptAt: sql.NullTime{Time: base.Add(time.Minute), Valid: true}, NextRetryAt: sql.NullTime{Time: base.Add(2 * time.Minute), Valid: true},
		LastError: sql.NullString{String: "rpc busy", Valid: true}, PredecessorAt: sql.NullTime{Time: base.Add(-time.Second), Valid: true},
		SuccessorAt: sql.NullTime{Time: base.Add(20 * time.Second), Valid: true}, CursorAt: sql.NullTime{Time: base.Add(14 * time.Second), Valid: true},
	}
	if err := p.WithChainTx(ctx, testChain, func(s Store) error { return s.ReplaceMissingRanges(ctx, testChain, []MissingRange{rangeRow}) }); err != nil {
		t.Fatal(err)
	}
	ranges, err := p.MissingRanges(ctx, testChain)
	if err != nil || len(ranges) != 1 || ranges[0].Cursor != 15 || ranges[0].RetryCount != 2 || ranges[0].LastError.String != "rpc busy" || !ranges[0].CursorAt.Valid {
		t.Fatalf("missing ranges: %+v %v", ranges, err)
	}
	if n, err := p.NetworkByRef(ctx, "900004663"); err != nil || n == nil || n.ChainID != testChain {
		t.Fatalf("NetworkByRef prefers the chain id: %+v %v", n, err)
	}
	if n, err := p.NetworkByRef(ctx, "900000007"); err != nil || n == nil || n.Name != "900004663" {
		t.Fatalf("NetworkByRef by id 7: %+v %v", n, err)
	}
	if err := p.SetNetworkError(ctx, testChain, "oops"); err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateNetworkHead(ctx, testChain, 110, base.Add(10*time.Second), base.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	nets, err := p.Networks(ctx)
	if nets = testNetworks(nets); err != nil || len(nets) != 2 || nets[0].ChainID != otherChain || nets[1].HeadBlock.Int64 != 110 || nets[1].LastError.Valid {
		t.Fatalf("Networks: %+v %v", nets, err)
	}

	// Blocks: 100..110, one per second, two of them with two transactions.
	blocks := make([]Block, 0, 11)
	for i := uint64(0); i <= 10; i++ {
		txs := 3
		if i == 2 || i == 7 {
			txs = 2
		}
		blocks = append(blocks, Block{
			ChainID: testChain, Number: 100 + i, Hash: fmt.Sprintf("0x%x", 100+i), ParentHash: fmt.Sprintf("0x%x", 99+i),
			TS: base.Add(time.Duration(i) * time.Second), GasUsed: 1_000_000 * (i + 1), PosterGas: sql.NullInt64{Valid: true},
			BaseFee: WeiFromUint64(20_000_000 + i), L1Block: 50, TxCount: txs, Backlogs: Uint64Array{i, math.MaxUint64 - i},
			ConstraintBips: pq.Int64Array{int64(i), 1}, MinBaseFee: NullWeiFromUint64(10_000_000), PricingVersion: PricingFull,
			ExponentBips: int64(i), PredictedBaseFee: NullWeiFromUint64(20_000_000), Anchored: i == 10,
		})
	}
	blocks[1].PosterGas.Int64 = 767
	if err := p.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	blocks[10].GasUsed = 99
	if err := p.UpsertBlocks(ctx, blocks[10:]); err != nil {
		t.Fatal(err)
	}
	latest, err := p.LatestBlock(ctx, testChain)
	if err != nil || latest.Number != 110 || latest.GasUsed != 99 || !latest.Anchored || latest.Backlogs[1] != math.MaxUint64-10 {
		t.Fatalf("LatestBlock: %+v %v", latest, err)
	}
	if latest.Hash != "0x6e" || latest.ParentHash != "0x6d" || !latest.PosterGas.Valid || latest.ConstraintBips[0] != 10 || latest.MinBaseFee.Wei.Int64() != 10_000_000 {
		t.Fatalf("LatestBlock new columns: %+v", latest)
	}
	if b, err := p.BlockByNumber(ctx, testChain, 105); err != nil || b == nil || b.Number != 105 || b.Backlogs[1] != math.MaxUint64-5 {
		t.Fatalf("BlockByNumber: %+v %v", b, err)
	}
	if b, err := p.BlockByNumber(ctx, testChain, 999); err != nil || b != nil {
		t.Fatalf("BlockByNumber missing: %+v %v", b, err)
	}
	if o, err := p.OldestBlock(ctx, testChain); err != nil || o.Number != 100 {
		t.Fatalf("OldestBlock: %+v %v", o, err)
	}
	if bs, err := p.RecentBlocks(ctx, testChain, 3); err != nil || len(bs) != 3 || bs[0].Number != 110 || bs[2].Number != 108 {
		t.Fatalf("RecentBlocks: %+v %v", bs, err)
	}
	if bs, err := p.BlocksAfter(ctx, testChain, 108, 10); err != nil || len(bs) != 2 || bs[0].Number != 109 {
		t.Fatalf("BlocksAfter: %+v %v", bs, err)
	}
	if bs, err := p.BlocksBetween(ctx, testChain, base.Add(2*time.Second), base.Add(4*time.Second)); err != nil || len(bs) != 2 || bs[0].Number != 102 {
		t.Fatalf("BlocksBetween: %+v %v", bs, err)
	}
	// (base+0, base+2] covers blocks 101 and 102. Block 101 has 767 poster gas.
	if total, compute, err := p.GasBetween(ctx, testChain, base, base.Add(2*time.Second)); err != nil || total != 5_000_000 || compute == nil || *compute != 4_999_233 {
		t.Fatalf("GasBetween: %d %v %v", total, compute, err)
	}
	if nums, err := p.TwoTxBlocks(ctx, testChain, 100, 10); err != nil || len(nums) != 2 || nums[0] != 102 || nums[1] != 107 {
		t.Fatalf("TwoTxBlocks: %v %v", nums, err)
	}

	// Buckets fold incrementally, including array-wise maxima, the exact
	// sum behind the average, and the last_block guard on the *_end fields.
	// The window is far from the block rows so RebuildBuckets below does
	// not touch it.
	fstart := base.Add(-24 * time.Hour)
	b1 := Bucket{ChainID: testChain, Resolution: Resolution1m, BucketStart: fstart, Blocks: 2, GasUsed: 100, PosterGas: sql.NullInt64{Valid: true}, FeesWei: WeiFromUint64(1000), PosterFeesWei: NullWeiFromUint64(0),
		BaseFeeMin: WeiFromUint64(10), BaseFeeAvg: WeiFromUint64(1), BaseFeeMax: WeiFromUint64(30), BaseFeeSum: NullWeiFromUint64(3), ExponentEndBips: 5,
		BacklogsEnd: Uint64Array{1, math.MaxUint64}, BacklogsMax: Uint64Array{5, math.MaxUint64}, ConstraintBipsEnd: pq.Int64Array{5, 0}, MinBaseFee: NullWeiFromUint64(2), PricingVersion: PricingFull,
		FloorFeesWei: NullWeiFromUint64(200), SurplusFeesWei: NullWeiFromUint64(800), ReplayErrorBips: 10, LastBlock: 200}
	if err := p.FoldBuckets(ctx, []Bucket{b1}); err != nil {
		t.Fatal(err)
	}
	b2 := b1
	b2.Blocks = 1
	b2.GasUsed = 50
	b2.PosterGas = sql.NullInt64{Valid: true}
	b2.FeesWei = WeiFromUint64(500)
	b2.BaseFeeMin = WeiFromUint64(5)
	b2.BaseFeeAvg = WeiFromUint64(3)
	b2.BaseFeeSum = NullWeiFromUint64(3)
	b2.BaseFeeMax = WeiFromUint64(25)
	b2.ExponentEndBips = 9
	b2.BacklogsEnd = Uint64Array{7, 8}
	b2.BacklogsMax = Uint64Array{3, 9}
	b2.ConstraintBipsEnd = pq.Int64Array{9, 0}
	b2.MinBaseFee = NullWeiFromUint64(4)
	b2.FloorFeesWei = NullWeiFromUint64(100)
	b2.SurplusFeesWei = NullWeiFromUint64(400)
	b2.ReplayErrorBips = 4
	b2.LastBlock = 150 // older than the stored fold: *_end must not change
	if err := p.FoldBuckets(ctx, []Bucket{b2}); err != nil {
		t.Fatal(err)
	}
	bk, err := p.Buckets(ctx, testChain, Resolution1m, fstart, fstart.Add(time.Minute))
	if err != nil || len(bk) != 1 {
		t.Fatalf("Buckets: %+v %v", bk, err)
	}
	got := bk[0]
	// Average from the exact sum: floor(6/3) = 2, not floor((1*2+3*1)/3) = 1.
	if got.Blocks != 3 || got.GasUsed != 150 || got.FeesWei.Int64() != 1500 || got.BaseFeeMin.Int64() != 5 || got.BaseFeeMax.Int64() != 30 || got.BaseFeeSum.Wei.Int64() != 6 || got.BaseFeeAvg.Int64() != 2 {
		t.Fatalf("fold counters: %+v", got)
	}
	if got.FloorFeesWei.Wei.Int64() != 300 || got.SurplusFeesWei.Wei.Int64() != 1200 || !got.PosterGas.Valid || got.PosterFeesWei.Wei.Sign() != 0 || got.MinBaseFee.Wei.Int64() != 2 || got.ConstraintBipsEnd[0] != 5 {
		t.Fatalf("fold fee split: %+v", got)
	}
	if got.ExponentEndBips != 5 || got.BacklogsEnd[0] != 1 || got.BacklogsEnd[1] != math.MaxUint64 || got.BacklogsMax[0] != 5 || got.BacklogsMax[1] != math.MaxUint64 || got.ReplayErrorBips != 10 || got.LastBlock != 200 {
		t.Fatalf("fold end fields: %+v", got)
	}
	b3 := b2
	b3.LastBlock = 300
	b3.ConstraintSetID.Valid = true
	b3.ConstraintSetID.Int64 = 1
	if err := p.FoldBuckets(ctx, []Bucket{b3}); err != nil {
		t.Fatal(err)
	}
	bk, _ = p.Buckets(ctx, testChain, Resolution1m, fstart, fstart.Add(time.Minute))
	if bk[0].ExponentEndBips != 9 || bk[0].BacklogsEnd[1] != 8 || bk[0].ConstraintSetID.Int64 != 1 || bk[0].LastBlock != 300 || bk[0].MinBaseFee.Wei.Int64() != 4 || bk[0].ConstraintBipsEnd[0] != 9 {
		t.Fatalf("newer fold should replace end fields: %+v", bk[0])
	}

	// RebuildBuckets recomputes a window from its rows exactly like the
	// Go builder, uses the set in force at the last block among the sets
	// whose constraint count matches the block's backlogs (a later set of
	// another shape is never applied), is idempotent, and drops a bucket
	// whose window has no rows.
	setID, err := p.InsertConstraintSet(ctx, ConstraintSet{ChainID: testChain, EffectiveBlock: 105, EffectiveAt: base, Constraints: JSONB(`[{"target":1,"window":1,"startingBacklog":0},{"target":2,"window":2,"startingBacklog":0}]`), Source: "owner_action"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.InsertConstraintSet(ctx, ConstraintSet{ChainID: testChain, EffectiveBlock: 108, EffectiveAt: base, Constraints: JSONB(`[{"target":1,"window":1,"startingBacklog":0}]`), Source: "observed"}); err != nil {
		t.Fatal(err)
	}
	if err := p.RebuildBuckets(ctx, testChain, Resolution1m, []time.Time{base, base.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := p.RebuildBuckets(ctx, testChain, Resolution1m, []time.Time{base}); err != nil {
		t.Fatal(err)
	}
	rows, _ := p.BlocksBetween(ctx, testChain, base, base.Add(time.Minute))
	want := FoldBlocks(rows, func(n uint64) sql.NullInt64 {
		if n >= 105 {
			return sql.NullInt64{Int64: setID, Valid: true}
		}
		return sql.NullInt64{}
	})[0]
	rb, err := p.Buckets(ctx, testChain, Resolution1m, base, base.Add(2*time.Minute))
	if err != nil || len(rb) != 1 {
		t.Fatalf("rebuilt buckets: %+v %v", rb, err)
	}
	r := rb[0]
	if r.Blocks != want.Blocks || r.GasUsed != want.GasUsed || r.FeesWei.String() != want.FeesWei.String() || r.BaseFeeSum.Wei.String() != want.BaseFeeSum.Wei.String() || !r.BaseFeeSum.Valid ||
		r.BaseFeeAvg.String() != want.BaseFeeAvg.String() || r.BaseFeeMin.String() != want.BaseFeeMin.String() || r.BaseFeeMax.String() != want.BaseFeeMax.String() {
		t.Fatalf("rebuilt counters: %+v\nwant %+v", r, want)
	}
	if r.PosterGas.Int64 != 767 || r.PosterFeesWei.Wei.String() != want.PosterFeesWei.Wei.String() || r.FloorFeesWei.Wei.String() != want.FloorFeesWei.Wei.String() || r.SurplusFeesWei.Wei.String() != want.SurplusFeesWei.Wei.String() || !r.SurplusFeesWei.Valid || r.MinBaseFee.Wei.String() != want.MinBaseFee.Wei.String() {
		t.Fatalf("rebuilt fee split: %+v\nwant %+v", r, want)
	}
	if r.LastBlock != 110 || r.ExponentEndBips != 10 || r.BacklogsEnd[1] != math.MaxUint64-10 || r.BacklogsMax[0] != 10 || r.BacklogsMax[1] != math.MaxUint64 ||
		r.ConstraintBipsEnd[0] != 10 || r.ConstraintSetID.Int64 != setID || r.ReplayErrorBips != want.ReplayErrorBips {
		t.Fatalf("rebuilt end fields: %+v\nwant %+v", r, want)
	}
	if r.PricingVersion != PricingFull || r.PricingVersion != want.PricingVersion {
		t.Fatalf("rebuilt pricing version: %d want %d", r.PricingVersion, want.PricingVersion)
	}
	// A malformed version-1 row without a minimum fee must not let PostgreSQL
	// LEAST fabricate a known floor from base_fee alone.
	if _, err := p.DB().ExecContext(ctx, `UPDATE blocks SET min_base_fee = NULL WHERE chain_id = $1 AND number = $2`, testChain, 104); err != nil {
		t.Fatal(err)
	}
	if err := p.RebuildBuckets(ctx, testChain, Resolution1m, []time.Time{base}); err != nil {
		t.Fatal(err)
	}
	missingFloor, _ := p.Buckets(ctx, testChain, Resolution1m, base, base.Add(time.Minute))
	if len(missingFloor) != 1 || missingFloor[0].FloorFeesWei.Valid || missingFloor[0].SurplusFeesWei.Valid {
		t.Fatalf("missing minimum fee must make compute destinations unknown: %+v", missingFloor)
	}
	if _, err := p.DB().ExecContext(ctx, `UPDATE blocks SET min_base_fee = 10000000 WHERE chain_id = $1 AND number = $2`, testChain, 104); err != nil {
		t.Fatal(err)
	}
	// One source block from before the breakdown existed makes the whole
	// window unknown: the rebuild must not sum its placeholder zero floor
	// and present the result as an exact split.
	if _, err := p.DB().ExecContext(ctx, `UPDATE blocks SET pricing_version = 0, min_base_fee = NULL, constraint_bips = NULL WHERE chain_id = $1 AND number = $2`, testChain, 103); err != nil {
		t.Fatal(err)
	}
	if err := p.RebuildBuckets(ctx, testChain, Resolution1m, []time.Time{base}); err != nil {
		t.Fatal(err)
	}
	mixed, _ := p.Buckets(ctx, testChain, Resolution1m, base, base.Add(time.Minute))
	if len(mixed) != 1 || mixed[0].PricingVersion != PricingUnknown {
		t.Fatalf("a window with unknown history must be unknown: %+v", mixed)
	}
	if mixed[0].FloorFeesWei.Valid || mixed[0].SurplusFeesWei.Valid || mixed[0].MinBaseFee.Valid || mixed[0].ConstraintBipsEnd != nil {
		t.Fatalf("no exact split may be rebuilt from unknown history: %+v", mixed[0])
	}
	if mixed[0].Blocks != want.Blocks || mixed[0].GasUsed != want.GasUsed || !mixed[0].BaseFeeSum.Valid {
		t.Fatalf("the additive fields stay exact: %+v", mixed[0])
	}
	// The Go builder agrees with the SQL rebuild on the same rows.
	mixedRows, _ := p.BlocksBetween(ctx, testChain, base, base.Add(time.Minute))
	if built := FoldBlocks(mixedRows, func(uint64) sql.NullInt64 { return sql.NullInt64{} })[0]; built.PricingVersion != PricingUnknown || built.FloorFeesWei.Valid || built.MinBaseFee.Valid {
		t.Fatalf("builder and rebuild disagree: %+v", built)
	}
	// A fold into that bucket keeps it unknown.
	if err := p.FoldBuckets(ctx, []Bucket{{ChainID: testChain, Resolution: Resolution1m, BucketStart: base, Blocks: 1, GasUsed: 1, FeesWei: WeiFromUint64(1),
		BaseFeeMin: WeiFromUint64(1), BaseFeeAvg: WeiFromUint64(1), BaseFeeMax: WeiFromUint64(1), BaseFeeSum: NullWeiFromUint64(1),
		PosterGas: sql.NullInt64{Valid: true}, PosterFeesWei: NullWeiFromUint64(0), BacklogsEnd: Uint64Array{1}, BacklogsMax: Uint64Array{1}, ConstraintBipsEnd: pq.Int64Array{1}, MinBaseFee: NullWeiFromUint64(1),
		FloorFeesWei: NullWeiFromUint64(1), SurplusFeesWei: NullWeiFromUint64(0), LastBlock: 999, PricingVersion: PricingFull}}); err != nil {
		t.Fatal(err)
	}
	folded, _ := p.Buckets(ctx, testChain, Resolution1m, base, base.Add(time.Minute))
	if folded[0].PricingVersion != PricingUnknown || folded[0].FloorFeesWei.Valid || folded[0].MinBaseFee.Valid || folded[0].ConstraintBipsEnd != nil {
		t.Fatalf("a fold into unknown history stays unknown: %+v", folded[0])
	}
	// Restore the block so the rest of the test sees the full breakdown.
	if _, err := p.DB().ExecContext(ctx, `UPDATE blocks SET pricing_version = 1, min_base_fee = 10000000, constraint_bips = '{3}' WHERE chain_id = $1 AND number = $2`, testChain, 103); err != nil {
		t.Fatal(err)
	}
	if err := p.RebuildBuckets(ctx, testChain, Resolution1m, []time.Time{base}); err != nil {
		t.Fatal(err)
	}
	// Reorg helpers: rows above a block go, their buckets follow.
	removed, err := p.DeleteBlocksAfter(ctx, testChain, 108)
	if err != nil || len(removed) != 2 || removed[0].Number != 109 || removed[1].Number != 110 {
		t.Fatalf("DeleteBlocksAfter: %+v %v", removed, err)
	}
	if err := p.RebuildBuckets(ctx, testChain, Resolution1m, BucketStarts(removed, Resolution1m)); err != nil {
		t.Fatal(err)
	}
	rb, _ = p.Buckets(ctx, testChain, Resolution1m, base, base.Add(2*time.Minute))
	if len(rb) != 1 || rb[0].Blocks != 9 || rb[0].LastBlock != 108 {
		t.Fatalf("bucket after rewind: %+v", rb)
	}
	if err := p.UpsertBlocks(ctx, removed); err != nil {
		t.Fatal(err)
	}
	if n, err := p.DeleteBucketsBefore(ctx, testChain, base); err != nil || n != 1 {
		t.Fatalf("DeleteBucketsBefore: %d %v", n, err)
	}
	if err := p.RewindAfter(ctx, testChain, 104); err != nil {
		t.Fatal(err)
	}
	if cs, _ := p.ConstraintSets(ctx, testChain); len(cs) != 0 {
		t.Fatalf("RewindAfter must drop the sets at 105 and 108: %+v", cs)
	}
	// UpdateConstraintSet moves a row in place, keeping its id.
	oid, err := p.InsertConstraintSet(ctx, ConstraintSet{ChainID: testChain, EffectiveBlock: 500, EffectiveAt: base, Constraints: JSONB(`[]`), Source: "observed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateConstraintSet(ctx, ConstraintSet{ID: oid, ChainID: testChain, EffectiveBlock: 400, EffectiveAt: base.Add(time.Second), Constraints: JSONB(`[{"target":1}]`), Source: "owner_action"}); err != nil {
		t.Fatal(err)
	}
	if cs, _ := p.ConstraintSets(ctx, testChain); len(cs) != 1 || cs[0].ID != oid || cs[0].EffectiveBlock != 400 || cs[0].Source != "owner_action" {
		t.Fatalf("UpdateConstraintSet: %+v", cs)
	}
	if err := p.RewindAfter(ctx, testChain, 1); err != nil {
		t.Fatal(err)
	}

	// State samples with and without L1 data.
	s1 := StateSample{ChainID: testChain, SampledAt: base, BlockNumber: 100, BaseFee: WeiFromUint64(1), MinBaseFee: WeiFromUint64(1), Constraints: JSONB(`[]`), Prices: JSONB(`{}`)}
	s2 := s1
	s2.SampledAt = base.Add(time.Minute)
	s2.L1 = JSONB(`{"baseFeeEstimate":"1"}`)
	s2.Accounts = JSONB(`{"infra":{"address":"0x1","balance":"2"}}`)
	s2.Legacy = JSONB(`{"speedLimit":1}`)
	s3 := s1
	s3.SampledAt = base.Add(2 * time.Minute)
	for _, s := range []StateSample{s1, s2, s3} {
		if err := p.InsertStateSample(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.InsertStateSample(ctx, s3); err != nil {
		t.Fatal("re-insert should upsert")
	}
	ls, err := p.LatestStateSample(ctx, testChain, false)
	if err != nil || ls.SampledAt.Unix() != s3.SampledAt.Unix() || ls.L1 != nil {
		t.Fatalf("LatestStateSample: %+v %v", ls, err)
	}
	ls, err = p.LatestStateSample(ctx, testChain, true)
	if err != nil || ls.SampledAt.Unix() != s2.SampledAt.Unix() || string(ls.L1) != `{"baseFeeEstimate": "1"}` || ls.Legacy == nil {
		t.Fatalf("LatestStateSample l1: %+v %v", ls, err)
	}
	if ss, err := p.L1Samples(ctx, testChain, base, base.Add(time.Hour), 15*time.Minute); err != nil || len(ss) != 1 {
		t.Fatalf("L1Samples: %+v %v", ss, err)
	}
	if n, err := p.PruneStateSamples(ctx, testChain, base.Add(30*time.Second)); err != nil || n != 1 {
		t.Fatalf("PruneStateSamples: %d %v", n, err)
	}
	s4 := s1
	s4.SampledAt = base.Add(3 * time.Minute)
	s4.BlockNumber = 200
	if err := p.InsertStateSample(ctx, s4); err != nil {
		t.Fatal(err)
	}
	if n, err := p.DeleteStateSamplesAfter(ctx, testChain, 150); err != nil || n != 1 {
		t.Fatalf("DeleteStateSamplesAfter: %d %v", n, err)
	}
	if ls, _ := p.LatestStateSample(ctx, testChain, false); ls.BlockNumber != 100 {
		t.Fatalf("sample above the block must go: %+v", ls)
	}

	// Owner actions and constraint sets.
	acts := []OwnerAction{
		{ChainID: testChain, BlockNumber: 28, TxHash: "0xa", TxIndex: sql.NullInt64{Int64: 2, Valid: true}, LogIndex: 0, TS: base.Add(-time.Hour), Method: "setGasPricingConstraints", Selector: "0xcc0d556a", Args: JSONB(`{"constraints":[]}`)},
		{ChainID: testChain, BlockNumber: 174150, TxHash: "0xb", LogIndex: 1, TS: base, Method: "setMinimumL2BaseFee", Selector: "0xa0188cdb", Args: JSONB(`{"priceInWei":"20000000"}`)},
	}
	if n, err := p.InsertOwnerActions(ctx, acts); err != nil || n != 2 {
		t.Fatalf("InsertOwnerActions: %d %v", n, err)
	}
	if n, err := p.InsertOwnerActions(ctx, acts); err != nil || n != 0 {
		t.Fatalf("duplicate InsertOwnerActions: %d %v", n, err)
	}
	if as, err := p.OwnerActions(ctx, testChain, time.Time{}, time.Time{}, 0); err != nil || len(as) != 2 || as[0].BlockNumber != 174150 || !as[1].TxIndex.Valid || as[1].TxIndex.Int64 != 2 {
		t.Fatalf("OwnerActions: %+v %v", as, err)
	}
	if as, err := p.OwnerActions(ctx, testChain, base.Add(-time.Minute), base.Add(time.Minute), 1); err != nil || len(as) != 1 || as[0].TxHash != "0xb" {
		t.Fatalf("OwnerActions ranged: %+v %v", as, err)
	}
	if as, err := p.OwnerActionsSince(ctx, testChain, 29); err != nil || len(as) != 1 || as[0].TxHash != "0xb" {
		t.Fatalf("OwnerActionsSince: %+v %v", as, err)
	}
	id, err := p.InsertConstraintSet(ctx, ConstraintSet{ChainID: testChain, EffectiveBlock: 28, EffectiveAt: base, Constraints: JSONB(`[{"target":1,"window":2,"startingBacklog":0}]`), Source: "genesis"})
	if err != nil || id == 0 {
		t.Fatalf("InsertConstraintSet: %d %v", id, err)
	}
	id2, err := p.InsertConstraintSet(ctx, ConstraintSet{ChainID: testChain, EffectiveBlock: 28, EffectiveAt: base, Constraints: JSONB(`[]`), Source: "genesis"})
	if err != nil || id2 != id {
		t.Fatalf("upsert should keep the id: %d vs %d %v", id2, id, err)
	}
	if cs, err := p.ConstraintSets(ctx, testChain); err != nil || len(cs) != 1 || string(cs[0].Constraints) != `[]` {
		t.Fatalf("ConstraintSets: %+v %v", cs, err)
	}

	// Batch reports aggregate per step.
	reports := []BatchReport{
		{ChainID: testChain, BlockNumber: 102, BatchNumber: 1, BatchTS: base, Poster: "0xp", CalldataLen: 100, CalldataNonzero: 80, ExtraGas: 10, L1BaseFee: WeiFromUint64(100), GasSpent: 1370, WeiSpent: WeiFromUint64(137_000), ReportVersion: 2, ArbOSVersion: 61, PerBatchGasCharge: 210_000, ParentGasFloorPerToken: 10, CostCalculationVersion: 1},
		{ChainID: testChain, BlockNumber: 107, BatchNumber: 2, BatchTS: base.Add(20 * time.Second), Poster: "0xp", CalldataLen: 50, CalldataNonzero: 40, ExtraGas: 10, L1BaseFee: WeiFromUint64(300), GasSpent: 690, WeiSpent: WeiFromUint64(207_000), ReportVersion: 2, ArbOSVersion: 61, PerBatchGasCharge: 210_000, ParentGasFloorPerToken: 10, CostCalculationVersion: 1},
		{ChainID: testChain, BlockNumber: 108, BatchNumber: 3, BatchTS: base.Add(30 * time.Second), Poster: "0xold", L1BaseFee: WeiFromUint64(1), GasSpent: 999, WeiSpent: WeiFromUint64(999)},
	}
	if err := p.UpsertBatchReports(ctx, reports); err != nil {
		t.Fatal(err)
	}
	if err := p.UpsertBatchReports(ctx, reports[:1]); err != nil {
		t.Fatal(err)
	}
	bb, err := p.BatchBuckets(ctx, testChain, base.Add(-time.Hour), base.Add(time.Hour), time.Minute)
	if err != nil || len(bb) != 1 || bb[0].Batches != 2 || bb[0].GasSpent != 2060 || bb[0].WeiSpent.Int64() != 344_000 || bb[0].L1BaseFeeAvg.Int64() != 200 || bb[0].CalldataBytes != 150 {
		t.Fatalf("BatchBuckets: %+v %v", bb, err)
	}
	if bb[0].T.Unix() != base.Unix() {
		t.Fatalf("bucket start = %v", bb[0].T)
	}
	// Reports sharing a second stay separate rows, ordered by time then block.
	if err := p.UpsertBatchReports(ctx, []BatchReport{{ChainID: testChain, BlockNumber: 101, BatchNumber: 0, BatchTS: base, Poster: "0xp", L1BaseFee: WeiFromUint64(1), WeiSpent: WeiFromUint64(1), CostCalculationVersion: 1}}); err != nil {
		t.Fatal(err)
	}
	rs, err := p.BatchReports(ctx, testChain, base, base.Add(time.Minute))
	if err != nil || len(rs) != 3 || rs[0].BlockNumber != 101 || rs[1].BlockNumber != 102 || rs[2].BlockNumber != 107 {
		t.Fatalf("BatchReports: %+v %v", rs, err)
	}
	if rs[1].ReportVersion != 2 || rs[1].ArbOSVersion != 61 || rs[1].PerBatchGasCharge != 210_000 || rs[1].ParentGasFloorPerToken != 10 || rs[1].CostCalculationVersion != 1 {
		t.Fatalf("BatchReport cost metadata: %+v", rs[1])
	}
	if err := p.RewindAfter(ctx, testChain, 105); err != nil {
		t.Fatal(err)
	}
	if rs, _ := p.BatchReports(ctx, testChain, base, base.Add(time.Minute)); len(rs) != 2 {
		t.Fatalf("RewindAfter must drop the report at 107: %+v", rs)
	}

	// Collector state.
	if _, ok, err := p.GetState(ctx, testChain, StateHead); err != nil || ok {
		t.Fatalf("GetState empty: %v %v", ok, err)
	}
	if err := p.SetState(ctx, testChain, StateHead, "110"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetState(ctx, testChain, StateHead, "111"); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := p.GetState(ctx, testChain, StateHead); err != nil || !ok || v != "111" {
		t.Fatalf("GetState: %s %v %v", v, ok, err)
	}
	if m, err := p.States(ctx, testChain); err != nil || m[StateHead] != "111" {
		t.Fatalf("States: %v %v", m, err)
	}
	if err := p.DeleteState(ctx, testChain, StateHead); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := p.GetState(ctx, testChain, StateHead); ok {
		t.Fatal("DeleteState")
	}
	if err := p.DeleteState(ctx, testChain, StateHead); err != nil {
		t.Fatal("deleting a missing key is not an error")
	}

	// Transactions roll back on error.
	err = p.WithTx(ctx, func(s Store) error {
		if err := s.SetState(ctx, testChain, "tx", "1"); err != nil {
			return err
		}
		return context.Canceled
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok, _ := p.GetState(ctx, testChain, "tx"); ok {
		t.Fatal("rolled back write should not exist")
	}
	if err := p.WithTx(ctx, func(s Store) error { return s.SetState(ctx, testChain, "tx", "2") }); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := p.GetState(ctx, testChain, "tx"); v != "2" {
		t.Fatal("committed write missing")
	}
	// A snapshot transaction is read only.
	err = p.WithSnapshotTx(ctx, func(s Store) error { return s.SetState(ctx, testChain, "tx", "3") })
	if err == nil {
		t.Fatal("a write inside a snapshot transaction must fail")
	}
	if v, _, _ := p.GetState(ctx, testChain, "tx"); v != "2" {
		t.Fatal("snapshot transaction wrote")
	}
	// A chain transaction holds the chain's advisory lock: a second one on
	// the same chain waits until the first commits, another chain's does
	// not, and a nested one reuses the transaction.
	entered := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- p.WithChainTx(ctx, testChain, func(s Store) error {
			close(entered)
			<-release
			return s.WithChainTx(ctx, testChain, func(inner Store) error { return inner.SetState(ctx, testChain, "locked", "1") })
		})
	}()
	<-entered
	otherDone := make(chan error, 1)
	go func() { otherDone <- p.WithChainTx(ctx, thirdChain, func(Store) error { return nil }) }()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("another chain must not wait for the lock")
	}
	sameDone := make(chan error, 1)
	go func() {
		sameDone <- p.WithChainTx(ctx, testChain, func(s Store) error { return s.SetState(ctx, testChain, "locked", "2") })
	}()
	select {
	case <-sameDone:
		t.Fatal("the same chain must wait for the open transaction")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-sameDone; err != nil {
		t.Fatal(err)
	}
	if v, _, _ := p.GetState(ctx, testChain, "locked"); v != "2" {
		t.Fatalf("the waiting transaction ran after the first: %q", v)
	}

	// Pruning.
	if n, err := p.PruneBlocks(ctx, testChain, base.Add(5*time.Second)); err != nil || n != 5 {
		t.Fatalf("PruneBlocks: %d %v", n, err)
	}
	// A window whose rows were all pruned loses its bucket on rebuild.
	if err := p.RebuildBuckets(ctx, testChain, Resolution1m, []time.Time{base}); err != nil {
		t.Fatal(err)
	}
	if bk, _ := p.Buckets(ctx, testChain, Resolution1m, base, base.Add(time.Minute)); len(bk) != 1 || bk[0].Blocks != 6 {
		t.Fatalf("bucket after prune: %+v", bk)
	}
	if _, err := p.PruneBlocks(ctx, testChain, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := p.RebuildBuckets(ctx, testChain, Resolution1m, []time.Time{base}); err != nil {
		t.Fatal(err)
	}
	if bk, _ := p.Buckets(ctx, testChain, Resolution1m, base, base.Add(time.Minute)); len(bk) != 0 {
		t.Fatalf("empty window keeps a bucket: %+v", bk)
	}
}

// TestIntegrationSetBasedEquivalence exercises multi-row writes, existing
// conflicts, duplicate input keys, nullable pricing history and several
// rebuild windows in the same call. The assertions use the same sequential
// merge model as the old per-row FoldBuckets implementation. Only one of
// the two rebuilt windows has poster gas, which pins that the grouped
// rebuild decides receipt coverage per window rather than per statement.
func TestIntegrationSetBasedEquivalence(t *testing.T) {
	p := openIntegration(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	seed := Block{ChainID: testChain, Number: 1, Hash: "seed", TS: base, GasUsed: 1, BaseFee: WeiFromUint64(10),
		Backlogs: Uint64Array{1}, ConstraintBips: pq.Int64Array{2}, PredictedBaseFee: NullWeiFromUint64(11),
		MinBaseFee: NullWeiFromUint64(5), PricingVersion: PricingFull}
	if err := p.UpsertBlocks(ctx, []Block{seed}); err != nil {
		t.Fatal(err)
	}
	update := seed
	update.Hash, update.GasUsed, update.Backlogs = "update", 2, Uint64Array{3, math.MaxUint64}
	other := Block{ChainID: testChain, Number: 2, Hash: "other", TS: base.Add(time.Minute), GasUsed: 3,
		PosterGas: sql.NullInt64{Int64: 1, Valid: true},
		BaseFee:   WeiFromUint64(20), Backlogs: Uint64Array{4, 5}, ConstraintBips: pq.Int64Array{6, 7},
		PredictedBaseFee: NullWeiFromUint64(19), MinBaseFee: NullWeiFromUint64(7), PricingVersion: PricingFull}
	final := update
	final.Hash, final.GasUsed, final.PricingVersion = "final", 4, PricingUnknown
	final.ConstraintBips, final.MinBaseFee = nil, NullWei{}
	if err := p.UpsertBlocks(ctx, []Block{update, other, final}); err != nil {
		t.Fatal(err)
	}
	gotBlock, err := p.BlockByNumber(ctx, testChain, 1)
	if err != nil || gotBlock == nil {
		t.Fatalf("upserted block: %+v %v", gotBlock, err)
	}
	if gotBlock.Hash != final.Hash || gotBlock.GasUsed != final.GasUsed || gotBlock.PricingVersion != PricingUnknown || gotBlock.MinBaseFee.Valid || gotBlock.ConstraintBips != nil || gotBlock.Backlogs[1] != math.MaxUint64 {
		t.Fatalf("last duplicate block must win with nullability intact: %+v", gotBlock)
	}
	if gotBlock.PosterGas.Valid {
		t.Fatalf("a null poster gas must survive the batch: %+v", gotBlock)
	}
	if gotOther, err := p.BlockByNumber(ctx, testChain, 2); err != nil || gotOther == nil || gotOther.Hash != other.Hash || !gotOther.MinBaseFee.Valid || gotOther.PosterGas != other.PosterGas {
		t.Fatalf("second batched block: %+v %v", gotOther, err)
	}

	foldStart := base.Add(-time.Hour)
	stored := Bucket{ChainID: testChain, Resolution: Resolution1m, BucketStart: foldStart, Blocks: 2, GasUsed: 2,
		FeesWei: WeiFromUint64(8), BaseFeeMin: WeiFromUint64(4), BaseFeeAvg: WeiFromUint64(4), BaseFeeMax: WeiFromUint64(4),
		BacklogsEnd: Uint64Array{5}, BacklogsMax: Uint64Array{5}, ConstraintSetID: sql.NullInt64{Int64: 10, Valid: true},
		LastBlock: 10, PricingVersion: PricingUnknown}
	if err := p.FoldBuckets(ctx, []Bucket{stored}); err != nil {
		t.Fatal(err)
	}
	a := Bucket{ChainID: testChain, Resolution: Resolution1m, BucketStart: foldStart, Blocks: 1, GasUsed: 1,
		FeesWei: WeiFromUint64(9), BaseFeeMin: WeiFromUint64(9), BaseFeeAvg: WeiFromUint64(9), BaseFeeMax: WeiFromUint64(9), BaseFeeSum: NullWeiFromUint64(9),
		ExponentEndBips: 20, BacklogsEnd: Uint64Array{3, 6}, BacklogsMax: Uint64Array{3, 6}, ConstraintBipsEnd: pq.Int64Array{20, 1},
		MinBaseFee: NullWeiFromUint64(2), FloorFeesWei: NullWeiFromUint64(2), SurplusFeesWei: NullWeiFromUint64(7), LastBlock: 20, PricingVersion: PricingFull}
	b := Bucket{ChainID: testChain, Resolution: Resolution1m, BucketStart: foldStart, Blocks: 1, GasUsed: 1,
		FeesWei: WeiFromUint64(3), BaseFeeMin: WeiFromUint64(3), BaseFeeAvg: WeiFromUint64(3), BaseFeeMax: WeiFromUint64(3), BaseFeeSum: NullWeiFromUint64(3),
		ExponentEndBips: 30, BacklogsEnd: Uint64Array{7, 8}, BacklogsMax: Uint64Array{7, 8}, ConstraintSetID: sql.NullInt64{Int64: 20, Valid: true},
		LastBlock: 30, PricingVersion: PricingUnknown}
	separate := a
	separate.BucketStart = foldStart.Add(time.Minute)
	if err := p.FoldBuckets(ctx, []Bucket{a, separate, b}); err != nil {
		t.Fatal(err)
	}
	wantFold := MergeBuckets(MergeBuckets(stored, a), b)
	folded, err := p.Buckets(ctx, testChain, Resolution1m, foldStart, foldStart.Add(2*time.Minute))
	if err != nil || len(folded) != 2 {
		t.Fatalf("folded buckets: %+v %v", folded, err)
	}
	gotFold := folded[0]
	if gotFold.Blocks != wantFold.Blocks || gotFold.GasUsed != wantFold.GasUsed || gotFold.FeesWei.String() != wantFold.FeesWei.String() || gotFold.BaseFeeAvg.String() != wantFold.BaseFeeAvg.String() || gotFold.BaseFeeAvg.Int64() != 4 {
		t.Fatalf("set fold differs from sequential merge: %+v want %+v", gotFold, wantFold)
	}
	if gotFold.BaseFeeSum.Valid || gotFold.FloorFeesWei.Valid || gotFold.SurplusFeesWei.Valid || gotFold.MinBaseFee.Valid || gotFold.ConstraintBipsEnd != nil || gotFold.PricingVersion != PricingUnknown {
		t.Fatalf("set fold changed unknown pricing fields: %+v", gotFold)
	}
	if gotFold.LastBlock != 30 || gotFold.ExponentEndBips != 30 || gotFold.BacklogsEnd[1] != 8 || gotFold.BacklogsMax[0] != 7 || gotFold.BacklogsMax[1] != 8 || gotFold.ConstraintSetID.Int64 != 20 {
		t.Fatalf("set fold changed end-field conflict semantics: %+v", gotFold)
	}

	emptyStart := base.Add(2 * time.Minute)
	stale := separate
	stale.BucketStart = emptyStart
	if err := p.FoldBuckets(ctx, []Bucket{stale}); err != nil {
		t.Fatal(err)
	}
	if err := p.RebuildBuckets(ctx, testChain, Resolution1m, []time.Time{base, base.Add(time.Minute), emptyStart, base}); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := p.Buckets(ctx, testChain, Resolution1m, base, base.Add(3*time.Minute))
	if err != nil || len(rebuilt) != 2 {
		t.Fatalf("rebuilt windows: %+v %v", rebuilt, err)
	}
	for i, got := range rebuilt {
		block, err := p.BlockByNumber(ctx, testChain, uint64(i+1))
		if err != nil || block == nil {
			t.Fatalf("source block %d: %+v %v", i+1, block, err)
		}
		want := NewBucketBuilder(testChain, Resolution1m, got.BucketStart)
		want.Add(*block, sql.NullInt64{})
		expected := want.Bucket()
		if got.Blocks != expected.Blocks || got.GasUsed != expected.GasUsed || got.FeesWei.String() != expected.FeesWei.String() || got.BaseFeeSum.Wei.String() != expected.BaseFeeSum.Wei.String() || got.BaseFeeAvg.String() != expected.BaseFeeAvg.String() || got.LastBlock != expected.LastBlock || got.PricingVersion != expected.PricingVersion {
			t.Fatalf("rebuilt bucket %d: %+v want %+v", i, got, expected)
		}
		if got.FloorFeesWei.Valid != expected.FloorFeesWei.Valid || got.MinBaseFee.Valid != expected.MinBaseFee.Valid || (got.ConstraintBipsEnd == nil) != (expected.ConstraintBipsEnd == nil) {
			t.Fatalf("rebuilt nullability %d: %+v want %+v", i, got, expected)
		}
		if got.PosterGas != expected.PosterGas || got.PosterFeesWei.Valid != expected.PosterFeesWei.Valid ||
			(expected.PosterFeesWei.Valid && got.PosterFeesWei.Wei.String() != expected.PosterFeesWei.Wei.String()) ||
			(expected.FloorFeesWei.Valid && got.FloorFeesWei.Wei.String() != expected.FloorFeesWei.Wei.String()) ||
			(expected.SurplusFeesWei.Valid && got.SurplusFeesWei.Wei.String() != expected.SurplusFeesWei.Wei.String()) {
			t.Fatalf("rebuilt poster accounting %d: %+v want %+v", i, got, expected)
		}
	}
	if rebuilt[0].PosterGas.Valid || !rebuilt[1].PosterGas.Valid || !rebuilt[1].PosterFeesWei.Valid || !rebuilt[1].SurplusFeesWei.Valid {
		t.Fatalf("receipt coverage must be decided per window: %+v %+v", rebuilt[0], rebuilt[1])
	}
}

func TestIntegrationNotifyListen(t *testing.T) {
	p := openIntegration(t)
	url := os.Getenv("TEST_DB_URL")
	lctx, lcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer lcancel()
	l, err := NewListener(lctx, url, []string{ChannelLive, ChannelOwnerAction}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal("second close should be a no-op")
	}
	select {
	case _, ok := <-l.Notifications():
		if ok {
			t.Fatal("expected closed channel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pump did not stop")
	}
	l, err = NewListener(lctx, url, []string{ChannelLive, ChannelOwnerAction}, logger.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// Give the listener a moment to connect, then notify until it arrives.
	// Another collector on the database notifies the same channel; its
	// payloads are skipped.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := p.Notify(context.Background(), ChannelLive, `{"chainId":900004663}`); err != nil {
			t.Fatal(err)
		}
		select {
		case n := <-l.Notifications():
			if n.Channel == ChannelLive && n.Payload == `{"chainId":900004663}` {
				return
			}
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("notification not delivered")
		}
	}
}

// TestIntegrationPosterGasRepair covers the two queries the poster-gas repair
// runs: finding the rows stored without receipts, and writing the value onto
// them under the guards that make the write safe to repeat.
func TestIntegrationPosterGasRepair(t *testing.T) {
	ctx := context.Background()
	p := openIntegration(t)
	base := time.Now().UTC().Truncate(time.Second)
	if err := p.UpsertNetwork(ctx, Network{ChainID: testChain, Name: "repair", DisplayName: "Repair", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	blocks := make([]Block, 0, 5)
	for i := uint64(0); i < 5; i++ {
		blocks = append(blocks, Block{
			ChainID: testChain, Number: 200 + i, Hash: fmt.Sprintf("0x%x", 200+i), ParentHash: fmt.Sprintf("0x%x", 199+i),
			TS: base.Add(time.Duration(i) * time.Second), GasUsed: 1_000, BaseFee: WeiFromUint64(20_000_000),
			L1Block: 50, TxCount: 1, Backlogs: Uint64Array{i}, ConstraintBips: pq.Int64Array{int64(i)},
			MinBaseFee: NullWeiFromUint64(10_000_000), PricingVersion: PricingFull, PredictedBaseFee: NullWeiFromUint64(20_000_000),
		})
	}
	// One row already has the value, so it must not come back as work.
	blocks[2].PosterGas = sql.NullInt64{Int64: 40, Valid: true}
	if err := p.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}

	missing, err := p.BlocksMissingPosterGas(ctx, testChain, 200, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 4 || missing[0].Number != 200 || missing[3].Number != 204 {
		t.Fatalf("missing rows: %v %v", numbersOf(missing), err)
	}
	// The cursor bound and the limit both narrow the scan.
	if from, err := p.BlocksMissingPosterGas(ctx, testChain, 203, 10); err != nil || len(from) != 2 {
		t.Fatalf("from 203: %v %v", numbersOf(from), err)
	}
	if capped, err := p.BlocksMissingPosterGas(ctx, testChain, 200, 2); err != nil || len(capped) != 2 {
		t.Fatalf("limit 2: %v %v", numbersOf(capped), err)
	}

	// 203 carries more gas than its row used and 999 is not stored: both are
	// skipped instead of failing the batch or inserting a row.
	if err := p.SetPosterGas(ctx, testChain, map[uint64]uint64{200: 10, 201: 20, 202: 99, 203: 5_000, 999: 1}); err != nil {
		t.Fatal(err)
	}
	for number, want := range map[uint64]sql.NullInt64{
		200: {Int64: 10, Valid: true},
		201: {Int64: 20, Valid: true},
		202: {Int64: 40, Valid: true}, // already recorded, left alone
		203: {},                       // rejected by the gas guard
	} {
		b, err := p.BlockByNumber(ctx, testChain, number)
		if err != nil || b == nil {
			t.Fatalf("block %d: %v", number, err)
		}
		if b.PosterGas != want {
			t.Fatalf("block %d poster gas %+v, want %+v", number, b.PosterGas, want)
		}
	}
	if b, err := p.BlockByNumber(ctx, testChain, 999); err != nil || b != nil {
		t.Fatalf("a number that was not stored was inserted: %+v %v", b, err)
	}
	// Repeating the write changes nothing: every row that has a value keeps it.
	if err := p.SetPosterGas(ctx, testChain, map[uint64]uint64{200: 777}); err != nil {
		t.Fatal(err)
	}
	if b, err := p.BlockByNumber(ctx, testChain, 200); err != nil || b.PosterGas.Int64 != 10 {
		t.Fatalf("a recorded value was overwritten: %+v %v", b.PosterGas, err)
	}
	if err := p.SetPosterGas(ctx, testChain, nil); err != nil {
		t.Fatalf("an empty write: %v", err)
	}
	// Both columns are BIGINT, so a value past its range describes no row
	// this store could hold. It is refused rather than wrapped negative and
	// skipped without a word.
	for name, arg := range map[string]map[uint64]uint64{
		"a block number past the column": {math.MaxInt64 + 1: 10},
		"a gas value past the column":    {204: math.MaxInt64 + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := p.SetPosterGas(ctx, testChain, arg); err == nil {
				t.Fatal("accepted a value the column cannot hold")
			}
		})
	}
}

func numbersOf(blocks []Block) []uint64 {
	out := make([]uint64, len(blocks))
	for i, b := range blocks {
		out[i] = b.Number
	}
	return out
}

// rateBlock is one block of the spread fixture: the compute gas of the second it lands in is the sum
// of every block stamped with it, so a second's rate is not one block's gas.
func rateBlock(n uint64, ts time.Time, gas uint64, poster sql.NullInt64) Block {
	return Block{
		ChainID: testChain, Number: n, Hash: fmt.Sprintf("0x%x", n), ParentHash: fmt.Sprintf("0x%x", n-1),
		TS: ts, GasUsed: gas, PosterGas: poster, BaseFee: WeiFromUint64(20_000_000), L1Block: 50, TxCount: 1,
		Backlogs: Uint64Array{1}, ConstraintBips: pq.Int64Array{1}, MinBaseFee: NullWeiFromUint64(10_000_000),
		PricingVersion: PricingFull, PredictedBaseFee: NullWeiFromUint64(20_000_000),
	}
}

func known(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func TestIntegrationComputeRateSpread(t *testing.T) {
	ctx := context.Background()
	p := openIntegration(t)
	base := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	if err := p.UpsertNetwork(ctx, Network{ChainID: testChain, Name: "spread", DisplayName: "Spread", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	blocks := []Block{
		// First minute: 300 in its first second, 45 in the next, 500 in the last.
		rateBlock(300, base, 100, known(10)),
		rateBlock(301, base, 250, known(40)),
		rateBlock(302, base.Add(time.Second), 50, known(5)),
		rateBlock(303, base.Add(59*time.Second), 500, known(0)),
		// Second minute: one block without authoritative poster gas voids the whole window.
		rateBlock(304, base.Add(time.Minute), 100, known(10)),
		rateBlock(305, base.Add(61*time.Second), 100, sql.NullInt64{}),
	}
	if err := p.UpsertBlocks(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	got, err := p.ComputeRateSpread(ctx, testChain, base, base.Add(2*time.Minute), time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Start.Equal(base) || got[0].MinRate != 45 || got[0].MaxRate != 500 {
		t.Fatalf("per-second spread: %+v", got)
	}
	// The window is the caller's: a to that stops short of a second leaves it out of the spread.
	if short, err := p.ComputeRateSpread(ctx, testChain, base, base.Add(30*time.Second), time.Minute, time.Second); err != nil || len(short) != 1 || short[0].MaxRate != 300 {
		t.Fatalf("short window: %+v %v", short, err)
	}

	// Buckets one resolution finer, whose rate is their compute gas over their own width.
	minutes := []Bucket{}
	for i, compute := range []uint64{600, 120, 6_000} {
		b := NewBucketBuilder(testChain, Resolution1m, base.Add(time.Duration(i)*time.Minute))
		b.Add(rateBlock(uint64(400+i), base.Add(time.Duration(i)*time.Minute), compute+7, known(7)), known(1))
		minutes = append(minutes, b.Bucket())
	}
	// A minute whose poster gas is unknown voids the quarter hour it falls in.
	unknown := NewBucketBuilder(testChain, Resolution1m, base.Add(20*time.Minute))
	unknown.Add(rateBlock(410, base.Add(20*time.Minute), 900, sql.NullInt64{}), known(1))
	if err := p.FoldBuckets(ctx, append(minutes, unknown.Bucket())); err != nil {
		t.Fatal(err)
	}
	spread, err := p.ComputeRateSpread(ctx, testChain, base, base.Add(30*time.Minute), 15*time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(spread) != 1 || !spread[0].Start.Equal(base) || spread[0].MinRate != 2 || spread[0].MaxRate != 100 {
		t.Fatalf("per-minute spread: %+v", spread)
	}
	if _, err := p.ComputeRateSpread(ctx, testChain, base, base.Add(time.Hour), time.Hour, 7*time.Minute); err == nil {
		t.Fatal("a unit no stored resolution is wide must be refused")
	}
	// Nothing to measure: a unit as wide as the step, and an empty window.
	if rows, err := p.ComputeRateSpread(ctx, testChain, base, base.Add(time.Hour), time.Minute, time.Minute); err != nil || rows != nil {
		t.Fatalf("unit as wide as the step: %+v %v", rows, err)
	}
}
