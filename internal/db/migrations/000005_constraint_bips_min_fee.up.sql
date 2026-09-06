-- Start-of-block per-constraint exponents, the minimum base fee in force,
-- and the split of fees into the floor (gas * min fee) and the surplus.
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS constraint_bips BIGINT[] NOT NULL DEFAULT '{}';
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS min_base_fee NUMERIC(40,0) NOT NULL DEFAULT 0;

ALTER TABLE buckets ADD COLUMN IF NOT EXISTS constraint_bips_end BIGINT[] NOT NULL DEFAULT '{}';
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS min_base_fee NUMERIC(40,0) NOT NULL DEFAULT 0;
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS floor_fees_wei NUMERIC(40,0) NOT NULL DEFAULT 0;
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS surplus_fees_wei NUMERIC(40,0) NOT NULL DEFAULT 0;
