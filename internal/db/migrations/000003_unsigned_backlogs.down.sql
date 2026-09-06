-- Values above the BIGINT range are clamped on the way back.
CREATE OR REPLACE FUNCTION gascurve_signed_array(v NUMERIC[]) RETURNS BIGINT[]
LANGUAGE sql IMMUTABLE AS $$
    SELECT COALESCE(ARRAY(SELECT LEAST(x, 9223372036854775807)::BIGINT FROM unnest(v) AS x), '{}'::BIGINT[])
$$;

ALTER TABLE blocks ALTER COLUMN backlogs DROP DEFAULT;
ALTER TABLE blocks ALTER COLUMN backlogs TYPE BIGINT[] USING gascurve_signed_array(backlogs);
ALTER TABLE blocks ALTER COLUMN backlogs SET DEFAULT '{}';

ALTER TABLE buckets ALTER COLUMN backlogs_end DROP DEFAULT;
ALTER TABLE buckets ALTER COLUMN backlogs_end TYPE BIGINT[] USING gascurve_signed_array(backlogs_end);
ALTER TABLE buckets ALTER COLUMN backlogs_end SET DEFAULT '{}';

ALTER TABLE buckets ALTER COLUMN backlogs_max DROP DEFAULT;
ALTER TABLE buckets ALTER COLUMN backlogs_max TYPE BIGINT[] USING gascurve_signed_array(backlogs_max);
ALTER TABLE buckets ALTER COLUMN backlogs_max SET DEFAULT '{}';

DROP FUNCTION gascurve_signed_array(NUMERIC[]);
