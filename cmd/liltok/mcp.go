package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/primaybr/liltok/internal/db"
	"github.com/spf13/cobra"
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      interface{}   `json:"id,omitempty"`
	Result  interface{}   `json:"result,omitempty"`
	Error   *jsonRPCError `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

type toolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type toolCallResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func newMCPCommand() *cobra.Command {
	var gatewayURL string

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Start Liltok as a Model Context Protocol (MCP) server over stdio",
		Long:  "Runs a standard JSON-RPC 2.0 MCP server on standard I/O allowing agents (such as Google Antigravity) to query Liltok for cached inferences, idioms, and metrics.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if gatewayURL == "" {
				gatewayURL = "http://localhost:8080"
			}
			gatewayURL = strings.TrimRight(gatewayURL, "/")

			return runMCPLoop(gatewayURL)
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "http://localhost:8080", "Liltok gateway HTTP endpoint")
	return cmd
}

func runMCPLoop(gatewayURL string) error {
	scanner := bufio.NewScanner(os.Stdin)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	tools := []mcpTool{
		{
			Name:        "liltok_ask",
			Description: "Query Liltok AI gateway to execute prompts with automatic multi-tier caching (Tier-1 local SQLite hit for 0ms, or routed upstream with provider KV-cache discounts). Saves tokens and avoids context exhaustion.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"prompt": map[string]interface{}{
						"type":        "string",
						"description": "The prompt or question to execute via Liltok",
					},
					"model": map[string]interface{}{
						"type":        "string",
						"description": "Target model (optional, e.g. claude-opus-5, claude-sonnet-5, gpt-4o, llama-3.3-70b-versatile, defaults to claude-opus-5)",
					},
					"system": map[string]interface{}{
						"type":        "string",
						"description": "Optional system instructions",
					},
					"api_key": map[string]interface{}{
						"type":        "string",
						"description": "Optional API key for uncached upstream requests (defaults to environment or config)",
					},
				},
				"required": []string{"prompt"},
			},
		},
		{
			Name:        "liltok_cache_search",
			Description: "Search local Liltok cache store (~/.liltok/liltok.db) for previously cached prompts, code snippets, or diagnostics.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "Search keyword or pattern to find in cached prompt records",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "liltok_stats",
			Description: "Get real-time operational metrics and spend savings from the running Liltok gateway.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			sendError(nil, -32700, "Parse error")
			continue
		}

		handleMCPRequest(req, tools, gatewayURL)
	}

	return scanner.Err()
}

func handleMCPRequest(req jsonRPCRequest, tools []mcpTool, gatewayURL string) {
	switch req.Method {
	case "initialize":
		res := map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]interface{}{
				"name":    "liltok",
				"version": "0.1.4-beta",
			},
		}
		sendResult(req.ID, res)

	case "notifications/initialized":
		// No response required for notifications

	case "ping":
		sendResult(req.ID, map[string]interface{}{})

	case "tools/list":
		res := map[string]interface{}{
			"tools": tools,
		}
		sendResult(req.ID, res)

	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			sendError(req.ID, -32602, "Invalid tool parameters")
			return
		}

		result := executeTool(params.Name, params.Arguments, gatewayURL)
		sendResult(req.ID, result)

	default:
		if req.ID != nil {
			sendError(req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
		}
	}
}

func executeTool(name string, args map[string]interface{}, gatewayURL string) toolCallResult {
	switch name {
	case "liltok_ask":
		prompt, _ := args["prompt"].(string)
		if strings.TrimSpace(prompt) == "" {
			return toolError("prompt parameter is required")
		}
		model, _ := args["model"].(string)
		if model == "" {
			model = "claude-opus-5"
		}
		system, _ := args["system"].(string)
		apiKey, _ := args["api_key"].(string)

		return callLiltokChat(gatewayURL, model, system, prompt, apiKey)

	case "liltok_cache_search":
		query, _ := args["query"].(string)
		if strings.TrimSpace(query) == "" {
			return toolError("query parameter is required")
		}
		return searchLiltokCache(gatewayURL, query)

	case "liltok_stats":
		return getLiltokStats(gatewayURL)

	default:
		return toolError(fmt.Sprintf("unknown tool: %s", name))
	}
}

func callLiltokChat(gatewayURL, model, system, prompt, apiKey string) toolCallResult {
	var messages []map[string]string
	if system != "" {
		messages = append(messages, map[string]string{"role": "system", "content": system})
	}
	messages = append(messages, map[string]string{"role": "user", "content": prompt})

	bodyObj := map[string]interface{}{
		"model":    model,
		"messages": messages,
		"stream":   false,
	}

	bodyBytes, err := json.Marshal(bodyObj)
	if err != nil {
		return toolError(fmt.Sprintf("failed to encode request: %v", err))
	}

	client := &http.Client{Timeout: 90 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), "POST", gatewayURL+"/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return toolError(fmt.Sprintf("failed to create HTTP request: %v", err))
	}
	req.Header.Set("Content-Type", "application/json")

	if apiKey == "" {
		lowerModel := strings.ToLower(model)
		if strings.Contains(lowerModel, "claude") {
			apiKey = os.Getenv("ANTHROPIC_API_KEY")
		} else if strings.Contains(lowerModel, "groq") || strings.Contains(lowerModel, "llama") {
			apiKey = os.Getenv("GROQ_API_KEY")
		} else if strings.Contains(lowerModel, "gemini") {
			apiKey = os.Getenv("GEMINI_API_KEY")
		} else if strings.Contains(lowerModel, "openrouter") {
			apiKey = os.Getenv("OPENROUTER_API_KEY")
		} else {
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
	}
	if apiKey == "" {
		apiKey = os.Getenv("LILTOK_API_KEY")
	}

	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey)
	}

	start := time.Now()
	resp, err := client.Do(req)
	duration := time.Since(start)
	if err != nil {
		return toolError(fmt.Sprintf("gateway connection failed (is liltok running at %s?): %v", gatewayURL, err))
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return toolError(fmt.Sprintf("failed to read gateway response: %v", err))
	}

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return toolError("Cache MISS and no upstream API key provided. To route uncached queries upstream, provide the 'api_key' argument or configure provider keys in ~/.liltok/liltok.yaml.")
		}
		return toolError(fmt.Sprintf("gateway returned HTTP %d: %s", resp.StatusCode, string(respBytes)))
	}

	var completion struct {
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
		Data *struct {
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
		} `json:"data"`
	}

	if err := json.Unmarshal(respBytes, &completion); err != nil {
		return toolText(string(respBytes))
	}

	var content string
	if len(completion.Choices) > 0 {
		content = completion.Choices[0].Message.Content
	} else if completion.Data != nil && len(completion.Data.Choices) > 0 {
		content = completion.Data.Choices[0].Message.Content
		if completion.Usage.PromptTokens == 0 {
			completion.Usage = completion.Data.Usage
		}
	}

	cacheStatus := resp.Header.Get("X-Cache")
	cacheTier := resp.Header.Get("X-Cache-Tier")
	if cacheStatus == "" {
		if duration < 20*time.Millisecond {
			cacheStatus = "HIT"
		} else {
			cacheStatus = "MISS"
		}
	}

	headerNote := fmt.Sprintf("[Liltok Gateway | Cache: %s | Tier: %s | Model: %s | Latency: %dms | Tokens: %d/%d]",
		cacheStatus, cacheTier, model, duration.Milliseconds(),
		completion.Usage.PromptTokens, completion.Usage.CompletionTokens)

	return toolText(fmt.Sprintf("%s\n\n%s", headerNote, content))
}

func searchLiltokCache(gatewayURL, query string) toolCallResult {
	// 1. Try querying the running gateway first via HTTP to avoid SQLite lock contention
	if gatewayURL != "" {
		searchURL := fmt.Sprintf("%s/api/v1/cache?q=%s", gatewayURL, url.QueryEscape(query))
		client := &http.Client{Timeout: 5 * time.Second}
		if resp, err := client.Get(searchURL); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var items []struct {
					Hash          string `json:"hash"`
					Model         string `json:"model"`
					PromptPreview string `json:"prompt_preview"`
					HitCount      int    `json:"hit_count"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&items); err == nil && len(items) > 0 {
					var sb strings.Builder
					sb.WriteString(fmt.Sprintf("Search results for '%s' in Liltok cache:\n\n", query))
					for i, item := range items {
						if i >= 5 {
							break
						}
						hashShort := item.Hash
						if len(hashShort) > 12 {
							hashShort = hashShort[:12]
						}
						sb.WriteString(fmt.Sprintf("### Match %d: Hash %s (Hits: %d | Model: %s)\n", i+1, hashShort, item.HitCount, item.Model))
						sb.WriteString(fmt.Sprintf("**Prompt Preview:** %s\n\n---\n", item.PromptPreview))
					}
					return toolText(sb.String())
				}
			}
		}
	}

	// 2. Direct local SQLite query fallback if gateway is offline
	dbPath := resolveDBPath()
	database, err := db.Open(dbPath)
	if err != nil {
		return toolError(fmt.Sprintf("failed to open cache db at %s: %v", dbPath, err))
	}
	defer database.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := database.QueryContext(ctx, `
		SELECT hash, model, normalized_prompt, response_payload, hit_count, last_accessed_at
		FROM cache_entries
		WHERE normalized_prompt LIKE ?
		ORDER BY hit_count DESC
		LIMIT 5
	`, "%"+query+"%")
	if err != nil {
		return toolError(fmt.Sprintf("cache search query failed: %v", err))
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Search results for '%s' in local cache:\n\n", query))

	count := 0
	for rows.Next() {
		count++
		var hash, model, prompt, updatedAt string
		var payload []byte
		var hits int64

		if err := rows.Scan(&hash, &model, &prompt, &payload, &hits, &updatedAt); err != nil {
			continue
		}

		previewPrompt := prompt
		if len(previewPrompt) > 200 {
			previewPrompt = previewPrompt[:200] + "..."
		}

		rawBytes := payload
		if len(payload) > 2 && payload[0] == 0x1f && payload[1] == 0x8b {
			if gz, err := gzip.NewReader(bytes.NewReader(payload)); err == nil {
				if decompressed, err := io.ReadAll(gz); err == nil {
					rawBytes = decompressed
				}
				_ = gz.Close()
			}
		}

		var respText string
		var obj struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(rawBytes, &obj); err == nil {
			if len(obj.Choices) > 0 {
				respText = obj.Choices[0].Message.Content
			} else if len(obj.Content) > 0 {
				respText = obj.Content[0].Text
			}
		}
		if respText == "" {
			respText = string(rawBytes)
		}
		respText = strings.ToValidUTF8(respText, "")
		if len(respText) > 300 {
			respText = respText[:300] + "..."
		}

		sb.WriteString(fmt.Sprintf("### Match %d: Hash %s (Hits: %d | Model: %s)\n", count, hash[:12], hits, model))
		sb.WriteString(fmt.Sprintf("**Prompt Preview:** %s\n\n", previewPrompt))
		sb.WriteString(fmt.Sprintf("**Response Preview:** %s\n\n---\n", respText))
	}

	if count == 0 {
		return toolText(fmt.Sprintf("No cache entries found matching '%s' in %s", query, dbPath))
	}

	return toolText(sb.String())
}

func getLiltokStats(gatewayURL string) toolCallResult {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(gatewayURL + "/api/v1/overview")
	if err != nil {
		return toolError(fmt.Sprintf("failed to connect to Liltok at %s: %v", gatewayURL, err))
	}
	defer resp.Body.Close()

	var stats struct {
		Status         string  `json:"status"`
		UptimeSeconds  int64   `json:"uptime_seconds"`
		TotalRequests  int64   `json:"total_requests"`
		TotalHits      int64   `json:"total_hits"`
		LocalHits      int64   `json:"local_hits"`
		ModelCacheHits int64   `json:"model_cache_hits"`
		HitRatePercent float64 `json:"hit_rate_percent"`
		TotalCostUSD   float64 `json:"total_cost_usd"`
		TotalSavedUSD  float64 `json:"total_saved_usd"`
		AvgLatencyMs   float64 `json:"avg_latency_ms"`
		CacheEntries   int64   `json:"cache_entries"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return toolError(fmt.Sprintf("failed to parse stats response: %v", err))
	}

	uncached := stats.TotalRequests - stats.TotalHits
	if uncached < 0 {
		uncached = 0
	}

	report := fmt.Sprintf(`Liltok AI Gateway Operational Status: %s
Uptime: %ds
Total Requests Processed: %d
Cache Optimization Rate: %.1f%%
  - Local Exact/Semantic Hits (0ms latency, $0.00 cost): %d
  - Upstream Model KV-Cache Hits (50%%-90%% discount): %d
  - Uncached Full Misses: %d
Total Dollars Saved: $%.4f
Gross Upstream Spend: $%.4f
Average Latency: %.1fms
Active Local Cache Entries: %d`,
		stats.Status, stats.UptimeSeconds, stats.TotalRequests, stats.HitRatePercent,
		stats.LocalHits, stats.ModelCacheHits, uncached,
		stats.TotalSavedUSD, stats.TotalCostUSD, stats.AvgLatencyMs, stats.CacheEntries)

	return toolText(report)
}

func toolText(text string) toolCallResult {
	return toolCallResult{
		Content: []toolContent{
			{
				Type: "text",
				Text: text,
			},
		},
	}
}

func toolError(msg string) toolCallResult {
	return toolCallResult{
		Content: []toolContent{
			{
				Type: "text",
				Text: "Error: " + msg,
			},
		},
		IsError: true,
	}
}

func sendResult(id interface{}, result interface{}) {
	res := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, _ := json.Marshal(res)
	os.Stdout.Write(data)
	os.Stdout.Write([]byte("\n"))
}

func sendError(id interface{}, code int, message string) {
	res := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonRPCError{
			Code:    code,
			Message: message,
		},
	}
	data, _ := json.Marshal(res)
	os.Stdout.Write(data)
	os.Stdout.Write([]byte("\n"))
}
