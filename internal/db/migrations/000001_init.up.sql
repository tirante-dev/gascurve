-- The gascurve schema. Nothing has been deployed from this repository yet,
-- so the schema is created in its final shape rather than built up through
-- a chain of migrations, one of which (an unconditional rewrite of the
-- columns 000004 and 000005 had backfilled with placeholders) would have
-- destroyed genuine values on any database that had already collected.
--
-- Two conventions run through the tables:
--
--   * Wei is NUMERIC(40,0) and backlogs are NUMERIC(20,0)[]: a backlog is
--     a uint64 in the pricer and saturates at 2^64-1, which does not fit
--     in a BIGINT.
--   * A nullable column means unknown, never zero. pricing_version says
--     which: version 1 rows carry the full pricing breakdown (the
--     per-constraint exponents, the minimum base fee in force and
--     therefore an exact fee split), version 0 rows are history recorded
--     without it, and their unknown columns are NULL so the API can report
--     them as unknown instead of serving a zero that looks authoritative.

CREATE TABLE IF NOT EXISTS networks (
    chain_id        BIGINT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    display_name    TEXT NOT NULL,
    explorer_url    TEXT NOT NULL DEFAULT '',
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    head_block      BIGINT,
    head_at         TIMESTAMPTZ,
    last_sample_at  TIMESTAMPTZ,
    last_error      TEXT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- hash and parent_hash are the block ancestry the collector compares a new
-- head against to notice a reorg and rewind to the common ancestor.
CREATE TABLE IF NOT EXISTS blocks (
    chain_id            BIGINT NOT NULL,
    number              BIGINT NOT NULL,
    ts                  TIMESTAMPTZ NOT NULL,
    gas_used            BIGINT NOT NULL,
    base_fee            NUMERIC(40,0) NOT NULL,
    l1_block            BIGINT NOT NULL DEFAULT 0,
    tx_count            INT NOT NULL DEFAULT 0,
    backlogs            NUMERIC(20,0)[] NOT NULL DEFAULT '{}'::NUMERIC(20,0)[],
    exponent_bips       BIGINT NOT NULL DEFAULT 0,
    predicted_base_fee  NUMERIC(40,0) NOT NULL DEFAULT 0,
    anchored            BOOLEAN NOT NULL DEFAULT FALSE,
    hash                TEXT NOT NULL DEFAULT '',
    parent_hash         TEXT NOT NULL DEFAULT '',
    constraint_bips     BIGINT[],
    min_base_fee        NUMERIC(40,0),
    pricing_version     SMALLINT NOT NULL DEFAULT 1,
    PRIMARY KEY (chain_id, number)
);
CREATE INDEX IF NOT EXISTS blocks_chain_ts ON blocks (chain_id, ts);
CREATE INDEX IF NOT EXISTS blocks_two_tx ON blocks (chain_id, number) WHERE tx_count = 2;

-- base_fee_sum is the exact running sum, so the average never re-rounds a
-- rounded average; it is NULL when the sum behind a bucket is not known.
CREATE TABLE IF NOT EXISTS buckets (
    chain_id            BIGINT NOT NULL,
    resolution          TEXT NOT NULL,
    bucket_start        TIMESTAMPTZ NOT NULL,
    blocks              INT NOT NULL DEFAULT 0,
    gas_used            BIGINT NOT NULL DEFAULT 0,
    fees_wei            NUMERIC(40,0) NOT NULL DEFAULT 0,
    base_fee_min        NUMERIC(40,0) NOT NULL DEFAULT 0,
    base_fee_avg        NUMERIC(40,0) NOT NULL DEFAULT 0,
    base_fee_max        NUMERIC(40,0) NOT NULL DEFAULT 0,
    exponent_end_bips   BIGINT NOT NULL DEFAULT 0,
    backlogs_end        NUMERIC(20,0)[] NOT NULL DEFAULT '{}'::NUMERIC(20,0)[],
    backlogs_max        NUMERIC(20,0)[] NOT NULL DEFAULT '{}'::NUMERIC(20,0)[],
    constraint_set_id   INT,
    replay_error_bips   BIGINT NOT NULL DEFAULT 0,
    last_block          BIGINT NOT NULL DEFAULT 0,
    base_fee_sum        NUMERIC(40,0),
    constraint_bips_end BIGINT[],
    min_base_fee        NUMERIC(40,0),
    floor_fees_wei      NUMERIC(40,0),
    surplus_fees_wei    NUMERIC(40,0),
    pricing_version     SMALLINT NOT NULL DEFAULT 1,
    PRIMARY KEY (chain_id, resolution, bucket_start)
);

CREATE TABLE IF NOT EXISTS state_samples (
    chain_id        BIGINT NOT NULL,
    sampled_at      TIMESTAMPTZ NOT NULL,
    block_number    BIGINT NOT NULL,
    base_fee        NUMERIC(40,0) NOT NULL,
    min_base_fee    NUMERIC(40,0) NOT NULL,
    constraints     JSONB NOT NULL DEFAULT '[]',
    legacy          JSONB,
    prices          JSONB NOT NULL DEFAULT '{}',
    l1              JSONB,
    accounts        JSONB,
    PRIMARY KEY (chain_id, sampled_at)
);
CREATE INDEX IF NOT EXISTS state_samples_l1 ON state_samples (chain_id, sampled_at) WHERE l1 IS NOT NULL;

CREATE TABLE IF NOT EXISTS owner_actions (
    chain_id        BIGINT NOT NULL,
    block_number    BIGINT NOT NULL,
    tx_hash         TEXT NOT NULL,
    log_index       INT NOT NULL,
    ts              TIMESTAMPTZ NOT NULL,
    method          TEXT NOT NULL,
    selector        TEXT NOT NULL,
    args            JSONB NOT NULL DEFAULT '{}',
    PRIMARY KEY (chain_id, tx_hash, log_index)
);
CREATE INDEX IF NOT EXISTS owner_actions_chain_block ON owner_actions (chain_id, block_number);
CREATE INDEX IF NOT EXISTS owner_actions_chain_ts ON owner_actions (chain_id, ts);

CREATE TABLE IF NOT EXISTS constraint_sets (
    id              SERIAL PRIMARY KEY,
    chain_id        BIGINT NOT NULL,
    effective_block BIGINT NOT NULL,
    effective_at    TIMESTAMPTZ NOT NULL,
    constraints     JSONB NOT NULL DEFAULT '[]',
    source          TEXT NOT NULL,
    UNIQUE (chain_id, effective_block, source)
);

CREATE TABLE IF NOT EXISTS batch_reports (
    chain_id            BIGINT NOT NULL,
    block_number        BIGINT NOT NULL,
    batch_number        BIGINT NOT NULL,
    batch_ts            TIMESTAMPTZ NOT NULL,
    poster              TEXT NOT NULL,
    calldata_len        BIGINT NOT NULL DEFAULT 0,
    calldata_nonzero    BIGINT NOT NULL DEFAULT 0,
    extra_gas           BIGINT NOT NULL DEFAULT 0,
    l1_base_fee         NUMERIC(40,0) NOT NULL DEFAULT 0,
    gas_spent           BIGINT NOT NULL DEFAULT 0,
    wei_spent           NUMERIC(40,0) NOT NULL DEFAULT 0,
    PRIMARY KEY (chain_id, block_number)
);
CREATE INDEX IF NOT EXISTS batch_reports_chain_ts ON batch_reports (chain_id, batch_ts);

CREATE TABLE IF NOT EXISTS collector_state (
    chain_id    BIGINT NOT NULL,
    key         TEXT NOT NULL,
    value       TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, key)
);
