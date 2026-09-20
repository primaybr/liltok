package miner

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed corpus
var embeddedCorpusFS embed.FS

var (
	curatedCacheMu sync.RWMutex
	curatedCache   []PromptItem
	curatedOnce    sync.Once
)

// PromptItem represents a single prompt to be mined and cached.
type PromptItem struct {
	ID           string   `json:"id"`
	Category     string   `json:"category"`
	SystemPrompt string   `json:"system_prompt,omitempty"`
	UserPrompt   string   `json:"user_prompt"`
	Tags         []string `json:"tags,omitempty"`
}

// GetCuratedPrompts returns the built-in curated prompt corpus across multiple categories.
// Prompts are loaded from embedded JSON files in internal/miner/corpus/ and cached in memory.
func GetCuratedPrompts(category string) []PromptItem {
	curatedOnce.Do(func() {
		curatedCache = loadAllEmbeddedPrompts()
	})

	category = strings.ToLower(strings.TrimSpace(category))
	if category == "" || category == "all" {
		out := make([]PromptItem, len(curatedCache))
		copy(out, curatedCache)
		return out
	}

	var filtered []PromptItem
	for _, it := range curatedCache {
		if strings.EqualFold(it.Category, category) || hasTag(it.Tags, category) {
			filtered = append(filtered, it)
		}
	}
	return filtered
}

// GetCuratedPromptsByLanguage filters the curated prompt corpus by language or technology tag.
func GetCuratedPromptsByLanguage(lang string) []PromptItem {
	all := GetCuratedPrompts("all")
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" || lang == "all" {
		return all
	}

	var filtered []PromptItem
	for _, it := range all {
		if hasTag(it.Tags, lang) || strings.Contains(strings.ToLower(it.ID), lang) {
			filtered = append(filtered, it)
		}
	}
	return filtered
}

func hasTag(tags []string, target string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, target) {
			return true
		}
	}
	return false
}

// loadAllEmbeddedPrompts walks the embedded corpus filesystem and parses all .json files.
func loadAllEmbeddedPrompts() []PromptItem {
	items, err := LoadCorpusFromFS(embeddedCorpusFS, "corpus")
	if err != nil {
		return nil
	}
	return items
}

// LoadCorpusFromFS recursively traverses an fs.FS filesystem root, parsing all JSON prompt files and deduplicating by ID.
func LoadCorpusFromFS(sys fs.FS, rootDir string) ([]PromptItem, error) {
	var items []PromptItem
	seenIDs := make(map[string]bool)

	if rootDir == "" {
		rootDir = "."
	}

	err := fs.WalkDir(sys, rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".json") {
			return nil
		}

		data, err := fs.ReadFile(sys, path)
		if err != nil {
			return err
		}

		var fileItems []PromptItem
		if err := json.Unmarshal(data, &fileItems); err != nil {
			return fmt.Errorf("failed to parse prompt corpus at %s: %w", path, err)
		}

		for _, it := range fileItems {
			if it.ID == "" || it.UserPrompt == "" {
				continue
			}
			if !seenIDs[it.ID] {
				seenIDs[it.ID] = true
				items = append(items, it)
			}
		}
		return nil
	})

	return items, err
}

// ResetCuratedCache resets the in-memory cache, primarily used in testing.
func ResetCuratedCache() {
	curatedOnce = sync.Once{}
	curatedCacheMu.Lock()
	curatedCache = nil
	curatedCacheMu.Unlock()
}

// LoadPromptsFromFile parses prompts from a text file, where each non-empty line is treated as a prompt.
func LoadPromptsFromFile(filePath string) ([]PromptItem, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open prompts file %s: %w", filePath, err)
	}
	defer file.Close()

	var items []PromptItem
	scanner := bufio.NewScanner(file)
	idx := 1
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		items = append(items, PromptItem{
			ID:           fmt.Sprintf("file-prompt-%d", idx),
			Category:     "custom",
			SystemPrompt: "You are an expert coding assistant. Provide clean, accurate, and concise answers.",
			UserPrompt:   line,
		})
		idx++
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading prompts file: %w", err)
	}

	return items, nil
}

// ScanWorkspaceGenerators scans a project workspace directory, detects tech stacks (Go, Node, Python, Flutter),
// and synthesizes context-aware prompts for immediate pre-warming.
func ScanWorkspaceGenerators(workspaceDir string) []PromptItem {
	var items []PromptItem

	if workspaceDir == "" {
		workspaceDir = "."
	}

	// 1. Check Go
	if fileExists(filepath.Join(workspaceDir, "go.mod")) {
		items = append(items, PromptItem{
			ID:           "ws-go-testing",
			Category:     "workspace",
			SystemPrompt: "You are an expert Go engineer.",
			UserPrompt:   "What are the best practices for writing table-driven unit tests with mocks and subtests in Go?",
			Tags:         []string{"go", "testing", "table-driven"},
		}, PromptItem{
			ID:           "ws-go-graceful-shutdown",
			Category:     "workspace",
			SystemPrompt: "You are an expert Go engineer.",
			UserPrompt:   "How do I implement graceful HTTP server shutdown in Go using context and signal.Notify?",
			Tags:         []string{"go", "http", "graceful-shutdown"},
		})
	}

	// 2. Check Node / TypeScript
	if fileExists(filepath.Join(workspaceDir, "package.json")) {
		items = append(items, PromptItem{
			ID:           "ws-ts-async-await",
			Category:     "workspace",
			SystemPrompt: "You are a senior TypeScript architect.",
			UserPrompt:   "What are best practices for error handling with async/await and Promise.allSettled in TypeScript?",
			Tags:         []string{"typescript", "async", "promises"},
		})
	}

	// 3. Check Flutter / Dart
	if fileExists(filepath.Join(workspaceDir, "pubspec.yaml")) {
		items = append(items, PromptItem{
			ID:           "ws-flutter-riverpod",
			Category:     "workspace",
			SystemPrompt: "You are a senior Flutter engineer.",
			UserPrompt:   "Explain the core Riverpod provider types (FutureProvider, NotifierProvider) and how to write tests with overrideWith.",
			Tags:         []string{"flutter", "dart", "riverpod", "testing"},
		})
	}

	// 4. Check Python
	if fileExists(filepath.Join(workspaceDir, "requirements.txt")) || fileExists(filepath.Join(workspaceDir, "pyproject.toml")) {
		items = append(items, PromptItem{
			ID:           "ws-py-type-hints",
			Category:     "workspace",
			SystemPrompt: "You are a senior Python engineer.",
			UserPrompt:   "How do I use Python typing with Generics, Protocol, and Pydantic for strict schema validation?",
			Tags:         []string{"python", "typing", "pydantic"},
		})
	}

	return items
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
