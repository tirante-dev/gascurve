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
// It is bounded on both ends, and both bounds are read off the bucket a row
// falls in rather than off the row, because the bucket is what gets rebuilt.
// Below, buckets starting before the live-start boundary belong to the
// backfill, which owns them additively and rebuilds them through
// history_epoch instead. Above, it stops where prune is about to remove the
// rows a rebuild would read, since repairing a block whose bucket can no
// longer be rebuilt would spend a call for nothing. That upper bound is
// prune's own cutoff and not the nominal retention window, because the two
// differ while the backfill is unfinished.

package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// Skipped are blocks a sweep gave up on after maxRepairAttempts. The
	// cursor only moves forward, so without recording them a block that
	// failed while an endpoint was briefly unavailable would be behind the
	// cursor for good, and a restart would resume past it rather than retry
	// it. They are swept again before the pass calls itself done.
	Skipped []uint64 `json:"skipped,omitempty"`
	// Sweeps counts the re-sweeps already made, so a block that fails every
	// time ends the pass instead of circling in it.
	Sweeps int `json:"sweeps,omitempty"`
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
// The count is in memory: it measures one endpoint's bad few minutes, not the
// block, so it starts over per process and per sweep.
const maxRepairAttempts = 3

// maxRepairSweeps bounds how often the pass goes back for the blocks it gave
// up on. The cursor only moves forward, so a block passed over is behind it
// and needs a sweep of its own to be seen again; without a bound a block that
// can never be read would keep the pass going for the life of the process.
const maxRepairSweeps = 3

// maxRepairSkipped bounds the block numbers a cursor carries, so a long
// outage cannot grow the checkpoint without limit. Past it the pass stops
// recording them and the log is the record.
const maxRepairSkipped = 10_000

// repairBatchFor sizes the next batch the way the backfill sizes its own: the
// configured header batch, capped by what the token bucket holds spare so the
// pass never holds the turnstile through a long sleep, and narrowed further
// while batches are failing. Receipts cost one call per block, so the whole
// available budget counts rather than half of it.
func (f *Follower) repairBatchFor(narrowed int) int {
	n := f.cfg.HeaderBatchSize
	if avail := max(f.rpc.Available(), 0); avail < n {
		n = max(avail, minBackfillBatch)
	}
	// The narrowing is applied last and wins. A spare budget below the
	// smallest batch raises the size back to minBackfillBatch, and a batch
	// that fails whole because one target in it will not read would then
	// never reach the single-block path that steps that target over: the
	// repair would retry the same few blocks for good.
	if narrowed > 0 && narrowed < n {
		n = narrowed
	}
	return max(n, 1)
}

// wholeResolutions names the stored resolutions whose bucket over ts still
// has every one of its rows, which is the condition for rebuilding it from
// them. A bucket is whole while its own start is at or above the retention
// horizon; the finer the resolution, the later its bucket starts, so a row
// near the horizon can repair its minute and quarter-hour buckets after its
// hour has already gone short.
func wholeResolutions(ts, horizon time.Time) []string {
	out := make([]string, 0, len(db.ResolutionOrder))
	for _, res := range db.ResolutionOrder {
		if !ts.UTC().Truncate(db.Resolutions[res]).Before(horizon) {
			out = append(out, res)
		}
	}
	return out
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
		return f.repairSwept(ctx, c)
	}
	next := rows[len(rows)-1].Number + 1

	// Which rows are worth a call, and which resolutions each one can still
	// repair. Both questions are asked of the buckets the row falls in
	// rather than of the row, because the bucket is what gets rebuilt.
	//
	// Below the boundary the buckets are the backfill's, which owns them
	// additively (history_epoch rebuilds those). The test is the row's own
	// timestamp, the same split commitFill makes, so the rows of the
	// boundary hour that sit below the first live block stay in scope.
	//
	// Past the horizon a window is losing the rows a rebuild would read, and
	// that falls per resolution rather than all at once: a horizon inside an
	// hour leaves that hour's bucket short while the minute and quarter-hour
	// buckets after it are whole. Taking the hour as the answer for all
	// three would step the cursor past those rows and leave the finer
	// buckets blank for good, so a row is read when any resolution can still
	// use it and only those resolutions are rebuilt.
	//
	// The horizon is prune's own cutoff, not the nominal retention window.
	// While the backfill is unfinished prune keeps every row from the
	// boundary hour up, however old, and reading retention literally would
	// abandon buckets whose rows are all still there.
	cutoff, pinned, err := f.pruneCutoff(ctx, boundary, true)
	if err != nil {
		return RepairIdle, err
	}
	// The slack is for a cutoff that walks forward with the clock while the
	// step reads. A cutoff pinned to the boundary does not move at all, so
	// it takes none: adding any there would abandon the rows just above it.
	horizon := cutoff
	if !pinned {
		horizon = cutoff.Add(repairPruneSlack)
	}
	// What prune would delete now is a prediction; what it has already
	// deleted is a fact, and the two disagree after block_retention is
	// raised, because the cutoff moves back over rows the shorter setting
	// removed while those rows stay gone. Rebuilding the bucket that
	// straddles that frontier would sum only its surviving suffix and
	// replace a correct aggregate with a short one, which is the one way
	// this pass could leave history worse than it found it.
	frontier, err := f.pruneFrontier(ctx)
	if err != nil {
		return RepairIdle, err
	}
	if frontier.After(horizon) {
		horizon = frontier
	}
	targets := make([]nitro.ReceiptTarget, 0, len(rows))
	repairable := make([]db.Block, 0, len(rows))
	byResolution := make(map[string][]db.Block, len(db.ResolutionOrder))
	var backfilled, stale int
	for _, b := range rows {
		if b.TS.Before(boundary) {
			backfilled++
			continue
		}
		whole := wholeResolutions(b.TS, horizon)
		if len(whole) == 0 {
			stale++
			continue
		}
		targets = append(targets, nitro.ReceiptTarget{Number: b.Number, Hash: b.Hash, TxCount: b.TxCount, GasUsed: b.GasUsed})
		repairable = append(repairable, b)
		for _, res := range whole {
			byResolution[res] = append(byResolution[res], b)
		}
	}
	if backfilled > 0 {
		f.log.Debug("passing over blocks whose buckets the backfill owns", "blocks", backfilled, "boundary", boundary)
	}
	if stale > 0 {
		f.log.Info("passing over blocks no resolution can still rebuild whole", "blocks", stale, "horizon", horizon)
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
			rebuild := byResolution[res]
			if len(rebuild) == 0 {
				continue
			}
			if err := s.RebuildBuckets(ctx, f.chainID, res, db.BucketStarts(rebuild, res)); err != nil {
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
	f.log.Warn("passing over a block after repeated failures, it is swept again before the pass ends", "block", first, "attempts", attempts, "err", cause.Error())
	c.Next = first + 1
	if len(c.Skipped) < maxRepairSkipped {
		c.Skipped = append(c.Skipped, first)
	}
	return RepairProgressed, f.savePosterGasCursor(ctx, f.store, c)
}

// repairSwept answers a sweep that found nothing left to read. Blocks the
// sweep gave up on are taken from the top again, since the reason was as
// likely to have been an endpoint having a bad few minutes as anything about
// the block. Only when a sweep skips nothing, or the pass has swept
// maxRepairSweeps times, does it call itself done: a block that fails on
// every sweep must end the pass rather than circle in it.
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
