ALTER TABLE buckets DROP COLUMN IF EXISTS surplus_fees_wei;
ALTER TABLE buckets DROP COLUMN IF EXISTS floor_fees_wei;
ALTER TABLE buckets DROP COLUMN IF EXISTS min_base_fee;
ALTER TABLE buckets DROP COLUMN IF EXISTS constraint_bips_end;
ALTER TABLE blocks DROP COLUMN IF EXISTS min_base_fee;
ALTER TABLE blocks DROP COLUMN IF EXISTS constraint_bips;
