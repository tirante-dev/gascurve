package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// Postgres implements Store on sqlx.
type Postgres struct {
	db *sqlx.DB
	q  sqlx.ExtContext
}

// NewPostgres wraps an open connection pool.
func NewPostgres(d *sqlx.DB) *Postgres {
	return &Postgres{db: d, q: d}
}

// DB exposes the underlying pool (for migrations and shutdown).
func (p *Postgres) DB() *sqlx.DB { return p.db }

// Ping checks connectivity.
func (p *Postgres) Ping(ctx context.Context) error {
	return p.db.PingContext(ctx)
}

// WithTx runs fn in a transaction. Nested calls reuse the outer transaction.
func (p *Postgres) WithTx(ctx context.Context, fn func(Store) error) error {
	if _, ok := p.q.(*sqlx.Tx); ok {
		return fn(p)
	}
	tx, err := p.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(&Postgres{db: p.db, q: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (p *Postgres) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	res, err := p.q.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	return res, nil
}

func selectAll[T any](ctx context.Context, p *Postgres, query string, args ...any) ([]T, error) {
	var out []T
	if err := sqlx.SelectContext(ctx, p.q, &out, query, args...); err != nil {
		return nil, fmt.Errorf("select: %w", err)
	}
	return out, nil
}

func getOne[T any](ctx context.Context, p *Postgres, query string, args ...any) (*T, error) {
	var out T
	if err := sqlx.GetContext(ctx, p.q, &out, query, args...); err != nil {
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

// NetworkByRef resolves a name or chain id.
func (p *Postgres) NetworkByRef(ctx context.Context, ref string) (*Network, error) {
	id, _ := strconv.ParseUint(ref, 10, 64)
	return getOne[Network](ctx, p, `SELECT `+networkColumns+` FROM networks WHERE name = $1 OR chain_id = $2 LIMIT 1`, ref, id)
}

// UpdateNetworkHead records the newest block and sample time.
func (p *Postgres) UpdateNetworkHead(ctx context.Context, chainID, headBlock uint64, headAt, sampledAt time.Time) error {
	_, err := p.exec(ctx, `UPDATE networks SET head_block = $2, head_at = $3, last_sample_at = $4, last_error = NULL, updated_at = now() WHERE chain_id = $1`,
		chainID, headBlock, headAt, sampledAt)
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

const blockColumns = `chain_id, number, ts, gas_used, base_fee, l1_block, tx_count, backlogs, exponent_bips, predicted_base_fee, anchored`

// UpsertBlocks writes blocks, replacing replay fields on conflict.
func (p *Postgres) UpsertBlocks(ctx context.Context, blocks []Block) error {
	for _, b := range blocks {
		if _, err := p.exec(ctx, `
			INSERT INTO blocks (`+blockColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (chain_id, number) DO UPDATE SET
				ts = EXCLUDED.ts, gas_used = EXCLUDED.gas_used, base_fee = EXCLUDED.base_fee,
				l1_block = EXCLUDED.l1_block, tx_count = EXCLUDED.tx_count, backlogs = EXCLUDED.backlogs,
				exponent_bips = EXCLUDED.exponent_bips, predicted_base_fee = EXCLUDED.predicted_base_fee,
				anchored = EXCLUDED.anchored`,
			b.ChainID, b.Number, b.TS, b.GasUsed, b.BaseFee, b.L1Block, b.TxCount, b.Backlogs, b.ExponentBips, b.PredictedBaseFee, b.Anchored); err != nil {
			return fmt.Errorf("block %d: %w", b.Number, err)
		}
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

// GasUsedBetween sums gas over (from, to].
func (p *Postgres) GasUsedBetween(ctx context.Context, chainID uint64, from, to time.Time) (uint64, error) {
	var total int64
	if err := sqlx.GetContext(ctx, p.q, &total, `SELECT COALESCE(SUM(gas_used), 0)::BIGINT FROM blocks WHERE chain_id = $1 AND ts > $2 AND ts <= $3`, chainID, from, to); err != nil {
		return 0, fmt.Errorf("gas used: %w", err)
	}
	return uint64(max(total, 0)), nil
}

// TwoTxBlocks lists candidate batch-report blocks.
func (p *Postgres) TwoTxBlocks(ctx context.Context, chainID, after uint64, limit int) ([]uint64, error) {
	var nums []int64
	if err := sqlx.SelectContext(ctx, p.q, &nums, `SELECT number FROM blocks WHERE chain_id = $1 AND tx_count = 2 AND number > $2 ORDER BY number ASC LIMIT $3`, chainID, after, limit); err != nil {
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

// FoldBuckets merges partial buckets into the stored rows.
func (p *Postgres) FoldBuckets(ctx context.Context, buckets []Bucket) error {
	for _, b := range buckets {
		if _, err := p.exec(ctx, `
			INSERT INTO buckets (chain_id, resolution, bucket_start, blocks, gas_used, fees_wei, base_fee_min, base_fee_avg, base_fee_max,
				exponent_end_bips, backlogs_end, backlogs_max, constraint_set_id, replay_error_bips, last_block)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			ON CONFLICT (chain_id, resolution, bucket_start) DO UPDATE SET
				blocks = buckets.blocks + EXCLUDED.blocks,
				gas_used = buckets.gas_used + EXCLUDED.gas_used,
				fees_wei = buckets.fees_wei + EXCLUDED.fees_wei,
				base_fee_min = CASE WHEN buckets.blocks = 0 THEN EXCLUDED.base_fee_min ELSE LEAST(buckets.base_fee_min, EXCLUDED.base_fee_min) END,
				base_fee_max = GREATEST(buckets.base_fee_max, EXCLUDED.base_fee_max),
				base_fee_avg = CASE WHEN buckets.blocks + EXCLUDED.blocks > 0
					THEN floor((buckets.base_fee_avg * buckets.blocks + EXCLUDED.base_fee_avg * EXCLUDED.blocks) / (buckets.blocks + EXCLUDED.blocks))
					ELSE 0 END,
				exponent_end_bips = CASE WHEN EXCLUDED.last_block >= buckets.last_block THEN EXCLUDED.exponent_end_bips ELSE buckets.exponent_end_bips END,
				backlogs_end = CASE WHEN EXCLUDED.last_block >= buckets.last_block THEN EXCLUDED.backlogs_end ELSE buckets.backlogs_end END,
				backlogs_max = ARRAY(SELECT GREATEST(a, b) FROM unnest(buckets.backlogs_max, EXCLUDED.backlogs_max) AS u(a, b)),
				constraint_set_id = CASE WHEN EXCLUDED.last_block >= buckets.last_block THEN COALESCE(EXCLUDED.constraint_set_id, buckets.constraint_set_id) ELSE buckets.constraint_set_id END,
				replay_error_bips = GREATEST(buckets.replay_error_bips, EXCLUDED.replay_error_bips),
				last_block = GREATEST(buckets.last_block, EXCLUDED.last_block)`,
			b.ChainID, b.Resolution, b.BucketStart, b.Blocks, b.GasUsed, b.FeesWei, b.BaseFeeMin, b.BaseFeeAvg, b.BaseFeeMax,
			b.ExponentEndBips, b.BacklogsEnd, b.BacklogsMax, b.ConstraintSetID, b.ReplayErrorBips, b.LastBlock); err != nil {
			return fmt.Errorf("bucket %s %s: %w", b.Resolution, b.BucketStart.Format(time.RFC3339), err)
		}
	}
	return nil
}

const bucketColumns = `chain_id, resolution, bucket_start, blocks, gas_used, fees_wei, base_fee_min, base_fee_avg, base_fee_max, exponent_end_bips, backlogs_end, backlogs_max, constraint_set_id, replay_error_bips, last_block`

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

const ownerActionColumns = `chain_id, block_number, tx_hash, log_index, ts, method, selector, args`

// InsertOwnerActions inserts new actions and returns the number added.
func (p *Postgres) InsertOwnerActions(ctx context.Context, actions []OwnerAction) (int, error) {
	inserted := 0
	for _, a := range actions {
		res, err := p.exec(ctx, `
			INSERT INTO owner_actions (`+ownerActionColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (chain_id, tx_hash, log_index) DO NOTHING`,
			a.ChainID, a.BlockNumber, a.TxHash, a.LogIndex, a.TS, a.Method, a.Selector, a.Args)
		if err != nil {
			return inserted, fmt.Errorf("owner action %s/%d: %w", a.TxHash, a.LogIndex, err)
		}
		n, _ := res.RowsAffected()
		inserted += int(n)
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

// InsertConstraintSet upserts a set and returns its id.
func (p *Postgres) InsertConstraintSet(ctx context.Context, cs ConstraintSet) (int64, error) {
	var id int64
	if err := sqlx.GetContext(ctx, p.q, &id, `
		INSERT INTO constraint_sets (chain_id, effective_block, effective_at, constraints, source)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (chain_id, effective_block, source) DO UPDATE SET constraints = EXCLUDED.constraints, effective_at = EXCLUDED.effective_at
		RETURNING id`,
		cs.ChainID, cs.EffectiveBlock, cs.EffectiveAt, cs.Constraints, cs.Source); err != nil {
		return 0, fmt.Errorf("constraint set: %w", err)
	}
	return id, nil
}

// ConstraintSets lists sets ascending by block.
func (p *Postgres) ConstraintSets(ctx context.Context, chainID uint64) ([]ConstraintSet, error) {
	return selectAll[ConstraintSet](ctx, p, `SELECT id, chain_id, effective_block, effective_at, constraints, source FROM constraint_sets WHERE chain_id = $1 ORDER BY effective_block ASC, id ASC`, chainID)
}

// UpsertBatchReports stores reports.
func (p *Postgres) UpsertBatchReports(ctx context.Context, reports []BatchReport) error {
	for _, r := range reports {
		if _, err := p.exec(ctx, `
			INSERT INTO batch_reports (chain_id, block_number, batch_number, batch_ts, poster, calldata_len, calldata_nonzero, extra_gas, l1_base_fee, gas_spent, wei_spent)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (chain_id, block_number) DO UPDATE SET
				batch_number = EXCLUDED.batch_number, batch_ts = EXCLUDED.batch_ts, poster = EXCLUDED.poster,
				calldata_len = EXCLUDED.calldata_len, calldata_nonzero = EXCLUDED.calldata_nonzero, extra_gas = EXCLUDED.extra_gas,
				l1_base_fee = EXCLUDED.l1_base_fee, gas_spent = EXCLUDED.gas_spent, wei_spent = EXCLUDED.wei_spent`,
			r.ChainID, r.BlockNumber, r.BatchNumber, r.BatchTS, r.Poster, r.CalldataLen, r.CalldataNonzero, r.ExtraGas, r.L1BaseFee, r.GasSpent, r.WeiSpent); err != nil {
			return fmt.Errorf("batch report %d: %w", r.BlockNumber, err)
		}
	}
	return nil
}

// BatchBuckets aggregates reports per step.
func (p *Postgres) BatchBuckets(ctx context.Context, chainID uint64, from, to time.Time, step time.Duration) ([]BatchBucket, error) {
	secs := max(int64(step/time.Second), 1)
	return selectAll[BatchBucket](ctx, p, `
		SELECT to_timestamp(floor(extract(epoch FROM batch_ts) / $4) * $4) AS t,
			count(*)::BIGINT AS batches,
			COALESCE(sum(gas_spent), 0)::BIGINT AS gas_spent,
			COALESCE(sum(wei_spent), 0) AS wei_spent,
			COALESCE(floor(avg(l1_base_fee)), 0) AS l1_base_fee_avg,
			COALESCE(sum(calldata_len), 0)::BIGINT AS calldata_bytes
		FROM batch_reports
		WHERE chain_id = $1 AND batch_ts >= $2 AND batch_ts < $3
		GROUP BY 1 ORDER BY 1 ASC`,
		chainID, from, to, secs)
}

// GetState reads a checkpoint.
func (p *Postgres) GetState(ctx context.Context, chainID uint64, key string) (value string, found bool, err error) {
	err = sqlx.GetContext(ctx, p.q, &value, `SELECT value FROM collector_state WHERE chain_id = $1 AND key = $2`, chainID, key)
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

// pqArray is a helper to build BIGINT[] parameters from unsigned values.
func pqArray(v []uint64) pq.Int64Array {
	return pq.Int64Array(Int64s(v))
}

// PQArray converts backlogs into the BIGINT[] parameter type.
func PQArray(v []uint64) pq.Int64Array { return pqArray(v) }
