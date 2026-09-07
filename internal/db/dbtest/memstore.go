// Package dbtest provides an in-memory db.Store for unit tests of the
// collector and the API. It mirrors the Postgres semantics that matter to
// callers: ordering, ranges, upserts and bucket folding.
package dbtest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/tirante-dev/gascurve/internal/db"
)

// MemStore is a thread-safe in-memory db.Store. WithChainTx holds a
// per-chain mutex for the duration of fn, so concurrent loops in a test
// serialize their chain transactions the way the advisory lock does.
type MemStore struct {
	mu sync.Mutex
	// chainMu serializes WithChainTx per chain; chainLocks guards the map.
	chainLocks sync.Mutex
	chainMu    map[uint64]*sync.Mutex

	NetworkRows   map[uint64]db.Network
	BlockRows     map[uint64]map[uint64]db.Block
	BucketRows    map[string]db.Bucket
	SampleRows    []db.StateSample
	MissingRows   map[uint64][]db.MissingRange
	ActionRows    map[string]db.OwnerAction
	SetRows       []db.ConstraintSet
	ReportRows    map[string]db.BatchReport
	StateRows     map[string]string
	Notifications []db.Notification

	// FailOn makes the named method return ErrInjected.
	FailOn map[string]bool
	// Hooks run just before the named method does its work, so a test can
	// change state while a transaction the store opened is still running.
	Hooks map[string]func()
	// PingErr is returned by Ping when set.
	PingErr error
	nextSet int64
}

// ErrInjected is returned by methods listed in FailOn.
var ErrInjected = errors.New("injected failure")

func New() *MemStore {
	return &MemStore{
		NetworkRows: map[uint64]db.Network{},
		BlockRows:   map[uint64]map[uint64]db.Block{},
		MissingRows: map[uint64][]db.MissingRange{},
		BucketRows:  map[string]db.Bucket{},
		ActionRows:  map[string]db.OwnerAction{},
		ReportRows:  map[string]db.BatchReport{},
		StateRows:   map[string]string{},
		FailOn:      map[string]bool{},
		Hooks:       map[string]func(){},
		chainMu:     map[uint64]*sync.Mutex{},
	}
}

func (m *MemStore) MissingRanges(_ context.Context, chainID uint64) ([]db.MissingRange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("MissingRanges"); err != nil {
		return nil, err
	}
	out := append([]db.MissingRange(nil), m.MissingRows[chainID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out, nil
}

// ReplaceMissingRanges replaces one chain's intervals after copying their byte slices, matching the
// value semantics of committed database rows.
func (m *MemStore) ReplaceMissingRanges(_ context.Context, chainID uint64, ranges []db.MissingRange) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("ReplaceMissingRanges"); err != nil {
		return err
	}
	out := make([]db.MissingRange, len(ranges))
	copy(out, ranges)
	for i := range out {
		out[i].ChainID = chainID
		out[i].ReplayState = append(db.JSONB(nil), out[i].ReplayState...)
	}
	if len(out) == 0 {
		delete(m.MissingRows, chainID)
		return nil
	}
	m.MissingRows[chainID] = out
	return nil
}

// hook runs the test hook of a method, outside the store lock so it may call back into the follower.
func (m *MemStore) hook(method string) {
	m.mu.Lock()
	fn := m.Hooks[method]
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (m *MemStore) fail(method string) error {
	if m.FailOn[method] {
		return ErrInjected
	}
	return nil
}

func (m *MemStore) Ping(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.PingErr
}

// SetFailure toggles the injected failure of a method under the lock, so tests may flip it from
// another goroutine.
func (m *MemStore) SetFailure(method string, fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fail {
		m.FailOn[method] = true
		return
	}
	delete(m.FailOn, method)
}

func (m *MemStore) WithTx(_ context.Context, fn func(db.Store) error) error {
	m.mu.Lock()
	err := m.fail("WithTx")
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return fn(m)
}

// WithChainTx runs fn holding the chain's mutex, mirroring the advisory lock: nested calls for the
// same chain reuse the transaction, one for a higher chain id adds its lock, and the incompatible
// combinations fail exactly as Postgres refuses them.
func (m *MemStore) WithChainTx(ctx context.Context, chainID uint64, fn func(db.Store) error) error {
	m.mu.Lock()
	err := m.fail("WithChainTx")
	m.mu.Unlock()
	if err != nil {
		return err
	}
	unlock := m.lockChain(chainID)
	defer unlock()
	return fn(&chainTx{MemStore: m, locks: []uint64{chainID}})
}

func (m *MemStore) lockChain(chainID uint64) func() {
	m.chainLocks.Lock()
	mu, ok := m.chainMu[chainID]
	if !ok {
		mu = &sync.Mutex{}
		m.chainMu[chainID] = mu
	}
	m.chainLocks.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (m *MemStore) WithSnapshotTx(_ context.Context, fn func(db.Store) error) error {
	m.mu.Lock()
	err := m.fail("WithSnapshotTx")
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return fn(m)
}

// chainTx is the store bound to an open chain transaction, carrying the chain locks it holds so nested
// calls can be checked the way Postgres checks them.
type chainTx struct {
	*MemStore
	locks []uint64
}

func (c *chainTx) WithTx(_ context.Context, fn func(db.Store) error) error { return fn(c) }

// WithChainTx reuses the open transaction for a chain it already locks, adds a missing lock in
// ascending order, and refuses one that would invert that order.
func (c *chainTx) WithChainTx(_ context.Context, chainID uint64, fn func(db.Store) error) error {
	for _, held := range c.locks {
		if held == chainID {
			return fn(c)
		}
	}
	if highest := slices.Max(c.locks); chainID < highest {
		return fmt.Errorf("%w: chain %d after %d", db.ErrLockOrder, chainID, highest)
	}
	unlock := c.lockChain(chainID)
	defer unlock()
	return fn(&chainTx{MemStore: c.MemStore, locks: append(append([]uint64(nil), c.locks...), chainID)})
}

// WithSnapshotTx refuses to run inside a writing transaction: the caller would read its own
// uncommitted rows instead of one database moment.
func (c *chainTx) WithSnapshotTx(context.Context, func(db.Store) error) error {
	return db.ErrNestedSnapshot
}

// UpsertNetwork stores static fields, keeping head data.
func (m *MemStore) UpsertNetwork(_ context.Context, n db.Network) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("UpsertNetwork"); err != nil {
		return err
	}
	old, ok := m.NetworkRows[n.ChainID]
	if ok {
		old.Name, old.DisplayName, old.ExplorerURL, old.Enabled = n.Name, n.DisplayName, n.ExplorerURL, n.Enabled
		m.NetworkRows[n.ChainID] = old
		return nil
	}
	m.NetworkRows[n.ChainID] = n
	return nil
}

func (m *MemStore) Networks(context.Context) ([]db.Network, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("Networks"); err != nil {
		return nil, err
	}
	out := make([]db.Network, 0, len(m.NetworkRows))
	for _, n := range m.NetworkRows {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChainID < out[j].ChainID })
	return out, nil
}

// NetworkByRef resolves a chain id (a decimal within the BIGINT range) or otherwise a name, never both.
func (m *MemStore) NetworkByRef(_ context.Context, ref string) (*db.Network, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("NetworkByRef"); err != nil {
		return nil, err
	}
	id, byID := db.ChainIDRef(ref)
	for _, n := range m.NetworkRows {
		if (byID && n.ChainID == uint64(id)) || (!byID && n.Name == ref) {
			n := n
			return &n, nil
		}
	}
	return nil, nil
}

func (m *MemStore) UpdateNetworkHead(_ context.Context, chainID, headBlock uint64, headAt, sampledAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("UpdateNetworkHead"); err != nil {
		return err
	}
	n := m.NetworkRows[chainID]
	n.ChainID = chainID
	n.HeadBlock = sql.NullInt64{Int64: int64(headBlock), Valid: true}
	n.HeadAt = sql.NullTime{Time: headAt, Valid: true}
	n.LastSampleAt = sql.NullTime{Time: sampledAt, Valid: true}
	n.LastError = sql.NullString{}
	m.NetworkRows[chainID] = n
	return nil
}

// SetNetworkHead records the head after a rewind, leaving the fields without a value null and the last
// error alone.
func (m *MemStore) SetNetworkHead(_ context.Context, chainID uint64, headBlock *uint64, headAt, sampledAt *time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("SetNetworkHead"); err != nil {
		return err
	}
	n := m.NetworkRows[chainID]
	n.ChainID = chainID
	n.HeadBlock = sql.NullInt64{}
	n.HeadAt = sql.NullTime{}
	n.LastSampleAt = sql.NullTime{}
	if headBlock != nil {
		n.HeadBlock = sql.NullInt64{Int64: int64(*headBlock), Valid: true}
	}
	if headAt != nil {
		n.HeadAt = sql.NullTime{Time: *headAt, Valid: true}
	}
	if sampledAt != nil {
		n.LastSampleAt = sql.NullTime{Time: *sampledAt, Valid: true}
	}
	m.NetworkRows[chainID] = n
	return nil
}

func (m *MemStore) SetNetworkError(_ context.Context, chainID uint64, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("SetNetworkError"); err != nil {
		return err
	}
	n := m.NetworkRows[chainID]
	n.ChainID = chainID
	n.LastError = sql.NullString{String: msg, Valid: msg != ""}
	m.NetworkRows[chainID] = n
	return nil
}

func (m *MemStore) UpsertBlocks(_ context.Context, blocks []db.Block) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("UpsertBlocks"); err != nil {
		return err
	}
	for _, b := range blocks {
		if m.BlockRows[b.ChainID] == nil {
			m.BlockRows[b.ChainID] = map[uint64]db.Block{}
		}
		m.BlockRows[b.ChainID][b.Number] = b
	}
	return nil
}

func (m *MemStore) sortedBlocks(chainID uint64) []db.Block {
	out := make([]db.Block, 0, len(m.BlockRows[chainID]))
	for _, b := range m.BlockRows[chainID] {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func (m *MemStore) BlockByNumber(_ context.Context, chainID, number uint64) (*db.Block, error) {
	m.hook("BlockByNumber")
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("BlockByNumber"); err != nil {
		return nil, err
	}
	b, ok := m.BlockRows[chainID][number]
	if !ok {
		return nil, nil
	}
	return &b, nil
}

func (m *MemStore) DeleteBlocksAfter(_ context.Context, chainID, after uint64) ([]db.Block, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("DeleteBlocksAfter"); err != nil {
		return nil, err
	}
	var out []db.Block
	for _, b := range m.sortedBlocks(chainID) {
		if b.Number > after {
			out = append(out, b)
			delete(m.BlockRows[chainID], b.Number)
		}
	}
	return out, nil
}

func (m *MemStore) LatestBlock(_ context.Context, chainID uint64) (*db.Block, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("LatestBlock"); err != nil {
		return nil, err
	}
	bs := m.sortedBlocks(chainID)
	if len(bs) == 0 {
		return nil, nil
	}
	return &bs[len(bs)-1], nil
}

func (m *MemStore) OldestBlock(_ context.Context, chainID uint64) (*db.Block, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("OldestBlock"); err != nil {
		return nil, err
	}
	bs := m.sortedBlocks(chainID)
	if len(bs) == 0 {
		return nil, nil
	}
	return &bs[0], nil
}

func (m *MemStore) RecentBlocks(_ context.Context, chainID uint64, limit int) ([]db.Block, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("RecentBlocks"); err != nil {
		return nil, err
	}
	bs := m.sortedBlocks(chainID)
	out := make([]db.Block, 0, limit)
	for i := len(bs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, bs[i])
	}
	return out, nil
}

func (m *MemStore) BlocksAfter(_ context.Context, chainID, after uint64, limit int) ([]db.Block, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("BlocksAfter"); err != nil {
		return nil, err
	}
	var out []db.Block
	for _, b := range m.sortedBlocks(chainID) {
		if b.Number > after && len(out) < limit {
			out = append(out, b)
		}
	}
	return out, nil
}

// BlocksMissingPosterGas returns blocks from a number up that carry no poster gas, ascending.
func (m *MemStore) BlocksMissingPosterGas(_ context.Context, chainID, from uint64, limit int) ([]db.Block, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("BlocksMissingPosterGas"); err != nil {
		return nil, err
	}
	var out []db.Block
	for _, b := range m.sortedBlocks(chainID) {
		if b.Number >= from && !b.PosterGas.Valid && len(out) < limit {
			out = append(out, b)
		}
	}
	return out, nil
}

// SetPosterGas records poster gas on stored rows, skipping a number no longer stored, already set, or
// carrying more gas than the row used.
func (m *MemStore) SetPosterGas(_ context.Context, chainID uint64, gas map[uint64]uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("SetPosterGas"); err != nil {
		return err
	}
	for number, g := range gas {
		b, ok := m.BlockRows[chainID][number]
		if !ok || b.PosterGas.Valid || g > b.GasUsed {
			continue
		}
		b.PosterGas = sql.NullInt64{Int64: int64(g), Valid: true}
		m.BlockRows[chainID][number] = b
	}
	return nil
}

func (m *MemStore) BlocksBetween(_ context.Context, chainID uint64, from, to time.Time) ([]db.Block, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("BlocksBetween"); err != nil {
		return nil, err
	}
	var out []db.Block
	for _, b := range m.sortedBlocks(chainID) {
		if !b.TS.Before(from) && b.TS.Before(to) {
			out = append(out, b)
		}
	}
	return out, nil
}

// GasBetween sums total gas and, with complete receipt coverage, compute gas over (from, to].
func (m *MemStore) GasBetween(_ context.Context, chainID uint64, from, to time.Time) (total uint64, compute *uint64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("GasBetween"); err != nil {
		return 0, nil, err
	}
	var computeTotal uint64
	known := true
	for _, b := range m.BlockRows[chainID] {
		if b.TS.After(from) && !b.TS.After(to) {
			total += b.GasUsed
			if !b.PosterGas.Valid || b.PosterGas.Int64 < 0 || uint64(b.PosterGas.Int64) > b.GasUsed {
				known = false
				continue
			}
			computeTotal += b.GasUsed - uint64(b.PosterGas.Int64)
		}
	}
	if !known {
		return total, nil, nil
	}
	return total, &computeTotal, nil
}

func (m *MemStore) TwoTxBlocks(_ context.Context, chainID, after uint64, limit int) ([]uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("TwoTxBlocks"); err != nil {
		return nil, err
	}
	var out []uint64
	for _, b := range m.sortedBlocks(chainID) {
		if b.TxCount == 2 && b.Number > after && len(out) < limit {
			out = append(out, b.Number)
		}
	}
	return out, nil
}

func (m *MemStore) PruneBlocks(_ context.Context, chainID uint64, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("PruneBlocks"); err != nil {
		return 0, err
	}
	var n int64
	for num, b := range m.BlockRows[chainID] {
		if b.TS.Before(before) {
			delete(m.BlockRows[chainID], num)
			n++
		}
	}
	return n, nil
}

func bucketKey(chainID uint64, res string, start time.Time) string {
	return fmt.Sprintf("%d/%s/%d", chainID, res, start.Unix())
}

// FoldBuckets merges buckets exactly like the SQL upsert.
func (m *MemStore) FoldBuckets(_ context.Context, buckets []db.Bucket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("FoldBuckets"); err != nil {
		return err
	}
	for _, b := range buckets {
		key := bucketKey(b.ChainID, b.Resolution, b.BucketStart)
		old, ok := m.BucketRows[key]
		if !ok {
			m.BucketRows[key] = b
			continue
		}
		m.BucketRows[key] = db.MergeBuckets(old, b)
	}
	return nil
}

// setIDAtLocked is the constraint set in force at a block among the sets whose constraint count
// matches the block's backlogs.
func (m *MemStore) setIDAtLocked(chainID, number uint64, backlogs int) sql.NullInt64 {
	var out sql.NullInt64
	var bestBlock uint64
	for _, cs := range m.SetRows {
		if cs.ChainID != chainID || cs.EffectiveBlock > number || setSize(cs) != backlogs {
			continue
		}
		if !out.Valid || cs.EffectiveBlock > bestBlock || (cs.EffectiveBlock == bestBlock && cs.ID > out.Int64) {
			out, bestBlock = sql.NullInt64{Int64: cs.ID, Valid: true}, cs.EffectiveBlock
		}
	}
	return out
}

// pruneFrontierLocked mirrors the Postgres floor for rebuilds: the recorded frontier, or zero when
// none is recorded or it does not parse.
func (m *MemStore) pruneFrontierLocked(chainID uint64) time.Time {
	raw, ok := m.StateRows[stateKey(chainID, db.StatePruneFrontier)]
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func (m *MemStore) RebuildBuckets(_ context.Context, chainID uint64, resolution string, starts []time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("RebuildBuckets"); err != nil {
		return err
	}
	width, ok := db.Resolutions[resolution]
	if !ok {
		return fmt.Errorf("rebuild buckets: unknown resolution %q", resolution)
	}
	// The same floor Postgres applies: a window below the prune frontier has
	// lost rows, so recomputing it would sum only what survived.
	frontier := m.pruneFrontierLocked(chainID)
	for _, start := range starts {
		if !frontier.IsZero() && start.UTC().Before(frontier) {
			continue
		}
		start = start.UTC()
		end := start.Add(width)
		acc := db.NewBucketBuilder(chainID, resolution, start)
		for _, b := range m.sortedBlocks(chainID) {
			if !b.TS.Before(start) && b.TS.Before(end) {
				acc.Add(b, m.setIDAtLocked(chainID, b.Number, len(b.Backlogs)))
			}
		}
		key := bucketKey(chainID, resolution, start)
		if acc.Blocks() == 0 {
			delete(m.BucketRows, key)
			continue
		}
		m.BucketRows[key] = acc.Bucket()
	}
	return nil
}

// setSize is the number of constraints in a set document, -1 when it cannot be read.
func setSize(cs db.ConstraintSet) int {
	var entries []json.RawMessage
	if err := cs.Constraints.Unmarshal(&entries); err != nil {
		return -1
	}
	return len(entries)
}

// BelowFrontier names those of starts below the recorded frontier: the starts RebuildBuckets declines.
func (m *MemStore) BelowFrontier(_ context.Context, chainID uint64, starts []time.Time) ([]time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("BelowFrontier"); err != nil {
		return nil, err
	}
	frontier := m.pruneFrontierLocked(chainID)
	if frontier.IsZero() {
		return nil, nil
	}
	var below []time.Time
	for _, start := range starts {
		if start.UTC().Before(frontier) {
			below = append(below, start.UTC())
		}
	}
	return below, nil
}

// DiscardBucketsBelowFrontier removes the buckets at those of starts below the recorded frontier.
func (m *MemStore) DiscardBucketsBelowFrontier(_ context.Context, chainID uint64, resolution string, starts []time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("DiscardBucketsBelowFrontier"); err != nil {
		return err
	}
	if _, ok := db.Resolutions[resolution]; !ok {
		return fmt.Errorf("discard buckets: unknown resolution %q", resolution)
	}
	frontier := m.pruneFrontierLocked(chainID)
	if frontier.IsZero() {
		return nil
	}
	for _, start := range starts {
		if start.UTC().Before(frontier) {
			delete(m.BucketRows, bucketKey(chainID, resolution, start.UTC()))
		}
	}
	return nil
}

func (m *MemStore) DeleteBucketsBefore(_ context.Context, chainID uint64, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("DeleteBucketsBefore"); err != nil {
		return 0, err
	}
	var n int64
	for k, b := range m.BucketRows {
		if b.ChainID == chainID && b.BucketStart.Before(before) {
			delete(m.BucketRows, k)
			n++
		}
	}
	return n, nil
}

func (m *MemStore) Buckets(_ context.Context, chainID uint64, resolution string, from, to time.Time) ([]db.Bucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("Buckets"); err != nil {
		return nil, err
	}
	var out []db.Bucket
	for _, b := range m.BucketRows {
		if b.ChainID == chainID && b.Resolution == resolution && !b.BucketStart.Before(from) && b.BucketStart.Before(to) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BucketStart.Before(out[j].BucketStart) })
	return out, nil
}

func (m *MemStore) InsertStateSample(_ context.Context, s db.StateSample) error {
	m.hook("InsertStateSample")
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("InsertStateSample"); err != nil {
		return err
	}
	for i, old := range m.SampleRows {
		if old.ChainID == s.ChainID && old.SampledAt.Equal(s.SampledAt) {
			m.SampleRows[i] = s
			return nil
		}
	}
	m.SampleRows = append(m.SampleRows, s)
	return nil
}

func (m *MemStore) LatestStateSample(_ context.Context, chainID uint64, withL1 bool) (*db.StateSample, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("LatestStateSample"); err != nil {
		return nil, err
	}
	var best *db.StateSample
	for i := range m.SampleRows {
		s := &m.SampleRows[i]
		if s.ChainID != chainID || (withL1 && s.L1 == nil) {
			continue
		}
		if best == nil || s.SampledAt.After(best.SampledAt) {
			best = s
		}
	}
	if best == nil {
		return nil, nil
	}
	out := *best
	return &out, nil
}

func (m *MemStore) StateSampleAt(_ context.Context, chainID, block uint64) (*db.StateSample, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("StateSampleAt"); err != nil {
		return nil, err
	}
	var best *db.StateSample
	for i := range m.SampleRows {
		s := &m.SampleRows[i]
		if s.ChainID != chainID || s.BlockNumber > block {
			continue
		}
		if best == nil || s.BlockNumber > best.BlockNumber ||
			(s.BlockNumber == best.BlockNumber && s.SampledAt.After(best.SampledAt)) {
			best = s
		}
	}
	if best == nil {
		return nil, nil
	}
	out := *best
	return &out, nil
}

func (m *MemStore) L1Samples(_ context.Context, chainID uint64, from, to time.Time, step time.Duration) ([]db.StateSample, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("L1Samples"); err != nil {
		return nil, err
	}
	secs := max(int64(step/time.Second), 1)
	var all []db.StateSample
	for _, s := range m.SampleRows {
		if s.ChainID == chainID && s.L1 != nil && !s.SampledAt.Before(from) && s.SampledAt.Before(to) {
			all = append(all, s)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].SampledAt.Before(all[j].SampledAt) })
	var out []db.StateSample
	seen := map[int64]bool{}
	for _, s := range all {
		k := s.SampledAt.Unix() / secs
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *MemStore) PruneStateSamples(_ context.Context, chainID uint64, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("PruneStateSamples"); err != nil {
		return 0, err
	}
	var kept []db.StateSample
	var n int64
	for _, s := range m.SampleRows {
		if s.ChainID == chainID && s.SampledAt.Before(before) {
			n++
			continue
		}
		kept = append(kept, s)
	}
	m.SampleRows = kept
	return n, nil
}

func (m *MemStore) DeleteStateSamplesAfter(_ context.Context, chainID, block uint64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("DeleteStateSamplesAfter"); err != nil {
		return 0, err
	}
	var kept []db.StateSample
	var n int64
	for _, s := range m.SampleRows {
		if s.ChainID == chainID && s.BlockNumber > block {
			n++
			continue
		}
		kept = append(kept, s)
	}
	m.SampleRows = kept
	return n, nil
}

func (m *MemStore) InsertOwnerActions(_ context.Context, actions []db.OwnerAction) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("InsertOwnerActions"); err != nil {
		return 0, err
	}
	n := 0
	for _, a := range actions {
		key := fmt.Sprintf("%d/%s/%d", a.ChainID, a.TxHash, a.LogIndex)
		if existing, ok := m.ActionRows[key]; ok {
			if !existing.TxIndex.Valid && a.TxIndex.Valid {
				existing.TxIndex = a.TxIndex
				m.ActionRows[key] = existing
			}
			continue
		}
		m.ActionRows[key] = a
		n++
	}
	return n, nil
}

func (m *MemStore) OwnerActions(_ context.Context, chainID uint64, from, to time.Time, limit int) ([]db.OwnerAction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("OwnerActions"); err != nil {
		return nil, err
	}
	var out []db.OwnerAction
	for _, a := range m.ActionRows {
		if a.ChainID != chainID {
			continue
		}
		if !from.IsZero() && a.TS.Before(from) {
			continue
		}
		if !to.IsZero() && !a.TS.Before(to) {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BlockNumber != out[j].BlockNumber {
			return out[i].BlockNumber > out[j].BlockNumber
		}
		return out[i].LogIndex > out[j].LogIndex
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemStore) OwnerActionsSince(_ context.Context, chainID, block uint64) ([]db.OwnerAction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("OwnerActionsSince"); err != nil {
		return nil, err
	}
	var out []db.OwnerAction
	for _, a := range m.ActionRows {
		if a.ChainID == chainID && a.BlockNumber >= block {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BlockNumber != out[j].BlockNumber {
			return out[i].BlockNumber < out[j].BlockNumber
		}
		return out[i].LogIndex < out[j].LogIndex
	})
	return out, nil
}

// RewindAfter deletes owner actions, constraint sets and batch reports above a block.
func (m *MemStore) RewindAfter(_ context.Context, chainID, block uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("RewindAfter"); err != nil {
		return err
	}
	for k, a := range m.ActionRows {
		if a.ChainID == chainID && a.BlockNumber > block {
			delete(m.ActionRows, k)
		}
	}
	kept := m.SetRows[:0]
	for _, cs := range m.SetRows {
		if cs.ChainID != chainID || cs.EffectiveBlock <= block {
			kept = append(kept, cs)
		}
	}
	m.SetRows = kept
	for k, r := range m.ReportRows {
		if r.ChainID == chainID && r.BlockNumber > block {
			delete(m.ReportRows, k)
		}
	}
	return nil
}

func (m *MemStore) InsertConstraintSet(_ context.Context, cs db.ConstraintSet) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("InsertConstraintSet"); err != nil {
		return 0, err
	}
	for i, old := range m.SetRows {
		if old.ChainID == cs.ChainID && old.EffectiveBlock == cs.EffectiveBlock && old.Source == cs.Source {
			cs.ID = old.ID
			m.SetRows[i] = cs
			return cs.ID, nil
		}
	}
	m.nextSet++
	cs.ID = m.nextSet
	m.SetRows = append(m.SetRows, cs)
	return cs.ID, nil
}

// UpdateConstraintSet rewrites the set with cs.ID in place.
func (m *MemStore) UpdateConstraintSet(_ context.Context, cs db.ConstraintSet) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("UpdateConstraintSet"); err != nil {
		return err
	}
	for i, old := range m.SetRows {
		if old.ID == cs.ID && old.ChainID == cs.ChainID {
			m.SetRows[i] = cs
			return nil
		}
	}
	return nil
}

func (m *MemStore) ConstraintSets(_ context.Context, chainID uint64) ([]db.ConstraintSet, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("ConstraintSets"); err != nil {
		return nil, err
	}
	var out []db.ConstraintSet
	for _, cs := range m.SetRows {
		if cs.ChainID == chainID {
			out = append(out, cs)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EffectiveBlock != out[j].EffectiveBlock {
			return out[i].EffectiveBlock < out[j].EffectiveBlock
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *MemStore) UpsertBatchReports(_ context.Context, reports []db.BatchReport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("UpsertBatchReports"); err != nil {
		return err
	}
	for _, r := range reports {
		m.ReportRows[fmt.Sprintf("%d/%d", r.ChainID, r.BlockNumber)] = r
	}
	return nil
}

func (m *MemStore) BatchReports(_ context.Context, chainID uint64, from, to time.Time) ([]db.BatchReport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("BatchReports"); err != nil {
		return nil, err
	}
	var out []db.BatchReport
	for _, r := range m.ReportRows {
		if r.CostCalculationVersion == 1 && r.ChainID == chainID && !r.BatchTS.Before(from) && r.BatchTS.Before(to) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].BatchTS.Equal(out[j].BatchTS) {
			return out[i].BatchTS.Before(out[j].BatchTS)
		}
		return out[i].BlockNumber < out[j].BlockNumber
	})
	return out, nil
}

func (m *MemStore) BatchBuckets(_ context.Context, chainID uint64, from, to time.Time, step time.Duration) ([]db.BatchBucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("BatchBuckets"); err != nil {
		return nil, err
	}
	secs := max(int64(step/time.Second), 1)
	agg := map[int64]*db.BatchBucket{}
	sums := map[int64]*big.Int{}
	for _, r := range m.ReportRows {
		if r.CostCalculationVersion != 1 || r.ChainID != chainID || r.BatchTS.Before(from) || !r.BatchTS.Before(to) {
			continue
		}
		k := (r.BatchTS.Unix() / secs) * secs
		b := agg[k]
		if b == nil {
			b = &db.BatchBucket{T: time.Unix(k, 0).UTC(), WeiSpent: db.NewWei(nil)}
			agg[k] = b
			sums[k] = new(big.Int)
		}
		b.Batches++
		b.GasSpent += r.GasSpent
		b.WeiSpent = db.NewWei(new(big.Int).Add(b.WeiSpent.BigInt(), r.WeiSpent.BigInt()))
		b.CalldataBytes += r.CalldataLen
		sums[k].Add(sums[k], r.L1BaseFee.BigInt())
	}
	out := make([]db.BatchBucket, 0, len(agg))
	for k, b := range agg {
		b.L1BaseFeeAvg = db.NewWei(new(big.Int).Div(sums[k], big.NewInt(b.Batches)))
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out, nil
}

func stateKey(chainID uint64, key string) string { return fmt.Sprintf("%d/%s", chainID, key) }

func (m *MemStore) GetState(_ context.Context, chainID uint64, key string) (value string, found bool, err error) {
	m.hook("GetState")
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("GetState"); err != nil {
		return "", false, err
	}
	value, found = m.StateRows[stateKey(chainID, key)]
	return value, found, nil
}

func (m *MemStore) SetState(_ context.Context, chainID uint64, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("SetState"); err != nil {
		return err
	}
	m.StateRows[stateKey(chainID, key)] = value
	return nil
}

func (m *MemStore) DeleteState(_ context.Context, chainID uint64, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("DeleteState"); err != nil {
		return err
	}
	delete(m.StateRows, stateKey(chainID, key))
	return nil
}

func (m *MemStore) States(_ context.Context, chainID uint64) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("States"); err != nil {
		return nil, err
	}
	prefix := stateKey(chainID, "")
	out := map[string]string{}
	for k, v := range m.StateRows {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out[k[len(prefix):]] = v
		}
	}
	return out, nil
}

func (m *MemStore) Notify(_ context.Context, channel, payload string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("Notify"); err != nil {
		return err
	}
	m.Notifications = append(m.Notifications, db.Notification{Channel: channel, Payload: payload})
	return nil
}

func (m *MemStore) LastNotification(channel string) (n db.Notification, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.Notifications) - 1; i >= 0; i-- {
		if m.Notifications[i].Channel == channel {
			return m.Notifications[i], true
		}
	}
	return db.Notification{}, false
}

func (m *MemStore) BucketCount(chainID uint64, resolution string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, b := range m.BucketRows {
		if b.ChainID == chainID && b.Resolution == resolution {
			n++
		}
	}
	return n
}

var _ db.Store = (*MemStore)(nil)
