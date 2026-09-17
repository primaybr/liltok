package exact

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/primaybr/liltok/internal/cache"
	"github.com/primaybr/liltok/internal/db"
)

// SQLiteStore provides persistent L2 caching in SQLite.
type SQLiteStore struct {
	db *db.DB
}

// NewSQLiteStore wraps an initialized SQLite database handle.
func NewSQLiteStore(database *db.DB) *SQLiteStore {
	return &SQLiteStore{db: database}
}

// Get retrieves an entry from SQLite, verifying that it has not expired.
func (s *SQLiteStore) Get(ctx context.Context, hash string) (*cache.CacheEntry, bool, error) {
	query := `
		SELECT hash, model, normalized_prompt, response_payload, prompt_tokens, completion_tokens,
		       hit_count, created_at, last_accessed_at, ttl_seconds, is_pinned, is_semantic
		FROM cache_entries
		WHERE hash = ?
	`
	row := s.db.QueryRowContext(ctx, query, hash)

	var entry cache.CacheEntry
	var createdAtStr, lastAccessedStr string
	err := row.Scan(
		&entry.Hash,
		&entry.Model,
		&entry.NormalizedPrompt,
		&entry.ResponsePayload,
		&entry.PromptTokens,
		&entry.CompletionTokens,
		&entry.HitCount,
		&createdAtStr,
		&lastAccessedStr,
		&entry.TTLSeconds,
		&entry.IsPinned,
		&entry.IsSemantic,
	)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("sqlite cache query failed: %w", err)
	}

	// Parse timestamps
	entry.CreatedAt = parseTimestamp(createdAtStr)
	entry.LastAccessedAt = parseTimestamp(lastAccessedStr)

	// Check TTL expiration
	if !entry.IsPinned && entry.TTLSeconds > 0 {
		expiration := entry.CreatedAt.Add(time.Duration(entry.TTLSeconds) * time.Second)
		if time.Now().After(expiration) {
			_ = s.Delete(ctx, hash)
			return nil, false, nil
		}
	}

	return &entry, true, nil
}

// Set writes or overwrites an entry in SQLite.
func (s *SQLiteStore) Set(ctx context.Context, entry *cache.CacheEntry) error {
	query := `
		INSERT INTO cache_entries (
			hash, model, normalized_prompt, response_payload, prompt_tokens, completion_tokens,
			hit_count, created_at, last_accessed_at, ttl_seconds, is_pinned, is_semantic
		) VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			response_payload = excluded.response_payload,
			last_accessed_at = CURRENT_TIMESTAMP,
			hit_count = cache_entries.hit_count
	`
	_, err := s.db.ExecContext(ctx, query,
		entry.Hash,
		entry.Model,
		entry.NormalizedPrompt,
		entry.ResponsePayload,
		entry.PromptTokens,
		entry.CompletionTokens,
		entry.HitCount,
		entry.TTLSeconds,
		entry.IsPinned,
		entry.IsSemantic,
	)
	if err != nil {
		return fmt.Errorf("sqlite cache insert failed: %w", err)
	}
	return nil
}

// IncrementHit increments the hit counter and updates last accessed timestamp.
func (s *SQLiteStore) IncrementHit(ctx context.Context, hash string) error {
	query := `
		UPDATE cache_entries 
		SET hit_count = hit_count + 1, last_accessed_at = CURRENT_TIMESTAMP
		WHERE hash = ?
	`
	_, err := s.db.ExecContext(ctx, query, hash)
	return err
}

// Delete removes an entry from SQLite by hash.
func (s *SQLiteStore) Delete(ctx context.Context, hash string) error {
	query := `DELETE FROM cache_entries WHERE hash = ?`
	_, err := s.db.ExecContext(ctx, query, hash)
	return err
}

// Purge removes entries for a specific model or all entries if model is empty.
func (s *SQLiteStore) Purge(ctx context.Context, model string) (int64, error) {
	var res sql.Result
	var err error
	if model == "" {
		res, err = s.db.ExecContext(ctx, "DELETE FROM cache_entries")
	} else {
		res, err = s.db.ExecContext(ctx, "DELETE FROM cache_entries WHERE model = ?", model)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Stats returns cache table statistics.
func (s *SQLiteStore) Stats(ctx context.Context) (int64, int64, error) {
	var totalEntries int64
	var totalHits sql.NullInt64

	row := s.db.QueryRowContext(ctx, "SELECT COUNT(*), SUM(hit_count) FROM cache_entries")
	if err := row.Scan(&totalEntries, &totalHits); err != nil {
		return 0, 0, err
	}

	hits := int64(0)
	if totalHits.Valid {
		hits = totalHits.Int64
	}
	return totalEntries, hits, nil
}

func parseTimestamp(ts string) time.Time {
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z07:00",
		time.RFC3339,
	}
	for _, f := range formats {
		if t, err := time.Parse(f, ts); err == nil {
			return t
		}
	}
	return time.Now()
}
