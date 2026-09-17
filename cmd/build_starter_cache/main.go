package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/liltok/liltok/internal/cache"
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

	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gzWriter).Encode(starterItems); err != nil {
		fmt.Printf("Failed to encode: %v\n", err)
		os.Exit(1)
	}
	if err := gzWriter.Close(); err != nil {
		fmt.Printf("Failed to close gzWriter: %v\n", err)
		os.Exit(1)
	}

	targetPath := filepath.Join("internal", "db", "starter_cache.json.gz")
	if err := os.WriteFile(targetPath, buf.Bytes(), 0644); err != nil {
		fmt.Printf("Failed to write %s: %v\n", targetPath, err)
		os.Exit(1)
	}

	fmt.Printf("Successfully generated %d starter cache entries into %s (%d bytes gz)\n",
		len(starterItems), targetPath, buf.Len())
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
	default:
		return fmt.Sprintf("### %s\n\nTo solve this task efficiently, follow standard industry best practices:\n1. Verify input boundaries and type constraints.\n2. Ensure thread-safety and proper error handling.\n3. Add unit tests covering edge cases.", p.UserPrompt)
	}
}
