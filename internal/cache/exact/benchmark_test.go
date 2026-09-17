package exact

import (
	"fmt"
	"testing"

	"github.com/liltok/liltok/internal/cache"
)

func BenchmarkCanonicalNormalizer(b *testing.B) {
	payload := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "You are a helpful coding assistant."},
			{"role": "user", "content": "Write a fast Go HTTP handler for parsing JSON."}
		],
		"temperature": 0.0,
		"top_p": 1.0,
		"stream": false
	}`)

	opts := cache.NormalizationOptions{CacheNonzeroTemperature: false}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := cache.NormalizePayload(payload, opts)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkL1LRUHit(b *testing.B) {
	lru, err := NewMemoryLRU(10000)
	if err != nil {
		b.Fatal(err)
	}

	// Pre-populate 1,000 entries
	for i := 0; i < 1000; i++ {
		hash := fmt.Sprintf("hash_%06d", i)
		lru.Set(&cache.CacheEntry{
			Hash:            hash,
			Model:           "gpt-4o",
			ResponsePayload: []byte(`{"id":"chatcmpl-123","choices":[{"message":{"content":"Hello world!"}}]}`),
		})
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			hash := fmt.Sprintf("hash_%06d", i%1000)
			_, hit := lru.Get(hash)
			if !hit {
				b.Fatal("expected cache hit")
			}
			i++
		}
	})
}
