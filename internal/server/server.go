package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/primaybr/liltok/internal/admin"
	"github.com/primaybr/liltok/internal/cache"
	"github.com/primaybr/liltok/internal/cache/semantic"
	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/ledger"
	"github.com/primaybr/liltok/internal/metrics"
	"github.com/primaybr/liltok/internal/proxy"
	"github.com/primaybr/liltok/internal/router"
	"github.com/primaybr/liltok/internal/server/middleware"
	"github.com/primaybr/liltok/internal/telemetry"
	"github.com/primaybr/liltok/internal/tokens"
)

// ServerConfig bundles dependencies required to launch the Gateway server.
type ServerConfig struct {
	Config        *config.Config
	ConfigPath    string
	CacheStore    cache.Store
	SemanticCache *semantic.SemanticCache
	Router        *router.Router
	Ledger        *ledger.Ledger
	Pricing       *tokens.PricingRegistry
	KeyManager    *ledger.KeyManager
	QuotaEnforcer *ledger.QuotaEnforcer
	Database      *db.DB
	Broadcaster   *admin.Broadcaster
}

// Server represents the Liltok HTTP gateway server.
type Server struct {
	cfg         *config.Config
	router      chi.Router
	httpServer  *http.Server
	proxy       *proxy.Proxy
	admin       *admin.AdminHandler
	prom        *metrics.PrometheusExporter
	broadcaster *admin.Broadcaster
}

// NewServer initializes the HTTP router and registers middlewares and routes.
func NewServer(sc ServerConfig) *Server {
	r := chi.NewRouter()

	// Apply Core Middlewares. LocalGuard runs first so rebinding and cross-site requests are
	// refused before any handler, logging or quota work.
	bindHost := ""
	if sc.Config != nil {
		bindHost = sc.Config.Server.Host
	}
	r.Use(middleware.LocalGuard(bindHost))
	adminToken := ""
	if sc.Config != nil {
		adminToken = sc.Config.Server.AdminToken
	}
	r.Use(middleware.AdminToken(adminToken))
	r.Use(middleware.RequestID)
	r.Use(middleware.Recovery)
	r.Use(middleware.Logger)
	r.Use(middleware.NewAuth(sc.KeyManager, sc.QuotaEnforcer))

	p := proxy.NewProxy(sc.Config, sc.CacheStore, sc.SemanticCache, sc.Router, sc.Ledger, sc.Pricing)
	if sc.Broadcaster != nil {
		p.SetBroadcaster(sc.Broadcaster)
	}

	var adminH *admin.AdminHandler
	if sc.Config != nil {
		adminH = admin.NewAdminHandler(sc.Config, sc.Database, sc.Ledger, sc.KeyManager, sc.Router, sc.CacheStore, sc.Broadcaster)
		if sc.ConfigPath != "" {
			adminH.SetConfigPath(sc.ConfigPath)
		}
	}
	promExp := metrics.NewPrometheusExporter(sc.Database, sc.Router)

	s := &Server{
		cfg:         sc.Config,
		router:      r,
		proxy:       p,
		admin:       adminH,
		prom:        promExp,
		broadcaster: sc.Broadcaster,
	}

	s.registerRoutes()
	return s
}

// registerRoutes configures the ingress routes for OpenAI, Anthropic, Admin dashboard, and Prometheus.
func (s *Server) registerRoutes() {
	// Liveness & health probe
	s.router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy","service":"liltok"}`))
	})
	s.router.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy","service":"liltok"}`))
	})

	s.router.Get("/api/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	s.router.Head("/api/hello", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Prometheus Metrics Endpoint
	if s.prom != nil {
		s.router.Get("/metrics", s.prom.ServeHTTP)
	}

	// Developer Dashboard SPA & Static Assets
	s.router.Get("/dashboard", admin.ServeDashboard)
	s.router.Get("/dashboard/*", admin.ServeDashboard)
	s.router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusTemporaryRedirect)
	})

	// Admin REST API and SSE Telemetry
	if s.admin != nil {
		s.admin.RegisterRoutes(s.router)
	}

	// OpenAI API Ingress Routes
	s.router.Route("/v1", func(r chi.Router) {
		r.Post("/chat/completions", s.proxy.HandleChatCompletions)
		r.Post("/completions", s.proxy.HandleCompletions)
		r.Post("/embeddings", s.proxy.HandleEmbeddings)
		r.Get("/models", s.proxy.HandleModels)

		// Anthropic Messages API Ingress Route (Claude Code in VS Code)
		r.Post("/messages", s.proxy.HandleAnthropicMessages)
	})
}

// Router returns the underlying Chi router for testing.
func (s *Server) Router() http.Handler {
	return s.router
}

// Start runs the HTTP server listening on the configured host and port.
func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      s.router,
		ReadTimeout:  time.Duration(s.cfg.Server.ReadTimeoutSeconds) * time.Second,
		WriteTimeout: time.Duration(s.cfg.Server.WriteTimeoutSeconds) * time.Second,
	}

	telemetry.Log.Info().
		Str("addr", addr).
		Msg("Liltok Gateway server listening")

	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}
