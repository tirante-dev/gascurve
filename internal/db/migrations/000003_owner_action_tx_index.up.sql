ALTER TABLE owner_actions
    ADD COLUMN IF NOT EXISTS tx_index INT;
