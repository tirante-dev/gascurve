-- Unknown values go back to the placeholders the earlier migrations used.
UPDATE blocks SET constraint_bips = '{}' WHERE constraint_bips IS NULL;
ALTER TABLE blocks ALTER COLUMN constraint_bips SET DEFAULT '{}';
ALTER TABLE blocks ALTER COLUMN constraint_bips SET NOT NULL;

UPDATE buckets SET constraint_bips_end = '{}' WHERE constraint_bips_end IS NULL;
UPDATE buckets SET floor_fees_wei = 0 WHERE floor_fees_wei IS NULL;
UPDATE buckets SET surplus_fees_wei = 0 WHERE surplus_fees_wei IS NULL;
UPDATE buckets SET base_fee_sum = base_fee_avg * blocks WHERE base_fee_sum IS NULL;
ALTER TABLE buckets ALTER COLUMN constraint_bips_end SET DEFAULT '{}';
ALTER TABLE buckets ALTER COLUMN constraint_bips_end SET NOT NULL;
ALTER TABLE buckets ALTER COLUMN floor_fees_wei SET DEFAULT 0;
ALTER TABLE buckets ALTER COLUMN floor_fees_wei SET NOT NULL;
ALTER TABLE buckets ALTER COLUMN surplus_fees_wei SET DEFAULT 0;
ALTER TABLE buckets ALTER COLUMN surplus_fees_wei SET NOT NULL;
ALTER TABLE buckets ALTER COLUMN base_fee_sum SET DEFAULT 0;
ALTER TABLE buckets ALTER COLUMN base_fee_sum SET NOT NULL;
