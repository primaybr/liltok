package sync

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/miner"
	"github.com/primaybr/liltok/internal/telemetry"
)

// SyncResult details the outcome of an OTA cache synchronization run.
type SyncResult struct {
	NewEntries int           `json:"new_entries"`
	UpToDate   bool          `json:"up_to_date"`
	ETag       string        `json:"etag"`
	Duration   time.Duration `json:"duration"`
}

// CacheSyncer coordinates Over-The-Air (OTA) cache synchronization from a remote CDN or GitHub Releases.
type CacheSyncer struct {
	database   *db.DB
	syncURL    string
	httpClient *http.Client
}

// NewCacheSyncer instantiates a cache syncer.
func NewCacheSyncer(database *db.DB, syncURL string, client *http.Client) *CacheSyncer {
	if client == nil {
		client = &http.Client{
			Timeout: 45 * time.Second,
		}
	}
	if syncURL == "" {
		syncURL = "https://github.com/primaybr/liltok/releases/latest/download/starter_cache.json.gz"
	}

	cs := &CacheSyncer{
		database:   database,
		syncURL:    syncURL,
		httpClient: client,
	}
	return cs
}

// Sync executes an HTTP conditional GET against the configured remote sync URL.
func (s *CacheSyncer) Sync(ctx context.Context, force bool) (SyncResult, error) {
	start := time.Now()
	res := SyncResult{}

	if s.syncURL == "" {
		return res, fmt.Errorf("sync URL is empty")
	}

	httpReq, err := http.NewRequestWithContext(ctx, "GET", s.syncURL, nil)
	if err != nil {
		return res, fmt.Errorf("failed to create sync request: %w", err)
	}

	httpReq.Header.Set("User-Agent", "liltok-cache-syncer/0.2.5-beta")

	// Apply conditional HTTP headers unless forced
	var lastETag, lastModified string
	if !force && s.database != nil {
		_ = s.database.QueryRowContext(ctx, "SELECT value FROM sync_metadata WHERE key = 'last_etag'").Scan(&lastETag)
		_ = s.database.QueryRowContext(ctx, "SELECT value FROM sync_metadata WHERE key = 'last_modified'").Scan(&lastModified)

		if lastETag != "" {
			httpReq.Header.Set("If-None-Match", lastETag)
		}
		if lastModified != "" {
			httpReq.Header.Set("If-Modified-Since", lastModified)
		}
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return res, fmt.Errorf("network error during cache sync: %w", err)
	}
	defer resp.Body.Close()

	res.Duration = time.Since(start)

	// 1. Unmodified (HTTP 304)
	if resp.StatusCode == http.StatusNotModified {
		res.UpToDate = true
		res.ETag = lastETag
		return res, nil
	}

	// 2. Not Found (HTTP 404)
	if resp.StatusCode == http.StatusNotFound {
		return res, fmt.Errorf("remote cache asset not found at %s (HTTP 404)", s.syncURL)
	}

	// 3. Other unexpected status
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("remote cache server returned HTTP %d", resp.StatusCode)
	}

	// 4. Downloaded new payload (HTTP 200)
	if s.database == nil {
		return res, fmt.Errorf("cannot import the downloaded cache pack: no database")
	}
	inserted, err := miner.ImportCacheFromGz(s.database, resp.Body)
	if err != nil {
		return res, fmt.Errorf("failed to ingest downloaded cache pack: %w", err)
	}

	res.NewEntries = inserted
	res.UpToDate = false
	res.ETag = strings.TrimSpace(resp.Header.Get("ETag"))

	// Persist metadata
	newModified := strings.TrimSpace(resp.Header.Get("Last-Modified"))
	if s.database != nil {
		if res.ETag != "" {
			s.setMetadata("last_etag", res.ETag)
		}
		if newModified != "" {
			s.setMetadata("last_modified", newModified)
		}
		s.setMetadata("last_sync_timestamp", time.Now().Format(time.RFC3339))
	}

	return res, nil
}

func (s *CacheSyncer) setMetadata(key, val string) {
	if s.database == nil {
		return
	}
	query := `
		INSERT OR REPLACE INTO sync_metadata (key, value, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
	`
	_, _ = s.database.Exec(query, key, val)
}

// StartBackgroundLoop spawns a non-blocking goroutine that checks for updates on startup
// and runs periodically every interval.
func (s *CacheSyncer) StartBackgroundLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}

	go func() {
		// Non-blocking initial sync 5 seconds after boot
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			s.runBackgroundTick(ctx)
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runBackgroundTick(ctx)
			}
		}
	}()
}

func (s *CacheSyncer) runBackgroundTick(ctx context.Context) {
	res, err := s.Sync(ctx, false)
	if err != nil {
		telemetry.Log.Debug().
			Err(err).
			Str("url", s.syncURL).
			Msg("OTA Cache Sync check skipped or remote unavailable")
		return
	}

	if res.UpToDate {
		telemetry.Log.Debug().
			Str("etag", res.ETag).
			Msg("OTA Cache is already up to date (304 Not Modified)")
	} else {
		telemetry.Log.Info().
			Int("new_entries", res.NewEntries).
			Str("etag", res.ETag).
			Str("duration", res.Duration.String()).
			Msg("OTA Cache Sync successfully merged new community entries")
	}
}
