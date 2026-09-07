package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// Postgres implements Store on sqlx.
type Postgres struct {
	db    *sqlx.DB
	q     sqlx.ExtContext
	stats *stats
	// tx describes the open transaction this store is bound to, nil when it
	// is the pool. Nested calls read it to tell whether they can reuse the
	// transaction, must add a lock to it, or are incompatible with it.
	tx *txMode
}

// Stats is cumulative database operation accounting for health and metrics.
// Latency measures driver calls, including connection pool waits.
type Stats struct {
	Operations   uint64
	Errors       uint64
	TotalLatency time.Duration
	LastLatency  time.Duration
}

type stats struct {
	operations   atomic.Uint64
	errors       atomic.Uint64
	totalLatency atomic.Int64
	lastLatency  atomic.Int64
}

// txMode is the metadata of an open transaction: the chain locks it holds,
// in acquisition order, and whether it is the read-only repeatable-read
// snapshot.
type txMode struct {
	locks    []uint64
	snapshot bool
}

// Nesting errors. A caller that believes it holds chain exclusion, or a
// single database moment, and does not is a correctness problem, so the
// combination is refused rather than silently downgraded.
var (
	// ErrNestedSnapshot is returned when a read-only repeatable-read
	// transaction is requested inside a writing one: it would see that
	// transaction's own uncommitted writes and no consistent snapshot.
	ErrNestedSnapshot = errors.New("db: snapshot transaction inside a write transaction")
	// ErrSnapshotWrite is returned when a chain transaction is requested
	// inside a read-only snapshot transaction, which cannot write.
	ErrSnapshotWrite = errors.New("db: chain transaction inside a snapshot transaction")
	// ErrLockOrder is returned when a chain lock is requested inside a
	// transaction that already holds a higher one. Locks are always taken
	// in ascending chain id order, so taking this one now could deadlock
	// against a transaction doing the reverse.
	ErrLockOrder = errors.New("db: chain lock out of order")
)

// NewPostgres wraps an open connection pool.
func NewPostgres(d *sqlx.DB) *Postgres {
	return &Postgres{db: d, q: d, stats: &stats{}}
}

// DB exposes the underlying pool (for migrations and shutdown).
func (p *Postgres) DB() *sqlx.DB { return p.db }

// Stats returns a concurrency-safe snapshot of database operation accounting.
func (p *Postgres) Stats() Stats {
	return Stats{
		Operations: p.stats.operations.Load(), Errors: p.stats.errors.Load(),
		TotalLatency: time.Duration(p.stats.totalLatency.Load()), LastLatency: time.Duration(p.stats.lastLatency.Load()),
	}
}

func (p *Postgres) observe(start time.Time, err error) {
	d := time.Since(start)
	p.stats.operations.Add(1)
	p.stats.totalLatency.Add(int64(d))
	p.stats.lastLatency.Store(int64(d))
	if err != nil {
		p.stats.errors.Add(1)
	}
}

// Ping checks connectivity.
func (p *Postgres) Ping(ctx context.Context) error {
	start := time.Now()
	err := p.db.PingContext(ctx)
	p.observe(start, err)
	return err
}

// WithTx runs fn in a transaction. Nested inside any open transaction it
// reuses it: a plain transaction asks for nothing the outer one does not
// already give.
func (p *Postgres) WithTx(ctx context.Context, fn func(Store) error) error {
	if p.tx != nil {
		return fn(p)
	}
	return p.transact(ctx, nil, &txMode{}, nil, fn)
}

// WithChainTx runs fn in a transaction that holds the chain's advisory
// lock (pg_advisory_xact_lock, released at commit or rollback) before any
// other statement. Nested inside a transaction that already holds that
// chain's lock it reuses it; inside one holding only lower chain ids it
// takes the missing lock, keeping the ascending order that makes the
// locks deadlock free; inside one holding a higher chain id, or inside a
// read-only snapshot, it fails rather than run without the exclusion the
// caller believes it has.
func (p *Postgres) WithChainTx(ctx context.Context, chainID uint64, fn func(Store) error) error {
	if p.tx == nil {
		return p.transact(ctx, nil, &txMode{locks: []uint64{chainID}}, func(s *Postgres) error {
			return s.lockChain(ctx, chainID)
		}, fn)
	}
	if p.tx.snapshot {
		return fmt.Errorf("%w: chain %d", ErrSnapshotWrite, chainID)
	}
	for _, held := range p.tx.locks {
		if held == chainID {
			return fn(p)
		}
	}
	if len(p.tx.locks) > 0 {
		if highest := slices.Max(p.tx.locks); chainID < highest {
			return fmt.Errorf("%w: chain %d after %d", ErrLockOrder, chainID, highest)
		}
	}
	if err := p.lockChain(ctx, chainID); err != nil {
		return err
	}
	p.tx.locks = append(p.tx.locks, chainID)
	return fn(p)
}

// lockChain takes the chain's transaction-scoped advisory lock.
func (p *Postgres) lockChain(ctx context.Context, chainID uint64) error {
	if _, err := p.exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(chainID)); err != nil {
		return fmt.Errorf("chain lock: %w", err)
	}
	return nil
}

// WithSnapshotTx runs fn in a read-only repeatable-read transaction. Inside
// another snapshot it reuses it; inside a writing transaction it fails,
// since it would read that transaction's own uncommitted rows instead of
// one committed database moment.
func (p *Postgres) WithSnapshotTx(ctx context.Context, fn func(Store) error) error {
	if p.tx != nil {
		if !p.tx.snapshot {
			return ErrNestedSnapshot
		}
		return fn(p)
	}
	return p.transact(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}, &txMode{snapshot: true}, nil, fn)
}

// transact begins a transaction with opts, records mode on the bound
// store so nested calls can check compatibility, runs setup and then fn on
// it, and commits unless either failed.
func (p *Postgres) transact(ctx context.Context, opts *sql.TxOptions, mode *txMode, setup func(*Postgres) error, fn func(Store) error) error {
	start := time.Now()
	tx, err := p.db.BeginTxx(ctx, opts)
	p.observe(start, err)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	bound := &Postgres{db: p.db, q: tx, stats: p.stats, tx: mode}
	if setup != nil {
		err = setup(bound)
	}
	if err == nil {
		err = fn(bound)
	}
	if err != nil {
		start = time.Now()
		rollbackErr := tx.Rollback()
		p.observe(start, rollbackErr)
		return err
	}
	start = time.Now()
	err = tx.Commit()
	p.observe(start, err)
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (p *Postgres) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	start := time.Now()
	res, err := p.q.ExecContext(ctx, query, args...)
	p.observe(start, err)
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	return res, nil
}

func selectAll[T any](ctx context.Context, p *Postgres, query string, args ...any) ([]T, error) {
	var out []T
	start := time.Now()
	err := sqlx.SelectContext(ctx, p.q, &out, query, args...)
	p.observe(start, err)
	if err != nil {
		return nil, fmt.Errorf("select: %w", err)
	}
	return out, nil
}

func getOne[T any](ctx context.Context, p *Postgres, query string, args ...any) (*T, error) {
	var out T
	start := time.Now()
	err := sqlx.GetContext(ctx, p.q, &out, query, args...)
	observed := err
	if errors.Is(err, sql.ErrNoRows) {
		observed = nil
	}
	p.observe(start, observed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get: %w", err)
	}
	return &out, nil
}

// UpsertNetwork inserts or updates the static network fields.
func (p *Postgres) UpsertNetwork(ctx context.Context, n Network) error {
	_, err := p.exec(ctx, `
		INSERT INTO networks (chain_id, name, display_name, explorer_url, enabled, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (chain_id) DO UPDATE SET
			name = EXCLUDED.name, display_name = EXCLUDED.display_name,
			explorer_url = EXCLUDED.explorer_url, enabled = EXCLUDED.enabled, updated_at = now()`,
		n.ChainID, n.Name, n.DisplayName, n.ExplorerURL, n.Enabled)
	return err
}

const networkColumns = `chain_id, name, display_name, explorer_url, enabled, head_block, head_at, last_sample_at, last_error, updated_at`

// Networks lists all networks ordered by chain id.
func (p *Postgres) Networks(ctx context.Context) ([]Network, error) {
	return selectAll[Network](ctx, p, `SELECT `+networkColumns+` FROM networks ORDER BY chain_id`)
}

// NetworkByRef resolves a reference deterministically: a decimal within
// the BIGINT range is a chain id and nothing else, anything else is a name.
func (p *Postgres) NetworkByRef(ctx context.Context, ref string) (*Network, error) {
	if id, ok := ChainIDRef(ref); ok {
		return getOne[Network](ctx, p, `SELECT `+networkColumns+` FROM networks WHERE chain_id = $1`, id)
	}
	return getOne[Network](ctx, p, `SELECT `+networkColumns+` FROM networks WHERE name = $1`, ref)
}

// ChainIDRef reports whether a network reference is a chain id: a decimal
// number that fits a BIGINT. Larger numbers and names are looked up by
// name only.
func ChainIDRef(ref string) (int64, bool) {
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id < 0 || strconv.FormatInt(id, 10) != ref {
		return 0, false
	}
	return id, true
}

// UpdateNetworkHead records the newest block and sample time.
func (p *Postgres) UpdateNetworkHead(ctx context.Context, chainID, headBlock uint64, headAt, sampledAt time.Time) error {
	_, err := p.exec(ctx, `UPDATE networks SET head_block = $2, head_at = $3, last_sample_at = $4, last_error = NULL, updated_at = now() WHERE chain_id = $1`,
		chainID, headBlock, headAt, sampledAt)
	return err
}

// SetNetworkHead records the head after a rewind, nulling the fields that
// no longer have a value. last_error is left alone: a rewind is not a
// successful sample and must not clear a standing error.
func (p *Postgres) SetNetworkHead(ctx context.Context, chainID uint64, headBlock *uint64, headAt, sampledAt *time.Time) error {
	var block sql.NullInt64
	if headBlock != nil {
		block = sql.NullInt64{Int64: int64(*headBlock), Valid: true}
	}
	var at, sampled sql.NullTime
	if headAt != nil {
		at = sql.NullTime{Time: *headAt, Valid: true}
	}
	if sampledAt != nil {
		sampled = sql.NullTime{Time: *sampledAt, Valid: true}
	}
	_, err := p.exec(ctx, `UPDATE networks SET head_block = $2, head_at = $3, last_sample_at = $4, updated_at = now() WHERE chain_id = $1`,
		chainID, block, at, sampled)
	return err
}

// SetNetworkError records or clears the last error.
func (p *Postgres) SetNetworkError(ctx context.Context, chainID uint64, msg string) error {
	var v sql.NullString
	if msg != "" {
		v = sql.NullString{String: msg, Valid: true}
	}
	_, err := p.exec(ctx, `UPDATE networks SET last_error = $2, updated_at = now() WHERE chain_id = $1`, chainID, v)
	return err
}

const blockColumns = `chain_id, number, hash, parent_hash, ts, gas_used, poster_gas, base_fee, l1_block, tx_count, backlogs, constraint_bips, exponent_bips, predicted_base_fee, min_base_fee, anchored, pricing_version`

// PostgreSQL accepts at most 65,535 bind parameters. Keeping a little room
// below that limit also avoids sending unnecessarily large statements if a
// caller supplies more rows than the collector's configured batch size.
const postgresBatchParameters = 60_000

// valueTuples builds ($1, ...), ($n, ...) for a multi-row INSERT. Values stay
// in bind parameters, so batching does not interpolate any row data into SQL.
func valueTuples(rows, columns int) string {
	var b strings.Builder
	for row := range rows {
		if row > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for column := range columns {
			if column > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(row*columns + column + 1))
		}
		b.WriteByte(')')
	}
	return b.String()
}

// uniqueLayers partitions rows so a single INSERT never contains the same
// conflict key twice. PostgreSQL rejects an INSERT whose ON CONFLICT clause
// would update one target row twice. Later occurrences go in later statements,
// retaining the old per-row conflict order for duplicate input keys.
func uniqueLayers[T any, K comparable](rows []T, key func(T) K) [][]T {
	counts := make(map[K]int, len(rows))
	var layers [][]T
	for _, row := range rows {
		k := key(row)
		layer := counts[k]
		if layer == len(layers) {
			layers = append(layers, nil)
		}
		layers[layer] = append(layers[layer], row)
		counts[k]++
	}
	return layers
}

// blockSpan is the lowest and highest block number in a batch. A batched
// statement fails as a whole, so its error names the rows it carried
// rather than only the statement that carried them.
func blockSpan(batch []Block) (lo, hi uint64) {
	lo, hi = batch[0].Number, batch[0].Number
	for _, b := range batch[1:] {
		lo, hi = min(lo, b.Number), max(hi, b.Number)
	}
	return lo, hi
}

// bucketSpan is the earliest and latest bucket start in a batch, for the
// same reason as blockSpan. One batch may hold several resolutions, so
// only the window they cover is named.
func bucketSpan(batch []Bucket) (first, last time.Time) {
	first, last = batch[0].BucketStart, batch[0].BucketStart
	for _, b := range batch[1:] {
		if b.BucketStart.Before(first) {
			first = b.BucketStart
		}
		if b.BucketStart.After(last) {
			last = b.BucketStart
		}
	}
	return first, last
}

// UpsertBlocks writes blocks, replacing replay fields on conflict.
func (p *Postgres) UpsertBlocks(ctx context.Context, blocks []Block) error {
	const columns = 17
	for _, layer := range uniqueLayers(blocks, func(b Block) struct{ chainID, number uint64 } {
		return struct{ chainID, number uint64 }{b.ChainID, b.Number}
	}) {
		for from := 0; from < len(layer); from += postgresBatchParameters / columns {
			batch := layer[from:min(from+postgresBatchParameters/columns, len(layer))]
			args := make([]any, 0, len(batch)*columns)
			for _, b := range batch {
				args = append(args, b.ChainID, b.Number, b.Hash, b.ParentHash, b.TS, b.GasUsed, b.PosterGas, b.BaseFee, b.L1Block, b.TxCount, b.Backlogs, b.ConstraintBips,
					b.ExponentBips, b.PredictedBaseFee, b.MinBaseFee, b.Anchored, b.PricingVersion)
			}
			if _, err := p.exec(ctx, `
			INSERT INTO blocks (`+blockColumns+`)
			VALUES `+valueTuples(len(batch), columns)+`
			ON CONFLICT (chain_id, number) DO UPDATE SET
				hash = EXCLUDED.hash, parent_hash = EXCLUDED.parent_hash,
				ts = EXCLUDED.ts, gas_used = EXCLUDED.gas_used, poster_gas = EXCLUDED.poster_gas, base_fee = EXCLUDED.base_fee,
				l1_block = EXCLUDED.l1_block, tx_count = EXCLUDED.tx_count, backlogs = EXCLUDED.backlogs,
				constraint_bips = EXCLUDED.constraint_bips, exponent_bips = EXCLUDED.exponent_bips,
				predicted_base_fee = EXCLUDED.predicted_base_fee, min_base_fee = EXCLUDED.min_base_fee,
				anchored = EXCLUDED.anchored, pricing_version = EXCLUDED.pricing_version`, args...); err != nil {
				lo, hi := blockSpan(batch)
				return fmt.Errorf("upsert %d blocks %d..%d: %w", len(batch), lo, hi, err)
			}
		}
	}
	return nil
}

// BlockByNumber returns one block.
func (p *Postgres) BlockByNumber(ctx context.Context, chainID, number uint64) (*Block, error) {
	return getOne[Block](ctx, p, `SELECT `+blockColumns+` FROM blocks WHERE chain_id = $1 AND number = $2`, chainID, number)
}

// DeleteBlocksAfter removes and returns blocks above a number.
func (p *Postgres) DeleteBlocksAfter(ctx context.Context, chainID, after uint64) ([]Block, error) {
	return selectAll[Block](ctx, p, `DELETE FROM blocks WHERE chain_id = $1 AND number > $2 RETURNING `+blockColumns+``, chainID, after)
}

// BlocksMissingPosterGas returns blocks stored without receipt-backed poster
// gas, ascending from a number. The scan rides the (chain_id, number) primary
// key, so a caller that advances its cursor past what it has repaired keeps
// each pass short.
func (p *Postgres) BlocksMissingPosterGas(ctx context.Context, chainID, from uint64, limit int) ([]Block, error) {
	return selectAll[Block](ctx, p, `SELECT `+blockColumns+` FROM blocks
		WHERE chain_id = $1 AND number >= $2 AND poster_gas IS NULL
		ORDER BY number ASC LIMIT $3`, chainID, from, limit)
}

// SetPosterGas writes poster gas onto stored rows in one statement. The join
// carries the guards the column's own CHECK enforces, so a row that has since
// been rewritten, pruned or rewound is left alone instead of failing the
// batch, and only a row that still lacks the value is touched: the repair is
// then idempotent against a live collector writing the same blocks.
func (p *Postgres) SetPosterGas(ctx context.Context, chainID uint64, gas map[uint64]uint64) error {
	if len(gas) == 0 {
		return nil
	}
	numbers := make([]int64, 0, len(gas))
	for n := range gas {
		numbers = append(numbers, int64(n))
	}
	slices.Sort(numbers)
	values := make([]int64, len(numbers))
	for i, n := range numbers {
		values[i] = int64(gas[uint64(n)])
	}
	if _, err := p.exec(ctx, `
		UPDATE blocks SET poster_gas = v.gas
		FROM (SELECT unnest($2::bigint[]) AS number, unnest($3::bigint[]) AS gas) v
		WHERE blocks.chain_id = $1 AND blocks.number = v.number
			AND blocks.poster_gas IS NULL
			AND v.gas >= 0 AND v.gas <= blocks.gas_used`,
		chainID, pq.Array(numbers), pq.Array(values)); err != nil {
		return fmt.Errorf("set poster gas on %d blocks %d..%d: %w", len(numbers), numbers[0], numbers[len(numbers)-1], err)
	}
	return nil
}

// LatestBlock returns the highest stored block.
func (p *Postgres) LatestBlock(ctx context.Context, chainID uint64) (*Block, error) {
	return getOne[Block](ctx, p, `SELECT `+blockColumns+` FROM blocks WHERE chain_id = $1 ORDER BY number DESC LIMIT 1`, chainID)
}

// OldestBlock returns the lowest stored block.
func (p *Postgres) OldestBlock(ctx context.Context, chainID uint64) (*Block, error) {
	return getOne[Block](ctx, p, `SELECT `+blockColumns+` FROM blocks WHERE chain_id = $1 ORDER BY number ASC LIMIT 1`, chainID)
}

// RecentBlocks returns the newest blocks first.
func (p *Postgres) RecentBlocks(ctx context.Context, chainID uint64, limit int) ([]Block, error) {
	return selectAll[Block](ctx, p, `SELECT `+blockColumns+` FROM blocks WHERE chain_id = $1 ORDER BY number DESC LIMIT $2`, chainID, limit)
}

// BlocksAfter returns blocks above a number, ascending.
func (p *Postgres) BlocksAfter(ctx context.Context, chainID, after uint64, limit int) ([]Block, error) {
	return selectAll[Block](ctx, p, `SELECT `+blockColumns+` FROM blocks WHERE chain_id = $1 AND number > $2 ORDER BY number ASC LIMIT $3`, chainID, after, limit)
}

// BlocksBetween returns blocks in a time range, ascending.
func (p *Postgres) BlocksBetween(ctx context.Context, chainID uint64, from, to time.Time) ([]Block, error) {
	return selectAll[Block](ctx, p, `SELECT `+blockColumns+` FROM blocks WHERE chain_id = $1 AND ts >= $2 AND ts < $3 ORDER BY number ASC`, chainID, from, to)
}

// GasBetween sums total gas and, when receipt coverage is complete, compute
// gas over (from, to].
func (p *Postgres) GasBetween(ctx context.Context, chainID uint64, from, to time.Time) (total uint64, compute *uint64, err error) {
	var row struct {
		Total   int64         `db:"total"`
		Compute sql.NullInt64 `db:"compute"`
	}
	start := time.Now()
	err = sqlx.GetContext(ctx, p.q, &row, `
		SELECT COALESCE(SUM(gas_used), 0)::BIGINT AS total,
			CASE WHEN count(*) = count(poster_gas) THEN COALESCE(SUM(gas_used - poster_gas), 0)::BIGINT END AS compute
		FROM blocks WHERE chain_id = $1 AND ts > $2 AND ts <= $3`, chainID, from, to)
	p.observe(start, err)
	if err != nil {
		return 0, nil, fmt.Errorf("gas between: %w", err)
	}
	total = uint64(max(row.Total, 0))
	if !row.Compute.Valid {
		return total, nil, nil
	}
	computeTotal := uint64(max(row.Compute.Int64, 0))
	return total, &computeTotal, nil
}

// TwoTxBlocks lists candidate batch-report blocks.
func (p *Postgres) TwoTxBlocks(ctx context.Context, chainID, after uint64, limit int) ([]uint64, error) {
	var nums []int64
	start := time.Now()
	err := sqlx.SelectContext(ctx, p.q, &nums, `SELECT number FROM blocks WHERE chain_id = $1 AND tx_count = 2 AND number > $2 ORDER BY number ASC LIMIT $3`, chainID, after, limit)
	p.observe(start, err)
	if err != nil {
		return nil, fmt.Errorf("two tx blocks: %w", err)
	}
	return Uint64s(nums), nil
}

// PruneBlocks deletes blocks older than before.
func (p *Postgres) PruneBlocks(ctx context.Context, chainID uint64, before time.Time) (int64, error) {
	res, err := p.exec(ctx, `DELETE FROM blocks WHERE chain_id = $1 AND ts < $2`, chainID, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const bucketColumns = `chain_id, resolution, bucket_start, blocks, gas_used, poster_gas, fees_wei, poster_fees_wei, base_fee_min, base_fee_avg, base_fee_max, base_fee_sum, exponent_end_bips, backlogs_end, backlogs_max, constraint_bips_end, min_base_fee, floor_fees_wei, surplus_fees_wei, constraint_set_id, replay_error_bips, last_block, pricing_version`

// FoldBuckets adds partial buckets into the stored rows (the backfill's
// path for windows without block rows; the cursor commits with it). A sum
// or fee split that is unknown (NULL) on either side stays unknown; the
// average then uses the rounded reconstruction for the unknown side.
func (p *Postgres) FoldBuckets(ctx context.Context, buckets []Bucket) error {
	type bucketKey struct {
		chainID     uint64
		resolution  string
		bucketStart time.Time
	}
	const columns = 23
	for _, layer := range uniqueLayers(buckets, func(b Bucket) bucketKey {
		return bucketKey{b.ChainID, b.Resolution, b.BucketStart.UTC()}
	}) {
		for from := 0; from < len(layer); from += postgresBatchParameters / columns {
			batch := layer[from:min(from+postgresBatchParameters/columns, len(layer))]
			args := make([]any, 0, len(batch)*columns)
			for _, b := range batch {
				args = append(args, b.ChainID, b.Resolution, b.BucketStart, b.Blocks, b.GasUsed, b.PosterGas, b.FeesWei, b.PosterFeesWei, b.BaseFeeMin, b.BaseFeeAvg, b.BaseFeeMax, b.BaseFeeSum,
					b.ExponentEndBips, b.BacklogsEnd, b.BacklogsMax, b.ConstraintBipsEnd, b.MinBaseFee, b.FloorFeesWei, b.SurplusFeesWei,
					b.ConstraintSetID, b.ReplayErrorBips, b.LastBlock, b.PricingVersion)
			}
			if _, err := p.exec(ctx, `
			INSERT INTO buckets (`+bucketColumns+`)
			VALUES `+valueTuples(len(batch), columns)+`
			ON CONFLICT (chain_id, resolution, bucket_start) DO UPDATE SET
				pricing_version = LEAST(buckets.pricing_version, EXCLUDED.pricing_version),
				blocks = buckets.blocks + EXCLUDED.blocks,
				gas_used = buckets.gas_used + EXCLUDED.gas_used,
				poster_gas = CASE WHEN buckets.poster_gas IS NULL OR EXCLUDED.poster_gas IS NULL THEN NULL
					ELSE buckets.poster_gas + EXCLUDED.poster_gas END,
				fees_wei = buckets.fees_wei + EXCLUDED.fees_wei,
				poster_fees_wei = CASE WHEN buckets.poster_fees_wei IS NULL OR EXCLUDED.poster_fees_wei IS NULL THEN NULL
					ELSE buckets.poster_fees_wei + EXCLUDED.poster_fees_wei END,
				base_fee_sum = buckets.base_fee_sum + EXCLUDED.base_fee_sum,
				floor_fees_wei = CASE WHEN LEAST(buckets.pricing_version, EXCLUDED.pricing_version) = 0 OR buckets.floor_fees_wei IS NULL OR EXCLUDED.floor_fees_wei IS NULL THEN NULL
					ELSE buckets.floor_fees_wei + EXCLUDED.floor_fees_wei END,
				surplus_fees_wei = CASE WHEN LEAST(buckets.pricing_version, EXCLUDED.pricing_version) = 0 OR buckets.surplus_fees_wei IS NULL OR EXCLUDED.surplus_fees_wei IS NULL THEN NULL
					ELSE buckets.surplus_fees_wei + EXCLUDED.surplus_fees_wei END,
				base_fee_min = CASE WHEN buckets.blocks = 0 THEN EXCLUDED.base_fee_min ELSE LEAST(buckets.base_fee_min, EXCLUDED.base_fee_min) END,
				base_fee_max = GREATEST(buckets.base_fee_max, EXCLUDED.base_fee_max),
				base_fee_avg = CASE WHEN buckets.blocks + EXCLUDED.blocks > 0
					THEN floor((COALESCE(buckets.base_fee_sum, buckets.base_fee_avg * buckets.blocks) + COALESCE(EXCLUDED.base_fee_sum, EXCLUDED.base_fee_avg * EXCLUDED.blocks)) / (buckets.blocks + EXCLUDED.blocks))
					ELSE 0 END,
				exponent_end_bips = CASE WHEN EXCLUDED.last_block >= buckets.last_block THEN EXCLUDED.exponent_end_bips ELSE buckets.exponent_end_bips END,
				backlogs_end = CASE WHEN EXCLUDED.last_block >= buckets.last_block THEN EXCLUDED.backlogs_end ELSE buckets.backlogs_end END,
				constraint_bips_end = CASE WHEN LEAST(buckets.pricing_version, EXCLUDED.pricing_version) = 0 THEN NULL
					WHEN EXCLUDED.last_block >= buckets.last_block THEN EXCLUDED.constraint_bips_end ELSE buckets.constraint_bips_end END,
				min_base_fee = CASE WHEN LEAST(buckets.pricing_version, EXCLUDED.pricing_version) = 0 THEN NULL
					WHEN EXCLUDED.last_block >= buckets.last_block THEN EXCLUDED.min_base_fee ELSE buckets.min_base_fee END,
				backlogs_max = ARRAY(SELECT GREATEST(a, b) FROM unnest(buckets.backlogs_max, EXCLUDED.backlogs_max) AS u(a, b)),
				constraint_set_id = CASE WHEN EXCLUDED.last_block >= buckets.last_block THEN COALESCE(EXCLUDED.constraint_set_id, buckets.constraint_set_id) ELSE buckets.constraint_set_id END,
				replay_error_bips = GREATEST(buckets.replay_error_bips, EXCLUDED.replay_error_bips),
				last_block = GREATEST(buckets.last_block, EXCLUDED.last_block)`, args...); err != nil {
				first, last := bucketSpan(batch)
				return fmt.Errorf("fold %d buckets %s..%s: %w", len(batch), first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339), err)
			}
		}
	}
	return nil
}

// RebuildBuckets recomputes buckets from the block rows in their windows,
// mirroring BucketBuilder in SQL. A window that has no rows loses its
// bucket. The constraint set is the one in force at the window's last
// block, and only when its constraint count matches the block's backlogs
// (the live model); a block is never tagged with a set of another shape.
// The bucket's pricing version is the lowest of its blocks': one block of
// history without the breakdown makes the window's exponents, floor and
// fee split unknown rather than letting a rebuild present them as exact.
func (p *Postgres) RebuildBuckets(ctx context.Context, chainID uint64, resolution string, starts []time.Time) error {
	width, ok := Resolutions[resolution]
	if !ok {
		return fmt.Errorf("rebuild buckets: unknown resolution %q", resolution)
	}
	if len(starts) == 0 {
		return nil
	}
	// The upserted CTE below is deliberately not referenced by the DELETE.
	// A data-modifying CTE always runs to completion whether or not the
	// primary query reads its output, and the two touch disjoint rows: the
	// insert covers exactly the requested starts that have blocks, the
	// delete exactly the ones that do not. Joining the delete to it would
	// be worse than redundant, since a rebuild that inserts nothing would
	// then delete nothing and leave the emptied buckets behind.
	if _, err := p.exec(ctx, `
			WITH requested AS (
				SELECT DISTINCT bucket_start, bucket_start + $3::BIGINT * interval '1 second' AS bucket_end
				FROM unnest($4::TIMESTAMPTZ[]) AS r(bucket_start)
			), w AS (
				SELECT requested.bucket_start, blocks.*
				FROM requested
				JOIN blocks ON blocks.chain_id = $1 AND blocks.ts >= requested.bucket_start AND blocks.ts < requested.bucket_end
			), lastb AS (
				SELECT DISTINCT ON (bucket_start) * FROM w ORDER BY bucket_start, number DESC
			), agg AS (
				SELECT bucket_start, count(*) AS blocks,
					COALESCE(sum(gas_used), 0)::BIGINT AS gas_used,
					CASE WHEN count(poster_gas) = count(*) THEN sum(poster_gas)::BIGINT END AS poster_gas,
					COALESCE(sum(base_fee * gas_used), 0) AS fees_wei,
					CASE WHEN count(poster_gas) = count(*)
						THEN sum(base_fee * poster_gas) END AS poster_fees_wei,
					COALESCE(min(base_fee), 0) AS base_fee_min,
					COALESCE(max(base_fee), 0) AS base_fee_max,
					COALESCE(sum(base_fee), 0) AS base_fee_sum,
					COALESCE(min(pricing_version), 1)::SMALLINT AS pricing_version,
					CASE WHEN min(pricing_version) > 0 AND count(min_base_fee) = count(*) AND count(poster_gas) = count(*)
						THEN sum(LEAST(base_fee, min_base_fee) * (gas_used - poster_gas)) END AS floor_fees_wei,
					CASE WHEN min(pricing_version) > 0 AND count(min_base_fee) = count(*) AND count(poster_gas) = count(*)
						THEN sum((base_fee - LEAST(base_fee, min_base_fee)) * (gas_used - poster_gas)) END AS surplus_fees_wei,
					COALESCE(max(CASE WHEN base_fee > 0
						THEN LEAST(floor(abs(predicted_base_fee - base_fee) * 10000 / base_fee), $5::NUMERIC)
						ELSE 0 END), 0)::BIGINT AS replay_error_bips
				FROM w
				GROUP BY bucket_start
			), backlog_values AS (
				SELECT w.bucket_start, u.i, max(u.v) AS v
				FROM w CROSS JOIN LATERAL unnest(w.backlogs) WITH ORDINALITY AS u(v, i)
				GROUP BY w.bucket_start, u.i
			), bmax AS (
				SELECT agg.bucket_start,
					COALESCE(array_agg(backlog_values.v ORDER BY backlog_values.i) FILTER (WHERE backlog_values.i IS NOT NULL), '{}'::NUMERIC[]) AS backlogs_max
				FROM agg LEFT JOIN backlog_values USING (bucket_start)
				GROUP BY agg.bucket_start
			), upserted AS (
			INSERT INTO buckets (`+bucketColumns+`)
			SELECT $1, $2, agg.bucket_start, agg.blocks, agg.gas_used, agg.poster_gas, agg.fees_wei, agg.poster_fees_wei, agg.base_fee_min, floor(agg.base_fee_sum / agg.blocks), agg.base_fee_max, agg.base_fee_sum,
				lastb.exponent_bips, lastb.backlogs, bmax.backlogs_max,
				CASE WHEN agg.pricing_version = 0 THEN NULL ELSE lastb.constraint_bips END,
				CASE WHEN agg.pricing_version = 0 THEN NULL ELSE lastb.min_base_fee END,
				agg.floor_fees_wei, agg.surplus_fees_wei,
				(SELECT cs.id FROM constraint_sets cs WHERE cs.chain_id = $1 AND cs.effective_block <= lastb.number
					AND jsonb_array_length(cs.constraints) = COALESCE(array_length(lastb.backlogs, 1), 0)
					ORDER BY cs.effective_block DESC, cs.id DESC LIMIT 1),
				agg.replay_error_bips, lastb.number, agg.pricing_version
			FROM agg JOIN bmax USING (bucket_start) JOIN lastb USING (bucket_start)
			ON CONFLICT (chain_id, resolution, bucket_start) DO UPDATE SET
				blocks = EXCLUDED.blocks, gas_used = EXCLUDED.gas_used, poster_gas = EXCLUDED.poster_gas,
				fees_wei = EXCLUDED.fees_wei, poster_fees_wei = EXCLUDED.poster_fees_wei,
				base_fee_min = EXCLUDED.base_fee_min, base_fee_avg = EXCLUDED.base_fee_avg, base_fee_max = EXCLUDED.base_fee_max,
				base_fee_sum = EXCLUDED.base_fee_sum, exponent_end_bips = EXCLUDED.exponent_end_bips,
				backlogs_end = EXCLUDED.backlogs_end, backlogs_max = EXCLUDED.backlogs_max,
				constraint_bips_end = EXCLUDED.constraint_bips_end, min_base_fee = EXCLUDED.min_base_fee,
				floor_fees_wei = EXCLUDED.floor_fees_wei, surplus_fees_wei = EXCLUDED.surplus_fees_wei,
				constraint_set_id = EXCLUDED.constraint_set_id, replay_error_bips = EXCLUDED.replay_error_bips,
				last_block = EXCLUDED.last_block, pricing_version = EXCLUDED.pricing_version
			RETURNING bucket_start
			)
			DELETE FROM buckets
			USING requested
			WHERE buckets.chain_id = $1 AND buckets.resolution = $2 AND buckets.bucket_start = requested.bucket_start
				AND NOT EXISTS (SELECT 1 FROM w WHERE w.bucket_start = requested.bucket_start)`,
		chainID, resolution, int64(width/time.Second), pq.Array(starts), strconv.FormatInt(math.MaxInt64, 10)); err != nil {
		return fmt.Errorf("rebuild %d buckets at %s: %w", len(starts), resolution, err)
	}
	return nil
}

// DeleteBucketsBefore drops every resolution's buckets starting before t.
func (p *Postgres) DeleteBucketsBefore(ctx context.Context, chainID uint64, before time.Time) (int64, error) {
	res, err := p.exec(ctx, `DELETE FROM buckets WHERE chain_id = $1 AND bucket_start < $2`, chainID, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Buckets returns buckets in a range, ascending.
func (p *Postgres) Buckets(ctx context.Context, chainID uint64, resolution string, from, to time.Time) ([]Bucket, error) {
	return selectAll[Bucket](ctx, p, `SELECT `+bucketColumns+` FROM buckets WHERE chain_id = $1 AND resolution = $2 AND bucket_start >= $3 AND bucket_start < $4 ORDER BY bucket_start ASC`,
		chainID, resolution, from, to)
}

const sampleColumns = `chain_id, sampled_at, block_number, base_fee, min_base_fee, constraints, legacy, prices, l1, accounts`

// InsertStateSample stores a sample (replacing one at the same instant).
func (p *Postgres) InsertStateSample(ctx context.Context, s StateSample) error {
	_, err := p.exec(ctx, `
		INSERT INTO state_samples (`+sampleColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (chain_id, sampled_at) DO UPDATE SET
			block_number = EXCLUDED.block_number, base_fee = EXCLUDED.base_fee, min_base_fee = EXCLUDED.min_base_fee,
			constraints = EXCLUDED.constraints, legacy = EXCLUDED.legacy, prices = EXCLUDED.prices,
			l1 = COALESCE(EXCLUDED.l1, state_samples.l1), accounts = COALESCE(EXCLUDED.accounts, state_samples.accounts)`,
		s.ChainID, s.SampledAt, s.BlockNumber, s.BaseFee, s.MinBaseFee, s.Constraints, s.Legacy, s.Prices, s.L1, s.Accounts)
	return err
}

// LatestStateSample returns the newest sample, optionally with L1 data.
func (p *Postgres) LatestStateSample(ctx context.Context, chainID uint64, withL1 bool) (*StateSample, error) {
	q := `SELECT ` + sampleColumns + ` FROM state_samples WHERE chain_id = $1`
	if withL1 {
		q += ` AND l1 IS NOT NULL`
	}
	return getOne[StateSample](ctx, p, q+` ORDER BY sampled_at DESC LIMIT 1`, chainID)
}

// StateSampleAt returns the newest sample at or below a block.
func (p *Postgres) StateSampleAt(ctx context.Context, chainID, block uint64) (*StateSample, error) {
	return getOne[StateSample](ctx, p, `SELECT `+sampleColumns+`
		FROM state_samples WHERE chain_id = $1 AND block_number <= $2
		ORDER BY block_number DESC, sampled_at DESC LIMIT 1`, chainID, block)
}

// L1Samples returns one L1-carrying sample per step.
func (p *Postgres) L1Samples(ctx context.Context, chainID uint64, from, to time.Time, step time.Duration) ([]StateSample, error) {
	secs := max(int64(step/time.Second), 1)
	return selectAll[StateSample](ctx, p, `
		SELECT DISTINCT ON (floor(extract(epoch FROM sampled_at) / $4)) `+sampleColumns+`
		FROM state_samples
		WHERE chain_id = $1 AND l1 IS NOT NULL AND sampled_at >= $2 AND sampled_at < $3
		ORDER BY floor(extract(epoch FROM sampled_at) / $4), sampled_at ASC`,
		chainID, from, to, secs)
}

// PruneStateSamples deletes samples older than before.
func (p *Postgres) PruneStateSamples(ctx context.Context, chainID uint64, before time.Time) (int64, error) {
	res, err := p.exec(ctx, `DELETE FROM state_samples WHERE chain_id = $1 AND sampled_at < $2`, chainID, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteStateSamplesAfter deletes samples taken above a block.
func (p *Postgres) DeleteStateSamplesAfter(ctx context.Context, chainID, block uint64) (int64, error) {
	res, err := p.exec(ctx, `DELETE FROM state_samples WHERE chain_id = $1 AND block_number > $2`, chainID, block)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const missingRangeColumns = `chain_id, from_block, to_block, detected_at, lifecycle, reason, cursor, replay_state, folded, retry_count, last_attempt_at, next_retry_at, last_error, predecessor_at, successor_at, cursor_at, created_at, updated_at`

// MissingRanges lists every durable missing interval in block order.
func (p *Postgres) MissingRanges(ctx context.Context, chainID uint64) ([]MissingRange, error) {
	return selectAll[MissingRange](ctx, p, `SELECT `+missingRangeColumns+` FROM missing_ranges WHERE chain_id = $1 ORDER BY from_block`, chainID)
}

// missingRangeBindings is the number of bound values per inserted row, one
// short of missingRangeColumns because updated_at is always now().
const missingRangeBindings = 17

// missingRangeBatch keeps one insert well inside the 65535 bound parameters a
// statement can carry.
const missingRangeBatch = 1000

// ReplaceMissingRanges replaces one chain's normalized intervals. Collector
// callers hold the chain transaction, so the delete and inserts commit as one
// durable lifecycle update. Normalization can reshape the whole set, so the
// rewrite stays a replace, but the rows go in batched statements: a per row
// round trip would hold the chain lock in proportion to the set size.
func (p *Postgres) ReplaceMissingRanges(ctx context.Context, chainID uint64, ranges []MissingRange) error {
	if _, err := p.exec(ctx, `DELETE FROM missing_ranges WHERE chain_id = $1`, chainID); err != nil {
		return err
	}
	for chunk := range slices.Chunk(ranges, missingRangeBatch) {
		if err := p.insertMissingRanges(ctx, chainID, chunk); err != nil {
			return fmt.Errorf("missing ranges %d..%d: %w", chunk[0].From, chunk[len(chunk)-1].To, err)
		}
	}
	return nil
}

func (p *Postgres) insertMissingRanges(ctx context.Context, chainID uint64, ranges []MissingRange) error {
	var values strings.Builder
	args := make([]any, 0, len(ranges)*missingRangeBindings)
	for _, r := range ranges {
		if len(args) > 0 {
			values.WriteString(", ")
		}
		values.WriteByte('(')
		for i := 1; i <= missingRangeBindings; i++ {
			if i > 1 {
				values.WriteString(", ")
			}
			if i == missingRangeBindings {
				fmt.Fprintf(&values, "COALESCE($%d, now())", len(args)+i)
				continue
			}
			fmt.Fprintf(&values, "$%d", len(args)+i)
		}
		values.WriteString(", now())")
		args = append(args, chainID, r.From, r.To, r.DetectedAt, r.Lifecycle, r.Reason, r.Cursor, r.ReplayState,
			r.Folded, r.RetryCount, r.LastAttemptAt, r.NextRetryAt, r.LastError, r.PredecessorAt, r.SuccessorAt,
			r.CursorAt, nullTime(r.CreatedAt))
	}
	_, err := p.exec(ctx, `INSERT INTO missing_ranges (`+missingRangeColumns+`) VALUES `+values.String(), args...)
	return err
}

func nullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: !t.IsZero()}
}

const ownerActionColumns = `chain_id, block_number, tx_hash, tx_index, log_index, ts, method, selector, args`

// InsertOwnerActions inserts new actions and returns the number added.
func (p *Postgres) InsertOwnerActions(ctx context.Context, actions []OwnerAction) (int, error) {
	inserted := 0
	for _, a := range actions {
		res, err := p.exec(ctx, `
			INSERT INTO owner_actions (`+ownerActionColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (chain_id, tx_hash, log_index) DO NOTHING`,
			a.ChainID, a.BlockNumber, a.TxHash, a.TxIndex, a.LogIndex, a.TS, a.Method, a.Selector, a.Args)
		if err != nil {
			return inserted, fmt.Errorf("owner action %s/%d: %w", a.TxHash, a.LogIndex, err)
		}
		n, _ := res.RowsAffected()
		inserted += int(n)
		if n == 0 && a.TxIndex.Valid {
			if _, err := p.exec(ctx, `UPDATE owner_actions SET tx_index = $4 WHERE chain_id = $1 AND tx_hash = $2 AND log_index = $3 AND tx_index IS NULL`,
				a.ChainID, a.TxHash, a.LogIndex, a.TxIndex); err != nil {
				return inserted, fmt.Errorf("owner action %s/%d transaction index: %w", a.TxHash, a.LogIndex, err)
			}
		}
	}
	return inserted, nil
}

// OwnerActions lists actions newest first.
func (p *Postgres) OwnerActions(ctx context.Context, chainID uint64, from, to time.Time, limit int) ([]OwnerAction, error) {
	q := `SELECT ` + ownerActionColumns + ` FROM owner_actions WHERE chain_id = $1`
	args := []any{chainID}
	if !from.IsZero() {
		args = append(args, from)
		q += fmt.Sprintf(` AND ts >= $%d`, len(args))
	}
	if !to.IsZero() {
		args = append(args, to)
		q += fmt.Sprintf(` AND ts < $%d`, len(args))
	}
	q += ` ORDER BY block_number DESC, log_index DESC`
	if limit > 0 {
		args = append(args, limit)
		q += fmt.Sprintf(` LIMIT $%d`, len(args))
	}
	return selectAll[OwnerAction](ctx, p, q, args...)
}

// OwnerActionsSince lists actions from a block on, ascending.
func (p *Postgres) OwnerActionsSince(ctx context.Context, chainID, block uint64) ([]OwnerAction, error) {
	return selectAll[OwnerAction](ctx, p, `SELECT `+ownerActionColumns+` FROM owner_actions WHERE chain_id = $1 AND block_number >= $2 ORDER BY block_number ASC, log_index ASC`, chainID, block)
}

// RewindAfter deletes owner actions, constraint sets and batch reports
// above a block.
func (p *Postgres) RewindAfter(ctx context.Context, chainID, block uint64) error {
	for _, q := range []string{
		`DELETE FROM owner_actions WHERE chain_id = $1 AND block_number > $2`,
		`DELETE FROM constraint_sets WHERE chain_id = $1 AND effective_block > $2`,
		`DELETE FROM batch_reports WHERE chain_id = $1 AND block_number > $2`,
	} {
		if _, err := p.exec(ctx, q, chainID, block); err != nil {
			return fmt.Errorf("rewind after %d: %w", block, err)
		}
	}
	return nil
}

// InsertConstraintSet upserts a set and returns its id.
func (p *Postgres) InsertConstraintSet(ctx context.Context, cs ConstraintSet) (int64, error) {
	var id int64
	start := time.Now()
	err := sqlx.GetContext(ctx, p.q, &id, `
		INSERT INTO constraint_sets (chain_id, effective_block, effective_at, constraints, source)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (chain_id, effective_block, source) DO UPDATE SET constraints = EXCLUDED.constraints, effective_at = EXCLUDED.effective_at
		RETURNING id`,
		cs.ChainID, cs.EffectiveBlock, cs.EffectiveAt, cs.Constraints, cs.Source)
	p.observe(start, err)
	if err != nil {
		return 0, fmt.Errorf("constraint set: %w", err)
	}
	return id, nil
}

// UpdateConstraintSet rewrites a set in place, keeping its id.
func (p *Postgres) UpdateConstraintSet(ctx context.Context, cs ConstraintSet) error {
	_, err := p.exec(ctx, `UPDATE constraint_sets SET effective_block = $3, effective_at = $4, constraints = $5, source = $6 WHERE id = $1 AND chain_id = $2`,
		cs.ID, cs.ChainID, cs.EffectiveBlock, cs.EffectiveAt, cs.Constraints, cs.Source)
	return err
}

// ConstraintSets lists sets ascending by block.
func (p *Postgres) ConstraintSets(ctx context.Context, chainID uint64) ([]ConstraintSet, error) {
	return selectAll[ConstraintSet](ctx, p, `SELECT id, chain_id, effective_block, effective_at, constraints, source FROM constraint_sets WHERE chain_id = $1 ORDER BY effective_block ASC, id ASC`, chainID)
}

// UpsertBatchReports stores reports.
func (p *Postgres) UpsertBatchReports(ctx context.Context, reports []BatchReport) error {
	for _, r := range reports {
		if _, err := p.exec(ctx, `
			INSERT INTO batch_reports (chain_id, block_number, batch_number, batch_ts, poster, calldata_len, calldata_nonzero, extra_gas, l1_base_fee, gas_spent, wei_spent,
				report_version, arbos_version, per_batch_gas_charge, parent_gas_floor_per_token, cost_calculation_version, attributed_gas_spent, attributed_wei_spent)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
			ON CONFLICT (chain_id, block_number) DO UPDATE SET
				batch_number = EXCLUDED.batch_number, batch_ts = EXCLUDED.batch_ts, poster = EXCLUDED.poster,
				calldata_len = EXCLUDED.calldata_len, calldata_nonzero = EXCLUDED.calldata_nonzero, extra_gas = EXCLUDED.extra_gas,
				l1_base_fee = EXCLUDED.l1_base_fee, gas_spent = EXCLUDED.gas_spent, wei_spent = EXCLUDED.wei_spent,
				report_version = EXCLUDED.report_version, arbos_version = EXCLUDED.arbos_version,
				per_batch_gas_charge = EXCLUDED.per_batch_gas_charge, parent_gas_floor_per_token = EXCLUDED.parent_gas_floor_per_token,
				cost_calculation_version = EXCLUDED.cost_calculation_version,
				attributed_gas_spent = EXCLUDED.attributed_gas_spent, attributed_wei_spent = EXCLUDED.attributed_wei_spent`,
			r.ChainID, r.BlockNumber, r.BatchNumber, r.BatchTS, r.Poster, r.CalldataLen, r.CalldataNonzero, r.ExtraGas, r.L1BaseFee, r.GasSpent, r.WeiSpent,
			r.ReportVersion, r.ArbOSVersion, r.PerBatchGasCharge, r.ParentGasFloorPerToken, r.CostCalculationVersion, r.GasSpent, r.WeiSpent); err != nil {
			return fmt.Errorf("batch report %d: %w", r.BlockNumber, err)
		}
	}
	return nil
}

const batchReportColumns = `chain_id, block_number, batch_number, batch_ts, poster, calldata_len, calldata_nonzero, extra_gas, l1_base_fee,
	attributed_gas_spent, attributed_wei_spent, report_version, arbos_version, per_batch_gas_charge, parent_gas_floor_per_token, cost_calculation_version`

// BatchReports lists reports in a time range, one row per report.
func (p *Postgres) BatchReports(ctx context.Context, chainID uint64, from, to time.Time) ([]BatchReport, error) {
	return selectAll[BatchReport](ctx, p, `SELECT `+batchReportColumns+` FROM batch_reports WHERE chain_id = $1 AND batch_ts >= $2 AND batch_ts < $3 AND cost_calculation_version = 1 ORDER BY batch_ts ASC, block_number ASC`, chainID, from, to)
}

// BatchBuckets aggregates reports per step.
func (p *Postgres) BatchBuckets(ctx context.Context, chainID uint64, from, to time.Time, step time.Duration) ([]BatchBucket, error) {
	secs := max(int64(step/time.Second), 1)
	return selectAll[BatchBucket](ctx, p, `
		SELECT to_timestamp(floor(extract(epoch FROM batch_ts) / $4) * $4) AS t,
			count(*)::BIGINT AS batches,
			COALESCE(sum(attributed_gas_spent), 0)::BIGINT AS gas_spent,
			COALESCE(sum(attributed_wei_spent), 0) AS wei_spent,
			COALESCE(floor(avg(l1_base_fee)), 0) AS l1_base_fee_avg,
			COALESCE(sum(calldata_len), 0)::BIGINT AS calldata_bytes
		FROM batch_reports
		WHERE chain_id = $1 AND batch_ts >= $2 AND batch_ts < $3 AND cost_calculation_version = 1
		GROUP BY 1 ORDER BY 1 ASC`,
		chainID, from, to, secs)
}

// GetState reads a checkpoint.
func (p *Postgres) GetState(ctx context.Context, chainID uint64, key string) (value string, found bool, err error) {
	start := time.Now()
	err = sqlx.GetContext(ctx, p.q, &value, `SELECT value FROM collector_state WHERE chain_id = $1 AND key = $2`, chainID, key)
	observed := err
	if errors.Is(err, sql.ErrNoRows) {
		observed = nil
	}
	p.observe(start, observed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("state %s: %w", key, err)
	}
	return value, true, nil
}

// SetState writes a checkpoint.
func (p *Postgres) SetState(ctx context.Context, chainID uint64, key, value string) error {
	_, err := p.exec(ctx, `
		INSERT INTO collector_state (chain_id, key, value, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (chain_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, chainID, key, value)
	return err
}

// DeleteState removes a checkpoint.
func (p *Postgres) DeleteState(ctx context.Context, chainID uint64, key string) error {
	_, err := p.exec(ctx, `DELETE FROM collector_state WHERE chain_id = $1 AND key = $2`, chainID, key)
	return err
}

// States reads every checkpoint of a network.
func (p *Postgres) States(ctx context.Context, chainID uint64) (map[string]string, error) {
	type kv struct {
		Key   string `db:"key"`
		Value string `db:"value"`
	}
	rows, err := selectAll[kv](ctx, p, `SELECT key, value FROM collector_state WHERE chain_id = $1`, chainID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}

// Notify sends a payload on a channel.
func (p *Postgres) Notify(ctx context.Context, channel, payload string) error {
	_, err := p.exec(ctx, `SELECT pg_notify($1, $2)`, channel, payload)
	return err
}

// ResetSchema drops and recreates the public schema, then migrates. For
// integration tests only.
func ResetSchema(ctx context.Context, d *sqlx.DB) error {
	if _, err := d.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		return fmt.Errorf("reset schema: %w", err)
	}
	return RunMigrations(d.DB)
}

var _ Store = (*Postgres)(nil)
