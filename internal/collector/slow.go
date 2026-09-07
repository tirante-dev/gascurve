package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

var errBatchCostUnreconstructable = errors.New("batch report cost parameters are not reconstructable")

// SlowTick runs the slow loop once: the ETH/USD spot, L1 getters, fee
// accounts, ArbOS version, owner action logs, batch report scan, pruning
// and RPC stats.
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
		{"eth usd", f.sampleEthUsd},
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

// sampleEthUsd refreshes the ETH/USD spot from the process-wide cache (one
// fetch per slow_interval, shared by every network) and records it for this
// chain, so the API can serve it without an outbound call. A failed fetch
// is not an error here: the previous value stands until the tick finds it
// older than eth_usd_max_age.
func (f *Follower) sampleEthUsd(ctx context.Context) error {
	if f.ethUsd == nil {
		return nil
	}
	p := f.ethUsd.value(ctx, f.now(), f.log)
	if p == nil {
		return nil
	}
	// The checkpoint comes first: a quote published in memory but not
	// stored would be served by the tick and the WebSocket while /live,
	// which reads the checkpoint, still showed the previous one.
	raw, _ := json.Marshal(model.EthUsd{Price: p.Price, At: p.At.UTC().Format(time.RFC3339), Source: p.Source})
	if err := f.store.SetState(ctx, f.chainID, db.StateEthUsd, string(raw)); err != nil {
		return err
	}
	f.mu.Lock()
	f.ethUsdPrice = p
	f.mu.Unlock()
	return nil
}

// sampleSlow reads the L1 pricer getters and fee account balances; they are
// attached to the next fast tick's state sample.
func (f *Follower) sampleSlow(ctx context.Context) error {
	head, err := f.currentHead(ctx)
	if err != nil {
		return err
	}
	l1, err := f.rpc.L1SampleAt(ctx, head)
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
	f.batchCostAnchor = &batchCostAnchor{block: head, params: nitro.BatchPostingCostParams{
		ArbOSVersion: l1.ArbOSVersion, PerBatchGasCharge: l1.PerBatchGasCharge,
		ParentGasFloorPerToken: l1.ParentGasFloorPerToken,
	}}
	f.accounts = &model.Accounts{
		Infra:    model.Account{Address: accounts.Infra.Address, Balance: weiString(accounts.Infra.Balance)},
		Network:  model.Account{Address: accounts.Network.Address, Balance: weiString(accounts.Network.Balance)},
		L1Reward: model.Account{Address: accounts.L1Reward.Address, Balance: weiString(accounts.L1Reward.Balance)},
	}
	// A generation, not a flag: the tick that persists this sample clears
	// exactly this generation, so a newer sample taken while that
	// transaction runs still gets stored by the next tick.
	f.slowGen++
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
// constraint_sets. A chain too large to scan from genesis starts at a
// cutoff whose pricing state is established first (see establishOrigin).
// Once a pass reaches the head it started from, owner_scan_through records
// that the timeline is complete through it.
func (f *Follower) scanOwnerActions(ctx context.Context) error {
	gen, err := f.generation(ctx, f.store)
	if err != nil {
		return err
	}
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
	} else if from, err = f.scanStart(ctx, head, gen); err != nil {
		return err
	}
	for from <= head {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		to := min(from+ownerLogChunk-1, head)
		if err := f.scanOwnerRange(ctx, from, to, gen); err != nil {
			return err
		}
		from = to + 1
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.reloadSetsLocked(ctx); err != nil {
		return err
	}
	if head <= f.ownerScanThrough {
		return nil
	}
	// The readiness checkpoint is a cursor like any other: it commits under
	// the generation check, so a rewind that lowered it cannot be undone by
	// a pass that started on the old fork.
	if err := f.withGeneration(ctx, gen, func(s db.Store) error {
		return s.SetState(ctx, f.chainID, db.StateOwnerScanThrough, strconv.FormatUint(head, 10))
	}); err != nil {
		return err
	}
	f.ownerScanThrough = head
	return nil
}

// originMargin is how much further back than backfill_depth a fresh owner
// scan starts, so the pricing state is known a little before the first
// block the backfill replays.
const originMargin = 10 * time.Minute

// scanStart is where a fresh owner scan begins: the block at backfill_depth
// (plus originMargin) before the head's own timestamp, so the scan covers
// the history the backfill can use and no more; a shallow depth, as in
// development, never re-indexes a whole chain. The depth is bounded before
// the margin is added to it, so no configured value can overflow the
// arithmetic. A chain younger than the depth starts at genesis, whose
// pricing state is nitro's default and needs no origin. Otherwise the
// state in force at the cutoff is established first (establishOrigin) and
// the scan runs from the cutoff on. A cutoff that cannot be resolved (the
// head is behind it) is an error: nothing is written and the next pass
// tries again, rather than an origin at the head marking all the history
// before it unavailable.
func (f *Follower) scanStart(ctx context.Context, head, gen uint64) (uint64, error) {
	cutoff, err := f.historyStart(ctx, head, f.historyDepth(f.cfg.BackfillDepth)+originMargin)
	if err != nil {
		return 0, err
	}
	if cutoff <= 1 {
		return 0, nil
	}
	if err := f.establishOrigin(ctx, cutoff, gen); err != nil {
		return 0, err
	}
	return cutoff, nil
}

// establishOrigin records the complete pricing state in force at the block
// a truncated owner scan starts from, once. With an archive endpoint the
// whole state is sampled there: the minimum base fee, the constraints and
// their backlogs, or, on a legacy chain, the speed limit, inertia,
// tolerance and backlog. The sample is end-of-block state for the cutoff,
// whose own gas it already contains, so the constraint set is recorded as
// effective at the block after it and the replay starts there; adding the
// cutoff's gas again would inflate every backlog and price from the origin
// until the next anchor. Without an archive endpoint nothing before the
// cutoff can be priced: the range is recorded as a hole and the backfill
// never enters it, rather than guessing the state in force.
func (f *Follower) establishOrigin(ctx context.Context, cutoff, gen uint64) error {
	f.mu.Lock()
	known := f.scanOrigin != nil
	f.mu.Unlock()
	if known {
		return nil
	}
	origin := &scanOrigin{Block: cutoff}
	var set *db.ConstraintSet
	if f.archive != nil {
		sample, err := f.archive.FastSampleAt(ctx, cutoff)
		if err != nil {
			return fmt.Errorf("origin state at %d: %w", cutoff, err)
		}
		l1, err := f.archive.L1SampleAt(ctx, cutoff)
		if err != nil {
			return fmt.Errorf("origin L1 state at %d: %w", cutoff, err)
		}
		origin.Archive = true
		origin.MinBaseFee = bigOrZero(sample.MinBaseFee).String()
		origin.BatchCost = &nitro.BatchPostingCostParams{
			ArbOSVersion: l1.ArbOSVersion, PerBatchGasCharge: l1.PerBatchGasCharge,
			ParentGasFloorPerToken: l1.ParentGasFloorPerToken,
		}
		switch {
		case sample.IsLegacy():
			if sample.Legacy == nil {
				return fmt.Errorf("origin state at %d: legacy chain without parameters", cutoff)
			}
			origin.Legacy = &model.LegacyParams{
				SpeedLimit: sample.Legacy.SpeedLimit, Inertia: sample.Legacy.Inertia,
				Tolerance: sample.Legacy.Tolerance, Backlog: sample.Legacy.Backlog,
			}
		default:
			set = &db.ConstraintSet{
				ChainID: f.chainID, EffectiveBlock: origin.replayFrom(), EffectiveAt: time.Unix(int64(sample.Header.Timestamp), 0).UTC(),
				Constraints: entriesJSON(entriesFromSample(sample)), Source: model.SourceObserved,
			}
		}
		f.log.Info("owner scan starts at a cutoff, the whole pricing state was sampled from the archive",
			"block", cutoff, "replayFrom", origin.replayFrom(), "minBaseFee", origin.MinBaseFee, "legacy", origin.Legacy != nil)
	} else {
		f.log.Warn("owner scan starts at a cutoff without an archive endpoint, history before it cannot be reconstructed", "block", cutoff)
	}
	raw, err := json.Marshal(origin)
	if err != nil {
		return err
	}
	err = f.withGeneration(ctx, gen, func(s db.Store) error {
		if set != nil {
			if _, err := s.InsertConstraintSet(ctx, *set); err != nil {
				return err
			}
		}
		if !origin.Archive && cutoff > 0 {
			if err := f.recordHole(ctx, s, hole{From: 0, To: cutoff - 1, Reason: reasonNoState}); err != nil {
				return err
			}
		}
		return s.SetState(ctx, f.chainID, db.StateOwnerScanOrigin, string(raw))
	})
	if err != nil {
		return fmt.Errorf("owner scan origin: %w", err)
	}
	f.mu.Lock()
	f.scanOrigin = origin
	f.mu.Unlock()
	return nil
}

// scanOwnerRange records the actions in [from, to] and advances the
// cursor in the same chain-locked transaction, but only when the chain has
// not been rewound since gen was read: logs fetched from a fork the fast
// loop has meanwhile replaced must not be written back, nor may their
// cursor restore a checkpoint the rewind lowered. A malformed OwnerActs
// event fails the range so the cursor never moves past an event that was
// not understood.
func (f *Follower) scanOwnerRange(ctx context.Context, from, to, gen uint64) error {
	actions, err := f.fetchOwnerActions(ctx, from, to)
	if err != nil {
		return err
	}
	return f.withGeneration(ctx, gen, func(s db.Store) error {
		if _, err := f.storeOwnerActions(ctx, s, actions); err != nil {
			return err
		}
		return s.SetState(ctx, f.chainID, db.StateOwnerLogCursor, strconv.FormatUint(to, 10))
	})
}

// fetchOwnerRange reads the owner actions over [from, to] in chunks of at
// most ownerLogChunk blocks, so a range wider than an endpoint accepts in
// one eth_getLogs is still covered.
func (f *Follower) fetchOwnerRange(ctx context.Context, from, to uint64) ([]*nitro.OwnerAction, error) {
	var out []*nitro.OwnerAction
	for start := from; start <= to; start += ownerLogChunk {
		end := min(start+ownerLogChunk-1, to)
		actions, err := f.fetchOwnerActions(ctx, start, end)
		if err != nil {
			return nil, err
		}
		out = append(out, actions...)
	}
	return out, nil
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

// storeOwnerActions records actions and reports whether any was new (the
// set, fee and legacy caches then need a reload once the transaction
// committed).
func (f *Follower) storeOwnerActions(ctx context.Context, s db.Store, actions []*nitro.OwnerAction) (bool, error) {
	changed := false
	for _, a := range actions {
		c, err := f.recordAction(ctx, s, a)
		if err != nil {
			return changed, err
		}
		changed = changed || c
	}
	return changed, nil
}

// recordAction stores one decoded action, its constraint set when it is a
// setGasPricingConstraints call, and notifies the API; it reports whether
// the action was new. When an observed set with the same constraints was
// recorded at a later block (the live shape seen before its action was
// found), that row is moved to the action in place, so the set keeps its
// id.
func (f *Follower) recordAction(ctx context.Context, s db.Store, a *nitro.OwnerAction) (recorded bool, err error) {
	args, err := db.MarshalJSONB(a.Args)
	if err != nil {
		return false, fmt.Errorf("encode args: %w", err)
	}
	at := time.Unix(int64(a.Timestamp), 0).UTC()
	row := db.OwnerAction{
		ChainID: f.chainID, BlockNumber: a.BlockNumber, TxHash: a.TxHash,
		TxIndex: sql.NullInt64{Int64: int64(a.TxIndex), Valid: true}, LogIndex: int64(a.LogIndex),
		TS: at, Method: a.Method, Selector: a.Selector, Args: args,
	}
	n, err := s.InsertOwnerActions(ctx, []db.OwnerAction{row})
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	f.log.Info("owner action", "block", a.BlockNumber, "method", a.Method)
	if a.Constraints != nil {
		entries := entriesOf(a.Constraints)
		source := model.SourceOwnerAction
		if a.BlockNumber <= genesisBlockLimit {
			source = model.SourceGenesis
		}
		cs := db.ConstraintSet{ChainID: f.chainID, EffectiveBlock: a.BlockNumber, EffectiveAt: at, Constraints: entriesJSON(entries), Source: source}
		sets, err := s.ConstraintSets(ctx, f.chainID)
		if err != nil {
			return false, err
		}
		if o := observedToReplace(sets, a.BlockNumber, source, entries); o != nil {
			f.log.Info("owner action explains an observed constraint set, moving it to the action", "setId", o.ID, "from", o.EffectiveBlock, "to", a.BlockNumber)
			cs.ID = o.ID
			if err := s.UpdateConstraintSet(ctx, cs); err != nil {
				return false, err
			}
		} else if _, err := s.InsertConstraintSet(ctx, cs); err != nil {
			return false, err
		}
	}
	payload, err := json.Marshal(model.OwnerActionNotification{ChainID: f.chainID, LogIndex: a.LogIndex, Action: model.OwnerAction{
		Block: a.BlockNumber, At: at.Format(time.RFC3339), TxHash: a.TxHash, Method: a.Method, Selector: a.Selector, Args: json.RawMessage(args),
	}})
	if err != nil {
		return false, fmt.Errorf("encode notification: %w", err)
	}
	return true, s.Notify(ctx, db.ChannelOwnerAction, string(payload))
}

// observedToReplace returns the observed set an action at block with
// entries explains: the first set at or after the block, when it is
// observed with the same constraints and no row already occupies (block,
// source). Nil means the action gets its own row.
func observedToReplace(sets []db.ConstraintSet, block uint64, source string, entries []model.ConstraintSetEntry) *db.ConstraintSet {
	var first *db.ConstraintSet
	for i := range sets {
		cs := &sets[i]
		if cs.EffectiveBlock == block && cs.Source == source {
			return nil
		}
		if cs.EffectiveBlock >= block && first == nil {
			first = cs
		}
	}
	if first == nil || first.Source != model.SourceObserved {
		return nil
	}
	have, err := setEntries(*first)
	if err != nil || !sameSets(have, entries) {
		return nil
	}
	return first
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
// stores the decoded cost. The policy is the active endpoint's: a budgeted
// endpoint only looks at two-transaction blocks (the shape of a report
// block) and at most batchScanLimit of them per slow tick; an unlimited
// one reads every block with full transactions until it has caught up.
// It is bound to the endpoint generation and taken again whenever the pool
// moved to another endpoint, so an unlimited scan does not keep reading
// full blocks on a paced fallback. The cursor commits under the generation
// check, so blocks read from a fork the fast loop has rewound are
// discarded.
func (f *Follower) scanBatchReports(ctx context.Context) error {
	pol := f.policy()
	resolver, err := f.batchCostResolver()
	if err != nil {
		return err
	}
	gen, err := f.generation(ctx, f.store)
	if err != nil {
		return err
	}
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
	for {
		if cur, changed := f.repolicy(pol); changed {
			f.log.Info("the endpoint changed during the batch scan, taking its policy", "unlimited", cur.unlimited)
			pol = cur
		}
		chunk := batchFetchChunk
		if pol.unlimited {
			chunk = nitro.MaxBatch
		}
		nums, err := f.batchCandidates(ctx, after, pol)
		if err != nil {
			return err
		}
		// The fast loop keeps appending blocks while this runs, so the
		// candidates can reach past the anchor the parameters were pinned
		// to. Those are left for the next tick, which pins a newer one,
		// rather than priced against a snapshot that predates them.
		nums = capBlocks(nums, resolver.anchor.block)
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
				r, err := batchReportOf(f.chainID, b, resolver)
				if err != nil {
					if errors.Is(err, errBatchCostUnreconstructable) {
						f.log.Warn("skipping batch report with unreconstructable cost", "block", b.Number, "err", err.Error())
						continue
					}
					return err
				}
				if r != nil {
					reports = append(reports, *r)
				}
			}
		}
		after = nums[len(nums)-1]
		err = f.withGeneration(ctx, gen, func(s db.Store) error {
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
		if !pol.unlimited || len(nums) < batchScanLimit {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

type batchCostResolver struct {
	origin  *scanOrigin
	anchor  batchCostAnchor
	changes []batchCostChange
}

func (f *Follower) batchCostResolver() (*batchCostResolver, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.batchCostAnchor == nil {
		return nil, errors.New("batch report cost parameters have not been sampled")
	}
	r := &batchCostResolver{anchor: *f.batchCostAnchor, changes: append([]batchCostChange(nil), f.batchCostChanges...)}
	if f.scanOrigin != nil {
		origin := *f.scanOrigin
		if f.scanOrigin.BatchCost != nil {
			params := *f.scanOrigin.BatchCost
			origin.BatchCost = &params
		}
		r.origin = &origin
	}
	return r, nil
}

func (r *batchCostResolver) paramsAt(block, arbosVersion uint64) (nitro.BatchPostingCostParams, error) {
	if block > r.anchor.block {
		return nitro.BatchPostingCostParams{}, fmt.Errorf("batch report block %d is above parameter anchor %d", block, r.anchor.block)
	}
	params := nitro.BatchPostingCostParams{ArbOSVersion: arbosVersion}
	var perBatchKnown, floorKnown bool
	switch {
	case r.origin == nil:
		params.PerBatchGasCharge = defaultPerBatchGasCharge(arbosVersion)
		params.ParentGasFloorPerToken = 0
		perBatchKnown, floorKnown = true, true
	case r.origin.BatchCost != nil && block > r.origin.Block:
		params.PerBatchGasCharge = r.origin.BatchCost.PerBatchGasCharge
		params.ParentGasFloorPerToken = r.origin.BatchCost.ParentGasFloorPerToken
		perBatchKnown, floorKnown = true, true
	}

	perBatchAfter, floorAfter := false, false
	for _, change := range r.changes {
		if r.origin != nil && r.origin.BatchCost != nil && change.block <= r.origin.Block {
			continue
		}
		if change.block <= block {
			if change.perBatchGas != nil {
				params.PerBatchGasCharge, perBatchKnown = *change.perBatchGas, true
			}
			if change.parentFloor != nil {
				params.ParentGasFloorPerToken, floorKnown = *change.parentFloor, true
			}
			continue
		}
		if change.block <= r.anchor.block {
			perBatchAfter = perBatchAfter || change.perBatchGas != nil
			floorAfter = floorAfter || change.parentFloor != nil
		}
	}

	// A truncated scan without archive state can still use the current
	// anchor for a parameter when no setter lies between the report and the
	// anchor. If there is one, the pre-setter value cannot be inferred.
	if !perBatchAfter {
		params.PerBatchGasCharge, perBatchKnown = r.anchor.params.PerBatchGasCharge, true
	}
	if !floorAfter {
		params.ParentGasFloorPerToken, floorKnown = r.anchor.params.ParentGasFloorPerToken, true
	}
	if !perBatchKnown {
		return nitro.BatchPostingCostParams{}, fmt.Errorf("%w: per-batch gas charge at block %d", errBatchCostUnreconstructable, block)
	}
	if arbosVersion >= 50 && !floorKnown {
		return nitro.BatchPostingCostParams{}, fmt.Errorf("%w: parent gas floor at block %d", errBatchCostUnreconstructable, block)
	}
	return params, nil
}

func defaultPerBatchGasCharge(arbosVersion uint64) int64 {
	switch {
	case arbosVersion >= 11:
		return 210_000
	case arbosVersion >= 6:
		return 100_000
	default:
		return 0
	}
}

// capBlocks truncates an ascending block list at the last entry that is
// not above through.
func capBlocks(nums []uint64, through uint64) []uint64 {
	for i, n := range nums {
		if n > through {
			return nums[:i]
		}
	}
	return nums
}

// batchCandidates lists the next blocks to inspect after the cursor: every
// block on an unlimited endpoint, only two-transaction blocks on a
// budgeted one.
func (f *Follower) batchCandidates(ctx context.Context, after uint64, pol policy) ([]uint64, error) {
	if !pol.unlimited {
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
func batchReportOf(chainID uint64, b nitro.Block, resolver *batchCostResolver) (*db.BatchReport, error) {
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
		params, err := resolver.paramsAt(b.Number, b.ArbOSVersion)
		if err != nil {
			return nil, fmt.Errorf("batch report %d parameters: %w", b.Number, err)
		}
		cost, err := r.Cost(params)
		if err != nil {
			return nil, fmt.Errorf("batch report %d cost: %w", b.Number, err)
		}
		return &db.BatchReport{
			ChainID: chainID, BlockNumber: b.Number, BatchNumber: r.BatchNumber,
			BatchTS: time.Unix(int64(r.BatchTimestamp), 0).UTC(), Poster: r.Poster,
			CalldataLen: r.CalldataLen, CalldataNonzero: r.CalldataNonZeros, ExtraGas: r.ExtraGas,
			L1BaseFee: db.NewWei(r.L1BaseFee), GasSpent: cost.GasSpent, WeiSpent: db.NewWei(cost.WeiSpent),
			ReportVersion: r.Version, ArbOSVersion: params.ArbOSVersion,
			PerBatchGasCharge: params.PerBatchGasCharge, ParentGasFloorPerToken: params.ParentGasFloorPerToken,
			CostCalculationVersion: nitro.BatchPostingCostCalculationVersion,
		}, nil
	}
	return nil, nil
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
	var n, m int64
	err = f.store.WithChainTx(ctx, f.chainID, func(s db.Store) error {
		if n, err = s.PruneBlocks(ctx, f.chainID, before); err != nil {
			return err
		}
		m, err = s.PruneStateSamples(ctx, f.chainID, now.Add(-f.cfg.SampleRetention))
		return err
	})
	if err != nil {
		return err
	}
	if n > 0 || m > 0 {
		f.log.Debug("pruned", "blocks", n, "samples", m)
	}
	return nil
}

// persistStats records rate limit accounting and, with a pool, the
// endpoint routing state for /status. It is also where the RPC counters
// reach the instruments: they are observed before the checkpoint writes,
// so a database that is refusing writes does not also take the endpoint
// series away.
func (f *Follower) persistStats(ctx context.Context) error {
	st := f.rpc.Stats()
	var status *nitro.PoolStatus
	if f.pool != nil {
		ps := f.pool.Status()
		status = &ps
	}
	f.metrics.ObservePool(poolMetrics(st, status))
	if err := f.store.SetState(ctx, f.chainID, db.StateRateLimitEvents, strconv.FormatUint(st.RateLimitEvents, 10)); err != nil {
		return err
	}
	if status != nil {
		b, err := json.Marshal(endpointsStatus(*status))
		if err != nil {
			return fmt.Errorf("encode endpoints: %w", err)
		}
		if err := f.store.SetState(ctx, f.chainID, db.StateEndpoints, string(b)); err != nil {
			return err
		}
		// A disabled endpoint is an error the operator must see even though
		// the network keeps running on another one.
		if msg := endpointError(*status); msg != "" {
			if err := f.store.SetNetworkError(ctx, f.chainID, msg); err != nil {
				return err
			}
		}
	}
	if st.Last429At.IsZero() {
		return nil
	}
	return f.store.SetState(ctx, f.chainID, db.StateLast429At, st.Last429At.UTC().Format(time.RFC3339))
}
