package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/telemetry"
)

// InProxyToolNames lists read-only diagnostic tools executable directly inside the gateway.
var InProxyToolNames = map[string]bool{
	"liltok_stats":        true,
	"liltok_cache_search": true,
	"liltok_ask":          true,
	"liltok_memory_list":  true,
}

// IsInProxyTool reports whether toolName is an authorized read-only gateway tool.
func IsInProxyTool(toolName string) bool {
	return InProxyToolNames[strings.ToLower(strings.TrimSpace(toolName))]
}

// AreAllInProxyTools checks whether every tool call in toolCalls can be safely executed in-proxy.
func AreAllInProxyTools(toolCalls []provider.UnifiedToolCall) bool {
	if len(toolCalls) == 0 {
		return false
	}
	for _, tc := range toolCalls {
		if !IsInProxyTool(tc.Function.Name) {
			return false
		}
	}
	return true
}

// maxInProxyTurns limits sub-turn continuation to prevent runaway recursion.
const maxInProxyTurns = 3

// executeInProxyLoop executes read-only gateway tool calls within the proxy and continues
// the conversation with the upstream provider until a terminal response or external tool call is reached.
func (p *Proxy) executeInProxyLoop(ctx context.Context, unifiedReq *provider.UnifiedChatRequest, resp *provider.UnifiedChatResponse, routeAlias string) (*provider.UnifiedChatResponse, string, int, error) {
	winningProvider := ""
	turns := 0
	currentResp := resp

	for turns < maxInProxyTurns && len(currentResp.ToolCalls) > 0 && AreAllInProxyTools(currentResp.ToolCalls) {
		turns++
		telemetry.Log.Info().
			Int("turn", turns).
			Int("tool_count", len(currentResp.ToolCalls)).
			Msg("Executing read-only tool calls in-proxy")

		toolResults := make([]provider.UnifiedChatMessage, 0, len(currentResp.ToolCalls))
		for _, tc := range currentResp.ToolCalls {
			start := time.Now()
			out, err := p.executeInProxyTool(ctx, tc)
			if err != nil {
				out = fmt.Sprintf(`{"error": %q}`, err.Error())
			}
			telemetry.Log.Debug().
				Str("tool", tc.Function.Name).
				Int64("latency_ms", time.Since(start).Milliseconds()).
				Msg("In-proxy tool executed")

			toolResults = append(toolResults, provider.UnifiedChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    out,
			})
		}

		// Append assistant turn with tool calls
		unifiedReq.Messages = append(unifiedReq.Messages, provider.UnifiedChatMessage{
			Role:      "assistant",
			Content:   currentResp.Content,
			ToolCalls: currentResp.ToolCalls,
		})

		// Append tool result turns
		unifiedReq.Messages = append(unifiedReq.Messages, toolResults...)
		unifiedReq.RawPayload = nil // force providers to re-serialize updated messages

		// Dispatch continuation turn to upstream router
		nextResp, nextProvider, err := p.router.DispatchChat(ctx, unifiedReq, routeAlias)
		if err != nil {
			telemetry.Log.Warn().
				Err(err).
				Int("turn", turns).
				Msg("In-proxy continuation dispatch failed; returning current turn response")
			return currentResp, winningProvider, turns, nil
		}

		// Accumulate token consumption
		nextResp.Usage.PromptTokens += currentResp.Usage.PromptTokens
		nextResp.Usage.CompletionTokens += currentResp.Usage.CompletionTokens
		nextResp.Usage.TotalTokens += currentResp.Usage.TotalTokens

		currentResp = nextResp
		winningProvider = nextProvider
	}

	return currentResp, winningProvider, turns, nil
}

// executeInProxyTool executes a single read-only gateway tool and returns its output string.
func (p *Proxy) executeInProxyTool(ctx context.Context, tc provider.UnifiedToolCall) (string, error) {
	name := strings.ToLower(strings.TrimSpace(tc.Function.Name))
	args := tc.Function.Arguments

	switch name {
	case "liltok_stats":
		return p.execStats(ctx)
	case "liltok_cache_search":
		return p.execCacheSearch(ctx, args)
	case "liltok_ask":
		return p.execAsk(ctx, args)
	case "liltok_memory_list":
		return p.execMemoryList(ctx, args)
	default:
		return "", fmt.Errorf("unsupported in-proxy tool: %s", name)
	}
}

func (p *Proxy) execStats(ctx context.Context) (string, error) {
	if p.ledger != nil {
		stats, err := p.ledger.GetOverviewStats(ctx)
		if err == nil {
			data := map[string]interface{}{
				"status":              "healthy",
				"total_requests":      stats.TotalRequests,
				"total_hits":          stats.TotalHits,
				"hit_rate_percent":    stats.HitRatePercent,
				"tier1_exact_hits":    stats.Tier1ExactHits,
				"tier2_prefix_hits":   stats.Tier2PrefixHits,
				"tier3_semantic_hits": stats.Tier3SemanticHits,
				"total_saved_usd":     stats.TotalSavedUSD,
				"gross_spend_usd":     stats.GrossTokenSpendUSD,
				"avg_latency_ms":      stats.AvgLatencyMs,
			}
			bytes, _ := json.Marshal(data)
			return string(bytes), nil
		}
	}
	return `{"status":"healthy","service":"liltok"}`, nil
}

type cacheSearchArgs struct {
	Query   string `json:"query"`
	Pattern string `json:"pattern"`
	Limit   int    `json:"limit"`
}

func (p *Proxy) execCacheSearch(ctx context.Context, argsJSON string) (string, error) {
	var args cacheSearchArgs
	if len(argsJSON) > 0 {
		_ = json.Unmarshal([]byte(argsJSON), &args)
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		query = strings.TrimSpace(args.Pattern)
	}
	if query == "" {
		return `{"results":[],"message":"empty query"}`, nil
	}
	limit := args.Limit
	if limit <= 0 || limit > 20 {
		limit = 5
	}

	if p.database == nil {
		return `{"results":[]}`, nil
	}

	rows, err := p.database.QueryContext(ctx, `
		SELECT hash, model, normalized_prompt, hit_count, created_at
		FROM cache_entries
		WHERE normalized_prompt LIKE ?
		ORDER BY hit_count DESC, created_at DESC
		LIMIT ?
	`, "%"+query+"%", limit)
	if err != nil {
		return "", fmt.Errorf("cache search failed: %w", err)
	}
	defer rows.Close()

	type searchResult struct {
		Hash      string `json:"hash"`
		Model     string `json:"model"`
		Preview   string `json:"preview"`
		HitCount  int    `json:"hit_count"`
		CreatedAt string `json:"created_at"`
	}

	results := make([]searchResult, 0, limit)
	for rows.Next() {
		var r searchResult
		var prompt string
		var t time.Time
		if err := rows.Scan(&r.Hash, &r.Model, &prompt, &r.HitCount, &t); err == nil {
			r.CreatedAt = t.UTC().Format(time.RFC3339)
			if len(prompt) > 200 {
				r.Preview = prompt[:200] + "..."
			} else {
				r.Preview = prompt
			}
			results = append(results, r)
		}
	}

	data := map[string]interface{}{
		"query":   query,
		"count":   len(results),
		"results": results,
	}
	bytes, _ := json.Marshal(data)
	return string(bytes), nil
}

type askArgs struct {
	Prompt string `json:"prompt"`
	Query  string `json:"query"`
}

func (p *Proxy) execAsk(ctx context.Context, argsJSON string) (string, error) {
	var args askArgs
	if len(argsJSON) > 0 {
		_ = json.Unmarshal([]byte(argsJSON), &args)
	}
	query := strings.TrimSpace(args.Prompt)
	if query == "" {
		query = strings.TrimSpace(args.Query)
	}
	if query == "" {
		return `{"error":"prompt parameter is required"}`, nil
	}

	if p.database != nil {
		row := p.database.QueryRowContext(ctx, `
			SELECT hash, model, response_payload, hit_count
			FROM cache_entries
			WHERE normalized_prompt LIKE ?
			ORDER BY hit_count DESC, created_at DESC
			LIMIT 1
		`, "%"+query+"%")

		var hash, model string
		var payload []byte
		var hitCount int
		if err := row.Scan(&hash, &model, &payload, &hitCount); err == nil {
			var parsed map[string]interface{}
			contentPreview := ""
			if json.Unmarshal(payload, &parsed) == nil {
				if choices, ok := parsed["choices"].([]interface{}); ok && len(choices) > 0 {
					if cMap, ok := choices[0].(map[string]interface{}); ok {
						if msg, ok := cMap["message"].(map[string]interface{}); ok {
							if text, ok := msg["content"].(string); ok {
								contentPreview = text
							}
						}
					}
				} else if content, ok := parsed["content"].([]interface{}); ok && len(content) > 0 {
					if block, ok := content[0].(map[string]interface{}); ok {
						if text, ok := block["text"].(string); ok {
							contentPreview = text
						}
					}
				}
			}
			if contentPreview == "" && len(payload) > 0 {
				contentPreview = string(payload)
			}
			data := map[string]interface{}{
				"found":     true,
				"hash":      hash,
				"model":     model,
				"hit_count": hitCount,
				"response":  contentPreview,
			}
			bytes, _ := json.Marshal(data)
			return string(bytes), nil
		} else if err != sql.ErrNoRows {
			telemetry.Log.Warn().Err(err).Msg("execAsk database scan error")
		}
	}

	return fmt.Sprintf(`{"found":false,"message":"No exact or similar cache entry found for query %q."}`, query), nil
}

type memoryListArgs struct {
	ProjectKey string `json:"project_key"`
	Category   string `json:"category"`
	Limit      int    `json:"limit"`
}

func (p *Proxy) execMemoryList(ctx context.Context, argsJSON string) (string, error) {
	var args memoryListArgs
	if len(argsJSON) > 0 {
		_ = json.Unmarshal([]byte(argsJSON), &args)
	}

	if p.database == nil {
		return `{"memories":[]}`, nil
	}

	projectKey := db.NormalizeProjectKey(args.ProjectKey)
	limit := args.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	memories, err := p.database.ListMemories(ctx, projectKey, limit)
	if err != nil {
		return "", fmt.Errorf("list memories failed: %w", err)
	}

	if args.Category != "" {
		filtered := make([]db.MemoryEntry, 0, len(memories))
		for _, m := range memories {
			if strings.EqualFold(m.Category, args.Category) {
				filtered = append(filtered, m)
			}
		}
		memories = filtered
	}

	data := map[string]interface{}{
		"project_key": projectKey,
		"category":    args.Category,
		"count":       len(memories),
		"memories":    memories,
	}
	bytes, _ := json.Marshal(data)
	return string(bytes), nil
}
