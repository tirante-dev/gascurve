-- Backlogs are uint64 in the pricer and saturate at 2^64-1, which does not
-- fit BIGINT. Store them as NUMERIC(20,0) arrays. Values that were stored
-- through a wrapping int64 cast are recovered by adding 2^64. A USING
-- expression cannot hold a subquery, so the element conversion goes
-- through a temporary function.
CREATE OR REPLACE FUNCTION gascurve_unsigned_array(v BIGINT[]) RETURNS NUMERIC(20,0)[]
LANGUAGE sql IMMUTABLE AS $$
    SELECT COALESCE(ARRAY(SELECT CASE WHEN x < 0 THEN x::NUMERIC + 18446744073709551616 ELSE x::NUMERIC END FROM unnest(v) AS x), '{}'::NUMERIC[])::NUMERIC(20,0)[]
$$;

ALTER TABLE blocks ALTER COLUMN backlogs DROP DEFAULT;
ALTER TABLE blocks ALTER COLUMN backlogs TYPE NUMERIC(20,0)[] USING gascurve_unsigned_array(backlogs);
ALTER TABLE blocks ALTER COLUMN backlogs SET DEFAULT '{}'::NUMERIC(20,0)[];

ALTER TABLE buckets ALTER COLUMN backlogs_end DROP DEFAULT;
ALTER TABLE buckets ALTER COLUMN backlogs_end TYPE NUMERIC(20,0)[] USING gascurve_unsigned_array(backlogs_end);
ALTER TABLE buckets ALTER COLUMN backlogs_end SET DEFAULT '{}'::NUMERIC(20,0)[];

ALTER TABLE buckets ALTER COLUMN backlogs_max DROP DEFAULT;
ALTER TABLE buckets ALTER COLUMN backlogs_max TYPE NUMERIC(20,0)[] USING gascurve_unsigned_array(backlogs_max);
ALTER TABLE buckets ALTER COLUMN backlogs_max SET DEFAULT '{}'::NUMERIC(20,0)[];

DROP FUNCTION gascurve_unsigned_array(BIGINT[]);
