package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

func openFileDB(t *testing.T, path string) *DB {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return database
}

func columnSet(t *testing.T, d *DB, table string) map[string]bool {
	t.Helper()
	rows, err := d.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		cols[name] = true
	}
	return cols
}

func latestVersion() int { return migrations[len(migrations)-1].version }

func TestMigrationListMatchesFiles(t *testing.T) {
	referenced := map[string]bool{}
	for i, m := range migrations {
		if m.version != i+1 {
			t.Errorf("migration %q has version %d, want %d (versions must be sequential from 1)", m.name, m.version, i+1)
		}
		if (m.file == "") == (m.up == nil) {
			t.Errorf("migration %d must have exactly one of file or up", m.version)
		}
		if m.file != "" {
			if referenced[m.file] {
				t.Errorf("migration file %s is referenced twice", m.file)
			}
			referenced[m.file] = true
		}
	}
	files, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !referenced[filepath.Base(f)] {
			t.Errorf("%s is embedded but not listed in migrations", f)
		}
	}
}

func TestMigrateFreshDatabase(t *testing.T) {
	d := openFileDB(t, filepath.Join(t.TempDir(), "fresh.db"))
	defer d.Close()

	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != latestVersion() {
		t.Fatalf("schema version = %d, want %d", v, latestVersion())
	}
	cols := columnSet(t, d, "request_logs")
	for _, c := range []string{"requested_model", "prompt_cost_usd", "completion_cost_usd", "pruned_bytes", "pruned_tokens"} {
		if !cols[c] {
			t.Errorf("request_logs is missing column %s", c)
		}
	}
	var name string
	if err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='sync_metadata'").Scan(&name); err != nil {
		t.Errorf("sync_metadata table was not created: %v", err)
	}
}

// TestMigrateLegacyDatabase opens a database created before versioned migrations: request_logs has
// the original columns, rows predate the cost split, and there is no schema_migrations table.
func TestMigrateLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE request_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT NOT NULL UNIQUE,
			timestamp TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			api_key_id TEXT,
			model TEXT NOT NULL,
			provider TEXT NOT NULL,
			cache_status TEXT NOT NULL,
			cache_tier TEXT NOT NULL,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0.0,
			saved_usd REAL NOT NULL DEFAULT 0.0,
			status_code INTEGER NOT NULL DEFAULT 200,
			error_message TEXT
		)`,
		`INSERT INTO request_logs (request_id, model, provider, cache_status, cache_tier, prompt_tokens, completion_tokens, cost_usd)
		 VALUES ('legacy-gemini', 'claude-sonnet-5', 'gemini', 'MISS', 'NONE', 0, 0, 0)`,
		`INSERT INTO request_logs (request_id, model, provider, cache_status, cache_tier, prompt_tokens, completion_tokens, cost_usd)
		 VALUES ('legacy-paid', 'gpt-4o', 'openai', 'MISS', 'NONE', 300, 100, 0.004)`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("build legacy schema: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	d := openFileDB(t, path)
	defer d.Close()

	if v, err := d.SchemaVersion(); err != nil || v != latestVersion() {
		t.Fatalf("schema version = %d (err %v), want %d", v, err, latestVersion())
	}
	var model, requested string
	if err := d.QueryRow("SELECT model, requested_model FROM request_logs WHERE request_id = 'legacy-gemini'").Scan(&model, &requested); err != nil {
		t.Fatal(err)
	}
	if model != "gemini-3.8-flash" || requested != "claude-sonnet-5" {
		t.Errorf("routed row = model %q requested %q, want gemini-3.8-flash / claude-sonnet-5", model, requested)
	}
	var promptCost, completionCost float64
	if err := d.QueryRow("SELECT prompt_cost_usd, completion_cost_usd FROM request_logs WHERE request_id = 'legacy-paid'").Scan(&promptCost, &completionCost); err != nil {
		t.Fatal(err)
	}
	if promptCost != 0.003 || completionCost != 0.001 {
		t.Errorf("cost split = %v / %v, want 0.003 / 0.001", promptCost, completionCost)
	}
}

// TestMigrateRunsOnce reopens a database and checks that applied migrations are not re-run: an edit
// to seeded pricing survives, where the old unversioned Migrate re-seeded it on every start.
func TestMigrateRunsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "once.db")
	d := openFileDB(t, path)
	if _, err := d.Exec("UPDATE model_pricing SET input_cost_per_m = 99 WHERE model_pattern = 'gpt-4o-mini'"); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d = openFileDB(t, path)
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var cost float64
	if err := d.QueryRow("SELECT input_cost_per_m FROM model_pricing WHERE model_pattern = 'gpt-4o-mini'").Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != 99 {
		t.Errorf("gpt-4o-mini input cost = %v after reopening, want the edited 99", cost)
	}
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(migrations) {
		t.Errorf("schema_migrations has %d rows, want %d", n, len(migrations))
	}
}

// TestPricingRefreshKeepsEditedRates reruns migration 007 against a seeded row a user edited and
// one still at its seed value: only the unedited row is corrected.
func TestPricingRefreshKeepsEditedRates(t *testing.T) {
	d := openFileDB(t, filepath.Join(t.TempDir(), "pricing.db"))
	defer d.Close()
	for _, stmt := range []string{
		`DELETE FROM schema_migrations WHERE version = 7`,
		`UPDATE model_pricing SET input_cost_per_m = 12.00, cached_input_cost_per_m = 1.20, output_cost_per_m = 60.00 WHERE model_pattern = 'claude-opus-5.*'`,
		`UPDATE model_pricing SET input_cost_per_m = 3.00, cached_input_cost_per_m = 0.30, output_cost_per_m = 15.00 WHERE model_pattern = 'claude-sonnet-5.*'`,
	} {
		if _, err := d.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	var opus, sonnet float64
	if err := d.QueryRow(`SELECT input_cost_per_m FROM model_pricing WHERE model_pattern = 'claude-opus-5.*'`).Scan(&opus); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT input_cost_per_m FROM model_pricing WHERE model_pattern = 'claude-sonnet-5.*'`).Scan(&sonnet); err != nil {
		t.Fatal(err)
	}
	if opus != 12.00 || sonnet != 2.00 {
		t.Fatalf("after 007: opus-5 = %v (edited, want 12), sonnet-5 = %v (seed value, want 2)", opus, sonnet)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM model_pricing WHERE model_pattern IN ('gpt-4o', 'claude-haiku-5.*')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stale rows remaining = %d (err %v), want 0", n, err)
	}
}

var keyBudgetColumns = []string{"daily_budget_usd", "daily_spend_usd", "daily_spend_date", "spend_month"}

func TestMigrateFreshDatabaseHasKeyBudgetColumns(t *testing.T) {
	d := openFileDB(t, filepath.Join(t.TempDir(), "fresh-keys.db"))
	defer d.Close()
	cols := columnSet(t, d, "api_keys")
	for _, c := range keyBudgetColumns {
		if !cols[c] {
			t.Errorf("api_keys is missing column %s", c)
		}
	}
}

// TestKeyBudgetPeriodsOverVersion7 builds a database at schema version 7 holding a key with spend,
// then migrates it: 008 adds the budget columns with no daily cap, and stamps the key with the
// current UTC month so the spend it already has keeps counting.
func TestKeyBudgetPeriodsOverVersion7(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	d := &DB{DB: raw, path: path}
	defer d.Close()

	ctx := context.Background()
	if _, err := d.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > 7 {
			break
		}
		if err := d.apply(ctx, m); err != nil {
			t.Fatalf("apply %d: %v", m.version, err)
		}
	}
	if v, err := d.SchemaVersion(); err != nil || v != 7 {
		t.Fatalf("setup schema version = %d (err %v), want 7", v, err)
	}
	if cols := columnSet(t, d, "api_keys"); cols["spend_month"] {
		t.Fatal("spend_month must not exist before 008")
	}
	if _, err := d.Exec(`INSERT INTO api_keys (id, key_hash, name, monthly_budget_usd, current_spend_usd)
		VALUES ('key_old', 'hash_old', 'old', 10, 3.5)`); err != nil {
		t.Fatal(err)
	}

	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate over v7: %v", err)
	}
	if v, err := d.SchemaVersion(); err != nil || v != latestVersion() {
		t.Fatalf("schema version = %d (err %v), want %d", v, err, latestVersion())
	}
	var (
		spend, dailyBudget, dailySpend float64
		dailyDate, month               string
	)
	if err := d.QueryRow(`SELECT current_spend_usd, daily_budget_usd, daily_spend_usd, daily_spend_date, spend_month
		FROM api_keys WHERE id = 'key_old'`).Scan(&spend, &dailyBudget, &dailySpend, &dailyDate, &month); err != nil {
		t.Fatal(err)
	}
	if spend != 3.5 || dailyBudget != 0 || dailySpend != 0 || dailyDate != "" {
		t.Errorf("migrated key = spend %v daily budget %v daily spend %v date %q; want 3.5, 0, 0, empty",
			spend, dailyBudget, dailySpend, dailyDate)
	}
	// strftime('now') is UTC; allow for the test straddling a month boundary.
	before := time.Now().UTC().Add(-time.Minute).Format("2006-01")
	after := time.Now().UTC().Format("2006-01")
	if month != before && month != after {
		t.Errorf("spend_month = %q, want the current UTC month %q", month, after)
	}
}
