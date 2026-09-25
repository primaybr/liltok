package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Server.Port != 8080 {
		t.Errorf("expected default port 8080, got %d", cfg.Server.Port)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("expected default host 127.0.0.1, got %s", cfg.Server.Host)
	}
	if !cfg.Storage.WALMode {
		t.Errorf("expected default WAL mode to be true")
	}
	if cfg.Cache.DefaultTTLSeconds != 604800 {
		t.Errorf("expected default TTL 604800, got %d", cfg.Cache.DefaultTTLSeconds)
	}
}

func TestLoadWithConfigFile(t *testing.T) {
	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "liltok.test.yaml")

	yamlContent := `
server:
  host: "0.0.0.0"
  port: 9090
storage:
  db_path: "./test.db"
cache:
  l1_max_entries: 5000
log:
  level: "debug"
`
	if err := os.WriteFile(configFile, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write test config file: %v", err)
	}

	cfg, err := Load(configFile)
	if err != nil {
		t.Fatalf("unexpected error loading config: %v", err)
	}

	if cfg.Server.Port != 9090 {
		t.Errorf("expected port 9090 from file, got %d", cfg.Server.Port)
	}
	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("expected host 0.0.0.0, got %s", cfg.Server.Host)
	}
	if cfg.Cache.L1MaxEntries != 5000 {
		t.Errorf("expected l1_max_entries 5000, got %d", cfg.Cache.L1MaxEntries)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("expected log level debug, got %s", cfg.Log.Level)
	}
	// Fallback to default check
	if cfg.Cache.DefaultTTLSeconds != 604800 {
		t.Errorf("expected default TTL to remain 604800, got %d", cfg.Cache.DefaultTTLSeconds)
	}
}

func TestLoadWithEnvOverrides(t *testing.T) {
	t.Setenv("LILTOK_PORT", "9999")
	t.Setenv("LILTOK_HOST", "192.168.1.50")
	t.Setenv("OPENAI_API_KEY", "sk-test-key-12345")
	t.Setenv("ANTHROPIC_API_KEY", "ant-test-key-67890")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-test-key")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("unexpected error loading config: %v", err)
	}

	if cfg.Server.Port != 9999 {
		t.Errorf("expected port 9999 from env, got %d", cfg.Server.Port)
	}
	if cfg.Server.Host != "192.168.1.50" {
		t.Errorf("expected host 192.168.1.50 from env, got %s", cfg.Server.Host)
	}
	if cfg.Providers.OpenAI.APIKey != "sk-test-key-12345" {
		t.Errorf("expected OpenAI key from env, got %s", cfg.Providers.OpenAI.APIKey)
	}
	if cfg.Providers.Anthropic.APIKey != "ant-test-key-67890" {
		t.Errorf("expected Anthropic key from env, got %s", cfg.Providers.Anthropic.APIKey)
	}
	if cfg.Providers.OpenRouter.APIKey != "sk-or-v1-test-key" {
		t.Errorf("expected OpenRouter key from env, got %s", cfg.Providers.OpenRouter.APIKey)
	}
}

func TestPersistProviders(t *testing.T) {
	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "liltok.yaml")

	initialYAML := `# test config
server:
  host: "127.0.0.1"
  port: 8080

providers:
  openai:
    api_key: ""
    base_url: "https://api.openai.com/v1"
  groq:
    api_key: ""
    base_url: "https://api.groq.com/openai/v1"
  gemini:
    api_key: ""
    base_url: "https://generativelanguage.googleapis.com"
  openrouter:
    api_key: ""
    base_url: "https://openrouter.ai/api/v1"
`
	if err := os.WriteFile(configFile, []byte(initialYAML), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	newProviders := ProvidersConfig{
		OpenAI:     ProviderCreds{APIKey: "sk-new-key", BaseURL: "https://api.openai.com/v1"},
		Groq:       ProviderCreds{APIKey: "gsk_test123", BaseURL: "https://api.groq.com/openai/v1"},
		Gemini:     ProviderCreds{APIKey: "gem-key-1,gem-key-2", BaseURL: "https://generativelanguage.googleapis.com"},
		OpenRouter: ProviderCreds{APIKey: "sk-or-v1-new-key", BaseURL: "https://openrouter.ai/api/v1"},
	}

	if err := PersistProviders(configFile, newProviders); err != nil {
		t.Fatalf("PersistProviders failed: %v", err)
	}

	reloaded, err := Load(configFile)
	if err != nil {
		t.Fatalf("failed to reload config: %v", err)
	}

	if reloaded.Providers.Groq.APIKey != "gsk_test123" {
		t.Errorf("expected Groq key 'gsk_test123', got %q", reloaded.Providers.Groq.APIKey)
	}
	if reloaded.Providers.Gemini.APIKey != "gem-key-1,gem-key-2" {
		t.Errorf("expected Gemini keys 'gem-key-1,gem-key-2', got %q", reloaded.Providers.Gemini.APIKey)
	}
	if reloaded.Providers.OpenRouter.APIKey != "sk-or-v1-new-key" {
		t.Errorf("expected OpenRouter key 'sk-or-v1-new-key', got %q", reloaded.Providers.OpenRouter.APIKey)
	}
	if reloaded.Providers.OpenAI.APIKey != "sk-new-key" {
		t.Errorf("expected OpenAI key 'sk-new-key', got %q", reloaded.Providers.OpenAI.APIKey)
	}
}

// Providers saved from the dashboard must survive a restart even when the config file
// predates them (no provider block, or a block without an api_key line).
func TestPersistProviders_AddsMissingProviderAndKeyLines(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "liltok.yaml")
	initialYAML := `# user config, keep this comment
providers:
  groq:
    api_key: "gsk_old" # inline note
    base_url: "https://api.groq.com/openai/v1"
  ollama:
    base_url: "http://localhost:11434"

routes:
  default_strategy: "free-first"
`
	if err := os.WriteFile(configFile, []byte(initialYAML), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	p := DefaultConfig().Providers
	p.Groq.APIKey = "gsk_new"
	p.Ollama.APIKey = "ollama-token"
	p.Cline.APIKey = "sk_cline_new"

	if err := PersistProviders(configFile, p); err != nil {
		t.Fatalf("PersistProviders failed: %v", err)
	}

	reloaded, err := Load(configFile)
	if err != nil {
		t.Fatalf("failed to reload config: %v", err)
	}
	if got := reloaded.Providers.Cline.APIKey; got != "sk_cline_new" {
		t.Errorf("expected Cline key added as a new provider block, got %q", got)
	}
	if got := reloaded.Providers.Ollama.APIKey; got != "ollama-token" {
		t.Errorf("expected api_key line added to existing ollama block, got %q", got)
	}
	if got := reloaded.Providers.Groq.APIKey; got != "gsk_new" {
		t.Errorf("expected Groq key updated, got %q", got)
	}
	if reloaded.Routes.DefaultStrategy != "free-first" {
		t.Errorf("other sections must be kept, default_strategy is %q", reloaded.Routes.DefaultStrategy)
	}

	raw, _ := os.ReadFile(configFile)
	if !strings.Contains(string(raw), "# user config, keep this comment") {
		t.Errorf("expected comments preserved, got:\n%s", raw)
	}
	if strings.Contains(string(raw), "openrouter:") {
		t.Errorf("providers with no key must not be added to the file, got:\n%s", raw)
	}
}

func TestPersistProviders_CreatesProvidersSection(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "liltok.yaml")
	if err := os.WriteFile(configFile, []byte("server:\n  port: 8080\n"), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}
	p := DefaultConfig().Providers
	p.Cline.APIKey = "sk_cline_new"
	if err := PersistProviders(configFile, p); err != nil {
		t.Fatalf("PersistProviders failed: %v", err)
	}
	reloaded, err := Load(configFile)
	if err != nil {
		t.Fatalf("failed to reload config: %v", err)
	}
	if reloaded.Providers.Cline.APIKey != "sk_cline_new" || reloaded.Server.Port != 8080 {
		t.Errorf("expected providers section created and server kept, got cline=%q port=%d", reloaded.Providers.Cline.APIKey, reloaded.Server.Port)
	}
}

func TestPersistProviders_CreatesMissingFile(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "nested", "liltok.yaml")
	p := DefaultConfig().Providers
	p.Cline.APIKey = "sk_cline_new"
	if err := PersistProviders(configFile, p); err != nil {
		t.Fatalf("PersistProviders failed on a missing file: %v", err)
	}
	reloaded, err := Load(configFile)
	if err != nil {
		t.Fatalf("failed to reload config: %v", err)
	}
	if reloaded.Providers.Cline.APIKey != "sk_cline_new" {
		t.Errorf("expected Cline key in the newly created file, got %q", reloaded.Providers.Cline.APIKey)
	}
}

// Configs written before routes.excluded_models existed keep the default exclusion; an explicit
// empty list clears it and the environment variable overrides both.
func TestExcludedModelsDefaultAndOverrides(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cfg, err := Load(write("old.yaml", "routes:\n  default_strategy: \"free-first\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes.ExcludedModels) != 1 || cfg.Routes.ExcludedModels[0] != "gemini-3.5-flash-lite" {
		t.Errorf("config without excluded_models should keep the default, got %v", cfg.Routes.ExcludedModels)
	}

	cfg, err = Load(write("cleared.yaml", "routes:\n  excluded_models: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes.ExcludedModels) != 0 {
		t.Errorf("explicit empty list should clear exclusions, got %v", cfg.Routes.ExcludedModels)
	}

	t.Setenv("LILTOK_EXCLUDED_MODELS", "a-model, google/b-model:free ,")
	cfg, err = Load(write("env.yaml", "routes:\n  excluded_models: [\"x\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes.ExcludedModels) != 2 || cfg.Routes.ExcludedModels[1] != "google/b-model:free" {
		t.Errorf("env override = %v, want [a-model google/b-model:free]", cfg.Routes.ExcludedModels)
	}
}
