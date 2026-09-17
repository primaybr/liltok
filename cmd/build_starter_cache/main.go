package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liltok/liltok/internal/cache"
	"github.com/liltok/liltok/internal/db"
	"github.com/liltok/liltok/internal/miner"
)

type StarterItem struct {
	Hash             string `json:"hash"`
	Model            string `json:"model"`
	NormalizedPrompt string `json:"normalized_prompt"`
	ResponsePayload  string `json:"response_payload"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TTLSeconds       int    `json:"ttl_seconds"`
	IsSemantic       bool   `json:"is_semantic"`
}

func main() {
	fromDB := flag.String("from-db", "", "Path to source SQLite database to merge (optional, e.g. ~/.liltok/liltok.db)")
	outPath := flag.String("out", filepath.Join("internal", "db", "starter_cache.json.gz"), "Target starter cache archive path")
	sanitize := flag.Bool("sanitize", true, "Automatically scrub personal home paths, API keys, and private IPs")
	minHits := flag.Int("min-hits", 0, "Minimum hits required for imported database entries")
	maxPromptBytes := flag.Int("max-prompt-bytes", 65536, "Maximum prompt byte length to include (default 65536, 0 = unlimited)")
	flag.Parse()

	prompts := miner.GetCuratedPrompts("all")
	models := []string{"claude-3-5-sonnet-20241022", "claude-opus-5", "gpt-4o"}

	var starterItems []StarterItem
	normOpts := cache.NormalizationOptions{CacheNonzeroTemperature: true}

	for _, p := range prompts {
		answer := generateCanonicalAnswer(p)

		for _, model := range models {
			// 1. Anthropic schema
			anthPayload := map[string]interface{}{
				"model": model,
				"messages": []map[string]string{
					{"role": "user", "content": p.UserPrompt},
				},
				"max_tokens": 4096,
			}
			if p.SystemPrompt != "" {
				anthPayload["system"] = p.SystemPrompt
			}
			anthBytes, _ := json.Marshal(anthPayload)
			normAnth, err := cache.NormalizePayload(anthBytes, normOpts)
			if err == nil {
				anthResp, _ := json.Marshal(map[string]interface{}{
					"id":          fmt.Sprintf("msg-seed-%s", p.ID),
					"type":        "message",
					"role":        "assistant",
					"model":       model,
					"content":     []map[string]string{{"type": "text", "text": answer}},
					"stop_reason": "end_turn",
					"usage": map[string]int{
						"input_tokens":  len(p.UserPrompt) / 4,
						"output_tokens": len(answer) / 4,
					},
				})
				starterItems = append(starterItems, StarterItem{
					Hash:             normAnth.Hash,
					Model:            model,
					NormalizedPrompt: normAnth.CanonicalJSON,
					ResponsePayload:  string(anthResp),
					PromptTokens:     len(p.UserPrompt) / 4,
					CompletionTokens: len(answer) / 4,
					TTLSeconds:       2592000,
					IsSemantic:       false,
				})
			}

			// 2. OpenAI schema
			openAIPayload := map[string]interface{}{
				"model": model,
				"messages": []map[string]string{
					{"role": "user", "content": p.UserPrompt},
				},
				"temperature": 0.0,
			}
			if p.SystemPrompt != "" {
				openAIPayload["messages"] = []map[string]string{
					{"role": "system", "content": p.SystemPrompt},
					{"role": "user", "content": p.UserPrompt},
				}
			}
			openAIBytes, _ := json.Marshal(openAIPayload)
			normOpenAI, err := cache.NormalizePayload(openAIBytes, normOpts)
			if err == nil {
				openAIResp, _ := json.Marshal(map[string]interface{}{
					"id":      fmt.Sprintf("chatcmpl-seed-%s", p.ID),
					"object":  "chat.completion",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{
						{
							"index": 0,
							"message": map[string]string{
								"role":    "assistant",
								"content": answer,
							},
							"finish_reason": "stop",
						},
					},
					"usage": map[string]int{
						"prompt_tokens":     len(p.UserPrompt) / 4,
						"completion_tokens": len(answer) / 4,
						"total_tokens":      (len(p.UserPrompt) + len(answer)) / 4,
					},
				})
				starterItems = append(starterItems, StarterItem{
					Hash:             normOpenAI.Hash,
					Model:            model,
					NormalizedPrompt: normOpenAI.CanonicalJSON,
					ResponsePayload:  string(openAIResp),
					PromptTokens:     len(p.UserPrompt) / 4,
					CompletionTokens: len(answer) / 4,
					TTLSeconds:       2592000,
					IsSemantic:       false,
				})
			}
		}
	}

	itemsMap := make(map[string]StarterItem)
	for _, item := range starterItems {
		itemsMap[item.Hash] = item
	}

	if *fromDB != "" {
		dbPath := *fromDB
		if strings.HasPrefix(dbPath, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				dbPath = filepath.Join(home, dbPath[1:])
			}
		}

		database, err := db.Open(dbPath)
		if err != nil {
			fmt.Printf("Warning: failed to open source database %s: %v\n", dbPath, err)
		} else {
			defer database.Close()

			rows, err := database.Query(`
				SELECT hash, model, normalized_prompt, response_payload, prompt_tokens, completion_tokens, ttl_seconds, is_semantic
				FROM cache_entries
				WHERE hit_count >= ?
			`, *minHits)
			if err != nil {
				fmt.Printf("Warning: failed to query cache entries: %v\n", err)
			} else {
				defer rows.Close()
				dbMerged := 0
				for rows.Next() {
					var item StarterItem
					var rawPayload []byte
					var isSem int
					if err := rows.Scan(&item.Hash, &item.Model, &item.NormalizedPrompt, &rawPayload, &item.PromptTokens, &item.CompletionTokens, &item.TTLSeconds, &isSem); err == nil {
						if *maxPromptBytes > 0 && len(item.NormalizedPrompt) > *maxPromptBytes {
							continue
						}

						item.ResponsePayload = string(rawPayload)
						item.IsSemantic = (isSem == 1)

						if *sanitize {
							item.NormalizedPrompt = miner.SanitizeContent(item.NormalizedPrompt)
							item.ResponsePayload = miner.SanitizeContent(item.ResponsePayload)
						}

						if _, exists := itemsMap[item.Hash]; !exists {
							dbMerged++
						}
						itemsMap[item.Hash] = item
					}
				}
				fmt.Printf("Merged %d entries from database %s (Total unique: %d)\n", dbMerged, dbPath, len(itemsMap))
			}
		}
	}

	var finalItems []StarterItem
	for _, item := range itemsMap {
		finalItems = append(finalItems, item)
	}

	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gzWriter).Encode(finalItems); err != nil {
		fmt.Printf("Failed to encode: %v\n", err)
		os.Exit(1)
	}
	if err := gzWriter.Close(); err != nil {
		fmt.Printf("Failed to close gzWriter: %v\n", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(*outPath), 0755); err != nil {
		fmt.Printf("Failed to create parent directory for %s: %v\n", *outPath, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*outPath, buf.Bytes(), 0644); err != nil {
		fmt.Printf("Failed to write %s: %v\n", *outPath, err)
		os.Exit(1)
	}

	fmt.Printf("Successfully generated %d starter cache entries into %s (%d bytes gz)\n",
		len(finalItems), *outPath, buf.Len())
}

func generateCanonicalAnswer(p miner.PromptItem) string {
	switch p.ID {
	case "err-go-nil-pointer":
		return `### Fixing 'panic: runtime error: invalid memory address or nil pointer dereference' in Go

This runtime panic occurs when dereferencing a nil pointer, invoking a method on a nil struct, or accessing an uninitialized interface.

#### Common Causes & Fixes:
1. **Uninitialized Pointer Access**:
   ` + "```go" + `
   // Bad: p is nil
   var p *Person
   fmt.Println(p.Name) // PANIC!

   // Fix: Initialize before dereference
   p = &Person{Name: "Alice"}
   // Or check defensively:
   if p != nil {
       fmt.Println(p.Name)
   }
   ` + "```" + `

2. **Uninitialized Map within a Struct**:
   ` + "```go" + `
   type Config struct {
       Settings map[string]string
   }
   // Fix: Always use make() before writing to a map
   cfg := Config{Settings: make(map[string]string)}
   cfg.Settings["theme"] = "dark"
   ` + "```" + `

3. **Nil Interface with Typed Pointer**:
   ` + "```go" + `
   var err *MyCustomError = nil
   var iErr error = err
   // iErr != nil because it has a concrete type (*MyCustomError)!
   // Fix: return bare nil instead of typed nil pointers.
   ` + "```" + `
`
	case "err-go-concurrent-map-writes":
		return `### Fixing 'fatal error: concurrent map writes' in Go

Go's built-in ` + "`map`" + ` is NOT thread-safe. Concurrent reads and writes result in an unrecoverable fatal crash.

#### Solution 1: ` + "`sync.RWMutex`" + ` (Recommended for structured types)
` + "```go" + `
type SafeStore struct {
    mu   sync.RWMutex
    data map[string]string
}

func (s *SafeStore) Set(key, value string) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.data[key] = value
}

func (s *SafeStore) Get(key string) (string, bool) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    val, ok := s.data[key]
    return val, ok
}
` + "```" + `

#### Solution 2: ` + "`sync.Map`" + ` (For high-concurrency disjoint keys)
` + "```go" + `
var m sync.Map
m.Store("user_123", "active")
val, ok := m.Load("user_123")
` + "```" + `
`
	case "err-js-cannot-read-properties-of-undefined":
		return `### Fixing 'TypeError: Cannot read properties of undefined (reading 'map')'

This error occurs when attempting to call array methods like ` + "`.map()`" + `, ` + "`.filter()`" + `, or access properties on an asynchronous state or prop that has not yet resolved.

#### 1. Optional Chaining & Default Empty Array (Modern Best Practice)
` + "```tsx" + `
// Optional chaining + fallback:
const list = data?.items?.map((item) => (
  <Card key={item.id} title={item.title} />
)) ?? null;
` + "```" + `

#### 2. Guarding State in React Components
` + "```tsx" + `
const [users, setUsers] = useState<User[]>([]); // Initialize with [] rather than undefined

if (!users || users.length === 0) {
  return <SkeletonLoader />;
}

return (
  <ul>
    {users.map(u => <li key={u.id}>{u.name}</li>)}
  </ul>
);
` + "```" + `
`
	case "err-js-cors-missing-allow-origin":
		return `### Resolving CORS: 'No Access-Control-Allow-Origin header is present'

Cross-Origin Resource Sharing (CORS) is a browser-enforced security mechanism. The backend server must explicitly declare which origins are permitted.

#### 1. Backend Server Fix (Express.js / Node.js)
` + "```javascript" + `
import cors from 'cors';
app.use(cors({
  origin: ['http://localhost:3000', 'https://myapp.com'],
  methods: ['GET', 'POST', 'PUT', 'DELETE', 'OPTIONS'],
  allowedHeaders: ['Content-Type', 'Authorization'],
  credentials: true
}));
` + "```" + `

#### 2. Backend Server Fix (Go / Chi / Gin)
` + "```go" + `
w.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
if r.Method == "OPTIONS" {
    w.WriteHeader(http.StatusOK)
    return
}
` + "```" + `

#### 3. Frontend Dev Proxy (Vite / Next.js)
` + "```javascript" + `
// vite.config.ts
export default defineConfig({
  server: {
    proxy: {
      '/api': 'http://localhost:8080'
    }
  }
});
` + "```" + `
`
	case "code-binary-search":
		return `### Idiomatic Binary Search Implementation

#### Go Implementation
` + "```go" + `
// BinarySearch returns the index of target in sorted slice nums, or -1 if not found.
// Time Complexity: O(log n)
// Space Complexity: O(1)
func BinarySearch(nums []int, target int) int {
    left, right := 0, len(nums)-1

    for left <= right {
        // Avoid integer overflow with left + (right-left)/2
        mid := left + (right-left)/2
        if nums[mid] == target {
            return mid
        } else if nums[mid] < target {
            left = mid + 1
        } else {
            right = mid - 1
        }
    }
    return -1
}
` + "```" + `
`
	case "git-undo-last-commit":
		return `### Undoing the Most Recent Git Commit

#### Option 1: Keep local changes in working directory (Soft Reset - Recommended)
` + "```bash" + `
# Undo commit, keeps your files modified and staged
git reset --soft HEAD~1

# Undo commit, keeps files modified but un-staged
git reset HEAD~1
` + "```" + `

#### Option 2: Completely discard the commit and all changes (Hard Reset - Destructive!)
` + "```bash" + `
git reset --hard HEAD~1
` + "```" + `

#### Option 3: If commit was already pushed to remote
` + "```bash" + `
# Reverts changes cleanly with a new inverse commit
git revert HEAD
git push origin main
` + "```" + `
`
	// Database & SQL Performance
	case "code-sql-composite-index":
		return `### Composite Indexing in Relational Databases (Leftmost Prefix Rule & Selectivity)

A composite (multi-column) B-tree index orders rows sequentially by column: first by column 1, then by column 2 within identical values of column 1, and so forth.

#### 1. Leftmost Prefix Rule
An index on (A, B, C) can be used by the query planner for queries filtering on:
- (A)
- (A, B)
- (A, B, C)

It CANNOT be used to jump directly to rows filtering only on (B), (C), or (B, C), because the B-tree is sorted primarily by column A.

#### 2. Column Ordering: Selectivity & Equality vs Range
- Equality columns first: Put columns evaluated with equality (=) before columns evaluated with ranges (<, >, BETWEEN, LIKE 'prefix%').
- Selectivity rule: Among equality columns, place the column with highest selectivity (most distinct values / lowest matching percentage) first to eliminate the maximum candidate rows early.
- Range cut-off: Once a range condition is evaluated on column B, subsequent column C cannot be used for B-tree index seeks, only as an in-memory index filter.

#### 3. Example Index & Query
` + "```sql" + `
-- Optimal index for: WHERE tenant_id = 42 AND status = 'active' AND created_at >= '2026-01-01'
CREATE INDEX idx_orders_tenant_status_created 
ON orders (tenant_id, status, created_at);

SELECT id, total_amount 
FROM orders 
WHERE tenant_id = 42 AND status = 'active' AND created_at >= '2026-01-01';
` + "```" + `

#### Warning on ORDER BY:
An index on (A, B) provides zero-cost sorting for "ORDER BY A, B" or "ORDER BY A DESC, B DESC", but triggers a filesort for "ORDER BY A ASC, B DESC" unless defined with explicit direction: CREATE INDEX ... (A ASC, B DESC).
`
	case "code-sql-explain-analyze":
		return `### Interpreting PostgreSQL EXPLAIN ANALYZE Output

EXPLAIN (ANALYZE, BUFFERS) runs the query and displays actual execution statistics and disk buffer usage alongside optimizer cost estimates.

#### 1. Scan Types
- Sequential Scan (Seq Scan): Reads every page in the table. Optimal for small tables or queries matching >15-20% of rows.
- Index Scan: Traverses B-tree to find matching tuple IDs (TIDs), then performs random reads on heap pages.
- Index Only Scan: Retrieves data directly from the index without accessing heap pages. Requires all projected columns in index and clean Visibility Map.
- Bitmap Index Scan + Bitmap Heap Scan:
  - Bitmap Index Scan builds an in-memory bitmap of matching physical disk pages.
  - Bitmap Heap Scan visits pages in physical disk order, converting random I/O into sequential I/O.

#### 2. Reading Execution Metrics
` + "```text" + `
Bitmap Heap Scan on orders (cost=12.45..450.20 rows=520 width=64) (actual time=0.120..1.450 rows=480 loops=1)
  Buffers: shared hit=32 read=8
` + "```" + `
- cost=12.45..450.20: 12.45 is startup cost (time to first row); 450.20 is total cost estimate in arbitrary planner units.
- actual time=0.120..1.450: Execution time in milliseconds (start..finish).
- loops=N: Multiply actual time and rows by N when loops > 1 (e.g., nested loops).
- Buffers: shared hit=32 read=8: hit = served from RAM cache; read = read from physical disk.
`
	case "code-sql-deadlock-prevention":
		return `### Preventing Deadlocks in Relational Databases

A deadlock occurs when two transactions hold locks each other needs, forming a circular wait (Tx 1 locks Row A and waits for Row B; Tx 2 locks Row B and waits for Row A).

#### 1. Ordered Resource Locking (Primary Defense)
Always acquire locks on shared rows in the exact same deterministic order across the application:
` + "```sql" + `
-- Sort IDs in ascending order before locking:
SELECT id, balance 
FROM accounts 
WHERE id IN (10, 20) 
ORDER BY id 
FOR UPDATE;
` + "```" + `

#### 2. Keep Transactions Minimal
- Never perform external HTTP requests, file I/O, or CPU-heavy serialization inside database transactions.
- Acquire locks at the latest possible time before commit.

#### 3. Optimistic Concurrency Control
For high-traffic records, avoid row locks and use version checks:
` + "```sql" + `
UPDATE products 
SET stock = stock - 1, version = version + 1 
WHERE id = :id AND version = :current_version;
` + "```" + `

#### 4. Defensive Timeouts
Configure lock timeouts so deadlocked transactions fail fast:
` + "```sql" + `
SET lock_timeout = '2s';
SET statement_timeout = '5s';
` + "```" + `
`
	case "code-sql-connection-reconnect":
		return `### Handling Severed Database Connections (PostgreSQL 08006, 57P01)

Connection drops occur due to admin termination (57P01), network timeouts (08006), broken TCP sockets, or proxy pool failover.

#### 1. Connection Pool Liveness Configuration
Configure pool parameters to recycle idle connections before firewalls drop TCP state:
` + "```go" + `
db.SetConnMaxLifetime(10 * time.Minute)
db.SetConnMaxIdleTime(2 * time.Minute)
db.SetMaxOpenConns(25)
db.SetMaxIdleConns(25)
` + "```" + `

#### 2. Transparent Reconnect & Retry Pattern
Retry read-only and idempotent queries on connection errors, but never auto-reconnect mid-transaction:
` + "```go" + `
func QueryWithRetry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
    var zero T
    for attempt := 0; attempt < 3; attempt++ {
        val, err := fn()
        if err == nil {
            return val, nil
        }
        if !isConnectionSevered(err) {
            return zero, err
        }
        time.Sleep(time.Duration(attempt*50) * time.Millisecond)
    }
    return zero, errors.New("exhausted reconnect retries")
}

func isConnectionSevered(err error) bool {
    msg := err.Error()
    return strings.Contains(msg, "08006") ||
           strings.Contains(msg, "57P01") ||
           strings.Contains(msg, "connection refused") ||
           strings.Contains(msg, "broken pipe")
}
` + "```" + `
`
	case "code-sql-two-stage-cte":
		return `### Two-Stage Candidate CTEs for Fast Deep Pagination

Single-stage deep pagination (OFFSET 50000 LIMIT 20) forces the database to evaluate joins, lateral queries, and aggregations across 50,020 rows before discarding 50,000.

#### The Pattern
- Stage 1 (Candidate CTE): Extract and paginate candidate IDs using a narrow composite index.
- Stage 2 (Outer Projection): Join candidate IDs back to core tables and execute heavy lateral joins or aggregations only for the paginated rows.

` + "```sql" + `
WITH candidates AS (
    -- Stage 1: Candidate ID pagination (fast index seek)
    SELECT id
    FROM posts
    WHERE status = 'published'
    ORDER BY published_at DESC
    LIMIT 20 OFFSET 50000
)
-- Stage 2: Heavy joins for the 20 paginated rows only
SELECT p.id, p.title, u.username, json_agg(c.body) AS comments
FROM candidates c_ids
JOIN posts p ON p.id = c_ids.id
JOIN users u ON u.id = p.author_id
LEFT JOIN comments c ON c.post_id = p.id
GROUP BY p.id, p.title, u.username;
` + "```" + `
This pattern eliminates N+1 query overhead and reduces execution time from seconds to milliseconds.
`

	// Distributed Systems & API Resilience
	case "code-arch-circuit-breaker":
		return `### Circuit Breaker Pattern in Distributed Systems

The Circuit Breaker pattern prevents an application from repeatedly calling a degraded service, preventing cascading failures.

#### Circuit Breaker States:
- Closed: Normal operation. Requests flow through. Failures are counted. If failure rate exceeds threshold (e.g. 50%), state transitions to Open.
- Open: Requests fail fast immediately with fallback response or HTTP 503. A cooldown reset timer (e.g. 30s) starts.
- Half-Open: When timer expires, a trial probe of test requests is permitted:
  - If all succeed, state resets to Closed.
  - If any probe fails, state trips back to Open with reset cooldown.

#### Go Implementation:
` + "```go" + `
type CircuitBreaker struct {
    mu          sync.Mutex
    state       string // "CLOSED", "OPEN", "HALF-OPEN"
    failures    int
    threshold   int
    cooldown    time.Duration
    lastTripped time.Time
}

func (cb *CircuitBreaker) Execute(fn func() error) error {
    cb.mu.Lock()
    now := time.Now()
    if cb.state == "OPEN" {
        if now.Sub(cb.lastTripped) > cb.cooldown {
            cb.state = "HALF-OPEN"
        } else {
            cb.mu.Unlock()
            return errors.New("circuit breaker is OPEN")
        }
    }
    cb.mu.Unlock()

    err := fn()

    cb.mu.Lock()
    defer cb.mu.Unlock()
    if err != nil {
        cb.failures++
        if cb.failures >= cb.threshold || cb.state == "HALF-OPEN" {
            cb.state = "OPEN"
            cb.lastTripped = now
        }
        return err
    }

    if cb.state == "HALF-OPEN" {
        cb.state = "CLOSED"
        cb.failures = 0
    }
    return nil
}
` + "```" + `
`
	case "code-arch-exponential-backoff":
		return `### Exponential Backoff with Full Jitter

Naive exponential backoff causes clients to retry simultaneously at synchronized intervals (1s, 2s, 4s, 8s), generating a Thundering Herd spike on recovering servers.

#### Full Jitter Formula:
` + "```text" + `
sleep = random(0, min(max_backoff, base * 2^attempt))
` + "```" + `

#### Go Implementation:
` + "```go" + `
func RetryWithFullJitter(ctx context.Context, maxRetries int, base, maxBackoff time.Duration, fn func() error) error {
    for attempt := 0; attempt < maxRetries; attempt++ {
        err := fn()
        if err == nil {
            return nil
        }

        multiplier := math.Pow(2, float64(attempt))
        ceiling := math.Min(float64(maxBackoff), float64(base)*multiplier)
        sleepDuration := time.Duration(rand.Float64() * ceiling)

        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(sleepDuration):
        }
    }
    return errors.New("retries exhausted")
}
` + "```" + `
Full Jitter uniformly distributes retry traffic across the time window, maximizing system stability.
`
	case "code-arch-idempotency-keys":
		return `### API Idempotency-Key Header Handling

Idempotency guarantees that submitting duplicate requests produces the exact same outcome without duplicate side effects (essential for payments and orders).

#### Protocol Workflow:
1. Client generates UUID: Idempotency-Key: <uuid>.
2. Server computes payload fingerprint: SHA-256(Method + Path + Body + UserID).
3. Server executes atomic check in Redis/DB:
   - In-flight: If key exists with status "PROCESSING", return HTTP 409 Conflict with Retry-After: 2.
   - Mismatch: If key exists but fingerprint differs, reject with HTTP 422 Unprocessable Entity.
   - Completed: Return cached HTTP status code and response body immediately.
4. On first execution: Mark status as "PROCESSING", run business logic, record response, and set status to "COMPLETED" with TTL (e.g. 24-72 hours).
`
	case "code-arch-rate-limiter-comparison":
		return `### Rate Limiting Comparison: Token Bucket vs Leaky Bucket vs Sliding Window

| Algorithm | Burst Handling | Memory | Accuracy | Best For |
|---|---|---|---|---|
| Token Bucket | Excellent (up to burst capacity) | O(1) per user | High | General REST APIs (Stripe, AWS) |
| Leaky Bucket | Smooths bursts (fixed outflow rate) | O(1) per user | High | Ingestion queues, traffic shaping |
| Sliding Window Counter | Good (weighted boundary approximation) | O(1) per user | Very High | High-volume edge gateways (Cloudflare) |
| Sliding Window Log | Perfect | O(N) entries | 100% | Low-volume, high-value financial calls |

- Token Bucket: Ideal for APIs where clients have bursty usage patterns.
- Leaky Bucket: Ideal where downstream services require a strictly constant throughput.
- Sliding Window Counter: Best balance of memory efficiency and boundary smoothing.
`

	// Modern Go Concurrency
	case "code-go-errgroup-context":
		return `### Go Concurrency with errgroup.WithContext

golang.org/x/sync/errgroup coordinates concurrent goroutines, cancels a shared context on the first error, and waits for all tasks to complete.

` + "```go" + `
package main

import (
    "context"
    "fmt"
    "golang.org/x/sync/errgroup"
)

func ProcessBatch(ctx context.Context, items []string) error {
    g, gCtx := errgroup.WithContext(ctx)
    g.SetLimit(10) // Bounded concurrency worker pool

    for _, item := range items {
        val := item
        g.Go(func() error {
            if err := gCtx.Err(); err != nil {
                return err // Short-circuit if another worker failed
            }
            return processItem(gCtx, val)
        })
    }

    return g.Wait() // Returns first non-nil error
}
` + "```" + `
`
	case "code-go-graceful-shutdown":
		return `### Graceful HTTP Server Shutdown in Go

` + "```go" + `
package main

import (
    "context"
    "errors"
    "log"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"
)

func main() {
    srv := &http.Server{
        Addr:         ":8080",
        Handler:      http.DefaultServeMux,
        ReadTimeout:  10 * time.Second,
        WriteTimeout: 10 * time.Second,
    }

    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

    go func() {
        if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            log.Fatalf("Server listen error: %v", err)
        }
    }()

    <-sigChan
    log.Println("Shutting down gracefully...")

    ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()

    if err := srv.Shutdown(ctx); err != nil {
        log.Fatalf("Server forced shutdown: %v", err)
    }
    log.Println("Server stopped cleanly.")
}
` + "```" + `
`
	case "code-go-strings-builder":
		return `### Zero-Allocation String Concatenation with strings.Builder

Using string concatenation (+) in loops creates new string allocations on each step (O(N^2) allocations). strings.Builder pre-allocates memory and returns a string via zero-copy conversion.

` + "```go" + `
package main

import "strings"

func JoinStrings(items []string, sep string) string {
    if len(items) == 0 {
        return ""
    }

    totalLen := 0
    for _, s := range items {
        totalLen += len(s)
    }
    totalLen += len(sep) * (len(items) - 1)

    var b strings.Builder
    b.Grow(totalLen) // Pre-allocate backing buffer in one allocation

    for i, s := range items {
        if i > 0 {
            b.WriteString(sep)
        }
        b.WriteString(s)
    }
    return b.String()
}
` + "```" + `
`
	case "code-go-fan-out-fan-in":
		return `### Fan-Out / Fan-In Pattern in Go

- Fan-Out: Multiple worker goroutines read from one input channel to parallelize CPU or I/O work.
- Fan-In: Consolidates outputs from multiple worker channels into a single output channel.

` + "```go" + `
func worker(in <-chan int) <-chan int {
    out := make(chan int)
    go func() {
        defer close(out)
        for n := range in {
            out <- n * 2
        }
    }()
    return out
}

func merge(cs ...<-chan int) <-chan int {
    var wg sync.WaitGroup
    out := make(chan int)

    output := func(c <-chan int) {
        defer wg.Done()
        for n := range c {
            out <- n
        }
    }

    wg.Add(len(cs))
    for _, c := range cs {
        go output(c)
    }

    go func() {
        wg.Wait()
        close(out)
    }()

    return out
}
` + "```" + `
`

	// Modern Frontend & TypeScript
	case "code-ts-discriminated-unions":
		return `### TypeScript Discriminated Unions & Exhaustive Checks

Discriminated unions use a common literal tag to distinguish types. The 'never' type guarantees exhaustive handling in switch statements.

` + "```typescript" + `
interface LoadingState {
  status: 'loading';
}

interface SuccessState {
  status: 'success';
  data: string[];
}

interface ErrorState {
  status: 'error';
  error: Error;
}

type QueryState = LoadingState | SuccessState | ErrorState;

function handleState(state: QueryState): string {
  switch (state.status) {
    case 'loading':
      return 'Loading data...';
    case 'success':
      return 'Loaded ' + state.data.length + ' records';
    case 'error':
      return 'Error: ' + state.error.message;
    default: {
      // Compile-time check: if a union case is unhandled, TypeScript errors here
      const _exhaustive: never = state;
      throw new Error('Unhandled state: ' + _exhaustive);
    }
  }
}
` + "```" + `
`
	case "code-react-useeffect-cleanup":
		return `### Preventing Race Conditions in React useEffect with AbortController

` + "```tsx" + `
import React, { useEffect, useState } from 'react';

export function UserDetail({ userId }: { userId: string }) {
  const [user, setUser] = useState<any>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);

    async function loadUser() {
      try {
        const res = await fetch('/api/users/' + userId, {
          signal: controller.signal,
        });
        if (!res.ok) throw new Error('Failed to load user');
        const data = await res.json();
        setUser(data);
      } catch (err: any) {
        if (err.name === 'AbortError') return; // Cancelled cleanly
        console.error(err);
      } finally {
        if (!controller.signal.aborted) setLoading(false);
      }
    }

    loadUser();

    return () => {
      controller.abort(); // Cancel in-flight request when userId changes or unmounts
    };
  }, [userId]);

  if (loading) return <div>Loading...</div>;
  return <div>{user?.name}</div>;
}
` + "```" + `
`
	case "code-nextjs-rsc-vs-client":
		return `### Next.js App Router: Server Components vs Client Components

- Server Components (Default):
  - Execute on the server only.
  - Direct access to database, filesystem, and server secrets.
  - Zero bundle size overhead (no client JavaScript shipped).
  - Cannot use useState, useEffect, or browser event listeners.

- Client Components ('use client'):
  - Hydrated and interactive in the browser.
  - Support useState, useEffect, and DOM event listeners (onClick).

#### Serialization Boundary:
Props passed from Server Components to Client Components must be JSON-serializable (no functions, class instances, or database connections). Pass Server Components as children to Client Components to avoid turning the entire tree into client components.
`

	// Security
	case "code-sec-jwt-refresh-rotation":
		return `### JWT Authentication with Sliding Refresh Token Rotation

1. Short-Lived Access Token: 5-15 minute expiry. Kept in memory.
2. Sliding Refresh Token: Stored in an HttpOnly, Secure, SameSite=Strict cookie.
3. Rotation on Refresh: Each refresh request invalidates the current refresh token and issues a new refresh token.
4. Family Revocation: Each token belongs to a FamilyID. If an already-invalidated refresh token is submitted (indicating token replay/theft), all active tokens in that family are immediately revoked.
`
	case "code-sec-sql-injection":
		return `### SQL Injection Prevention via Parameterized Queries

SQL injection happens when untrusted user input is concatenated directly into SQL text.

#### Why Parameterized Queries Work:
Parameterized queries send SQL structure and data separately. The database compiles the query syntax first; parameters are bound as raw literal values that cannot alter the SQL AST.

` + "```go" + `
// VULNERABLE:
// query := fmt.Sprintf("SELECT * FROM users WHERE email = '%s'", email)

// SECURE (PostgreSQL parameterized query):
query := "SELECT id, email FROM users WHERE email = $1"
rows, err := db.QueryContext(ctx, query, email)
` + "```" + `
`
	case "code-sec-password-hashing":
		return `### Secure Password Hashing: Argon2id vs bcrypt

Never use fast cryptographic hashes (SHA-256, MD5) for password verification.

- Argon2id: Memory-hard and CPU-hard. Provides maximum resistance against GPU/ASIC attacks.
  - Recommended parameters: Memory = 64MB, Iterations = 3, Parallelism = 2.
- bcrypt: CPU-bound, battle-tested standard. Cost factor >= 12.
- Constant-time verification: Always use subtle.ConstantTimeCompare to avoid side-channel timing attacks.
`
	case "code-sec-cors-csrf":
		return `### CORS vs CSRF Differences and Defenses

- CORS: Browser security mechanism restricting JavaScript from reading cross-origin responses unless authorized via Access-Control-Allow-Origin. CORS does not prevent cross-site requests from reaching the server!
- CSRF: Exploits automatic browser credential transmission (cookies). An external site tricks a user's browser into executing state-changing requests.

#### Defenses:
1. SameSite Cookies: Set SameSite=Lax or SameSite=Strict on session cookies.
2. Custom Request Headers: Require custom headers (e.g. Content-Type: application/json or X-Requested-With) to trigger CORS preflight.
3. CSRF Tokens: Synchronizer token pattern for state-changing forms.
`

	// DevOps & Infrastructure
	case "devops-docker-scratch-go":
		return `### Minimal Multi-Stage Dockerfile for Go using Scratch Base Image

` + "```dockerfile" + `
# Stage 1: Build static binary
FROM golang:1.23-alpine AS builder

WORKDIR /src
RUN apk add --no-cache ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /app/server ./cmd/server

RUN echo "nonroot:x:10001:10001:NonRoot:/:/sbin/nologin" > /etc/passwd_nonroot

# Stage 2: Scratch runtime (minimal attack surface)
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/passwd_nonroot /etc/passwd
COPY --from=builder /app/server /server

USER 10001:10001
EXPOSE 8080

ENTRYPOINT ["/server"]
` + "```" + `
`
	case "devops-nginx-reverse-proxy":
		return `### Production Nginx Reverse Proxy with SSL, WebSockets, & Gzip

` + "```nginx" + `
events {
    worker_connections 1024;
}

http {
    include mime.types;
    gzip on;
    gzip_types text/plain text/css application/json application/javascript;

    map $http_upgrade $connection_upgrade {
        default upgrade;
        '' close;
    }

    server {
        listen 80;
        server_name api.example.com;
        return 301 https://$host$request_uri;
    }

    server {
        listen 443 ssl http2;
        server_name api.example.com;

        ssl_certificate /etc/letsencrypt/live/api.example.com/fullchain.pem;
        ssl_certificate_key /etc/letsencrypt/live/api.example.com/privkey.pem;
        ssl_protocols TLSv1.2 TLSv1.3;

        location / {
            proxy_pass http://127.0.0.1:8080;
            proxy_http_version 1.1;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection $connection_upgrade;
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
        }
    }
}
` + "```" + `
`
	case "devops-k8s-probes":
		return `### Kubernetes Probes: Startup, Liveness, and Readiness

` + "```yaml" + `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api-service
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: api
        image: api-service:v1.0.0
        ports:
        - containerPort: 8080
        
        # 1. Startup Probe: 30s allowance for slow cold starts
        startupProbe:
          httpGet:
            path: /healthz/startup
            port: 8080
          failureThreshold: 30
          periodSeconds: 2

        # 2. Readiness Probe: Checks DB pool health before routing traffic
        readinessProbe:
          httpGet:
            path: /healthz/ready
            port: 8080
          initialDelaySeconds: 2
          periodSeconds: 5

        # 3. Liveness Probe: Detects process deadlocks
        livenessProbe:
          httpGet:
            path: /healthz/live
            port: 8080
          periodSeconds: 10
` + "```" + `
`

	// Git Workflows
	case "git-interactive-rebase-squash":
		return `### Git Interactive Rebase: Squashing Commits

` + "```bash" + `
# 1. Start interactive rebase for the last 4 commits
git rebase -i HEAD~4

# In editor, keep first commit as 'pick', change subsequent commits to 'squash' (or 's'):
# pick e1a2b3c Add user authentication model
# squash f4d5e6f Fix typo in auth validator
# squash 7890abc Add unit tests for auth

# 2. Save and exit editor, then edit the combined commit message.
# 3. Safely push the consolidated commit to remote:
git push --force-with-lease origin feature/auth
` + "```" + `
`
	case "git-resolve-merge-conflict":
		return `### Resolving Git Merge Conflicts Step-by-Step

` + "```bash" + `
# 1. Inspect conflicting files:
git status

# 2. Open file and inspect conflict markers:
# <<<<<<< HEAD (current branch)
# =======
# >>>>>>> feature-branch (incoming branch)

# 3. Edit file to resolve code, delete conflict markers, and stage:
git add path/to/resolved-file.ts

# 4. Continue rebase or merge:
git rebase --continue
# or: git merge --continue

# To abort and return to pre-merge state:
git rebase --abort
` + "```" + `
`
	case "git-stash-workflow":
		return `### Advanced Git Stash Workflow

` + "```bash" + `
# 1. Stash changes with a descriptive label including untracked files (-u)
git stash push -m "WIP: auth modal" -u

# 2. List all stashes
git stash list

# 3. Inspect diff of a stash
git stash show -p stash@{0}

# 4. Pop (apply and delete from stash list)
git stash pop stash@{0}

# 5. Create a new branch directly from stash
git stash branch feature/auth-modal stash@{0}
` + "```" + `
`
	default:
		return fmt.Sprintf("### %s\n\nTo solve this task efficiently, follow standard industry best practices:\n1. Verify input boundaries and type constraints.\n2. Ensure thread-safety and proper error handling.\n3. Add unit tests covering edge cases.", p.UserPrompt)
	}
}
