package miner_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/miner"
)

func TestGetCuratedPrompts(t *testing.T) {
	miner.ResetCuratedCache()
	all := miner.GetCuratedPrompts("all")
	t.Logf("Total curated prompts loaded: %d", len(all))
	if len(all) < 800 {
		t.Fatalf("expected at least 800 curated prompts across expanded corpus, got %d", len(all))
	}

	categories := []string{"php", "kubernetes", "docker", "flutter", "python", "go", "c", "ui_ux"}
	for _, cat := range categories {
		prompts := miner.GetCuratedPrompts(cat)
		t.Logf("Category %s: %d prompts", cat, len(prompts))
		if len(prompts) < 100 {
			t.Errorf("expected at least 100 prompts for category %s, got %d", cat, len(prompts))
		}
	}

	// Verify legacy query filters
	errorsOnly := miner.GetCuratedPrompts("errors")
	if len(errorsOnly) == 0 {
		t.Errorf("expected error prompts, got 0")
	}

	codingOnly := miner.GetCuratedPrompts("coding")
	if len(codingOnly) == 0 {
		t.Errorf("expected coding prompts, got 0")
	}
}

func TestGetCuratedPromptsByLanguage(t *testing.T) {
	languages := []string{"go", "php", "python", "flutter", "c"}
	for _, lang := range languages {
		prompts := miner.GetCuratedPromptsByLanguage(lang)
		if len(prompts) < 100 {
			t.Errorf("expected at least 100 prompts for language %s, got %d", lang, len(prompts))
		}
	}

	jsPrompts := miner.GetCuratedPromptsByLanguage("javascript")
	if len(jsPrompts) == 0 {
		t.Errorf("expected JavaScript prompts, got 0")
	}
}

func TestLoadPromptsFromFile(t *testing.T) {
	tmpDir := t.TempDir()
	promptFile := filepath.Join(tmpDir, "test_prompts.txt")

	content := `# Comments should be ignored
How do I implement merge sort in Go?

What is the difference between TCP and UDP?
# Another comment
Explain raft consensus algorithm
`
	if err := os.WriteFile(promptFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test prompts file: %v", err)
	}

	items, err := miner.LoadPromptsFromFile(promptFile)
	if err != nil {
		t.Fatalf("unexpected error loading prompts: %v", err)
	}

	if len(items) != 3 {
		t.Fatalf("expected 3 parsed prompts, got %d", len(items))
	}

	if items[0].UserPrompt != "How do I implement merge sort in Go?" {
		t.Errorf("unexpected first prompt: %s", items[0].UserPrompt)
	}
}

func TestScanWorkspaceGenerators(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Empty workspace
	emptyItems := miner.ScanWorkspaceGenerators(tmpDir)
	if len(emptyItems) != 0 {
		t.Errorf("expected 0 items for empty directory, got %d", len(emptyItems))
	}

	// 2. Add go.mod
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module example.com/app\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatalf("failed to create go.mod: %v", err)
	}

	goItems := miner.ScanWorkspaceGenerators(tmpDir)
	if len(goItems) < 2 {
		t.Fatalf("expected at least 2 Go workspace prompts, got %d", len(goItems))
	}

	// 3. Add package.json
	if err := os.WriteFile(filepath.Join(tmpDir, "package.json"), []byte(`{"name":"app"}`), 0644); err != nil {
		t.Fatalf("failed to create package.json: %v", err)
	}

	bothItems := miner.ScanWorkspaceGenerators(tmpDir)
	if len(bothItems) < 3 {
		t.Fatalf("expected at least 3 prompts for Go + Node workspace, got %d", len(bothItems))
	}
}

func TestCorpusFileBudget(t *testing.T) {
	err := filepath.WalkDir("corpus", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Count(string(data), "\n") + 1
		if lines > 250 {
			t.Errorf("file %s exceeds 250 lines budget: %d lines", path, lines)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk corpus: %v", err)
	}
}
