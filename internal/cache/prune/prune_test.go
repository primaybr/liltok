package prune_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/cache/prune"
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

func TestPruneJSONPayload_SessionCompactorEndToEnd(t *testing.T) {
	opts := prune.DefaultOptions()
	opts.RecentTurnsToKeep = 2
	opts.CompactorHeadBytes = 30
	opts.CompactorTailBytes = 30
	opts.CompactorMinSizeBytes = 100
	p := prune.NewPruner(opts)

	hugeOldLog := strings.Repeat("HISTORICAL_LOG_ENTRY_WITH_LONG_DETAILS\n", 40) + "DONE_EXIT_0"

	payload := map[string]interface{}{
		"model": "claude-3-7-sonnet-20250219",
		"messages": []map[string]interface{}{
			// Turn 1 (Historical)
			{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "tool_result", "tool_use_id": "c1", "content": hugeOldLog},
				},
			},
			{"role": "assistant", "content": "Done with turn 1."},
			// Turn 2 (Recent)
			{"role": "user", "content": "Recent question"},
			{"role": "assistant", "content": "Recent answer"},
			// Turn 3 (Recent)
			{"role": "user", "content": "Final prompt"},
		},
	}
	rawBytes, _ := json.Marshal(payload)

	prunedJSON, stats, err := p.PruneJSONPayload(rawBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.SavedBytes <= 0 {
		t.Fatalf("expected positive saved bytes, got %d", stats.SavedBytes)
	}
	if !strings.Contains(string(prunedJSON), "output truncated:") {
		t.Errorf("expected session compactor indicator in output: %s", string(prunedJSON))
	}
	if !strings.Contains(string(prunedJSON), "HISTORICAL_LOG_ENTRY") || !strings.Contains(string(prunedJSON), "DONE_EXIT_0") {
		t.Errorf("expected head and tail preserved in historical output")
	}
}

// Tool output is what the agent later copies into Edit old_string, so it must reach the model verbatim.
func TestPruneJSONPayload_PreservesToolResultsVerbatim(t *testing.T) {
	readOutput := "1\tpackage config\n2\t\n3\t// ==========================\n4\tvar x = 1   \n\n\n\n8\tvar y = 2\n"
	p := prune.NewPruner(prune.DefaultOptions())

	anthropic := map[string]interface{}{
		"model": "claude-sonnet-5",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "edit config"},
			{"role": "assistant", "content": []map[string]interface{}{{"type": "tool_use", "id": "r1", "name": "Read", "input": map[string]string{"file_path": "config.go"}}}},
			{"role": "user", "content": []map[string]interface{}{
				{"type": "tool_result", "tool_use_id": "r1", "content": readOutput},
				{"type": "tool_result", "tool_use_id": "r2", "content": []map[string]interface{}{{"type": "text", "text": readOutput}}},
			}},
		},
	}
	openai := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "edit config"},
			{"role": "tool", "tool_call_id": "r1", "content": readOutput},
		},
	}

	for name, payload := range map[string]interface{}{"anthropic": anthropic, "openai": openai} {
		raw, _ := json.Marshal(payload)
		out, _, err := p.PruneJSONPayload(raw)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		var decoded struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(out, &decoded); err != nil {
			t.Fatalf("%s: unmarshal failed: %v", name, err)
		}
		expected, _ := json.Marshal(readOutput)
		last := string(decoded.Messages[len(decoded.Messages)-1])
		if strings.Count(last, string(expected)) != strings.Count(string(mustMarshal(payload)), string(expected)) {
			t.Errorf("%s: tool result content was modified by the pruner:\n%s", name, last)
		}
	}
}

func mustMarshal(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
