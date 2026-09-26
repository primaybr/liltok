package config

import (
	"reflect"
	"testing"
)

func TestShareConfigDefaultsAndEnv(t *testing.T) {
	t.Setenv("LILTOK_SHARE_DENY_TERMS", "")
	t.Setenv("LILTOK_SHARE_URL_ALLOWLIST", "")
	cfg := DefaultConfig()
	if len(cfg.Share.DenyTerms) != 0 {
		t.Errorf("default deny terms = %q, want none", cfg.Share.DenyTerms)
	}
	wantAllow := []string{"go.dev", "pkg.go.dev", "developer.mozilla.org", "docs.python.org", "nodejs.org", "react.dev",
		"www.typescriptlang.org", "docs.docker.com", "kubernetes.io", "www.postgresql.org", "sqlite.org"}
	if !reflect.DeepEqual(cfg.Share.URLAllowlist, wantAllow) {
		t.Errorf("default allowlist = %q", cfg.Share.URLAllowlist)
	}

	t.Setenv("LILTOK_SHARE_DENY_TERMS", "acme-billing, projectx")
	t.Setenv("LILTOK_SHARE_URL_ALLOWLIST", "go.dev")
	cfg = DefaultConfig()
	applyEnvOverrides(cfg)
	if !reflect.DeepEqual(cfg.Share.DenyTerms, []string{"acme-billing", "projectx"}) {
		t.Errorf("env deny terms = %q", cfg.Share.DenyTerms)
	}
	if !reflect.DeepEqual(cfg.Share.URLAllowlist, []string{"go.dev"}) {
		t.Errorf("env allowlist = %q", cfg.Share.URLAllowlist)
	}
}

func TestShareConfigEmptyEnvClearsLists(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Share.DenyTerms = []string{"x-term"}
	t.Setenv("LILTOK_SHARE_DENY_TERMS", "")
	t.Setenv("LILTOK_SHARE_URL_ALLOWLIST", "")
	applyEnvOverrides(cfg)
	if len(cfg.Share.DenyTerms) != 0 {
		t.Errorf("deny terms after empty env = %q, want empty", cfg.Share.DenyTerms)
	}
	if len(cfg.Share.URLAllowlist) != 0 {
		t.Errorf("allowlist after empty env = %q, want empty", cfg.Share.URLAllowlist)
	}
}
