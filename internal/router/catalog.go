package router

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/primaybr/liltok/internal/telemetry"
)

// Model catalog: health learned from real traffic. When an upstream answers that a model does not
// exist or was decommissioned, the provider/model pair is marked inactive and fallback chains skip
// it without calling it. After the recheck TTL one request is let through again; success
// reactivates the model, another "gone" answer restarts the TTL. This replaces the hand-maintained
// tables of deprecated models, which went stale whenever a provider retired a model.

const (
	CatalogActive   = "active"
	CatalogInactive = "inactive"

	defaultModelRecheck = 24 * time.Hour
)

// CatalogEntry is the recorded health of one provider/model pair.
type CatalogEntry struct {
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	FailCount int       `json:"fail_count"`
	// RecheckAt is when an inactive model is next allowed one request; zero for active models.
	RecheckAt time.Time `json:"recheck_at,omitempty"`
}

// CatalogStore persists catalog entries so learned health survives restarts.
type CatalogStore interface {
	LoadCatalog(ctx context.Context) ([]CatalogEntry, error)
	SaveCatalogEntry(ctx context.Context, e CatalogEntry) error
}

// ModelCatalog tracks provider/model health. The zero value is not usable; use NewModelCatalog.
type ModelCatalog struct {
	mu      sync.Mutex
	entries map[string]*CatalogEntry
	store   CatalogStore
	recheck time.Duration
	now     func() time.Time
}

// NewModelCatalog returns an in-memory catalog; recheck <= 0 uses 24 hours.
func NewModelCatalog(recheck time.Duration) *ModelCatalog {
	if recheck <= 0 {
		recheck = defaultModelRecheck
	}
	return &ModelCatalog{entries: map[string]*CatalogEntry{}, recheck: recheck, now: time.Now}
}

func catalogKey(providerName, model string) string {
	return strings.ToLower(providerName) + "\x00" + strings.ToLower(model)
}

// SetStore loads persisted entries and writes later status changes through to store.
func (c *ModelCatalog) SetStore(ctx context.Context, store CatalogStore) error {
	entries, err := store.LoadCatalog(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store = store
	for i := range entries {
		e := entries[i]
		c.entries[catalogKey(e.Provider, e.Model)] = &e
	}
	return nil
}

// Usable reports whether a request may be sent to the model. An inactive model is usable once its
// recheck time has passed; that request claims the recheck, so concurrent requests keep skipping it
// until the outcome is recorded or another TTL passes.
func (c *ModelCatalog) Usable(providerName, model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[catalogKey(providerName, model)]
	if !ok || e.Status != CatalogInactive {
		return true
	}
	now := c.now()
	if now.Sub(e.CheckedAt) < c.recheck {
		return false
	}
	e.CheckedAt = now
	return true
}

// MarkInactive records that the upstream reported the model as gone.
func (c *ModelCatalog) MarkInactive(providerName, model, reason string) {
	c.mu.Lock()
	key := catalogKey(providerName, model)
	e, ok := c.entries[key]
	if !ok {
		e = &CatalogEntry{Provider: strings.ToLower(providerName), Model: model}
		c.entries[key] = e
	}
	e.Status = CatalogInactive
	e.Reason = reason
	e.CheckedAt = c.now()
	e.FailCount++
	snapshot, store := *e, c.store
	c.mu.Unlock()

	telemetry.Log.Warn().
		Str("provider", snapshot.Provider).
		Str("model", snapshot.Model).
		Str("reason", reason).
		Dur("recheck_after", c.recheck).
		Msg("Upstream reports model unavailable; skipping it in fallback chains until recheck")
	c.persist(store, snapshot)
}

// MarkActive records a successful reply. It only writes when the model was inactive, so the
// common case is a map lookup.
func (c *ModelCatalog) MarkActive(providerName, model string) {
	c.mu.Lock()
	e, ok := c.entries[catalogKey(providerName, model)]
	if !ok || e.Status == CatalogActive {
		c.mu.Unlock()
		return
	}
	e.Status = CatalogActive
	e.Reason = ""
	e.CheckedAt = c.now()
	e.FailCount = 0
	snapshot, store := *e, c.store
	c.mu.Unlock()

	telemetry.Log.Info().
		Str("provider", snapshot.Provider).
		Str("model", snapshot.Model).
		Msg("Model answered again; restored to fallback chains")
	c.persist(store, snapshot)
}

// Reactivate clears an inactive mark by hand. It reports whether the model was inactive.
func (c *ModelCatalog) Reactivate(providerName, model string) bool {
	c.mu.Lock()
	e, ok := c.entries[catalogKey(providerName, model)]
	wasInactive := ok && e.Status == CatalogInactive
	c.mu.Unlock()
	if wasInactive {
		c.MarkActive(providerName, model)
	}
	return wasInactive
}

// Entries returns every recorded entry, with RecheckAt filled in for inactive models.
func (c *ModelCatalog) Entries() []CatalogEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CatalogEntry, 0, len(c.entries))
	for _, e := range c.entries {
		cp := *e
		if cp.Status == CatalogInactive {
			cp.RecheckAt = cp.CheckedAt.Add(c.recheck)
		}
		out = append(out, cp)
	}
	return out
}

func (c *ModelCatalog) persist(store CatalogStore, e CatalogEntry) {
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.SaveCatalogEntry(ctx, e); err != nil {
		telemetry.Log.Warn().Err(err).Str("provider", e.Provider).Str("model", e.Model).Msg("Failed to persist model catalog entry")
	}
}

// upstreamStatusRegex extracts the HTTP status adapters put in their errors
// ("returned status 404: ...", "error status 410: ...", "gemini error 404: ...").
var upstreamStatusRegex = regexp.MustCompile(`(?:status|error) (\d{3})\b`)

// modelGonePhrases are how providers word "this model does not exist / was retired". A 400 must
// use one of the first group: a 400 is often about the payload ("tools are not supported by this
// model"), which says nothing about whether the model exists.
var (
	modelGonePhrases400 = []string{"decommissioned", "does not exist", "not found", "not_found", "no longer"}
	modelGonePhrases404 = append([]string{"no endpoints", "not available", "unknown model", "invalid model"}, modelGonePhrases400...)
)

// modelGoneReason reports whether err says the requested model no longer exists at the provider,
// and returns a short reason. A 404 or 400 counts only when the body is about a model or endpoint
// and uses one of the modelGonePhrases, so a wrong base URL (404 page not found) or a bad payload
// does not mark models inactive. 410 Gone always counts.
func modelGoneReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	msg := err.Error()
	m := upstreamStatusRegex.FindStringSubmatch(msg)
	if m == nil {
		return "", false
	}
	code, _ := strconv.Atoi(m[1])
	lower := strings.ToLower(msg)
	switch code {
	case 410:
		return "status 410 gone", true
	case 400, 404:
		if !strings.Contains(lower, "model") && !strings.Contains(lower, "endpoint") {
			return "", false
		}
		phrases := modelGonePhrases404
		if code == 400 {
			phrases = modelGonePhrases400
		}
		for _, p := range phrases {
			if strings.Contains(lower, p) {
				return fmt.Sprintf("status %d: %s", code, p), true
			}
		}
	}
	return "", false
}

// modelAliases maps shorthand names clients send to the provider's real model ID. Keys are
// lowercase with the provider prefix removed; matching is exact. Retired model IDs do not belong
// here: the catalog learns those from the provider's own "not found" replies.
var modelAliases = map[string]map[string]string{
	"openrouter": {
		"auto": "openrouter/free",
	},
	"kilo": {
		"auto":      "kilo-auto/free",
		"free":      "kilo-auto/free",
		"kilo-auto": "kilo-auto/free",
	},
}

// ResolveModelAlias returns the provider's model ID for a shorthand name, and whether it was one.
func ResolveModelAlias(providerName, model string) (string, bool) {
	aliases := modelAliases[strings.ToLower(providerName)]
	if aliases == nil {
		return model, false
	}
	key := strings.ToLower(strings.TrimSpace(model))
	key = strings.TrimPrefix(key, strings.ToLower(providerName)+"/")
	if target, ok := aliases[key]; ok {
		return target, true
	}
	return model, false
}

func isModelAlias(providerName, model string) bool {
	_, ok := ResolveModelAlias(providerName, model)
	return ok
}

// SQLCatalogStore persists the catalog in the model_catalog table (migration 006).
type SQLCatalogStore struct {
	db *sql.DB
}

// NewSQLCatalogStore returns a store backed by db.
func NewSQLCatalogStore(db *sql.DB) *SQLCatalogStore {
	return &SQLCatalogStore{db: db}
}

func (s *SQLCatalogStore) LoadCatalog(ctx context.Context) ([]CatalogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, model, status, reason, checked_at, fail_count FROM model_catalog`)
	if err != nil {
		return nil, fmt.Errorf("load model catalog: %w", err)
	}
	defer rows.Close()
	var out []CatalogEntry
	for rows.Next() {
		var (
			e       CatalogEntry
			checked string
		)
		if err := rows.Scan(&e.Provider, &e.Model, &e.Status, &e.Reason, &checked, &e.FailCount); err != nil {
			return nil, err
		}
		e.CheckedAt, _ = time.Parse(time.RFC3339, checked)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLCatalogStore) SaveCatalogEntry(ctx context.Context, e CatalogEntry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO model_catalog (provider, model, status, reason, checked_at, fail_count)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, model) DO UPDATE SET
			status = excluded.status, reason = excluded.reason,
			checked_at = excluded.checked_at, fail_count = excluded.fail_count`,
		strings.ToLower(e.Provider), e.Model, e.Status, e.Reason, e.CheckedAt.UTC().Format(time.RFC3339), e.FailCount)
	return err
}
