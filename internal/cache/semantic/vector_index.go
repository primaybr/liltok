package semantic

import (
	"context"
	"encoding/binary"
	"math"
	"sync"
	"time"

	"github.com/primaybr/liltok/internal/db"
)

// DefaultSemanticThreshold is the minimum cosine similarity required for a Tier-3 hit.
const DefaultSemanticThreshold float32 = 0.95

// CosineSimilarity calculates the cosine similarity between two float32 vectors.
func CosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		ai := float64(a[i])
		bi := float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	sim := float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
	if sim > 1.0 {
		sim = 1.0
	} else if sim < -1.0 {
		sim = -1.0
	}
	return sim
}

// Float32SliceToBytes serializes a float32 slice into little-endian bytes for SQLite BLOB storage.
func Float32SliceToBytes(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:(i+1)*4], math.Float32bits(v))
	}
	return buf
}

// BytesToFloat32Slice deserializes little-endian bytes back into a float32 slice.
func BytesToFloat32Slice(buf []byte) []float32 {
	if len(buf)%4 != 0 {
		return nil
	}
	count := len(buf) / 4
	vec := make([]float32, count)
	for i := 0; i < count; i++ {
		bits := binary.LittleEndian.Uint32(buf[i*4 : (i+1)*4])
		vec[i] = math.Float32frombits(bits)
	}
	return vec
}

// SemanticEntry represents a cached response along with its semantic embedding vector and guardrail hashes.
type SemanticEntry struct {
	Hash            string    `json:"hash"`
	Model           string    `json:"model"`
	SystemHash      string    `json:"system_hash"`
	ToolsHash       string    `json:"tools_hash"`
	PromptText      string    `json:"prompt_text"`
	Vector          []float32 `json:"vector"`
	ResponsePayload []byte    `json:"response_payload"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// VectorIndex maintains an in-memory index of vectors with optional SQLite persistence.
type VectorIndex struct {
	mu      sync.RWMutex
	entries []*SemanticEntry
	db      *db.DB
}

// NewVectorIndex creates a new in-memory VectorIndex and loads active embeddings from SQLite if present.
func NewVectorIndex(database *db.DB) *VectorIndex {
	idx := &VectorIndex{
		entries: make([]*SemanticEntry, 0, 1024),
		db:      database,
	}

	if database != nil {
		_ = idx.loadFromDB()
	}

	return idx
}

func (idx *VectorIndex) loadFromDB() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	rows, err := idx.db.Query(`
		SELECT se.cache_hash, se.embedding, se.dimension, ce.model, ce.response_payload
		FROM semantic_embeddings se
		JOIN cache_entries ce ON se.cache_hash = ce.hash
		WHERE datetime(ce.created_at, '+' || ce.ttl_seconds || ' seconds') > CURRENT_TIMESTAMP
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var hash, model string
		var rawBlob []byte
		var dim int
		var payload []byte

		if err := rows.Scan(&hash, &rawBlob, &dim, &model, &payload); err == nil {
			vec := BytesToFloat32Slice(rawBlob)
			if len(vec) == dim {
				idx.entries = append(idx.entries, &SemanticEntry{
					Hash:            hash,
					Model:           model,
					Vector:          vec,
					ResponsePayload: payload,
					CreatedAt:       time.Now(),
					ExpiresAt:       time.Now().Add(7 * 24 * time.Hour),
				})
			}
		}
	}

	return nil
}

// Search searches for the closest cached vector matching the query vector and guardrails.
func (idx *VectorIndex) Search(ctx context.Context, model, systemHash, toolsHash string, queryVec []float32, threshold float32) (*SemanticEntry, float32, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if len(queryVec) == 0 || len(idx.entries) == 0 {
		return nil, 0, false
	}

	now := time.Now()
	var bestEntry *SemanticEntry
	var bestSim float32 = -1.0

	for _, entry := range idx.entries {
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			continue
		}
		// Strict guardrail check: Model, System prompt, and Tool declarations must match
		if entry.Model != model {
			continue
		}
		if systemHash != "" && entry.SystemHash != "" && entry.SystemHash != systemHash {
			continue
		}
		if toolsHash != "" && entry.ToolsHash != "" && entry.ToolsHash != toolsHash {
			continue
		}

		sim := CosineSimilarity(queryVec, entry.Vector)
		if sim > bestSim {
			bestSim = sim
			bestEntry = entry
		}
	}

	if bestSim >= threshold && bestEntry != nil {
		return bestEntry, bestSim, true
	}

	return nil, bestSim, false
}

// Insert inserts a new semantic entry into memory and persists to SQLite.
func (idx *VectorIndex) Insert(ctx context.Context, entry *SemanticEntry) error {
	if entry == nil || len(entry.Vector) == 0 {
		return nil
	}

	idx.mu.Lock()
	idx.entries = append(idx.entries, entry)
	idx.mu.Unlock()

	if idx.db != nil {
		blob := Float32SliceToBytes(entry.Vector)
		_, err := idx.db.ExecContext(ctx, `
			INSERT OR REPLACE INTO semantic_embeddings (cache_hash, embedding, dimension, created_at)
			VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		`, entry.Hash, blob, len(entry.Vector))
		return err
	}

	return nil
}

// Size returns count of active entries.
func (idx *VectorIndex) Size() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.entries)
}
