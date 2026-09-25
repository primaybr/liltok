package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/crypto"
)

func TestMaintainerKeygen(t *testing.T) {
	env := newTestEnv(t, "")
	keyPath := filepath.Join(env.home, "keys", "nested", "maint.key")

	out, err := env.run(t, "maintainer", "keygen", "--out", keyPath)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	assertContains(t, out, "Private Key File: "+keyPath, "maintainer:", "enabled: true")

	pubKeyStr := fieldAfter(t, out, "Public Key:")
	pub, err := crypto.ParsePublicKey(pubKeyStr)
	if err != nil {
		t.Fatalf("printed public key does not parse: %v", err)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("private key file not written: %v", err)
	}
	priv, err := crypto.ParsePrivateKey(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("private key file does not parse: %v", err)
	}

	// The printed public key and the saved private key must be a matching pair.
	envelope, err := crypto.EncryptPayload(pub, []byte("round trip"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := crypto.DecryptPayload(priv, envelope)
	if err != nil || string(plain) != "round trip" {
		t.Fatalf("key pair mismatch: plain=%q err=%v", plain, err)
	}
}

func TestMaintainerKeygenDefaultPath(t *testing.T) {
	env := newTestEnv(t, "")
	out, err := env.run(t, "maintainer", "keygen")
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	want := filepath.Join(env.home, ".liltok", "maintainer.key")
	assertContains(t, out, "Private Key File: "+want)
	if _, err := os.Stat(want); err != nil {
		t.Errorf("default key file missing: %v", err)
	}
}

func TestMaintainerKeygenWriteError(t *testing.T) {
	env := newTestEnv(t, "")
	blocker := filepath.Join(env.home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := env.run(t, "maintainer", "keygen", "-o", filepath.Join(blocker, "maint.key"))
	if err == nil || !strings.Contains(err.Error(), "failed to create directory") {
		t.Errorf("err = %v, want directory creation failure", err)
	}
}
