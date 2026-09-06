package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

// SlowTick runs the slow loop once: L1 getters, fee accounts, ArbOS
// version, owner action logs, batch report scan, pruning and RPC stats.
// Every step runs even when an earlier one failed; errors are joined.
func (f *Follower) SlowTick(ctx context.Context) error {
	if err := f.ensureInit(ctx); err != nil {
		return err
	}
	var errs []error
	for _, step := range []struct {
		name string
		fn   func(context.Context) error
	}{
		{"l1", f.sampleSlow},
		{"arbos", f.checkArbOSVersion},
		{"owner actions", f.scanOwnerActions},
		{"batch reports", f.scanBatchReports},
		{"prune", f.prune},
		{"stats", f.persistStats},
	} {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := step.fn(ctx); err != nil {
			f.log.Warn("slow step failed", "step", step.name, "err", err.Error())
			errs = append(errs, fmt.Errorf("%s: %w", step.name, err))
		}
	}
	return errors.Join(errs...)
}

// sampleSlow reads the L1 pricer getters and fee account balances; they are
// attached to the next fast tick's state sample.
func (f *Follower) sampleSlow(ctx context.Context) error {
	l1, err := f.rpc.L1Sample(ctx)
	if err != nil {
		return err
	}
	accounts, err := f.rpc.FeeAccounts(ctx)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.l1 = &model.L1{
		BaseFeeEstimate:    weiString(l1.BaseFeeEstimate),
		Surplus:            weiString(l1.Surplus),
		FeesAvailable:      weiString(l1.FeesAvailable),
		UnitsSinceUpdate:   l1.UnitsSinceUpdate,
		LastUpdateAt:       time.Unix(int64(l1.LastUpdateTime), 0).UTC().Format(time.RFC3339),
		EquilibrationUnits: l1.EquilibrationUnits,
		PerBatchGasCharge:  l1.PerBatchGasCharge,
		RewardRate:         l1.RewardRate,
	}
	f.accounts = &model.Accounts{
		Infra:    model.Account{Address: accounts.Infra.Address, Balance: weiString(accounts.Infra.Balance)},
		Network:  model.Account{Address: accounts.Network.Address, Balance: weiString(accounts.Network.Balance)},
		L1Reward: model.Account{Address: accounts.L1Reward.Address, Balance: weiString(accounts.L1Reward.Balance)},
	}
	f.slowPending = true
	f.mu.Unlock()
	return nil
}

// checkArbOSVersion records the ArbOS version and warns when it changes,
// since an upgrade may change the pricing model.
func (f *Follower) checkArbOSVersion(ctx context.Context) error {
	v, err := f.rpc.ArbOSVersion(ctx)
	if err != nil {
		return err
	}
	cur := strconv.FormatUint(v, 10)
	prev, ok, err := f.store.GetState(ctx, f.chainID, db.StateArbOSVersion)
	if err != nil {
		return err
	}
	if ok && prev != cur {
		f.log.Warn("ArbOS version changed, verify the pricer model still matches", "previous", prev, "current", cur)
	}
	return f.store.SetState(ctx, f.chainID, db.StateArbOSVersion, cur)
}

// scanOwnerActions fetches OwnerActs logs since the cursor in chunks of at
// most ownerLogChunk blocks and maintains owner_actions and
// constraint_sets.
func (f *Follower) scanOwnerActions(ctx context.Context) error {
	head, err := f.currentHead(ctx)
	if err != nil {
		return err
	}
	cursor, ok, err := f.store.GetState(ctx, f.chainID, db.StateOwnerLogCursor)
	if err != nil {
		return err
	}
	var from uint64
	if ok {
		last, perr := strconv.ParseUint(cursor, 10, 64)
		if perr != nil {
			return fmt.Errorf("owner cursor %q: %w", cursor, perr)
		}
		from = last + 1
	} else if head >= smallChainBlocks {
		from = head - largeChainLookback
	}
	for from <= head {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		to := min(from+ownerLogChunk-1, head)
		if err := f.scanOwnerRange(ctx, from, to); err != nil {
			return err
		}
		from = to + 1
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.reloadSetsLocked(ctx); err != nil {
		return err
	}
	f.ownerScanDone = true
	return f.checkObservedLocked(ctx)
}

// scanOwnerRange records the actions in [from, to] and advances the
// cursor in the same transaction. A malformed OwnerActs event fails the
// range so the cursor never moves past an event that was not understood.
func (f *Follower) scanOwnerRange(ctx context.Context, from, to uint64) error {
	actions, err := f.fetchOwnerActions(ctx, from, to)
	if err != nil {
		return err
	}
	return f.store.WithTx(ctx, func(s db.Store) error {
		if err := f.storeOwnerActions(ctx, s, actions); err != nil {
			return err
		}
		return s.SetState(ctx, f.chainID, db.StateOwnerLogCursor, strconv.FormatUint(to, 10))
	})
}

// fetchOwnerActions reads and decodes the OwnerActs logs in [from, to].
// Unknown selectors decode to raw actions; a structurally malformed event
// is an error.
func (f *Follower) fetchOwnerActions(ctx context.Context, from, to uint64) ([]*nitro.OwnerAction, error) {
	logs, err := f.rpc.OwnerActsLogs(ctx, from, to)
	if err != nil {
		return nil, fmt.Errorf("logs %d..%d: %w", from, to, err)
	}
	actions := make([]*nitro.OwnerAction, 0, len(logs))
	for _, l := range logs {
		a, err := nitro.DecodeOwnerActs(l)
		if err != nil {
			return nil, fmt.Errorf("owner action in %d..%d: %w", from, to, err)
		}
		if a.Timestamp == 0 {
			h, err := f.rpc.HeaderByNumber(ctx, a.BlockNumber)
			if err != nil {
				return nil, fmt.Errorf("header %d: %w", a.BlockNumber, err)
			}
			a.Timestamp = h.Timestamp
		}
		actions = append(actions, a)
	}
	return actions, nil
}

func (f *Follower) storeOwnerActions(ctx context.Context, s db.Store, actions []*nitro.OwnerAction) error {
	for _, a := range actions {
		if err := f.recordAction(ctx, s, a); err != nil {
			return err
		}
	}
	return nil
}

// recordAction stores one decoded action, its constraint set when it is a
// setGasPricingConstraints call, and notifies the API.
func (f *Follower) recordAction(ctx context.Context, s db.Store, a *nitro.OwnerAction) error {
	args, err := db.MarshalJSONB(a.Args)
	if err != nil {
		return fmt.Errorf("encode args: %w", err)
	}
	at := time.Unix(int64(a.Timestamp), 0).UTC()
	row := db.OwnerAction{
		ChainID: f.chainID, BlockNumber: a.BlockNumber, TxHash: a.TxHash, LogIndex: int64(a.LogIndex),
		TS: at, Method: a.Method, Selector: a.Selector, Args: args,
	}
	n, err := s.InsertOwnerActions(ctx, []db.OwnerAction{row})
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	f.log.Info("owner action", "block", a.BlockNumber, "method", a.Method)
	if a.Constraints != nil {
		entries := make([]model.ConstraintSetEntry, len(a.Constraints))
		for i, c := range a.Constraints {
			entries[i] = model.ConstraintSetEntry{Target: c.GasTargetPerSecond, Window: c.AdjustmentWindowSeconds, StartingBacklog: c.StartingBacklog}
		}
		source := model.SourceOwnerAction
		if a.BlockNumber <= genesisBlockLimit {
			source = model.SourceGenesis
		}
		if _, err := s.InsertConstraintSet(ctx, db.ConstraintSet{
			ChainID: f.chainID, EffectiveBlock: a.BlockNumber, EffectiveAt: at, Constraints: entriesJSON(entries), Source: source,
		}); err != nil {
			return err
		}
	}
	payload, err := json.Marshal(model.OwnerActionNotification{ChainID: f.chainID, LogIndex: a.LogIndex, Action: model.OwnerAction{
		Block: a.BlockNumber, At: at.Format(time.RFC3339), TxHash: a.TxHash, Method: a.Method, Selector: a.Selector, Args: json.RawMessage(args),
	}})
	if err != nil {
		return fmt.Errorf("encode notification: %w", err)
	}
	return s.Notify(ctx, db.ChannelOwnerAction, string(payload))
}

// checkObservedLocked inserts an 'observed' constraint set once per process
// when the live set differs from the latest known one.
func (f *Follower) checkObservedLocked(ctx context.Context) error {
	if f.observedChecked || !f.ownerScanDone || f.lastSample == nil || f.lastSample.IsLegacy() {
		return nil
	}
	live := f.lastSample.Constraints
	if len(f.sets) > 0 {
		entries, err := setEntries(f.sets[len(f.sets)-1])
		if err != nil {
			return err
		}
		if sameEntries(entries, live) {
			f.observedChecked = true
			return nil
		}
	}
	entries := entriesFromSample(f.lastSample)
	f.log.Info("live constraint set differs from the latest known, recording an observed set", "block", f.lastSample.Header.Number)
	if _, err := f.store.InsertConstraintSet(ctx, db.ConstraintSet{
		ChainID: f.chainID, EffectiveBlock: f.lastSample.Header.Number, EffectiveAt: f.now().UTC(),
		Constraints: entriesJSON(entries), Source: model.SourceObserved,
	}); err != nil {
		return err
	}
	f.observedChecked = true
	return f.reloadSetsLocked(ctx)
}

// currentHead returns the followed head, or asks the RPC when unknown.
func (f *Follower) currentHead(ctx context.Context) (uint64, error) {
	f.mu.Lock()
	head := f.head
	f.mu.Unlock()
	if head > 0 {
		return head, nil
	}
	return f.rpc.BlockNumber(ctx)
}

// scanBatchReports inspects new blocks for batch posting reports and
// stores the decoded cost. A budgeted network only looks at
// two-transaction blocks (the shape of a report block) and at most
// batchScanLimit of them per slow tick; an unlimited one reads every block
// with full transactions until it has caught up.
func (f *Follower) scanBatchReports(ctx context.Context) error {
	cursor, ok, err := f.store.GetState(ctx, f.chainID, db.StateBatchScanCursor)
	if err != nil {
		return err
	}
	var after uint64
	if ok {
		if after, err = strconv.ParseUint(cursor, 10, 64); err != nil {
			return fmt.Errorf("batch cursor %q: %w", cursor, err)
		}
	} else {
		oldest, err := f.store.OldestBlock(ctx, f.chainID)
		if err != nil {
			return err
		}
		if oldest == nil {
			return nil
		}
		after = oldest.Number - 1
	}
	chunk := batchFetchChunk
	if f.unlimited() {
		chunk = nitro.MaxBatch
	}
	for {
		nums, err := f.batchCandidates(ctx, after)
		if err != nil {
			return err
		}
		if len(nums) == 0 {
			return nil
		}
		var reports []db.BatchReport
		for start := 0; start < len(nums); start += chunk {
			end := min(start+chunk, len(nums))
			blocks, err := f.rpc.BlocksWithTxs(ctx, nums[start:end])
			if err != nil {
				return err
			}
			for _, b := range blocks {
				if r := batchReportOf(f.chainID, b); r != nil {
					reports = append(reports, *r)
				}
			}
		}
		after = nums[len(nums)-1]
		err = f.store.WithTx(ctx, func(s db.Store) error {
			if len(reports) > 0 {
				if err := s.UpsertBatchReports(ctx, reports); err != nil {
					return err
				}
			}
			return s.SetState(ctx, f.chainID, db.StateBatchScanCursor, strconv.FormatUint(after, 10))
		})
		if err != nil {
			return err
		}
		if !f.unlimited() || len(nums) < batchScanLimit {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// batchCandidates lists the next blocks to inspect after the cursor: every
// block on an unlimited network, only two-transaction blocks on a budgeted
// one.
func (f *Follower) batchCandidates(ctx context.Context, after uint64) ([]uint64, error) {
	if !f.unlimited() {
		return f.store.TwoTxBlocks(ctx, f.chainID, after, batchScanLimit)
	}
	blocks, err := f.store.BlocksAfter(ctx, f.chainID, after, batchScanLimit)
	if err != nil {
		return nil, err
	}
	nums := make([]uint64, len(blocks))
	for i, b := range blocks {
		nums[i] = b.Number
	}
	return nums, nil
}

// batchReportOf decodes a batch posting report from a block's internal
// transactions, or returns nil.
func batchReportOf(chainID uint64, b nitro.Block) *db.BatchReport {
	for _, tx := range b.Txs {
		if !nitro.IsInternalTx(tx) {
			continue
		}
		decoded, err := nitro.DecodeInternalTx(tx.Input)
		if err != nil {
			continue
		}
		r, ok := decoded.(*nitro.BatchPostingReport)
		if !ok {
			continue
		}
		return &db.BatchReport{
			ChainID: chainID, BlockNumber: b.Number, BatchNumber: r.BatchNumber,
			BatchTS: time.Unix(int64(r.BatchTimestamp), 0).UTC(), Poster: r.Poster,
			CalldataLen: r.CalldataLen, CalldataNonzero: r.CalldataNonZeros, ExtraGas: r.ExtraGas,
			L1BaseFee: db.NewWei(r.L1BaseFee), GasSpent: r.GasSpent(), WeiSpent: db.NewWei(r.WeiSpent()),
		}
	}
	return nil
}

// prune drops per-block rows and raw samples beyond retention. Rows in the
// hour of the first live block are kept while the backfill is still
// running: its last segment rebuilds those buckets from rows.
func (f *Follower) prune(ctx context.Context) error {
	now := f.now()
	before := now.Add(-f.cfg.BlockRetention)
	c, err := f.loadCursor(ctx)
	if err != nil {
		return err
	}
	if !c.Done {
		f.mu.Lock()
		boundary, ok := f.boundaryLocked()
		f.mu.Unlock()
		if ok && boundary.Before(before) {
			before = boundary
		}
	}
	n, err := f.store.PruneBlocks(ctx, f.chainID, before)
	if err != nil {
		return err
	}
	m, err := f.store.PruneStateSamples(ctx, f.chainID, now.Add(-f.cfg.SampleRetention))
	if err != nil {
		return err
	}
	if n > 0 || m > 0 {
		f.log.Debug("pruned", "blocks", n, "samples", m)
	}
	return nil
}

// persistStats records rate limit accounting and, with a pool, the
// endpoint routing state for /status.
func (f *Follower) persistStats(ctx context.Context) error {
	st := f.rpc.Stats()
	if err := f.store.SetState(ctx, f.chainID, db.StateRateLimitEvents, strconv.FormatUint(st.RateLimitEvents, 10)); err != nil {
		return err
	}
	if f.pool != nil {
		b, err := json.Marshal(endpointsStatus(f.pool.Status()))
		if err != nil {
			return fmt.Errorf("encode endpoints: %w", err)
		}
		if err := f.store.SetState(ctx, f.chainID, db.StateEndpoints, string(b)); err != nil {
			return err
		}
	}
	if st.Last429At.IsZero() {
		return nil
	}
	return f.store.SetState(ctx, f.chainID, db.StateLast429At, st.Last429At.UTC().Format(time.RFC3339))
}
