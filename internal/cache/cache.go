package cache

import (
	"context"
	"time"
)

// CacheEntry represents an immutable stored completion response.
type CacheEntry struct {
	Hash             string    `json:"hash"`
	Model            string    `json:"model"`
	NormalizedPrompt string    `json:"normalized_prompt"`
	ResponsePayload  []byte    `json:"response_payload"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	HitCount         int64     `json:"hit_count"`
	CreatedAt        time.Time `json:"created_at"`
	LastAccessedAt   time.Time `json:"last_accessed_at"`
	TTLSeconds       int       `json:"ttl_seconds"`
	IsPinned         bool      `json:"is_pinned"`
	IsSemantic       bool      `json:"is_semantic"`
}

// CacheStats provides aggregated cache operational metrics.
type CacheStats struct {
	TotalEntries  int64 `json:"total_entries"`
	MemoryEntries int   `json:"memory_entries"`
	TotalHits     int64 `json:"total_hits"`
}

// Store defines the interface for multi-tier cache backends.
type Store interface {
	Get(ctx context.Context, hash string) (*CacheEntry, bool, error)
	Set(ctx context.Context, entry *CacheEntry) error
	Delete(ctx context.Context, hash string) error
	Purge(ctx context.Context, model string) (int64, error)
	Stats(ctx context.Context) (CacheStats, error)
	Close() error
}
