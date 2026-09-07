package db

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
)

func newMock(t *testing.T) (*Postgres, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp), sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	d := sqlx.NewDb(sqlDB, "sqlmock")
	p := NewPostgres(d)
	if p.DB() != d {
		t.Fatal("DB()")
	}
	return p, mock
}

func TestPostgresStats(t *testing.T) {
	p, mock := newMock(t)
	mock.ExpectPing()
	if err := p.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	mock.ExpectPing().WillReturnError(errBoom)
	if err := p.Ping(context.Background()); !errors.Is(err, errBoom) {
		t.Fatalf("Ping error = %v", err)
	}
	st := p.Stats()
	if st.Operations != 2 || st.Errors != 1 || st.TotalLatency < 0 || st.LastLatency < 0 {
		t.Fatalf("stats: %+v", st)
	}
}

var (
	now        = time.Date(2026, 9, 6, 7, 20, 0, 0, time.UTC)
	errBoom    = errors.New("boom")
	blockCols  = []string{"chain_id", "number", "hash", "parent_hash", "ts", "gas_used", "poster_gas", "base_fee", "l1_block", "tx_count", "backlogs", "constraint_bips", "exponent_bips", "predicted_base_fee", "min_base_fee", "anchored", "pricing_version"}
	netCols    = []string{"chain_id", "name", "display_name", "explorer_url", "enabled", "head_block", "head_at", "last_sample_at", "last_error", "updated_at"}
	bucketCol  = []string{"chain_id", "resolution", "bucket_start", "blocks", "gas_used", "poster_gas", "fees_wei", "poster_fees_wei", "base_fee_min", "base_fee_avg", "base_fee_max", "base_fee_sum", "exponent_end_bips", "backlogs_end", "backlogs_max", "constraint_bips_end", "min_base_fee", "floor_fees_wei", "surplus_fees_wei", "constraint_set_id", "replay_error_bips", "last_block", "pricing_version"}
	sampleCol  = []string{"chain_id", "sampled_at", "block_number", "base_fee", "min_base_fee", "constraints", "legacy", "prices", "l1", "accounts"}
	missingCol = []string{"chain_id", "from_block", "to_block", "detected_at", "lifecycle", "reason", "cursor", "replay_state", "folded", "retry_count", "last_attempt_at", "next_retry_at", "last_error", "predecessor_at", "successor_at", "cursor_at", "created_at", "updated_at"}
)

func blockRow() *sqlmock.Rows {
	return sqlmock.NewRows(blockCols).AddRow(4663, 100, "0xh", "0xp", now, 1000, 7, "20000000", 5, 2, "{1,18446744073709551615}", "{3,31}", 34, "20000001", "20000000", true, 1)
}

func TestPostgresQueries(t *testing.T) {
	p, mock := newMock(t)
	ctx := context.Background()

	mock.ExpectPing()
	if err := p.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	mock.ExpectExec("INSERT INTO networks").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.UpsertNetwork(ctx, Network{ChainID: 4663, Name: "robinhood"}); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT .* FROM networks ORDER BY").WillReturnRows(sqlmock.NewRows(netCols).AddRow(4663, "robinhood", "Robinhood", "", true, 100, now, now, nil, now))
	nets, err := p.Networks(ctx)
	if err != nil || len(nets) != 1 || nets[0].HeadBlock.Int64 != 100 || nets[0].LastError.Valid {
		t.Fatalf("Networks: %+v %v", nets, err)
	}
	// A decimal reference queries the chain id only, a name the name only,
	// and a decimal beyond BIGINT is a name (unknown), never an error.
	mock.ExpectQuery("SELECT .* FROM networks WHERE chain_id = \\$1").WithArgs(int64(4663)).WillReturnRows(sqlmock.NewRows(netCols).AddRow(4663, "robinhood", "Robinhood", "", true, nil, nil, nil, "err", now))
	n, err := p.NetworkByRef(ctx, "4663")
	if err != nil || n == nil || n.LastError.String != "err" || n.HeadBlock.Valid {
		t.Fatalf("NetworkByRef: %+v %v", n, err)
	}
	mock.ExpectQuery("SELECT .* FROM networks WHERE name = \\$1").WithArgs("nope").WillReturnRows(sqlmock.NewRows(netCols))
	if n, err := p.NetworkByRef(ctx, "nope"); err != nil || n != nil {
		t.Fatalf("NetworkByRef missing: %+v %v", n, err)
	}
	mock.ExpectQuery("SELECT .* FROM networks WHERE name = \\$1").WithArgs("9223372036854775808").WillReturnRows(sqlmock.NewRows(netCols))
	if n, err := p.NetworkByRef(ctx, "9223372036854775808"); err != nil || n != nil {
		t.Fatalf("NetworkByRef huge: %+v %v", n, err)
	}
	mock.ExpectExec("UPDATE networks SET head_block").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.UpdateNetworkHead(ctx, 4663, 100, now, now); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("UPDATE networks SET last_error").WithArgs(4663, sql.NullString{String: "bad", Valid: true}).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.SetNetworkError(ctx, 4663, "bad"); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("UPDATE networks SET last_error").WithArgs(4663, sql.NullString{}).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.SetNetworkError(ctx, 4663, ""); err != nil {
		t.Fatal(err)
	}

	mock.ExpectExec("INSERT INTO blocks").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.UpsertBlocks(ctx, []Block{{ChainID: 4663, Number: 1, TS: now, BaseFee: WeiFromUint64(1), Backlogs: Uint64Array{1}}}); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT .* FROM blocks WHERE chain_id = \\$1 ORDER BY number DESC LIMIT 1").WillReturnRows(blockRow())
	b, err := p.LatestBlock(ctx, 4663)
	if err != nil || b == nil || b.Number != 100 || b.BaseFee.Int64() != 20000000 || len(b.Backlogs) != 2 || !b.Anchored {
		t.Fatalf("LatestBlock: %+v %v", b, err)
	}
	if b.Hash != "0xh" || b.ParentHash != "0xp" || b.Backlogs[1] != math.MaxUint64 || b.ConstraintBips[1] != 31 || b.MinBaseFee.Wei.Int64() != 20000000 {
		t.Fatalf("LatestBlock new fields: %+v", b)
	}
	mock.ExpectQuery("SELECT .* FROM blocks WHERE chain_id = \\$1 AND number = \\$2").WillReturnRows(blockRow())
	if b, err := p.BlockByNumber(ctx, 4663, 100); err != nil || b == nil || b.Number != 100 {
		t.Fatalf("BlockByNumber: %+v %v", b, err)
	}
	mock.ExpectQuery("DELETE FROM blocks WHERE chain_id = \\$1 AND number > \\$2 RETURNING").WillReturnRows(blockRow())
	if bs, err := p.DeleteBlocksAfter(ctx, 4663, 99); err != nil || len(bs) != 1 {
		t.Fatalf("DeleteBlocksAfter: %+v %v", bs, err)
	}
	mock.ExpectQuery("SELECT .* FROM blocks WHERE chain_id = \\$1 ORDER BY number ASC LIMIT 1").WillReturnRows(sqlmock.NewRows(blockCols))
	if b, err := p.OldestBlock(ctx, 4663); err != nil || b != nil {
		t.Fatalf("OldestBlock empty: %+v %v", b, err)
	}
	mock.ExpectQuery("SELECT .* FROM blocks WHERE chain_id = \\$1 ORDER BY number DESC LIMIT \\$2").WillReturnRows(blockRow())
	if bs, err := p.RecentBlocks(ctx, 4663, 10); err != nil || len(bs) != 1 {
		t.Fatalf("RecentBlocks: %+v %v", bs, err)
	}
	mock.ExpectQuery("SELECT .* FROM blocks WHERE chain_id = \\$1 AND number > \\$2").WillReturnRows(blockRow())
	if bs, err := p.BlocksAfter(ctx, 4663, 50, 10); err != nil || len(bs) != 1 {
		t.Fatalf("BlocksAfter: %+v %v", bs, err)
	}
	mock.ExpectQuery("SELECT .* FROM blocks WHERE chain_id = \\$1 AND ts >= \\$2").WillReturnRows(blockRow())
	if bs, err := p.BlocksBetween(ctx, 4663, now, now); err != nil || len(bs) != 1 {
		t.Fatalf("BlocksBetween: %+v %v", bs, err)
	}
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(gas_used\\), 0\\)").WillReturnRows(sqlmock.NewRows([]string{"total", "compute"}).AddRow(13000, 12345))
	if total, compute, err := p.GasBetween(ctx, 4663, now, now); err != nil || total != 13000 || compute == nil || *compute != 12345 {
		t.Fatalf("GasBetween: %d %v %v", total, compute, err)
	}
	mock.ExpectQuery("SELECT number FROM blocks WHERE chain_id = \\$1 AND tx_count = 2").WillReturnRows(sqlmock.NewRows([]string{"number"}).AddRow(7).AddRow(9))
	if nums, err := p.TwoTxBlocks(ctx, 4663, 0, 10); err != nil || len(nums) != 2 || nums[1] != 9 {
		t.Fatalf("TwoTxBlocks: %v %v", nums, err)
	}
	mock.ExpectExec("DELETE FROM blocks").WillReturnResult(sqlmock.NewResult(0, 3))
	if n, err := p.PruneBlocks(ctx, 4663, now); err != nil || n != 3 {
		t.Fatalf("PruneBlocks: %d %v", n, err)
	}

	mock.ExpectExec("INSERT INTO buckets").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.FoldBuckets(ctx, []Bucket{{ChainID: 4663, Resolution: "1m", BucketStart: now, Blocks: 1, FeesWei: WeiFromUint64(1), BaseFeeMin: WeiFromUint64(1), BaseFeeAvg: WeiFromUint64(1), BaseFeeMax: WeiFromUint64(1), BacklogsEnd: Uint64Array{1}, BacklogsMax: Uint64Array{1}}}); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("WITH w AS").WithArgs(4663, "1m", now, now.Add(time.Minute), "9223372036854775807").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM buckets WHERE chain_id = \\$1 AND resolution = \\$2 AND bucket_start = \\$3").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := p.RebuildBuckets(ctx, 4663, "1m", []time.Time{now}); err != nil {
		t.Fatal(err)
	}
	if err := p.RebuildBuckets(ctx, 4663, "2m", []time.Time{now}); err == nil {
		t.Fatal("unknown resolution")
	}
	mock.ExpectExec("DELETE FROM buckets WHERE chain_id = \\$1 AND bucket_start < \\$2").WillReturnResult(sqlmock.NewResult(0, 4))
	if n, err := p.DeleteBucketsBefore(ctx, 4663, now); err != nil || n != 4 {
		t.Fatalf("DeleteBucketsBefore: %d %v", n, err)
	}
	mock.ExpectQuery("SELECT .* FROM buckets").WillReturnRows(sqlmock.NewRows(bucketCol).AddRow(4663, "1m", now, 10, 1000, 0, "5", "0", "1", "2", "3", "20", 34, "{1,2}", "{3,4}", "{5,6}", "7", "8", "9", 1, 50, 100, 1))
	bk, err := p.Buckets(ctx, 4663, "1m", now, now)
	if err != nil || len(bk) != 1 || bk[0].BacklogsMax[1] != 4 || bk[0].ConstraintSetID.Int64 != 1 || bk[0].LastBlock != 100 {
		t.Fatalf("Buckets: %+v %v", bk, err)
	}
	if bk[0].BaseFeeSum.Wei.Int64() != 20 || !bk[0].PosterGas.Valid || bk[0].PosterFeesWei.Wei.Sign() != 0 || bk[0].ConstraintBipsEnd[1] != 6 || bk[0].MinBaseFee.Wei.Int64() != 7 || bk[0].FloorFeesWei.Wei.Int64() != 8 || bk[0].SurplusFeesWei.Wei.Int64() != 9 {
		t.Fatalf("Buckets new fields: %+v", bk[0])
	}
	// A bucket written before the sum and fee split existed scans as
	// unknown, not zero, and a NULL exponent array as nil.
	mock.ExpectQuery("SELECT .* FROM buckets").WillReturnRows(sqlmock.NewRows(bucketCol).AddRow(4663, "1m", now, 10, 1000, nil, "5", nil, "1", "2", "3", nil, 34, "{1,2}", "{3,4}", nil, nil, nil, nil, 1, 50, 100, 0))
	bk, err = p.Buckets(ctx, 4663, "1m", now, now)
	if err != nil || len(bk) != 1 || bk[0].BaseFeeSum.Valid || bk[0].FloorFeesWei.Valid || bk[0].SurplusFeesWei.Valid || bk[0].ConstraintBipsEnd != nil {
		t.Fatalf("Buckets unknown fields: %+v %v", bk, err)
	}
	if bk[0].MinBaseFee.Valid || bk[0].PricingVersion != PricingUnknown {
		t.Fatalf("history without a breakdown has no known floor: %+v", bk[0])
	}

	mock.ExpectExec("INSERT INTO state_samples").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.InsertStateSample(ctx, StateSample{ChainID: 4663, SampledAt: now, Constraints: JSONB(`[]`), Prices: JSONB(`{}`)}); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT .* FROM state_samples WHERE chain_id = \\$1 AND l1 IS NOT NULL ORDER BY").WillReturnRows(sqlmock.NewRows(sampleCol).AddRow(4663, now, 1, "1", "1", []byte(`[]`), nil, []byte(`{}`), []byte(`{"a":1}`), nil))
	s, err := p.LatestStateSample(ctx, 4663, true)
	if err != nil || s == nil || string(s.L1) != `{"a":1}` || s.Legacy != nil {
		t.Fatalf("LatestStateSample: %+v %v", s, err)
	}
	mock.ExpectQuery("SELECT .* FROM state_samples WHERE chain_id = \\$1 ORDER BY").WillReturnRows(sqlmock.NewRows(sampleCol))
	if s, err := p.LatestStateSample(ctx, 4663, false); err != nil || s != nil {
		t.Fatalf("LatestStateSample empty: %+v %v", s, err)
	}
	mock.ExpectQuery("SELECT DISTINCT ON").WithArgs(4663, now, now, int64(60)).WillReturnRows(sqlmock.NewRows(sampleCol).AddRow(4663, now, 1, "1", "1", []byte(`[]`), nil, []byte(`{}`), []byte(`{}`), nil))
	if ss, err := p.L1Samples(ctx, 4663, now, now, time.Minute); err != nil || len(ss) != 1 {
		t.Fatalf("L1Samples: %+v %v", ss, err)
	}
	mock.ExpectExec("DELETE FROM state_samples WHERE chain_id = \\$1 AND sampled_at < \\$2").WillReturnResult(sqlmock.NewResult(0, 2))
	if n, err := p.PruneStateSamples(ctx, 4663, now); err != nil || n != 2 {
		t.Fatalf("PruneStateSamples: %d %v", n, err)
	}
	mock.ExpectExec("DELETE FROM state_samples WHERE chain_id = \\$1 AND block_number > \\$2").WithArgs(4663, 100).WillReturnResult(sqlmock.NewResult(0, 3))
	if n, err := p.DeleteStateSamplesAfter(ctx, 4663, 100); err != nil || n != 3 {
		t.Fatalf("DeleteStateSamplesAfter: %d %v", n, err)
	}

	mock.ExpectQuery("SELECT .* FROM missing_ranges WHERE chain_id = \\$1 ORDER BY").WillReturnRows(sqlmock.NewRows(missingCol).AddRow(
		4663, 100, 199, now.Add(-time.Hour), "retrying", "catch up limit", 150, []byte(`{"block":149}`), 150, 2,
		now.Add(-time.Minute), now.Add(time.Minute), "rpc busy", now.Add(-2*time.Hour), now, now.Add(-time.Minute), now.Add(-time.Hour), now,
	))
	ranges, err := p.MissingRanges(ctx, 4663)
	if err != nil || len(ranges) != 1 || ranges[0].Cursor != 150 || ranges[0].RetryCount != 2 || string(ranges[0].ReplayState) != `{"block":149}` || !ranges[0].SuccessorAt.Valid {
		t.Fatalf("MissingRanges: %+v %v", ranges, err)
	}
	mock.ExpectExec("DELETE FROM missing_ranges WHERE chain_id").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO missing_ranges").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.ReplaceMissingRanges(ctx, 4663, ranges); err != nil {
		t.Fatalf("ReplaceMissingRanges: %v", err)
	}
	// Every row of a replacement goes in one statement, so the chain lock is
	// not held for a round trip per range.
	second := ranges[0]
	second.From, second.To = 300, 399
	mock.ExpectExec("DELETE FROM missing_ranges WHERE chain_id").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO missing_ranges .* VALUES \(\$1, .*COALESCE\(\$17, now\(\)\), now\(\)\), \(\$18, .*COALESCE\(\$34, now\(\)\), now\(\)\)`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	if err := p.ReplaceMissingRanges(ctx, 4663, append(ranges, second)); err != nil {
		t.Fatalf("ReplaceMissingRanges batched: %v", err)
	}
	mock.ExpectExec("DELETE FROM missing_ranges WHERE chain_id").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO missing_ranges").WillReturnError(errBoom)
	if err := p.ReplaceMissingRanges(ctx, 4663, ranges); err == nil {
		t.Fatal("ReplaceMissingRanges insert error")
	}

	mock.ExpectExec("INSERT INTO owner_actions").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO owner_actions").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("UPDATE owner_actions SET tx_index").WillReturnResult(sqlmock.NewResult(0, 1))
	if n, err := p.InsertOwnerActions(ctx, []OwnerAction{{TxHash: "a", Args: JSONB(`{}`)}, {TxHash: "b", TxIndex: sql.NullInt64{Int64: 2, Valid: true}, Args: JSONB(`{}`)}}); err != nil || n != 1 {
		t.Fatalf("InsertOwnerActions: %d %v", n, err)
	}
	oaCols := []string{"chain_id", "block_number", "tx_hash", "tx_index", "log_index", "ts", "method", "selector", "args"}
	mock.ExpectQuery("SELECT .* FROM owner_actions WHERE chain_id = \\$1 AND ts >= \\$2 AND ts < \\$3 ORDER BY .* LIMIT \\$4").WillReturnRows(sqlmock.NewRows(oaCols).AddRow(4663, 1, "0x", 2, 0, now, "m", "0x1", []byte(`{}`)))
	if as, err := p.OwnerActions(ctx, 4663, now, now, 5); err != nil || len(as) != 1 || as[0].TxIndex.Int64 != 2 {
		t.Fatalf("OwnerActions: %+v %v", as, err)
	}
	mock.ExpectQuery("SELECT .* FROM owner_actions WHERE chain_id = \\$1 ORDER BY").WillReturnRows(sqlmock.NewRows(oaCols))
	if as, err := p.OwnerActions(ctx, 4663, time.Time{}, time.Time{}, 0); err != nil || len(as) != 0 {
		t.Fatalf("OwnerActions unbounded: %+v %v", as, err)
	}
	mock.ExpectQuery("SELECT .* FROM owner_actions WHERE chain_id = \\$1 AND block_number >= \\$2 ORDER BY block_number ASC").WithArgs(4663, 5).WillReturnRows(sqlmock.NewRows(oaCols).AddRow(4663, 6, "0x", nil, 0, now, "m", "0x1", []byte(`{}`)))
	if as, err := p.OwnerActionsSince(ctx, 4663, 5); err != nil || len(as) != 1 || as[0].BlockNumber != 6 {
		t.Fatalf("OwnerActionsSince: %+v %v", as, err)
	}
	mock.ExpectExec("DELETE FROM owner_actions WHERE chain_id = \\$1 AND block_number > \\$2").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM constraint_sets WHERE chain_id = \\$1 AND effective_block > \\$2").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM batch_reports WHERE chain_id = \\$1 AND block_number > \\$2").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.RewindAfter(ctx, 4663, 5); err != nil {
		t.Fatal(err)
	}

	mock.ExpectQuery("INSERT INTO constraint_sets").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(9))
	if id, err := p.InsertConstraintSet(ctx, ConstraintSet{ChainID: 4663, Constraints: JSONB(`[]`), Source: "genesis"}); err != nil || id != 9 {
		t.Fatalf("InsertConstraintSet: %d %v", id, err)
	}
	mock.ExpectExec("UPDATE constraint_sets SET effective_block").WithArgs(9, 4663, 28, now, JSONB(`[]`), "genesis").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.UpdateConstraintSet(ctx, ConstraintSet{ID: 9, ChainID: 4663, EffectiveBlock: 28, EffectiveAt: now, Constraints: JSONB(`[]`), Source: "genesis"}); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT .* FROM constraint_sets").WillReturnRows(sqlmock.NewRows([]string{"id", "chain_id", "effective_block", "effective_at", "constraints", "source"}).AddRow(9, 4663, 28, now, []byte(`[]`), "genesis"))
	if cs, err := p.ConstraintSets(ctx, 4663); err != nil || len(cs) != 1 || cs[0].EffectiveBlock != 28 {
		t.Fatalf("ConstraintSets: %+v %v", cs, err)
	}

	mock.ExpectExec("INSERT INTO batch_reports").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.UpsertBatchReports(ctx, []BatchReport{{ChainID: 4663, BlockNumber: 1, BatchTS: now, L1BaseFee: WeiFromUint64(1), WeiSpent: WeiFromUint64(1)}}); err != nil {
		t.Fatal(err)
	}
	brCols := []string{"chain_id", "block_number", "batch_number", "batch_ts", "poster", "calldata_len", "calldata_nonzero", "extra_gas", "l1_base_fee",
		"attributed_gas_spent", "attributed_wei_spent", "report_version", "arbos_version", "per_batch_gas_charge", "parent_gas_floor_per_token", "cost_calculation_version"}
	mock.ExpectQuery("SELECT .* FROM batch_reports WHERE chain_id = \\$1 AND batch_ts >= \\$2 AND batch_ts < \\$3 AND cost_calculation_version = 1 ORDER BY batch_ts ASC, block_number ASC").
		WillReturnRows(sqlmock.NewRows(brCols).AddRow(4663, 1, 2, now, "0xp", 3, 4, 5, "6", 7, "8", 2, 61, 210_000, 10, 1))
	if rs, err := p.BatchReports(ctx, 4663, now, now); err != nil || len(rs) != 1 || rs[0].WeiSpent.Int64() != 8 || rs[0].ReportVersion != 2 || rs[0].ArbOSVersion != 61 || rs[0].PerBatchGasCharge != 210_000 || rs[0].ParentGasFloorPerToken != 10 || rs[0].CostCalculationVersion != 1 {
		t.Fatalf("BatchReports: %+v %v", rs, err)
	}
	mock.ExpectQuery("SELECT to_timestamp").WithArgs(4663, now, now, int64(900)).WillReturnRows(sqlmock.NewRows([]string{"t", "batches", "gas_spent", "wei_spent", "l1_base_fee_avg", "calldata_bytes"}).AddRow(now, 3, 100, "200", "50", 400))
	bb, err := p.BatchBuckets(ctx, 4663, now, now, 15*time.Minute)
	if err != nil || len(bb) != 1 || bb[0].Batches != 3 || bb[0].WeiSpent.Int64() != 200 || bb[0].CalldataBytes != 400 {
		t.Fatalf("BatchBuckets: %+v %v", bb, err)
	}

	mock.ExpectQuery("SELECT value FROM collector_state").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("v"))
	if v, ok, err := p.GetState(ctx, 4663, "k"); err != nil || !ok || v != "v" {
		t.Fatalf("GetState: %s %v %v", v, ok, err)
	}
	mock.ExpectQuery("SELECT value FROM collector_state").WillReturnRows(sqlmock.NewRows([]string{"value"}))
	if _, ok, err := p.GetState(ctx, 4663, "k"); err != nil || ok {
		t.Fatalf("GetState missing: %v %v", ok, err)
	}
	mock.ExpectExec("INSERT INTO collector_state").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.SetState(ctx, 4663, "k", "v"); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("DELETE FROM collector_state WHERE chain_id = \\$1 AND key = \\$2").WithArgs(4663, "k").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := p.DeleteState(ctx, 4663, "k"); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT key, value FROM collector_state").WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow("a", "1").AddRow("b", "2"))
	if m, err := p.States(ctx, 4663); err != nil || len(m) != 2 || m["b"] != "2" {
		t.Fatalf("States: %v %v", m, err)
	}
	mock.ExpectExec("SELECT pg_notify").WithArgs("gascurve_live", "{}").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := p.Notify(ctx, ChannelLive, "{}"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresErrors(t *testing.T) {
	p, mock := newMock(t)
	ctx := context.Background()
	calls := []struct {
		name  string
		query bool
		fn    func() error
	}{
		{"UpsertNetwork", false, func() error { return p.UpsertNetwork(ctx, Network{}) }},
		{"Networks", true, func() error { _, err := p.Networks(ctx); return err }},
		{"NetworkByRef", true, func() error { _, err := p.NetworkByRef(ctx, "x"); return err }},
		{"UpdateNetworkHead", false, func() error { return p.UpdateNetworkHead(ctx, 1, 1, now, now) }},
		{"SetNetworkError", false, func() error { return p.SetNetworkError(ctx, 1, "x") }},
		{"UpsertBlocks", false, func() error { return p.UpsertBlocks(ctx, []Block{{}}) }},
		{"BlockByNumber", true, func() error { _, err := p.BlockByNumber(ctx, 1, 1); return err }},
		{"DeleteBlocksAfter", true, func() error { _, err := p.DeleteBlocksAfter(ctx, 1, 1); return err }},
		{"LatestBlock", true, func() error { _, err := p.LatestBlock(ctx, 1); return err }},
		{"OldestBlock", true, func() error { _, err := p.OldestBlock(ctx, 1); return err }},
		{"RecentBlocks", true, func() error { _, err := p.RecentBlocks(ctx, 1, 1); return err }},
		{"BlocksAfter", true, func() error { _, err := p.BlocksAfter(ctx, 1, 1, 1); return err }},
		{"BlocksBetween", true, func() error { _, err := p.BlocksBetween(ctx, 1, now, now); return err }},
		{"GasBetween", true, func() error { _, _, err := p.GasBetween(ctx, 1, now, now); return err }},
		{"TwoTxBlocks", true, func() error { _, err := p.TwoTxBlocks(ctx, 1, 1, 1); return err }},
		{"PruneBlocks", false, func() error { _, err := p.PruneBlocks(ctx, 1, now); return err }},
		{"FoldBuckets", false, func() error { return p.FoldBuckets(ctx, []Bucket{{}}) }},
		{"RebuildBuckets", false, func() error { return p.RebuildBuckets(ctx, 1, "1m", []time.Time{now}) }},
		{"DeleteBucketsBefore", false, func() error { _, err := p.DeleteBucketsBefore(ctx, 1, now); return err }},
		{"Buckets", true, func() error { _, err := p.Buckets(ctx, 1, "1m", now, now); return err }},
		{"InsertStateSample", false, func() error { return p.InsertStateSample(ctx, StateSample{}) }},
		{"LatestStateSample", true, func() error { _, err := p.LatestStateSample(ctx, 1, false); return err }},
		{"L1Samples", true, func() error { _, err := p.L1Samples(ctx, 1, now, now, 0); return err }},
		{"PruneStateSamples", false, func() error { _, err := p.PruneStateSamples(ctx, 1, now); return err }},
		{"DeleteStateSamplesAfter", false, func() error { _, err := p.DeleteStateSamplesAfter(ctx, 1, 1); return err }},
		{"MissingRanges", true, func() error { _, err := p.MissingRanges(ctx, 1); return err }},
		{"ReplaceMissingRanges", false, func() error { return p.ReplaceMissingRanges(ctx, 1, nil) }},
		{"InsertOwnerActions", false, func() error { _, err := p.InsertOwnerActions(ctx, []OwnerAction{{}}); return err }},
		{"OwnerActions", true, func() error { _, err := p.OwnerActions(ctx, 1, now, now, 1); return err }},
		{"OwnerActionsSince", true, func() error { _, err := p.OwnerActionsSince(ctx, 1, 1); return err }},
		{"RewindAfter", false, func() error { return p.RewindAfter(ctx, 1, 1) }},
		{"InsertConstraintSet", true, func() error { _, err := p.InsertConstraintSet(ctx, ConstraintSet{}); return err }},
		{"UpdateConstraintSet", false, func() error { return p.UpdateConstraintSet(ctx, ConstraintSet{}) }},
		{"ConstraintSets", true, func() error { _, err := p.ConstraintSets(ctx, 1); return err }},
		{"UpsertBatchReports", false, func() error { return p.UpsertBatchReports(ctx, []BatchReport{{}}) }},
		{"BatchReports", true, func() error { _, err := p.BatchReports(ctx, 1, now, now); return err }},
		{"BatchBuckets", true, func() error { _, err := p.BatchBuckets(ctx, 1, now, now, 0); return err }},
		{"GetState", true, func() error { _, _, err := p.GetState(ctx, 1, "k"); return err }},
		{"SetState", false, func() error { return p.SetState(ctx, 1, "k", "v") }},
		{"DeleteState", false, func() error { return p.DeleteState(ctx, 1, "k") }},
		{"States", true, func() error { _, err := p.States(ctx, 1); return err }},
		{"Notify", false, func() error { return p.Notify(ctx, "c", "p") }},
	}
	for _, c := range calls {
		if c.query {
			mock.ExpectQuery(".*").WillReturnError(errBoom)
		} else {
			mock.ExpectExec(".*").WillReturnError(errBoom)
		}
		if err := c.fn(); !errors.Is(err, errBoom) {
			t.Errorf("%s: expected boom, got %v", c.name, err)
		}
	}
	mock.ExpectPing().WillReturnError(errBoom)
	if err := p.Ping(ctx); !errors.Is(err, errBoom) {
		t.Errorf("Ping: %v", err)
	}
	// The rebuild's empty-bucket delete can fail on its own.
	mock.ExpectExec("WITH w AS").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM buckets").WillReturnError(errBoom)
	if err := p.RebuildBuckets(ctx, 1, "1m", []time.Time{now}); !errors.Is(err, errBoom) {
		t.Errorf("RebuildBuckets delete: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWithTx(t *testing.T) {
	p, mock := newMock(t)
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO collector_state").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO collector_state").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err := p.WithTx(ctx, func(s Store) error {
		if err := s.SetState(ctx, 1, "a", "1"); err != nil {
			return err
		}
		// Nested WithTx reuses the transaction.
		return s.WithTx(ctx, func(inner Store) error { return inner.SetState(ctx, 1, "b", "2") })
	})
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectRollback()
	if err := p.WithTx(ctx, func(Store) error { return errBoom }); !errors.Is(err, errBoom) {
		t.Fatalf("rollback: %v", err)
	}

	mock.ExpectBegin().WillReturnError(errBoom)
	if err := p.WithTx(ctx, func(Store) error { return nil }); !errors.Is(err, errBoom) {
		t.Fatalf("begin error: %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectCommit().WillReturnError(errBoom)
	if err := p.WithTx(ctx, func(Store) error { return nil }); !errors.Is(err, errBoom) {
		t.Fatalf("commit error: %v", err)
	}

	// A chain transaction takes the advisory lock before anything else;
	// nested chain and plain transactions reuse it.
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(\\$1\\)").WithArgs(int64(4663)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO collector_state").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO collector_state").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err = p.WithChainTx(ctx, 4663, func(s Store) error {
		if err := s.WithChainTx(ctx, 4663, func(inner Store) error { return inner.SetState(ctx, 4663, "a", "1") }); err != nil {
			return err
		}
		return s.WithTx(ctx, func(inner Store) error { return inner.SetState(ctx, 4663, "b", "2") })
	})
	if err != nil {
		t.Fatal(err)
	}
	// A failed lock rolls back without running fn.
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WillReturnError(errBoom)
	mock.ExpectRollback()
	ran := false
	if err := p.WithChainTx(ctx, 4663, func(Store) error { ran = true; return nil }); !errors.Is(err, errBoom) || ran {
		t.Fatalf("lock error: %v ran=%v", err, ran)
	}
	// A snapshot transaction is repeatable read and read only; nesting one
	// inside another reuses it.
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value FROM collector_state").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("v"))
	mock.ExpectCommit()
	err = p.WithSnapshotTx(ctx, func(s Store) error {
		return s.WithSnapshotTx(ctx, func(inner Store) error { _, _, err := inner.GetState(ctx, 1, "k"); return err })
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestNestedTransactionModes: a nested transaction either reuses the open
// one, adds the lock it is missing in ascending chain order, or fails.
// Silently returning the outer transaction would leave the caller
// believing it holds chain exclusion, or one database moment, when it does
// not.
func TestNestedTransactionModes(t *testing.T) {
	p, mock := newMock(t)
	ctx := context.Background()

	// A second chain lock inside a chain transaction is taken, in
	// ascending chain id order, and reused from then on.
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(\\$1\\)").WithArgs(int64(10)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(\\$1\\)").WithArgs(int64(20)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO collector_state").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err := p.WithChainTx(ctx, 10, func(s Store) error {
		return s.WithChainTx(ctx, 20, func(inner Store) error {
			// Both locks are held now, so either chain reuses.
			return inner.WithChainTx(ctx, 10, func(x Store) error { return x.SetState(ctx, 10, "a", "1") })
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	// A lower chain id after a higher one would invert the lock order.
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(\\$1\\)").WithArgs(int64(20)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()
	ran := false
	err = p.WithChainTx(ctx, 20, func(s Store) error {
		return s.WithChainTx(ctx, 10, func(Store) error { ran = true; return nil })
	})
	if !errors.Is(err, ErrLockOrder) || ran {
		t.Fatalf("lock order: %v ran=%v", err, ran)
	}

	// A failing lock acquisition inside an open transaction surfaces.
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(\\$1\\)").WithArgs(int64(10)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(\\$1\\)").WithArgs(int64(20)).WillReturnError(errBoom)
	mock.ExpectRollback()
	err = p.WithChainTx(ctx, 10, func(s Store) error {
		return s.WithChainTx(ctx, 20, func(Store) error { return nil })
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("nested lock error: %v", err)
	}

	// A snapshot inside a writing transaction would read that
	// transaction's own uncommitted rows.
	mock.ExpectBegin()
	mock.ExpectRollback()
	err = p.WithTx(ctx, func(s Store) error {
		return s.WithSnapshotTx(ctx, func(Store) error { return nil })
	})
	if !errors.Is(err, ErrNestedSnapshot) {
		t.Fatalf("snapshot inside a write transaction: %v", err)
	}

	// And a chain transaction inside a read-only snapshot cannot write.
	mock.ExpectBegin()
	mock.ExpectRollback()
	err = p.WithSnapshotTx(ctx, func(s Store) error {
		return s.WithChainTx(ctx, 10, func(Store) error { return nil })
	})
	if !errors.Is(err, ErrSnapshotWrite) {
		t.Fatalf("chain transaction inside a snapshot: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestResetSchemaError(t *testing.T) {
	p, mock := newMock(t)
	mock.ExpectExec("DROP SCHEMA").WillReturnError(errBoom)
	if err := ResetSchema(context.Background(), p.DB()); !errors.Is(err, errBoom) {
		t.Fatalf("ResetSchema: %v", err)
	}
	// Migrator construction against a mock fails at driver initialisation.
	mock.ExpectExec("DROP SCHEMA").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := ResetSchema(context.Background(), p.DB()); err == nil {
		t.Fatal("expected migrator error")
	}
	if _, err := NewMigrator(p.DB().DB); err == nil {
		t.Fatal("expected migrator error")
	}
	if err := RunMigrations(p.DB().DB); err == nil {
		t.Fatal("expected migrations error")
	}
}
