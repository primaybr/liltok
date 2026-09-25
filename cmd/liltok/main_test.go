package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/config"
)

func TestVersionCommand(t *testing.T) {
	out, err := runCLI(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	assertContains(t, out,
		"liltok version "+version,
		"commit:    "+commit,
		"built at:  "+date,
		"go version: "+runtime.Version(),
		"platform:  "+runtime.GOOS+"/"+runtime.GOARCH,
	)
}

func TestUnknownCommandFails(t *testing.T) {
	if _, err := runCLI(t, "definitely-not-a-command"); err == nil {
		t.Fatal("expected error for unknown command")
	}
}

func TestInitCommand(t *testing.T) {
	env := newTestEnv(t, "")
	target := filepath.Join(env.home, ".liltok", "liltok.yaml")

	out, err := runCLI(t, "init")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	assertContains(t, out, "Successfully initialized liltok workspace", "Created default config at "+target)

	// The generated file must be a valid config carrying the documented defaults.
	cfg, err := config.Load(target)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if cfg.Server.Port != 8080 || cfg.Server.Host != "127.0.0.1" {
		t.Errorf("server = %s:%d, want 127.0.0.1:8080", cfg.Server.Host, cfg.Server.Port)
	}
	if cfg.Routes.DefaultStrategy != "auto-resilient" {
		t.Errorf("default strategy = %q, want auto-resilient", cfg.Routes.DefaultStrategy)
	}
	if want := filepath.Join(env.home, ".liltok", "liltok.db"); filepath.Clean(cfg.Storage.DBPath) != want {
		t.Errorf("db path = %q, want %q", cfg.Storage.DBPath, want)
	}

	// A second init must not overwrite a config the user may have edited.
	if err := os.WriteFile(target, []byte("# user edited\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI(t, "init")
	if err != nil {
		t.Fatalf("second init: %v", err)
	}
	assertContains(t, out, "Configuration already exists at "+target)
	data, _ := os.ReadFile(target)
	if string(data) != "# user edited\n" {
		t.Errorf("init overwrote existing config: %q", data)
	}
}

func TestInitCommandDirectoryError(t *testing.T) {
	env := newTestEnv(t, "")
	// A regular file where the .liltok directory should go makes MkdirAll fail.
	if err := os.WriteFile(filepath.Join(env.home, ".liltok"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, "init")
	if err == nil || !strings.Contains(err.Error(), "failed to create directory") {
		t.Fatalf("err = %v, want directory creation failure", err)
	}
}

func TestResolveConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	work := t.TempDir()
	t.Chdir(work)

	if got := resolveConfigPath("explicit.yaml"); got != "explicit.yaml" {
		t.Errorf("explicit: got %q", got)
	}
	if got := resolveConfigPath(""); got != "" {
		t.Errorf("nothing present: got %q, want empty", got)
	}

	// configs/liltok.yaml is the lowest-priority candidate.
	if err := os.MkdirAll("configs", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("configs", "liltok.yaml"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if got := resolveConfigPath(""); got != filepath.Join("configs", "liltok.yaml") {
		t.Errorf("configs dir: got %q", got)
	}

	// ./liltok.yaml beats configs/liltok.yaml.
	if err := os.WriteFile("liltok.yaml", nil, 0644); err != nil {
		t.Fatal(err)
	}
	if got := resolveConfigPath(""); got != "liltok.yaml" {
		t.Errorf("cwd: got %q", got)
	}

	// ~/.liltok/liltok.yaml beats both.
	homeCfg := filepath.Join(home, ".liltok", "liltok.yaml")
	if err := os.MkdirAll(filepath.Dir(homeCfg), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(homeCfg, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if got := resolveConfigPath(""); got != homeCfg {
		t.Errorf("home: got %q, want %q", got, homeCfg)
	}
}

func TestResolveDBPath(t *testing.T) {
	env := newTestEnv(t, "")
	configPath = env.cfgPath
	t.Cleanup(func() { configPath = "" })
	if got := resolveDBPath(); got != env.dbPath {
		t.Errorf("resolveDBPath = %q, want %q", got, env.dbPath)
	}

	// An unreadable config falls back to the documented default path string.
	configPath = filepath.Join(env.home, "missing.yaml")
	if got := resolveDBPath(); got != "~/.liltok/liltok.db" {
		t.Errorf("fallback = %q", got)
	}
}

func TestStartCommandConfigError(t *testing.T) {
	env := newTestEnv(t, "")
	_, err := runCLI(t, "--config", filepath.Join(env.home, "missing.yaml"), "start")
	if err == nil || !strings.Contains(err.Error(), "error loading configuration") {
		t.Fatalf("err = %v, want configuration error", err)
	}
}

func TestStartCommandDatabaseError(t *testing.T) {
	env := newTestEnv(t, "")
	// Point db_path below a regular file so the database directory cannot be created.
	blocker := filepath.Join(env.home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := "storage:\n  db_path: '" + filepath.ToSlash(filepath.Join(blocker, "sub", "liltok.db")) + "'\nlog:\n  level: 'error'\n"
	if err := os.WriteFile(env.cfgPath, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := env.run(t, "start")
	if err == nil || !strings.Contains(err.Error(), "failed to open database") {
		t.Fatalf("err = %v, want database open failure", err)
	}
	assertContains(t, out, "liltok Gateway v"+version)
}

// TestStartCommandPortInUse runs the full start wiring (database, caches, router, ledger, server)
// and makes the listener fail by occupying the port first, so the command returns instead of
// blocking. It also checks that --port overrides the configured port.
func TestStartCommandPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	busyPort := ln.Addr().(*net.TCPAddr).Port

	env := newTestEnv(t, "server:\n  host: '127.0.0.1'\n  port: 1\ncache:\n  semantic_cache_enabled: true\n")

	out, err := env.run(t, "start", "--port", fmt.Sprint(busyPort))
	if err == nil || !strings.Contains(err.Error(), "server fatal error") {
		t.Fatalf("err = %v, want server fatal error", err)
	}
	assertContains(t, out,
		fmt.Sprintf("Listening on http://127.0.0.1:%d", busyPort),
		"Database: "+env.dbPath,
	)
	if _, statErr := os.Stat(env.dbPath); statErr != nil {
		t.Errorf("start did not create the database: %v", statErr)
	}
}
