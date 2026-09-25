package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/primaybr/liltok/internal/admin"
	"github.com/primaybr/liltok/internal/cache/exact"
	"github.com/primaybr/liltok/internal/cache/semantic"
	cachesync "github.com/primaybr/liltok/internal/cache/sync"
	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/ledger"
	"github.com/primaybr/liltok/internal/router"
	"github.com/primaybr/liltok/internal/server"
	"github.com/primaybr/liltok/internal/telemetry"
	"github.com/primaybr/liltok/internal/tokens"
	"github.com/spf13/cobra"
)

var (
	version = "0.2.3-beta"
	commit  = "none"
	date    = "unknown"

	configPath string
	portFlag   int
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCommand builds the full liltok command tree. It is separate from main so tests can
// drive every subcommand through cobra without exiting the process.
func newRootCommand() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "liltok",
		Short: "liltok: The Open-Source Local AI Gateway & Router",
		Long: `liltok is an ultra-low-latency, local-first AI Gateway and Router engineered 
to slash token consumption and eliminate SaaS gateway fees through intelligent multi-tier caching 
and resilient routing.`,
	}

	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "Path to configuration file (default is ~/.liltok/liltok.yaml)")

	// start command
	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the liltok AI Gateway and Router",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve config path
			cfgPath := resolveConfigPath(configPath)
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("error loading configuration: %w", err)
			}

			if portFlag > 0 {
				cfg.Server.Port = portFlag
			}

			telemetry.InitGlobalLogger(cfg.Log.Level, cfg.Log.Format)
			log := telemetry.Log

			log.Info().
				Str("version", version).
				Str("host", cfg.Server.Host).
				Int("port", cfg.Server.Port).
				Str("db_path", cfg.Storage.DBPath).
				Msg("Starting liltok Gateway")

			fmt.Println("==================================================================")
			fmt.Printf(" liltok Gateway v%s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
			fmt.Printf(" Listening on http://%s:%d\n", cfg.Server.Host, cfg.Server.Port)
			fmt.Printf(" Database: %s\n", cfg.Storage.DBPath)
			fmt.Println(" Little Token, Big Savings.")
			fmt.Println("==================================================================")

			// Initialize SQLite Database
			database, err := db.Open(cfg.Storage.DBPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", cfg.Storage.DBPath, err)
			}
			defer database.Close()

			// Initialize Tier-1 Exact Match Cache
			cacheStore, err := exact.NewTieredStore(database, cfg.Cache.L1MaxEntries)
			if err != nil {
				return fmt.Errorf("failed to initialize cache store: %w", err)
			}
			defer func() {
				if err := cacheStore.Close(); err != nil {
					log.Warn().Err(err).Msg("Failed to close cache store cleanly")
				}
			}()

			// Initialize Tier-3 Semantic Similarity Cache
			var semCache *semantic.SemanticCache
			if cfg.Cache.SemanticCacheEnabled {
				semCache = semantic.NewSemanticCache(database, semantic.NewFastLocalEmbedder(256), float32(cfg.Cache.SemanticThreshold))
				log.Info().
					Float64("threshold", cfg.Cache.SemanticThreshold).
					Msg("Tier-3 Semantic Similarity Cache enabled")
			}

			// Initialize Router with Fallback Chains and Circuit Breakers
			rtr := router.NewRouter(cfg)
			// Persist model health so models that answered "not found" stay skipped across restarts.
			if err := rtr.Catalog().SetStore(context.Background(), router.NewSQLCatalogStore(database.DB)); err != nil {
				log.Warn().Err(err).Msg("Failed to load model catalog; model health will not persist")
			}
			// Load routes edited through the admin API over the built-in ones.
			if err := rtr.SetRouteStore(context.Background(), router.NewSQLRouteStore(database.DB)); err != nil {
				log.Warn().Err(err).Msg("Failed to load saved routes; route edits will not persist")
			}

			// Initialize Virtual Key Manager, Quota Enforcer, and Persistent Financial Ledger
			km := ledger.NewKeyManager(database)
			qe := ledger.NewQuotaEnforcer()
			led := ledger.NewLedger(database, km)
			defer func() {
				if err := led.Close(); err != nil {
					log.Warn().Err(err).Msg("Failed to flush request ledger on shutdown")
				}
			}()
			pricing := tokens.NewPricingRegistry(database)
			// least_cost routes order targets by input plus output price per million tokens.
			rtr.SetTargetCost(func(providerName, model string) float64 {
				return pricing.CalculateDetailedForRouting(model, providerName, 1_000_000, 1_000_000, 0, "MISS", "NONE").TotalCostUSD
			})

			// Initialize Real-time SSE Broadcaster
			broadcaster := admin.NewBroadcaster()

			// Instantiate and start HTTP Gateway Server
			srv := server.NewServer(server.ServerConfig{
				Config:        cfg,
				ConfigPath:    cfgPath,
				CacheStore:    cacheStore,
				SemanticCache: semCache,
				Router:        rtr,
				Ledger:        led,
				Pricing:       pricing,
				KeyManager:    km,
				QuotaEnforcer: qe,
				Database:      database,
				Broadcaster:   broadcaster,
			})

			// Initialize and start OTA Cache Syncer if enabled
			if cfg.Cache.AutoSync && cfg.Cache.SyncURL != "" {
				syncer := cachesync.NewCacheSyncer(database, cfg.Cache.SyncURL, nil)
				interval := time.Duration(cfg.Cache.SyncIntervalHours) * time.Hour
				if interval <= 0 {
					interval = 24 * time.Hour
				}
				syncer.StartBackgroundLoop(context.Background(), interval)
			}

			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

			errChan := make(chan error, 1)
			go func() {
				if err := srv.Start(); err != nil && err != http.ErrServerClosed {
					errChan <- err
				}
			}()

			select {
			case sig := <-sigChan:
				log.Info().Str("signal", sig.String()).Msg("Received termination signal, shutting down gracefully...")
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := srv.Shutdown(ctx); err != nil {
					log.Error().Err(err).Msg("Server graceful shutdown failed")
				}
				log.Info().Msg("Liltok Gateway shutdown cleanly")
				return nil
			case err := <-errChan:
				return fmt.Errorf("server fatal error: %w", err)
			}
		},
	}
	startCmd.Flags().IntVarP(&portFlag, "port", "p", 0, "Override server port")

	// init command
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize default configuration and liltok data directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("failed to detect user home directory: %w", err)
			}

			dir := filepath.Join(home, ".liltok")
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", dir, err)
			}

			targetConfig := filepath.Join(dir, "liltok.yaml")
			if _, err := os.Stat(targetConfig); err == nil {
				fmt.Printf("Configuration already exists at %s\n", targetConfig)
				return nil
			}

			defaultYaml := `# liltok Gateway Configuration
server:
  host: "127.0.0.1"
  port: 8080
  read_timeout_seconds: 60
  write_timeout_seconds: 300

storage:
  db_path: "~/.liltok/liltok.db"
  wal_mode: true

cache:
  l1_max_entries: 10000
  default_ttl_seconds: 604800
  cache_nonzero_temperature: false
  semantic_cache_enabled: false
  semantic_threshold: 0.95
  prune_diffs: true

log:
  level: "info"
  format: "console"

providers:
  openai:
    api_key: ""
    base_url: "https://api.openai.com/v1"
  anthropic:
    api_key: ""
    base_url: "https://api.anthropic.com"
  nvidianim:
    api_key: ""
    base_url: "https://integrate.api.nvidia.com/v1"
  groq:
    api_key: ""
    base_url: "https://api.groq.com/openai/v1"
  gemini:
    api_key: ""
    base_url: "https://generativelanguage.googleapis.com"
  openrouter:
    api_key: ""
    base_url: "https://openrouter.ai/api/v1"
  ollama:
    base_url: "http://localhost:11434"

routes:
  default_strategy: "auto-resilient"
`
			if err := os.WriteFile(targetConfig, []byte(defaultYaml), 0644); err != nil {
				return fmt.Errorf("failed to write config to %s: %w", targetConfig, err)
			}

			fmt.Printf("Successfully initialized liltok workspace at %s\n", dir)
			fmt.Printf("Created default config at %s\n", targetConfig)
			return nil
		},
	}

	// version command
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print liltok version and runtime information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("liltok version %s\n", version)
			fmt.Printf("  commit:    %s\n", commit)
			fmt.Printf("  built at:  %s\n", date)
			fmt.Printf("  go version: %s\n", runtime.Version())
			fmt.Printf("  platform:  %s/%s\n", runtime.GOOS, runtime.GOARCH)
		},
	}

	rootCmd.AddCommand(startCmd, initCmd, versionCmd, newKeysCommand(), newCacheCommand(), newMineCommand(), newMCPCommand(), newMaintainerCommand(), newRouteCommand())
	return rootCmd
}

// resolveConfigPath finds the active configuration file:
// 1. Explicit path passed by flag (--config / -c)
// 2. User home directory: ~/.liltok/liltok.yaml
// 3. Current working directory: ./liltok.yaml
// 4. Configs directory: ./configs/liltok.yaml
func resolveConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	home, err := os.UserHomeDir()
	if err == nil {
		candidate := filepath.Join(home, ".liltok", "liltok.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if _, err := os.Stat("liltok.yaml"); err == nil {
		return "liltok.yaml"
	}
	if _, err := os.Stat(filepath.Join("configs", "liltok.yaml")); err == nil {
		return filepath.Join("configs", "liltok.yaml")
	}
	return ""
}
