package db

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
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
	if _, err := d.Exec("UPDATE model_pricing SET input_cost_per_m = 99 WHERE model_pattern = 'gpt-4o'"); err != nil {
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
	if err := d.QueryRow("SELECT input_cost_per_m FROM model_pricing WHERE model_pattern = 'gpt-4o'").Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != 99 {
		t.Errorf("gpt-4o input cost = %v after reopening, want the edited 99", cost)
	}
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(migrations) {
		t.Errorf("schema_migrations has %d rows, want %d", n, len(migrations))
	}
}
