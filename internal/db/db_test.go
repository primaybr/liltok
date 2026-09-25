package db

import (
	"path/filepath"
	"testing"
)

func TestOpenMemoryDB(t *testing.T) {
	database, err := Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	defer database.Close()

	var tableName string
	err = database.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='cache_entries'").Scan(&tableName)
	if err != nil {
		t.Fatalf("failed to query cache_entries table: %v", err)
	}

	if tableName != "cache_entries" {
		t.Errorf("expected table cache_entries, got %s", tableName)
	}

	// Verify model_pricing seed data
	var count int
	err = database.QueryRow("SELECT COUNT(*) FROM model_pricing").Scan(&count)
	if err != nil {
		t.Fatalf("failed to query model_pricing count: %v", err)
	}
	if count < 5 {
		t.Errorf("expected at least 5 seeded pricing entries, got %d", count)
	}
}

func TestOpenFileDB(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "sub", "test.db")

	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open file db: %v", err)
	}
	defer database.Close()

	var journalMode string
	err = database.QueryRow("PRAGMA journal_mode").Scan(&journalMode)
	if err != nil {
		t.Fatalf("failed to query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("expected journal_mode wal, got %s", journalMode)
	}
}

func TestStarterCacheSeeding(t *testing.T) {
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "") // this test checks the seeding itself
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "seeded.db")

	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open file db: %v", err)
	}
	defer database.Close()

	var cacheCount int
	err = database.QueryRow("SELECT COUNT(*) FROM cache_entries").Scan(&cacheCount)
	if err != nil {
		t.Fatalf("failed to query cache_entries count: %v", err)
	}

	if cacheCount < 50 {
		t.Errorf("expected at least 50 pre-bundled cache entries, got %d", cacheCount)
	}

	// Idempotency: Calling SeedStarterCache again should return 0 (no duplicates)
	reseeded, err := database.SeedStarterCache()
	if err != nil {
		t.Fatalf("unexpected error re-seeding: %v", err)
	}
	if reseeded != 0 {
		t.Errorf("expected 0 reseeded entries on non-empty db, got %d", reseeded)
	}
}
