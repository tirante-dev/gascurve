-- Move each pricing group back onto the block whose replay produced it. The
-- last block of every contiguous run has no successor to take a group from. A
-- zero there would read as a real prediction to the older binary and report a
-- 10000 bip error, which would then poison any bucket rebuilt over it, so those
-- rows take their own base fee instead: the convention the older code already
-- used for a block it could not predict.

WITH shifted AS (
    SELECT chain_id,
           number,
           LEAD(predicted_base_fee) OVER w AS fee,
           LEAD(exponent_bips) OVER w      AS exponent,
           LEAD(constraint_bips) OVER w    AS bips,
           LEAD(number) OVER w             AS child
    FROM blocks
    WINDOW w AS (PARTITION BY chain_id ORDER BY number)
)
UPDATE blocks b
SET predicted_base_fee = CASE WHEN s.child = b.number + 1 THEN s.fee ELSE b.base_fee END,
    exponent_bips      = CASE WHEN s.child = b.number + 1 THEN s.exponent ELSE 0 END,
    constraint_bips    = CASE WHEN s.child = b.number + 1 THEN s.bips END
FROM shifted s
WHERE s.chain_id = b.chain_id AND s.number = b.number;

UPDATE blocks SET predicted_base_fee = base_fee WHERE predicted_base_fee IS NULL;

ALTER TABLE blocks
    ALTER COLUMN predicted_base_fee SET DEFAULT 0,
    ALTER COLUMN predicted_base_fee SET NOT NULL;

COMMENT ON COLUMN blocks.predicted_base_fee IS NULL;
COMMENT ON COLUMN blocks.exponent_bips IS NULL;
COMMENT ON COLUMN blocks.constraint_bips IS NULL;
