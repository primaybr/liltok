package miner

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/primaybr/liltok/internal/cache"
	"github.com/primaybr/liltok/internal/cache/semantic"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/telemetry"
)

// MinerConfig contains execution parameters for the free-tier cache miner.
type MinerConfig struct {
	Provider     string   // "groq", "nvidianim", "ollama"
	APIKey       string   // Free tier API key
	BaseURL      string   // Optional override URL
	Model        string   // Upstream generator model
	Workers      int      // Concurrency (default 2)
	RateLimitRPM int      // Throttle (default 30)
	TargetModels []string // Target models to synthesize cache for (e.g. "claude-3-5-sonnet-20241022", "gpt-4o")
}

// MiningStats tracks progress of a mining session.
type MiningStats struct {
	TotalPrompts    int           `json:"total_prompts"`
	Completed       int64         `json:"completed"`
	CacheEntries    int64         `json:"cache_entries"`
	TokensGenerated int64         `json:"tokens_generated"`
	Errors          int64         `json:"errors"`
	Duration        time.Duration `json:"duration"`
}

// CacheExportItem represents an exportable/importable cache record.
type CacheExportItem struct {
	Hash             string `json:"hash"`
	Model            string `json:"model"`
	NormalizedPrompt string `json:"normalized_prompt"`
	ResponsePayload  string `json:"response_payload"` // Base64 or raw string
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TTLSeconds       int    `json:"ttl_seconds"`
	IsSemantic       bool   `json:"is_semantic"`
}

// CacheMiner coordinates parallel prompt generation, response synthesis, and cache seeding.
type CacheMiner struct {
	cfg        MinerConfig
	database   *db.DB
	semCache   *semantic.SemanticCache
	httpClient *http.Client
}

// NewCacheMiner instantiates a cache miner.
func NewCacheMiner(cfg MinerConfig, database *db.DB, semCache *semantic.SemanticCache) *CacheMiner {
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.RateLimitRPM <= 0 {
		cfg.RateLimitRPM = 30
	}
	if len(cfg.TargetModels) == 0 {
		cfg.TargetModels = []string{"claude-opus-5", "claude-sonnet-5", "gpt-4o", "claude-3-5-sonnet-20241022"}
	}

	// Resolve provider defaults
	switch strings.ToLower(cfg.Provider) {
	case "nvidianim":
		if cfg.BaseURL == "" {
			cfg.BaseURL = "https://integrate.api.nvidia.com/v1"
		}
		if cfg.Model == "" {
			cfg.Model = "meta/llama-3.3-70b-instruct"
		}
	case "ollama":
		if cfg.BaseURL == "" {
			cfg.BaseURL = "http://localhost:11434/v1"
		}
		if cfg.Model == "" {
			cfg.Model = "qwen2.5-coder:7b"
		}
	default: // "groq"
		cfg.Provider = "groq"
		if cfg.BaseURL == "" {
			cfg.BaseURL = "https://api.groq.com/openai/v1"
		}
		if cfg.Model == "" {
			cfg.Model = "llama-3.3-70b-versatile"
		}
	}

	return &CacheMiner{
		cfg:      cfg,
		database: database,
		semCache: semCache,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// MinePrompts executes mining across the given slice of prompt items with throttling and concurrent workers.
func (m *CacheMiner) MinePrompts(ctx context.Context, prompts []PromptItem) (MiningStats, error) {
	start := time.Now()
	stats := MiningStats{
		TotalPrompts: len(prompts),
	}

	if len(prompts) == 0 {
		return stats, nil
	}

	// Rate limiter: 60s / RateLimitRPM per request token
	delayBetweenRequests := time.Duration(float64(time.Minute) / float64(m.cfg.RateLimitRPM))
	throttleTicker := time.NewTicker(delayBetweenRequests)
	defer throttleTicker.Stop()

	jobs := make(chan PromptItem, len(prompts))
	for _, p := range prompts {
		jobs <- p
	}
	close(jobs)

	var wg sync.WaitGroup
	var mu sync.Mutex

	for w := 0; w < m.cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case item, ok := <-jobs:
					if !ok {
						return
					}

					// Wait for rate limiter ticket
					mu.Lock()
					<-throttleTicker.C
					mu.Unlock()

					err := m.mineSinglePrompt(ctx, item, &stats)
					if err != nil {
						atomic.AddInt64(&stats.Errors, 1)
						telemetry.Log.Warn().
							Str("prompt_id", item.ID).
							Err(err).
							Msg("Cache miner prompt failed")
					} else {
						atomic.AddInt64(&stats.Completed, 1)
					}
				}
			}
		}()
	}

	wg.Wait()
	stats.Duration = time.Since(start)
	return stats, nil
}

func (m *CacheMiner) mineSinglePrompt(ctx context.Context, item PromptItem, stats *MiningStats) error {
	// 1. Generate canonical completion using free provider
	reqPayload := map[string]interface{}{
		"model": m.cfg.Model,
		"messages": []map[string]string{
			{"role": "user", "content": item.UserPrompt},
		},
		"temperature": 0.0,
		"max_tokens":  2048,
	}
	if item.SystemPrompt != "" {
		reqPayload["messages"] = []map[string]string{
			{"role": "system", "content": item.SystemPrompt},
			{"role": "user", "content": item.UserPrompt},
		}
	}

	jsonBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal generator payload: %w", err)
	}

	targetURL := strings.TrimRight(m.cfg.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return fmt.Errorf("failed to create http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if m.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	}

	resp, err := m.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http call to %s failed: %w", m.cfg.Provider, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("provider %s returned status %d: %s", m.cfg.Provider, resp.StatusCode, string(respBytes))
	}

	var parsedResp struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(respBytes, &parsedResp); err != nil || len(parsedResp.Choices) == 0 {
		return fmt.Errorf("failed to parse valid response: %w", err)
	}

	generatedText := parsedResp.Choices[0].Message.Content
	atomic.AddInt64(&stats.TokensGenerated, int64(parsedResp.Usage.TotalTokens))

	// 2. Synthesize entries for each target model
	for _, targetModel := range m.cfg.TargetModels {
		if err := m.storeSynthesizedEntry(ctx, targetModel, item, generatedText, parsedResp.Usage.PromptTokens, parsedResp.Usage.CompletionTokens); err == nil {
			atomic.AddInt64(&stats.CacheEntries, 2)
		}
	}

	return nil
}

func (m *CacheMiner) storeSynthesizedEntry(ctx context.Context, targetModel string, item PromptItem, answer string, pTokens, cTokens int) error {
	normOpts := cache.NormalizationOptions{
		CacheNonzeroTemperature: true,
	}

	// 1. OpenAI Schema Synthesis
	openAIPayload := map[string]interface{}{
		"model": targetModel,
		"messages": []map[string]string{
			{"role": "user", "content": item.UserPrompt},
		},
		"temperature": 0.0,
	}
	if item.SystemPrompt != "" {
		openAIPayload["messages"] = []map[string]string{
			{"role": "system", "content": item.SystemPrompt},
			{"role": "user", "content": item.UserPrompt},
		}
	}
	openAIBytes, _ := json.Marshal(openAIPayload)
	normOpenAI, err := cache.NormalizePayload(openAIBytes, normOpts)
	if err == nil {
		openAIResp, _ := json.Marshal(map[string]interface{}{
			"id":      fmt.Sprintf("chatcmpl-mined-%s", item.ID),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   targetModel,
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
				"prompt_tokens":     pTokens,
				"completion_tokens": cTokens,
				"total_tokens":      pTokens + cTokens,
			},
		})
		m.insertDBEntry(ctx, normOpenAI.Hash, targetModel, normOpenAI.CanonicalJSON, openAIResp, pTokens, cTokens)
	}

	// 2. Anthropic Schema Synthesis (for Claude Code in VS Code)
	anthPayload := map[string]interface{}{
		"model": targetModel,
		"messages": []map[string]string{
			{"role": "user", "content": item.UserPrompt},
		},
		"max_tokens": 4096,
	}
	if item.SystemPrompt != "" {
		anthPayload["system"] = item.SystemPrompt
	}
	anthBytes, _ := json.Marshal(anthPayload)
	normAnth, err := cache.NormalizePayload(anthBytes, normOpts)
	if err == nil {
		anthResp, _ := json.Marshal(map[string]interface{}{
			"id":          fmt.Sprintf("msg-mined-%s", item.ID),
			"type":        "message",
			"role":        "assistant",
			"model":       targetModel,
			"content":     []map[string]string{{"type": "text", "text": answer}},
			"stop_reason": "end_turn",
			"usage": map[string]int{
				"input_tokens":  pTokens,
				"output_tokens": cTokens,
			},
		})
		m.insertDBEntry(ctx, normAnth.Hash, targetModel, normAnth.CanonicalJSON, anthResp, pTokens, cTokens)
	}

	// 3. Store into Tier-3 Semantic Similarity Cache
	if m.semCache != nil {
		respBytes, _ := json.Marshal(map[string]interface{}{
			"id":      fmt.Sprintf("chatcmpl-mined-%s", item.ID),
			"model":   targetModel,
			"choices": []map[string]interface{}{{"message": map[string]string{"role": "assistant", "content": answer}}},
		})
		_ = m.semCache.Store(ctx, normOpenAI.Hash, targetModel, item.SystemPrompt, "", item.UserPrompt, respBytes, 30*24*time.Hour)
	}

	return nil
}

func (m *CacheMiner) insertDBEntry(ctx context.Context, hash, model, normalizedPrompt string, responsePayload []byte, pTokens, cTokens int) {
	if m.database == nil {
		return
	}

	query := `
		INSERT OR REPLACE INTO cache_entries (
			hash, model, normalized_prompt, response_payload,
			prompt_tokens, completion_tokens, hit_count,
			created_at, last_accessed_at, ttl_seconds, is_pinned, is_semantic
		) VALUES (?, ?, ?, ?, ?, ?, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 2592000, 1, 0)
	`
	_, _ = m.database.ExecContext(ctx, query, hash, model, normalizedPrompt, responsePayload, pTokens, cTokens)
}

// ExportOptions defines filtering and privacy sanitation options for cache export.
type ExportOptions struct {
	MinHits  int
	Model    string
	Sanitize bool
}

var (
	secretKeyRegex = regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9_\-]{20,}|gsk_[a-zA-Z0-9_\-]{20,}|nvapi-[a-zA-Z0-9_\-]{20,}|Bearer\s+[a-zA-Z0-9_\-\.]{20,})`)
	homePathRegex  = regexp.MustCompile(`(?i)([A-Za-z]:[\\/]+users[\\/]+[^\s\\/"',;:]+|/users/[^\s/"',;:]+|/home/[^\s/"',;:]+)`)
	privateIPRegex = regexp.MustCompile(`\b(192\.168\.\d{1,3}\.\d{1,3}|10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(1[6-9]|2\d|3[0-1])\.\d{1,3}\.\d{1,3})\b`)
)

// SanitizeContent scrubs API keys, user home paths, and private IPs from prompt/response payloads.
func SanitizeContent(input string) string {
	s := secretKeyRegex.ReplaceAllString(input, "[REDACTED_API_KEY]")
	s = homePathRegex.ReplaceAllString(s, "/home/dev")
	s = privateIPRegex.ReplaceAllString(s, "127.0.0.1")
	return s
}

// ExportCacheToGz dumps existing cache entries from SQLite into a Gzip-compressed JSON stream.
func ExportCacheToGz(database *db.DB, writer io.Writer) (int, error) {
	return ExportCacheWithOptions(database, writer, ExportOptions{})
}

// ExportCacheWithOptions dumps filtered and sanitized cache entries from SQLite into Gzip-compressed JSON.
func ExportCacheWithOptions(database *db.DB, writer io.Writer, opts ExportOptions) (int, error) {
	query := `
		SELECT hash, model, normalized_prompt, response_payload, prompt_tokens, completion_tokens, ttl_seconds, is_semantic
		FROM cache_entries
		WHERE hit_count >= ?
	`
	args := []interface{}{opts.MinHits}
	if opts.Model != "" {
		query += " AND model = ?"
		args = append(args, opts.Model)
	}

	rows, err := database.QueryContext(context.Background(), query, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to query cache entries for export: %w", err)
	}
	defer rows.Close()

	var items []CacheExportItem
	for rows.Next() {
		var item CacheExportItem
		var rawPayload []byte
		var isSem int
		if err := rows.Scan(&item.Hash, &item.Model, &item.NormalizedPrompt, &rawPayload, &item.PromptTokens, &item.CompletionTokens, &item.TTLSeconds, &isSem); err == nil {
			item.ResponsePayload = string(rawPayload)
			item.IsSemantic = (isSem == 1)

			if opts.Sanitize {
				item.NormalizedPrompt = SanitizeContent(item.NormalizedPrompt)
				item.ResponsePayload = SanitizeContent(item.ResponsePayload)
			}

			items = append(items, item)
		}
	}

	gzWriter := gzip.NewWriter(writer)

	encoder := json.NewEncoder(gzWriter)
	if err := encoder.Encode(items); err != nil {
		_ = gzWriter.Close()
		return 0, fmt.Errorf("failed to encode items: %w", err)
	}

	if err := gzWriter.Close(); err != nil {
		return 0, fmt.Errorf("failed to close gzip writer: %w", err)
	}

	return len(items), nil
}

// ImportCacheFromGz ingests compressed cache entries into SQLite using batch INSERT OR IGNORE.
func ImportCacheFromGz(database *db.DB, reader io.Reader) (int, error) {
	gzReader, err := gzip.NewReader(reader)
	if err != nil {
		return 0, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzReader.Close()

	var items []CacheExportItem
	decoder := json.NewDecoder(gzReader)
	if err := decoder.Decode(&items); err != nil {
		return 0, fmt.Errorf("failed to decode cache items: %w", err)
	}

	if len(items) == 0 {
		return 0, nil
	}

	tx, err := database.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var beforeCount int
	_ = tx.QueryRow("SELECT COUNT(*) FROM cache_entries").Scan(&beforeCount)

	stmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO cache_entries (
			hash, model, normalized_prompt, response_payload,
			prompt_tokens, completion_tokens, hit_count,
			created_at, last_accessed_at, ttl_seconds, is_pinned, is_semantic
		) VALUES (?, ?, ?, ?, ?, ?, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ?, 1, ?)
	`)
	if err != nil {
		return 0, fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, item := range items {
		isSemInt := 0
		if item.IsSemantic {
			isSemInt = 1
		}
		_, err := stmt.Exec(item.Hash, item.Model, item.NormalizedPrompt, []byte(item.ResponsePayload), item.PromptTokens, item.CompletionTokens, item.TTLSeconds, isSemInt)
		if err != nil {
			return 0, fmt.Errorf("failed to insert item %s: %w", item.Hash, err)
		}
	}

	var afterCount int
	_ = tx.QueryRow("SELECT COUNT(*) FROM cache_entries").Scan(&afterCount)

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit import transaction: %w", err)
	}

	return afterCount - beforeCount, nil
}

// PackOptions defines configuration for merging and packaging the starter cache.
type PackOptions struct {
	MinHits        int
	MaxPromptBytes int // Maximum prompt byte length to include in starter pack (default 65536)
	Sanitize       bool
}

// PackResult summarizes the merge and packaging output.
type PackResult struct {
	TotalEntries    int    `json:"total_entries"`
	ExistingEntries int    `json:"existing_entries"`
	MergedFromDB    int    `json:"merged_from_db"`
	SizeBytes       int    `json:"size_bytes"`
	TargetPath      string `json:"target_path"`
}

// PackStarterCache merges existing starter pack entries with entries from SQLite database,
// applies automated privacy sanitization, deduplicates by SHA-256 hash, and writes directly to targetGzPath.
func PackStarterCache(database *db.DB, targetGzPath string, opts PackOptions) (*PackResult, error) {
	itemsMap := make(map[string]CacheExportItem)

	// 1. Read existing starter pack entries if archive already exists
	if _, err := os.Stat(targetGzPath); err == nil {
		if data, err := os.ReadFile(targetGzPath); err == nil {
			if gzReader, err := gzip.NewReader(bytes.NewReader(data)); err == nil {
				var existing []CacheExportItem
				if err := json.NewDecoder(gzReader).Decode(&existing); err == nil {
					for _, it := range existing {
						itemsMap[it.Hash] = it
					}
				}
				_ = gzReader.Close()
			}
		}
	}
	existingCount := len(itemsMap)
	mergedFromDB := 0

	// 2. Extract and sanitize entries from local database
	if database != nil {
		maxBytes := opts.MaxPromptBytes
		if maxBytes == 0 {
			maxBytes = 65536 // 64KB default to keep binary compact and skip session transcript dumps
		}

		rows, err := database.QueryContext(context.Background(), `
			SELECT hash, model, normalized_prompt, response_payload, prompt_tokens, completion_tokens, ttl_seconds, is_semantic
			FROM cache_entries
			WHERE hit_count >= ?
		`, opts.MinHits)
		if err != nil {
			return nil, fmt.Errorf("failed to query database cache entries: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var it CacheExportItem
			var rawPayload []byte
			var isSem int
			if err := rows.Scan(&it.Hash, &it.Model, &it.NormalizedPrompt, &rawPayload, &it.PromptTokens, &it.CompletionTokens, &it.TTLSeconds, &isSem); err == nil {
				if maxBytes > 0 && len(it.NormalizedPrompt) > maxBytes {
					continue
				}

				it.ResponsePayload = string(rawPayload)
				it.IsSemantic = (isSem == 1)

				if opts.Sanitize {
					it.NormalizedPrompt = SanitizeContent(it.NormalizedPrompt)
					it.ResponsePayload = SanitizeContent(it.ResponsePayload)
				}

				if _, exists := itemsMap[it.Hash]; !exists {
					mergedFromDB++
				}
				itemsMap[it.Hash] = it
			}
		}
	}

	// 3. Flatten map into list
	var finalItems []CacheExportItem
	for _, it := range itemsMap {
		finalItems = append(finalItems, it)
	}

	// 4. Encode to gzip
	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gzWriter).Encode(finalItems); err != nil {
		_ = gzWriter.Close()
		return nil, fmt.Errorf("failed to encode starter pack json: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		return nil, fmt.Errorf("failed to close gzip writer: %w", err)
	}

	// 5. Ensure parent directory exists and write archive
	if err := os.MkdirAll(filepath.Dir(targetGzPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory for %s: %w", targetGzPath, err)
	}
	if err := os.WriteFile(targetGzPath, buf.Bytes(), 0644); err != nil {
		return nil, fmt.Errorf("failed to write %s: %w", targetGzPath, err)
	}

	return &PackResult{
		TotalEntries:    len(finalItems),
		ExistingEntries: existingCount,
		MergedFromDB:    mergedFromDB,
		SizeBytes:       buf.Len(),
		TargetPath:      targetGzPath,
	}, nil
}
