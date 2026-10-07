package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/primaybr/liltok/internal/guardrails"
)

// MemoryEntry represents a persistent memory record stored in SQLite.
type MemoryEntry struct {
	ID         string    `json:"id"`
	ProjectKey string    `json:"project_key"`
	Category   string    `json:"category"`
	Content    string    `json:"content"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// NormalizeProjectKey generates a deterministic 16-hex character key for a project path or repo identifier.
func NormalizeProjectKey(project string) string {
	project = strings.TrimSpace(project)
	if project == "" || strings.EqualFold(project, "global") {
		return "global"
	}
	if len(project) == 16 && isHex16(project) {
		return strings.ToLower(project)
	}
	normalized := strings.ToLower(strings.ReplaceAll(project, "\\", "/"))
	h := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(h[:8])
}

func isHex16(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// MaxMemoryContentBytes defines the hard size limit (4 KB) for an individual memory entry.
const MaxMemoryContentBytes = 4096

// SaveMemory inserts or updates a memory entry for a project.
func (d *DB) SaveMemory(ctx context.Context, projectKey, category, content string) (*MemoryEntry, error) {
	category = strings.ToLower(strings.TrimSpace(category))
	switch category {
	case "convention", "environment", "quirk", "architecture", "user":
		// valid
	default:
		category = "convention"
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("memory content cannot be empty")
	}
	if len(content) > MaxMemoryContentBytes {
		return nil, fmt.Errorf("memory content exceeds maximum allowed size (%d bytes)", MaxMemoryContentBytes)
	}
	if guardrails.HasSecrets(content) {
		return nil, fmt.Errorf("memory rejected: sensitive credential or secret token detected")
	}

	normKey := NormalizeProjectKey(projectKey)
	idHash := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s", normKey, category, content)))
	id := hex.EncodeToString(idHash[:8])

	now := time.Now().UTC()
	query := `
		INSERT INTO gateway_memories (id, project_key, category, content, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			content = excluded.content,
			updated_at = excluded.updated_at
	`
	_, err := d.ExecContext(ctx, query, id, normKey, category, content, now, now)
	if err != nil {
		return nil, fmt.Errorf("failed to save memory: %w", err)
	}

	return &MemoryEntry{
		ID:         id,
		ProjectKey: normKey,
		Category:   category,
		Content:    content,
		CreatedAt:  now,
		UpdatedAt:  now,
	}, nil
}

// ListMemories returns all memories for a project ordered by category and updated_at desc.
func (d *DB) ListMemories(ctx context.Context, projectKey string, limit int) ([]MemoryEntry, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	normKey := NormalizeProjectKey(projectKey)
	query := `
		SELECT id, project_key, category, content, created_at, updated_at
		FROM gateway_memories
		WHERE project_key = ? OR project_key = 'global'
		ORDER BY updated_at DESC
		LIMIT ?
	`
	rows, err := d.QueryContext(ctx, query, normKey, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query memories: %w", err)
	}
	defer rows.Close()

	var entries []MemoryEntry
	for rows.Next() {
		var e MemoryEntry
		var createdVal, updatedVal interface{}
		if err := rows.Scan(&e.ID, &e.ProjectKey, &e.Category, &e.Content, &createdVal, &updatedVal); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTimeVal(createdVal)
		e.UpdatedAt = parseTimeVal(updatedVal)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func parseTimeVal(val interface{}) time.Time {
	switch v := val.(type) {
	case time.Time:
		return v
	case string:
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
		if t, err := time.Parse("2006-01-02 15:04:05", v); err == nil {
			return t
		}
	}
	return time.Now().UTC()
}

// DeleteMemory removes a memory entry by ID.
func (d *DB) DeleteMemory(ctx context.Context, id string) error {
	query := `DELETE FROM gateway_memories WHERE id = ?`
	res, err := d.ExecContext(ctx, query, strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("failed to delete memory: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// FormatMemoriesForPrompt formats a slice of memories into a bounded prompt block.
func FormatMemoriesForPrompt(memories []MemoryEntry, maxChars int) string {
	if len(memories) == 0 {
		return ""
	}
	if maxChars <= 0 {
		maxChars = 1200
	}

	var sb strings.Builder
	sb.WriteString("[Gateway Memory - Persistent Project Context]\n")
	for _, m := range memories {
		line := fmt.Sprintf("- [%s] %s\n", strings.ToUpper(m.Category), m.Content)
		if sb.Len()+len(line) > maxChars {
			break
		}
		sb.WriteString(line)
	}
	return strings.TrimSpace(sb.String())
}
