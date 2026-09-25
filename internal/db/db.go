package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB wraps the underlying sql.DB connection pool.
type DB struct {
	*sql.DB
	path string
}

// Open initializes and configures a SQLite connection with WAL mode and performance PRAGMAs.
func Open(dbPath string) (*DB, error) {
	if dbPath == "" {
		dbPath = ":memory:"
	}

	if dbPath != ":memory:" {
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create database directory %s: %w", dir, err)
		}
	}

	// modernc.org/sqlite uses driver name "sqlite"
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=cache_size(-64000)&_pragma=temp_store(MEMORY)", dbPath)
	if dbPath == ":memory:" {
		dsn = "file::memory:?mode=memory&cache=shared&_pragma=foreign_keys(ON)"
	}

	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database at %s: %w", dbPath, err)
	}

	// SQLite operates best with a bounded connection pool
	conn.SetMaxOpenConns(1) // Single writer prevents lock contention
	conn.SetMaxIdleConns(1)

	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to ping sqlite database: %w", err)
	}

	database := &DB{
		DB:   conn,
		path: dbPath,
	}

	if err := database.Migrate(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	// Seed pre-bundled starter cache on cold start (0-latency day one hits)
	_, _ = database.SeedStarterCache()

	return database, nil
}

// Path returns the SQLite database file path.
func (d *DB) Path() string {
	return d.path
}
