-- Nitro records the ArbOS version that produced a block in bytes 16 through 23
-- of the header mix digest, so the version in force at any historical block is
-- already in every header the collector fetches. Storing it makes the pricing
-- model behind a row a fact rather than an inference from the constraint set,
-- and lets a bucket say whether it spans a version boundary the replay cannot
-- vouch for. NULL means the row was written before this column existed, never
-- version zero.

ALTER TABLE blocks
    ADD COLUMN arbos_version INT NULL;

ALTER TABLE buckets
    ADD COLUMN arbos_version_min INT NULL,
    ADD COLUMN arbos_version_max INT NULL;

COMMENT ON COLUMN blocks.arbos_version IS
    'ArbOS version in force at this block, from the header mix digest; NULL for history recorded before it was stored';
COMMENT ON COLUMN buckets.arbos_version_min IS
    'lowest ArbOS version of the blocks folded in; NULL when any of them did not record one';
COMMENT ON COLUMN buckets.arbos_version_max IS
    'highest ArbOS version of the blocks folded in; differs from the minimum exactly when the bucket spans an upgrade';
