package config

import (
	"os"
	"path/filepath"
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
}
