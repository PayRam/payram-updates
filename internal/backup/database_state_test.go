package backup

import (
	"strings"
	"testing"
)

func TestMoneyRecordDigestSQL_CountsAMissingColumnAsEmpty(t *testing.T) {
	record := moneyRecords[0] // withdrawals
	all := map[string]bool{"withdrawals.id": true, "withdrawals.tx_hash": true, "withdrawals.signed_tx": true, "withdrawals.tx_hash_history": true, "withdrawals.status": true}
	older := map[string]bool{"withdrawals.id": true, "withdrawals.tx_hash": true, "withdrawals.status": true}

	full := moneyRecordDigestSQL(record, all)
	for _, want := range []string{"md5(signed_tx)", "tx_hash_history", "status IN ('initiated', 'sent', 'processed')", "ORDER BY id", "FROM public.withdrawals"} {
		if !strings.Contains(full, want) {
			t.Errorf("statement misses %q: %s", want, full)
		}
	}
	// A version without signed_tx and tx_hash_history digests its rows with
	// the same number of parts, so a later version adding those columns
	// (NULL in old rows) does not look like money moving.
	partial := moneyRecordDigestSQL(record, older)
	if strings.Count(partial, "|") != strings.Count(full, "|") {
		t.Fatalf("a missing column changes the row's shape:\n%s\n%s", partial, full)
	}
	if strings.Contains(partial, "signed_tx") {
		t.Fatalf("a missing column is referenced: %s", partial)
	}
	if got := moneyRecordDigestSQL(record, map[string]bool{}); got != "" {
		t.Fatalf("a table that does not exist must not be queried: %s", got)
	}
}

func TestMoneyRecordDigestSQL_SweepLocksOnly(t *testing.T) {
	var locks moneyRecord
	for _, record := range moneyRecords {
		if record.Table == "generic_data_stores" {
			locks = record
		}
	}
	columns := map[string]bool{"generic_data_stores.id": true, "generic_data_stores.type": true, "generic_data_stores.status": true, "generic_data_stores.data": true, "generic_data_stores.valid_till": true}
	statement := moneyRecordDigestSQL(locks, columns)
	if !strings.Contains(statement, "WHERE type LIKE '%sweep%'") || !strings.Contains(statement, "extract(epoch FROM valid_till)") {
		t.Fatalf("statement: %s", statement)
	}
	delete(columns, "generic_data_stores.type")
	if got := moneyRecordDigestSQL(locks, columns); got != "" {
		t.Fatalf("without the type column the filter cannot run, so the table is skipped: %s", got)
	}
}

func TestParseKeyedLines(t *testing.T) {
	got, err := parseKeyedLines("migrations=true\nschema=abc=def\n\nmoney:withdrawals=123\n")
	if err != nil {
		t.Fatal(err)
	}
	if got["migrations"] != "true" || got["schema"] != "abc=def" || got["money:withdrawals"] != "123" {
		t.Fatalf("got %v", got)
	}
	if _, err := parseKeyedLines("ERROR something\n"); err == nil {
		t.Fatal("a line without a key must be refused")
	}
}

func TestSplitNames(t *testing.T) {
	if got := splitNames("b\x1fa\x1f"); strings.Join(got, ",") != "a,b" {
		t.Fatalf("got %v", got)
	}
	if got := splitNames(""); got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want an empty list", got)
	}
}

func TestDescribeMoneyRecords(t *testing.T) {
	got := DescribeMoneyRecords([]string{"withdrawals", "deposit_addresses", "unknown"})
	if len(got) != 2 || !strings.Contains(got[0], "payout") || !strings.Contains(got[1], "payment address") {
		t.Fatalf("got %v", got)
	}
}
