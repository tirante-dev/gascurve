-- Preserve the ranges in the legacy checkpoint shape before removing the
-- table. Older collectors can read the basic range, cursor and replay state,
-- although they do not understand retry lifecycle or timestamp bounds.
WITH checkpoints AS (
    SELECT chain_id,
           jsonb_agg(
               jsonb_strip_nulls(jsonb_build_object(
                   'from', from_block,
                   'to', to_block,
                   'at', to_char(detected_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
                   'next', NULLIF(cursor, 0),
                   'state', replay_state,
                   'folded', NULLIF(folded, 0),
                   'reason', CASE WHEN lifecycle = 'blocked' THEN NULLIF(reason, '') END
               )) ORDER BY from_block
           )::TEXT AS value
    FROM missing_ranges
    GROUP BY chain_id
)
INSERT INTO collector_state (chain_id, key, value, updated_at)
SELECT chain_id, 'holes', value, now()
FROM checkpoints
ON CONFLICT (chain_id, key) DO UPDATE
SET value = EXCLUDED.value, updated_at = now();

DROP TABLE IF EXISTS missing_ranges;
