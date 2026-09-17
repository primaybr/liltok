package miner

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
func GetCuratedPrompts(category string) []PromptItem {
	var all []PromptItem

	category = strings.ToLower(strings.TrimSpace(category))

	if category == "" || category == "all" || category == "errors" {
		all = append(all, getErrorPrompts()...)
	}
	if category == "" || category == "all" || category == "coding" {
		all = append(all, getCodingPrompts()...)
	}
	if category == "" || category == "all" || category == "devops" {
		all = append(all, getDevOpsPrompts()...)
	}
	if category == "" || category == "all" || category == "git" {
		all = append(all, getGitPrompts()...)
	}

	return all
}

// getErrorPrompts returns top universal compiler, runtime, and framework error messages with diagnostic prompts.
func getErrorPrompts() []PromptItem {
	return []PromptItem{
		// Go
		{
			ID:           "err-go-nil-pointer",
			Category:     "errors",
			SystemPrompt: "You are an expert systems programmer. Provide concise, direct diagnostic steps and code fixes for the given error.",
			UserPrompt:   "How do I debug and fix 'panic: runtime error: invalid memory address or nil pointer dereference' in Go?",
			Tags:         []string{"go", "runtime-error", "nil-pointer"},
		},
		{
			ID:           "err-go-concurrent-map-writes",
			Category:     "errors",
			SystemPrompt: "You are an expert systems programmer. Provide concise, direct diagnostic steps and code fixes for the given error.",
			UserPrompt:   "Explain 'fatal error: concurrent map writes' in Go and show how to fix it with sync.RWMutex or sync.Map.",
			Tags:         []string{"go", "concurrency", "mutex"},
		},
		{
			ID:           "err-go-slice-bounds-out-of-range",
			Category:     "errors",
			SystemPrompt: "You are an expert systems programmer. Provide concise, direct diagnostic steps and code fixes for the given error.",
			UserPrompt:   "What causes 'panic: runtime error: index out of range' or 'slice bounds out of range' in Go, and how can it be prevented defensively?",
			Tags:         []string{"go", "slices", "bounds-check"},
		},

		// JavaScript / TypeScript
		{
			ID:           "err-js-cannot-read-properties-of-undefined",
			Category:     "errors",
			SystemPrompt: "You are a senior full-stack JavaScript/TypeScript engineer. Give clear explanations and modern code snippets.",
			UserPrompt:   "How do I fix 'TypeError: Cannot read properties of undefined (reading 'map')' in JavaScript / React?",
			Tags:         []string{"javascript", "react", "type-error"},
		},
		{
			ID:           "err-js-cors-missing-allow-origin",
			Category:     "errors",
			SystemPrompt: "You are a web security and API expert. Explain HTTP headers clearly.",
			UserPrompt:   "Explain the CORS error: 'Access to fetch at from origin has been blocked by CORS policy: No Access-Control-Allow-Origin header is present on the requested resource.' How do I solve it?",
			Tags:         []string{"web", "http", "cors", "security"},
		},
		{
			ID:           "err-ts-type-not-assignable",
			Category:     "errors",
			SystemPrompt: "You are a TypeScript expert. Explain type narrowing and interfaces.",
			UserPrompt:   "How do I resolve TypeScript error 'Type X is not assignable to type Y' when handling API responses?",
			Tags:         []string{"typescript", "typing", "generics"},
		},

		// Python
		{
			ID:           "err-py-modulenotfound",
			Category:     "errors",
			SystemPrompt: "You are a senior Python engineer. Explain packaging, venv, and import resolution.",
			UserPrompt:   "How do I troubleshoot 'ModuleNotFoundError: No module named' in Python with virtual environments and PYTHONPATH?",
			Tags:         []string{"python", "virtualenv", "packaging"},
		},
		{
			ID:           "err-py-keyerror",
			Category:     "errors",
			SystemPrompt: "You are a senior Python engineer. Give clean idioms.",
			UserPrompt:   "What causes KeyError in Python dictionaries and what are the best practices (dict.get, defaultdict) to avoid it?",
			Tags:         []string{"python", "dictionary", "keyerror"},
		},

		// Docker & Containers
		{
			ID:           "err-docker-oom-137",
			Category:     "errors",
			SystemPrompt: "You are a DevOps and container specialist.",
			UserPrompt:   "Why does my Docker container exit with code 137 (OOMKilled) and how do I inspect and increase memory limits?",
			Tags:         []string{"docker", "containers", "oom", "linux"},
		},
		{
			ID:           "err-docker-port-already-allocated",
			Category:     "errors",
			SystemPrompt: "You are a DevOps and container specialist.",
			UserPrompt:   "How do I fix 'Bind for 0.0.0.0:8080 failed: port is already allocated' in Docker?",
			Tags:         []string{"docker", "networking", "ports"},
		},

		// Flutter / Dart
		{
			ID:           "err-flutter-renderflex-overflowed",
			Category:     "errors",
			SystemPrompt: "You are a Flutter and mobile architecture specialist.",
			UserPrompt:   "How do I fix 'A RenderFlex overflowed by X pixels on the bottom/right' in Flutter?",
			Tags:         []string{"flutter", "dart", "layout", "renderflex"},
		},
		{
			ID:           "err-flutter-setstate-during-build",
			Category:     "errors",
			SystemPrompt: "You are a Flutter and mobile architecture specialist.",
			UserPrompt:   "How do I fix 'setState() or markNeedsBuild() called during build' in Flutter?",
			Tags:         []string{"flutter", "dart", "state-management"},
		},
	}
}

// getCodingPrompts returns common algorithmic patterns, syntax references, and idiomatic idioms.
func getCodingPrompts() []PromptItem {
	return []PromptItem{
		{
			ID:           "code-binary-search",
			Category:     "coding",
			SystemPrompt: "You are a software engineer specializing in clean, robust algorithms with time and space complexity analysis.",
			UserPrompt:   "Write an idiomatic binary search implementation with boundary checks and discuss time/space complexity.",
			Tags:         []string{"algorithms", "binary-search"},
		},
		{
			ID:           "code-lru-cache",
			Category:     "coding",
			SystemPrompt: "You are a software engineer specializing in clean, robust algorithms with time and space complexity analysis.",
			UserPrompt:   "Design and implement an in-memory LRU (Least Recently Used) cache with O(1) Get and Put operations using a doubly linked list and hash map.",
			Tags:         []string{"data-structures", "lru", "cache"},
		},
		{
			ID:           "code-rate-limiter-token-bucket",
			Category:     "coding",
			SystemPrompt: "You are a distributed systems architect.",
			UserPrompt:   "Explain the Token Bucket rate limiting algorithm and provide an idiomatic thread-safe implementation.",
			Tags:         []string{"algorithms", "rate-limiting", "token-bucket"},
		},
		{
			ID:           "code-debounce-throttle",
			Category:     "coding",
			SystemPrompt: "You are a senior frontend engineer.",
			UserPrompt:   "What is the difference between debounce and throttle in JavaScript/TypeScript? Provide clean implementations of both.",
			Tags:         []string{"javascript", "typescript", "debounce", "throttle"},
		},
		{
			ID:           "code-regex-email-uuid",
			Category:     "coding",
			SystemPrompt: "You are a senior software engineer.",
			UserPrompt:   "Provide production-ready regular expressions for validating standard email addresses and RFC 4122 UUID v4 strings.",
			Tags:         []string{"regex", "validation"},
		},
		{
			ID:           "code-sql-window-functions",
			Category:     "coding",
			SystemPrompt: "You are a database architect and SQL tuning specialist.",
			UserPrompt:   "Explain SQL window functions (ROW_NUMBER, RANK, DENSE_RANK, LEAD, LAG) with concrete examples.",
			Tags:         []string{"sql", "database", "window-functions"},
		},
		{
			ID:           "code-go-worker-pool",
			Category:     "coding",
			SystemPrompt: "You are a Go concurrency expert.",
			UserPrompt:   "How do I write an idiomatic Go worker pool pattern with channels, sync.WaitGroup, and context cancellation?",
			Tags:         []string{"go", "concurrency", "worker-pool"},
		},
	}
}

// getDevOpsPrompts returns standard container, deployment, and infrastructure configurations.
func getDevOpsPrompts() []PromptItem {
	return []PromptItem{
		{
			ID:           "devops-docker-multistage-go",
			Category:     "devops",
			SystemPrompt: "You are a DevOps and containerization specialist focusing on minimal image sizes and secure builds.",
			UserPrompt:   "Provide an optimized multi-stage Dockerfile for a Go application using Alpine or Scratch with non-root user.",
			Tags:         []string{"docker", "go", "multistage"},
		},
		{
			ID:           "devops-docker-multistage-node",
			Category:     "devops",
			SystemPrompt: "You are a DevOps and containerization specialist focusing on minimal image sizes and secure builds.",
			UserPrompt:   "Provide a secure multi-stage Dockerfile for a Node.js / Next.js production build with standalone output.",
			Tags:         []string{"docker", "nodejs", "nextjs"},
		},
		{
			ID:           "devops-nginx-reverse-proxy",
			Category:     "devops",
			SystemPrompt: "You are a network systems and web infrastructure engineer.",
			UserPrompt:   "Provide a production-ready Nginx reverse proxy configuration with WebSocket support, gzip compression, and SSL termination.",
			Tags:         []string{"nginx", "reverse-proxy", "ssl"},
		},
		{
			ID:           "devops-gh-actions-ci",
			Category:     "devops",
			SystemPrompt: "You are a CI/CD specialist.",
			UserPrompt:   "Write a complete GitHub Actions workflow for linting, testing with coverage, and compiling cross-platform binaries.",
			Tags:         []string{"github-actions", "cicd"},
		},
	}
}

// getGitPrompts returns standard Git troubleshooting and operational workflows.
func getGitPrompts() []PromptItem {
	return []PromptItem{
		{
			ID:           "git-undo-last-commit",
			Category:     "git",
			SystemPrompt: "You are a Git version control expert. Provide clean, safe terminal commands.",
			UserPrompt:   "How do I undo the most recent Git commit, both keeping and discarding local changes (soft vs hard reset)?",
			Tags:         []string{"git", "reset", "commit"},
		},
		{
			ID:           "git-resolve-merge-conflicts",
			Category:     "git",
			SystemPrompt: "You are a Git version control expert. Provide clean, safe terminal commands.",
			UserPrompt:   "What is the step-by-step workflow for resolving Git merge conflicts during a rebase?",
			Tags:         []string{"git", "rebase", "conflicts"},
		},
		{
			ID:           "git-discard-untracked-changes",
			Category:     "git",
			SystemPrompt: "You are a Git version control expert. Provide clean, safe terminal commands.",
			UserPrompt:   "How do I discard all untracked files and unstaged changes in Git cleanly (git clean, git restore)?",
			Tags:         []string{"git", "clean", "restore"},
		},
		{
			ID:           "git-squash-commits",
			Category:     "git",
			SystemPrompt: "You are a Git version control expert. Provide clean, safe terminal commands.",
			UserPrompt:   "How do I squash multiple commits into a single commit using interactive rebase before creating a PR?",
			Tags:         []string{"git", "rebase", "squash"},
		},
	}
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
