-- Nitro applies the minimum L2 base fee only to compute gas. The poster-gas
-- part of a transaction is paid to the L1 pricer funds pool instead. Existing
-- rows predate the authoritative gasUsedForL1 receipt input, so their two-way
-- destination split is not trustworthy and becomes unknown until the history
-- is replayed from receipts. Total fees_wei is deliberately unchanged.

ALTER TABLE blocks
    ADD COLUMN poster_gas BIGINT,
    ADD CONSTRAINT blocks_poster_gas_valid
        CHECK (poster_gas IS NULL OR (poster_gas >= 0 AND poster_gas <= gas_used));

ALTER TABLE buckets
    ADD COLUMN poster_gas BIGINT,
    ADD COLUMN poster_fees_wei NUMERIC(40,0),
    ADD CONSTRAINT buckets_poster_gas_valid
        CHECK (poster_gas IS NULL OR (poster_gas >= 0 AND poster_gas <= gas_used));

COMMENT ON COLUMN blocks.poster_gas IS
    'sum of receipt gasUsedForL1 for the block; NULL when receipts were not collected';
COMMENT ON COLUMN buckets.poster_gas IS
    'sum of source-block poster_gas; NULL when any source block lacks receipts';
COMMENT ON COLUMN buckets.floor_fees_wei IS
    'min(min_base_fee, base_fee) times compute gas; NULL when the destination split is unknown';
COMMENT ON COLUMN buckets.surplus_fees_wei IS
    '(base_fee - min(min_base_fee, base_fee)) times compute gas; NULL when the destination split is unknown';
COMMENT ON COLUMN buckets.poster_fees_wei IS
    'base_fee times poster gas; NULL when the destination split is unknown';

UPDATE buckets
SET floor_fees_wei = NULL,
    surplus_fees_wei = NULL
WHERE pricing_version >= 1;
