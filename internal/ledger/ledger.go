package ledger

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/liltok/liltok/internal/db"
	"github.com/liltok/liltok/internal/telemetry"
)

// RequestLog represents an immutable record of an inference request in the financial ledger.
type RequestLog struct {
	ID               int64     `json:"id,omitempty"`
	RequestID        string    `json:"request_id"`
	Timestamp        time.Time `json:"timestamp"`
	APIKeyID         string    `json:"api_key_id,omitempty"`
	Model            string    `json:"model"`
	Provider         string    `json:"provider"`
	CacheStatus      string    `json:"cache_status"`
	CacheTier        string    `json:"cache_tier"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	LatencyMs        int64     `json:"latency_ms"`
	CostUSD          float64   `json:"cost_usd"`
	SavedUSD         float64   `json:"saved_usd"`
	StatusCode       int       `json:"status_code"`
	ErrorMessage     string    `json:"error_message,omitempty"`
}

// OverviewStats summarizes gateway performance and monetary savings.
type OverviewStats struct {
	TotalRequests  int64   `json:"total_requests"`
	TotalHits      int64   `json:"total_hits"`
	LocalHits      int64   `json:"local_hits"`
	ModelCacheHits int64   `json:"model_cache_hits"`
	HitRatePercent float64 `json:"hit_rate_percent"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	TotalSavedUSD  float64 `json:"total_saved_usd"`
	AvgLatencyMs   float64 `json:"avg_latency_ms"`
}

// Ledger provides asynchronous persistent audit logging and spend accounting.
type Ledger struct {
	db      *db.DB
	km      *KeyManager
	logChan chan *RequestLog
	quit    chan struct{}
	wg      sync.WaitGroup
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

	for {
		select {
		case logItem, ok := <-l.logChan:
			if !ok {
				return
			}
			l.persistLog(logItem)
		case <-l.quit:
			// Drain remaining logs
			for {
				select {
				case logItem, ok := <-l.logChan:
					if !ok {
						return
					}
					l.persistLog(logItem)
				default:
					return
				}
			}
		}
	}
}

func (l *Ledger) persistLog(item *RequestLog) {
	if l.db == nil || item == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if item.Timestamp.IsZero() {
		item.Timestamp = time.Now()
	}

	_, err := l.db.ExecContext(ctx, `
		INSERT INTO request_logs (
			request_id, timestamp, api_key_id, model, provider,
			cache_status, cache_tier, prompt_tokens, completion_tokens,
			cached_tokens, latency_ms, cost_usd, saved_usd,
			status_code, error_message
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		item.RequestID, item.Timestamp.UTC().Format(time.RFC3339), item.APIKeyID, item.Model, item.Provider,
		item.CacheStatus, item.CacheTier, item.PromptTokens, item.CompletionTokens,
		item.CachedTokens, item.LatencyMs, item.CostUSD, item.SavedUSD,
		item.StatusCode, item.ErrorMessage,
	)

	if err != nil {
		telemetry.Log.Error().
			Str("request_id", item.RequestID).
			Err(err).
			Msg("Failed to persist request log to SQLite")
		return
	}

	// Update virtual key current spend
	if item.APIKeyID != "" && item.CostUSD > 0 && l.km != nil {
		_ = l.km.UpdateSpend(ctx, item.APIKeyID, item.CostUSD)
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
		telemetry.Log.Warn().
			Str("request_id", item.RequestID).
			Msg("Ledger log channel full; audit entry dropped")
	}
}

// GetOverviewStats computes real-time operational aggregates.
func (l *Ledger) GetOverviewStats(ctx context.Context) (OverviewStats, error) {
	var stats OverviewStats
	if l.db == nil {
		return stats, nil
	}

	row := l.db.QueryRowContext(ctx, `
		SELECT 
			COUNT(*),
			SUM(CASE WHEN cache_status = 'HIT' THEN 1 ELSE 0 END),
			SUM(CASE WHEN (cache_tier = 'TIER2_PREFIX' OR cached_tokens > 0) AND cache_status != 'HIT' THEN 1 ELSE 0 END),
			COALESCE(SUM(cost_usd), 0.0),
			COALESCE(SUM(saved_usd), 0.0),
			COALESCE(AVG(latency_ms), 0.0)
		FROM request_logs
	`)

	var localHits, modelCacheHits sqlNullInt64
	var totalCost, totalSaved, avgLatency sqlNullFloat64

	err := row.Scan(&stats.TotalRequests, &localHits, &modelCacheHits, &totalCost, &totalSaved, &avgLatency)
	if err != nil {
		return stats, fmt.Errorf("failed to compute overview stats: %w", err)
	}

	stats.LocalHits = localHits.Int64
	stats.ModelCacheHits = modelCacheHits.Int64
	stats.TotalHits = stats.LocalHits + stats.ModelCacheHits
	stats.TotalCostUSD = totalCost.Float64
	stats.TotalSavedUSD = totalSaved.Float64
	stats.AvgLatencyMs = avgLatency.Float64

	if stats.TotalRequests > 0 {
		stats.HitRatePercent = (float64(stats.TotalHits) / float64(stats.TotalRequests)) * 100.0
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
