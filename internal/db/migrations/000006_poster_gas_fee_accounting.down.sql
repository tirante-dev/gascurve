-- An older binary cannot represent poster fees. Mark pricing and destination
-- history unknown before dropping the receipt-backed fields so a rollback
-- never presents the compute-only split as a complete two-way split.

UPDATE blocks
SET pricing_version = 0,
    constraint_bips = NULL,
    min_base_fee = NULL
WHERE pricing_version >= 1;

UPDATE buckets
SET pricing_version = 0,
    constraint_bips_end = NULL,
    min_base_fee = NULL,
    floor_fees_wei = NULL,
    surplus_fees_wei = NULL
WHERE pricing_version >= 1;

ALTER TABLE buckets
    DROP COLUMN poster_fees_wei,
    DROP COLUMN poster_gas;
ALTER TABLE blocks DROP COLUMN poster_gas;
