package db

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"
)

// Network is a row of the networks table.
type Network struct {
	ChainID      uint64         `db:"chain_id"`
	Name         string         `db:"name"`
	DisplayName  string         `db:"display_name"`
	ExplorerURL  string         `db:"explorer_url"`
	Enabled      bool           `db:"enabled"`
	HeadBlock    sql.NullInt64  `db:"head_block"`
	HeadAt       sql.NullTime   `db:"head_at"`
	LastSampleAt sql.NullTime   `db:"last_sample_at"`
	LastError    sql.NullString `db:"last_error"`
	UpdatedAt    time.Time      `db:"updated_at"`
}

// PricingVersion values of a blocks or buckets row.
const (
	// PricingUnknown marks history written before the pricing breakdown
	// existed: no per-constraint exponents, no floor in force, so no exact
	// fee split can be derived from it.
	PricingUnknown int16 = 0
	// PricingFull marks a row written with the whole breakdown.
	PricingFull int16 = 1
)

// Block is a row of the blocks table. Backlogs are the end-of-block
// values, ConstraintBips the start-of-block per-constraint exponents (nil
// for rows written before they were recorded, an empty array for a legacy
// block) and MinBaseFee the floor in force at the block, unknown (NULL)
// for that same history. PricingVersion says which: PricingUnknown for
// history without a breakdown, PricingFull for rows written with one.
type Block struct {
	ChainID          uint64        `db:"chain_id"`
	Number           uint64        `db:"number"`
	Hash             string        `db:"hash"`
	ParentHash       string        `db:"parent_hash"`
	TS               time.Time     `db:"ts"`
	GasUsed          uint64        `db:"gas_used"`
	BaseFee          Wei           `db:"base_fee"`
	L1Block          uint64        `db:"l1_block"`
	TxCount          int           `db:"tx_count"`
	Backlogs         Uint64Array   `db:"backlogs"`
	ConstraintBips   pq.Int64Array `db:"constraint_bips"`
	ExponentBips     int64         `db:"exponent_bips"`
	PredictedBaseFee Wei           `db:"predicted_base_fee"`
	MinBaseFee       NullWei       `db:"min_base_fee"`
	Anchored         bool          `db:"anchored"`
	PricingVersion   int16         `db:"pricing_version"`
}

// Known reports whether the block carries the full pricing breakdown, so
// its floor and fee split are exact rather than unknown history.
func (b Block) Known() bool { return b.PricingVersion >= PricingFull && b.MinBaseFee.Valid }

// Bucket resolutions.
const (
	Resolution1m  = "1m"
	Resolution15m = "15m"
	Resolution1h  = "1h"
)

// Resolutions lists the stored bucket resolutions and their widths.
var Resolutions = map[string]time.Duration{
	Resolution1m:  time.Minute,
	Resolution15m: 15 * time.Minute,
	Resolution1h:  time.Hour,
}

// Bucket is a row of the buckets table. FoldBuckets merges a Bucket into
// the stored row: counters and sums add, min/max combine, the average is
// derived from the exact sum, and the *_end fields are replaced.
// RebuildBuckets instead recomputes a row from the block rows in its
// window. BaseFeeSum, ConstraintBipsEnd, MinBaseFee, FloorFeesWei and
// SurplusFeesWei are unknown (NULL, nil) for rows written before they were
// recorded and for any window holding one such block; a fold into such a
// row keeps them unknown.
type Bucket struct {
	ChainID           uint64        `db:"chain_id"`
	Resolution        string        `db:"resolution"`
	BucketStart       time.Time     `db:"bucket_start"`
	Blocks            int64         `db:"blocks"`
	GasUsed           uint64        `db:"gas_used"`
	FeesWei           Wei           `db:"fees_wei"`
	BaseFeeMin        Wei           `db:"base_fee_min"`
	BaseFeeAvg        Wei           `db:"base_fee_avg"`
	BaseFeeMax        Wei           `db:"base_fee_max"`
	BaseFeeSum        NullWei       `db:"base_fee_sum"`
	ExponentEndBips   int64         `db:"exponent_end_bips"`
	BacklogsEnd       Uint64Array   `db:"backlogs_end"`
	BacklogsMax       Uint64Array   `db:"backlogs_max"`
	ConstraintBipsEnd pq.Int64Array `db:"constraint_bips_end"`
	MinBaseFee        NullWei       `db:"min_base_fee"`
	FloorFeesWei      NullWei       `db:"floor_fees_wei"`
	SurplusFeesWei    NullWei       `db:"surplus_fees_wei"`
	ConstraintSetID   sql.NullInt64 `db:"constraint_set_id"`
	ReplayErrorBips   int64         `db:"replay_error_bips"`
	// LastBlock is the highest block folded into the bucket; *_end fields
	// and the constraint set are only replaced by folds with a higher one.
	LastBlock uint64 `db:"last_block"`
	// PricingVersion is the lowest version of the blocks folded in:
	// PricingUnknown as soon as one of them lacks the breakdown, which is
	// what makes the fee split unknown for the whole bucket.
	PricingVersion int16 `db:"pricing_version"`
}

// StateSample is a row of the state_samples table.
type StateSample struct {
	ChainID     uint64    `db:"chain_id"`
	SampledAt   time.Time `db:"sampled_at"`
	BlockNumber uint64    `db:"block_number"`
	BaseFee     Wei       `db:"base_fee"`
	MinBaseFee  Wei       `db:"min_base_fee"`
	Constraints JSONB     `db:"constraints"`
	Legacy      JSONB     `db:"legacy"`
	Prices      JSONB     `db:"prices"`
	L1          JSONB     `db:"l1"`
	Accounts    JSONB     `db:"accounts"`
}

// OwnerAction is a row of the owner_actions table.
type OwnerAction struct {
	ChainID     uint64    `db:"chain_id"`
	BlockNumber uint64    `db:"block_number"`
	TxHash      string    `db:"tx_hash"`
	LogIndex    int64     `db:"log_index"`
	TS          time.Time `db:"ts"`
	Method      string    `db:"method"`
	Selector    string    `db:"selector"`
	Args        JSONB     `db:"args"`
}

// ConstraintSet is a row of the constraint_sets table.
type ConstraintSet struct {
	ID             int64     `db:"id"`
	ChainID        uint64    `db:"chain_id"`
	EffectiveBlock uint64    `db:"effective_block"`
	EffectiveAt    time.Time `db:"effective_at"`
	Constraints    JSONB     `db:"constraints"`
	Source         string    `db:"source"`
}

// BatchReport is a row of the batch_reports table.
type BatchReport struct {
	ChainID         uint64    `db:"chain_id"`
	BlockNumber     uint64    `db:"block_number"`
	BatchNumber     uint64    `db:"batch_number"`
	BatchTS         time.Time `db:"batch_ts"`
	Poster          string    `db:"poster"`
	CalldataLen     uint64    `db:"calldata_len"`
	CalldataNonzero uint64    `db:"calldata_nonzero"`
	ExtraGas        uint64    `db:"extra_gas"`
	L1BaseFee       Wei       `db:"l1_base_fee"`
	GasSpent        uint64    `db:"gas_spent"`
	WeiSpent        Wei       `db:"wei_spent"`
}

// BatchBucket is an aggregate of batch reports over a time step.
type BatchBucket struct {
	T             time.Time `db:"t"`
	Batches       int64     `db:"batches"`
	GasSpent      uint64    `db:"gas_spent"`
	WeiSpent      Wei       `db:"wei_spent"`
	L1BaseFeeAvg  Wei       `db:"l1_base_fee_avg"`
	CalldataBytes uint64    `db:"calldata_bytes"`
}

// Store is everything the collector and the API need from Postgres. Both
// are unit-tested against fakes of this interface.
type Store interface {
	Ping(ctx context.Context) error
	// WithTx runs fn inside a transaction; the Store passed to fn is bound
	// to that transaction.
	WithTx(ctx context.Context, fn func(Store) error) error
	// WithChainTx is WithTx holding the chain's transaction-scoped advisory
	// lock (pg_advisory_xact_lock(chain_id)) from the start, so the live
	// tick, the slow loop, the backfill and a reorg rewind never interleave
	// their writes for one chain and no rebuild sees another writer's
	// uncommitted rows. Nested calls reuse the outer transaction.
	WithChainTx(ctx context.Context, chainID uint64, fn func(Store) error) error
	// WithSnapshotTx runs fn in a read-only repeatable-read transaction, so
	// every read inside it sees one database moment.
	WithSnapshotTx(ctx context.Context, fn func(Store) error) error

	UpsertNetwork(ctx context.Context, n Network) error
	Networks(ctx context.Context) ([]Network, error)
	// NetworkByRef resolves a name or a decimal chain id. Returns nil, nil
	// when unknown.
	NetworkByRef(ctx context.Context, ref string) (*Network, error)
	UpdateNetworkHead(ctx context.Context, chainID, headBlock uint64, headAt, sampledAt time.Time) error
	// SetNetworkHead records the head after a reorg rewind, where every
	// field may be unknown: a nil headBlock nulls head_block and head_at
	// (no block survived) and a nil sampledAt nulls last_sample_at (no
	// state sample survived). Unlike UpdateNetworkHead it does not clear
	// last_error, since a rewind is not a successful sample.
	SetNetworkHead(ctx context.Context, chainID uint64, headBlock *uint64, headAt, sampledAt *time.Time) error
	// SetNetworkError records the last collector error; empty clears it.
	SetNetworkError(ctx context.Context, chainID uint64, msg string) error

	UpsertBlocks(ctx context.Context, blocks []Block) error
	// BlockByNumber returns one block or nil.
	BlockByNumber(ctx context.Context, chainID, number uint64) (*Block, error)
	// LatestBlock returns the highest stored block or nil.
	LatestBlock(ctx context.Context, chainID uint64) (*Block, error)
	// OldestBlock returns the lowest stored block or nil.
	OldestBlock(ctx context.Context, chainID uint64) (*Block, error)
	// RecentBlocks returns up to limit blocks, newest first.
	RecentBlocks(ctx context.Context, chainID uint64, limit int) ([]Block, error)
	// BlocksAfter returns blocks with number > after, ascending, up to limit.
	BlocksAfter(ctx context.Context, chainID, after uint64, limit int) ([]Block, error)
	// BlocksBetween returns blocks with from <= ts < to, ascending.
	BlocksBetween(ctx context.Context, chainID uint64, from, to time.Time) ([]Block, error)
	// GasUsedBetween sums gas_used over from < ts <= to.
	GasUsedBetween(ctx context.Context, chainID uint64, from, to time.Time) (uint64, error)
	// TwoTxBlocks lists block numbers > after with exactly two transactions.
	TwoTxBlocks(ctx context.Context, chainID, after uint64, limit int) ([]uint64, error)
	PruneBlocks(ctx context.Context, chainID uint64, before time.Time) (int64, error)
	// DeleteBlocksAfter removes blocks with number > after (a reorg rewind)
	// and returns the removed rows, ascending.
	DeleteBlocksAfter(ctx context.Context, chainID, after uint64) ([]Block, error)

	// FoldBuckets adds partial buckets into the stored rows.
	FoldBuckets(ctx context.Context, buckets []Bucket) error
	// RebuildBuckets recomputes the buckets starting at starts from the
	// block rows inside their windows; a window without rows loses its row.
	RebuildBuckets(ctx context.Context, chainID uint64, resolution string, starts []time.Time) error
	// DeleteBucketsBefore drops every resolution's buckets starting before t.
	DeleteBucketsBefore(ctx context.Context, chainID uint64, before time.Time) (int64, error)
	// Buckets returns buckets with from <= bucket_start < to, ascending.
	Buckets(ctx context.Context, chainID uint64, resolution string, from, to time.Time) ([]Bucket, error)

	InsertStateSample(ctx context.Context, s StateSample) error
	// LatestStateSample returns the newest sample, optionally only among
	// those carrying L1 data. Nil when none.
	LatestStateSample(ctx context.Context, chainID uint64, withL1 bool) (*StateSample, error)
	// StateSampleAt returns the newest sample taken at or below a block,
	// nil when none. Historical replay needs the pricer parameters that
	// were really in force there, which the sample carries, never the
	// live ones.
	StateSampleAt(ctx context.Context, chainID, block uint64) (*StateSample, error)
	// L1Samples returns one L1-carrying sample per step over [from, to).
	L1Samples(ctx context.Context, chainID uint64, from, to time.Time, step time.Duration) ([]StateSample, error)
	PruneStateSamples(ctx context.Context, chainID uint64, before time.Time) (int64, error)
	// DeleteStateSamplesAfter removes samples taken at blocks above block
	// (a reorg rewind) and returns how many.
	DeleteStateSamplesAfter(ctx context.Context, chainID, block uint64) (int64, error)

	// InsertOwnerActions inserts new actions, ignoring duplicates, and
	// returns how many were new.
	InsertOwnerActions(ctx context.Context, actions []OwnerAction) (int, error)
	// OwnerActions lists actions newest first; zero from/to means unbounded.
	OwnerActions(ctx context.Context, chainID uint64, from, to time.Time, limit int) ([]OwnerAction, error)
	// OwnerActionsSince lists actions with block_number >= block, ascending
	// by block and log index.
	OwnerActionsSince(ctx context.Context, chainID, block uint64) ([]OwnerAction, error)
	// RewindAfter deletes owner actions, constraint sets and batch reports
	// above block (a reorg rewind).
	RewindAfter(ctx context.Context, chainID, block uint64) error

	// InsertConstraintSet upserts on (chain, block, source) and returns the id.
	InsertConstraintSet(ctx context.Context, cs ConstraintSet) (int64, error)
	// UpdateConstraintSet rewrites the effective block, time, constraints
	// and source of the set with cs.ID, keeping the id.
	UpdateConstraintSet(ctx context.Context, cs ConstraintSet) error
	// ConstraintSets lists sets ascending by effective block.
	ConstraintSets(ctx context.Context, chainID uint64) ([]ConstraintSet, error)

	UpsertBatchReports(ctx context.Context, reports []BatchReport) error
	// BatchReports lists reports with from <= batch_ts < to ordered by
	// batch_ts then block_number (one point per report, never grouped).
	BatchReports(ctx context.Context, chainID uint64, from, to time.Time) ([]BatchReport, error)
	// BatchBuckets aggregates reports by batch_ts over [from, to) in steps.
	BatchBuckets(ctx context.Context, chainID uint64, from, to time.Time, step time.Duration) ([]BatchBucket, error)

	GetState(ctx context.Context, chainID uint64, key string) (string, bool, error)
	SetState(ctx context.Context, chainID uint64, key, value string) error
	// DeleteState removes a checkpoint; a missing key is not an error.
	DeleteState(ctx context.Context, chainID uint64, key string) error
	States(ctx context.Context, chainID uint64) (map[string]string, error)

	Notify(ctx context.Context, channel, payload string) error
}
