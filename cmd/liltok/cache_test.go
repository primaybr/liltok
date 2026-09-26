package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/primaybr/liltok/internal/crypto"
	"github.com/primaybr/liltok/internal/miner"
)

// readPack decodes a gzip JSON cache pack written by export or pack.
func readPack(t *testing.T, path string) []miner.CacheExportItem {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pack: %v", err)
	}
	return decodePack(t, data)
}

func decodePack(t *testing.T, data []byte) []miner.CacheExportItem {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	var items []miner.CacheExportItem
	if err := json.NewDecoder(gz).Decode(&items); err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	return items
}

// encodePack builds a gzip JSON cache pack in the format the importer and syncer accept.
func encodePack(t *testing.T, items []miner.CacheExportItem) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(items); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCacheStatsAndList(t *testing.T) {
	env := newTestEnv(t, "")

	out, err := env.run(t, "cache", "list")
	if err != nil {
		t.Fatalf("cache list (empty): %v", err)
	}
	assertContains(t, out, "Cache is currently empty.")

	longPrompt := strings.Repeat("p", 60)
	env.insertEntry(t, "0123456789abcdef0123", "gpt-4o", longPrompt, `{"ok":true}`, 3, false)
	env.insertEntry(t, "short", "claude-x", "hello", `{}`, 4, true)

	out, err = env.run(t, "cache", "stats")
	if err != nil {
		t.Fatalf("cache stats: %v", err)
	}
	assertContains(t, out,
		"Database:         "+env.dbPath,
		"Active Entries:   2",
		"Exact Cache Hits: 3",
		"Total Hits:       7",
	)

	out, err = env.run(t, "cache", "list")
	if err != nil {
		t.Fatalf("cache list: %v", err)
	}
	assertContains(t, out,
		"HASH", "PROMPT PREVIEW",
		"0123456789ab...", // hashes longer than 12 chars are shortened
		strings.Repeat("p", 50)+"...",
		"EXACT", "SEMANTIC", "short", "claude-x",
	)
	if strings.Contains(out, "Cache is currently empty.") {
		t.Errorf("non-empty cache reported as empty")
	}
}

func TestCachePurge(t *testing.T) {
	env := newTestEnv(t, "")

	_, err := env.run(t, "cache", "purge")
	if err == nil || !strings.Contains(err.Error(), "must specify --all, --model <name> or --larger-than <size>") {
		t.Fatalf("err = %v, want flag requirement error", err)
	}

	env.insertEntry(t, "h1", "gpt-4o", "a", "{}", 0, false)
	env.insertEntry(t, "h2", "gpt-4o", "b", "{}", 0, false)
	env.insertEntry(t, "h3", "claude-x", "c", "{}", 0, false)

	out, err := env.run(t, "cache", "purge", "--model", "gpt-4o")
	if err != nil {
		t.Fatalf("purge --model: %v", err)
	}
	assertContains(t, out, "Successfully purged 2 cache entries.")
	if n := env.countEntries(t, ""); n != 1 {
		t.Fatalf("remaining entries = %d, want 1", n)
	}
	if n := env.countEntries(t, "claude-x"); n != 1 {
		t.Errorf("purge --model removed entries of another model")
	}

	out, err = env.run(t, "cache", "purge", "--all")
	if err != nil {
		t.Fatalf("purge --all: %v", err)
	}
	assertContains(t, out, "Successfully purged 1 cache entries.")
	if n := env.countEntries(t, ""); n != 0 {
		t.Errorf("remaining entries = %d, want 0", n)
	}
}

func TestCacheUpdate(t *testing.T) {
	pack := encodePack(t, []miner.CacheExportItem{
		{Hash: "remote-1", Model: "gpt-4o", NormalizedPrompt: "one", ResponsePayload: "{}", TTLSeconds: 60},
		{Hash: "remote-2", Model: "gpt-4o", NormalizedPrompt: "two", ResponsePayload: "{}", TTLSeconds: 60},
	})
	var requests, conditional int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			atomic.AddInt32(&conditional, 1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write(pack)
	}))
	defer srv.Close()

	// The sync URL comes from the config file when --url is not given.
	env := newTestEnv(t, "cache:\n  sync_url: '"+srv.URL+"/pack.json.gz'\n")

	out, err := env.run(t, "cache", "update")
	if err != nil {
		t.Fatalf("cache update: %v", err)
	}
	assertContains(t, out,
		"Remote Source: "+srv.URL+"/pack.json.gz",
		"Conditional Check",
		"Successfully synchronized 2 new cache entries",
		`Current Release ETag: "v1"`,
		"Total Active Cache: 2 entries",
	)

	// The stored ETag turns the second run into a 304 round trip.
	out, err = env.run(t, "cache", "update")
	if err != nil {
		t.Fatalf("second cache update: %v", err)
	}
	assertContains(t, out, "already up to date", "Total Active Cache: 2 entries")
	if got := atomic.LoadInt32(&conditional); got != 1 {
		t.Errorf("conditional requests = %d, want 1", got)
	}

	// --force skips the ETag and downloads again; the entries already exist so none are new.
	out, err = env.run(t, "cache", "update", "--force", "--url", srv.URL)
	if err != nil {
		t.Fatalf("forced cache update: %v", err)
	}
	assertContains(t, out, "Force re-sync", "Successfully synchronized 0 new cache entries")
	if got := atomic.LoadInt32(&requests); got != 3 {
		t.Errorf("requests = %d, want 3", got)
	}
}

func TestCacheUpdateRemoteError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	env := newTestEnv(t, "")

	_, err := env.run(t, "cache", "update", "-u", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "sync failed") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want sync failure with 404", err)
	}
}

func TestCacheExportImportRoundTrip(t *testing.T) {
	env := newTestEnv(t, "")
	env.insertEntry(t, "keep-hash", "gpt-4o", "key sk-abcdefghijklmnopqrstuvwxyz0123456789 here", `{"a":1}`, 5, false)
	env.insertEntry(t, "cold-hash", "gpt-4o", "rarely used", `{"b":2}`, 0, false)
	env.insertEntry(t, "other-hash", "claude-x", "other model", `{"c":3}`, 9, true)

	outFile := filepath.Join(env.home, "export.json.gz")
	out, err := env.run(t, "cache", "export", "-o", outFile, "--min-hits", "1", "--model", "gpt-4o")
	if err != nil {
		t.Fatalf("cache export: %v", err)
	}
	assertContains(t, out, "Output File:       "+outFile, "Exported Entries:  1", "Min Hits Filter:   1", "Privacy Sanitized: true", "Encrypted:         false")

	items := readPack(t, outFile)
	if len(items) != 1 || items[0].Hash != "keep-hash" {
		t.Fatalf("exported items = %+v, want only keep-hash", items)
	}
	if strings.Contains(items[0].NormalizedPrompt, "sk-abcdefghijklmnop") {
		t.Errorf("export did not sanitize API key: %q", items[0].NormalizedPrompt)
	}

	// Import the pack into a second, empty workspace.
	dst := newTestEnv(t, "")
	if _, err := dst.run(t, "cache", "import"); err == nil || !strings.Contains(err.Error(), "must specify path") {
		t.Errorf("import without a file: err = %v", err)
	}
	if _, err := dst.run(t, "cache", "import", filepath.Join(dst.home, "missing.json.gz")); err == nil || !strings.Contains(err.Error(), "failed to open cache pack") {
		t.Errorf("import of missing file: err = %v", err)
	}
	out, err = dst.run(t, "cache", "import", "--file", outFile)
	if err != nil {
		t.Fatalf("cache import: %v", err)
	}
	assertContains(t, out, "Successfully imported 1 new cache entries from "+outFile)
	if n := dst.countEntries(t, "gpt-4o"); n != 1 {
		t.Errorf("imported entries = %d, want 1", n)
	}

	// A file that is not gzip is rejected by the importer.
	junk := filepath.Join(dst.home, "junk.json.gz")
	if err := os.WriteFile(junk, []byte("not gzip"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.run(t, "cache", "import", junk); err == nil || !strings.Contains(err.Error(), "import failed") {
		t.Errorf("import of junk: err = %v", err)
	}
}

func TestCacheExportDefaultOutput(t *testing.T) {
	env := newTestEnv(t, "")
	env.insertEntry(t, "h1", "gpt-4o", "p", "{}", 0, false)
	t.Chdir(env.home)

	out, err := env.run(t, "cache", "export", "--sanitize=false")
	if err != nil {
		t.Fatalf("cache export: %v", err)
	}
	assertContains(t, out, "Output File:       community_cache_pack.json.gz", "Privacy Sanitized: false")
	if items := readPack(t, filepath.Join(env.home, "community_cache_pack.json.gz")); len(items) != 1 {
		t.Errorf("exported %d items, want 1", len(items))
	}
}

func TestCacheEncryptedExportAndDecrypt(t *testing.T) {
	env := newTestEnv(t, "")
	env.insertEntry(t, "secret-hash", "gpt-4o", "private prompt", `{"x":1}`, 2, false)

	pair, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(env.home, "maintainer.key")
	if err := os.WriteFile(keyFile, []byte(pair.PrivateKeyStr+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// With --encrypt and the default -o value the output name switches to the .enc default.
	t.Chdir(env.home)
	out, err := env.run(t, "cache", "export", "--encrypt", "--pubkey", pair.PublicKeyStr)
	if err != nil {
		t.Fatalf("encrypted export: %v", err)
	}
	assertContains(t, out, "Output File:       cache_submission.enc", "Encrypted:         true", "encrypted for the repository maintainer")

	encPath := filepath.Join(env.home, "cache_submission.enc")
	raw, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("private prompt")) {
		t.Fatal("encrypted envelope contains plaintext prompt")
	}

	out, err = env.run(t, "cache", "decrypt", encPath, "--key", keyFile)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	decPath := filepath.Join(env.home, "cache_submission.json.gz")
	assertContains(t, out, "Input Envelope:    "+encPath, "Output Archive:    "+decPath)
	items := readPack(t, decPath)
	if len(items) != 1 || items[0].NormalizedPrompt != "private prompt" {
		t.Errorf("decrypted items = %+v", items)
	}

	// A different private key cannot open the envelope.
	other, _ := crypto.GenerateKeyPair()
	otherKey := filepath.Join(env.home, "other.key")
	if err := os.WriteFile(otherKey, []byte(other.PrivateKeyStr), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(t, "cache", "decrypt", "-i", encPath, "-k", otherKey, "-o", filepath.Join(env.home, "x.gz")); err == nil || !strings.Contains(err.Error(), "decryption failed") {
		t.Errorf("decrypt with wrong key: err = %v", err)
	}
}

func TestCacheEncryptedExportUsesConfigKey(t *testing.T) {
	pair, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	env := newTestEnv(t, "maintainer:\n  public_key: '"+pair.PublicKeyStr+"'\n")
	env.insertEntry(t, "h1", "gpt-4o", "p", "{}", 0, false)

	outFile := filepath.Join(env.home, "sub.enc")
	if _, err := env.run(t, "cache", "export", "--encrypt", "-o", outFile); err != nil {
		t.Fatalf("export with config key: %v", err)
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := crypto.ParsePrivateKey(pair.PrivateKeyStr)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := crypto.DecryptPayload(priv, raw)
	if err != nil {
		t.Fatalf("envelope not encrypted for the configured key: %v", err)
	}
	if items := decodePack(t, plain); len(items) != 1 {
		t.Errorf("decrypted %d items, want 1", len(items))
	}
}

func TestCacheExportDecryptErrors(t *testing.T) {
	env := newTestEnv(t, "")

	outFile := filepath.Join(env.home, "x.enc")
	if _, err := env.run(t, "cache", "export", "--encrypt", "-o", outFile, "--pubkey", "not-a-key"); err == nil || !strings.Contains(err.Error(), "invalid maintainer public key") {
		t.Errorf("bad pubkey: err = %v", err)
	}
	if crypto.DefaultMaintainerPublicKey == "" {
		if _, err := env.run(t, "cache", "export", "--encrypt", "-o", outFile); err == nil || !strings.Contains(err.Error(), "maintainer public key required") {
			t.Errorf("missing pubkey: err = %v", err)
		}
	}

	if _, err := env.run(t, "cache", "decrypt"); err == nil || !strings.Contains(err.Error(), "must specify input") {
		t.Errorf("decrypt without input: err = %v", err)
	}
	// Without --key the default key path under the temp home is used and does not exist.
	if _, err := env.run(t, "cache", "decrypt", outFile); err == nil || !strings.Contains(err.Error(), "failed to read private key") {
		t.Errorf("decrypt without key file: err = %v", err)
	}
	badKey := filepath.Join(env.home, "bad.key")
	if err := os.WriteFile(badKey, []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(t, "cache", "decrypt", outFile, "-k", badKey); err == nil || !strings.Contains(err.Error(), "invalid private key") {
		t.Errorf("decrypt with bad key: err = %v", err)
	}
	pair, _ := crypto.GenerateKeyPair()
	goodKey := filepath.Join(env.home, "good.key")
	if err := os.WriteFile(goodKey, []byte(pair.PrivateKeyStr), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := env.run(t, "cache", "decrypt", filepath.Join(env.home, "missing.enc"), "-k", goodKey); err == nil || !strings.Contains(err.Error(), "failed to read encrypted file") {
		t.Errorf("decrypt missing envelope: err = %v", err)
	}
}

func TestCachePack(t *testing.T) {
	env := newTestEnv(t, "")
	env.insertEntry(t, "db-1", "gpt-4o", "from db", "{}", 2, false)
	env.insertEntry(t, "db-2", "gpt-4o", strings.Repeat("x", 200), "{}", 2, false)
	env.insertEntry(t, "db-3", "gpt-4o", "too cold", "{}", 0, false)

	// Seed the target archive with one existing entry so the merge path is exercised.
	target := filepath.Join(env.home, "out", "starter.json.gz")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, encodePack(t, []miner.CacheExportItem{{Hash: "base-1", Model: "m", NormalizedPrompt: "base"}}), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := env.run(t, "cache", "pack", "--from-db", env.dbPath, "-o", target, "--min-hits", "1", "--max-prompt-bytes", "100")
	if err != nil {
		t.Fatalf("cache pack: %v", err)
	}
	assertContains(t, out,
		"Source Database:        "+env.dbPath,
		"Output Archive:         "+target,
		"Existing Base Entries:  1",
		"Merged from Database:   1",
		"Total Packed Entries:   2",
	)
	hashes := map[string]bool{}
	for _, it := range readPack(t, target) {
		hashes[it.Hash] = true
	}
	if !hashes["base-1"] || !hashes["db-1"] || hashes["db-2"] || hashes["db-3"] {
		t.Errorf("packed hashes = %v, want base-1 and db-1 only", hashes)
	}
}

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]int{"262144": 262144, "256KB": 262144, "256k": 262144, "1MB": 1048576, "2m": 2097152, " 512 B ": 512} {
		if got, err := parseByteSize(in); err != nil || got != want {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-5KB", "0", "1.5MB"} {
		if _, err := parseByteSize(bad); err == nil {
			t.Errorf("parseByteSize(%q) should fail", bad)
		}
	}
}

func TestCachePurgeLargerThan(t *testing.T) {
	env := newTestEnv(t, "")

	// Through a running gateway: the CLI calls the admin API, which also flushes the memory tier.
	var gotQuery string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery
		_, _ = w.Write([]byte(`{"status":"purged","deleted_count":7}`))
	}))
	out, err := env.run(t, "cache", "purge", "--larger-than", "256KB", "--gateway-url", gw.URL)
	gw.Close()
	if err != nil || gotQuery != "POST /api/v1/cache/purge?larger_than=262144" {
		t.Fatalf("gateway purge: err %v, request %q", err, gotQuery)
	}
	assertContains(t, out, "Purged 7 cache entries larger than 262144 bytes through the gateway.")

	if _, err := env.run(t, "cache", "purge", "--larger-than", "lots"); err == nil || !strings.Contains(err.Error(), "invalid size") {
		t.Fatalf("bad size error = %v", err)
	}

	// Gateway offline: the database is edited directly.
	env.insertEntry(t, "small", "gpt-4o", "short", "{}", 0, false)
	env.insertEntry(t, "large", "gpt-4o", strings.Repeat("y", 3000), "{}", 0, false)
	out, err = env.run(t, "cache", "purge", "--larger-than", "2KB", "--gateway-url", gw.URL)
	if err != nil {
		t.Fatalf("offline purge: %v", err)
	}
	assertContains(t, out, "Purged 1 cache entries larger than 2048 bytes (gateway offline; database edited directly).")
	if n := env.countEntries(t, ""); n != 1 {
		t.Fatalf("remaining entries = %d, want only the small one", n)
	}
}
