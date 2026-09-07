//go:build integration

package db

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

func TestIntegrationUpgradeFromProductionSchema(t *testing.T) {
	p := openIntegration(t)
	freshVersion, freshShape := integrationSchemaState(t, p)

	emptyIntegrationSchema(t, p)
	m, err := NewMigrator(p.DB().DB)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.m.Migrate(productionSchemaVersion); err != nil {
		t.Fatalf("install production schema %s at version %d: %v", productionRelease, productionSchemaVersion, err)
	}
	if version, dirty, err := m.Version(); err != nil || dirty || version != productionSchemaVersion {
		t.Fatalf("production schema version = %d dirty=%v err=%v", version, dirty, err)
	}

	ctx := context.Background()
	if _, err := p.DB().ExecContext(ctx,
		"INSERT INTO networks (chain_id, name, display_name, explorer_url) "+
			"VALUES (900000001, 'migration-test', 'Migration Test', 'https://example.com'); "+
			"INSERT INTO blocks (chain_id, number, ts, gas_used, base_fee, backlogs, pricing_version) "+
			"VALUES (900000001, 42, '2026-01-01T00:00:00Z', 123, 456, '{7,8}', 1); "+
			"INSERT INTO batch_reports (chain_id, block_number, batch_number, batch_ts, poster, gas_spent, wei_spent) "+
			"VALUES (900000001, 42, 7, '2026-01-01T00:00:00Z', '0xposter', 1234, 5678);"); err != nil {
		t.Fatal(err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("upgrade production schema %s to head: %v", productionRelease, err)
	}
	upgradedVersion, upgradedShape := integrationSchemaState(t, p)
	if upgradedVersion != freshVersion {
		t.Fatalf("upgraded version = %d, fresh version = %d", upgradedVersion, freshVersion)
	}
	if !reflect.DeepEqual(upgradedShape, freshShape) {
		t.Fatalf("upgraded schema differs from fresh schema\nupgraded:\n%s\nfresh:\n%s",
			strings.Join(upgradedShape, "\n"), strings.Join(freshShape, "\n"))
	}

	var displayName, baseFee, backlogs string
	if err := p.DB().QueryRowContext(ctx,
		"SELECT n.display_name, b.base_fee::text, b.backlogs::text "+
			"FROM networks n JOIN blocks b USING (chain_id) "+
			"WHERE n.chain_id = 900000001 AND b.number = 42").
		Scan(&displayName, &baseFee, &backlogs); err != nil {
		t.Fatal(err)
	}
	if displayName != "Migration Test" || baseFee != "456" || backlogs != "{7,8}" {
		t.Fatalf("production data changed during upgrade: display_name=%q base_fee=%q backlogs=%q",
			displayName, baseFee, backlogs)
	}

	var legacyGas int64
	var legacyWei string
	var reportVersion, arbosVersion, perBatchGas, parentFloor, calculationVersion, attributedGas sql.NullInt64
	var attributedWei sql.NullString
	if err := p.DB().QueryRowContext(ctx,
		"SELECT gas_spent, wei_spent::text, report_version, arbos_version, per_batch_gas_charge, "+
			"parent_gas_floor_per_token, cost_calculation_version, attributed_gas_spent, attributed_wei_spent::text "+
			"FROM batch_reports WHERE chain_id = 900000001 AND block_number = 42").
		Scan(&legacyGas, &legacyWei, &reportVersion, &arbosVersion, &perBatchGas, &parentFloor, &calculationVersion, &attributedGas, &attributedWei); err != nil {
		t.Fatal(err)
	}
	if legacyGas != 1234 || legacyWei != "5678" || reportVersion.Valid || arbosVersion.Valid || perBatchGas.Valid || parentFloor.Valid || calculationVersion.Valid || attributedGas.Valid || attributedWei.Valid {
		t.Fatalf("legacy batch report changed during upgrade: gas=%d wei=%s metadata=%v/%v/%v/%v/%v attributed=%v/%v",
			legacyGas, legacyWei, reportVersion, arbosVersion, perBatchGas, parentFloor, calculationVersion, attributedGas, attributedWei)
	}
}

func integrationSchemaState(t *testing.T, p *Postgres) (uint, []string) {
	t.Helper()
	m, err := NewMigrator(p.DB().DB)
	if err != nil {
		t.Fatal(err)
	}
	version, dirty, err := m.Version()
	if err != nil || dirty {
		t.Fatalf("schema version = %d dirty=%v err=%v", version, dirty, err)
	}

	rows, err := p.DB().Query(
		"SELECT definition FROM (" +
			"SELECT format('column|%s|%s|%s|%s|%s|%s', table_name, ordinal_position, column_name, data_type, is_nullable, coalesce(column_default, '')) AS definition " +
			"FROM information_schema.columns WHERE table_schema = 'public' " +
			"UNION ALL " +
			"SELECT format('constraint|%s|%s|%s', conrelid::regclass::text, conname, pg_get_constraintdef(oid)) " +
			"FROM pg_constraint WHERE connamespace = 'public'::regnamespace " +
			"UNION ALL " +
			"SELECT format('index|%s|%s|%s', tablename, indexname, indexdef) " +
			"FROM pg_indexes WHERE schemaname = 'public'" +
			") schema_objects ORDER BY definition")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var shape []string
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			t.Fatal(err)
		}
		shape = append(shape, definition)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(shape) == 0 {
		t.Fatal("schema has no objects")
	}
	return version, shape
}
