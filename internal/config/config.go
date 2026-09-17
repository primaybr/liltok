package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServerConfig holds HTTP server network and lifecycle settings.
type ServerConfig struct {
	Host                string `yaml:"host"`
	Port                int    `yaml:"port"`
	ReadTimeoutSeconds  int    `yaml:"read_timeout_seconds"`
	WriteTimeoutSeconds int    `yaml:"write_timeout_seconds"`
}

// StorageConfig holds persistent SQLite database configurations.
type StorageConfig struct {
	DBPath  string `yaml:"db_path"`
	WALMode bool   `yaml:"wal_mode"`
}

// CacheConfig holds multi-tier caching policies and thresholds.
type CacheConfig struct {
	L1MaxEntries            int     `yaml:"l1_max_entries"`
	DefaultTTLSeconds       int     `yaml:"default_ttl_seconds"`
	CacheNonzeroTemperature bool    `yaml:"cache_nonzero_temperature"`
	SemanticCacheEnabled    bool    `yaml:"semantic_cache_enabled"`
	SemanticThreshold       float64 `yaml:"semantic_threshold"`
	PruneDiffs              bool    `yaml:"prune_diffs"`
	AutoSync                bool    `yaml:"auto_sync"`
	SyncURL                 string  `yaml:"sync_url"`
	SyncIntervalHours       int     `yaml:"sync_interval_hours"`
}

// LogConfig holds structured logging configuration.
type LogConfig struct {
	Level  string `yaml:"level"`  // "trace", "debug", "info", "warn", "error"
	Format string `yaml:"format"` // "console", "json"
}

// ProviderCreds holds connection details for upstream LLM providers.
type ProviderCreds struct {
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
}

// ProvidersConfig aggregates upstream provider configurations.
type ProvidersConfig struct {
	OpenAI    ProviderCreds `yaml:"openai"`
	Anthropic ProviderCreds `yaml:"anthropic"`
	NVIDIANIM ProviderCreds `yaml:"nvidianim"`
	Groq      ProviderCreds `yaml:"groq"`
	Gemini    ProviderCreds `yaml:"gemini"`
	Ollama    ProviderCreds `yaml:"ollama"`
}

// RouteConfig specifies default routing strategies and fallback options.
type RouteConfig struct {
	DefaultStrategy string `yaml:"default_strategy"` // "auto-resilient", "free-first", "premium-only"
}

// Config represents the complete runtime configuration of Liltok.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Storage   StorageConfig   `yaml:"storage"`
	Cache     CacheConfig     `yaml:"cache"`
	Log       LogConfig       `yaml:"log"`
	Providers ProvidersConfig `yaml:"providers"`
	Routes    RouteConfig     `yaml:"routes"`
}

// Load loads configuration by cascading Defaults -> Config File (if exists) -> Environment Variables.
func Load(configPath string) (*Config, error) {
	cfg := DefaultConfig()

	// If a config path is specified or standard default path exists, load it
	if configPath != "" {
		expandedPath := expandHomeDir(configPath)
		data, err := os.ReadFile(expandedPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file %q: %w", configPath, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("failed to parse yaml config %q: %w", configPath, err)
		}
	}

	// Apply environment variable overrides
	applyEnvOverrides(cfg)

	// Expand home directory in DBPath
	cfg.Storage.DBPath = expandHomeDir(cfg.Storage.DBPath)

	return cfg, nil
}

// applyEnvOverrides allows environment variables to take highest precedence.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("LILTOK_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("LILTOK_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = p
		}
	}
	if v := os.Getenv("LILTOK_DB_PATH"); v != "" {
		cfg.Storage.DBPath = v
	}
	if v := os.Getenv("LILTOK_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("LILTOK_LOG_FORMAT"); v != "" {
		cfg.Log.Format = v
	}

	// Upstream API Keys & URLs
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		cfg.Providers.OpenAI.APIKey = v
	}
	if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
		cfg.Providers.OpenAI.BaseURL = v
	}
	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		cfg.Providers.Anthropic.APIKey = v
	}
	if v := os.Getenv("ANTHROPIC_BASE_URL"); v != "" {
		cfg.Providers.Anthropic.BaseURL = v
	}
	if v := os.Getenv("NVIDIA_API_KEY"); v != "" {
		cfg.Providers.NVIDIANIM.APIKey = v
	}
	if v := os.Getenv("GROQ_API_KEY"); v != "" {
		cfg.Providers.Groq.APIKey = v
	}
	if v := os.Getenv("GEMINI_API_KEY"); v != "" {
		cfg.Providers.Gemini.APIKey = v
	}
	if v := os.Getenv("OLLAMA_BASE_URL"); v != "" {
		cfg.Providers.Ollama.BaseURL = v
	}
	if v := os.Getenv("LILTOK_SYNC_URL"); v != "" {
		cfg.Cache.SyncURL = v
	}
	if v := os.Getenv("LILTOK_AUTO_SYNC"); v != "" {
		cfg.Cache.AutoSync = strings.ToLower(v) == "true" || v == "1"
	}
}

func expandHomeDir(path string) string {
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") || path == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
