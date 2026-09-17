package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/liltok/liltok/internal/db"
)

// SemanticCache provides high-level Tier-3 semantic similarity caching.
type SemanticCache struct {
	embedder  Embedder
	index     *VectorIndex
	threshold float32
}

// NewSemanticCache creates a new SemanticCache with given DB, embedder, and match threshold.
func NewSemanticCache(database *db.DB, embedder Embedder, threshold float32) *SemanticCache {
	if embedder == nil {
		embedder = NewFastLocalEmbedder(256)
	}
	if threshold <= 0 {
		threshold = DefaultSemanticThreshold
	}
	return &SemanticCache{
		embedder:  embedder,
		index:     NewVectorIndex(database),
		threshold: threshold,
	}
}

// ComputeHash returns hex-encoded SHA-256 string for system prompt or tools.
func ComputeHash(input string) string {
	if input == "" {
		return ""
	}
	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:])
}

// Lookup evaluates guardrails, embeds the user query, and queries the vector index.
func (sc *SemanticCache) Lookup(ctx context.Context, model, systemPrompt, toolsJSON, userQuery string) (*SemanticEntry, float32, bool) {
	decision := CheckSemanticEligibility(userQuery)
	if !decision.IsEligible {
		return nil, 0, false
	}

	queryVec, err := sc.embedder.Embed(ctx, userQuery)
	if err != nil || len(queryVec) == 0 {
		return nil, 0, false
	}

	sysHash := ComputeHash(systemPrompt)
	toolsHash := ComputeHash(toolsJSON)

	return sc.index.Search(ctx, model, sysHash, toolsHash, queryVec, sc.threshold)
}

// Store records a query, its embedding, and response into the semantic cache.
func (sc *SemanticCache) Store(ctx context.Context, hash, model, systemPrompt, toolsJSON, userQuery string, responsePayload []byte, ttl time.Duration) error {
	decision := CheckSemanticEligibility(userQuery)
	if !decision.IsEligible {
		return nil
	}

	vec, err := sc.embedder.Embed(ctx, userQuery)
	if err != nil || len(vec) == 0 {
		return err
	}

	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}

	entry := &SemanticEntry{
		Hash:            hash,
		Model:           model,
		SystemHash:      ComputeHash(systemPrompt),
		ToolsHash:       ComputeHash(toolsJSON),
		PromptText:      userQuery,
		Vector:          vec,
		ResponsePayload: responsePayload,
		CreatedAt:       time.Now(),
		ExpiresAt:       time.Now().Add(ttl),
	}

	return sc.index.Insert(ctx, entry)
}

// Index returns underlying VectorIndex.
func (sc *SemanticCache) Index() *VectorIndex {
	return sc.index
}
