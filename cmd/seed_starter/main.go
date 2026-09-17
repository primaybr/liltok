package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/primaybr/liltok/internal/db"
)

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

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("Failed to get home directory: %v\n", err)
		os.Exit(1)
	}

	dbPath := filepath.Join(home, ".liltok", "liltok.db")
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}

	archivePath := filepath.Join("internal", "db", "starter_cache.json.gz")
	gzBytes, err := os.ReadFile(archivePath)
	if err != nil {
		fmt.Printf("Failed to read %s: %v\n", archivePath, err)
		os.Exit(1)
	}

	gzReader, err := gzip.NewReader(bytes.NewReader(gzBytes))
	if err != nil {
		fmt.Printf("Failed to decompress %s: %v\n", archivePath, err)
		os.Exit(1)
	}
	defer gzReader.Close()

	var items []StarterCacheItem
	if err := json.NewDecoder(gzReader).Decode(&items); err != nil {
		fmt.Printf("Failed to decode JSON: %v\n", err)
		os.Exit(1)
	}

	database, err := db.Open(dbPath)
	if err != nil {
		fmt.Printf("Failed to open database %s: %v\n", dbPath, err)
		os.Exit(1)
	}
	defer database.Close()

	tx, err := database.Begin()
	if err != nil {
		fmt.Printf("Failed to begin transaction: %v\n", err)
		os.Exit(1)
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
		fmt.Printf("Failed to prepare statement: %v\n", err)
		os.Exit(1)
	}
	defer stmt.Close()

	seeded := 0
	for _, item := range items {
		isSem := 0
		if item.IsSemantic {
			isSem = 1
		}
		_, err := stmt.Exec(
			item.Hash,
			item.Model,
			item.NormalizedPrompt,
			[]byte(item.ResponsePayload),
			item.PromptTokens,
			item.CompletionTokens,
			item.TTLSeconds,
			isSem,
		)
		if err != nil {
			fmt.Printf("Failed to insert item %s: %v\n", item.Hash, err)
			continue
		}
		seeded++
	}

	if err := tx.Commit(); err != nil {
		fmt.Printf("Failed to commit transaction: %v\n", err)
		os.Exit(1)
	}

	var totalEntries int
	_ = database.QueryRow("SELECT COUNT(*) FROM cache_entries").Scan(&totalEntries)

	fmt.Printf("Successfully seeded %d starter cache entries into %s\n", seeded, dbPath)
	fmt.Printf("Total active cache entries in database: %d\n", totalEntries)
}
