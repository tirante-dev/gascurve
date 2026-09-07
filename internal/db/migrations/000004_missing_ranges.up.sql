-- Missing ranges are operational data, not a bounded checkpoint. Each range
-- keeps its recovery cursor, replay state and retry lifecycle independently so
-- overload cannot evict older gaps and a malformed replay state cannot erase
-- the record that history is incomplete.
CREATE TABLE missing_ranges (
    chain_id        BIGINT NOT NULL,
    from_block      BIGINT NOT NULL,
    to_block        BIGINT NOT NULL,
    detected_at     TIMESTAMPTZ NOT NULL,
    lifecycle       TEXT NOT NULL DEFAULT 'pending',
    reason          TEXT NOT NULL DEFAULT '',
    cursor          BIGINT NOT NULL DEFAULT 0,
    replay_state    JSONB,
    folded          BIGINT NOT NULL DEFAULT 0,
    retry_count     BIGINT NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ,
    next_retry_at   TIMESTAMPTZ,
    last_error      TEXT,
    predecessor_at  TIMESTAMPTZ,
    successor_at    TIMESTAMPTZ,
    cursor_at       TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, from_block),
    CHECK (from_block >= 0),
    CHECK (to_block >= from_block),
    CHECK (lifecycle IN ('pending', 'retrying', 'blocked')),
    CHECK (cursor = 0 OR (cursor >= from_block AND cursor <= to_block + 1)),
    CHECK (folded >= 0),
    CHECK (retry_count >= 0)
);

CREATE INDEX missing_ranges_recovery
    ON missing_ranges (chain_id, lifecycle, next_retry_at, from_block DESC);
