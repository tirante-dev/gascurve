package collector

import (
	"context"
	"fmt"
	"strconv"

	"github.com/tirante-dev/gascurve/internal/db"
)

// historyEpoch reads the epoch the reconstructed history was last rebuilt
// at. A chain that has never rebuilt reports 0, as does an unreadable
// value: the checkpoint only decides whether a rebuild runs, and the
// configured epoch has to be raised past it either way.
func (f *Follower) historyEpoch(ctx context.Context, s db.Store) (int, error) {
	raw, ok, err := s.GetState(ctx, f.chainID, db.StateHistoryEpoch)
	if err != nil || !ok {
		return 0, err
	}
	e, err := strconv.Atoi(raw)
	if err != nil {
		f.log.Warn("unreadable history epoch checkpoint, treating it as zero", "value", raw, "err", err.Error())
		return 0, nil
	}
	return e, nil
}

// applyHistoryEpochLocked rebuilds the reconstructed history once when the
// network's history_epoch is above the epoch stored for the chain, and
// records the new epoch so the next start is a no-op. That is what makes
// it safe in a restarting pod: the rebuild is keyed to a raised number,
// not to the presence of a setting, so a container that comes back with
// the same configuration does not rebuild again.
//
// The rebuild drops what the backfill owns and nothing else. The
// backfill's buckets below the bucket boundary go, its cursor starts over
// and the owner-scan checkpoints are cleared so the origin is established
// again, this time sampling the pricing state from the archive endpoint
// the network has meanwhile been given. Block rows, owner actions,
// constraint sets and the row-backed buckets above the boundary are
// untouched: they are observations, not reconstruction. The generation is
// bumped inside the same transaction, so a backfill or gap-fill step that
// fetched before the rebuild discards its work instead of committing it
// onto history that is being replaced.
//
// Clearing owner_scan_through with the rest is what orders the rebuild:
// the backfill starts no segment until a scan pass has completed again,
// and that pass is what re-establishes the origin. The owner actions
// themselves survive, so the rescan re-reads logs it has already recorded
// and writes the same rows, costing one pass rather than any history.
//
// Called with f.mu held, from ensureInit, so the loops see either the old
// history or the reset one and never a half-cleared database. It runs
// before the owner-scan checkpoints are read, so a checkpoint too damaged
// to parse cannot block the rebuild that would replace it; the live start
// is the only state it needs loaded, for the bucket boundary.
func (f *Follower) applyHistoryEpochLocked(ctx context.Context) error {
	want := f.net.HistoryEpoch
	if want <= 0 {
		return nil
	}
	have, err := f.historyEpoch(ctx, f.store)
	if err != nil {
		return err
	}
	if want <= have {
		return nil
	}
	boundary, hasBoundary := f.boundaryLocked()
	var deleted int64
	rebuilt := false
	err = f.store.WithChainTx(ctx, f.chainID, func(s db.Store) error {
		deleted, rebuilt = 0, false
		// Re-read inside the transaction: another collector for this chain
		// may have rebuilt while this one was starting.
		current, err := f.historyEpoch(ctx, s)
		if err != nil {
			return err
		}
		if want <= current {
			f.log.Info("another writer rebuilt the history at this epoch already", "epoch", current)
			return nil
		}
		if err := f.bumpGeneration(ctx, s); err != nil {
			return err
		}
		if hasBoundary {
			if deleted, err = s.DeleteBucketsBefore(ctx, f.chainID, boundary); err != nil {
				return fmt.Errorf("delete backfilled buckets: %w", err)
			}
		}
		if err := f.saveCursor(ctx, s, &backfillCursor{}); err != nil {
			return err
		}
		for _, key := range []string{db.StateOwnerScanOrigin, db.StateOwnerLogCursor, db.StateOwnerScanThrough} {
			if err := s.DeleteState(ctx, f.chainID, key); err != nil {
				return fmt.Errorf("clear %s: %w", key, err)
			}
		}
		if err := f.resetHistoryHoles(ctx, s, deleted > 0); err != nil {
			return err
		}
		rebuilt = true
		return s.SetState(ctx, f.chainID, db.StateHistoryEpoch, strconv.Itoa(want))
	})
	if err != nil {
		return fmt.Errorf("rebuild history at epoch %d: %w", want, err)
	}
	if !rebuilt {
		return nil
	}
	// The archive client is bound after the endpoint pool is verified,
	// which is later than this, so the configuration answers here.
	archive := f.net.HasArchive()
	f.log.Info("rebuilding the reconstructed history", "epoch", want, "previousEpoch", have, "bucketsDeleted", deleted, "archive", archive)
	if !archive {
		f.log.Warn("the history rebuild has no endpoint marked archive, the replay will be unanchored again")
	}
	// The scan-state load that follows in ensureInit reads the same
	// cleared checkpoints; clearing them here keeps the rebuild correct on
	// its own rather than through its position in that sequence.
	f.scanOrigin = nil
	f.ownerScanThrough = 0
	return nil
}

// resetHistoryHoles puts the recorded ranges back in step with history that is
// being rebuilt. A range blocked for lack of state becomes pending so it is
// examined again against the origin the rebuild re-establishes, which is the
// point of rebuilding after an archive endpoint appears. The durable record
// stays visible until it is actually filled. A range that had folded into the
// deleted additive buckets restarts, since its contribution went with them.
// A row-backed range keeps its progress because those buckets were not deleted.
func (f *Follower) resetHistoryHoles(ctx context.Context, s db.Store, bucketsDeleted bool) error {
	holes, err := f.loadHoles(ctx, s)
	if err != nil || len(holes) == 0 {
		return err
	}
	out := make([]hole, 0, len(holes))
	changed := false
	for _, h := range holes {
		if h.Reason == reasonNoState {
			f.log.Info("requeueing a blocked range so the rebuild can examine it again", "from", h.From, "to", h.To)
			h.Lifecycle, h.Reason, h.NextRetryAt, h.LastError = rangePending, "", "", ""
			changed = true
		}
		if bucketsDeleted && h.Folded > 0 {
			f.log.Info("restarting a range whose folded buckets the rebuild deleted", "from", h.From, "to", h.To)
			h.Next, h.State, h.Folded, h.CursorAt = 0, nil, 0, ""
			changed = true
		}
		out = append(out, h)
	}
	if !changed {
		return nil
	}
	return f.saveHoles(ctx, s, out)
}
