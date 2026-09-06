-- Block ancestry for reorg detection: the collector compares a new head's
-- parent hash with the stored head and rewinds to the common ancestor.
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS hash TEXT NOT NULL DEFAULT '';
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS parent_hash TEXT NOT NULL DEFAULT '';
