package semantic

import (
	"context"
	"testing"
)

func BenchmarkFastLocalEmbedder(b *testing.B) {
	embedder := NewFastLocalEmbedder(256)
	query := "How do I optimize SQL queries with an index in SQLite?"
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := embedder.Embed(ctx, query)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCosineSimilarity(b *testing.B) {
	embedder := NewFastLocalEmbedder(256)
	ctx := context.Background()
	vec1, _ := embedder.Embed(ctx, "How do I optimize SQL queries with an index in SQLite?")
	vec2, _ := embedder.Embed(ctx, "How do I speed up SQLite queries using indexes?")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sim := CosineSimilarity(vec1, vec2)
		if sim <= 0 {
			b.Fatal("unexpected similarity <= 0")
		}
	}
}
