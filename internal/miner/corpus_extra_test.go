package miner_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/primaybr/liltok/internal/miner"
)

func TestLoadCorpusFromFS(t *testing.T) {
	fsys := fstest.MapFS{
		"root/go/a.json": {Data: []byte(`[
			{"id":"dup","category":"go","user_prompt":"first"},
			{"id":"","user_prompt":"missing id"},
			{"id":"no-prompt","user_prompt":""},
			{"id":"keep","category":"go","user_prompt":"kept"}
		]`)},
		"root/go/b.JSON":    {Data: []byte(`[{"id":"dup","user_prompt":"second copy"}]`)},
		"root/notes.txt":    {Data: []byte("not a corpus file")},
		"other/x.json":      {Data: []byte(`[{"id":"outside","user_prompt":"x"}]`)},
		"broken/bad.json":   {Data: []byte(`{"id":"not a list"}`)},
		"root/empty/.keep":  {Data: nil},
		"root/php/c.json":   {Data: []byte(`[]`)},
		"root/php/d.json":   {Data: []byte(`[{"id":"php-1","user_prompt":"php"}]`)},
		"root/php/e.yaml":   {Data: []byte("id: ignored")},
		"root/php/sub/.dir": {Data: nil},
	}

	items, err := miner.LoadCorpusFromFS(fsys, "root")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]string{}
	for _, it := range items {
		got[it.ID] = it.UserPrompt
	}
	if len(items) != 3 || got["dup"] != "first" || got["keep"] != "kept" || got["php-1"] != "php" {
		t.Errorf("unexpected items: %+v", items)
	}

	if _, err := miner.LoadCorpusFromFS(fsys, "broken"); err == nil || !strings.Contains(err.Error(), "broken/bad.json") {
		t.Errorf("expected parse error naming the file, got %v", err)
	}

	if _, err := miner.LoadCorpusFromFS(fsys, "absent"); err == nil {
		t.Error("expected error for missing root")
	}

	all, err := miner.LoadCorpusFromFS(fstest.MapFS{
		"a.json": {Data: []byte(`[{"id":"top","user_prompt":"t"}]`)},
	}, "")
	if err != nil || len(all) != 1 || all[0].ID != "top" {
		t.Errorf("empty root should walk from '.', got %+v err=%v", all, err)
	}
}

func TestLoadPromptsFromFile_MissingFile(t *testing.T) {
	_, err := miner.LoadPromptsFromFile(filepath.Join(t.TempDir(), "absent.txt"))
	if err == nil || !strings.Contains(err.Error(), "failed to open prompts file") {
		t.Errorf("expected open error, got %v", err)
	}
}

func TestLoadPromptsFromFile_AssignsSequentialIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.txt")
	if err := os.WriteFile(path, []byte("  one  \n# skip\n\ntwo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	items, err := miner.LoadPromptsFromFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].ID != "file-prompt-1" || items[1].ID != "file-prompt-2" || items[0].UserPrompt != "one" {
		t.Errorf("unexpected items: %+v", items)
	}
	if items[0].Category != "custom" || items[0].SystemPrompt == "" {
		t.Errorf("expected custom category with default system prompt: %+v", items[0])
	}
}

func TestScanWorkspaceGenerators_DetectsStacks(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		wantIDs []string
	}{
		{"flutter", []string{"pubspec.yaml"}, []string{"ws-flutter-riverpod"}},
		{"python requirements", []string{"requirements.txt"}, []string{"ws-py-type-hints"}},
		{"python pyproject", []string{"pyproject.toml"}, []string{"ws-py-type-hints"}},
		{"python both files yields one prompt", []string{"requirements.txt", "pyproject.toml"}, []string{"ws-py-type-hints"}},
		{"all stacks", []string{"go.mod", "package.json", "pubspec.yaml", "requirements.txt"},
			[]string{"ws-go-testing", "ws-go-graceful-shutdown", "ws-ts-async-await", "ws-flutter-riverpod", "ws-py-type-hints"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			items := miner.ScanWorkspaceGenerators(dir)
			if len(items) != len(tt.wantIDs) {
				t.Fatalf("got %d items, want %v", len(items), tt.wantIDs)
			}
			for i, it := range items {
				if it.ID != tt.wantIDs[i] || it.Category != "workspace" {
					t.Errorf("item %d = %s/%s, want %s/workspace", i, it.ID, it.Category, tt.wantIDs[i])
				}
			}
		})
	}
}

func TestScanWorkspaceGenerators_IgnoresDirectoriesNamedLikeManifests(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "go.mod"), 0755); err != nil {
		t.Fatal(err)
	}
	if items := miner.ScanWorkspaceGenerators(dir); len(items) != 0 {
		t.Errorf("directory named go.mod should not count as a manifest, got %d items", len(items))
	}
}

func TestScanWorkspaceGenerators_EmptyDirUsesCwd(t *testing.T) {
	// The package directory has no manifests of its own, so scanning "." finds nothing.
	for _, f := range []string{"go.mod", "package.json", "pubspec.yaml", "requirements.txt", "pyproject.toml"} {
		if _, err := os.Stat(f); err == nil {
			t.Skipf("package directory unexpectedly contains %s", f)
		}
	}
	if items := miner.ScanWorkspaceGenerators(""); len(items) != 0 {
		t.Errorf("expected no prompts for the package directory, got %d", len(items))
	}
}

func TestGetCuratedPromptsByLanguage_AllAndUnknown(t *testing.T) {
	all := miner.GetCuratedPrompts("all")
	for _, lang := range []string{"", "  ALL "} {
		if got := miner.GetCuratedPromptsByLanguage(lang); len(got) != len(all) {
			t.Errorf("language %q returned %d prompts, want all %d", lang, len(got), len(all))
		}
	}
	if got := miner.GetCuratedPromptsByLanguage("no-such-language-zzz"); len(got) != 0 {
		t.Errorf("unknown language returned %d prompts", len(got))
	}

	// GetCuratedPrompts must hand out copies so callers cannot corrupt the cache.
	if len(all) > 0 {
		orig := all[0].ID
		all[0].ID = "mutated"
		if miner.GetCuratedPrompts("")[0].ID != orig {
			t.Error("GetCuratedPrompts returned a shared slice")
		}
	}
}
