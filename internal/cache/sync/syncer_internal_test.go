package sync

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewCacheSyncer_Defaults(t *testing.T) {
	s := NewCacheSyncer(nil, "", nil)
	if !strings.HasSuffix(s.syncURL, "/releases/latest/download/starter_cache.json.gz") {
		t.Errorf("default sync URL = %q, want GitHub latest release asset", s.syncURL)
	}
	if s.httpClient == nil || s.httpClient.Timeout != 45*time.Second {
		t.Errorf("expected default client with 45s timeout, got %+v", s.httpClient)
	}

	custom := &http.Client{Timeout: time.Second}
	s = NewCacheSyncer(nil, "http://mirror.invalid/pack.gz", custom)
	if s.syncURL != "http://mirror.invalid/pack.gz" || s.httpClient != custom {
		t.Errorf("explicit URL or client not kept: url=%q client=%p", s.syncURL, s.httpClient)
	}
}

func TestSync_RejectsEmptyAndMalformedURL(t *testing.T) {
	empty := &CacheSyncer{httpClient: http.DefaultClient}
	if _, err := empty.Sync(context.Background(), false); err == nil || !strings.Contains(err.Error(), "sync URL is empty") {
		t.Errorf("expected empty URL error, got %v", err)
	}

	bad := NewCacheSyncer(nil, "://not a url", nil)
	if _, err := bad.Sync(context.Background(), false); err == nil || !strings.Contains(err.Error(), "failed to create sync request") {
		t.Errorf("expected request construction error, got %v", err)
	}
}

func TestSetMetadata_NilDatabaseIsNoop(t *testing.T) {
	s := &CacheSyncer{}
	// Must not panic without a database.
	s.setMetadata("k", "v")
}
