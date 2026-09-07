// Poster gas that was never recorded. Receipt-backed poster gas arrived after
// the collector had already been storing blocks, so every row written before
// it carries none, and a bucket is only as good as its worst source block: one
// block without receipts leaves the whole bucket with no poster gas, no
// compute-gas rate and no fee split. Nothing repairs that on its own. Retention
// never drops a bucket, and no loop revisits one it has finished, so those
// buckets would stay blank for as long as they are served.
//
// This is the pass that fills them. It walks the blocks that still lack the
// value, reads eth_getBlockReceipts for each (one call, not the two a header
// read costs, since the stored row already carries what the receipts are
// checked against), writes the gas onto the rows and rebuilds the buckets over
// them. Rebuilding is what actually repairs the history: the bucket aggregate
// recomputes poster gas, the compute-gas rate and the floor, surplus and
// poster fee columns from the rows underneath it.
//
// It is bounded on both ends. Below, it starts at the first live block,
// because only buckets from there on are rebuilt from rows; the ones under
// that belong to the backfill, which owns them additively and rebuilds them
// through history_epoch instead. Above, it stops where block retention is
// about to remove the rows a rebuild would read, since repairing a block whose
// bucket can no longer be rebuilt would spend a call for nothing.

package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// posterGasCursor is the durable progress of the repair: the next block it
// will examine, and whether it has run out of work. Every block written since
// receipts were introduced carries poster gas, so Done is reached once and
// stays true; a rewind that removes blocks cannot create new work below the
// cursor, because the fast loop rewrites what it re-fetches with the receipts
// already attached.
type posterGasCursor struct {
	Next uint64 `json:"next"`
	Done bool   `json:"done"`
}

// loadPosterGasCursor reads the checkpoint. An unreadable one starts the pass
// over rather than failing the loop: repeating work is harmless here, since
// every write is guarded on the row still lacking the value.
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

// repairPruneSlack keeps the pass off buckets the next prune is about to cut
// into. A bucket is rebuilt from the rows inside its window, so a window that
// has lost part of its rows rebuilds short: fewer blocks, less gas, a bucket
// that reads as a quiet stretch that never happened. That is worse than
// leaving it unrepaired.
//
// The condition is on the bucket's own start, not the block's timestamp: a
// window is whole exactly while its start is at or above the retention
// horizon. Measuring it that way rather than through a fixed margin on the
// block keeps the pass working at any configured block_retention, including
// one shorter than a bucket, where a margin wide enough to be safe at 48h
// would put the cutoff past the present and quietly repair nothing.
//
// The slack is for the prune that runs on the slow loop while a step is
// reading: minutes, where a step is seconds.
const repairPruneSlack = 5 * time.Minute

// maxRepairAttempts is how often a single block is read before the pass gives
// up on it and moves past. A batch that fails narrows to one block, and the
// error there can be either kind: receipts that do not describe the block
// (deterministic, no retry will help) or the endpoint being unreachable, rate
// limited or behind (transient, and every retry might). Counting attempts
// covers both without having to tell them apart, which matters because the
// two are not reliably distinguishable: an endpoint that has fallen behind
// answers for a block it does not have with a null receipt set, exactly like
// a block that cannot be verified. Skipping on the first error would let one
// outage walk the cursor through a whole range and leave it unrepairable.
// The count is in memory, so a restart gives every skipped block another go.
const maxRepairAttempts = 3

// repairBatchFor sizes the next batch the way the backfill sizes its own: the
// configured header batch, capped by what the token bucket holds spare so the
// pass never holds the turnstile through a long sleep, and narrowed further
// while batches are failing. Receipts cost one call per block, so the whole
// available budget counts rather than half of it.
func (f *Follower) repairBatchFor(narrowed int) int {
	n := f.cfg.HeaderBatchSize
	if narrowed > 0 && narrowed < n {
		n = narrowed
	}
	if avail := max(f.rpc.Available(), 0); avail < n {
		n = max(avail, minBackfillBatch)
	}
	return max(n, 1)
}

// RepairStep fills in poster gas for one batch of the blocks stored without
// it and rebuilds the buckets over them. A step discarded by a rewind is not
// an error: the cursor is unmoved and the next step reads the canonical rows.
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
	if f.historyMustWait() {
		return RepairIdle, nil
	}
	rows, err := f.store.BlocksMissingPosterGas(ctx, f.chainID, c.Next, f.repairBatchFor(narrowed))
	if err != nil {
		return RepairIdle, fmt.Errorf("blocks missing poster gas from %d: %w", c.Next, err)
	}
	if len(rows) == 0 {
		c.Done = true
		f.log.Info("poster gas repair finished", "through", c.Next)
		return RepairNone, f.savePosterGasCursor(ctx, f.store, c)
	}
	next := rows[len(rows)-1].Number + 1

	// Which rows are worth a call. Two ways one is not, and both are read
	// off the bucket the row belongs to rather than off the row: what gets
	// rebuilt is the bucket, so the bucket is what has to be in scope and
	// whole. Below the boundary the buckets are the backfill's, which owns
	// them additively (history_epoch rebuilds those); at or above it they
	// are rebuilt from rows, including the rows of the boundary hour that
	// sit below the first live block. Past the retention horizon the window
	// is losing the rows a rebuild would read.
	horizon := f.now().Add(-f.cfg.BlockRetention).Add(repairPruneSlack)
	targets := make([]nitro.ReceiptTarget, 0, len(rows))
	repairable := make([]db.Block, 0, len(rows))
	var backfilled, stale int
	for _, b := range rows {
		bucket := b.TS.UTC().Truncate(boundaryWidth)
		switch {
		case bucket.Before(boundary):
			backfilled++
		case bucket.Before(horizon):
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
		f.log.Info("passing over blocks whose buckets retention empties before a rebuild could read them", "blocks", stale, "horizon", horizon)
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
			if err := s.RebuildBuckets(ctx, f.chainID, res, db.BucketStarts(repairable, res)); err != nil {
				return fmt.Errorf("poster gas buckets: %w", err)
			}
		}
		return f.savePosterGasCursor(ctx, s, c)
	}); err != nil {
		return RepairIdle, err
	}
	f.log.Info("repaired poster gas", "blocks", len(gas), "from", repairable[0].Number, "to", repairable[len(repairable)-1].Number)
	return RepairProgressed, nil
}

// repairFailed narrows the batch and retries. Once a single block is left it
// is retried maxRepairAttempts times before the pass gives up on it and moves
// past, leaving it without poster gas: the bucket over it goes on reporting
// that it has none, which is true. Retrying first is what keeps an endpoint
// that is unreachable, rate limited or behind from walking the cursor through
// a range one block per step and leaving all of it unrepairable, since the
// cursor only ever moves forward.
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
	f.log.Warn("leaving a block without poster gas after repeated failures", "block", first, "attempts", attempts, "err", cause.Error())
	c.Next = first + 1
	return RepairProgressed, f.savePosterGasCursor(ctx, f.store, c)
}

// widenRepair drops the narrowing and the per-block attempt count a failure
// imposed, once a batch succeeds.
func (f *Follower) widenRepair() {
	f.mu.Lock()
	f.repairNarrow, f.repairBlock, f.repairAttempts = 0, 0, 0
	f.mu.Unlock()
}
