package miner_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/cache"
	"github.com/primaybr/liltok/internal/miner"
)

var testPrompts = []miner.PromptItem{
	{ID: "t-1", UserPrompt: "How do I reverse a slice in Go?"},
	{ID: "t-2", SystemPrompt: "You are an expert SQL engineer.", UserPrompt: "When should I use a covering index?"},
}

// minedEntry builds a cache entry the way the miner stores one: the Anthropic shape
// (max_tokens 4096, system as a field) or the OpenAI shape (temperature 0, system as a message).
func minedEntry(t *testing.T, p miner.PromptItem, model string, anthropic bool, answer string) miner.CacheExportItem {
	t.Helper()
	var req map[string]interface{}
	var resp interface{}
	if anthropic {
		req = map[string]interface{}{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": p.UserPrompt}},
			"max_tokens": 4096,
		}
		if p.SystemPrompt != "" {
			req["system"] = p.SystemPrompt
		}
		resp = map[string]interface{}{"type": "message", "content": []map[string]string{{"type": "text", "text": answer}}}
	} else {
		msgs := []map[string]string{{"role": "user", "content": p.UserPrompt}}
		if p.SystemPrompt != "" {
			msgs = append([]map[string]string{{"role": "system", "content": p.SystemPrompt}}, msgs...)
		}
		req = map[string]interface{}{"model": model, "messages": msgs, "temperature": 0.0}
		resp = map[string]interface{}{"choices": []map[string]interface{}{{"message": map[string]string{"role": "assistant", "content": answer}}}}
	}
	return entryFor(t, model, req, resp)
}

func entryFor(t *testing.T, model string, req map[string]interface{}, resp interface{}) miner.CacheExportItem {
	t.Helper()
	reqBytes, _ := json.Marshal(req)
	norm, err := cache.NormalizePayload(reqBytes, cache.NormalizationOptions{CacheNonzeroTemperature: true})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	respBytes, _ := json.Marshal(resp)
	return miner.CacheExportItem{Hash: norm.Hash, Model: model, NormalizedPrompt: norm.CanonicalJSON, ResponsePayload: string(respBytes)}
}

func TestStarterFilter(t *testing.T) {
	f := miner.NewStarterFilter(testPrompts)

	sessionReq := map[string]interface{}{
		"model":      "claude-sonnet-5",
		"max_tokens": 4096,
		"system":     "You are Claude Code. The user's email address is dev@corp.example.",
		"messages":   []map[string]string{{"role": "user", "content": "<system-reminder>cwd: /home/alice/secret-project</system-reminder>\nHow do I reverse a slice in Go?"}},
	}
	withTools := map[string]interface{}{
		"model":      "gpt-4o",
		"max_tokens": 4096,
		"messages":   []map[string]string{{"role": "user", "content": testPrompts[0].UserPrompt}},
		"tools":      []map[string]string{{"name": "Bash"}},
	}
	edited := minedEntry(t, testPrompts[0], "gpt-4o", true, "use slices.Reverse")
	edited.NormalizedPrompt = strings.Replace(edited.NormalizedPrompt, "Go?", "Go!", 1)

	cases := []struct {
		name string
		item miner.CacheExportItem
		want string
	}{
		{"mined anthropic shape", minedEntry(t, testPrompts[0], "gpt-4o", true, "use slices.Reverse"), ""},
		{"mined openai shape with system", minedEntry(t, testPrompts[1], "claude-opus-5", false, "when the query reads only indexed columns"), ""},
		{"wrong system prompt", minedEntry(t, miner.PromptItem{SystemPrompt: "Other.", UserPrompt: testPrompts[1].UserPrompt}, "gpt-4o", true, "x"), miner.RejectNotCurated},
		{"uncurated question", minedEntry(t, miner.PromptItem{UserPrompt: "How do I deploy my-internal-app?"}, "gpt-4o", true, "x"), miner.RejectNotCurated},
		{"agent session turn", entryFor(t, "claude-sonnet-5", sessionReq, map[string]interface{}{"content": []map[string]string{{"type": "text", "text": "ok"}}}), miner.RejectNotCurated},
		{"tools declared", entryFor(t, "gpt-4o", withTools, map[string]interface{}{"content": []map[string]string{{"type": "text", "text": "ok"}}}), miner.RejectNotCurated},
		{"prompt edited after hashing", edited, miner.RejectPromptEdit},
		{"response not json", func() miner.CacheExportItem {
			it := minedEntry(t, testPrompts[0], "gpt-4o", true, "x")
			it.ResponsePayload = `{"content": [broken`
			return it
		}(), miner.RejectBadResponse},
		{"empty answer", minedEntry(t, testPrompts[0], "gpt-4o", false, "  "), miner.RejectEmptyAnswer},
		{"placeholder answer", minedEntry(t, testPrompts[0], "gpt-4o", true, "### Q\n\nTo solve this task efficiently, follow standard industry best practices:\n1. ..."), miner.RejectPlaceholder},
	}
	for _, tc := range cases {
		if got := f.Check(tc.item); got != tc.want {
			t.Errorf("%s: Check = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestEmbeddedStarterPack_OnlyCuratedEntries guards the pack that ships inside every binary and is
// published as a release asset: each entry must be a mined answer to a curated corpus prompt, and
// no entry may carry client context markers (the 0.1.8-0.2.4 packs shipped agent session requests).
func TestEmbeddedStarterPack_OnlyCuratedEntries(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "db", "starter_cache.json.gz"))
	if err != nil {
		t.Fatalf("read embedded pack: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("embedded pack is not gzip: %v", err)
	}
	var items []miner.CacheExportItem
	if err := json.NewDecoder(zr).Decode(&items); err != nil {
		t.Fatalf("embedded pack is not a JSON item list: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("embedded pack is empty")
	}

	// Context blocks as Claude Code writes them; a bare "userEmail" can appear in model-written code.
	markers := regexp.MustCompile(`(?i)<system-reminder>|# userEmail|# claudeMd|# gitStatus|[A-Za-z]:[\\/]+Users[\\/]`)
	f := miner.NewStarterFilter(nil)
	bad := 0
	for _, it := range items {
		reason := f.Check(it)
		if reason == "" && markers.MatchString(it.NormalizedPrompt+it.ResponsePayload) {
			reason = "context_marker"
		}
		if reason != "" {
			if bad < 10 {
				t.Errorf("entry %s (%s) not allowed in the starter pack: %s", it.Hash, it.Model, reason)
			}
			bad++
		}
	}
	if bad > 0 {
		t.Fatalf("%d of %d embedded entries are not allowed; rebuild with `go run ./cmd/build_starter_cache`", bad, len(items))
	}
}
