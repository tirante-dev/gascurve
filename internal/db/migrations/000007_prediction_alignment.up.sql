-- ArbOS computes the base fee while processing block N and writes it into the
-- header of N+1, so the replay's output for N describes N+1. Stored rows kept
-- that output on N and compared it against N's own header, which measured how
-- far the fee moved between two blocks rather than how wrong the model is.
-- Move each row's pricing group onto the block it prices. The group is shifted
-- only across consecutive block numbers: the first block of every contiguous
-- run has no replayed parent, so its prediction is unknown rather than zero.

ALTER TABLE blocks
    ALTER COLUMN predicted_base_fee DROP NOT NULL,
    ALTER COLUMN predicted_base_fee DROP DEFAULT;

COMMENT ON COLUMN blocks.predicted_base_fee IS
    'the model''s fee for this block, computed while replaying its parent; NULL when the parent was not replayed';
COMMENT ON COLUMN blocks.exponent_bips IS
    'exponent that produced predicted_base_fee; meaningful only when that is not NULL, since 0 is also a valid exponent at the floor';
COMMENT ON COLUMN blocks.constraint_bips IS
    'per-constraint shares of exponent_bips; NULL when there is no prediction or none was recorded';

-- A row only takes its predecessor's group when that predecessor is the parent
-- it actually builds on. Adjacent numbers are not enough: a partially repaired
-- reorg can leave a row whose parent_hash names a block the stored predecessor
-- is not. Rows written before hashes were stored carry '' and cannot be
-- checked, matching the collector's own convention.
WITH shifted AS (
    SELECT chain_id,
           number,
           fee,
           exponent,
           bips,
           parent = number - 1
               AND (parent_hash = '' OR own_parent_hash = '' OR parent_hash = own_parent_hash) AS links
    FROM (
        SELECT chain_id,
               number,
               parent_hash                    AS own_parent_hash,
               LAG(predicted_base_fee) OVER w AS fee,
               LAG(exponent_bips) OVER w      AS exponent,
               LAG(constraint_bips) OVER w    AS bips,
               LAG(number) OVER w             AS parent,
               LAG(hash) OVER w               AS parent_hash
        FROM blocks
        WINDOW w AS (PARTITION BY chain_id ORDER BY number)
    ) neighbours
)
UPDATE blocks b
SET predicted_base_fee = CASE WHEN s.links THEN s.fee END,
    exponent_bips      = CASE WHEN s.links THEN s.exponent ELSE 0 END,
    constraint_bips    = CASE WHEN s.links THEN s.bips END
FROM shifted s
WHERE s.chain_id = b.chain_id AND s.number = b.number;

-- A bucket copies the pricing group of its last block, so the shift above left
-- every stored bucket describing the block before the one it names. Rejoin on
-- last_block to take the aligned group. A bucket whose last block has aged out
-- keeps a group that is one block stale and cannot be recovered.
UPDATE buckets b
SET exponent_end_bips   = bl.exponent_bips,
    constraint_bips_end = bl.constraint_bips
FROM blocks bl
WHERE bl.chain_id = b.chain_id AND bl.number = b.last_block;

-- The stored error is a maximum over every block of the bucket, most of which
-- have aged out, so unlike the end group it cannot be rejoined. It measured the
-- fee moving between blocks rather than the model, and reporting no error until
-- the buckets fold again is honest where keeping that number is not.
UPDATE buckets SET replay_error_bips = 0;
