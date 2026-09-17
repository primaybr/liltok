package semantic_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/cache/semantic"
)

func TestGuardrails(t *testing.T) {
	tests := []struct {
		prompt   string
		eligible bool
	}{
		{"What is the capital of France?", true},
		{"How do I write a binary search tree in Go?", true},
		{"What is the current time in Tokyo right now?", false},
		{"Tell me what day today is.", false},
		{"Generate a random uuid for my user table", false},
		{"", false},
	}

	for _, tt := range tests {
		dec := semantic.CheckSemanticEligibility(tt.prompt)
		if dec.IsEligible != tt.eligible {
			t.Errorf("Prompt %q: expected eligible=%v, got=%v (reason: %s)", tt.prompt, tt.eligible, dec.IsEligible, dec.Reason)
		}
	}
}

func TestFastLocalEmbedder(t *testing.T) {
	emb := semantic.NewFastLocalEmbedder(256)
	if emb.Dimension() != 256 {
		t.Errorf("Expected dimension 256, got %d", emb.Dimension())
	}

	vec, err := emb.Embed(context.Background(), "How to write quicksort in Go")
	if err != nil {
		t.Fatalf("Unexpected embed error: %v", err)
	}
	if len(vec) != 256 {
		t.Fatalf("Expected vector length 256, got %d", len(vec))
	}

	// Verify L2 norm is 1.0
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSq)
	if math.Abs(norm-1.0) > 1e-4 {
		t.Errorf("Expected L2 norm ~1.0, got %f", norm)
	}

	// Phrasing variation test: "How to write quicksort in Go" vs "Please show how to write quicksort in Go"
	vec2, _ := emb.Embed(context.Background(), "Please show how to write quicksort in Go")
	sim := semantic.CosineSimilarity(vec, vec2)
	if sim < 0.80 {
		t.Errorf("Expected high similarity between phrasing variations, got %f", sim)
	}

	// Unrelated prompt test
	vecUnrelated, _ := emb.Embed(context.Background(), "The weather forecast in Antarctica")
	simUnrelated := semantic.CosineSimilarity(vec, vecUnrelated)
	if simUnrelated > 0.4 {
		t.Errorf("Expected low similarity for unrelated text, got %f", simUnrelated)
	}
}

func TestCosineSimilarity(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	c := []float32{0, 1, 0}
	d := []float32{-1, 0, 0}

	if sim := semantic.CosineSimilarity(a, b); math.Abs(float64(sim)-1.0) > 1e-5 {
		t.Errorf("Expected identical similarity 1.0, got %f", sim)
	}
	if sim := semantic.CosineSimilarity(a, c); math.Abs(float64(sim)) > 1e-5 {
		t.Errorf("Expected orthogonal similarity 0.0, got %f", sim)
	}
	if sim := semantic.CosineSimilarity(a, d); math.Abs(float64(sim)-(-1.0)) > 1e-5 {
		t.Errorf("Expected opposite similarity -1.0, got %f", sim)
	}
}

func TestBinarySerialization(t *testing.T) {
	original := []float32{0.123, -0.456, 1.789, 0.0}
	blob := semantic.Float32SliceToBytes(original)
	restored := semantic.BytesToFloat32Slice(blob)

	if len(restored) != len(original) {
		t.Fatalf("Restored length mismatch: %d vs %d", len(restored), len(original))
	}
	for i := range original {
		if math.Abs(float64(original[i]-restored[i])) > 1e-6 {
			t.Errorf("Index %d mismatch: %f vs %f", i, original[i], restored[i])
		}
	}
}

func TestSemanticCache_Flow(t *testing.T) {
	cache := semantic.NewSemanticCache(nil, semantic.NewFastLocalEmbedder(256), 0.90)

	ctx := context.Background()
	query1 := "Explain the quicksort algorithm in Go"
	payload := []byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"Quicksort is..."}}]}`)

	// Store query 1
	err := cache.Store(ctx, "hash-1", "gpt-4o", "You are a teacher", `[{"name":"foo"}]`, query1, payload, 1*time.Hour)
	if err != nil {
		t.Fatalf("Failed to store in semantic cache: %v", err)
	}

	if count := cache.Index().Size(); count != 1 {
		t.Fatalf("Expected index size 1, got %d", count)
	}

	// Lookup query 1 (exact query text) -> must hit
	entry, sim, hit := cache.Lookup(ctx, "gpt-4o", "You are a teacher", `[{"name":"foo"}]`, query1)
	if !hit || entry == nil {
		t.Fatalf("Expected semantic cache HIT for identical query, got hit=%v", hit)
	}
	if sim < 0.99 {
		t.Errorf("Expected sim ~1.0, got %f", sim)
	}

	// Lookup with slight phrasing variant
	variantQuery := "Please explain the quicksort algorithm in Go"
	entry2, sim2, hit2 := cache.Lookup(ctx, "gpt-4o", "You are a teacher", `[{"name":"foo"}]`, variantQuery)
	t.Logf("variant query sim2: %f, hit2: %v", sim2, hit2)
	if !hit2 || entry2 == nil {
		t.Fatalf("Expected semantic cache HIT for variant query, got hit=%v (sim: %f)", hit2, sim2)
	}
	if sim2 < 0.90 {
		t.Errorf("Expected sim >= 0.90, got %f", sim2)
	}

	// Lookup with mismatched system prompt -> must MISS (guardrail)
	_, _, hitMismatchSys := cache.Lookup(ctx, "gpt-4o", "Different system prompt", `[{"name":"foo"}]`, query1)
	if hitMismatchSys {
		t.Errorf("Expected MISS when system prompt differs")
	}

	// Lookup with mismatched tools -> must MISS (guardrail)
	_, _, hitMismatchTools := cache.Lookup(ctx, "gpt-4o", "You are a teacher", `[{"name":"bar"}]`, query1)
	if hitMismatchTools {
		t.Errorf("Expected MISS when tools differ")
	}

	// Ineligible query (temporal) -> must bypass
	_, _, hitIneligible := cache.Lookup(ctx, "gpt-4o", "You are a teacher", `[{"name":"foo"}]`, "What is today's stock price?")
	if hitIneligible {
		t.Errorf("Expected bypass for temporal query")
	}
}

func TestOllamaEmbedder(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		// Return 4-dimensional normalized vector
		resp := map[string]interface{}{
			"embedding": []float32{0.5, 0.5, 0.5, 0.5},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	emb := semantic.NewOllamaEmbedder(mockServer.URL, "nomic-embed-text", 2*time.Second)
	vec, err := emb.Embed(context.Background(), "Test sentence")
	if err != nil {
		t.Fatalf("Unexpected error from OllamaEmbedder: %v", err)
	}
	if len(vec) != 4 {
		t.Fatalf("Expected vector length 4, got %d", len(vec))
	}
}
