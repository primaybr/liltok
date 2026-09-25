package ledger

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/telemetry"
)

// RequestLog represents an immutable record of an inference request in the financial ledger.
type RequestLog struct {
	ID                int64     `json:"id,omitempty"`
	RequestID         string    `json:"request_id"`
	Timestamp         time.Time `json:"timestamp"`
	APIKeyID          string    `json:"api_key_id,omitempty"`
	Model             string    `json:"model"`
	RequestedModel    string    `json:"requested_model,omitempty"`
	Provider          string    `json:"provider"`
	CacheStatus       string    `json:"cache_status"`
	CacheTier         string    `json:"cache_tier"`
	PromptTokens      int       `json:"prompt_tokens"`
	CompletionTokens  int       `json:"completion_tokens"`
	CachedTokens      int       `json:"cached_tokens"`
	LatencyMs         int64     `json:"latency_ms"`
	CostUSD           float64   `json:"cost_usd"`
	PromptCostUSD     float64   `json:"prompt_cost_usd"`
	CompletionCostUSD float64   `json:"completion_cost_usd"`
	SavedUSD          float64   `json:"saved_usd"`
	PrunedBytes       int       `json:"pruned_bytes,omitempty"`
	PrunedTokens      int       `json:"pruned_tokens,omitempty"`
	StatusCode        int       `json:"status_code"`
	ErrorMessage      string    `json:"error_message,omitempty"`
}

// OverviewStats summarizes gateway performance and monetary savings.
type OverviewStats struct {
	TotalRequests          int64            `json:"total_requests"`
	TotalHits              int64            `json:"total_hits"`
	LocalHits              int64            `json:"local_hits"`
	ModelCacheHits         int64            `json:"model_cache_hits"`
	HitRatePercent         float64          `json:"hit_rate_percent"`
	Tier1ExactHits         int64            `json:"tier1_exact_hits"`
	Tier2PrefixHits        int64            `json:"tier2_prefix_hits"`
	Tier3SemanticHits      int64            `json:"tier3_semantic_hits"`
	Misses                 int64            `json:"misses"`
	TotalTokensIn          int64            `json:"total_tokens_in"`
	TotalTokensOut         int64            `json:"total_tokens_out"`
	TotalCostUSD           float64          `json:"total_cost_usd"`
	TotalPromptCostUSD     float64          `json:"total_prompt_cost_usd"`
	TotalCompletionCostUSD float64          `json:"total_completion_cost_usd"`
	GrossTokenSpendUSD     float64          `json:"gross_token_spend_usd"`
	TotalSavedUSD          float64          `json:"total_saved_usd"`
	TotalPrunedBytes       int64            `json:"total_pruned_bytes"`
	TotalPrunedTokens      int64            `json:"total_pruned_tokens"`
	AvgLatencyMs           float64          `json:"avg_latency_ms"`
	ProviderCounts         map[string]int64 `json:"provider_counts"`
}

const (
	// maxLogBatch caps how many queued records one transaction writes. The database has a single
	// connection shared with cache reads, so a batch must stay short.
	maxLogBatch = 256
	// dropWarnInterval rate-limits the warning logged when the queue is full and records are dropped.
	dropWarnInterval = 5 * time.Second
)

// Ledger provides asynchronous persistent audit logging and spend accounting.
type Ledger struct {
	db      *db.DB
	km      *KeyManager
	logChan chan *RequestLog
	quit    chan struct{}
	wg      sync.WaitGroup

	dropped      atomic.Int64 // records dropped since the last warning
	lastDropWarn atomic.Int64 // unix nanos of the last drop warning
}

// NewLedger initializes the ledger and starts the background writer worker.
func NewLedger(database *db.DB, km *KeyManager) *Ledger {
	l := &Ledger{
		db:      database,
		km:      km,
		logChan: make(chan *RequestLog, 2048),
		quit:    make(chan struct{}),
	}

	l.wg.Add(1)
	go l.worker()

	return l
}

func (l *Ledger) worker() {
	defer l.wg.Done()

	batch := make([]*RequestLog, 0, maxLogBatch)
	for {
		select {
		case logItem, ok := <-l.logChan:
			if !ok {
				return
			}
			batch = l.drain(append(batch[:0], logItem))
			l.persistBatch(batch)
		case <-l.quit:
			for {
				batch = l.drain(batch[:0])
				if len(batch) == 0 {
					return
				}
				l.persistBatch(batch)
			}
		}
	}
}

// drain appends queued records to batch without blocking, up to maxLogBatch.
func (l *Ledger) drain(batch []*RequestLog) []*RequestLog {
	for len(batch) < maxLogBatch {
		select {
		case logItem, ok := <-l.logChan:
			if !ok {
				return batch
			}
			batch = append(batch, logItem)
		default:
			return batch
		}
	}
	return batch
}

// persistBatch writes the records in one transaction, then applies virtual key spend. Spend updates
// run after the commit because the database has a single connection, which the transaction holds.
func (l *Ledger) persistBatch(items []*RequestLog) {
	if l.db == nil || len(items) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		telemetry.Log.Error().Err(err).Int("records", len(items)).Msg("Failed to begin request log batch")
		return
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO request_logs (
			request_id, timestamp, api_key_id, model, requested_model, provider,
			cache_status, cache_tier, prompt_tokens, completion_tokens,
			cached_tokens, latency_ms, cost_usd, prompt_cost_usd, completion_cost_usd, saved_usd,
			pruned_bytes, pruned_tokens,
			status_code, error_message
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		telemetry.Log.Error().Err(err).Int("records", len(items)).Msg("Failed to prepare request log insert")
		return
	}
	defer stmt.Close()

	written := make([]*RequestLog, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		if item.Timestamp.IsZero() {
			item.Timestamp = time.Now()
		}
		if item.RequestedModel == "" {
			item.RequestedModel = item.Model
		}
		_, err := stmt.ExecContext(ctx,
			item.RequestID, item.Timestamp.UTC().Format(time.RFC3339), item.APIKeyID, item.Model, item.RequestedModel, item.Provider,
			item.CacheStatus, item.CacheTier, item.PromptTokens, item.CompletionTokens,
			item.CachedTokens, item.LatencyMs, item.CostUSD, item.PromptCostUSD, item.CompletionCostUSD, item.SavedUSD,
			item.PrunedBytes, item.PrunedTokens,
			item.StatusCode, item.ErrorMessage,
		)
		if err != nil {
			telemetry.Log.Error().
				Str("request_id", item.RequestID).
				Err(err).
				Msg("Failed to persist request log to SQLite")
			continue
		}
		written = append(written, item)
	}

	if err := tx.Commit(); err != nil {
		telemetry.Log.Error().Err(err).Int("records", len(items)).Msg("Failed to commit request log batch")
		return
	}

	if l.km == nil {
		return
	}
	for _, item := range written {
		if item.APIKeyID != "" && item.CostUSD > 0 {
			_ = l.km.UpdateSpend(ctx, item.APIKeyID, item.CostUSD)
		}
	}
}

// Record queues an audit record asynchronously without blocking client requests.
func (l *Ledger) Record(item *RequestLog) {
	if item == nil {
		return
	}
	select {
	case l.logChan <- item:
	default:
		l.dropped.Add(1)
		now := time.Now().UnixNano()
		last := l.lastDropWarn.Load()
		if now-last >= int64(dropWarnInterval) && l.lastDropWarn.CompareAndSwap(last, now) {
			telemetry.Log.Warn().
				Int64("dropped", l.dropped.Swap(0)).
				Str("request_id", item.RequestID).
				Msg("Ledger log channel full; audit entries dropped")
		}
	}
}

// GetOverviewStats computes real-time operational aggregates.
func (l *Ledger) GetOverviewStats(ctx context.Context) (OverviewStats, error) {
	var stats OverviewStats
	stats.ProviderCounts = make(map[string]int64)
	if l.db == nil {
		return stats, nil
	}

	row := l.db.QueryRowContext(ctx, `
		SELECT 
			COUNT(*),
			SUM(CASE WHEN cache_status = 'HIT' AND (cache_tier = 'TIER1_EXACT' OR cache_tier IS NULL OR cache_tier = '' OR cache_tier NOT IN ('TIER2_PREFIX', 'TIER3_SEMANTIC')) THEN 1 ELSE 0 END),
			SUM(CASE WHEN (cache_tier = 'TIER2_PREFIX' OR (cached_tokens IS NOT NULL AND cached_tokens > 0)) AND cache_status != 'HIT' THEN 1 ELSE 0 END),
			SUM(CASE WHEN cache_status = 'HIT' AND cache_tier = 'TIER3_SEMANTIC' THEN 1 ELSE 0 END),
			SUM(CASE WHEN cache_status != 'HIT' AND (cache_tier != 'TIER2_PREFIX' OR cache_tier IS NULL) AND (cached_tokens IS NULL OR cached_tokens = 0) THEN 1 ELSE 0 END),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(cost_usd), 0.0),
			COALESCE(SUM(prompt_cost_usd), 0.0),
			COALESCE(SUM(completion_cost_usd), 0.0),
			COALESCE(SUM(saved_usd), 0.0),
			COALESCE(AVG(latency_ms), 0.0),
			COALESCE(SUM(pruned_bytes), 0),
			COALESCE(SUM(pruned_tokens), 0)
		FROM request_logs
	`)

	var t1Hits, t2Hits, t3Hits, misses, tokensIn, tokensOut sqlNullInt64
	var totalCost, promptCost, completionCost, totalSaved, avgLatency sqlNullFloat64
	var prunedBytes, prunedTokens sqlNullInt64

	err := row.Scan(
		&stats.TotalRequests,
		&t1Hits,
		&t2Hits,
		&t3Hits,
		&misses,
		&tokensIn,
		&tokensOut,
		&totalCost,
		&promptCost,
		&completionCost,
		&totalSaved,
		&avgLatency,
		&prunedBytes,
		&prunedTokens,
	)
	if err != nil {
		return stats, fmt.Errorf("failed to compute overview stats: %w", err)
	}

	stats.Tier1ExactHits = t1Hits.Int64
	stats.Tier2PrefixHits = t2Hits.Int64
	stats.Tier3SemanticHits = t3Hits.Int64
	stats.Misses = misses.Int64
	stats.LocalHits = stats.Tier1ExactHits + stats.Tier3SemanticHits
	stats.ModelCacheHits = stats.Tier2PrefixHits
	stats.TotalHits = stats.LocalHits + stats.ModelCacheHits
	stats.TotalTokensIn = tokensIn.Int64
	stats.TotalTokensOut = tokensOut.Int64
	stats.TotalCostUSD = totalCost.Float64
	stats.TotalPromptCostUSD = promptCost.Float64
	stats.TotalCompletionCostUSD = completionCost.Float64
	stats.TotalSavedUSD = totalSaved.Float64
	stats.TotalPrunedBytes = prunedBytes.Int64
	stats.TotalPrunedTokens = prunedTokens.Int64
	stats.GrossTokenSpendUSD = stats.TotalCostUSD + stats.TotalSavedUSD
	stats.AvgLatencyMs = avgLatency.Float64

	if stats.TotalRequests > 0 {
		stats.HitRatePercent = (float64(stats.TotalHits) / float64(stats.TotalRequests)) * 100.0
	}

	// Compute all-time routing share across all recorded providers
	pRows, err := l.db.QueryContext(ctx, `
		SELECT provider, COUNT(*)
		FROM request_logs
		GROUP BY provider
		ORDER BY COUNT(*) DESC
	`)
	if err == nil {
		defer pRows.Close()
		for pRows.Next() {
			var prov string
			var count int64
			if err := pRows.Scan(&prov, &count); err == nil {
				stats.ProviderCounts[prov] = count
			}
		}
	}

	return stats, nil
}

type sqlNullInt64 struct {
	Int64 int64
	Valid bool
}

func (s *sqlNullInt64) Scan(value interface{}) error {
	if value == nil {
		s.Int64, s.Valid = 0, false
		return nil
	}
	if v, ok := value.(int64); ok {
		s.Int64, s.Valid = v, true
		return nil
	}
	s.Int64, s.Valid = 0, false
	return nil
}

type sqlNullFloat64 struct {
	Float64 float64
	Valid   bool
}

func (s *sqlNullFloat64) Scan(value interface{}) error {
	if value == nil {
		s.Float64, s.Valid = 0.0, false
		return nil
	}
	if v, ok := value.(float64); ok {
		s.Float64, s.Valid = v, true
		return nil
	}
	s.Float64, s.Valid = 0.0, false
	return nil
}

// Close flushes all queued audit entries and terminates the background worker.
func (l *Ledger) Close() error {
	close(l.quit)
	l.wg.Wait()
	return nil
}
