CREATE INDEX CONCURRENTLY state_samples_chain_block
    ON state_samples (chain_id, block_number DESC, sampled_at DESC);
