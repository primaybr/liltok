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

	"github.com/liltok/liltok/internal/admin"
	"github.com/liltok/liltok/internal/cache/exact"
	"github.com/liltok/liltok/internal/cache/semantic"
	cachesync "github.com/liltok/liltok/internal/cache/sync"
	"github.com/liltok/liltok/internal/config"
	"github.com/liltok/liltok/internal/db"
	"github.com/liltok/liltok/internal/ledger"
	"github.com/liltok/liltok/internal/router"
	"github.com/liltok/liltok/internal/server"
	"github.com/liltok/liltok/internal/telemetry"
	"github.com/liltok/liltok/internal/tokens"
	"github.com/spf13/cobra"
)

var (
	version = "0.1.0-dev"
	commit  = "none"
	date    = "unknown"

	configPath string
	portFlag   int
)

func main() {
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
			cfgPath := configPath
			if cfgPath == "" {
				home, err := os.UserHomeDir()
				if err == nil {
					candidate := filepath.Join(home, ".liltok", "liltok.yaml")
					if _, err := os.Stat(candidate); err == nil {
						cfgPath = candidate
					}
				}
			}

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
			defer cacheStore.Close()

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

			// Initialize Virtual Key Manager, Quota Enforcer, and Persistent Financial Ledger
			km := ledger.NewKeyManager(database)
			qe := ledger.NewQuotaEnforcer()
			led := ledger.NewLedger(database, km)
			defer led.Close()
			pricing := tokens.NewPricingRegistry(database)

			// Initialize Real-time SSE Broadcaster
			broadcaster := admin.NewBroadcaster()

			// Instantiate and start HTTP Gateway Server
			srv := server.NewServer(server.ServerConfig{
				Config:        cfg,
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

	rootCmd.AddCommand(startCmd, initCmd, versionCmd, newKeysCommand(), newCacheCommand(), newMineCommand())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
