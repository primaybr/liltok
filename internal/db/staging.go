package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// StagedEntry represents a candidate cache entry under maintainer moderation.
type StagedEntry struct {
	ID               int64     `json:"id"`
	SourceFile       string    `json:"source_file"`
	Hash             string    `json:"hash"`
	Model            string    `json:"model"`
	NormalizedPrompt string    `json:"normalized_prompt"`
	ResponsePayload  string    `json:"response_payload"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TTLSeconds       int       `json:"ttl_seconds"`
	IsSemantic       bool      `json:"is_semantic"`
	IsDuplicate      bool      `json:"is_duplicate"`
	Status           string    `json:"status"`
	StagedAt         time.Time `json:"staged_at"`
}

// InsertStagedEntries batches incoming candidate cache items into the staging table.
func (d *DB) InsertStagedEntries(entries []StarterCacheItem, sourceFile string) (int, int, error) {
	if len(entries) == 0 {
		return 0, 0, nil
	}

	tx, err := d.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmtCheck, err := tx.Prepare("SELECT 1 FROM cache_entries WHERE hash = ?")
	if err != nil {
		return 0, 0, fmt.Errorf("failed to prepare duplicate check: %w", err)
	}
	defer stmtCheck.Close()

	stmtInsert, err := tx.Prepare(`
		INSERT INTO staged_cache_entries (
			source_file, hash, model, normalized_prompt, response_payload,
			prompt_tokens, completion_tokens, ttl_seconds, is_semantic,
			is_duplicate, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')
		ON CONFLICT(hash) DO UPDATE SET
			source_file = excluded.source_file,
			normalized_prompt = excluded.normalized_prompt,
			response_payload = excluded.response_payload,
			prompt_tokens = excluded.prompt_tokens,
			completion_tokens = excluded.completion_tokens,
			ttl_seconds = excluded.ttl_seconds,
			is_duplicate = excluded.is_duplicate,
			status = 'pending',
			staged_at = CURRENT_TIMESTAMP
	`)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to prepare insert statement: %w", err)
	}
	defer stmtInsert.Close()

	inserted := 0
	duplicates := 0

	for _, e := range entries {
		if e.Hash == "" || e.Model == "" || e.NormalizedPrompt == "" {
			continue
		}

		var dummy int
		isDup := false
		err := stmtCheck.QueryRow(e.Hash).Scan(&dummy)
		if err == nil {
			isDup = true
			duplicates++
		}

		ttl := e.TTLSeconds
		if ttl <= 0 {
			ttl = 604800
		}

		isSemInt := 0
		if e.IsSemantic {
			isSemInt = 1
		}

		isDupInt := 0
		if isDup {
			isDupInt = 1
		}

		_, err = stmtInsert.Exec(
			sourceFile,
			e.Hash,
			e.Model,
			e.NormalizedPrompt,
			[]byte(e.ResponsePayload),
			e.PromptTokens,
			e.CompletionTokens,
			ttl,
			isSemInt,
			isDupInt,
		)
		if err != nil {
			return 0, 0, fmt.Errorf("failed to insert staged entry %s: %w", e.Hash, err)
		}
		inserted++
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("failed to commit staging transaction: %w", err)
	}

	return inserted, duplicates, nil
}

// GetStagedEntries retrieves staged items matching the given status and pagination.
func (d *DB) GetStagedEntries(status string, limit, offset int) ([]StagedEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	query := `
		SELECT 
			id, source_file, hash, model, normalized_prompt, response_payload,
			prompt_tokens, completion_tokens, ttl_seconds, is_semantic,
			is_duplicate, status, staged_at
		FROM staged_cache_entries
	`
	var args []interface{}
	if status != "" && status != "all" {
		query += " WHERE status = ?"
		args = append(args, status)
	}
	query += " ORDER BY is_duplicate ASC, staged_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query staged entries error: %w", err)
	}
	defer rows.Close()

	var results []StagedEntry
	for rows.Next() {
		var (
			e          StagedEntry
			payload    []byte
			isSemantic int
			isDup      int
		)
		err := rows.Scan(
			&e.ID,
			&e.SourceFile,
			&e.Hash,
			&e.Model,
			&e.NormalizedPrompt,
			&payload,
			&e.PromptTokens,
			&e.CompletionTokens,
			&e.TTLSeconds,
			&isSemantic,
			&isDup,
			&e.Status,
			&e.StagedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan staged entry error: %w", err)
		}

		e.ResponsePayload = string(payload)
		e.IsSemantic = isSemantic == 1
		e.IsDuplicate = isDup == 1
		results = append(results, e)
	}

	return results, rows.Err()
}

// CountStagedEntries returns counts by status.
func (d *DB) CountStagedEntries() (pending int, approved int, rejected int, err error) {
	rows, err := d.Query(`
		SELECT status, COUNT(*) 
		FROM staged_cache_entries 
		GROUP BY status
	`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count staged entries error: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err == nil {
			switch status {
			case "pending":
				pending = count
			case "approved":
				approved = count
			case "rejected":
				rejected = count
			}
		}
	}
	return pending, approved, rejected, nil
}

// ApproveStagedEntries copies approved entries into active cache_entries and marks them approved.
func (d *DB) ApproveStagedEntries(ctx context.Context, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to start approval transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}

	selectQuery := fmt.Sprintf(`
		SELECT hash, model, normalized_prompt, response_payload,
		       prompt_tokens, completion_tokens, ttl_seconds, is_semantic
		FROM staged_cache_entries
		WHERE id IN (%s)
	`, strings.Join(placeholders, ","))

	rows, err := tx.QueryContext(ctx, selectQuery, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch staged entries for approval: %w", err)
	}

	type toMerge struct {
		hash             string
		model            string
		normalizedPrompt string
		payload          []byte
		promptTokens     int
		completionTokens int
		ttlSeconds       int
		isSemantic       int
	}

	var items []toMerge
	for rows.Next() {
		var m toMerge
		if err := rows.Scan(&m.hash, &m.model, &m.normalizedPrompt, &m.payload, &m.promptTokens, &m.completionTokens, &m.ttlSeconds, &m.isSemantic); err != nil {
			rows.Close()
			return 0, fmt.Errorf("failed to scan item for approval: %w", err)
		}
		items = append(items, m)
	}
	rows.Close()

	insertStmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO cache_entries (
			hash, model, normalized_prompt, response_payload,
			prompt_tokens, completion_tokens, hit_count,
			created_at, last_accessed_at, ttl_seconds, is_pinned, is_semantic
		) VALUES (?, ?, ?, ?, ?, ?, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ?, 0, ?)
	`)
	if err != nil {
		return 0, fmt.Errorf("failed to prepare cache insertion: %w", err)
	}
	defer insertStmt.Close()

	for _, item := range items {
		_, err := insertStmt.ExecContext(ctx,
			item.hash,
			item.model,
			item.normalizedPrompt,
			item.payload,
			item.promptTokens,
			item.completionTokens,
			item.ttlSeconds,
			item.isSemantic,
		)
		if err != nil {
			return 0, fmt.Errorf("failed to insert approved entry %s: %w", item.hash, err)
		}
	}

	updateQuery := fmt.Sprintf(`
		UPDATE staged_cache_entries
		SET status = 'approved'
		WHERE id IN (%s)
	`, strings.Join(placeholders, ","))

	if _, err := tx.ExecContext(ctx, updateQuery, args...); err != nil {
		return 0, fmt.Errorf("failed to update staging status: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit approval transaction: %w", err)
	}

	return len(items), nil
}

// RejectStagedEntries marks staged entries as rejected.
func (d *DB) RejectStagedEntries(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}

	query := fmt.Sprintf(`
		UPDATE staged_cache_entries
		SET status = 'rejected'
		WHERE id IN (%s)
	`, strings.Join(placeholders, ","))

	_, err := d.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("failed to reject staged entries: %w", err)
	}
	return nil
}

// ClearStagedEntries deletes staged entries matching status or all.
func (d *DB) ClearStagedEntries(ctx context.Context, status string) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if status == "" || status == "all" {
		res, err = d.ExecContext(ctx, "DELETE FROM staged_cache_entries")
	} else {
		res, err = d.ExecContext(ctx, "DELETE FROM staged_cache_entries WHERE status = ?", status)
	}
	if err != nil {
		return 0, fmt.Errorf("failed to clear staged entries: %w", err)
	}
	return res.RowsAffected()
}
