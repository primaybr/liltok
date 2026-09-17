package cache

import (
	"testing"
)

func TestNormalizerKeyOrderInvariance(t *testing.T) {
	jsonA := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"temperature":0.0,"stream":true}`)
	jsonB := []byte(`{"stream":false,"temperature":0,"messages":[{"content":"hello","role":"user"}],"model":"gpt-4o"}`)

	opts := NormalizationOptions{CacheNonzeroTemperature: false}

	normA, err := NormalizePayload(jsonA, opts)
	if err != nil {
		t.Fatalf("unexpected error normalizing A: %v", err)
	}

	normB, err := NormalizePayload(jsonB, opts)
	if err != nil {
		t.Fatalf("unexpected error normalizing B: %v", err)
	}

	if normA.Hash != normB.Hash {
		t.Errorf("expected identical hashes, got %s vs %s", normA.Hash, normB.Hash)
	}
	if !normA.IsCacheable || !normB.IsCacheable {
		t.Errorf("expected both to be cacheable with temp 0")
	}
}

func TestNormalizerToolOrderInvariance(t *testing.T) {
	jsonA := []byte(`{
		"model": "gpt-4o",
		"messages": [{"role":"user","content":"call tool"}],
		"tools": [
			{"type": "function", "function": {"name": "search_web"}},
			{"type": "function", "function": {"name": "analyze_code"}}
		]
	}`)

	jsonB := []byte(`{
		"model": "gpt-4o",
		"messages": [{"role":"user","content":"call tool"}],
		"tools": [
			{"type": "function", "function": {"name": "analyze_code"}},
			{"type": "function", "function": {"name": "search_web"}}
		]
	}`)

	opts := NormalizationOptions{}
	normA, err := NormalizePayload(jsonA, opts)
	if err != nil {
		t.Fatalf("normalize A failed: %v", err)
	}
	normB, err := NormalizePayload(jsonB, opts)
	if err != nil {
		t.Fatalf("normalize B failed: %v", err)
	}

	if normA.Hash != normB.Hash {
		t.Errorf("expected identical hashes for reordered tools, got %s vs %s", normA.Hash, normB.Hash)
	}
}

func TestNormalizerTemperaturePolicy(t *testing.T) {
	// Temp > 0 without seed -> uncacheable by default
	jsonUncached := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)
	normUncached, err := NormalizePayload(jsonUncached, NormalizationOptions{CacheNonzeroTemperature: false})
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	if normUncached.IsCacheable {
		t.Errorf("expected temp 0.7 without seed to be uncacheable")
	}

	// Temp > 0 with seed -> cacheable
	jsonSeeded := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"seed":42}`)
	normSeeded, err := NormalizePayload(jsonSeeded, NormalizationOptions{CacheNonzeroTemperature: false})
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	if !normSeeded.IsCacheable {
		t.Errorf("expected temp 0.7 with seed to be cacheable")
	}

	// Temp > 0 with CacheNonzeroTemperature override
	normOverride, err := NormalizePayload(jsonUncached, NormalizationOptions{CacheNonzeroTemperature: true})
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	if !normOverride.IsCacheable {
		t.Errorf("expected temp 0.7 with override to be cacheable")
	}
}
