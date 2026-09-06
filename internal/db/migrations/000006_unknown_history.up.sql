-- Values that 000004 and 000005 filled in for rows written before those
-- columns existed are placeholders, not history: a base fee sum rebuilt
-- from a rounded average, empty exponent arrays and zero fee splits. They
-- become NULL (unknown) so the API can report them as such instead of
-- serving numbers that look exact; rows written from now on always carry
-- real values. The average is kept: where the sum equals the rounded
-- average times the block count the average is exact anyway.
ALTER TABLE buckets ALTER COLUMN base_fee_sum DROP NOT NULL;
ALTER TABLE buckets ALTER COLUMN base_fee_sum DROP DEFAULT;
UPDATE buckets SET base_fee_sum = NULL WHERE blocks > 1 AND base_fee_sum = base_fee_avg * blocks;

ALTER TABLE buckets ALTER COLUMN constraint_bips_end DROP NOT NULL;
ALTER TABLE buckets ALTER COLUMN constraint_bips_end DROP DEFAULT;
ALTER TABLE buckets ALTER COLUMN floor_fees_wei DROP NOT NULL;
ALTER TABLE buckets ALTER COLUMN floor_fees_wei DROP DEFAULT;
ALTER TABLE buckets ALTER COLUMN surplus_fees_wei DROP NOT NULL;
ALTER TABLE buckets ALTER COLUMN surplus_fees_wei DROP DEFAULT;
UPDATE buckets SET constraint_bips_end = NULL, floor_fees_wei = NULL, surplus_fees_wei = NULL;

ALTER TABLE blocks ALTER COLUMN constraint_bips DROP NOT NULL;
ALTER TABLE blocks ALTER COLUMN constraint_bips DROP DEFAULT;
UPDATE blocks SET constraint_bips = NULL;
