-- Exact running sum so the average never re-rounds a rounded average.
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS base_fee_sum NUMERIC(40,0) NOT NULL DEFAULT 0;
UPDATE buckets SET base_fee_sum = base_fee_avg * blocks WHERE base_fee_sum = 0 AND blocks > 0;
