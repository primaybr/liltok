package tokens

import (
	"testing"
)

func BenchmarkCountTokens(b *testing.B) {
	prompt := "You are a professional software engineer with deep expertise in Go concurrency, SQLite WAL architecture, and reverse proxy design. Build a high performance HTTP router."
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = CountTokens("gpt-4o", prompt)
	}
}

func BenchmarkPricingCalculate(b *testing.B) {
	reg := NewPricingRegistry(nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = reg.Calculate("gpt-4o", 1500, 400, 1000, "HIT", "TIER1_EXACT")
	}
}
