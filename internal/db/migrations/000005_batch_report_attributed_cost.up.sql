ALTER TABLE batch_reports
    ADD COLUMN report_version SMALLINT,
    ADD COLUMN arbos_version BIGINT,
    ADD COLUMN per_batch_gas_charge BIGINT,
    ADD COLUMN parent_gas_floor_per_token BIGINT,
    ADD COLUMN cost_calculation_version SMALLINT,
    ADD COLUMN attributed_gas_spent BIGINT,
    ADD COLUMN attributed_wei_spent NUMERIC(40,0);
