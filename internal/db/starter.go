package db

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed starter_cache.json.gz
var starterCacheGz []byte

// StarterCacheItem represents an embedded cache entry.
type StarterCacheItem struct {
	Hash             string `json:"hash"`
	Model            string `json:"model"`
	NormalizedPrompt string `json:"normalized_prompt"`
	ResponsePayload  string `json:"response_payload"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TTLSeconds       int    `json:"ttl_seconds"`
	IsSemantic       bool   `json:"is_semantic"`
}

// SeedStarterCache inspects the database. If it is empty or has fewer than 5 entries,
// it unpacks and inserts the embedded starter cache so that users get instant cache hits on day one.
func (d *DB) SeedStarterCache() (int, error) {
	if len(starterCacheGz) == 0 {
		return 0, nil
	}
	// Seeding decodes and inserts the whole embedded pack; test runs that open many temporary
	// databases set LILTOK_SKIP_STARTER_SEED=1 to avoid doing that on every Open.
	if os.Getenv("LILTOK_SKIP_STARTER_SEED") == "1" {
		return 0, nil
	}

	var existingCount int
	_ = d.QueryRow("SELECT COUNT(*) FROM cache_entries").Scan(&existingCount)
	if existingCount >= 5 {
		// User already has entries; preserve user cache state
		return 0, nil
	}

	gzReader, err := gzip.NewReader(bytes.NewReader(starterCacheGz))
	if err != nil {
		return 0, fmt.Errorf("failed to decompress starter cache: %w", err)
	}
	defer gzReader.Close()

	var items []StarterCacheItem
	if err := json.NewDecoder(gzReader).Decode(&items); err != nil {
		return 0, fmt.Errorf("failed to decode starter cache json: %w", err)
	}

	if len(items) == 0 {
		return 0, nil
	}

	tx, err := d.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin starter seed transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO cache_entries (
			hash, model, normalized_prompt, response_payload,
			prompt_tokens, completion_tokens, hit_count,
			created_at, last_accessed_at, ttl_seconds, is_pinned, is_semantic
		) VALUES (?, ?, ?, ?, ?, ?, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ?, 1, ?)
	`)
	if err != nil {
		return 0, fmt.Errorf("failed to prepare starter insert statement: %w", err)
	}
	defer stmt.Close()

	inserted := 0
	for _, item := range items {
		isSemInt := 0
		if item.IsSemantic {
			isSemInt = 1
		}
		_, err := stmt.Exec(item.Hash, item.Model, item.NormalizedPrompt, []byte(item.ResponsePayload), item.PromptTokens, item.CompletionTokens, item.TTLSeconds, isSemInt)
		if err == nil {
			inserted++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit starter seed transaction: %w", err)
	}

	return inserted, nil
}

// ForceSeedStarterCache unpacks and inserts or replaces all embedded starter cache entries into the database.
func (d *DB) ForceSeedStarterCache() (int, error) {
	if len(starterCacheGz) == 0 {
		return 0, nil
	}

	gzReader, err := gzip.NewReader(bytes.NewReader(starterCacheGz))
	if err != nil {
		return 0, fmt.Errorf("failed to decompress starter cache: %w", err)
	}
	defer gzReader.Close()

	var items []StarterCacheItem
	if err := json.NewDecoder(gzReader).Decode(&items); err != nil {
		return 0, fmt.Errorf("failed to decode starter cache json: %w", err)
	}

	if len(items) == 0 {
		return 0, nil
	}

	tx, err := d.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin starter seed transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
		INSERT OR REPLACE INTO cache_entries (
			hash, model, normalized_prompt, response_payload,
			prompt_tokens, completion_tokens, hit_count,
			created_at, last_accessed_at, ttl_seconds, is_pinned, is_semantic
		) VALUES (?, ?, ?, ?, ?, ?, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ?, 1, ?)
	`)
	if err != nil {
		return 0, fmt.Errorf("failed to prepare starter replace statement: %w", err)
	}
	defer stmt.Close()

	seeded := 0
	for _, item := range items {
		isSemInt := 0
		if item.IsSemantic {
			isSemInt = 1
		}
		_, err := stmt.Exec(item.Hash, item.Model, item.NormalizedPrompt, []byte(item.ResponsePayload), item.PromptTokens, item.CompletionTokens, item.TTLSeconds, isSemInt)
		if err == nil {
			seeded++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit starter seed transaction: %w", err)
	}

	return seeded, nil
}
