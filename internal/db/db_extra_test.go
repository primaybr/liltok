package db

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openTestDB opens a migrated, unseeded file database that is closed when the test ends.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// closedTestDB returns a database whose pool is already closed, so every query fails.
func closedTestDB(t *testing.T) *DB {
	t.Helper()
	d := openTestDB(t)
	_ = d.Close()
	return d
}

// withStarterPack swaps the embedded starter pack for the test's duration.
func withStarterPack(t *testing.T, data []byte) {
	t.Helper()
	orig := starterCacheGz
	starterCacheGz = data
	t.Cleanup(func() { starterCacheGz = orig })
}

func gzJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(v); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzRaw(t *testing.T, raw string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(raw))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func countRows(t *testing.T, d *DB, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := d.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func TestOpen_PathAndDefaults(t *testing.T) {
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	path := filepath.Join(t.TempDir(), "nested", "p.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	if d.Path() != path {
		t.Errorf("Path() = %q, want %q", d.Path(), path)
	}

	mem, err := Open("")
	if err != nil {
		t.Fatalf("open empty path: %v", err)
	}
	defer mem.Close()
	if mem.Path() != ":memory:" {
		t.Errorf("empty path should default to :memory:, got %q", mem.Path())
	}
}

func TestOpen_Errors(t *testing.T) {
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	dir := t.TempDir()

	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocker, "sub", "x.db")); err == nil || !strings.Contains(err.Error(), "failed to create database directory") {
		t.Errorf("expected directory error, got %v", err)
	}

	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, bytes.Repeat([]byte("this is not sqlite "), 512), 0644); err != nil {
		t.Fatal(err)
	}
	if d, err := Open(garbage); err == nil {
		_ = d.Close()
		t.Error("expected error opening a non-SQLite file")
	}
}

func TestSchemaVersion_ClosedDatabase(t *testing.T) {
	d := closedTestDB(t)
	if _, err := d.SchemaVersion(); err == nil {
		t.Error("expected error from closed database")
	}
	if err := d.Migrate(); err == nil || !strings.Contains(err.Error(), "create schema_migrations") {
		t.Errorf("expected create schema_migrations error, got %v", err)
	}
}

// withExtraMigration appends a migration for the test's duration.
func withExtraMigration(t *testing.T, m migration) {
	t.Helper()
	orig := migrations
	migrations = append(append([]migration(nil), orig...), m)
	t.Cleanup(func() { migrations = orig })
}

func TestMigrate_FailingMigrationsReportVersionAndRollBack(t *testing.T) {
	tests := []struct {
		name    string
		m       migration
		wantErr string
	}{
		{"missing sql file", migration{version: 90, name: "missing_file", file: "does_not_exist.sql"}, "migration 090_missing_file"},
		{"invalid sql", migration{version: 91, name: "bad_sql", up: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "CREATE TABLE half_done (id INTEGER)")
			if err != nil {
				return err
			}
			return errors.New("step failed")
		}}, "migration 091_bad_sql: step failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := openTestDB(t)
			withExtraMigration(t, tt.m)

			err := d.Migrate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
			if v, _ := d.SchemaVersion(); v != latestVersionBefore(tt.m.version) {
				t.Errorf("schema version = %d, failed migration must not be recorded", v)
			}
			if n := countRows(t, d, "SELECT COUNT(*) FROM sqlite_master WHERE name = 'half_done'"); n != 0 {
				t.Error("partial migration was not rolled back")
			}
		})
	}
}

func latestVersionBefore(v int) int {
	best := 0
	for _, m := range migrations {
		if m.version < v && m.version > best {
			best = m.version
		}
	}
	return best
}

func TestMigrate_SkipsMigrationAppliedConcurrently(t *testing.T) {
	d := openTestDB(t)
	path := d.Path()

	// The step records its own version through a second connection, as another process
	// sharing the file would, then fails. Migrate must treat the version as applied.
	withExtraMigration(t, migration{version: 92, name: "raced", up: func(ctx context.Context, tx *sql.Tx) error {
		other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
		if err != nil {
			return err
		}
		defer other.Close()
		if _, err := other.ExecContext(ctx, "INSERT INTO schema_migrations (version, name) VALUES (92, 'raced')"); err != nil {
			return err
		}
		return errors.New("lost the race")
	}})

	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate should skip a migration another process applied, got %v", err)
	}
	if v, err := d.SchemaVersion(); err != nil || v != 92 {
		t.Errorf("schema version = %d (err %v), want 92", v, err)
	}
}

func TestAddColumnsIfMissing(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if _, err := d.Exec("CREATE TABLE widgets (id INTEGER PRIMARY KEY, Name TEXT)"); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	step := addColumnsIfMissing("widgets", []columnDef{{"name", "TEXT"}, {"color", "TEXT DEFAULT 'red'"}})
	if err := step(ctx, tx); err != nil {
		t.Fatalf("add columns: %v", err)
	}
	// Running it again must be a no-op, matching column names case-insensitively.
	if err := step(ctx, tx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, d, "SELECT COUNT(*) FROM pragma_table_info('widgets')"); n != 3 {
		t.Errorf("widgets has %d columns, want 3", n)
	}

	tx, err = d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	err = addColumnsIfMissing("no_such_table", []columnDef{{"x", "TEXT"}})(ctx, tx)
	if err == nil || !strings.Contains(err.Error(), "add column no_such_table.x") {
		t.Errorf("expected add column error for missing table, got %v", err)
	}
}

func TestTableColumns_QueryError(t *testing.T) {
	d := openTestDB(t)
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tableColumns(context.Background(), tx, "bad name)"); err == nil || !strings.Contains(err.Error(), "read columns") {
		t.Errorf("expected read columns error, got %v", err)
	}
}

var testStarterItems = []StarterCacheItem{
	{Hash: "s1", Model: "gpt-4o", NormalizedPrompt: "p1", ResponsePayload: "r1", PromptTokens: 1, CompletionTokens: 2, TTLSeconds: 60},
	{Hash: "s2", Model: "claude-sonnet-5", NormalizedPrompt: "p2", ResponsePayload: "r2", TTLSeconds: 60, IsSemantic: true},
}

func TestSeedStarterCache_SmallPack(t *testing.T) {
	d := openTestDB(t)
	withStarterPack(t, gzJSON(t, testStarterItems))

	// The skip switch wins even on an empty database.
	if n, err := d.SeedStarterCache(); err != nil || n != 0 {
		t.Fatalf("seed with skip set: n=%d err=%v, want 0", n, err)
	}

	t.Setenv("LILTOK_SKIP_STARTER_SEED", "")
	n, err := d.SeedStarterCache()
	if err != nil || n != 2 {
		t.Fatalf("seed: n=%d err=%v, want 2", n, err)
	}
	var sem, pinned int
	if err := d.QueryRow("SELECT is_semantic, is_pinned FROM cache_entries WHERE hash = 's2'").Scan(&sem, &pinned); err != nil {
		t.Fatal(err)
	}
	if sem != 1 || pinned != 1 {
		t.Errorf("seeded row is_semantic=%d is_pinned=%d, want 1/1", sem, pinned)
	}

	// Fewer than 5 entries: seeding runs again, but INSERT OR IGNORE adds nothing new, and the
	// returned count reports only rows actually added.
	if n, err := d.SeedStarterCache(); err != nil || n != 0 {
		t.Errorf("reseed: n=%d err=%v, want 0 new rows", n, err)
	}
	if got := countRows(t, d, "SELECT COUNT(*) FROM cache_entries"); got != 2 {
		t.Errorf("cache has %d rows after reseed, want 2", got)
	}
}

func TestSeedStarterCache_SkipsPopulatedDatabase(t *testing.T) {
	d := openTestDB(t)
	withStarterPack(t, gzJSON(t, testStarterItems))
	for i := 0; i < 5; i++ {
		if _, err := d.Exec(`INSERT INTO cache_entries (hash, model, normalized_prompt, response_payload, created_at, last_accessed_at)
			VALUES (?, 'm', 'p', x'00', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, "user-"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "")
	if n, err := d.SeedStarterCache(); err != nil || n != 0 {
		t.Errorf("seed on populated db: n=%d err=%v, want 0", n, err)
	}
	if got := countRows(t, d, "SELECT COUNT(*) FROM cache_entries WHERE hash LIKE 's%'"); got != 0 {
		t.Errorf("starter rows were added to a populated db: %d", got)
	}
}

func TestSeedAndForceSeed_BadPacks(t *testing.T) {
	tests := []struct {
		name    string
		pack    []byte
		wantErr string
	}{
		{"no pack", nil, ""},
		{"not gzip", []byte("plain"), "failed to decompress"},
		{"bad json", gzRaw(t, "{oops"), "failed to decode"},
		{"empty list", gzJSON(t, []StarterCacheItem{}), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := openTestDB(t)
			withStarterPack(t, tt.pack)
			t.Setenv("LILTOK_SKIP_STARTER_SEED", "")

			for fnName, fn := range map[string]func() (int, error){
				"SeedStarterCache":      d.SeedStarterCache,
				"ForceSeedStarterCache": d.ForceSeedStarterCache,
			} {
				n, err := fn()
				if n != 0 {
					t.Errorf("%s returned %d entries", fnName, n)
				}
				if tt.wantErr == "" && err != nil {
					t.Errorf("%s: unexpected error %v", fnName, err)
				}
				if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
					t.Errorf("%s: err = %v, want containing %q", fnName, err, tt.wantErr)
				}
			}
		})
	}
}

func TestSeedAndForceSeed_ClosedDatabase(t *testing.T) {
	d := closedTestDB(t)
	withStarterPack(t, gzJSON(t, testStarterItems))
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "")
	if _, err := d.SeedStarterCache(); err == nil || !strings.Contains(err.Error(), "begin starter seed") {
		t.Errorf("seed: expected begin error, got %v", err)
	}
	if _, err := d.ForceSeedStarterCache(); err == nil || !strings.Contains(err.Error(), "begin starter seed") {
		t.Errorf("force seed: expected begin error, got %v", err)
	}
}

func TestForceSeedStarterCache_ReplacesExisting(t *testing.T) {
	d := openTestDB(t)
	withStarterPack(t, gzJSON(t, testStarterItems))
	if _, err := d.Exec(`INSERT INTO cache_entries (hash, model, normalized_prompt, response_payload, hit_count, created_at, last_accessed_at)
		VALUES ('s1', 'gpt-4o', 'p1', 'stale', 42, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}

	// Force seeding ignores the skip switch; it is an explicit user action.
	n, err := d.ForceSeedStarterCache()
	if err != nil || n != 2 {
		t.Fatalf("force seed: n=%d err=%v, want 2", n, err)
	}
	var payload []byte
	var hits int
	if err := d.QueryRow("SELECT response_payload, hit_count FROM cache_entries WHERE hash = 's1'").Scan(&payload, &hits); err != nil {
		t.Fatal(err)
	}
	if string(payload) != "r1" || hits != 0 {
		t.Errorf("s1 payload=%q hits=%d, want replaced r1 with 0 hits", payload, hits)
	}
}

func TestInsertStagedEntries_ValidationDuplicatesAndUpsert(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.Exec(`INSERT INTO cache_entries (hash, model, normalized_prompt, response_payload, created_at, last_accessed_at)
		VALUES ('already-cached', 'gpt-4o', 'p', x'00', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}

	if ins, dup, err := d.InsertStagedEntries(nil, "none"); err != nil || ins != 0 || dup != 0 {
		t.Errorf("empty input: %d/%d/%v", ins, dup, err)
	}

	items := []StarterCacheItem{
		{Hash: "", Model: "m", NormalizedPrompt: "p"},
		{Hash: "h", Model: "", NormalizedPrompt: "p"},
		{Hash: "h", Model: "m", NormalizedPrompt: ""},
		{Hash: "already-cached", Model: "gpt-4o", NormalizedPrompt: "p", ResponsePayload: "r"},
		{Hash: "fresh", Model: "gpt-4o", NormalizedPrompt: "p", ResponsePayload: "r", IsSemantic: true, TTLSeconds: 0},
	}
	ins, dup, err := d.InsertStagedEntries(items, "batch-1.enc")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if ins != 2 || dup != 1 {
		t.Fatalf("inserted=%d duplicates=%d, want 2 and 1", ins, dup)
	}

	all, err := d.GetStagedEntries("all", 0, -5)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 staged rows, got %d", len(all))
	}
	// Non-duplicates sort first.
	if all[0].Hash != "fresh" || all[0].IsDuplicate || !all[0].IsSemantic || all[0].TTLSeconds != 604800 || all[0].SourceFile != "batch-1.enc" {
		t.Errorf("unexpected fresh row: %+v", all[0])
	}
	if all[1].Hash != "already-cached" || !all[1].IsDuplicate || all[1].ResponsePayload != "r" {
		t.Errorf("unexpected duplicate row: %+v", all[1])
	}

	// Re-staging a rejected hash resets it to pending with the new content.
	if err := d.RejectStagedEntries(context.Background(), []int64{all[0].ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.InsertStagedEntries([]StarterCacheItem{{Hash: "fresh", Model: "gpt-4o", NormalizedPrompt: "p2", ResponsePayload: "r2", TTLSeconds: 10}}, "batch-2.enc"); err != nil {
		t.Fatal(err)
	}
	pending, err := d.GetStagedEntries("pending", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fresh *StagedEntry
	for i := range pending {
		if pending[i].Hash == "fresh" {
			fresh = &pending[i]
		}
	}
	if fresh == nil || fresh.NormalizedPrompt != "p2" || fresh.ResponsePayload != "r2" || fresh.TTLSeconds != 10 || fresh.SourceFile != "batch-2.enc" {
		t.Errorf("re-staged row not updated: %+v", fresh)
	}
	if n := countRows(t, d, "SELECT COUNT(*) FROM staged_cache_entries"); n != 2 {
		t.Errorf("upsert created a new row: %d rows", n)
	}
}

func TestGetStagedEntries_Pagination(t *testing.T) {
	d := openTestDB(t)
	var items []StarterCacheItem
	for _, h := range []string{"a", "b", "c"} {
		items = append(items, StarterCacheItem{Hash: h, Model: "m", NormalizedPrompt: "p", ResponsePayload: "r"})
	}
	if _, _, err := d.InsertStagedEntries(items, "f"); err != nil {
		t.Fatal(err)
	}
	page1, err := d.GetStagedEntries("", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	page2, err := d.GetStagedEntries("", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || len(page2) != 1 {
		t.Errorf("pages have %d and %d rows, want 2 and 1", len(page1), len(page2))
	}
	if none, err := d.GetStagedEntries("approved", 10, 0); err != nil || len(none) != 0 {
		t.Errorf("approved filter: %d rows, err %v", len(none), err)
	}
}

func TestApproveStagedEntries_CopiesFields(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if n, err := d.ApproveStagedEntries(ctx, nil); err != nil || n != 0 {
		t.Errorf("empty ids: %d/%v", n, err)
	}
	if err := d.RejectStagedEntries(ctx, nil); err != nil {
		t.Errorf("empty reject: %v", err)
	}

	if _, _, err := d.InsertStagedEntries([]StarterCacheItem{
		{Hash: "ap", Model: "gpt-4o", NormalizedPrompt: "np", ResponsePayload: "payload", PromptTokens: 3, CompletionTokens: 4, TTLSeconds: 77, IsSemantic: true},
	}, "f"); err != nil {
		t.Fatal(err)
	}
	staged, err := d.GetStagedEntries("pending", 10, 0)
	if err != nil || len(staged) != 1 {
		t.Fatalf("staged=%v err=%v", staged, err)
	}

	// Unknown IDs are ignored; only matching rows are merged.
	n, err := d.ApproveStagedEntries(ctx, []int64{staged[0].ID, 99999})
	if err != nil || n != 1 {
		t.Fatalf("approve: n=%d err=%v, want 1", n, err)
	}

	var model, np string
	var payload []byte
	var pTok, cTok, ttl, sem, hits, pinned int
	err = d.QueryRow(`SELECT model, normalized_prompt, response_payload, prompt_tokens, completion_tokens, ttl_seconds, is_semantic, hit_count, is_pinned
		FROM cache_entries WHERE hash = 'ap'`).Scan(&model, &np, &payload, &pTok, &cTok, &ttl, &sem, &hits, &pinned)
	if err != nil {
		t.Fatalf("approved entry missing: %v", err)
	}
	if model != "gpt-4o" || np != "np" || string(payload) != "payload" || pTok != 3 || cTok != 4 || ttl != 77 || sem != 1 || hits != 1 || pinned != 0 {
		t.Errorf("approved row mismatch: %s %s %s %d %d %d %d %d %d", model, np, payload, pTok, cTok, ttl, sem, hits, pinned)
	}
	if p, a, r, err := d.CountStagedEntries(); err != nil || p != 0 || a != 1 || r != 0 {
		t.Errorf("counts = %d/%d/%d err=%v, want 0/1/0", p, a, r, err)
	}
}

func TestClearStagedEntries(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	stage := func() []StagedEntry {
		t.Helper()
		if _, err := d.ClearStagedEntries(ctx, "all"); err != nil {
			t.Fatal(err)
		}
		var items []StarterCacheItem
		for _, h := range []string{"x", "y", "z"} {
			items = append(items, StarterCacheItem{Hash: h, Model: "m", NormalizedPrompt: "p", ResponsePayload: "r"})
		}
		if _, _, err := d.InsertStagedEntries(items, "f"); err != nil {
			t.Fatal(err)
		}
		entries, err := d.GetStagedEntries("all", 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}

	entries := stage()
	if err := d.RejectStagedEntries(ctx, []int64{entries[0].ID}); err != nil {
		t.Fatal(err)
	}
	n, err := d.ClearStagedEntries(ctx, "rejected")
	if err != nil || n != 1 {
		t.Errorf("clear rejected: n=%d err=%v, want 1", n, err)
	}
	if p, _, r, _ := d.CountStagedEntries(); p != 2 || r != 0 {
		t.Errorf("after clearing rejected: pending=%d rejected=%d, want 2/0", p, r)
	}

	stage()
	n, err = d.ClearStagedEntries(ctx, "")
	if err != nil || n != 3 {
		t.Errorf("clear all: n=%d err=%v, want 3", n, err)
	}
}

func TestStaging_ClosedDatabaseErrors(t *testing.T) {
	d := closedTestDB(t)
	ctx := context.Background()
	item := []StarterCacheItem{{Hash: "h", Model: "m", NormalizedPrompt: "p"}}

	if _, _, err := d.InsertStagedEntries(item, "f"); err == nil {
		t.Error("InsertStagedEntries: expected error")
	}
	if _, err := d.GetStagedEntries("all", 1, 0); err == nil {
		t.Error("GetStagedEntries: expected error")
	}
	if _, _, _, err := d.CountStagedEntries(); err == nil {
		t.Error("CountStagedEntries: expected error")
	}
	if _, err := d.ApproveStagedEntries(ctx, []int64{1}); err == nil {
		t.Error("ApproveStagedEntries: expected error")
	}
	if err := d.RejectStagedEntries(ctx, []int64{1}); err == nil {
		t.Error("RejectStagedEntries: expected error")
	}
	if _, err := d.ClearStagedEntries(ctx, "all"); err == nil {
		t.Error("ClearStagedEntries: expected error")
	}
}
