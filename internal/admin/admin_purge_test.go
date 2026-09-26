package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/primaybr/liltok/internal/admin"
	"github.com/primaybr/liltok/internal/cache"
	"github.com/primaybr/liltok/internal/cache/exact"
	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
)

func purgeEnv(t *testing.T) (*chi.Mux, *db.DB, *exact.TieredStore) {
	t.Helper()
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(t.TempDir() + "/purge.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store, err := exact.NewTieredStore(database, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := admin.NewAdminHandler(config.DefaultConfig(), database, nil, nil, nil, store, admin.NewBroadcaster())
	r := chi.NewRouter()
	h.RegisterRoutes(r)
	return r, database, store
}

// setAndPersist stores an entry through the tiered store and waits for its SQLite copy.
func setAndPersist(t *testing.T, database *db.DB, store *exact.TieredStore, hash, prompt string) {
	t.Helper()
	if err := store.Set(context.Background(), &cache.CacheEntry{Hash: hash, Model: "m", NormalizedPrompt: prompt, ResponsePayload: []byte(`{}`), TTLSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		_ = database.QueryRow(`SELECT COUNT(*) FROM cache_entries WHERE hash = ?`, hash).Scan(&n)
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("entry %s was not persisted", hash)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAdminPurgeLargerThan(t *testing.T) {
	r, database, store := purgeEnv(t)
	setAndPersist(t, database, store, "small", "short prompt")
	setAndPersist(t, database, store, "large", strings.Repeat("x", 5000))

	post := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("POST", target, nil))
		return rec
	}
	if rec := post("/api/v1/cache/purge?larger_than=abc"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid larger_than = %d, want 400", rec.Code)
	}
	if rec := post("/api/v1/cache/purge"); rec.Code != http.StatusBadRequest {
		t.Fatalf("purge without a selector = %d, want 400", rec.Code)
	}

	rec := post("/api/v1/cache/purge?larger_than=4096")
	var out struct {
		DeletedCount int64 `json:"deleted_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK || out.DeletedCount != 1 {
		t.Fatalf("purge = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := store.Get(context.Background(), "large"); ok {
		t.Fatal("the purged entry must not be served from the memory tier")
	}
	if _, ok, _ := store.Get(context.Background(), "small"); !ok {
		t.Fatal("entries under the limit must remain")
	}
}

func TestAdminDeleteEntryEvictsMemory(t *testing.T) {
	r, database, store := purgeEnv(t)
	setAndPersist(t, database, store, "doomed", "prompt")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/v1/cache/doomed", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	if _, ok, _ := store.Get(context.Background(), "doomed"); ok {
		t.Fatal("a deleted entry must not keep being served from the memory tier")
	}
}

func TestAdminAuthEndpoints(t *testing.T) {
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	database, err := db.Open(t.TempDir() + "/auth.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	cfg := config.DefaultConfig()
	cfg.Server.AdminToken = "s3cret"
	r := chi.NewRouter()
	admin.NewAdminHandler(cfg, database, nil, nil, nil, nil, admin.NewBroadcaster()).RegisterRoutes(r)

	status := func(cookie *http.Cookie) map[string]bool {
		req := httptest.NewRequest("GET", "/api/v1/auth", nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var st map[string]bool
		_ = json.Unmarshal(rec.Body.Bytes(), &st)
		return st
	}
	if st := status(nil); !st["required"] || st["authenticated"] {
		t.Fatalf("status before sign-in = %v", st)
	}

	login := func(token string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/auth", strings.NewReader(`{"token":"`+token+`"}`)))
		return rec
	}
	if rec := login("guess"); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("wrong token = %d with cookies %v", rec.Code, rec.Result().Cookies())
	}
	rec := login("s3cret")
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusOK || len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("sign-in = %d cookies %+v; want one HttpOnly SameSite=Strict cookie", rec.Code, cookies)
	}
	if st := status(cookies[0]); !st["authenticated"] {
		t.Fatalf("status with the cookie = %v", st)
	}

	out := httptest.NewRecorder()
	r.ServeHTTP(out, httptest.NewRequest("DELETE", "/api/v1/auth", nil))
	if c := out.Result().Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
		t.Fatalf("sign-out must expire the cookie, got %+v", c)
	}
}
