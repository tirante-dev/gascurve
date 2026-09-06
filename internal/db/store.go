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

// Block is a row of the blocks table.
type Block struct {
	ChainID          uint64        `db:"chain_id"`
	Number           uint64        `db:"number"`
	TS               time.Time     `db:"ts"`
	GasUsed          uint64        `db:"gas_used"`
	BaseFee          Wei           `db:"base_fee"`
	L1Block          uint64        `db:"l1_block"`
	TxCount          int           `db:"tx_count"`
	Backlogs         pq.Int64Array `db:"backlogs"`
	ExponentBips     int64         `db:"exponent_bips"`
	PredictedBaseFee Wei           `db:"predicted_base_fee"`
	Anchored         bool          `db:"anchored"`
}

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
// the stored row: counters add, min/max combine, the average is weighted by
// block count, and the *_end fields are replaced.
type Bucket struct {
	ChainID         uint64        `db:"chain_id"`
	Resolution      string        `db:"resolution"`
	BucketStart     time.Time     `db:"bucket_start"`
	Blocks          int64         `db:"blocks"`
	GasUsed         uint64        `db:"gas_used"`
	FeesWei         Wei           `db:"fees_wei"`
	BaseFeeMin      Wei           `db:"base_fee_min"`
	BaseFeeAvg      Wei           `db:"base_fee_avg"`
	BaseFeeMax      Wei           `db:"base_fee_max"`
	ExponentEndBips int64         `db:"exponent_end_bips"`
	BacklogsEnd     pq.Int64Array `db:"backlogs_end"`
	BacklogsMax     pq.Int64Array `db:"backlogs_max"`
	ConstraintSetID sql.NullInt64 `db:"constraint_set_id"`
	ReplayErrorBips int64         `db:"replay_error_bips"`
	// LastBlock is the highest block folded into the bucket; *_end fields
	// and the constraint set are only replaced by folds with a higher one.
	LastBlock uint64 `db:"last_block"`
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

	UpsertNetwork(ctx context.Context, n Network) error
	Networks(ctx context.Context) ([]Network, error)
	// NetworkByRef resolves a name or a decimal chain id. Returns nil, nil
	// when unknown.
	NetworkByRef(ctx context.Context, ref string) (*Network, error)
	UpdateNetworkHead(ctx context.Context, chainID, headBlock uint64, headAt, sampledAt time.Time) error
	// SetNetworkError records the last collector error; empty clears it.
	SetNetworkError(ctx context.Context, chainID uint64, msg string) error

	UpsertBlocks(ctx context.Context, blocks []Block) error
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

	FoldBuckets(ctx context.Context, buckets []Bucket) error
	// Buckets returns buckets with from <= bucket_start < to, ascending.
	Buckets(ctx context.Context, chainID uint64, resolution string, from, to time.Time) ([]Bucket, error)

	InsertStateSample(ctx context.Context, s StateSample) error
	// LatestStateSample returns the newest sample, optionally only among
	// those carrying L1 data. Nil when none.
	LatestStateSample(ctx context.Context, chainID uint64, withL1 bool) (*StateSample, error)
	// L1Samples returns one L1-carrying sample per step over [from, to).
	L1Samples(ctx context.Context, chainID uint64, from, to time.Time, step time.Duration) ([]StateSample, error)
	PruneStateSamples(ctx context.Context, chainID uint64, before time.Time) (int64, error)

	// InsertOwnerActions inserts new actions, ignoring duplicates, and
	// returns how many were new.
	InsertOwnerActions(ctx context.Context, actions []OwnerAction) (int, error)
	// OwnerActions lists actions newest first; zero from/to means unbounded.
	OwnerActions(ctx context.Context, chainID uint64, from, to time.Time, limit int) ([]OwnerAction, error)

	// InsertConstraintSet upserts on (chain, block, source) and returns the id.
	InsertConstraintSet(ctx context.Context, cs ConstraintSet) (int64, error)
	// ConstraintSets lists sets ascending by effective block.
	ConstraintSets(ctx context.Context, chainID uint64) ([]ConstraintSet, error)

	UpsertBatchReports(ctx context.Context, reports []BatchReport) error
	// BatchBuckets aggregates reports by batch_ts over [from, to) in steps.
	BatchBuckets(ctx context.Context, chainID uint64, from, to time.Time, step time.Duration) ([]BatchBucket, error)

	GetState(ctx context.Context, chainID uint64, key string) (string, bool, error)
	SetState(ctx context.Context, chainID uint64, key, value string) error
	States(ctx context.Context, chainID uint64) (map[string]string, error)

	Notify(ctx context.Context, channel, payload string) error
}
