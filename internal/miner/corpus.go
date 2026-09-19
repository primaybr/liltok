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

	if category == "php" {
		return getPHPPrompts()
	}

	if category == "" || category == "all" || category == "errors" {
		all = append(all, getErrorPrompts()...)
		all = append(all, getPHPPrompts()...)
	}
	if category == "" || category == "all" || category == "coding" {
		all = append(all, getCodingPrompts()...)
	}
	if category == "" || category == "all" || category == "security" {
		all = append(all, getSecurityPrompts()...)
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

// getPHPPrompts returns canonical PHP syntax, runtime, type, and database error diagnostics.
func getPHPPrompts() []PromptItem {
	return []PromptItem{
		{
			ID:           "err-php-syntax-unexpected-token",
			Category:     "errors",
			SystemPrompt: "You are an expert PHP systems engineer. Provide concise, direct diagnostic steps and code fixes for PHP syntax and parse errors.",
			UserPrompt:   "How do I fix 'Parse error: syntax error, unexpected token \":\", expecting \"{\"' or unexpected token errors in PHP?",
			Tags:         []string{"php", "syntax-error", "parse-error", "tokens"},
		},
		{
			ID:           "err-php-strict-types-bom",
			Category:     "errors",
			SystemPrompt: "You are an expert PHP and runtime engineer. Explain script bootstrapping, encoding, and strict_types declarations.",
			UserPrompt:   "What causes 'Fatal error: strict_types declaration must be the very first statement in the script' in PHP, and how does a UTF-8 BOM or leading whitespace trigger it?",
			Tags:         []string{"php", "strict-types", "bom", "encoding", "fatal-error"},
		},
		{
			ID:           "err-php-cannot-redeclare-method",
			Category:     "errors",
			SystemPrompt: "You are a senior PHP architect. Explain method resolution, inheritance, and naming rules in PHP.",
			UserPrompt:   "Explain 'Fatal error: Cannot redeclare ClassName::methodName()' in PHP, and how method name case-insensitivity (such as rollBack vs rollback) causes redeclaration collisions.",
			Tags:         []string{"php", "fatal-error", "oop", "case-sensitivity"},
		},
		{
			ID:           "err-php-typeerror-return-value",
			Category:     "errors",
			SystemPrompt: "You are a senior PHP engineer. Explain strict typing, nullability, and union types in PHP 8.",
			UserPrompt:   "How do I resolve 'Fatal error: Uncaught TypeError: Return value of ... must be of type X, null returned' in PHP 8 with nullable return types and early exits?",
			Tags:         []string{"php", "type-error", "strict-types", "nullability", "php8"},
		},
		{
			ID:           "err-php-array-to-string-conversion",
			Category:     "errors",
			SystemPrompt: "You are a PHP developer specializing in data processing and type casting.",
			UserPrompt:   "How do I fix 'Warning: Array to string conversion' in PHP when processing nested specification arrays or concatenating query values?",
			Tags:         []string{"php", "warning", "arrays", "data-structures"},
		},
		{
			ID:           "err-php-pdo-connection-closed",
			Category:     "errors",
			SystemPrompt: "You are a backend systems architect specializing in PHP database connection pooling and PostgreSQL resilience.",
			UserPrompt:   "How do I handle 'Fatal error: Uncaught PDOException: SQLSTATE[08006] [7] FATAL: terminating connection due to administrator command' or severed database connections in PHP with transparent auto-reconnect outside transactions?",
			Tags:         []string{"php", "pdo", "database", "postgresql", "connection-pooling"},
		},
		{
			ID:           "err-php-memory-limit-exhausted",
			Category:     "errors",
			SystemPrompt: "You are a PHP performance specialist. Explain memory profiling, generators (yield), and cursor iteration.",
			UserPrompt:   "How do I debug and resolve 'Fatal error: Allowed memory size of X bytes exhausted' in PHP when processing large batch files or stream pipelines?",
			Tags:         []string{"php", "memory-limit", "performance", "generators"},
		},
		{
			ID:           "err-php-max-execution-time",
			Category:     "errors",
			SystemPrompt: "You are a DevOps and backend architecture expert.",
			UserPrompt:   "How do I fix 'Fatal error: Maximum execution time of X seconds exceeded' in PHP, and how should long-running batch jobs be decoupled into detached CLI worker processes?",
			Tags:         []string{"php", "timeout", "cli", "background-worker"},
		},
		{
			ID:           "err-php-undefined-array-key",
			Category:     "errors",
			SystemPrompt: "You are a modern PHP engineer.",
			UserPrompt:   "What is the modern, idiomatic way in PHP 8 to resolve 'Warning: Undefined array key' and 'Warning: Undefined variable' using the null coalescing operator (??) and array_key_exists?",
			Tags:         []string{"php", "php8", "warning", "null-coalescing"},
		},
		{
			ID:           "err-php-class-not-found-psr4",
			Category:     "errors",
			SystemPrompt: "You are a PHP package maintainer and Composer expert.",
			UserPrompt:   "How do I diagnose and fix 'Fatal error: Uncaught Error: Class \"...\" not found' in PHP with Composer PSR-4 autoloading and namespace conventions?",
			Tags:         []string{"php", "composer", "autoload", "psr-4"},
		},
		{
			ID:           "err-php-unhandled-match-error",
			Category:     "errors",
			SystemPrompt: "You are a modern PHP engineer.",
			UserPrompt:   "How do I fix 'Fatal error: Uncaught UnhandledMatchError' in PHP 8 match expressions and handle default exhaustive branches?",
			Tags:         []string{"php", "php8", "match-expression", "unhandled-match-error"},
		},
		{
			ID:           "err-php-uninitialized-typed-property",
			Category:     "errors",
			SystemPrompt: "You are an expert in PHP 8 object-oriented design and constructor property promotion.",
			UserPrompt:   "How do I fix 'Fatal error: Uncaught Error: Typed property ClassName::$propertyName must not be accessed before initialization' in PHP 8 using constructor promotion or default values?",
			Tags:         []string{"php", "typed-properties", "oop", "php8"},
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

		// Database & SQL Performance
		{
			ID:           "code-sql-composite-index",
			Category:     "coding",
			SystemPrompt: "You are a database performance and SQL indexing specialist.",
			UserPrompt:   "Explain composite indexing in relational databases (PostgreSQL/MySQL), including the leftmost prefix rule and how column cardinality/selectivity dictates index column order.",
			Tags:         []string{"sql", "database", "indexing", "b-tree"},
		},
		{
			ID:           "code-sql-explain-analyze",
			Category:     "coding",
			SystemPrompt: "You are a database performance and SQL tuning specialist.",
			UserPrompt:   "How do I interpret PostgreSQL EXPLAIN ANALYZE output, including Seq Scan vs Index Scan vs Bitmap Heap Scan, actual time, loops, and cost estimations?",
			Tags:         []string{"sql", "postgres", "explain-analyze", "performance"},
		},
		{
			ID:           "code-sql-deadlock-prevention",
			Category:     "coding",
			SystemPrompt: "You are a database architect and transaction concurrency expert.",
			UserPrompt:   "What causes deadlocks in relational databases, and what are the best practices (ordered resource locking, transaction isolation levels) to prevent them?",
			Tags:         []string{"sql", "database", "deadlocks", "concurrency"},
		},
		{
			ID:           "code-sql-connection-reconnect",
			Category:     "coding",
			SystemPrompt: "You are a database connectivity and backend resilience expert.",
			UserPrompt:   "How do I handle severed database connections (such as PostgreSQL 08006, 57P01, or server termination) with transparent auto-reconnect and connection pool health checks?",
			Tags:         []string{"sql", "database", "connection-pool", "resilience"},
		},
		{
			ID:           "code-sql-two-stage-cte",
			Category:     "coding",
			SystemPrompt: "You are a database architect specializing in large-scale SQL query optimization.",
			UserPrompt:   "Explain the two-stage candidate CTE pattern for complex feeds and deep pagination to avoid expensive lateral joins and table scans across non-paginated rows.",
			Tags:         []string{"sql", "cte", "pagination", "performance"},
		},

		// Distributed Systems & API Resilience
		{
			ID:           "code-arch-circuit-breaker",
			Category:     "coding",
			SystemPrompt: "You are a distributed systems architect.",
			UserPrompt:   "Explain the Circuit Breaker pattern with its three states (Closed, Open, Half-Open), failure thresholds, and recovery timeouts.",
			Tags:         []string{"distributed-systems", "circuit-breaker", "resilience"},
		},
		{
			ID:           "code-arch-exponential-backoff",
			Category:     "coding",
			SystemPrompt: "You are a distributed systems and networking engineer.",
			UserPrompt:   "Explain exponential backoff with Full Jitter and show why jitter prevents the thundering herd problem in distributed API retries.",
			Tags:         []string{"distributed-systems", "retry", "jitter", "backoff"},
		},
		{
			ID:           "code-arch-idempotency-keys",
			Category:     "coding",
			SystemPrompt: "You are a backend architect specializing in payment and financial API reliability.",
			UserPrompt:   "How should an API implement Idempotency-Key header processing, request payload fingerprinting, and atomic lock/replay states with TTL?",
			Tags:         []string{"api", "idempotency", "architecture"},
		},
		{
			ID:           "code-arch-rate-limiter-comparison",
			Category:     "coding",
			SystemPrompt: "You are a high-concurrency systems architect.",
			UserPrompt:   "Compare Token Bucket, Leaky Bucket, and Sliding Window Counter rate limiting algorithms with their memory, precision, and burst handling trade-offs.",
			Tags:         []string{"rate-limiting", "architecture", "algorithms"},
		},

		// Modern Go Concurrency
		{
			ID:           "code-go-errgroup-context",
			Category:     "coding",
			SystemPrompt: "You are a Go concurrency and systems programming expert.",
			UserPrompt:   "How do I use golang.org/x/sync/errgroup with context cancellation to manage concurrent worker tasks and short-circuit on first error?",
			Tags:         []string{"go", "concurrency", "errgroup", "context"},
		},
		{
			ID:           "code-go-graceful-shutdown",
			Category:     "coding",
			SystemPrompt: "You are a Go systems engineer.",
			UserPrompt:   "How do I implement graceful HTTP server shutdown in Go using signal.Notify for SIGINT/SIGTERM and context with timeout?",
			Tags:         []string{"go", "http", "graceful-shutdown", "concurrency"},
		},
		{
			ID:           "code-go-strings-builder",
			Category:     "coding",
			SystemPrompt: "You are a Go performance optimization engineer.",
			UserPrompt:   "How do I use strings.Builder with Grow() for zero-allocation string concatenation in Go, and why is it faster than bytes.Buffer or string concatenation (+)?",
			Tags:         []string{"go", "performance", "memory-allocation"},
		},
		{
			ID:           "code-go-fan-out-fan-in",
			Category:     "coding",
			SystemPrompt: "You are a Go concurrency expert.",
			UserPrompt:   "Explain and implement the Fan-Out / Fan-In concurrency pattern in Go using channels, goroutines, and sync.WaitGroup.",
			Tags:         []string{"go", "concurrency", "fan-out-fan-in", "channels"},
		},

		// Modern Frontend & TypeScript
		{
			ID:           "code-ts-discriminated-unions",
			Category:     "coding",
			SystemPrompt: "You are a TypeScript architect.",
			UserPrompt:   "Explain TypeScript discriminated unions and how to implement exhaustive pattern matching using the 'never' type in switch statements.",
			Tags:         []string{"typescript", "types", "discriminated-unions"},
		},
		{
			ID:           "code-react-useeffect-cleanup",
			Category:     "coding",
			SystemPrompt: "You are a senior React and frontend performance expert.",
			UserPrompt:   "How do I properly cancel async operations and prevent race conditions or memory leaks in React useEffect using AbortController and cleanup functions?",
			Tags:         []string{"react", "hooks", "useeffect", "abortcontroller"},
		},
		{
			ID:           "code-nextjs-rsc-vs-client",
			Category:     "coding",
			SystemPrompt: "You are a Next.js and full-stack web architect.",
			UserPrompt:   "Explain the boundary and serialization rules between React Server Components (RSC) and Client Components ('use client') in Next.js App Router.",
			Tags:         []string{"react", "nextjs", "rsc", "server-components"},
		},
	}
}

// getSecurityPrompts returns high-value security, authentication, and cryptography engineering prompts.
func getSecurityPrompts() []PromptItem {
	return []PromptItem{
		{
			ID:           "code-sec-jwt-refresh-rotation",
			Category:     "security",
			SystemPrompt: "You are an application security and authentication architect.",
			UserPrompt:   "Explain JWT authentication architecture with short-lived access tokens, sliding refresh token rotation, and family-based revocation on reuse.",
			Tags:         []string{"security", "auth", "jwt", "refresh-tokens"},
		},
		{
			ID:           "code-sec-sql-injection",
			Category:     "security",
			SystemPrompt: "You are an application security specialist.",
			UserPrompt:   "How does SQL injection occur, why is input escaping insufficient compared to parameterized prepared statements, and how do ORMs mitigate it?",
			Tags:         []string{"security", "sql-injection", "owasp"},
		},
		{
			ID:           "code-sec-password-hashing",
			Category:     "security",
			SystemPrompt: "You are a cryptography and security engineer.",
			UserPrompt:   "What are the modern recommendations for secure password hashing (Argon2id vs bcrypt vs PBKDF2), and how should memory/time cost parameters be tuned?",
			Tags:         []string{"security", "cryptography", "passwords", "argon2id"},
		},
		{
			ID:           "code-sec-cors-csrf",
			Category:     "security",
			SystemPrompt: "You are a web application security engineer.",
			UserPrompt:   "What are the fundamental differences between CORS and CSRF vulnerabilities, and how do SameSite cookies and custom request headers mitigate CSRF?",
			Tags:         []string{"security", "csrf", "cors", "web-security"},
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
		{
			ID:           "devops-docker-scratch-go",
			Category:     "devops",
			SystemPrompt: "You are a DevOps and containerization specialist focusing on minimal image sizes and secure builds.",
			UserPrompt:   "Provide an optimized multi-stage Dockerfile compiling Go into a minimal scratch image with CA certificates and non-root user.",
			Tags:         []string{"docker", "go", "scratch", "security"},
		},
		{
			ID:           "devops-k8s-probes",
			Category:     "devops",
			SystemPrompt: "You are a Kubernetes and cloud infrastructure architect.",
			UserPrompt:   "Explain how to configure Kubernetes liveness, readiness, and startup probes with HTTP handlers, initialDelaySeconds, and periodSeconds.",
			Tags:         []string{"kubernetes", "probes", "devops", "cloud"},
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
		{
			ID:           "git-interactive-rebase-squash",
			Category:     "git",
			SystemPrompt: "You are a Git version control expert. Provide clean, safe terminal commands.",
			UserPrompt:   "Provide a complete interactive rebase workflow for squashing multiple feature commits into a single clean commit with git rebase -i.",
			Tags:         []string{"git", "rebase", "squash", "workflow"},
		},
		{
			ID:           "git-resolve-merge-conflict",
			Category:     "git",
			SystemPrompt: "You are a Git version control expert. Provide clean, safe terminal commands.",
			UserPrompt:   "Provide a step-by-step guide to identifying and resolving Git merge conflicts during a merge or rebase.",
			Tags:         []string{"git", "merge", "conflicts", "diff"},
		},
		{
			ID:           "git-stash-workflow",
			Category:     "git",
			SystemPrompt: "You are a Git version control expert. Provide clean, safe terminal commands.",
			UserPrompt:   "Explain advanced Git stash workflows: saving with messages, stashing untracked files (-u), listing, inspecting, popping, and stashing to a new branch.",
			Tags:         []string{"git", "stash", "workflow"},
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
