ALTER TABLE batch_reports
    DROP COLUMN IF EXISTS attributed_wei_spent,
    DROP COLUMN IF EXISTS attributed_gas_spent,
    DROP COLUMN IF EXISTS cost_calculation_version,
    DROP COLUMN IF EXISTS parent_gas_floor_per_token,
    DROP COLUMN IF EXISTS per_batch_gas_charge,
    DROP COLUMN IF EXISTS arbos_version,
    DROP COLUMN IF EXISTS report_version;
