package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// setTempHome points the user home directory at a fresh temp dir so tests never touch the
// developer's real ~/.liltok.
func setTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func TestApplyEnvOverrides_AllVariables(t *testing.T) {
	env := map[string]string{
		"LILTOK_HOST":                      "0.0.0.0",
		"LILTOK_PORT":                      "7070",
		"LILTOK_DB_PATH":                   "custom.db",
		"LILTOK_LOG_LEVEL":                 "trace",
		"LILTOK_LOG_FORMAT":                "json",
		"OPENAI_API_KEY":                   "test-openai",
		"OPENAI_BASE_URL":                  "http://openai.test",
		"ANTHROPIC_API_KEY":                "test-anthropic",
		"ANTHROPIC_BASE_URL":               "http://anthropic.test",
		"NVIDIA_API_KEY":                   "test-nvidia",
		"GROQ_API_KEY":                     "test-groq",
		"GEMINI_API_KEY":                   "test-gemini",
		"OPENROUTER_API_KEY":               "test-openrouter",
		"OPENROUTER_BASE_URL":              "http://openrouter.test",
		"OLLAMA_BASE_URL":                  "http://ollama.test",
		"KILO_API_KEY":                     "test-kilo",
		"KILO_BASE_URL":                    "http://kilo.test",
		"CLINE_API_KEY":                    "test-cline",
		"CLINE_BASE_URL":                   "http://cline.test",
		"LILTOK_SYNC_URL":                  "http://sync.test",
		"LILTOK_AUTO_SYNC":                 "TRUE",
		"LILTOK_SESSION_COMPACTOR_ENABLED": "1",
		"LILTOK_RECENT_TURNS_TO_KEEP":      "9",
		"LILTOK_COMPACTOR_HEAD_BYTES":      "111",
		"LILTOK_COMPACTOR_TAIL_BYTES":      "222",
		"LILTOK_COMPACTOR_MIN_SIZE_BYTES":  "333",
		"LILTOK_ATTEMPT_TIMEOUT_SECONDS":   "0",
		"LILTOK_MODEL_RECHECK_HOURS":       "48",
		"LILTOK_CAPTURE_DIR":               "captures",
		"LILTOK_PROTECT_CODE_FILES":        "true",
		"LILTOK_MAINTAINER_MODE":           "1",
		"LILTOK_MAINTAINER_KEY_FILE":       "maint.key",
		"LILTOK_MAINTAINER_PUBKEY":         "pubkey-value",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}

	cfg := DefaultConfig()
	applyEnvOverrides(cfg)

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"host", cfg.Server.Host, "0.0.0.0"},
		{"port", cfg.Server.Port, 7070},
		{"db path", cfg.Storage.DBPath, "custom.db"},
		{"log level", cfg.Log.Level, "trace"},
		{"log format", cfg.Log.Format, "json"},
		{"openai key", cfg.Providers.OpenAI.APIKey, "test-openai"},
		{"openai url", cfg.Providers.OpenAI.BaseURL, "http://openai.test"},
		{"anthropic key", cfg.Providers.Anthropic.APIKey, "test-anthropic"},
		{"anthropic url", cfg.Providers.Anthropic.BaseURL, "http://anthropic.test"},
		{"nvidia key", cfg.Providers.NVIDIANIM.APIKey, "test-nvidia"},
		{"groq key", cfg.Providers.Groq.APIKey, "test-groq"},
		{"gemini key", cfg.Providers.Gemini.APIKey, "test-gemini"},
		{"openrouter key", cfg.Providers.OpenRouter.APIKey, "test-openrouter"},
		{"openrouter url", cfg.Providers.OpenRouter.BaseURL, "http://openrouter.test"},
		{"ollama url", cfg.Providers.Ollama.BaseURL, "http://ollama.test"},
		{"kilo key", cfg.Providers.Kilo.APIKey, "test-kilo"},
		{"kilo url", cfg.Providers.Kilo.BaseURL, "http://kilo.test"},
		{"cline key", cfg.Providers.Cline.APIKey, "test-cline"},
		{"cline url", cfg.Providers.Cline.BaseURL, "http://cline.test"},
		{"sync url", cfg.Cache.SyncURL, "http://sync.test"},
		{"auto sync", cfg.Cache.AutoSync, true},
		{"compactor", cfg.Cache.SessionCompactorEnabled, true},
		{"recent turns", cfg.Cache.RecentTurnsToKeep, 9},
		{"head bytes", cfg.Cache.CompactorHeadBytes, 111},
		{"tail bytes", cfg.Cache.CompactorTailBytes, 222},
		{"min size", cfg.Cache.CompactorMinSizeBytes, 333},
		{"attempt timeout", cfg.Routes.AttemptTimeoutSeconds, 0},
		{"recheck hours", cfg.Routes.ModelRecheckHours, 48},
		{"capture dir", cfg.Routes.CaptureDir, "captures"},
		{"protect code", cfg.Cache.ProtectCodeFiles, true},
		{"maintainer", cfg.Maintainer.Enabled, true},
		{"maintainer key", cfg.Maintainer.PrivateKeyFile, "maint.key"},
		{"maintainer pubkey", cfg.Maintainer.PublicKey, "pubkey-value"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestApplyEnvOverrides_InvalidValuesKeepDefaults(t *testing.T) {
	t.Setenv("LILTOK_PORT", "not-a-port")
	t.Setenv("LILTOK_RECENT_TURNS_TO_KEEP", "x")
	t.Setenv("LILTOK_COMPACTOR_HEAD_BYTES", "x")
	t.Setenv("LILTOK_COMPACTOR_TAIL_BYTES", "x")
	t.Setenv("LILTOK_COMPACTOR_MIN_SIZE_BYTES", "x")
	t.Setenv("LILTOK_ATTEMPT_TIMEOUT_SECONDS", "-5")
	t.Setenv("LILTOK_MODEL_RECHECK_HOURS", "-1")
	t.Setenv("LILTOK_AUTO_SYNC", "yes")

	def := DefaultConfig()
	cfg := DefaultConfig()
	cfg.Cache.AutoSync = true
	applyEnvOverrides(cfg)

	if cfg.Server.Port != def.Server.Port {
		t.Errorf("invalid port must be ignored, got %d", cfg.Server.Port)
	}
	if cfg.Cache.RecentTurnsToKeep != def.Cache.RecentTurnsToKeep ||
		cfg.Cache.CompactorHeadBytes != def.Cache.CompactorHeadBytes ||
		cfg.Cache.CompactorTailBytes != def.Cache.CompactorTailBytes ||
		cfg.Cache.CompactorMinSizeBytes != def.Cache.CompactorMinSizeBytes {
		t.Errorf("non-numeric compactor values must be ignored, got %+v", cfg.Cache)
	}
	if cfg.Routes.AttemptTimeoutSeconds != def.Routes.AttemptTimeoutSeconds {
		t.Errorf("negative attempt timeout must be ignored, got %d", cfg.Routes.AttemptTimeoutSeconds)
	}
	if cfg.Routes.ModelRecheckHours != def.Routes.ModelRecheckHours {
		t.Errorf("negative recheck hours must be ignored, got %d", cfg.Routes.ModelRecheckHours)
	}
	if cfg.Cache.AutoSync {
		t.Errorf("LILTOK_AUTO_SYNC=yes is not a true value, auto sync must be off")
	}
}

func TestLoad_Errors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil || !strings.Contains(err.Error(), "failed to read config file") {
		t.Errorf("expected read error for a missing file, got %v", err)
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("server: [unclosed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "failed to parse yaml config") {
		t.Errorf("expected parse error for invalid yaml, got %v", err)
	}
}

func TestLoad_ExpandsHomeInPaths(t *testing.T) {
	home := setTempHome(t)
	file := filepath.Join(t.TempDir(), "liltok.yaml")
	body := "storage:\n  db_path: \"~/data/liltok.db\"\nmaintainer:\n  private_key_file: \"~/keys/m.key\"\n"
	if err := os.WriteFile(file, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "data/liltok.db"); cfg.Storage.DBPath != want {
		t.Errorf("db path = %q, want %q", cfg.Storage.DBPath, want)
	}
	if want := filepath.Join(home, "keys/m.key"); cfg.Maintainer.PrivateKeyFile != want {
		t.Errorf("key file = %q, want %q", cfg.Maintainer.PrivateKeyFile, want)
	}
}

func TestExpandHomeDir(t *testing.T) {
	home := setTempHome(t)
	tests := []struct {
		in, want string
	}{
		{"~", home},
		{"~/a/b", filepath.Join(home, "a/b")},
		{`~\a`, filepath.Join(home, "a")},
		{"relative/path", "relative/path"},
		{"~user/x", "~user/x"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ExpandHomeDir(tt.in); got != tt.want {
			t.Errorf("ExpandHomeDir(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPersistDefaultStrategy_ReplacesExistingKey(t *testing.T) {
	file := filepath.Join(t.TempDir(), "liltok.yaml")
	body := "# keep me\nserver:\n  port: 8080\nroutes:\n    default_strategy: \"auto-resilient\" # old\n  attempt_timeout_seconds: 30\n"
	if err := os.WriteFile(file, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if err := PersistDefaultStrategy(file, "free-first"); err != nil {
		t.Fatalf("PersistDefaultStrategy: %v", err)
	}
	raw, _ := os.ReadFile(file)
	got := string(raw)
	if !strings.Contains(got, "\n    default_strategy: \"free-first\"\n") {
		t.Errorf("expected the line replaced with its indent kept, got:\n%s", got)
	}
	if strings.Contains(got, "auto-resilient") {
		t.Errorf("old value must be gone, got:\n%s", got)
	}
	if !strings.Contains(got, "# keep me") || !strings.Contains(got, "attempt_timeout_seconds: 30") {
		t.Errorf("other content must be preserved, got:\n%s", got)
	}
}

func TestPersistDefaultStrategy_AppendsWhenNoRoutes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "liltok.yaml")
	// default_strategy under another section must not be touched.
	body := "server:\n  port: 8080\nother:\n  default_strategy: \"untouched\"\n"
	if err := os.WriteFile(file, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if err := PersistDefaultStrategy(file, "premium-only"); err != nil {
		t.Fatalf("PersistDefaultStrategy: %v", err)
	}
	cfg, err := Load(file)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Routes.DefaultStrategy != "premium-only" {
		t.Errorf("default strategy = %q, want premium-only", cfg.Routes.DefaultStrategy)
	}
	raw, _ := os.ReadFile(file)
	if !strings.Contains(string(raw), "default_strategy: \"untouched\"") {
		t.Errorf("default_strategy outside routes must be left alone, got:\n%s", raw)
	}
}

func TestPersistDefaultStrategy_RoutesWithoutKey(t *testing.T) {
	file := filepath.Join(t.TempDir(), "liltok.yaml")
	body := "routes:\n  attempt_timeout_seconds: 30\nserver:\n  port: 8080\n"
	if err := os.WriteFile(file, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if err := PersistDefaultStrategy(file, "free-first"); err != nil {
		t.Fatalf("PersistDefaultStrategy: %v", err)
	}
	cfg, err := Load(file)
	if err != nil {
		t.Fatalf("file must stay valid yaml after the update: %v", err)
	}
	if cfg.Routes.DefaultStrategy != "free-first" || cfg.Routes.AttemptTimeoutSeconds != 30 || cfg.Server.Port != 8080 {
		t.Errorf("got strategy=%q timeout=%d port=%d", cfg.Routes.DefaultStrategy, cfg.Routes.AttemptTimeoutSeconds, cfg.Server.Port)
	}
}

func TestPersistDefaultStrategy_MissingFile(t *testing.T) {
	if err := PersistDefaultStrategy(filepath.Join(t.TempDir(), "none.yaml"), "free-first"); !os.IsNotExist(err) {
		t.Errorf("expected not-exist error, got %v", err)
	}
}

func TestPersistDefaultStrategy_DefaultPathUsesHome(t *testing.T) {
	home := setTempHome(t)
	file := filepath.Join(home, ".liltok", "liltok.yaml")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("routes:\n  default_strategy: \"a\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := PersistDefaultStrategy("", "b"); err != nil {
		t.Fatalf("PersistDefaultStrategy: %v", err)
	}
	raw, _ := os.ReadFile(file)
	if !strings.Contains(string(raw), "default_strategy: \"b\"") {
		t.Errorf("expected file under temp home updated, got:\n%s", raw)
	}
}

func TestPersistProviders_DefaultPathUsesHome(t *testing.T) {
	home := setTempHome(t)
	p := ProvidersConfig{Groq: ProviderCreds{APIKey: "test-key"}}
	if err := PersistProviders("", p); err != nil {
		t.Fatalf("PersistProviders: %v", err)
	}
	cfg, err := Load(filepath.Join(home, ".liltok", "liltok.yaml"))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Providers.Groq.APIKey != "test-key" {
		t.Errorf("groq key = %q, want test-key", cfg.Providers.Groq.APIKey)
	}
}

func TestPersistProviders_Errors(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name, body, want string
	}{
		{"invalid yaml", "providers: [unclosed\n", "failed to parse"},
		{"top level sequence", "- a\n- b\n", "top level is not a mapping"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "_")+".yaml")
			if err := os.WriteFile(file, []byte(tt.body), 0644); err != nil {
				t.Fatal(err)
			}
			err := PersistProviders(file, ProvidersConfig{Groq: ProviderCreds{APIKey: "test-key"}})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %v, want error containing %q", err, tt.want)
			}
		})
	}

	// A directory in place of the file is a read error other than not-exist.
	if err := PersistProviders(dir, ProvidersConfig{}); err == nil {
		t.Errorf("expected an error when the config path is a directory")
	}
}

func TestPersistProviders_ScalarProvidersReplacedAndBlankKeyCleared(t *testing.T) {
	file := filepath.Join(t.TempDir(), "liltok.yaml")
	// "providers:" with a scalar value is replaced by a mapping; an existing key with a
	// single-quoted style keeps its style when rewritten.
	body := "providers: none\n"
	if err := os.WriteFile(file, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if err := PersistProviders(file, ProvidersConfig{Kilo: ProviderCreds{APIKey: "test-key"}}); err != nil {
		t.Fatalf("PersistProviders: %v", err)
	}
	cfg, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers.Kilo.APIKey != "test-key" {
		t.Errorf("kilo key = %q", cfg.Providers.Kilo.APIKey)
	}

	// Clearing the key keeps the api_key line but empties it.
	if err := PersistProviders(file, ProvidersConfig{}); err != nil {
		t.Fatalf("PersistProviders: %v", err)
	}
	cfg, err = Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers.Kilo.APIKey != "" {
		t.Errorf("kilo key should be cleared, got %q", cfg.Providers.Kilo.APIKey)
	}
}

func TestSetScalarKeepsExistingStyle(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("api_key: 'old'\nplain: x\n"), &doc); err != nil {
		t.Fatal(err)
	}
	m := doc.Content[0]
	setScalar(m, "api_key", "new")
	setScalar(m, "absent", "")
	if len(m.Content) != 4 {
		t.Fatalf("an empty value for a missing key must not add it, got %d nodes", len(m.Content))
	}
	if v := m.Content[1]; v.Value != "new" || v.Style != yaml.SingleQuotedStyle {
		t.Errorf("api_key = %q style %v, want new with single quotes kept", v.Value, v.Style)
	}

	if got := mappingValue(m, "plain", false); got != nil {
		t.Errorf("a scalar value must not be returned as a mapping without create")
	}
	setScalar(m, "plain", "y")
	if v := m.Content[3]; v.Value != "y" || v.Style != yaml.DoubleQuotedStyle {
		t.Errorf("plain = %q style %v, want y double quoted", v.Value, v.Style)
	}
	if got := mappingValue(m, "missing", false); got != nil {
		t.Errorf("a missing key must return nil without create")
	}
}

func TestCacheMaxPromptBytesDefaultAndEnv(t *testing.T) {
	if got := DefaultConfig().Cache.MaxPromptBytes; got != 256*1024 {
		t.Fatalf("default max_prompt_bytes = %d, want 262144", got)
	}
	t.Setenv("LILTOK_CACHE_MAX_PROMPT_BYTES", "0")
	cfg := DefaultConfig()
	applyEnvOverrides(cfg)
	if cfg.Cache.MaxPromptBytes != 0 {
		t.Fatalf("env override = %d, want 0 (no limit)", cfg.Cache.MaxPromptBytes)
	}
}
