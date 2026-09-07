// Receipt-backed poster gas arrived after the collector had been storing blocks, and a bucket with
// one source block lacking it has no poster gas, no compute-gas rate and no fee split. Nothing
// repairs that on its own: retention never drops a bucket and no loop revisits a finished one. This
// pass reads eth_getBlockReceipts for the rows that lack the value (one call per block, the stored
// row already carrying what the receipts are checked against), writes it, and rebuilds the buckets
// over them, which is what recomputes the aggregates. The region below the live-start boundary is
// the backfill's and is rebuilt through history_epoch instead. Design notes are in docs/ARCHITECTURE.md.

package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

// RepairStatus is the outcome of one poster-gas repair step.
type RepairStatus int

// Poster-gas repair step outcomes.
const (
	// RepairProgressed means one batch was examined: read and written, or
	// passed over because it was too old to rebuild.
	RepairProgressed RepairStatus = iota
	// RepairIdle means there is work but this step did none, because the
	// fast loop is catching up and history yields to it.
	RepairIdle
	// RepairNone means nothing is left to repair, so the caller is free to
	// spend the step on the backfill.
	RepairNone
)

// posterGasCursor is the durable progress of the repair. Every block written since receipts were
// introduced carries poster gas, so Done is reached once and stays true.
type posterGasCursor struct {
	Next uint64 `json:"next"`
	Done bool   `json:"done"`
	// Skipped are blocks a sweep gave up on. The cursor only moves forward, so without them a block
	// that failed during a brief outage would be behind it for good, restart or not.
	Skipped []uint64 `json:"skipped,omitempty"`
	// Sweeps counts the re-sweeps, so a block that fails every time ends the pass.
	Sweeps int `json:"sweeps,omitempty"`
}

// loadPosterGasCursor reads the checkpoint. An unreadable one starts the pass over: every write is
// guarded on the row still lacking the value, so repeating work is harmless.
func (f *Follower) loadPosterGasCursor(ctx context.Context) (*posterGasCursor, error) {
	raw, ok, err := f.store.GetState(ctx, f.chainID, db.StatePosterGasRepair)
	if err != nil {
		return nil, err
	}
	c := &posterGasCursor{}
	if !ok {
		return c, nil
	}
	if err := json.Unmarshal([]byte(raw), c); err != nil {
		f.log.Warn("unreadable poster gas repair cursor, starting the pass again", "value", raw, "err", err.Error())
		return &posterGasCursor{}, nil
	}
	return c, nil
}

func (f *Follower) savePosterGasCursor(ctx context.Context, s db.Store, c *posterGasCursor) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.SetState(ctx, f.chainID, db.StatePosterGasRepair, string(b))
}

// maxRepairAttempts is how often a single block is read before the pass moves past it. The error can
// be deterministic (receipts that do not describe the block) or transient (an endpoint unreachable,
// rate limited or behind), and the two cannot be told apart: a lagging endpoint answers for a block it
// lacks with a null receipt set. Skipping on the first error would let one outage walk the cursor
// through a range. The count is in memory, per process and per sweep.
const maxRepairAttempts = 3

// maxRepairSweeps bounds how often the pass goes back for the blocks it gave up on, since the cursor
// only moves forward: without a bound a block that can never be read would keep it going for good.
const maxRepairSweeps = 3

// maxRepairSkipped bounds the block numbers a cursor carries; past it the log is the record.
const maxRepairSkipped = 10_000

// repairBatchFor sizes the batch as the backfill does: the configured header batch, capped by the
// spare budget, narrowed while batches fail. Receipts cost one call per block, so the whole budget counts.
func (f *Follower) repairBatchFor(narrowed int) int {
	n := f.cfg.HeaderBatchSize
	if avail := max(f.rpc.Available(), 0); avail < n {
		n = max(avail, minBackfillBatch)
	}
	// The narrowing is applied last and wins: a spare budget below the smallest batch would otherwise
	// raise it back up, and a batch that fails whole would never reach the single-block path.
	if narrowed > 0 && narrowed < n {
		n = narrowed
	}
	return max(n, 1)
}

// RepairStep fills in poster gas for one batch. A step discarded by a rewind is not an error: the
// cursor is unmoved and the next step reads the canonical rows.
func (f *Follower) RepairStep(ctx context.Context) (RepairStatus, error) {
	status, err := f.repairStep(ctx)
	if errors.Is(err, errStaleGeneration) {
		f.log.Warn("poster gas repair step discarded after a rewind", "err", err.Error())
		return RepairIdle, nil
	}
	return status, err
}

func (f *Follower) repairStep(ctx context.Context) (RepairStatus, error) {
	if err := f.ensureInit(ctx); err != nil {
		return RepairIdle, err
	}
	// Captured before any network call and compared inside the transaction
	// that commits the step, exactly as the backfill and the gap filler do.
	gen, err := f.generation(ctx, f.store)
	if err != nil {
		return RepairIdle, err
	}
	c, err := f.loadPosterGasCursor(ctx)
	if err != nil {
		return RepairIdle, err
	}
	if c.Done {
		return RepairNone, nil
	}
	f.mu.Lock()
	boundary, hasBoundary := f.boundaryLocked()
	narrowed := f.repairNarrow
	f.mu.Unlock()
	if !hasBoundary {
		// No block is row-backed yet, so no bucket here could be rebuilt.
		return RepairNone, nil
	}
	if f.repairMustWait() {
		return RepairIdle, nil
	}
	rows, err := f.store.BlocksMissingPosterGas(ctx, f.chainID, c.Next, f.repairBatchFor(narrowed))
	if err != nil {
		return RepairIdle, fmt.Errorf("blocks missing poster gas from %d: %w", c.Next, err)
	}
	if len(rows) == 0 {
		return f.repairSwept(ctx, c)
	}
	next := rows[len(rows)-1].Number + 1

	// Which rows are worth a call. Correctness is the store's: RebuildBuckets refuses a window below
	// the prune frontier itself. What is left is cost, answered off the same recorded frontier so the
	// two cannot disagree: a row whose finest bucket is already below it can rebuild nothing. The
	// boundary is this pass's own: buckets below it are the backfill's. The test is the row's
	// timestamp, the split commitFill makes, so the boundary hour's rows below the first live block stay.
	frontier, err := f.pruneFrontier(ctx)
	if err != nil {
		return RepairIdle, err
	}
	finest := db.Resolutions[db.ResolutionOrder[0]]
	targets := make([]nitro.ReceiptTarget, 0, len(rows))
	repairable := make([]db.Block, 0, len(rows))
	var backfilled, stale int
	for _, b := range rows {
		switch {
		case b.TS.Before(boundary):
			backfilled++
		case b.TS.UTC().Truncate(finest).Before(frontier):
			stale++
		default:
			targets = append(targets, nitro.ReceiptTarget{Number: b.Number, Hash: b.Hash, TxCount: b.TxCount, GasUsed: b.GasUsed})
			repairable = append(repairable, b)
		}
	}
	if backfilled > 0 {
		f.log.Debug("passing over blocks whose buckets the backfill owns", "blocks", backfilled, "boundary", boundary)
	}
	if stale > 0 {
		f.log.Info("passing over blocks whose buckets prune has already cut into", "blocks", stale, "frontier", frontier)
	}
	if len(targets) == 0 {
		c.Next = next
		return RepairProgressed, f.savePosterGasCursor(ctx, f.store, c)
	}

	gas, err := f.rpc.PosterGasByNumbers(ctx, targets)
	if err != nil {
		return f.repairFailed(ctx, c, targets, err)
	}
	f.widenRepair()
	c.Next = next
	if err := f.withGeneration(ctx, gen, func(s db.Store) error {
		if err := s.SetPosterGas(ctx, f.chainID, gas); err != nil {
			return err
		}
		for _, res := range db.ResolutionOrder {
			// Every resolution is asked for; the store drops the windows it must not touch.
			if err := s.RebuildBuckets(ctx, f.chainID, res, db.BucketStarts(repairable, res)); err != nil {
				return fmt.Errorf("poster gas %s buckets: %w", res, err)
			}
		}
		return f.savePosterGasCursor(ctx, s, c)
	}); err != nil {
		return RepairIdle, err
	}
	f.log.Info("repaired poster gas", "blocks", len(gas), "from", repairable[0].Number, "to", repairable[len(repairable)-1].Number)
	return RepairProgressed, nil
}

// repairFailed narrows the batch and retries; a lone block is retried maxRepairAttempts times before
// the pass moves past it, so an outage cannot walk the forward-only cursor through a range.
func (f *Follower) repairFailed(ctx context.Context, c *posterGasCursor, targets []nitro.ReceiptTarget, cause error) (RepairStatus, error) {
	first, last := targets[0].Number, targets[len(targets)-1].Number
	if len(targets) > 1 {
		f.mu.Lock()
		f.repairNarrow = len(targets) / 2
		f.mu.Unlock()
		return RepairIdle, fmt.Errorf("poster gas receipts %d..%d: %w", first, last, cause)
	}
	f.mu.Lock()
	if f.repairBlock != first {
		f.repairBlock, f.repairAttempts = first, 0
	}
	f.repairAttempts++
	attempts := f.repairAttempts
	f.mu.Unlock()
	if attempts < maxRepairAttempts {
		return RepairIdle, fmt.Errorf("poster gas receipts for block %d, attempt %d of %d: %w", first, attempts, maxRepairAttempts, cause)
	}
	f.log.Warn("passing over a block after repeated failures, it is swept again before the pass ends", "block", first, "attempts", attempts, "err", cause.Error())
	c.Next = first + 1
	if len(c.Skipped) < maxRepairSkipped {
		c.Skipped = append(c.Skipped, first)
	}
	return RepairProgressed, f.savePosterGasCursor(ctx, f.store, c)
}

// repairSwept answers a sweep that found nothing left: blocks it gave up on are swept again, up to
// maxRepairSweeps, since the cause was as likely an endpoint's bad few minutes as the block.
func (f *Follower) repairSwept(ctx context.Context, c *posterGasCursor) (RepairStatus, error) {
	if len(c.Skipped) == 0 {
		c.Done = true
		f.log.Info("poster gas repair finished", "through", c.Next, "sweeps", c.Sweeps+1)
		return RepairNone, f.savePosterGasCursor(ctx, f.store, c)
	}
	if c.Sweeps+1 >= maxRepairSweeps {
		c.Done = true
		f.log.Warn("poster gas repair finished with blocks it could not read", "blocks", len(c.Skipped), "first", c.Skipped[0], "sweeps", c.Sweeps+1)
		return RepairNone, f.savePosterGasCursor(ctx, f.store, c)
	}
	from := slices.Min(c.Skipped)
	f.log.Info("sweeping the blocks the poster gas repair passed over", "blocks", len(c.Skipped), "from", from, "sweep", c.Sweeps+2)
	c.Next, c.Skipped, c.Sweeps = from, nil, c.Sweeps+1
	f.widenRepair()
	return RepairProgressed, f.savePosterGasCursor(ctx, f.store, c)
}

// widenRepair drops the narrowing and the per-block attempt count a failure
// imposed, once a batch succeeds.
func (f *Follower) widenRepair() {
	f.mu.Lock()
	f.repairNarrow, f.repairBlock, f.repairAttempts = 0, 0, 0
	f.mu.Unlock()
}
