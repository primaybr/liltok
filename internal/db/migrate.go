package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migration is one numbered schema change. Each runs once, in its own transaction, and its version
// is recorded in schema_migrations in that same transaction.
type migration struct {
	version int
	name    string
	file    string                                      // SQL file under migrations/, or
	up      func(ctx context.Context, tx *sql.Tx) error // a Go step when plain SQL cannot express it
}

// migrations lists every schema change in order. Append new entries with the next version; never
// edit or renumber one that has shipped, since installed databases have already recorded it.
var migrations = []migration{
	{version: 1, name: "init_schema", file: "001_init_schema.sql"},
	// Databases created before these columns were in 001 gained them through ad-hoc ALTERs; add each
	// only where it is missing so both kinds of database converge.
	{version: 2, name: "request_logs_breakdown_columns", up: addColumnsIfMissing("request_logs", []columnDef{
		{"requested_model", "TEXT DEFAULT ''"},
		{"prompt_cost_usd", "REAL NOT NULL DEFAULT 0.0"},
		{"completion_cost_usd", "REAL NOT NULL DEFAULT 0.0"},
		{"pruned_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"pruned_tokens", "INTEGER NOT NULL DEFAULT 0"},
	})},
	{version: 3, name: "backfill_routed_models", file: "003_backfill_routed_models.sql"},
	{version: 4, name: "backfill_cost_breakdown", file: "004_backfill_cost_breakdown.sql"},
	{version: 5, name: "sync_metadata", file: "005_sync_metadata.sql"},
	{version: 6, name: "model_catalog", file: "006_model_catalog.sql"},
}

type columnDef struct {
	name string
	decl string
}

func addColumnsIfMissing(table string, cols []columnDef) func(ctx context.Context, tx *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		existing, err := tableColumns(ctx, tx, table)
		if err != nil {
			return err
		}
		for _, c := range cols {
			if existing[c.name] {
				continue
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, c.name, c.decl)); err != nil {
				return fmt.Errorf("add column %s.%s: %w", table, c.name, err)
			}
		}
		return nil
	}
}

func tableColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[strings.ToLower(name)] = true
	}
	return cols, rows.Err()
}

// Migrate applies every migration not yet recorded in schema_migrations, in version order. Versions
// above the newest one this binary knows (a database used by a newer liltok) are left alone.
func (d *DB) Migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if _, err := d.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := d.apply(ctx, m); err != nil {
			// Another process sharing the database file may have applied it first.
			if now, qerr := d.appliedVersions(ctx); qerr == nil && now[m.version] {
				continue
			}
			return fmt.Errorf("migration %03d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

// SchemaVersion returns the highest applied migration version, or 0 for an unmigrated database.
func (d *DB) SchemaVersion() (int, error) {
	var v sql.NullInt64
	if err := d.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

func (d *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := d.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func (d *DB) apply(ctx context.Context, m migration) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if m.file != "" {
		script, err := migrationFiles.ReadFile("migrations/" + m.file)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			return err
		}
	}
	if m.up != nil {
		if err := m.up(ctx, tx); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, name) VALUES (?, ?)", m.version, m.name); err != nil {
		return err
	}
	return tx.Commit()
}
