package backup

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/jobs"
)

// databaseStateTimeout bounds ReadDatabaseState.
const databaseStateTimeout = 5 * time.Minute

// EmptyRecordsDigest is the digest of a table with no money records (and of
// a table that does not exist).
const EmptyRecordsDigest = "d41d8cd98f00b204e9800998ecf8427e" // md5("")

// moneyRecord is one table where PayRam records money it moved or may move.
type moneyRecord struct {
	Table string
	// What says in plain words what a new or changed row means.
	What string
	// Columns are the columns whose values a row's digest covers, as SQL
	// expressions of the column (named first). A column the table does not
	// have counts as empty, so a column a newer version added (NULL in old
	// rows) does not change the digest of rows it never touched.
	Columns []recordColumn
	// Where limits the rows (a column the filter needs must exist).
	Where       string
	WhereColumn string
}

type recordColumn struct {
	Name string
	Expr string // SQL using Name; "" means the column itself
}

// moneyRecords are, in payram-core (checked at origin/main 230ca041), every
// place PayRam records an on-chain transaction it signs or broadcasts, and the
// records whose loss would let money go astray after a restore. Each digest
// covers what changes only when PayRam does something with money, not the
// bookkeeping that follows (confirmations, webhook status, retries):
//   - withdrawals: every payout. A row is written before it is signed and
//     marked "initiated" before it is broadcast (tx_hash, signed_tx); a
//     replacement (replace-by-fee, re-issue) changes tx_hash and adds to
//     tx_hash_history. All rows count, so a payout request accepted since the
//     backup also counts: a restore would drop it silently.
//   - generic_data_stores, type sweep_initiated: the lock written before an
//     EVM or Tron sweep is broadcast; the hash is merged into data after the
//     broadcast, and an unclear broadcast only extends valid_till.
//   - internal_blockchain_transactions: gas sent to deposit addresses and
//     approval broadcasts, written (with the hash) before the broadcast.
//   - address_deployments: deposit wallets deployed by the hot wallet.
//   - sweep_transactions: Bitcoin sweeps (signed and broadcast by the
//     merchant's app, which reports the hash back).
//   - deposit_addresses: payment addresses handed out to customers. After a
//     restore PayRam would not watch an address handed out since the backup,
//     and could hand it to another customer.
//
// The sweeps table is not here: its rows are sweeps the listener detected on
// chain, found again by the re-read of the blockchain after a restore; the
// broadcasts behind them are in the tables above.
var moneyRecords = []moneyRecord{
	{
		Table: "withdrawals",
		What:  "requested, signed or sent a payout",
		Columns: []recordColumn{
			{Name: "tx_hash"},
			{Name: "signed_tx", Expr: "md5(signed_tx)"},
			{Name: "tx_hash_history"},
			{Name: "status", Expr: "(status IN ('initiated', 'sent', 'processed'))::text"},
		},
	},
	{
		Table:       "generic_data_stores",
		What:        "started a sweep",
		Columns:     []recordColumn{{Name: "status"}, {Name: "data", Expr: "md5(data)"}, {Name: "valid_till", Expr: "extract(epoch FROM valid_till)"}},
		Where:       "type LIKE '%sweep%'",
		WhereColumn: "type",
	},
	{
		Table:   "internal_blockchain_transactions",
		What:    "sent gas to a deposit address or broadcast a sweep approval",
		Columns: []recordColumn{{Name: "tx_hash"}},
	},
	{
		Table:   "address_deployments",
		What:    "deployed a deposit wallet",
		Columns: []recordColumn{{Name: "transaction_hash"}},
	},
	{
		Table: "sweep_transactions",
		What:  "recorded a Bitcoin sweep",
		Columns: []recordColumn{
			{Name: "tx_hash"},
			{Name: "signed_payload", Expr: "md5(signed_payload)"},
			{Name: "previous_tx_hashes", Expr: "md5(previous_tx_hashes::text)"},
		},
	},
	{
		Table:   "deposit_addresses",
		What:    "handed a payment address to a customer",
		Columns: []recordColumn{{Name: "address"}},
	},
}

// DescribeMoneyRecords says in plain words what a change in each of tables
// means ("requested, signed or sent a payout", ...), in the order given.
func DescribeMoneyRecords(tables []string) []string {
	var out []string
	for _, table := range tables {
		for _, record := range moneyRecords {
			if record.Table == table {
				out = append(out, record.What)
			}
		}
	}
	return out
}

// ReadDatabaseState reads, through containerName and by the rules of the
// pre-update backup, what decides whether PayRam's database suits a version:
// its applied migrations and seeds, a digest of its tables and columns, and
// a digest per table of its money records (see moneyRecords). It only reads,
// in read-only transactions.
func (e *ContainerBackupExecutor) ReadDatabaseState(ctx context.Context, containerName string) (*jobs.DatabaseState, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseStateTimeout)
	defer cancel()
	target, err := e.ResolveDatabase(ctx, containerName)
	if err != nil {
		return nil, err
	}
	query := func(statements ...string) (map[string]string, error) {
		out, err := target.Executor.Query(ctx, target.Context, statements...)
		if err != nil {
			return nil, err
		}
		return parseKeyedLines(out)
	}

	tables := make([]string, 0, len(moneyRecords))
	for _, record := range moneyRecords {
		tables = append(tables, "'"+record.Table+"'")
	}
	shape, err := query(
		"SELECT 'migrations=' || (to_regclass('public.migrations') IS NOT NULL)::text",
		"SELECT 'seeds=' || (to_regclass('public.seeder_logs') IS NOT NULL)::text",
		"SELECT 'schema=' || md5(coalesce(string_agg(table_name || '.' || column_name || ' ' || udt_name || ' ' || is_nullable, ',' ORDER BY table_name, column_name), '')) FROM information_schema.columns WHERE table_schema = 'public'",
		"SELECT 'columns=' || coalesce(string_agg(table_name || '.' || column_name, ',' ORDER BY table_name, column_name), '') FROM information_schema.columns WHERE table_schema = 'public' AND table_name IN ("+strings.Join(tables, ", ")+")",
	)
	if err != nil {
		return nil, fmt.Errorf("could not read the database's structure: %w", err)
	}
	for _, key := range []string{"migrations", "seeds", "schema", "columns"} {
		if _, ok := shape[key]; !ok {
			return nil, fmt.Errorf("unexpected answer from the database (no %s line)", key)
		}
	}
	columns := map[string]bool{}
	for _, column := range strings.Split(shape["columns"], ",") {
		if column != "" {
			columns[column] = true
		}
	}

	var statements []string
	if shape["migrations"] == "true" {
		statements = append(statements, "SELECT 'migration_ids=' || coalesce(string_agg(id::text, chr(31) ORDER BY id), '') FROM public.migrations")
	}
	if shape["seeds"] == "true" {
		statements = append(statements, "SELECT 'seed_names=' || coalesce(string_agg(name::text, chr(31) ORDER BY name), '') FROM public.seeder_logs")
	}
	for _, record := range moneyRecords {
		if statement := moneyRecordDigestSQL(record, columns); statement != "" {
			statements = append(statements, statement)
		}
	}

	state := &jobs.DatabaseState{
		ReadAt:       time.Now().UTC(),
		SchemaDigest: shape["schema"],
		MigrationIDs: []string{},
		SeedNames:    []string{},
		MoneyRecords: map[string]string{},
	}
	if len(statements) == 0 {
		return state, nil
	}
	values, err := query(statements...)
	if err != nil {
		return nil, fmt.Errorf("could not read the database's migrations and money records: %w", err)
	}
	state.MigrationIDs = splitNames(values["migration_ids"])
	state.SeedNames = splitNames(values["seed_names"])
	for _, record := range moneyRecords {
		if digest, ok := values["money:"+record.Table]; ok && digest != EmptyRecordsDigest {
			state.MoneyRecords[record.Table] = digest
		}
	}
	return state, nil
}

// moneyRecordDigestSQL builds the statement that digests one money table:
// md5 over every row's md5 of its id and covered columns, in id order. ""
// when the table does not exist (it has no id column).
func moneyRecordDigestSQL(record moneyRecord, columns map[string]bool) string {
	if !columns[record.Table+".id"] {
		return ""
	}
	parts := []string{"id::text"}
	for _, column := range record.Columns {
		if !columns[record.Table+"."+column.Name] {
			parts = append(parts, "''")
			continue
		}
		expr := column.Expr
		if expr == "" {
			expr = column.Name
		}
		parts = append(parts, "coalesce(("+expr+")::text, '')")
	}
	where := ""
	if record.Where != "" {
		if !columns[record.Table+"."+record.WhereColumn] {
			return ""
		}
		where = " WHERE " + record.Where
	}
	return fmt.Sprintf("SELECT 'money:%s=' || md5(coalesce(string_agg(md5(%s), '' ORDER BY id), '')) FROM public.%s%s",
		record.Table, strings.Join(parts, " || '|' || "), record.Table, where)
}

// parseKeyedLines reads "key=value" lines.
func parseKeyedLines(out string) (map[string]string, error) {
	values := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("unexpected answer from the database: %q", truncateLine(line))
		}
		values[key] = value
	}
	return values, nil
}

func splitNames(joined string) []string {
	names := []string{}
	for _, name := range strings.Split(joined, "\x1f") {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func truncateLine(line string) string {
	if len(line) > 200 {
		return line[:200] + "..."
	}
	return line
}
