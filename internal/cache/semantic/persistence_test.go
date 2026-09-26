package semantic_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/primaybr/liltok/internal/cache/semantic"
	"github.com/primaybr/liltok/internal/db"
)

func openSemanticDB(t *testing.T, path string) *db.DB {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return d
}

func insertCacheRow(t *testing.T, d *db.DB, hash string) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO cache_entries (hash, model, normalized_prompt, response_payload, ttl_seconds)
		VALUES (?, 'gpt-4o', '{}', ?, 3600)`, hash, []byte(`{"choices":[]}`)); err != nil {
		t.Fatalf("insert cache row: %v", err)
	}
}

// Entries reloaded from SQLite must keep their system-prompt and tools scope; before migration 010
// they came back with empty hashes, which Search treated as a wildcard.
func TestSemanticScopeSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sem.db")
	emb := semantic.NewFastLocalEmbedder(256)
	ctx := context.Background()
	query := "Explain the quicksort algorithm in Go"

	d := openSemanticDB(t, path)
	insertCacheRow(t, d, "hash-1")
	sc := semantic.NewSemanticCache(d, emb, 0.9)
	if err := sc.Store(ctx, "hash-1", "gpt-4o", "You are a teacher", `[{"name":"foo"}]`, query, []byte(`{"choices":[]}`), time.Hour); err != nil {
		t.Fatalf("store: %v", err)
	}
	_ = d.Close()

	d = openSemanticDB(t, path)
	defer d.Close()
	reloaded := semantic.NewSemanticCache(d, emb, 0.9)
	if n := reloaded.Index().Size(); n != 1 {
		t.Fatalf("reloaded index size = %d, want 1", n)
	}
	if _, _, hit := reloaded.Lookup(ctx, "gpt-4o", "Different system prompt", `[{"name":"foo"}]`, query); hit {
		t.Error("reloaded entry matched a different system prompt")
	}
	if _, _, hit := reloaded.Lookup(ctx, "gpt-4o", "You are a teacher", `[{"name":"bar"}]`, query); hit {
		t.Error("reloaded entry matched different tools")
	}
	if _, _, hit := reloaded.Lookup(ctx, "gpt-4o", "You are a teacher", `[{"name":"foo"}]`, query); !hit {
		t.Error("reloaded entry did not match its own scope")
	}
}

func TestSemanticReloadSkipsExpiredAndLegacyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sem.db")
	emb := semantic.NewFastLocalEmbedder(256)
	ctx := context.Background()

	d := openSemanticDB(t, path)
	for _, h := range []string{"live", "expired", "legacy"} {
		insertCacheRow(t, d, h)
	}
	idx := semantic.NewSemanticCache(d, emb, 0.9).Index()
	vec, _ := emb.Embed(ctx, "some question text")
	for _, e := range []*semantic.SemanticEntry{
		{Hash: "live", Model: "gpt-4o", SystemHash: "s", ToolsHash: "t", Vector: vec, ExpiresAt: time.Now().Add(time.Hour)},
		{Hash: "expired", Model: "gpt-4o", SystemHash: "s", ToolsHash: "t", Vector: vec, ExpiresAt: time.Now().Add(-time.Minute)},
	} {
		if err := idx.Insert(ctx, e); err != nil {
			t.Fatalf("insert %s: %v", e.Hash, err)
		}
	}
	// A row written before migration 010 has no expiry and empty hashes.
	if _, err := d.Exec(`INSERT INTO semantic_embeddings (cache_hash, embedding, dimension) VALUES ('legacy', ?, ?)`,
		semantic.Float32SliceToBytes(vec), len(vec)); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	_ = d.Close()

	d = openSemanticDB(t, path)
	defer d.Close()
	if n := semantic.NewSemanticCache(d, emb, 0.9).Index().Size(); n != 1 {
		t.Fatalf("reloaded index size = %d, want 1 (only the live entry)", n)
	}
}
