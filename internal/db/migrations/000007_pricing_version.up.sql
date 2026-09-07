-- Provenance for the pricing breakdown. Version 1 is a row written with
-- the full breakdown (per-constraint exponents, the minimum base fee in
-- force and therefore an exact fee split); version 0 is history from
-- before those columns existed, whose values were never recorded.
--
-- The values migration 000006 set to NULL cannot be recovered: it nulled
-- every constraint-bips and fee-split column unconditionally, including
-- rows written after 000005 with real values. It only ever ran on a
-- development database, so no deployment lost history to it, and this
-- migration marks what is unknown rather than inventing replacements.
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS pricing_version SMALLINT NOT NULL DEFAULT 1;
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS pricing_version SMALLINT NOT NULL DEFAULT 1;

UPDATE blocks SET pricing_version = 0 WHERE constraint_bips IS NULL;
UPDATE buckets SET pricing_version = 0 WHERE constraint_bips_end IS NULL;

-- The floor in force is unknown for that history too: migration 000005
-- filled it with zero, which reads as an authoritative floor of nothing
-- and made every rebuilt fee split look exact. NULL says unknown.
ALTER TABLE blocks ALTER COLUMN min_base_fee DROP NOT NULL;
ALTER TABLE blocks ALTER COLUMN min_base_fee DROP DEFAULT;
ALTER TABLE buckets ALTER COLUMN min_base_fee DROP NOT NULL;
ALTER TABLE buckets ALTER COLUMN min_base_fee DROP DEFAULT;

UPDATE blocks SET min_base_fee = NULL WHERE pricing_version = 0;
UPDATE buckets SET min_base_fee = NULL WHERE pricing_version = 0;
