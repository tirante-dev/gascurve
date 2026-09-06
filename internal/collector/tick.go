package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

// Tick runs one fast iteration: sample, catch up on headers, replay, write
// blocks, buckets and the state sample in one transaction, then NOTIFY.
func (f *Follower) Tick(ctx context.Context) error {
	if err := f.ensureInit(ctx); err != nil {
		return f.fail(ctx, err)
	}
	sample, err := f.rpc.FastSample(ctx)
	if err != nil {
		return f.fail(ctx, fmt.Errorf("sample: %w", err))
	}
	head := sample.Header.Number

	f.mu.Lock()
	stored := f.head
	f.mu.Unlock()

	switch {
	case stored != 0 && head < stored:
		f.log.Warn("head went backwards, skipping tick", "head", head, "stored", stored)
		return nil
	case stored != 0 && head == stored:
		return f.sampleOnly(ctx, sample)
	}

	from := stored + 1
	if stored == 0 {
		from = max(head-min(head, initialBlocks-1), 1)
	}
	maxGap := uint64(f.cfg.HeaderBatchSize) * maxCatchUpFactor
	if gap := head - from + 1; gap > maxGap {
		f.log.Warn("catch-up gap exceeds budget, skipping blocks", "gap", gap, "skipped", gap-maxGap)
		from = head - maxGap + 1
		f.mu.Lock()
		f.state = nil // the replay restarts from the sampled backlogs
		f.mu.Unlock()
	}

	f.catchingUp.Store(true)
	headers, err := f.fetchHeaders(ctx, from, head-1)
	f.catchingUp.Store(false)
	if err != nil {
		return f.fail(ctx, fmt.Errorf("headers %d..%d: %w", from, head-1, err))
	}
	headers = append(headers, sample.Header)
	return f.process(ctx, sample, headers)
}

// fetchHeaders reads [from, to] in header_batch_size batches.
func (f *Follower) fetchHeaders(ctx context.Context, from, to uint64) ([]nitro.Header, error) {
	if to < from {
		return nil, nil
	}
	out := make([]nitro.Header, 0, to-from+1)
	for start := from; start <= to; start += uint64(f.cfg.HeaderBatchSize) {
		end := min(start+uint64(f.cfg.HeaderBatchSize)-1, to)
		numbers := make([]uint64, 0, end-start+1)
		for n := start; n <= end; n++ {
			numbers = append(numbers, n)
		}
		hs, err := f.rpc.HeadersByNumbers(ctx, numbers)
		if err != nil {
			return nil, err
		}
		out = append(out, hs...)
	}
	return out, nil
}

// process replays headers (the last one is the sampled head) and persists.
func (f *Follower) process(ctx context.Context, sample *nitro.Sample, headers []nitro.Header) error {
	head := sample.Header.Number
	f.mu.Lock()
	f.prepareStateLocked(ctx, sample)
	st := f.state
	prevTs := f.prevTs
	f.mu.Unlock()

	blocks := make([]pricer.Block, len(headers))
	for i, h := range headers {
		blocks[i] = pricer.Block{Number: h.Number, Timestamp: h.Timestamp, GasUsed: h.GasUsed, BaseFee: h.BaseFee}
	}
	anchor := func(n uint64) ([]uint64, bool) {
		if n == head {
			return sampleBacklogs(sample), true
		}
		return nil, false
	}
	results := pricer.Replay(st, prevTs, blocks, anchor)
	rows := blockRows(f.chainID, headers, results)

	f.mu.Lock()
	buckets := foldBuckets(rows, f.setIDFor)
	f.mu.Unlock()

	headResult := results[len(results)-1]
	if err := f.persist(ctx, sample, rows, buckets, &headResult); err != nil {
		return f.fail(ctx, err)
	}
	f.mu.Lock()
	f.head = head
	f.prevTs = sample.Header.Timestamp
	f.lastResult = &headResult
	f.mu.Unlock()
	return nil
}

// sampleOnly handles a tick where no new block appeared: the replay state
// is re-anchored and a fresh snapshot is published.
func (f *Follower) sampleOnly(ctx context.Context, sample *nitro.Sample) error {
	f.mu.Lock()
	f.prepareStateLocked(ctx, sample)
	f.state.SetBacklogs(sampleBacklogs(sample))
	last := f.lastResult
	f.mu.Unlock()
	if last == nil {
		last = &pricer.Result{Number: sample.Header.Number}
	}
	if err := f.persist(ctx, sample, nil, nil, last); err != nil {
		return f.fail(ctx, err)
	}
	return nil
}

// prepareStateLocked keeps the replay state aligned with the sampled model,
// rebuilding it when the constraint set changed or nothing is known yet.
func (f *Follower) prepareStateLocked(ctx context.Context, sample *nitro.Sample) {
	f.lastSample = sample
	if sameShape(f.state, sample) {
		f.state.MinBaseFee = new(big.Int).Set(sample.MinBaseFee)
		return
	}
	st := stateFromSample(sample)
	if f.state != nil {
		f.log.Info("pricer parameters changed, restarting replay from the sampled backlogs")
	} else if last, err := f.store.LatestBlock(ctx, f.chainID); err == nil && last != nil && len(last.Backlogs) == len(st.Backlogs()) {
		st.SetBacklogs(db.Uint64s(last.Backlogs))
	}
	f.state = st
}

// persist writes everything for a tick in one transaction and notifies.
func (f *Follower) persist(ctx context.Context, sample *nitro.Sample, rows []db.Block, buckets []db.Bucket, headResult *pricer.Result) error {
	head := sample.Header.Number
	headAt := time.Unix(int64(sample.Header.Timestamp), 0).UTC()
	f.mu.Lock()
	l1, accounts, includeSlow := f.l1, f.accounts, f.slowPending
	f.mu.Unlock()

	var snapshot *model.LiveSnapshot
	err := f.store.WithTx(ctx, func(s db.Store) error {
		if len(rows) > 0 {
			if err := s.UpsertBlocks(ctx, rows); err != nil {
				return fmt.Errorf("upsert blocks: %w", err)
			}
			if err := s.FoldBuckets(ctx, buckets); err != nil {
				return fmt.Errorf("fold buckets: %w", err)
			}
		}
		g10, err := s.GasUsedBetween(ctx, f.chainID, headAt.Add(-10*time.Second), headAt)
		if err != nil {
			return fmt.Errorf("gas per second: %w", err)
		}
		g60, err := s.GasUsedBetween(ctx, f.chainID, headAt.Add(-60*time.Second), headAt)
		if err != nil {
			return fmt.Errorf("gas per second: %w", err)
		}
		snap := buildSnapshot(f.chainID, sample, headResult, model.GasPerSecond{S10: g10 / 10, S60: g60 / 60}, l1, accounts)
		snapshot = &snap
		row, err := sampleRow(f.chainID, sample, &snap, includeSlow)
		if err != nil {
			return err
		}
		if err := s.InsertStateSample(ctx, row); err != nil {
			return fmt.Errorf("state sample: %w", err)
		}
		if err := s.UpdateNetworkHead(ctx, f.chainID, head, headAt, sample.SampledAt); err != nil {
			return fmt.Errorf("network head: %w", err)
		}
		if err := s.SetState(ctx, f.chainID, db.StateHead, fmt.Sprint(head)); err != nil {
			return fmt.Errorf("head checkpoint: %w", err)
		}
		payload, err := json.Marshal(snap)
		if err != nil {
			return fmt.Errorf("encode snapshot: %w", err)
		}
		return s.Notify(ctx, db.ChannelLive, string(payload))
	})
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.snapshot = snapshot
	if includeSlow {
		f.slowPending = false
	}
	f.mu.Unlock()
	return nil
}

// fail records the error on the network row and returns it.
func (f *Follower) fail(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	f.log.Warn("tick failed", "err", err.Error())
	if serr := f.store.SetNetworkError(ctx, f.chainID, err.Error()); serr != nil {
		f.log.Warn("record error", "err", serr.Error())
	}
	return err
}

// blockRows joins headers with replay results.
func blockRows(chainID uint64, headers []nitro.Header, results []pricer.Result) []db.Block {
	rows := make([]db.Block, len(headers))
	for i, h := range headers {
		r := results[i]
		rows[i] = db.Block{
			ChainID:          chainID,
			Number:           h.Number,
			TS:               time.Unix(int64(h.Timestamp), 0).UTC(),
			GasUsed:          h.GasUsed,
			BaseFee:          db.NewWei(h.BaseFee),
			L1Block:          h.L1BlockNumber,
			TxCount:          h.TxCount,
			Backlogs:         db.PQArray(r.Backlogs),
			ExponentBips:     int64(r.Exponent),
			PredictedBaseFee: db.NewWei(r.Predicted),
			Anchored:         r.Anchored,
		}
	}
	return rows
}

// buildSnapshot assembles the LiveSnapshot from a sample.
func buildSnapshot(chainID uint64, sample *nitro.Sample, headResult *pricer.Result, gps model.GasPerSecond, l1 *model.L1, accounts *model.Accounts) model.LiveSnapshot {
	live := stateFromSample(sample)
	_, exponent, per := live.Step(0)
	h := sample.Header
	minFee := sample.MinBaseFee
	if minFee == nil {
		minFee = new(big.Int)
	}
	snap := model.LiveSnapshot{
		ChainID:   chainID,
		SampledAt: sample.SampledAt.UTC().Format(time.RFC3339),
		Block: model.LiveBlock{
			Number: h.Number, TS: h.Timestamp, GasUsed: h.GasUsed, BaseFee: weiString(h.BaseFee), TxCount: h.TxCount,
		},
		BaseFee:        weiString(h.BaseFee),
		MinBaseFee:     minFee.String(),
		MultiplierBips: multiplierBips(h.BaseFee, minFee),
		ExponentBips:   int64(exponent),
		Model:          model.ModelConstraints,
		Constraints:    make([]model.Constraint, 0, len(sample.Constraints)),
		Prices: model.Prices{
			PerL2Tx: weiString(sample.Prices.PerL2Tx), PerL1CalldataByte: weiString(sample.Prices.PerL1CalldataByte),
			PerL2Storage: weiString(sample.Prices.PerL2Storage), PerArbGasBase: weiString(sample.Prices.PerArbGasBase),
			PerArbGasCongestion: weiString(sample.Prices.PerArbGasCongestion), PerArbGasTotal: weiString(sample.Prices.PerArbGasTotal),
		},
		GasPerSecond:    gps,
		L1:              l1,
		Accounts:        accounts,
		ReplayErrorBips: headResult.ErrorBips,
	}
	for i, c := range sample.Constraints {
		snap.Constraints = append(snap.Constraints, model.Constraint{Target: c.Target, Window: c.Window, Backlog: c.Backlog, ExponentBips: int64(per[i])})
	}
	if sample.IsLegacy() {
		snap.Model = model.ModelLegacy
		if sample.Legacy != nil {
			snap.Legacy = &model.LegacyParams{SpeedLimit: sample.Legacy.SpeedLimit, Inertia: sample.Legacy.Inertia, Tolerance: sample.Legacy.Tolerance, Backlog: sample.Legacy.Backlog}
		}
	}
	return snap
}

// sampleRow converts a snapshot into a state_samples row.
func sampleRow(chainID uint64, sample *nitro.Sample, snap *model.LiveSnapshot, includeSlow bool) (db.StateSample, error) {
	constraints, err := db.MarshalJSONB(snap.Constraints)
	if err != nil {
		return db.StateSample{}, fmt.Errorf("encode constraints: %w", err)
	}
	prices, err := db.MarshalJSONB(snap.Prices)
	if err != nil {
		return db.StateSample{}, fmt.Errorf("encode prices: %w", err)
	}
	row := db.StateSample{
		ChainID:     chainID,
		SampledAt:   sample.SampledAt.UTC(),
		BlockNumber: sample.Header.Number,
		BaseFee:     db.NewWei(sample.Header.BaseFee),
		MinBaseFee:  db.NewWei(sample.MinBaseFee),
		Constraints: constraints,
		Prices:      prices,
	}
	if snap.Legacy != nil {
		if row.Legacy, err = db.MarshalJSONB(snap.Legacy); err != nil {
			return db.StateSample{}, fmt.Errorf("encode legacy: %w", err)
		}
	}
	if includeSlow {
		if snap.L1 != nil {
			if row.L1, err = db.MarshalJSONB(snap.L1); err != nil {
				return db.StateSample{}, fmt.Errorf("encode l1: %w", err)
			}
		}
		if snap.Accounts != nil {
			if row.Accounts, err = db.MarshalJSONB(snap.Accounts); err != nil {
				return db.StateSample{}, fmt.Errorf("encode accounts: %w", err)
			}
		}
	}
	return row, nil
}

func weiString(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

func multiplierBips(baseFee, minFee *big.Int) int64 {
	if baseFee == nil || minFee == nil || minFee.Sign() == 0 {
		return 0
	}
	m := new(big.Int).Mul(baseFee, big.NewInt(int64(pricer.OneInBips)))
	m.Div(m, minFee)
	if !m.IsInt64() {
		return 1<<63 - 1
	}
	return m.Int64()
}
