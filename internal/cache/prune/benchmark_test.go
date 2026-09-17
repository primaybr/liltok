package prune

import (
	"testing"
)

func BenchmarkDiffCompactor(b *testing.B) {
	diff := `diff --git a/server.go b/server.go
index 83a45c..92b11f 100644
--- a/server.go
+++ b/server.go
@@ -10,7 +10,7 @@ import (
  "context"
  "fmt"
- "old/package"
+ "new/package"
  "net/http"
  "time"
`
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = CompactDiff(diff)
	}
}

func BenchmarkTreeCompactor(b *testing.B) {
	tree := `
.
├── cmd/
│   └── liltok/
│       └── main.go
├── internal/
│   ├── cache/
│   │   ├── cache.go
│   │   └── normalizer.go
│   └── server/
│       └── server.go
└── Makefile
`
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = CompactTree(tree)
	}
}
