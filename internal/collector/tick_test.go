package collector

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"

	"github.com/lib/pq"
)

func lastSnapshot(t *testing.T, store *dbtest.MemStore) model.LiveSnapshot {
	t.Helper()
	n, ok := store.LastNotification(db.ChannelLive)
	if !ok {
		t.Fatal("no live notification")
	}
	var snap model.LiveSnapshot
	if err := json.Unmarshal([]byte(n.Payload), &snap); err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	return snap
}

func holesOf(t *testing.T, store *dbtest.MemStore) []hole {
	t.Helper()
	rows, err := store.MissingRanges(context.Background(), 4663)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		return nil
	}
	out := make([]hole, len(rows))
	for i, row := range rows {
		out[i], err = holeFromRow(row)
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func blockCountIn(store *dbtest.MemStore, res string) int64 {
	var total int64
	for _, b := range store.BucketRows {
		if b.Resolution == res {
			total += b.Blocks
		}
	}
	return total
}

func TestTickFreshStartAndCatchUp(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)

	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// A fresh database persists the sampled head only: nothing before it
	// has a known state, so nothing before it is replayed.
	blocks, _ := store.RecentBlocks(ctx, 4663, 100)
	if len(blocks) != 1 || blocks[0].Number != 1000 || rpc.calledTimes("HeadersByNumbers") != 0 {
		t.Fatalf("fresh start stored %d blocks (%v), header calls %d", len(blocks), blocks, rpc.calledTimes("HeadersByNumbers"))
	}
	head := blocks[0]
	if !head.Anchored || head.Backlogs[1] != 11_194_391_810_886 || head.Backlogs[0] != 3_111_506 {
		t.Fatalf("head not anchored to the sample: %+v", head)
	}
	if head.Hash != "0x3e8" || head.ParentHash != "0x3e7" || head.MinBaseFee.Wei.Int64() != 20_000_000 {
		t.Fatalf("head row fields: %+v", head)
	}
	// The seed's parent was never replayed, so the block's own facts are stored but its prediction is
	// absent rather than a fabricated exact hit.
	if head.PredictedBaseFee.Valid || head.ConstraintBips != nil || head.ExponentBips != 0 || db.ReplayErrorBips(head) != 0 {
		t.Fatalf("seed carries no prediction: %+v", head)
	}
	if f.Head() != 1000 {
		t.Fatalf("Head() = %d", f.Head())
	}
	n, _ := store.NetworkByRef(ctx, "robinhood")
	if n == nil || n.HeadBlock.Int64 != 1000 || n.DisplayName != "Robinhood Chain" || !n.LastSampleAt.Valid {
		t.Fatalf("network row: %+v", n)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateHead); v != "1000" {
		t.Fatalf("head checkpoint = %q", v)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateLiveStart); v != `{"block":1000,"ts":`+big.NewInt(int64(tsFor(1000))).String()+`}` {
		t.Fatalf("live start = %q", v)
	}
	if holes := holesOf(t, store); len(holes) != 0 {
		t.Fatalf("no hole on a fresh start: %+v", holes)
	}
	if store.BucketCount(4663, db.Resolution1m) != 1 || store.BucketCount(4663, db.Resolution1h) != 1 {
		t.Fatal("buckets not built")
	}
	bk, _ := store.Buckets(ctx, 4663, db.Resolution1m, baseTime, baseTime.Add(time.Hour))
	if bk[0].Blocks != 1 || bk[0].LastBlock != 1000 || bk[0].BacklogsEnd[1] != 11_194_391_810_886 || bk[0].FeesWei.Sign() <= 0 {
		t.Fatalf("bucket: %+v", bk[0])
	}
	if bk[0].MinBaseFee.Wei.Int64() != 20_000_000 || bk[0].FloorFeesWei.Wei.Int64() != 20_000_000*int64(gasFor(1000)) || bk[0].SurplusFeesWei.Wei.Int64() != (feeFor(1000).Int64()-20_000_000)*int64(gasFor(1000)) || !bk[0].BaseFeeSum.Valid {
		t.Fatalf("bucket fee split: %+v", bk[0])
	}
	sample, _ := store.LatestStateSample(ctx, 4663, false)
	if sample == nil || sample.BlockNumber != 1000 || sample.L1 != nil {
		t.Fatalf("sample: %+v", sample)
	}
	snap := lastSnapshot(t, store)
	if snap.ChainID != 4663 || snap.Model != model.ModelConstraints || len(snap.Constraints) != 2 || snap.Block.Number != 1000 {
		t.Fatalf("snapshot: %+v", snap)
	}
	if snap.ExponentBips != 32_425 || snap.Constraints[1].ExponentBips != 32_391 || snap.MinBaseFee != "20000000" || snap.MultiplierBips != 10_000 {
		t.Fatalf("snapshot pricer values: %+v", snap)
	}
	if snap.Prices.PerArbGasTotal != "6" || snap.GasPerSecond.S10 == 0 || snap.L1 != nil || snap.Legacy != nil {
		t.Fatalf("snapshot prices/gps: %+v", snap)
	}
	if f.Snapshot() == nil || f.Snapshot().Block.Number != 1000 {
		t.Fatal("Snapshot() not cached")
	}

	// Same head: sample only, no new blocks, but a new notification.
	before := len(store.Notifications)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if blocks, _ := store.RecentBlocks(ctx, 4663, 100); len(blocks) != 1 {
		t.Fatal("sample-only tick must not add blocks")
	}
	if len(store.Notifications) != before+1 {
		t.Fatal("sample-only tick must notify")
	}

	// Head advances by 25 with a batch size of 10: three header batches.
	rpc.setHead(1025)
	rpc.headerCalls = nil
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rpc.headerCalls) != 3 || len(rpc.headerCalls[0]) != 10 || len(rpc.headerCalls[2]) != 4 || rpc.headerCalls[0][0] != 1001 {
		t.Fatalf("header batches = %v", rpc.headerCalls)
	}
	blocks, _ = store.RecentBlocks(ctx, 4663, 100)
	if len(blocks) != 26 || blocks[0].Number != 1025 {
		t.Fatalf("after catch-up: %d blocks, head %d", len(blocks), blocks[0].Number)
	}
	// Replay continuity: block 1001 drained by dt=0 from the anchored 1000
	// state, so its backlogs are the anchored ones plus its own gas, and
	// its start-of-block bips are the anchored exponents.
	var b1001 db.Block
	for _, b := range blocks {
		if b.Number == 1001 {
			b1001 = b
		}
	}
	if b1001.Backlogs[0] != 3_111_506+gasFor(1001) || b1001.Anchored {
		t.Fatalf("block 1001 replay: %+v", b1001)
	}
	// 1001 is priced by the seed's start-of-block state, one block of gas below the anchored backlog:
	// 2_111_500 gas over the 15 s constraint's 900 M is 23 bips, against the anchored 3_111_506's 34.
	if b1001.ConstraintBips[0] != 23 || b1001.ConstraintBips[1] != 32_391 || b1001.ExponentBips != 32_414 {
		t.Fatalf("block 1001 constraint bips: %+v", b1001)
	}
	// The anchored exponents price 1002, the block the replay at 1001 computed them for.
	var b1002 db.Block
	for _, b := range blocks {
		if b.Number == 1002 {
			b1002 = b
		}
	}
	if b1002.ConstraintBips[0] != 34 || b1002.ConstraintBips[1] != 32_391 || b1002.ExponentBips != 32_425 {
		t.Fatalf("block 1002 constraint bips: %+v", b1002)
	}
	// Buckets are rebuilt from rows: exactly one contribution per block.
	if blockCountIn(store, db.Resolution1m) != 26 || blockCountIn(store, db.Resolution1h) != 26 {
		t.Fatalf("bucket block counts: 1m %d 1h %d", blockCountIn(store, db.Resolution1m), blockCountIn(store, db.Resolution1h))
	}

	// Head going backwards is ignored.
	rpc.setHead(1020)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.Head() != 1025 {
		t.Fatal("head must not move backwards")
	}
}

// TestTickPersistsRPCCapacity records the live call rate the observed block
// interval required. A skipped gap is an explicit saturated-capacity signal,
// not just a warning hidden in collector logs.
func TestTickPersistsRPCCapacity(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.setHead(gapHead)
	rpc.mu.Lock()
	rpc.sampledAt = baseTime.Add(time.Second)
	rpc.mu.Unlock()
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	raw, ok, err := store.GetState(ctx, 4663, db.StateRPCCapacity)
	if err != nil || !ok {
		t.Fatalf("capacity checkpoint: %q %v %v", raw, ok, err)
	}
	var capacity model.RPCCapacity
	if err := json.Unmarshal([]byte(raw), &capacity); err != nil {
		t.Fatal(err)
	}
	if !capacity.Saturated || capacity.ConfiguredCallsPerSecond != 4 || capacity.RequiredCallsPerSecond != 304 || capacity.HeadroomCallsPerSecond == nil || *capacity.HeadroomCallsPerSecond != -300 {
		t.Fatalf("rpc capacity: %+v", capacity)
	}
}

func TestTickGapSkipAndParameterChange(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Gap of 150 with batch 10 (max catch-up 100): the state before the
	// retained blocks is unknown, so only the sampled head is persisted
	// and the hole is recorded.
	rpc.setHead(1150)
	rpc.headerCalls = nil
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	blocks, _ := store.BlocksAfter(ctx, 4663, 1000, 1000)
	if len(blocks) != 1 || blocks[0].Number != 1150 || !blocks[0].Anchored || len(rpc.headerCalls) != 0 {
		t.Fatalf("gap skip: %d blocks from %d, header calls %v", len(blocks), blocks[0].Number, rpc.headerCalls)
	}
	holes := holesOf(t, store)
	if len(holes) != 1 || holes[0].From != 1001 || holes[0].To != 1149 || holes[0].At == "" {
		t.Fatalf("hole: %+v", holes)
	}
	// The owner changes the constraint set without a recorded action: the
	// range cannot be replayed, so the head is seeded again and the
	// snapshot reflects the new shape.
	rpc.mu.Lock()
	rpc.constraints = []nitro.Constraint{{Target: 60_000_000, Window: 15, Backlog: 7}}
	rpc.mu.Unlock()
	rpc.setHead(1152)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	snap := lastSnapshot(t, store)
	if len(snap.Constraints) != 1 || snap.Constraints[0].Backlog != 7 {
		t.Fatalf("snapshot after change: %+v", snap)
	}
	latest, _ := store.LatestBlock(ctx, 4663)
	if len(latest.Backlogs) != 1 || latest.Number != 1152 {
		t.Fatalf("block backlogs after change: %+v", latest)
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1151); b != nil {
		t.Fatal("block 1151 has no known state and must not be stored")
	}
	if holes := holesOf(t, store); len(holes) != 2 || holes[1].From != 1151 || holes[1].To != 1151 {
		t.Fatalf("second hole: %+v", holes)
	}
	// Two log reads: the skipped gap is scanned for owner actions before
	// the head is seeded (nothing may be delayed to the slow loop just
	// because the blocks between were not replayed), and the shape change
	// looks for the action that explains it.
	if rpc.calledTimes("OwnerActsLogs") != 2 {
		t.Fatalf("a skipped gap and a shape change must both look for owner actions: %d log calls", rpc.calledTimes("OwnerActsLogs"))
	}
	if got := rpc.logRanges[0]; got != [2]uint64{1001, 1150} {
		t.Fatalf("the skipped interval must be scanned for owner actions: %v", rpc.logRanges)
	}
	// A change at the head itself (same height) re-anchors without a hole.
	rpc.mu.Lock()
	rpc.constraints = []nitro.Constraint{{Target: 60_000_000, Window: 15, Backlog: 9}, {Target: 1, Window: 1, Backlog: 2}}
	rpc.mu.Unlock()
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if snap := lastSnapshot(t, store); len(snap.Constraints) != 2 || len(holesOf(t, store)) != 2 {
		t.Fatalf("same-height shape change: %+v", snap)
	}
}

// TestTickSplitsAtOwnerAction: a constraint set change late in a block is
// located through the owner log and its receipt. Gas from earlier
// transactions stays on the old state, while the action transaction and
// the gas after it are added to the reset state.
func TestTickSplitsAtOwnerAction(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if rpc.calledTimes("TransactionReceipts") != 0 {
		t.Fatal("a block range without owner pricing actions must not fetch receipts")
	}
	// setGasPricingConstraints at block 1005 switches to one constraint
	// with a starting backlog of 5. The action is the last of three
	// transactions, with 800,000 gas consumed before it.
	newSet := []nitro.ConstraintParam{{GasTargetPerSecond: 60_000_000, AdjustmentWindowSeconds: 15, StartingBacklog: 5}}
	calldata := nitro.EncodeSetGasPricingConstraints(newSet)
	action := ownerLog(1005, calldata)
	action.TxIndex = 2
	rpc.logs = []nitro.Log{action}
	rpc.receipts[action.TxHash] = nitro.Receipt{
		TxHash: action.TxHash, BlockNumber: action.BlockNumber, TxIndex: action.TxIndex,
		GasUsed: 100_000, CumulativeGasUsed: 900_000,
	}
	afterReset := gasFor(1005) - 800_000
	rpc.mu.Lock()
	rpc.constraints = []nitro.Constraint{{Target: 60_000_000, Window: 15, Backlog: 5 + afterReset + gasFor(1006) + gasFor(1007) + gasFor(1008)}}
	rpc.mu.Unlock()
	rpc.setHead(1008)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if rpc.calledTimes("OwnerActsLogs") != 1 || rpc.logRanges[0] != [2]uint64{1001, 1008} {
		t.Fatalf("owner log scan: %d calls %v", rpc.calledTimes("OwnerActsLogs"), rpc.logRanges)
	}
	if rpc.calledTimes("TransactionReceipts") != 1 {
		t.Fatalf("only the action block should need receipts: %d calls", rpc.calledTimes("TransactionReceipts"))
	}
	blocks, _ := store.BlocksAfter(ctx, 4663, 1000, 100)
	if len(blocks) != 8 {
		t.Fatalf("blocks = %d", len(blocks))
	}
	for _, b := range blocks {
		if b.Number < 1005 && len(b.Backlogs) != 2 {
			t.Fatalf("block %d should keep the old set: %+v", b.Number, b)
		}
		if b.Number >= 1005 && len(b.Backlogs) != 1 {
			t.Fatalf("block %d should use the new set: %+v", b.Number, b)
		}
	}
	// The 800,000 gas before the late reset is discarded with the old
	// backlog. Only the action transaction and the remainder of its block
	// survive on top of the starting backlog.
	if blocks[4].Backlogs[0] != 5+afterReset || db.ReplayErrorBips(blocks[7]) != 0 && !blocks[7].Anchored {
		t.Fatalf("block 1005: %+v", blocks[4])
	}
	// The seed recorded the live shape as an observed set at 1000 (no set
	// was known); the action's set follows it.
	sets, _ := store.ConstraintSets(ctx, 4663)
	if len(sets) != 2 || sets[0].EffectiveBlock != 1000 || sets[0].Source != model.SourceObserved || sets[1].EffectiveBlock != 1005 || sets[1].Source != model.SourceOwnerAction {
		t.Fatalf("constraint set recorded by the fast loop: %+v", sets)
	}
	acts, _ := store.OwnerActions(ctx, 4663, time.Time{}, time.Time{}, 0)
	if len(acts) != 1 || !acts[0].TxIndex.Valid || acts[0].TxIndex.Int64 != 2 {
		t.Fatalf("owner action transaction index: %+v", acts)
	}
	if holes := holesOf(t, store); len(holes) != 0 {
		t.Fatalf("no hole expected: %+v", holes)
	}
	// A minimum fee change at 1012 splits the fee too.
	minFee := big.NewInt(30_000_000)
	sel := nitro.Selector("setMinimumL2BaseFee(uint256)")
	rpc.logs = append(rpc.logs, ownerLog(1012, append(sel[:], padTo32(minFee.Bytes())...)))
	rpc.minFee = minFee
	rpc.setHead(1014)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	b1011, _ := store.BlockByNumber(ctx, 4663, 1011)
	b1012, _ := store.BlockByNumber(ctx, 4663, 1012)
	b1013, _ := store.BlockByNumber(ctx, 4663, 1013)
	if b1011.MinBaseFee.Wei.Int64() != 20_000_000 || b1012.MinBaseFee.Wei.Int64() != 20_000_000 || b1013.MinBaseFee.Wei.Int64() != 30_000_000 {
		t.Fatalf("min fee split: %s then %s then %s", b1011.MinBaseFee.Wei, b1012.MinBaseFee.Wei, b1013.MinBaseFee.Wei)
	}
	if f.minFeeAt(1011).Int64() != pricer.InitialMinimumBaseFeeWei || f.minFeeAt(1012).Int64() != pricer.InitialMinimumBaseFeeWei || f.minFeeAt(1013).Int64() != 30_000_000 {
		t.Fatalf("minFeeAt: %s %s %s", f.minFeeAt(1011), f.minFeeAt(1012), f.minFeeAt(1013))
	}
	// A failing owner log fetch fails the tick without moving the head.
	rpc.mu.Lock()
	rpc.constraints = []nitro.Constraint{{Target: 1, Window: 1, Backlog: 1}}
	rpc.mu.Unlock()
	rpc.errs["OwnerActsLogs"] = errRPC
	rpc.setHead(1016)
	if err := f.Tick(ctx); !errors.Is(err, errRPC) || f.Head() != 1014 {
		t.Fatalf("log failure: %v head %d", err, f.Head())
	}
}

// TestTickCatchUpBoundaries: every catch-up interval is scanned for owner
// actions and the replay splits at each pricing change in it, even when
// the sampled head looks exactly like the stored state: a constraint set
// with the same shape (a backlog reset), a change that is changed back,
// a fee change that is changed back, and a legacy parameter change.
func TestTickCatchUpBoundaries(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	same := []nitro.ConstraintParam{{GasTargetPerSecond: 60_000_000, AdjustmentWindowSeconds: 15, StartingBacklog: 11}, {GasTargetPerSecond: 40_000_000, AdjustmentWindowSeconds: 86_400, StartingBacklog: 22}}
	other := []nitro.ConstraintParam{{GasTargetPerSecond: 1, AdjustmentWindowSeconds: 1, StartingBacklog: 0}}
	feeSel := nitro.Selector("setMinimumL2BaseFee(uint256)")
	rpc.logs = []nitro.Log{
		ownerLog(1003, nitro.EncodeSetGasPricingConstraints(same)),                    // reset, shape unchanged
		ownerLog(1005, nitro.EncodeSetGasPricingConstraints(other)),                   // change
		ownerLog(1007, nitro.EncodeSetGasPricingConstraints(same)),                    // and back
		ownerLog(1008, append(feeSel[:], padTo32(big.NewInt(30_000_000).Bytes())...)), // fee up
		ownerLog(1009, append(feeSel[:], padTo32(big.NewInt(20_000_000).Bytes())...)), // and back
	}
	rpc.setHead(1010)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if rpc.calledTimes("OwnerActsLogs") != 1 || rpc.logRanges[0] != [2]uint64{1001, 1010} {
		t.Fatalf("the interval must be scanned once: %d %v", rpc.calledTimes("OwnerActsLogs"), rpc.logRanges)
	}
	if holes := holesOf(t, store); len(holes) != 0 {
		t.Fatalf("no hole expected: %+v", holes)
	}
	blocks, _ := store.BlocksAfter(ctx, 4663, 1000, 100)
	if len(blocks) != 10 {
		t.Fatalf("blocks = %d", len(blocks))
	}
	b := func(n uint64) db.Block { return blocks[n-1001] }
	if b(1002).Backlogs[0] != 3_111_506+gasFor(1001)+gasFor(1002) || b(1003).Backlogs[0] != 11+gasFor(1003) || b(1003).Backlogs[1] != 22+gasFor(1003) {
		t.Fatalf("a same-shape set must reset the backlogs at its block: %v -> %v", b(1002).Backlogs, b(1003).Backlogs)
	}
	if len(b(1004).Backlogs) != 2 || len(b(1005).Backlogs) != 1 || len(b(1006).Backlogs) != 1 || len(b(1007).Backlogs) != 2 || b(1007).Backlogs[0] != 11+gasFor(1007) {
		t.Fatalf("change and change back: %v %v %v %v", b(1004).Backlogs, b(1005).Backlogs, b(1006).Backlogs, b(1007).Backlogs)
	}
	if b(1007).MinBaseFee.Wei.Int64() != 20_000_000 || b(1008).MinBaseFee.Wei.Int64() != 20_000_000 || b(1009).MinBaseFee.Wei.Int64() != 30_000_000 || b(1010).MinBaseFee.Wei.Int64() != 20_000_000 {
		t.Fatalf("fee change and change back: %s %s %s", b(1007).MinBaseFee.Wei, b(1008).MinBaseFee.Wei, b(1009).MinBaseFee.Wei)
	}
	sets, _ := store.ConstraintSets(ctx, 4663)
	if len(sets) != 4 || sets[1].EffectiveBlock != 1003 || sets[3].EffectiveBlock != 1007 {
		t.Fatalf("every set action is recorded, none observed: %+v", sets)
	}
	if acts, _ := store.OwnerActions(ctx, 4663, time.Time{}, time.Time{}, 0); len(acts) != 5 {
		t.Fatalf("actions recorded with the tick: %d", len(acts))
	}
	// The actions commit with the tick, in its transaction: when recording
	// them fails nothing of the tick is written and nobody is notified,
	// the caches stay put, and the retry records them once.
	rpc.logs = append(rpc.logs, ownerLog(1012, append(feeSel[:], padTo32(big.NewInt(25_000_000).Bytes())...)))
	rpc.minFee = big.NewInt(25_000_000)
	rpc.setHead(1013)
	notified := len(store.Notifications)
	store.FailOn["InsertOwnerActions"] = true
	if err := f.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("expected injected error, got %v", err)
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1011); b != nil || len(store.Notifications) != notified || f.Head() != 1010 {
		t.Fatalf("nothing of the tick may commit without its actions: block %+v, %d notifications", b, len(store.Notifications)-notified)
	}
	if acts, _ := store.OwnerActions(ctx, 4663, time.Time{}, time.Time{}, 0); len(acts) != 5 || f.minFeeAt(1012).Int64() != 20_000_000 {
		t.Fatalf("caches must not advance before the commit: %d actions, fee %s", len(acts), f.minFeeAt(1012))
	}
	delete(store.FailOn, "InsertOwnerActions")
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if acts, _ := store.OwnerActions(ctx, 4663, time.Time{}, time.Time{}, 0); len(acts) != 6 || f.minFeeAt(1012).Int64() != 20_000_000 || f.minFeeAt(1013).Int64() != 25_000_000 {
		t.Fatalf("retry records the action: %d actions, fees %s then %s", len(acts), f.minFeeAt(1012), f.minFeeAt(1013))
	}
	b1012, _ := store.BlockByNumber(ctx, 4663, 1012)
	if b1012.MinBaseFee.Wei.Int64() != 20_000_000 {
		t.Fatalf("fee split on the retry: %+v", b1012)
	}
	// A fee change nothing explains (no action in the interval) is a hole,
	// like a shape change.
	rpc.minFee = big.NewInt(40_000_000)
	rpc.setHead(1015)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].From != 1014 || holes[0].To != 1014 {
		t.Fatalf("unexplained fee change: %+v", holes)
	}

	// Legacy: a speed limit change inside the interval splits the replay,
	// so the block after the change drains at the new rate.
	rpcL := newFakeRPC(1000)
	lp := legacyParams()
	lp.Backlog = 5
	rpcL.legacy = &lp
	storeL := dbtest.New()
	fl := newTestFollower(t, rpcL, storeL)
	if err := fl.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	limitSel := nitro.Selector("setSpeedLimit(uint64)")
	rpcL.logs = []nitro.Log{ownerLog(1015, append(limitSel[:], padTo32(big.NewInt(1_000_000).Bytes())...))}
	rpcL.mu.Lock()
	rpcL.legacy.SpeedLimit = 1_000_000
	rpcL.mu.Unlock()
	rpcL.setHead(1021)
	if err := fl.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if holes := holesOf(t, storeL); len(holes) != 0 {
		t.Fatalf("a recorded legacy change needs no hole: %+v", holes)
	}
	b1009, _ := storeL.BlockByNumber(ctx, 4663, 1009)
	b1010, _ := storeL.BlockByNumber(ctx, 4663, 1010)
	b1019, _ := storeL.BlockByNumber(ctx, 4663, 1019)
	b1020, _ := storeL.BlockByNumber(ctx, 4663, 1020)
	if b1010.Backlogs[0] != pricer.SaturatingUSub(b1009.Backlogs[0], 7_000_000)+gasFor(1010) {
		t.Fatalf("before the change the old speed limit drains: %v -> %v", b1009.Backlogs, b1010.Backlogs)
	}
	if b1020.Backlogs[0] != pricer.SaturatingUSub(b1019.Backlogs[0], 1_000_000)+gasFor(1020) {
		t.Fatalf("after the change the new speed limit drains: %v -> %v", b1019.Backlogs, b1020.Backlogs)
	}
	fl.mu.Lock()
	if len(fl.legacyChanges) != 1 || fl.legacyChanges[0].block != 1015 || fl.state.Legacy.SpeedLimit != 1_000_000 {
		t.Fatalf("legacy change cache: %+v state %+v", fl.legacyChanges, fl.state.Legacy)
	}
	fl.mu.Unlock()
	// A restarted follower rebuilds the legacy state at the stored head
	// with the recorded changes applied.
	rpcL.setHead(1022)
	fl2 := newTestFollower(t, rpcL, storeL)
	if err := fl2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	b1022, _ := storeL.BlockByNumber(ctx, 4663, 1022)
	if b1022 == nil || len(holesOf(t, storeL)) != 0 {
		t.Fatalf("restart with a recorded legacy change: %+v %+v", b1022, holesOf(t, storeL))
	}
	// An unrecorded legacy change is still a hole.
	rpcL.mu.Lock()
	rpcL.legacy.Inertia = 7
	rpcL.mu.Unlock()
	rpcL.setHead(1024)
	if err := fl2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if holes := holesOf(t, storeL); len(holes) != 1 || holes[0].From != 1023 {
		t.Fatalf("unexplained legacy change: %+v", holes)
	}
	// Decoding helpers: unknown methods and malformed arguments are ignored.
	if _, ok := legacyChangeOf(1, methodSetL2GasPricingInertia, db.JSONB(`{"sec":3}`)); !ok {
		t.Fatal("inertia change")
	}
	if _, ok := legacyChangeOf(1, methodSetL2GasBacklogTolerance, db.JSONB(`{"limit":3}`)); ok {
		t.Fatal("tolerance without sec")
	}
	if _, ok := legacyChangeOf(1, "setSpeedLimit", db.JSONB(`{bad`)); ok {
		t.Fatal("bad args")
	}
	if _, ok := legacyChangeOf(1, "addChainOwner", db.JSONB(`{}`)); ok {
		t.Fatal("unrelated method")
	}
	if _, ok := minFeeChangeOf(1, methodSetMinimumL2BaseFee, db.JSONB(`{"priceInWei":"x"}`)); ok {
		t.Fatal("bad fee")
	}
	if _, ok := minFeeChangeOf(1, methodSetMinimumL2BaseFee, db.JSONB(`{bad`)); ok {
		t.Fatal("bad fee args")
	}
	tl := &timeline{}
	tl.applyAt(&pricer.State{}, 1)
	if tl.legacyAt(1, nil) != nil || tl.boundaryAt(1) {
		t.Fatal("empty timeline")
	}
}

// TestTickReorgResetsBackfill: a reorg whose ancestor lies below the top
// of the backfill's range restarts the backfill from a fresh cursor with
// its backfill-only buckets dropped; a shallow reorg leaves it alone.
func TestTickReorgResetsBackfill(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	// The backfill only replays from a known constraint set, so the sets
	// are recorded before it starts filling rows below the live start.
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	f.cfg.BackfillDepth = config.Depth(30 * time.Second) // block 700
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, f, 100)
	c, _ := f.loadCursor(ctx)
	if !c.Done || c.Top != 1000 {
		t.Fatalf("cursor: %+v", c)
	}
	// A shallow reorg at the head leaves the cursor alone.
	rpc.setHead(1002)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.fork(1002, "s")
	rpc.setHead(1003)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if c2, _ := f.loadCursor(ctx); !c2.Done || c2.Top != 1000 {
		t.Fatalf("shallow reorg must keep the cursor: %+v", c2)
	}
	// A reorg below the first live block: the ancestor is 994, below the
	// backfill's top (1000), so the backfill starts over and the live
	// start is re-established at the next commit.
	rpc.fork(995, "d")
	rpc.setHead(1005)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.Head() != 1005 {
		t.Fatalf("head = %d", f.Head())
	}
	c3, _ := f.loadCursor(ctx)
	if c3.Done || c3.Active || c3.Top != 0 || c3.DepthStart != 0 {
		t.Fatalf("backfill must restart after a reorg below its top: %+v", c3)
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateLiveStart); v != `{"block":1005,"ts":`+big.NewInt(int64(tsFor(1005))).String()+`}` {
		t.Fatalf("live start after a rewind below it: %q", v)
	}
	for n := uint64(995); n <= 1005; n++ {
		if b, _ := store.BlockByNumber(ctx, 4663, n); b == nil || b.Hash != rpc.hashFor(n) {
			t.Fatalf("block %d not canonical: %+v", n, b)
		}
	}
	// The restarted backfill finishes from the surviving rows.
	runBackfill(t, f, 100)
	if c4, _ := f.loadCursor(ctx); !c4.Done {
		t.Fatalf("restarted backfill: %+v", c4)
	}
	// A corrupt cursor fails the rewind; so does a failing live start
	// delete.
	_ = store.SetState(ctx, 4663, db.StateBackfillCursor, "{bad")
	if _, err := f.rewindBackfill(ctx, store, 1, time.Time{}, false); err == nil {
		t.Fatal("corrupt cursor")
	}
	_ = store.SetState(ctx, 4663, db.StateBackfillCursor, `{"top":50}`)
	store.FailOn["DeleteBucketsBefore"] = true
	if _, err := f.rewindBackfill(ctx, store, 1, baseTime, true); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("bucket delete failure: %v", err)
	}
	delete(store.FailOn, "DeleteBucketsBefore")
	store.FailOn["GetState"] = true
	if _, err := f.rewindBackfill(ctx, store, 1, baseTime, true); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("cursor read failure: %v", err)
	}
	delete(store.FailOn, "GetState")
	rpc.fork(1004, "e")
	rpc.setHead(1006)
	f.mu.Lock()
	f.liveStart = &liveStart{Block: 1005, TS: int64(tsFor(1005))}
	f.mu.Unlock()
	store.FailOn["DeleteState"] = true
	if err := f.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("live start delete failure: %v", err)
	}
	delete(store.FailOn, "DeleteState")
}

// ownerLog builds an OwnerActs log at a block carrying calldata.
func ownerLog(block uint64, calldata []byte) nitro.Log {
	sel := calldata[:4]
	data := append(padTo32(big.NewInt(32).Bytes()), padTo32(big.NewInt(int64(len(calldata))).Bytes())...)
	data = append(data, calldata...)
	if pad := (32 - len(calldata)%32) % 32; pad > 0 {
		data = append(data, make([]byte, pad)...)
	}
	return nitro.Log{
		Address: nitro.ArbOwnerAddress,
		Topics: []string{nitro.OwnerActsTopic, nitro.EncodeHex(append(append([]byte{}, sel...), make([]byte, 28)...)),
			"0x0000000000000000000000002a153c6a1b66dbc930a8d7017230ab0253005c09"},
		Data: data, BlockNumber: block, BlockTimestamp: tsFor(block), TxHash: "0xowner" + big.NewInt(int64(block)).String(), LogIndex: 0,
	}
}

func padTo32(b []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func TestTickLegacy(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(500)
	rpc.legacy = &nitro.LegacyParams{SpeedLimit: 7_000_000, Inertia: 102, Tolerance: 10, Backlog: 5}
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	snap := lastSnapshot(t, store)
	if snap.Model != model.ModelLegacy || snap.Legacy == nil || snap.Legacy.Backlog != 5 || len(snap.Constraints) != 0 {
		t.Fatalf("legacy snapshot: %+v", snap)
	}
	latest, _ := store.LatestBlock(ctx, 4663)
	if len(latest.Backlogs) != 1 || latest.Backlogs[0] != 5 || !latest.Anchored {
		t.Fatalf("legacy block: %+v", latest)
	}
	sample, _ := store.LatestStateSample(ctx, 4663, false)
	if sample.Legacy == nil {
		t.Fatal("legacy json missing")
	}
	// Restarting a follower resumes from the stored head and backlogs.
	rpc.setHead(505)
	f2 := newTestFollower(t, rpc, store)
	if err := f2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f2.Head() != 505 {
		t.Fatalf("resumed head = %d", f2.Head())
	}
	blocks, _ := store.BlocksAfter(ctx, 4663, 500, 10)
	if len(blocks) != 5 || blocks[0].Backlogs[0] != 5+gasFor(501) {
		t.Fatalf("resumed blocks = %+v", blocks)
	}
}

func TestTickErrors(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)

	rpc.errs["FastSample"] = errRPC
	if err := f.Tick(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("expected rpc error, got %v", err)
	}
	n, _ := store.NetworkByRef(ctx, "robinhood")
	if n == nil || !n.LastError.Valid {
		t.Fatal("error must be recorded on the network row")
	}
	delete(rpc.errs, "FastSample")

	for _, method := range []string{"UpsertBlocks", "RebuildBuckets", "GasBetween", "InsertStateSample", "UpdateNetworkHead", "SetState", "Notify"} {
		store.FailOn[method] = true
		if err := f.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
			t.Fatalf("%s: expected injected error, got %v", method, err)
		}
		delete(store.FailOn, method)
	}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.setHead(1005)
	rpc.errs["HeadersByNumbers"] = errRPC
	if err := f.Tick(ctx); !errors.Is(err, errRPC) {
		t.Fatalf("expected header error, got %v", err)
	}
	delete(rpc.errs, "HeadersByNumbers")
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Errors are cleared on success.
	n, _ = store.NetworkByRef(ctx, "robinhood")
	if n.LastError.Valid {
		t.Fatal("error should clear after a good tick")
	}
	// Sample-only path with a failing store.
	store.FailOn["Notify"] = true
	if err := f.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("sample-only failure: %v", err)
	}
	delete(store.FailOn, "Notify")
	// A canceled context does not try to record the error.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	rpc.errs["FastSample"] = errRPC
	if err := f.Tick(cctx); err == nil {
		t.Fatal("expected error")
	}
	delete(rpc.errs, "FastSample")
	// A failing reload after a failed tick is logged, not fatal.
	store.FailOn["Notify"] = true
	store.FailOn["LatestBlock"] = true
	if err := f.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("reload failure: %v", err)
	}
	delete(store.FailOn, "Notify")
	delete(store.FailOn, "LatestBlock")
	// Init failures.
	for _, method := range []string{"UpsertNetwork", "LatestBlock", "ConstraintSets", "OwnerActions", "GetState"} {
		fresh := dbtest.New()
		fresh.FailOn[method] = true
		if err := newTestFollower(t, rpc, fresh).Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
			t.Fatalf("init %s failure: %v", method, err)
		}
	}
	// A database written before the live_start checkpoint derives it from
	// the oldest block; a corrupt checkpoint is an error.
	old := dbtest.New()
	_ = old.UpsertBlocks(ctx, []db.Block{{ChainID: 4663, Number: 990, TS: baseTime.Add(99 * time.Second), BaseFee: db.WeiFromUint64(1), PredictedBaseFee: db.NullWeiFromUint64(1), Backlogs: db.Uint64Array{1, 2}}})
	fo := newTestFollower(t, rpc, old)
	if err := fo.ensureInit(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := old.GetState(ctx, 4663, db.StateLiveStart); v == "" || fo.liveStart == nil || fo.liveStart.Block != 990 {
		t.Fatalf("derived live start: %q %+v", v, fo.liveStart)
	}
	old.FailOn["OldestBlock"] = true
	if err := newTestFollower(t, rpc, dbtest.New()).loadLiveStartLocked(ctx); err != nil {
		t.Fatal(err)
	}
	bad := dbtest.New()
	_ = bad.SetState(ctx, 4663, db.StateLiveStart, "{bad")
	if err := newTestFollower(t, rpc, bad).Tick(ctx); err == nil {
		t.Fatal("corrupt live start should fail")
	}
	bad2 := dbtest.New()
	_ = bad2.UpsertBlocks(ctx, []db.Block{{ChainID: 4663, Number: 1, TS: baseTime}})
	bad2.FailOn["OldestBlock"] = true
	if err := newTestFollower(t, rpc, bad2).Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("oldest block failure: %v", err)
	}
	bad2.FailOn["OldestBlock"] = false
	bad2.FailOn["SetState"] = true
	if err := newTestFollower(t, rpc, bad2).Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("save live start failure: %v", err)
	}
}

func TestTickUsesReceiptPosterGasForReplay(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	seed, err := store.BlockByNumber(ctx, 4663, 1000)
	if err != nil || seed == nil {
		t.Fatalf("seed: %+v %v", seed, err)
	}
	rpc.posterGas = func(n uint64) uint64 {
		if n == 1001 {
			return 767
		}
		return 0
	}
	rpc.setHead(1002)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := store.BlockByNumber(ctx, 4663, 1001)
	if err != nil || row == nil {
		t.Fatalf("block 1001: %+v %v", row, err)
	}
	want := seed.Backlogs[0] + gasFor(1001) - 767
	if row.Backlogs[0] != want || !row.PosterGas.Valid || row.PosterGas.Int64 != 767 {
		t.Fatalf("compute-gas replay: %+v, want backlog %d", row, want)
	}
	snap := lastSnapshot(t, store)
	if snap.ComputeGasPerSecond.S10 == nil || snap.GasPerSecond.S10 <= *snap.ComputeGasPerSecond.S10 {
		t.Fatalf("live rates must preserve total beside compute: %+v", snap)
	}
}

// TestTickNothingAdvancesBeforeCommit: a failed transaction leaves the
// follower's head, replay state and snapshot untouched, the head is
// reloaded from the database (which may have committed after all), and
// the retry folds every block exactly once.
func TestTickNothingAdvancesBeforeCommit(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	before := f.state.Clone()
	f.mu.Unlock()
	rpc.setHead(1010)
	store.FailOn["UpsertBlocks"] = true
	if err := f.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("expected injected error, got %v", err)
	}
	f.mu.Lock()
	head, st, sampled := f.head, f.state, f.lastSample
	f.mu.Unlock()
	if head != 1000 || f.Snapshot().Block.Number != 1000 {
		t.Fatalf("head advanced before commit: %d", head)
	}
	if sampled == nil || sampled.Header.Number != 1000 {
		t.Fatalf("the sample must not be published before the commit: %+v", sampled)
	}
	if st != nil {
		t.Fatal("state must be reloaded from the database after a failed commit")
	}
	delete(store.FailOn, "UpsertBlocks")
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	after := f.state
	f.mu.Unlock()
	if f.Head() != 1010 || after == nil {
		t.Fatalf("retry: head %d state %v", f.Head(), after)
	}
	// The retry replayed from the committed state (block 1000's row), so
	// block 1001 starts exactly where the seed left off.
	b1001, _ := store.BlockByNumber(ctx, 4663, 1001)
	if b1001.Backlogs[0] != before.Backlogs()[0]+gasFor(1001) {
		t.Fatalf("retry did not replay from the committed state: %+v vs %v", b1001, before.Backlogs())
	}
	if blockCountIn(store, db.Resolution1m) != 11 || blockCountIn(store, db.Resolution15m) != 11 {
		t.Fatalf("buckets must count each block once: 1m %d 15m %d", blockCountIn(store, db.Resolution1m), blockCountIn(store, db.Resolution15m))
	}
	// An ambiguous commit: the store applied everything but reported an
	// error at the end. The head is reloaded from what was committed and
	// the next tick continues without folding anything twice.
	rpc.setHead(1020)
	store.FailOn["Notify"] = true
	if err := f.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("expected injected error, got %v", err)
	}
	if f.Head() != 1020 {
		t.Fatalf("head must reload from the database: %d", f.Head())
	}
	delete(store.FailOn, "Notify")
	rpc.setHead(1022)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if blockCountIn(store, db.Resolution1m) != 23 || len(store.BlockRows[4663]) != 23 {
		t.Fatalf("buckets after an ambiguous commit count %d blocks over %d rows", blockCountIn(store, db.Resolution1m), len(store.BlockRows[4663]))
	}
	b1021, _ := store.BlockByNumber(ctx, 4663, 1021)
	b1020, _ := store.BlockByNumber(ctx, 4663, 1020)
	if b1021.Backlogs[0] != b1020.Backlogs[0]+gasFor(1021) || b1021.Anchored {
		t.Fatalf("replay after reload: %v -> %v", b1020.Backlogs, b1021.Backlogs)
	}
	// Replaying the same rows again changes nothing.
	rows, _ := store.BlocksAfter(ctx, 4663, 1000, 100)
	_ = store.UpsertBlocks(ctx, rows)
	for _, res := range db.ResolutionOrder {
		_ = store.RebuildBuckets(ctx, 4663, res, db.BucketStarts(rows, res))
	}
	if blockCountIn(store, db.Resolution1m) != 23 {
		t.Fatalf("rebuild is not idempotent: %d", blockCountIn(store, db.Resolution1m))
	}
}

// TestTickReorg: a head whose ancestry does not match the stored chain
// rewinds to the common ancestor in one transaction (blocks, buckets,
// owner actions, constraint sets, batch reports, cursors) and replays the
// canonical chain forward from there.
func TestTickReorg(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.setHead(1006)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Things recorded on the orphaned blocks.
	_, _ = store.InsertOwnerActions(ctx, []db.OwnerAction{{ChainID: 4663, BlockNumber: 1005, TxHash: "0xo", TS: baseTime, Method: "setSpeedLimit"}})
	_, _ = store.InsertConstraintSet(ctx, db.ConstraintSet{ChainID: 4663, EffectiveBlock: 1005, EffectiveAt: baseTime, Source: model.SourceObserved, Constraints: entriesJSON(nil)})
	_ = store.UpsertBatchReports(ctx, []db.BatchReport{{ChainID: 4663, BlockNumber: 1004, BatchTS: baseTime, CostCalculationVersion: 1}})
	_ = store.SetState(ctx, 4663, db.StateOwnerLogCursor, "1006")
	_ = store.SetState(ctx, 4663, db.StateBatchScanCursor, "1006")
	f.mu.Lock()
	_ = f.reloadSetsLocked(ctx)
	f.mu.Unlock()
	// The chain reorganizes at 1003: blocks 1003..1008 now have new hashes.
	rpc.fork(1003, "f")
	rpc.setHead(1008)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.Head() != 1008 {
		t.Fatalf("head = %d", f.Head())
	}
	blocks, _ := store.BlocksAfter(ctx, 4663, 1000, 100)
	if len(blocks) != 8 {
		t.Fatalf("blocks after rewind = %d", len(blocks))
	}
	for _, b := range blocks {
		if b.Hash != rpc.hashFor(b.Number) || b.ParentHash != rpc.hashFor(b.Number-1) {
			t.Fatalf("block %d not canonical: %+v", b.Number, b)
		}
	}
	// The replay continued from the ancestor's committed state.
	b1002, _ := store.BlockByNumber(ctx, 4663, 1002)
	b1003, _ := store.BlockByNumber(ctx, 4663, 1003)
	if b1003.Backlogs[0] != b1002.Backlogs[0]+gasFor(1003) {
		t.Fatalf("replay after rewind: %v -> %v", b1002.Backlogs, b1003.Backlogs)
	}
	if blockCountIn(store, db.Resolution1m) != 9 {
		t.Fatalf("buckets after rewind count %d blocks", blockCountIn(store, db.Resolution1m))
	}
	if acts, _ := store.OwnerActions(ctx, 4663, time.Time{}, time.Time{}, 0); len(acts) != 0 {
		t.Fatalf("owner actions above the ancestor must go: %+v", acts)
	}
	if sets, _ := store.ConstraintSets(ctx, 4663); len(sets) != 1 || sets[0].EffectiveBlock != 1000 {
		t.Fatalf("constraint sets above the ancestor must go, the seed's observed set stays: %+v", sets)
	}
	if len(store.ReportRows) != 0 {
		t.Fatal("batch reports above the ancestor must go")
	}
	for _, key := range []string{db.StateOwnerLogCursor, db.StateBatchScanCursor} {
		if v, _, _ := store.GetState(ctx, 4663, key); v != "1002" {
			t.Fatalf("%s = %q, want 1002", key, v)
		}
	}
	for _, sm := range store.SampleRows {
		if sm.BlockNumber > 1002 && sm.BlockNumber != 1008 {
			t.Fatalf("samples above the ancestor must go: %+v", sm)
		}
	}
	// Same height, different hash: the head itself is replaced.
	rpc.fork(1008, "g")
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1008); b.Hash != "0xg3f0" || f.Head() != 1008 {
		t.Fatalf("same-height reorg: %+v head %d", b, f.Head())
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1007); b.Hash != "0xf3ef" {
		t.Fatalf("block 1007 must survive a reorg at 1008: %+v", b)
	}
	// A reorg deeper than the stored ancestry is refused and retried.
	f.net.CallsPerSecond = 0
	rpc.setHead(1140)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.fork(1, "e")
	rpc.setHead(1141)
	if err := f.Tick(ctx); !errors.Is(err, errReorgTooDeep) {
		t.Fatalf("deep reorg: %v", err)
	}
	if f.Head() != 1140 || len(store.BlockRows[4663]) != 141 {
		t.Fatalf("a refused reorg changes nothing: head %d rows %d", f.Head(), len(store.BlockRows[4663]))
	}
	rpc.mu.Lock()
	rpc.forks = rpc.forks[:2]
	rpc.mu.Unlock()
	// A reorg deeper than the stored rows rewinds to below them and seeds
	// the new head.
	shallow := dbtest.New()
	fs := newTestFollower(t, rpc, shallow)
	if err := fs.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.fork(1100, "d")
	rpc.setHead(1142)
	if err := fs.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if fs.Head() != 1142 || len(shallow.BlockRows[4663]) != 1 || len(holesOf(t, shallow)) != 0 {
		t.Fatalf("rewind below the only row: head %d rows %d", fs.Head(), len(shallow.BlockRows[4663]))
	}
	if b, _ := shallow.BlockByNumber(ctx, 4663, 1142); b == nil || b.Hash != "0xd476" {
		t.Fatalf("seeded head after a rewind: %+v", b)
	}
	// The live start was above the ancestor: it starts over at the seed.
	if v, _, _ := shallow.GetState(ctx, 4663, db.StateLiveStart); v != `{"block":1142,"ts":`+big.NewInt(int64(tsFor(1142))).String()+`}` || fs.liveStart == nil || fs.liveStart.Block != 1142 {
		t.Fatalf("live start after a rewind below it: %q %+v", v, fs.liveStart)
	}
	rpc.mu.Lock()
	rpc.forks = rpc.forks[:2]
	rpc.mu.Unlock()
	// Headers that do not link inside the fetched range fail the tick.
	if err := verifyChain([]nitro.Header{{Number: 1, Hash: "0xa"}, {Number: 2, ParentHash: "0xb"}}); err == nil {
		t.Fatal("unlinked headers")
	}
	if err := verifyChain([]nitro.Header{{Number: 1, Hash: "0xa"}, {Number: 2, ParentHash: "0xa"}, {Number: 3}}); err != nil {
		t.Fatal(err)
	}
	// A row without a stored hash (written before hashes were kept) is
	// unknown, not trusted: it is the ancestor only when the node's header
	// matches every field it holds.
	if n, err := f.findAncestor(ctx, 1139); err != nil || n != 1139 {
		t.Fatalf("ancestor with matching hashes: %d %v", n, err)
	}
	keep := store.BlockRows[4663][1137]
	store.BlockRows[4663][1137] = db.Block{ChainID: 4663, Number: 1137, TS: keep.TS}
	rpc.fork(1137, "x")
	if n, err := f.findAncestor(ctx, 1139); err != nil || n != 1136 {
		t.Fatalf("a hashless row whose fields differ from the node is skipped: %d %v", n, err)
	}
	unhashed := keep
	unhashed.Hash = ""
	store.BlockRows[4663][1137] = unhashed
	if n, err := f.findAncestor(ctx, 1139); err != nil || n != 1137 {
		t.Fatalf("a hashless row whose fields match the node is the ancestor: %d %v", n, err)
	}
	store.BlockRows[4663][1137] = keep
	rpc.mu.Lock()
	rpc.forks = rpc.forks[:2]
	rpc.mu.Unlock()
	if n, err := f.findAncestor(ctx, 0); err != nil || n != 0 {
		t.Fatalf("ancestor at genesis: %d %v", n, err)
	}
	rpc.errs["HeaderByNumber"] = errRPC
	if _, err := f.findAncestor(ctx, 1139); !errors.Is(err, errRPC) {
		t.Fatalf("ancestor header failure: %v", err)
	}
	delete(rpc.errs, "HeaderByNumber")
	// Rewind failures surface and leave the head where it was.
	rpc.fork(1139, "h")
	rpc.setHead(1145)
	for _, method := range []string{"RecentBlocks", "DeleteBlocksAfter", "RewindAfter"} {
		store.FailOn[method] = true
		if err := f.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
			t.Fatalf("%s: %v", method, err)
		}
		// The in-memory store has no rollback, so a failure after the
		// block delete leaves the head at the ancestor; Postgres would
		// keep 1140. Either way the follower reloaded from the store.
		if head := f.Head(); head != 1140 && (method != "RewindAfter" || head != 1138) {
			t.Fatalf("%s: head moved to %d", method, head)
		}
		delete(store.FailOn, method)
	}
	if err := f.Tick(ctx); err != nil || f.Head() != 1145 {
		t.Fatalf("shallow reorg after failures: %v head %d", err, f.Head())
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1138); b.Hash != "0xg472" {
		t.Fatalf("block 1138 is the ancestor and must survive: %+v", b)
	}
	// State samples above the ancestor go with the rewind (a failure there
	// fails the tick), so no sample describes an orphaned block.
	rpc.fork(1145, "j")
	store.FailOn["DeleteStateSamplesAfter"] = true
	if err := f.Tick(ctx); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("DeleteStateSamplesAfter: %v", err)
	}
	delete(store.FailOn, "DeleteStateSamplesAfter")
	if err := f.Tick(ctx); err != nil || f.Head() != 1145 {
		t.Fatalf("same-height reorg after a failure: %v head %d", err, f.Head())
	}
	for _, sm := range store.SampleRows {
		if b, _ := store.BlockByNumber(ctx, 4663, sm.BlockNumber); sm.BlockNumber > 1144 && (b == nil || b.Hash != "0xj479") {
			t.Fatalf("sample at an orphaned block survived: %+v", sm)
		}
	}
	if err := rewindCursor(ctx, store, 4663, "nope", 5); err != nil {
		t.Fatal(err)
	}
	_ = store.SetState(ctx, 4663, "junk", "x")
	if err := rewindCursor(ctx, store, 4663, "junk", 5); err == nil {
		t.Fatal("a corrupt cursor must surface")
	}
	store.FailOn["GetState"] = true
	if err := rewindCursor(ctx, store, 4663, "junk", 5); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatal("GetState failure")
	}
	delete(store.FailOn, "GetState")
}

func TestSlowDataAttachedToNextSample(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.sampleSlow(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sample, _ := store.LatestStateSample(ctx, 4663, true)
	if sample == nil || sample.L1 == nil || sample.Accounts == nil {
		t.Fatalf("slow data not attached: %+v", sample)
	}
	var l1 model.L1
	if err := sample.L1.Unmarshal(&l1); err != nil || l1.BaseFeeEstimate != "2369608" || l1.Surplus != "-5" || l1.PerBatchGasCharge != 210_000 {
		t.Fatalf("l1 json: %+v %v", l1, err)
	}
	snap := lastSnapshot(t, store)
	if snap.L1 == nil || snap.Accounts == nil || snap.Accounts.Network.Balance != "10706" {
		t.Fatalf("snapshot slow data: %+v", snap)
	}
	// The next tick does not re-attach until the slow loop runs again.
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	latest, _ := store.LatestStateSample(ctx, 4663, false)
	if latest.L1 != nil {
		t.Fatal("l1 should only be attached once per slow tick")
	}
}

func TestHelpers(t *testing.T) {
	s := &nitro.Sample{Constraints: []nitro.Constraint{{Target: 1, Window: 2, Backlog: 3}}, MinBaseFee: big.NewInt(9)}
	st := stateFromSample(s)
	if !sameShape(st, s) || sameShape(nil, s) || st.Backlogs()[0] != 3 {
		t.Fatal("constraint shape")
	}
	s2 := &nitro.Sample{Constraints: []nitro.Constraint{{Target: 1, Window: 3}}}
	if sameShape(st, s2) {
		t.Fatal("window change is a different shape")
	}
	leg := &nitro.Sample{Legacy: &nitro.LegacyParams{SpeedLimit: 1, Inertia: 2, Tolerance: 3, Backlog: 4}}
	lst := stateFromSample(leg)
	if !sameShape(lst, leg) || sameShape(st, leg) || sameShape(lst, s) || lst.Backlogs()[0] != 4 {
		t.Fatal("legacy shape")
	}
	if sameShape(lst, &nitro.Sample{Legacy: &nitro.LegacyParams{SpeedLimit: 2, Inertia: 2, Tolerance: 3}}) {
		t.Fatal("legacy param change")
	}
	empty := &nitro.Sample{}
	if stateFromSample(empty).Legacy != nil || sampleBacklogs(empty) != nil || sameShape(stateFromSample(empty), empty) {
		t.Fatal("empty sample")
	}
	if multiplierBips(big.NewInt(30), big.NewInt(10)) != 30_000 || multiplierBips(big.NewInt(1), nil) != 0 || multiplierBips(nil, big.NewInt(1)) != 0 || multiplierBips(big.NewInt(1), big.NewInt(0)) != 0 {
		t.Fatal("multiplierBips")
	}
	if multiplierBips(new(big.Int).Lsh(big.NewInt(1), 90), big.NewInt(1)) != 1<<63-1 {
		t.Fatal("multiplierBips saturation")
	}
	if weiString(nil) != "0" || weiString(big.NewInt(7)) != "7" || bigOrZero(nil).Sign() != 0 || bigOrZero(big.NewInt(3)).Int64() != 3 {
		t.Fatal("weiString / bigOrZero")
	}
	snap := buildSnapshot(1, s, 3, model.GasPerSecond{}, model.NullableGasPerSecond{}, nil, nil, nil)
	if snap.ReplayErrorBips != 3 || snap.MultiplierBips != 0 || snap.MinBaseFee != "9" {
		t.Fatalf("buildSnapshot: %+v", snap)
	}
	nilFee := &nitro.Sample{Constraints: []nitro.Constraint{{Target: 1, Window: 2, Backlog: 3}}}
	if snap := buildSnapshot(1, nilFee, 0, model.GasPerSecond{}, model.NullableGasPerSecond{}, nil, nil, nil); snap.MinBaseFee != "0" {
		t.Fatalf("nil min fee: %+v", snap)
	}
	f := newTestFollower(t, newFakeRPC(1), dbtest.New())
	f.sets = []db.ConstraintSet{{ID: 1, EffectiveBlock: 10}, {ID: 2, EffectiveBlock: 20}}
	if f.setAt(5) != nil {
		t.Fatal("no set before 10")
	}
	if f.setAt(25).ID != 2 {
		t.Fatal("set at 25")
	}
	// The minimum fee before the first recorded change is nitro's genesis
	// default, never the live value.
	f.minFeeChanges = []minFeeChange{{block: 100, fee: big.NewInt(5), pos: actionPosition{txHash: "0xfee"}}}
	if f.minFeeAt(50).Int64() != pricer.InitialMinimumBaseFeeWei || f.minFeeAt(100).Int64() != pricer.InitialMinimumBaseFeeWei || f.minFeeAt(150).Int64() != 5 || f.minFeeChangeBlock(50) != 0 || f.minFeeChangeBlock(150) != 100 {
		t.Fatal("minFeeAt")
	}
	if !f.boundaryAt(100) || f.boundaryAt(101) || !f.boundaryAt(20) {
		t.Fatal("boundaryAt")
	}
	if _, err := setEntries(db.ConstraintSet{Constraints: db.JSONB(`{bad`)}); err == nil {
		t.Fatal("bad entries")
	}
	if !sameEntries([]model.ConstraintSetEntry{{Target: 1, Window: 2}}, []nitro.Constraint{{Target: 1, Window: 2}}) || sameEntries(nil, []nitro.Constraint{{}}) || sameEntries([]model.ConstraintSetEntry{{Target: 1}}, []nitro.Constraint{{Target: 2}}) {
		t.Fatal("sameEntries")
	}
	if !sameSets([]model.ConstraintSetEntry{{Target: 1, Window: 2, StartingBacklog: 5}}, []model.ConstraintSetEntry{{Target: 1, Window: 2}}) || sameSets(nil, []model.ConstraintSetEntry{{}}) || sameSets([]model.ConstraintSetEntry{{Target: 1}}, []model.ConstraintSetEntry{{Target: 2}}) {
		t.Fatal("sameSets")
	}
	if string(entriesJSON(nil)) != "[]" {
		t.Fatal("entriesJSON nil")
	}
	if err := sleepContext(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(cctx, time.Hour); err == nil {
		t.Fatal("expected cancellation")
	}
	nf := NewFollower(Options{Network: config.NetworkConfig{ChainID: 1}, Collector: config.CollectorConfig{HeaderBatchSize: 500}, RPC: newFakeRPC(1), Store: dbtest.New()})
	if nf.cfg.HeaderBatchSize != nitro.MaxBatch || nf.log == nil || nf.now == nil || nf.sleep == nil {
		t.Fatal("defaults")
	}
	// Shape helpers for a restart: the recorded set wins over the sample,
	// a legacy sample needs its parameters, an unreadable set falls back.
	f.sets = []db.ConstraintSet{{ID: 1, EffectiveBlock: 10, Constraints: entriesJSON([]model.ConstraintSetEntry{{Target: 7, Window: 8}})}}
	if st := f.stateAtLocked(20, s); len(st.Constraints) != 1 || st.Constraints[0].Target != 7 || st.Backlogs()[0] != 0 {
		t.Fatalf("stateAt with set: %+v", st)
	}
	if st := f.stateAtLocked(5, s); st.Constraints[0].Target != 1 {
		t.Fatalf("stateAt without set: %+v", st)
	}
	f.sets[0].Constraints = db.JSONB(`{bad`)
	if st := f.stateAtLocked(20, s); st.Constraints[0].Target != 1 {
		t.Fatalf("stateAt with a bad set: %+v", st)
	}
	if f.stateAtLocked(1, &nitro.Sample{}) != nil || f.stateAtLocked(1, leg).Legacy.SpeedLimit != 1 {
		t.Fatal("legacy stateAt")
	}
	// Missing-range read failures surface instead of overwriting durable rows.
	store := dbtest.New()
	f.store = store
	store.FailOn["MissingRanges"] = true
	if err := f.recordHole(context.Background(), store, hole{From: 1, To: 2}); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatal("missing-range read failure")
	}
}

func TestTickUnlimitedNeverSkips(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	f.net.CallsPerSecond = 0
	if !f.policy().unlimited {
		t.Fatal("calls_per_second 0 is unlimited")
	}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// The same gap that a budgeted follower skips is fetched in full: 150
	// blocks in 15 batches of 10, and the replay stays continuous.
	rpc.setHead(1150)
	rpc.headerCalls = nil
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rpc.headerCalls) != 15 || rpc.headerCalls[0][0] != 1001 || rpc.headerCalls[14][8] != 1149 {
		t.Fatalf("header batches = %d", len(rpc.headerCalls))
	}
	blocks, _ := store.BlocksAfter(ctx, 4663, 1000, 1000)
	if len(blocks) != 150 || blocks[0].Number != 1001 || blocks[0].Anchored {
		t.Fatalf("unlimited catch-up: %d blocks from %d", len(blocks), blocks[0].Number)
	}
	if blocks[0].Backlogs[0] != 3_111_506+gasFor(1001) {
		t.Fatalf("replay must continue from the anchored state: %+v", blocks[0])
	}
	if len(holesOf(t, store)) != 0 {
		t.Fatal("no hole on an unlimited network")
	}
	// A custom catch-up bound applies on a budgeted network: the gap is
	// skipped entirely and recorded.
	f.net.CallsPerSecond = 4
	f.cfg.MaxCatchUpBatches = 2
	rpc.setHead(1250)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	blocks, _ = store.BlocksAfter(ctx, 4663, 1150, 1000)
	if len(blocks) != 1 || blocks[0].Number != 1250 {
		t.Fatalf("custom catch-up bound: %d blocks from %d", len(blocks), blocks[0].Number)
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].From != 1151 || holes[0].To != 1249 {
		t.Fatalf("hole: %+v", holes)
	}
	// A gap within the bound is replayed in full.
	rpc.setHead(1265)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if blocks, _ := store.BlocksAfter(ctx, 4663, 1250, 1000); len(blocks) != 15 {
		t.Fatalf("bounded catch-up: %d blocks", len(blocks))
	}
}

func TestTickAt(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// A head event for 1005 samples state at 1005 even though the node's
	// latest is already 1008: the sample and the header agree.
	rpc.setHead(1008)
	if err := f.TickAt(ctx, 1005); err != nil {
		t.Fatal(err)
	}
	if len(rpc.sampleAt) != 1 || rpc.sampleAt[0] != 1005 || rpc.calledTimes("FastSample") != 1 {
		t.Fatalf("FastSampleAt calls = %v, FastSample calls = %d", rpc.sampleAt, rpc.calledTimes("FastSample"))
	}
	if f.Head() != 1005 {
		t.Fatalf("head = %d", f.Head())
	}
	latest, _ := store.LatestBlock(ctx, 4663)
	if latest.Number != 1005 || !latest.Anchored {
		t.Fatalf("head block: %+v", latest)
	}
	if snap := lastSnapshot(t, store); snap.Block.Number != 1005 {
		t.Fatalf("snapshot block = %d", snap.Block.Number)
	}
	// The same head again is a sample-only tick.
	before := len(store.Notifications)
	if err := f.TickAt(ctx, 1005); err != nil {
		t.Fatal(err)
	}
	if len(store.Notifications) != before+1 || f.Head() != 1005 {
		t.Fatal("repeated head should only re-sample")
	}
	// An older head is ignored, a head the node does not have yet fails.
	if err := f.TickAt(ctx, 1003); err != nil || f.Head() != 1005 {
		t.Fatalf("older head: %v head %d", err, f.Head())
	}
	if err := f.TickAt(ctx, 1010); err == nil {
		t.Fatal("head beyond the node should fail")
	}
	if err := f.TickAt(ctx, 1008); err != nil || f.Head() != 1008 {
		t.Fatalf("catch up to 1008: %v head %d", err, f.Head())
	}
	// The sample rides the fast lane, the catch-up headers are bulk work.
	if rpc.classOf("FastSample") != nitro.Fast || rpc.classOf("FastSampleAt") != nitro.Fast || rpc.classOf("HeadersByNumbers") != nitro.Bulk {
		t.Fatalf("classes: sample %v, sampleAt %v, headers %v", rpc.classOf("FastSample"), rpc.classOf("FastSampleAt"), rpc.classOf("HeadersByNumbers"))
	}
}

// TestTickRestartRebuildsState: after a restart the replay state comes
// from the stored head row (backlogs, min fee) and the recorded set, so
// the first catch-up is exact; a row whose shape is unknown seeds instead.
func TestTickRestartRebuildsState(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rpc.setHead(1003)
	f2 := newTestFollower(t, rpc, store)
	if err := f2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	b1001, _ := store.BlockByNumber(ctx, 4663, 1001)
	if b1001 == nil || b1001.Backlogs[0] != 3_111_506+gasFor(1001) || b1001.MinBaseFee.Wei.Int64() != 20_000_000 {
		t.Fatalf("restart replay: %+v", b1001)
	}
	// A stored head whose backlog count does not match any known shape
	// (say the model changed while the collector was down) is seeded.
	row, _ := store.BlockByNumber(ctx, 4663, 1003)
	row.Backlogs = db.Uint64Array{1, 2, 3}
	row.MinBaseFee = db.NewNullWei(nil)
	_ = store.UpsertBlocks(ctx, []db.Block{*row})
	rpc.setHead(1005)
	f3 := newTestFollower(t, rpc, store)
	if err := f3.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := store.BlockByNumber(ctx, 4663, 1004); b != nil {
		t.Fatal("block 1004 has no known state and must not be replayed")
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].From != 1004 || holes[0].To != 1004 {
		t.Fatalf("hole: %+v", holes)
	}
	// A head row that vanished (pruned) is unknown too; a lookup failure
	// surfaces.
	f4 := newTestFollower(t, rpc, store)
	_ = f4.ensureInit(ctx)
	delete(store.BlockRows[4663], 1005)
	f4.mu.Lock()
	st, _, err := f4.replayStateLocked(ctx, rpc.sampleLocked(1005, rpc.constraints, nil))
	f4.mu.Unlock()
	if err != nil || st != nil {
		t.Fatalf("missing head row: %v %v", st, err)
	}
	store.FailOn["BlockByNumber"] = true
	f4.mu.Lock()
	_, _, err = f4.replayStateLocked(ctx, rpc.sampleLocked(1005, rpc.constraints, nil))
	f4.mu.Unlock()
	if !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("lookup failure: %v", err)
	}
}

// TestRewindNetworkHeadStatus: a rewind is not a sample. The network row
// takes last_sample_at from the newest state sample that survived it, so
// /status does not report the rewind itself as a fresh successful sample,
// and a rewind that leaves no block at all nulls the head and its time
// instead of storing an epoch one.
func TestRewindNetworkHeadStatus(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	f := newTestFollower(t, rpc, store)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	first, _ := store.LatestStateSample(ctx, 4663, false)
	surviving := first.SampledAt
	rpc.setHead(1010)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// The chain reorganizes from 1006 on and the replacement sampling
	// never happens: what the rewind wrote is what /status shows.
	rpc.fork(1006, "z")
	if _, err := f.rewindToAncestor(ctx, 1009); err != nil {
		t.Fatal(err)
	}
	n := store.NetworkRows[4663]
	if !n.HeadBlock.Valid || n.HeadBlock.Int64 != 1005 || !n.HeadAt.Valid || n.HeadAt.Time.Unix() != int64(tsFor(1005)) {
		t.Fatalf("the head moves to the surviving ancestor: %+v", n)
	}
	if !n.LastSampleAt.Valid || !n.LastSampleAt.Time.Equal(surviving) {
		t.Fatalf("last_sample_at must be the newest surviving sample %v, got %+v", surviving, n.LastSampleAt)
	}
	if n.LastSampleAt.Time.Equal(f.now()) {
		t.Fatal("a rewind must not claim to be a fresh sample")
	}
	// Nothing survives at all: the head, its time and the sample time are
	// null rather than the epoch.
	store.BlockRows[4663] = map[uint64]db.Block{}
	store.SampleRows = nil
	if err := store.WithChainTx(ctx, 4663, func(s db.Store) error { return f.rewindNetworkHead(ctx, s, 0) }); err != nil {
		t.Fatal(err)
	}
	if n := store.NetworkRows[4663]; n.HeadBlock.Valid || n.HeadAt.Valid || n.LastSampleAt.Valid {
		t.Fatalf("an empty chain has no head and no sample: %+v", n)
	}
	// Failures on the way surface instead of leaving a stale row.
	for _, method := range []string{"LatestStateSample", "SetNetworkHead", "BlockByNumber"} {
		store.FailOn[method] = true
		err := store.WithChainTx(ctx, 4663, func(s db.Store) error { return f.rewindNetworkHead(ctx, s, 5) })
		store.FailOn[method] = false
		if !errors.Is(err, dbtest.ErrInjected) {
			t.Fatalf("%s: %v", method, err)
		}
	}
}

// TestCatchUpReEvaluatesAfterFailover: a catch-up decided on an unlimited
// endpoint that fails over to a paced one halfway through must not carry
// the unlimited decision on to the fallback. The budget is decided again
// against the endpoint that would serve the rest, and the remaining gap is
// skipped and queued instead of being fetched on the public endpoint.
func TestCatchUpReEvaluatesAfterFailover(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	pool := &fakePool{fakeRPC: rpc, pol: nitro.Policy{Unlimited: true}}
	store := dbtest.New()
	f := NewFollower(Options{
		Network:   config.NetworkConfig{Name: "robinhood", ChainID: 4663, CallsPerSecond: 4, Enabled: true},
		Collector: testConfig(), RPC: pool, Store: store, Log: logger.Nop(),
		Now:   func() time.Time { return baseTime.Add(100 * time.Second) },
		Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	})
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// A gap far wider than a paced endpoint's budget. The pool moves to a
	// paced fallback while the first batch is in flight.
	rpc.setHead(gapHead)
	rpc.headerCalls = nil
	rpc.hooks["HeadersByNumbers"] = func() {
		pool.pol = nitro.Policy{Unlimited: false, Rate: 4}
		pool.status = nitro.PoolStatus{Active: 1, Failovers: 1}
	}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := len(rpc.headerCalls); calls != 1 {
		t.Fatalf("the catch-up must stop at the failover, got %d batches", calls)
	}
	if holes := holesOf(t, store); len(holes) != 1 || holes[0].From != 1001 || holes[0].To != gapHead-1 {
		t.Fatalf("the rest of the gap must be queued: %+v", holes)
	}
	if f.Head() != gapHead {
		t.Fatalf("the sampled head is still seeded: %d", f.Head())
	}
	// A failover between endpoints with the same policy changes nothing.
	delete(rpc.hooks, "HeadersByNumbers")
	pool.pol = nitro.Policy{Unlimited: true}
	rpc.setHead(gapHead + 150)
	rpc.headerCalls = nil
	rpc.hooks["HeadersByNumbers"] = func() {
		pool.status = nitro.PoolStatus{Active: 0, Failovers: 2}
	}
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := len(rpc.headerCalls); calls != 15 {
		t.Fatalf("an unlimited fallback keeps catching up, got %d batches", calls)
	}
}

// A reorg reaching a bucket straddling the prune frontier leaves it with no correct aggregate: prune
// took its early rows, the rewind orphans its retained ones, and what is stored is the dead fork.
// RebuildBuckets declines the window, so the rewind discards exactly the windows the store declined.
func TestRewindDiscardsBucketsStraddlingThePruneFrontier(t *testing.T) {
	ctx := context.Background()
	rpc := newFakeRPC(1000)
	store := dbtest.New()
	seedSets(t, store)
	f := newTestFollower(t, rpc, store)
	// The same setup as the reorg test above: a backfill supplies the
	// replay state a fork below the live start needs, so the rows the
	// rewind re-fetches are real rows and the bucket over them a real one.
	f.cfg.BackfillDepth = config.Depth(30 * time.Second)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runBackfill(t, f, 100)
	rpc.setHead(1002)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Blocks arrive ten a second, so the minute bucket at 07:01 holds blocks 600 through 1199, and a
	// frontier at 07:01:30 falls inside it. That minute is the case that matters: it starts exactly at
	// the bucket boundary, so the rewind's own DeleteBucketsBefore leaves it alone and only the discard
	// below the frontier can remove it. The quarter hour and hour start earlier and would go either way.
	inside := time.Unix(int64(tsFor(900)), 0).UTC()
	if err := store.SetState(ctx, 4663, db.StatePruneFrontier, inside.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	minute := inside.Truncate(time.Minute)
	if before, _ := store.Buckets(ctx, 4663, db.Resolution1m, minute, minute.Add(time.Minute)); len(before) != 1 {
		t.Fatalf("the straddling minute bucket is not stored before the reorg: %d", len(before))
	}

	// A reorg reaching into the retained suffix of that bucket.
	rpc.fork(995, "d")
	rpc.setHead(1005)
	if err := f.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	for n := uint64(995); n <= 1005; n++ {
		if b, _ := store.BlockByNumber(ctx, 4663, n); b == nil || b.Hash != rpc.hashFor(n) {
			t.Fatalf("block %d not canonical after the rewind: %+v", n, b)
		}
	}
	// Every window the removed rows fell in starts below the frontier, so
	// each is declined by the rebuild and must be gone rather than stale.
	// The minute is the case that proves the discard; see above.
	for _, res := range db.ResolutionOrder {
		start := inside.Truncate(db.Resolutions[res])
		after, err := store.Buckets(ctx, 4663, res, start, start.Add(db.Resolutions[res]))
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != 0 {
			t.Fatalf("%s bucket %s straddles the frontier and survived the rewind with old-fork rows: %+v", res, start, after[0])
		}
	}
}

// TestShiftPredictions covers the alignment rule directly: a row's pricing group is the one its
// parent's replay produced, a carry supplies the first row, a break in numbering ends the chain, and
// the group of the last row is handed on.
func TestShiftPredictions(t *testing.T) {
	group := func(fee int64, exp int64) db.Block {
		return db.Block{PredictedBaseFee: db.NullWeiFromUint64(uint64(fee)), ExponentBips: exp,
			ConstraintBips: pq.Int64Array{exp}}
	}
	rows := []db.Block{group(10, 1), group(20, 2), group(30, 3)}
	for i := range rows {
		rows[i].Number = uint64(100 + i)
	}
	carry := prediction{fee: big.NewInt(9), exponent: 99, perConstraint: pq.Int64Array{99}, known: true}
	next := shiftPredictions(rows, carry)
	for i, want := range []int64{9, 10, 20} {
		if !rows[i].PredictedBaseFee.Valid || rows[i].PredictedBaseFee.Wei.Int64() != want {
			t.Fatalf("row %d predicted = %+v, want %d", i, rows[i].PredictedBaseFee, want)
		}
	}
	if rows[0].ExponentBips != 99 || rows[2].ExponentBips != 2 || rows[2].ConstraintBips[0] != 2 {
		t.Fatalf("exponents did not move with the fee: %+v", rows)
	}
	if !next.known || next.fee.Int64() != 30 || next.exponent != 3 {
		t.Fatalf("carry out = %+v", next)
	}

	// No carry leaves the first row unpredicted rather than claiming a perfect hit.
	rows = []db.Block{group(10, 1), group(20, 2)}
	rows[0].Number, rows[1].Number = 100, 101
	shiftPredictions(rows, prediction{})
	if rows[0].PredictedBaseFee.Valid || rows[0].ExponentBips != 0 || rows[0].ConstraintBips != nil {
		t.Fatalf("an unpredicted row must be empty: %+v", rows[0])
	}
	if rows[1].PredictedBaseFee.Wei.Int64() != 10 {
		t.Fatalf("the second row still takes the first's group: %+v", rows[1])
	}

	// A gap in numbering breaks the chain: the block after it has no replayed parent.
	rows = []db.Block{group(10, 1), group(20, 2), group(30, 3)}
	rows[0].Number, rows[1].Number, rows[2].Number = 100, 105, 106
	shiftPredictions(rows, carry)
	if !rows[0].PredictedBaseFee.Valid || rows[1].PredictedBaseFee.Valid {
		t.Fatalf("the block after a break must carry no prediction: %+v", rows)
	}
	if rows[2].PredictedBaseFee.Wei.Int64() != 20 {
		t.Fatalf("the chain resumes after the break: %+v", rows[2])
	}
}

// TestCarryRoundTrip: a checkpointed group survives encode and decode, and an absent or unreadable
// one decodes to no prediction rather than a zero fee.
func TestCarryRoundTrip(t *testing.T) {
	p := prediction{fee: big.NewInt(42), exponent: 7, perConstraint: pq.Int64Array{3, 4}, known: true}
	if got := decodeCarry(p.encode()); !got.known || got.fee.Int64() != 42 || got.exponent != 7 || len(got.perConstraint) != 2 {
		t.Fatalf("round trip = %+v", got)
	}
	if got := decodeCarry(prediction{}.encode()); got.known {
		t.Fatalf("an empty group must decode to no prediction: %+v", got)
	}
	if got := decodeCarry("not a number", 1, nil); got.known {
		t.Fatalf("an unreadable fee must decode to no prediction: %+v", got)
	}
}

// TestReloadHeadKeepsCarryOnAFailedTick: a metered RPC fails ticks routinely and every failure
// reloads the head. The committed result still describes an unmoved head, so the group it carries
// into the next block must survive; a head that moved or forked drops it, and the published error
// then comes from the stored row rather than a fresh zero.
func TestReloadHeadKeepsCarryOnAFailedTick(t *testing.T) {
	ctx := context.Background()
	store := dbtest.New()
	f := &Follower{chainID: 4663, store: store, log: logger.Nop()}
	row := db.Block{ChainID: 4663, Number: 100, Hash: "0x64", ParentHash: "0x63", TS: time.Unix(1000, 0).UTC(),
		BaseFee: db.WeiFromUint64(100), PredictedBaseFee: db.NullWeiFromUint64(90), Backlogs: db.Uint64Array{1},
		MinBaseFee: db.NullWeiFromUint64(20), PricingVersion: db.PricingFull}
	if err := store.UpsertBlocks(ctx, []db.Block{row}); err != nil {
		t.Fatal(err)
	}
	f.head, f.headHash = 100, "0x64"
	f.lastResult = &pricer.Result{Number: 100, Predicted: big.NewInt(7), Exponent: 5}
	f.lastErrBips = 42
	if err := f.reloadHeadLocked(ctx); err != nil {
		t.Fatal(err)
	}
	if f.lastResult == nil || f.lastErrBips != 42 {
		t.Fatalf("an unmoved head keeps its carry: result=%+v err=%d", f.lastResult, f.lastErrBips)
	}
	if got := f.carryLocked(101); !got.known || got.fee.Int64() != 7 {
		t.Fatalf("the carry must still reach the next block: %+v", got)
	}

	// A head on another fork invalidates it, and the error comes from the stored row: |90-100|/100.
	f.headHash = "0xdead"
	if err := f.reloadHeadLocked(ctx); err != nil {
		t.Fatal(err)
	}
	if f.lastResult != nil {
		t.Fatalf("a forked head must drop the carry: %+v", f.lastResult)
	}
	if f.lastErrBips != 1000 {
		t.Fatalf("the published error must come from the stored head, got %d", f.lastErrBips)
	}
	if got := f.carryLocked(101); got.known {
		t.Fatalf("no carry after a fork: %+v", got)
	}
}
