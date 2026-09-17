package miner_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/primaybr/liltok/internal/miner"
)

func TestGetCuratedPrompts(t *testing.T) {
	all := miner.GetCuratedPrompts("all")
	if len(all) < 15 {
		t.Fatalf("expected at least 15 curated prompts, got %d", len(all))
	}

	errorsOnly := miner.GetCuratedPrompts("errors")
	if len(errorsOnly) == 0 {
		t.Errorf("expected non-empty errors category")
	}
	for _, p := range errorsOnly {
		if p.Category != "errors" {
			t.Errorf("expected category errors, got %s", p.Category)
		}
	}

	codingOnly := miner.GetCuratedPrompts("coding")
	if len(codingOnly) == 0 {
		t.Errorf("expected non-empty coding category")
	}

	devopsOnly := miner.GetCuratedPrompts("devops")
	if len(devopsOnly) == 0 {
		t.Errorf("expected non-empty devops category")
	}

	gitOnly := miner.GetCuratedPrompts("git")
	if len(gitOnly) == 0 {
		t.Errorf("expected non-empty git category")
	}

	securityOnly := miner.GetCuratedPrompts("security")
	if len(securityOnly) == 0 {
		t.Errorf("expected non-empty security category")
	}
	for _, p := range securityOnly {
		if p.Category != "security" {
			t.Errorf("expected category security, got %s", p.Category)
		}
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
