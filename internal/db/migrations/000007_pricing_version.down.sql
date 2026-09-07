-- Unknown floors go back to the zero placeholder the earlier migrations
-- used, and the provenance column goes away.
UPDATE blocks SET min_base_fee = 0 WHERE min_base_fee IS NULL;
UPDATE buckets SET min_base_fee = 0 WHERE min_base_fee IS NULL;
ALTER TABLE blocks ALTER COLUMN min_base_fee SET DEFAULT 0;
ALTER TABLE blocks ALTER COLUMN min_base_fee SET NOT NULL;
ALTER TABLE buckets ALTER COLUMN min_base_fee SET DEFAULT 0;
ALTER TABLE buckets ALTER COLUMN min_base_fee SET NOT NULL;

ALTER TABLE blocks DROP COLUMN IF EXISTS pricing_version;
ALTER TABLE buckets DROP COLUMN IF EXISTS pricing_version;
