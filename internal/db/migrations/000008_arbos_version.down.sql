-- The version is recoverable from the chain, so dropping the columns loses
-- nothing that a rebuild cannot recompute from headers.

ALTER TABLE buckets
    DROP COLUMN arbos_version_min,
    DROP COLUMN arbos_version_max;

ALTER TABLE blocks
    DROP COLUMN arbos_version;
