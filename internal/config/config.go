package config

import (
	"bytes"
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
	SessionCompactorEnabled bool    `yaml:"session_compactor_enabled"`
	RecentTurnsToKeep       int     `yaml:"recent_turns_to_keep"`
	CompactorHeadBytes      int     `yaml:"compactor_head_bytes"`
	CompactorTailBytes      int     `yaml:"compactor_tail_bytes"`
	CompactorMinSizeBytes   int     `yaml:"compactor_min_size_bytes"`
	ProtectCodeFiles        bool    `yaml:"protect_code_files"`
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
	Gemini     ProviderCreds `yaml:"gemini"`
	OpenRouter ProviderCreds `yaml:"openrouter"`
	Ollama     ProviderCreds `yaml:"ollama"`
	Kilo       ProviderCreds `yaml:"kilo"`
	Cline      ProviderCreds `yaml:"cline"`
}

// RouteConfig specifies default routing strategies and fallback options.
type RouteConfig struct {
	DefaultStrategy string `yaml:"default_strategy"` // "auto-resilient", "free-first", "premium-only"
	// AttemptTimeoutSeconds bounds each non-premium upstream attempt in a fallback chain,
	// so a provider that accepts a request and never answers cannot stall the chain. 0 disables it.
	AttemptTimeoutSeconds int `yaml:"attempt_timeout_seconds"`
	// CaptureDir, when set, receives a replay fixture for every routed /v1/messages request.
	// Fixtures hold the full conversation, so leave it empty outside debugging sessions.
	CaptureDir string `yaml:"capture_dir"`
	// ExcludedModels lists upstream models never used, from any provider. Entries match ignoring
	// case, a provider prefix ("google/") and a variant suffix (":free").
	ExcludedModels []string `yaml:"excluded_models"`
	// LastResortModels lists upstream models tried only after every other target in the chain has
	// failed or been skipped. Entries match like ExcludedModels.
	LastResortModels []string `yaml:"last_resort_models"`
}

// MaintainerConfig controls local moderation and encrypted cache curation settings.
type MaintainerConfig struct {
	Enabled        bool   `yaml:"enabled"`
	PrivateKeyFile string `yaml:"private_key_file"`
	PublicKey      string `yaml:"public_key"`
}

// Config represents the complete runtime configuration of Liltok.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Storage    StorageConfig    `yaml:"storage"`
	Cache      CacheConfig      `yaml:"cache"`
	Log        LogConfig        `yaml:"log"`
	Providers  ProvidersConfig  `yaml:"providers"`
	Routes     RouteConfig      `yaml:"routes"`
	Maintainer MaintainerConfig `yaml:"maintainer"`
}

// Load loads configuration by cascading Defaults -> Config File (if exists) -> Environment Variables.
func Load(configPath string) (*Config, error) {
	cfg := DefaultConfig()

	// If a config path is specified or standard default path exists, load it
	if configPath != "" {
		expandedPath := ExpandHomeDir(configPath)
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

	// Expand home directory in DBPath and Maintainer Key
	cfg.Storage.DBPath = ExpandHomeDir(cfg.Storage.DBPath)
	cfg.Maintainer.PrivateKeyFile = ExpandHomeDir(cfg.Maintainer.PrivateKeyFile)

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
	if v := os.Getenv("OPENROUTER_API_KEY"); v != "" {
		cfg.Providers.OpenRouter.APIKey = v
	}
	if v := os.Getenv("OPENROUTER_BASE_URL"); v != "" {
		cfg.Providers.OpenRouter.BaseURL = v
	}
	if v := os.Getenv("OLLAMA_BASE_URL"); v != "" {
		cfg.Providers.Ollama.BaseURL = v
	}
	if v := os.Getenv("KILO_API_KEY"); v != "" {
		cfg.Providers.Kilo.APIKey = v
	}
	if v := os.Getenv("KILO_BASE_URL"); v != "" {
		cfg.Providers.Kilo.BaseURL = v
	}
	if v := os.Getenv("CLINE_API_KEY"); v != "" {
		cfg.Providers.Cline.APIKey = v
	}
	if v := os.Getenv("CLINE_BASE_URL"); v != "" {
		cfg.Providers.Cline.BaseURL = v
	}
	if v := os.Getenv("LILTOK_SYNC_URL"); v != "" {
		cfg.Cache.SyncURL = v
	}
	if v := os.Getenv("LILTOK_AUTO_SYNC"); v != "" {
		cfg.Cache.AutoSync = strings.ToLower(v) == "true" || v == "1"
	}
	if v := os.Getenv("LILTOK_SESSION_COMPACTOR_ENABLED"); v != "" {
		cfg.Cache.SessionCompactorEnabled = strings.ToLower(v) == "true" || v == "1"
	}
	if v := os.Getenv("LILTOK_RECENT_TURNS_TO_KEEP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Cache.RecentTurnsToKeep = n
		}
	}
	if v := os.Getenv("LILTOK_COMPACTOR_HEAD_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Cache.CompactorHeadBytes = n
		}
	}
	if v := os.Getenv("LILTOK_COMPACTOR_TAIL_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Cache.CompactorTailBytes = n
		}
	}
	if v := os.Getenv("LILTOK_COMPACTOR_MIN_SIZE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Cache.CompactorMinSizeBytes = n
		}
	}
	if v := os.Getenv("LILTOK_ATTEMPT_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Routes.AttemptTimeoutSeconds = n
		}
	}
	if v, ok := os.LookupEnv("LILTOK_EXCLUDED_MODELS"); ok {
		cfg.Routes.ExcludedModels = splitModelList(v)
	}
	if v, ok := os.LookupEnv("LILTOK_LAST_RESORT_MODELS"); ok {
		cfg.Routes.LastResortModels = splitModelList(v)
	}
	if v := os.Getenv("LILTOK_CAPTURE_DIR"); v != "" {
		cfg.Routes.CaptureDir = v
	}
	if v := os.Getenv("LILTOK_PROTECT_CODE_FILES"); v != "" {
		cfg.Cache.ProtectCodeFiles = strings.ToLower(v) == "true" || v == "1"
	}
	if v := os.Getenv("LILTOK_MAINTAINER_MODE"); v != "" {
		cfg.Maintainer.Enabled = strings.ToLower(v) == "true" || v == "1"
	}
	if v := os.Getenv("LILTOK_MAINTAINER_KEY_FILE"); v != "" {
		cfg.Maintainer.PrivateKeyFile = v
	}
	if v := os.Getenv("LILTOK_MAINTAINER_PUBKEY"); v != "" {
		cfg.Maintainer.PublicKey = v
	}
}

// ExpandHomeDir resolves ~ to user home directory.
func ExpandHomeDir(path string) string {
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

// PersistDefaultStrategy updates default_strategy in the YAML config file while preserving comments.
func PersistDefaultStrategy(configPath, strategy string) error {
	if configPath == "" {
		configPath = "~/.liltok/liltok.yaml"
	}
	expanded := ExpandHomeDir(configPath)
	data, err := os.ReadFile(expanded)
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	found := false
	inRoutes := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "routes:" {
			inRoutes = true
			continue
		}
		if inRoutes && strings.HasPrefix(trimmed, "default_strategy:") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = fmt.Sprintf("%sdefault_strategy: %q", indent, strategy)
			found = true
			break
		}
		if inRoutes && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && trimmed != "" {
			inRoutes = false
		}
	}

	if !found {
		lines = append(lines, fmt.Sprintf("routes:\n  default_strategy: %q", strategy))
	}

	return os.WriteFile(expanded, []byte(strings.Join(lines, "\n")), 0644)
}

// providerEntry pairs a provider's config key with its credentials.
type providerEntry struct {
	Name  string
	Creds ProviderCreds
}

// providerEntries lists every configured provider in config-file order.
func providerEntries(p ProvidersConfig) []providerEntry {
	return []providerEntry{
		{"openai", p.OpenAI}, {"anthropic", p.Anthropic}, {"nvidianim", p.NVIDIANIM},
		{"groq", p.Groq}, {"gemini", p.Gemini}, {"openrouter", p.OpenRouter},
		{"ollama", p.Ollama}, {"kilo", p.Kilo}, {"cline", p.Cline},
	}
}

// mappingValue returns the mapping stored under key in m. With create set, a missing key, or a key
// with an empty value ("providers:" and nothing under it), is given a new empty mapping.
func mappingValue(m *yaml.Node, key string, create bool) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		v := m.Content[i+1]
		if v.Kind != yaml.MappingNode {
			if !create {
				return nil
			}
			*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
		return v
	}
	if !create {
		return nil
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

// setScalar sets key to value in mapping m, keeping the existing quoting and comments when the key is
// already present. A missing key is only added when value is non-empty.
func setScalar(m *yaml.Node, key, value string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			v.Kind, v.Tag, v.Value = yaml.ScalarNode, "!!str", value
			if v.Style == 0 {
				v.Style = yaml.DoubleQuotedStyle
			}
			return
		}
	}
	if value == "" {
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle})
}

// PersistProviders writes provider credentials into the YAML config file, preserving comments and all
// other settings. Providers already in the file are updated. A provider missing from the file is added
// only when it has an API key, so a key saved from the dashboard survives a restart even when the file
// predates that provider.
func PersistProviders(configPath string, p ProvidersConfig) error {
	if configPath == "" {
		configPath = "~/.liltok/liltok.yaml"
	}
	expanded := ExpandHomeDir(configPath)
	data, err := os.ReadFile(expanded)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(expanded), 0700); err != nil {
			return err
		}
		data, err = nil, nil
	}
	if err != nil {
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("failed to parse %s: %w", configPath, err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: top level is not a mapping", configPath)
	}

	providers := mappingValue(root, "providers", true)
	for _, e := range providerEntries(p) {
		block := mappingValue(providers, e.Name, false)
		if block == nil {
			if e.Creds.APIKey == "" {
				continue
			}
			block = mappingValue(providers, e.Name, true)
		}
		setScalar(block, "api_key", e.Creds.APIKey)
		setScalar(block, "base_url", e.Creds.BaseURL)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("failed to encode %s: %w", configPath, err)
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(expanded, buf.Bytes(), 0600)
}

// splitModelList parses a comma-separated list of model IDs, dropping blanks.
func splitModelList(v string) []string {
	var out []string
	for _, m := range strings.Split(v, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}
