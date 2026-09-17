package prune_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/liltok/liltok/internal/cache/prune"
)

func TestStripWhitespaceAndDividers(t *testing.T) {
	input := `
func Hello() {
    // ===================================
    var a = 1;   
    /* ----------------------------- */
    var b = 2;


    # #################################
    return a + b;
}
`
	cleaned := prune.StripWhitespaceAndDividers(input)
	if strings.Contains(cleaned, "======") {
		t.Errorf("Expected divider comments to be stripped, got: %s", cleaned)
	}
	if strings.Contains(cleaned, "------") {
		t.Errorf("Expected divider comments to be stripped, got: %s", cleaned)
	}
	if strings.Contains(cleaned, "######") {
		t.Errorf("Expected divider comments to be stripped, got: %s", cleaned)
	}
	if strings.Contains(cleaned, "\n\n\n") {
		t.Errorf("Expected consecutive newlines to be collapsed, got: %s", cleaned)
	}
}

func TestCompactDiff(t *testing.T) {
	diff := `diff --git a/internal/cache/cache.go b/internal/cache/cache.go
index abcdef12..34567890 100644
--- a/internal/cache/cache.go
+++ b/internal/cache/cache.go
@@ -1,15 +1,15 @@
 package cache
 
 import (
 	"context"
 	"fmt"
 	"time"
 )
 
-func OldFunction() {
+func NewFunction() {
 	var x = 1
 	var y = 2
 	var z = 3
 	var a = 4
 	var b = 5
 }
`
	compacted := prune.CompactDiff(diff)
	if strings.Contains(compacted, "index abcdef12..34567890") {
		t.Errorf("Expected git index metadata line to be stripped, got: %s", compacted)
	}
	if !strings.Contains(compacted, "-func OldFunction()") {
		t.Errorf("Expected removed line to remain intact, got: %s", compacted)
	}
	if !strings.Contains(compacted, "+func NewFunction()") {
		t.Errorf("Expected added line to remain intact, got: %s", compacted)
	}
	if len(compacted) >= len(diff) {
		t.Errorf("Expected compacted diff to be smaller than original: orig %d, compacted %d", len(diff), len(compacted))
	}
}

func TestCompactTree(t *testing.T) {
	tree := `
Project Layout:
├── cmd/
│   └── main.go
├── internal/
│   ├── cache/
│   │   ├── cache.go
│   │   └── normalizer.go
│   └── proxy/
│       └── proxy.go
└── README.md
`
	compacted := prune.CompactTree(tree)
	if !strings.Contains(compacted, "cmd/main.go") {
		t.Errorf("Expected cmd/main.go in compacted output, got: %s", compacted)
	}
	if !strings.Contains(compacted, "internal/cache/{cache.go, normalizer.go}") {
		t.Errorf("Expected grouped files in internal/cache/, got: %s", compacted)
	}
	if !strings.Contains(compacted, "internal/proxy/proxy.go") {
		t.Errorf("Expected internal/proxy/proxy.go, got: %s", compacted)
	}
	if len(compacted) >= len(tree) {
		t.Errorf("Expected compacted tree to be smaller than original: orig %d, compacted %d", len(tree), len(compacted))
	}
}

func TestPrunerJSONPayload_OpenAI(t *testing.T) {
	pruner := prune.NewPruner(prune.DefaultOptions())

	originalPayload := `{
		"model": "gpt-4o",
		"messages": [
			{
				"role": "user",
				"content": "Please review this diff:\ndiff --git a/foo.go b/foo.go\nindex 123456..789012 100644\n--- a/foo.go\n+++ b/foo.go\n@@ -1,10 +1,10 @@\n line 1\n line 2\n line 3\n line 4\n-old code\n+new code\n line 5\n line 6\n line 7\n line 8\n"
			}
		]
	}`

	prunedJSON, stats, err := pruner.PruneJSONPayload([]byte(originalPayload))
	if err != nil {
		t.Fatalf("Unexpected error pruning JSON: %v", err)
	}

	if stats.SavedBytes <= 0 {
		t.Errorf("Expected positive saved bytes, got %d", stats.SavedBytes)
	}

	var root map[string]interface{}
	if err := json.Unmarshal(prunedJSON, &root); err != nil {
		t.Fatalf("Failed to parse pruned JSON: %v", err)
	}

	msgs := root["messages"].([]interface{})
	firstMsg := msgs[0].(map[string]interface{})
	content := firstMsg["content"].(string)

	if strings.Contains(content, "index 123456..789012") {
		t.Errorf("Expected index line stripped from pruned payload, got: %s", content)
	}
	if !strings.Contains(content, "-old code") || !strings.Contains(content, "+new code") {
		t.Errorf("Expected code change intact, got: %s", content)
	}
}

func TestPrunerJSONPayload_Anthropic(t *testing.T) {
	pruner := prune.NewPruner(prune.DefaultOptions())

	originalPayload := `{
		"model": "claude-3-5-sonnet-20241022",
		"system": "You are a helpful assistant.\n// ======================================\nAlways be concise.",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "text",
						"text": "Check files:\n├── src/\n│   ├── a.ts\n│   └── b.ts\n"
					}
				]
			}
		]
	}`

	prunedJSON, stats, err := pruner.PruneJSONPayload([]byte(originalPayload))
	if err != nil {
		t.Fatalf("Unexpected error pruning Anthropic JSON: %v", err)
	}

	if stats.SavedBytes <= 0 {
		t.Errorf("Expected positive saved bytes for Anthropic payload, got %d", stats.SavedBytes)
	}

	var root map[string]interface{}
	_ = json.Unmarshal(prunedJSON, &root)
	sys := root["system"].(string)
	if strings.Contains(sys, "=================") {
		t.Errorf("Expected divider removed from system prompt, got: %s", sys)
	}
}

func TestPruner_NonDestructive(t *testing.T) {
	pruner := prune.NewPruner(prune.DefaultOptions())
	cleanText := "Simple question: what is 2 + 2?"

	res, saved := pruner.PruneText(cleanText)
	if saved != 0 {
		t.Errorf("Expected 0 saved bytes on clean text, got %d", saved)
	}
	if res != cleanText {
		t.Errorf("Expected identical text, got %q", res)
	}
}
