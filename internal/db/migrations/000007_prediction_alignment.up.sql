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

WITH shifted AS (
    SELECT chain_id,
           number,
           LAG(predicted_base_fee) OVER w AS fee,
           LAG(exponent_bips) OVER w      AS exponent,
           LAG(constraint_bips) OVER w    AS bips,
           LAG(number) OVER w             AS parent
    FROM blocks
    WINDOW w AS (PARTITION BY chain_id ORDER BY number)
)
UPDATE blocks b
SET predicted_base_fee = CASE WHEN s.parent = b.number - 1 THEN s.fee END,
    exponent_bips      = CASE WHEN s.parent = b.number - 1 THEN s.exponent ELSE 0 END,
    constraint_bips    = CASE WHEN s.parent = b.number - 1 THEN s.bips END
FROM shifted s
WHERE s.chain_id = b.chain_id AND s.number = b.number;

-- Stored buckets folded the misaligned per-block error, and the blocks behind
-- the older ones are no longer retained, so the aggregate cannot be recomputed.
-- Clearing it reports no error until the buckets are folded again, which is
-- honest where keeping a number that measured the wrong thing is not.
UPDATE buckets SET replay_error_bips = 0;
