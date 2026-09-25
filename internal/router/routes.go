package router

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/primaybr/liltok/internal/telemetry"
)

// Route editing: routes can be created or replaced at runtime and persisted in the
// provider_routes and route_targets tables. A saved route with a built-in ID (auto-resilient,
// free-first, premium-only) overrides that built-in until it is reset.

const maxRouteTargets = 100

var routeIDRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// RouteStore persists edited routes.
type RouteStore interface {
	LoadRoutes(ctx context.Context) ([]Route, error)
	SaveRoute(ctx context.Context, route Route) error
	DeleteRoute(ctx context.Context, id string) error
}

// RouteInfo describes a route for listing.
type RouteInfo struct {
	Route
	// BuiltIn is true for the routes liltok ships with.
	BuiltIn bool
	// Customized is true when a saved edit replaces the built-in definition.
	Customized bool
}

// SetRouteStore loads saved routes over the built-in ones and persists later edits to store.
// Saved routes that no longer validate (for example, naming a provider that was removed) are
// skipped with a warning.
func (r *Router) SetRouteStore(ctx context.Context, store RouteStore) error {
	saved, err := store.LoadRoutes(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routeStore = store
	for _, route := range saved {
		if err := r.validateRouteLocked(route); err != nil {
			telemetry.Log.Warn().Str("route", route.ID).Err(err).Msg("Skipping saved route that no longer validates")
			continue
		}
		r.putRouteLocked(route)
	}
	return nil
}

// Routes lists every route: built-in routes first in their fixed order, then custom routes by ID.
func (r *Router) Routes() []RouteInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]RouteInfo, 0, len(r.routes))
	for _, id := range builtInRouteOrder {
		if route, ok := r.routes[id]; ok {
			out = append(out, RouteInfo{Route: copyRoute(route), BuiltIn: true, Customized: !routesEqual(route, r.builtInRoutes[id])})
		}
	}
	var custom []string
	for id := range r.routes {
		if _, builtIn := r.builtInRoutes[id]; !builtIn {
			custom = append(custom, id)
		}
	}
	sort.Strings(custom)
	for _, id := range custom {
		out = append(out, RouteInfo{Route: copyRoute(r.routes[id])})
	}
	return out
}

// UpsertRoute validates a route, persists it, and makes it live. Strategy defaults to fallback.
func (r *Router) UpsertRoute(ctx context.Context, route Route) error {
	route.ID = strings.TrimSpace(route.ID)
	route.Strategy = strings.TrimSpace(route.Strategy)
	if route.Strategy == "" {
		route.Strategy = StrategyFallback
	}
	for i := range route.Targets {
		route.Targets[i].ProviderName = strings.ToLower(strings.TrimSpace(route.Targets[i].ProviderName))
		route.Targets[i].UpstreamModel = strings.TrimSpace(route.Targets[i].UpstreamModel)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateRouteLocked(route); err != nil {
		return err
	}
	if r.routeStore != nil {
		if err := r.routeStore.SaveRoute(ctx, route); err != nil {
			return fmt.Errorf("save route %s: %w", route.ID, err)
		}
	}
	r.putRouteLocked(route)
	return nil
}

// ResetRoute removes a saved edit. A built-in route returns to its shipped definition and a custom
// route is deleted. It reports false when no route has that ID.
func (r *Router) ResetRoute(ctx context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	builtIn, isBuiltIn := r.builtInRoutes[id]
	if _, exists := r.routes[id]; !exists && !isBuiltIn {
		return false, nil
	}
	if r.routeStore != nil {
		if err := r.routeStore.DeleteRoute(ctx, id); err != nil {
			return true, fmt.Errorf("delete route %s: %w", id, err)
		}
	}
	if isBuiltIn {
		r.routes[id] = copyRoute(builtIn)
	} else {
		delete(r.routes, id)
	}
	r.roundRobin.Delete(id)
	return true, nil
}

func (r *Router) validateRouteLocked(route Route) error {
	if !routeIDRegex.MatchString(route.ID) {
		return fmt.Errorf("route id %q must be 1-64 lowercase letters, digits, '.', '_' or '-', starting with a letter or digit", route.ID)
	}
	if !IsKnownStrategy(route.Strategy) {
		return fmt.Errorf("unknown strategy %q (use one of %s)", route.Strategy, strings.Join(KnownStrategies, ", "))
	}
	if len(route.Targets) == 0 {
		return fmt.Errorf("route %s needs at least one target", route.ID)
	}
	if len(route.Targets) > maxRouteTargets {
		return fmt.Errorf("route %s has %d targets; the limit is %d", route.ID, len(route.Targets), maxRouteTargets)
	}
	for i, t := range route.Targets {
		if _, ok := r.providers[t.ProviderName]; !ok {
			return fmt.Errorf("target %d: unknown provider %q", i+1, t.ProviderName)
		}
		if t.UpstreamModel == "" {
			return fmt.Errorf("target %d: model is required", i+1)
		}
	}
	return nil
}

// putRouteLocked makes route live and creates circuit breakers for its targets.
func (r *Router) putRouteLocked(route Route) {
	route = copyRoute(route)
	r.routes[route.ID] = route
	r.roundRobin.Delete(route.ID)
	for _, t := range route.Targets {
		key := t.ProviderName + "/" + t.UpstreamModel
		if _, ok := r.breakers[key]; !ok {
			r.breakers[key] = NewCircuitBreaker(key)
		}
	}
}

func copyRoute(route Route) Route {
	route.Targets = append([]TargetSpec(nil), route.Targets...)
	return route
}

func routesEqual(a, b Route) bool {
	if a.Strategy != b.Strategy || a.Description != b.Description || len(a.Targets) != len(b.Targets) {
		return false
	}
	for i := range a.Targets {
		if a.Targets[i] != b.Targets[i] {
			return false
		}
	}
	return true
}

// SQLRouteStore persists routes in the provider_routes and route_targets tables.
type SQLRouteStore struct {
	db *sql.DB
}

// NewSQLRouteStore returns a store backed by db.
func NewSQLRouteStore(db *sql.DB) *SQLRouteStore {
	return &SQLRouteStore{db: db}
}

func (s *SQLRouteStore) LoadRoutes(ctx context.Context) ([]Route, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.strategy, COALESCE(r.description, ''), t.provider, t.upstream_model
		FROM provider_routes r
		JOIN route_targets t ON t.route_id = r.id
		WHERE r.is_active = 1
		ORDER BY r.id, t.priority, t.id`)
	if err != nil {
		return nil, fmt.Errorf("load routes: %w", err)
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		var id, strategy, description, providerName, model string
		if err := rows.Scan(&id, &strategy, &description, &providerName, &model); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != id {
			out = append(out, Route{ID: id, Strategy: strategy, Description: description})
		}
		last := &out[len(out)-1]
		last.Targets = append(last.Targets, TargetSpec{ProviderName: providerName, UpstreamModel: model})
	}
	return out, rows.Err()
}

func (s *SQLRouteStore) SaveRoute(ctx context.Context, route Route) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_routes (id, strategy, description, is_active) VALUES (?, ?, ?, 1)
		ON CONFLICT (id) DO UPDATE SET strategy = excluded.strategy, description = excluded.description, is_active = 1`,
		route.ID, route.Strategy, route.Description); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM route_targets WHERE route_id = ?`, route.ID); err != nil {
		return err
	}
	for i, t := range route.Targets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO route_targets (route_id, priority, provider, upstream_model) VALUES (?, ?, ?, ?)`,
			route.ID, i+1, t.ProviderName, t.UpstreamModel); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLRouteStore) DeleteRoute(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM route_targets WHERE route_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM provider_routes WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
