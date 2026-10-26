//go:build integration

package backup

// Integration test of the database state a failed update's switch back is
// judged by, against a real PostgreSQL in Docker with the payram-core
// tables that record money (columns as in payram-core origin/main
// 230ca041). Run with:
//
//	go test -tags integration -run Integration -v ./internal/backup/

import (
	"context"
	"reflect"
	"testing"
)

// seedPayramMoneyTables creates the payram-core tables ReadDatabaseState
// reads, as an install before 3.1.4 has them (withdrawals has no
// tx_hash_history yet), with a payout sent before the update.
func seedPayramMoneyTables(t *testing.T, container string) {
	t.Helper()
	psqlIn(t, container, `
CREATE TABLE migrations (id varchar(255) PRIMARY KEY);
INSERT INTO migrations VALUES ('202601010000'), ('202605071600');
CREATE TABLE seeder_logs (name text PRIMARY KEY);
INSERT INTO seeder_logs VALUES ('configurations');
CREATE TABLE withdrawals (id bigserial PRIMARY KEY, created_at timestamptz, updated_at timestamptz, status varchar(200) NOT NULL DEFAULT 'pending',
  tx_hash varchar(200), signed_tx text, webhook_status text, retry_count int NOT NULL DEFAULT 0, amount numeric(38,18));
INSERT INTO withdrawals (status, tx_hash, signed_tx, amount) VALUES ('initiated', '0xaaa', 'f86b...signed', 25), ('pending', NULL, NULL, 7);
CREATE TABLE generic_data_stores (id bigserial PRIMARY KEY, type varchar(200) NOT NULL, valid_till timestamptz, data text, status varchar(20) DEFAULT 'active');
INSERT INTO generic_data_stores (type, valid_till, data) VALUES ('sweep_initiated', '2026-10-26T10:00:00Z', '{"address":"0xd1"}'), ('other', now(), 'x');
CREATE TABLE internal_blockchain_transactions (id bigserial PRIMARY KEY, tx_hash varchar(200) NOT NULL, status varchar(20) NOT NULL DEFAULT 'sent');
CREATE TABLE address_deployments (id bigserial PRIMARY KEY, address varchar(200) NOT NULL, transaction_hash varchar(200), status varchar(20) NOT NULL DEFAULT 'pending');
CREATE TABLE deposit_addresses (id bigserial PRIMARY KEY, address varchar(200) NOT NULL, status varchar(30) NOT NULL DEFAULT 'active');
INSERT INTO deposit_addresses (address) VALUES ('0xd1'), ('0xd2');
CREATE TABLE api_keys (id bigserial PRIMARY KEY, key text);`)
}

func TestIntegration_DatabaseStateTellsMigrationsFromMoney(t *testing.T) {
	container := startPostgres(t, itImage, false)
	mgr, upgradeBackups := newIntegrationManager(t, container)
	ctx := context.Background()
	waitForDatabase(t, mgr, container)
	seedPayramMoneyTables(t, container)

	read := func() map[string]any {
		t.Helper()
		state, err := upgradeBackups.ReadDatabaseState(ctx, container)
		if err != nil {
			t.Fatalf("ReadDatabaseState: %v", err)
		}
		return map[string]any{"migrations": state.MigrationIDs, "seeds": state.SeedNames, "schema": state.SchemaDigest, "money": state.MoneyRecords}
	}
	before := read()
	if got := before["migrations"]; !reflect.DeepEqual(got, []string{"202601010000", "202605071600"}) {
		t.Fatalf("migrations = %v", got)
	}
	if money := before["money"].(map[string]string); money["withdrawals"] == "" || money["deposit_addresses"] == "" || money["generic_data_stores"] == "" {
		t.Fatalf("money records = %v", money)
	}
	if _, ok := before["money"].(map[string]string)["internal_blockchain_transactions"]; ok {
		t.Fatal("an empty table must be left out (it equals a table that does not exist)")
	}

	t.Run("bookkeeping on payouts sent before the backup is not money moving", func(t *testing.T) {
		psqlIn(t, container, `UPDATE withdrawals SET status = 'sent', webhook_status = 'delivered', retry_count = retry_count + 1, updated_at = now() WHERE tx_hash = '0xaaa';
UPDATE deposit_addresses SET status = 'inactive' WHERE address = '0xd2';
UPDATE generic_data_stores SET data = 'changed' WHERE type = 'other';`)
		if got := read()["money"]; !reflect.DeepEqual(got, before["money"]) {
			t.Fatalf("money records changed by bookkeeping:\n got %v\nwant %v", got, before["money"])
		}
	})

	t.Run("a migration that adds a column and backfills nothing is structure, not money", func(t *testing.T) {
		psqlIn(t, container, `ALTER TABLE withdrawals ADD COLUMN tx_hash_history text; ALTER TABLE api_keys ADD COLUMN key_hash text;
INSERT INTO migrations VALUES ('202606131200');`)
		now := read()
		if reflect.DeepEqual(now["migrations"], before["migrations"]) || now["schema"] == before["schema"] {
			t.Fatal("the migration was not seen")
		}
		if !reflect.DeepEqual(now["money"], before["money"]) {
			t.Fatalf("a new NULL column looks like money moving:\n got %v\nwant %v", now["money"], before["money"])
		}
	})

	for _, tc := range []struct {
		name, table, sql string
	}{
		{"a payout request accepted", "withdrawals", `INSERT INTO withdrawals (status, amount) VALUES ('pending', 3)`},
		{"a payout re-issued under a new hash", "withdrawals", `UPDATE withdrawals SET tx_hash = '0xbbb', tx_hash_history = '["0xaaa","0xbbb"]' WHERE tx_hash = '0xaaa'`},
		{"a pending payout signed (on the wire, hash not saved yet)", "withdrawals", `UPDATE withdrawals SET signed_tx = 'f86c...', status = 'initiated' WHERE status = 'pending'`},
		{"a sweep lock extended after an unclear broadcast", "generic_data_stores", `UPDATE generic_data_stores SET valid_till = valid_till + interval '20 minutes' WHERE type = 'sweep_initiated'`},
		{"gas sent to a deposit address", "internal_blockchain_transactions", `INSERT INTO internal_blockchain_transactions (tx_hash, status) VALUES ('0xgas', 'pending')`},
		{"a deposit wallet deployed", "address_deployments", `INSERT INTO address_deployments (address, transaction_hash, status) VALUES ('0xd3', '0xdeploy', 'broadcasted')`},
		{"a payment address handed out", "deposit_addresses", `INSERT INTO deposit_addresses (address) VALUES ('0xd9')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was := read()["money"].(map[string]string)
			psqlIn(t, container, tc.sql)
			now := read()["money"].(map[string]string)
			for table := range moneyTables() {
				if changed := now[table] != was[table]; changed != (table == tc.table) {
					t.Errorf("%s changed=%v (was %q now %q)", table, changed, was[table], now[table])
				}
			}
		})
	}
}

// TestIntegration_SwitchBackRestoreGoesIntoTheBackupsDatabase: the restore a
// failed update makes (RestoreOptions.Database from ResolveDatabase) puts the
// database back to what the backup and its recorded state hold.
func TestIntegration_SwitchBackRestoreGoesIntoTheBackupsDatabase(t *testing.T) {
	container := startPostgres(t, itImage, false)
	mgr, upgradeBackups := newIntegrationManager(t, container)
	ctx := context.Background()
	waitForDatabase(t, mgr, container)
	seedPayramMoneyTables(t, container)

	atBackup, err := upgradeBackups.ReadDatabaseState(ctx, container)
	if err != nil {
		t.Fatal(err)
	}
	result := upgradeBackups.ExecuteBackup(ctx, container, BackupMeta{FromVersion: "3.1.3", TargetVersion: "3.10.0"})
	if !result.Success {
		t.Fatalf("backup: %s", result.ErrorMessage)
	}

	// The new version migrates: 3.1.4's EncryptAPIKeys and the column it adds.
	psqlIn(t, container, `ALTER TABLE api_keys ADD COLUMN key_hash text; INSERT INTO migrations VALUES ('202606131200');`)

	target, err := upgradeBackups.ResolveDatabase(ctx, container)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := mgr.RestoreBackup(ctx, result.Path, RestoreOptions{Confirmed: true, ContainerLockHeld: true, Database: target})
	if err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if restored.SafetyBackupPath == "" {
		t.Error("no safety backup of the migrated database")
	}
	now, err := upgradeBackups.ReadDatabaseState(ctx, container)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(now.MigrationIDs, atBackup.MigrationIDs) || now.SchemaDigest != atBackup.SchemaDigest || !reflect.DeepEqual(now.MoneyRecords, atBackup.MoneyRecords) {
		t.Fatalf("after the restore the database is not the backup's:\n now %+v\nwant %+v", now, atBackup)
	}
}

func moneyTables() map[string]bool {
	tables := map[string]bool{}
	for _, record := range moneyRecords {
		tables[record.Table] = true
	}
	return tables
}
