package admin

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/liltok/liltok/internal/config"
	"github.com/liltok/liltok/internal/crypto"
	"github.com/liltok/liltok/internal/db"
)

func TestModerationEndpointsLifecycle(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer database.Close()

	pair, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair failed: %v", err)
	}

	tempDir := t.TempDir()
	keyFile := filepath.Join(tempDir, "maintainer.key")
	if err := os.WriteFile(keyFile, []byte(pair.PrivateKeyStr), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Maintainer.Enabled = true
	cfg.Maintainer.PrivateKeyFile = keyFile
	cfg.Maintainer.PublicKey = pair.PublicKeyStr

	handler := NewAdminHandler(cfg, database, nil, nil, nil, nil, nil)
	r := chi.NewRouter()
	handler.RegisterRoutes(r)

	// 1. Status Check
	req := httptest.NewRequest("GET", "/api/v1/moderate/status", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var statusResp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &statusResp)
	if statusResp["enabled"] != true || statusResp["has_key"] != true {
		t.Fatalf("expected maintainer enabled with key, got %v", statusResp)
	}

	// 2. Prepare encrypted payload for upload
	candidates := []db.StarterCacheItem{
		{
			Hash:             "mod_hash_1",
			Model:            "gpt-4o",
			NormalizedPrompt: "How to reverse a linked list?",
			ResponsePayload:  "Iterative solution...",
			PromptTokens:     12,
			CompletionTokens: 25,
			TTLSeconds:       86400,
		},
		{
			Hash:             "mod_hash_2",
			Model:            "claude-opus-5",
			NormalizedPrompt: "Explain Paxos consensus",
			ResponsePayload:  "Paxos is a family of protocols...",
			PromptTokens:     20,
			CompletionTokens: 60,
			TTLSeconds:       86400,
		},
	}

	rawJSON, _ := json.Marshal(candidates)
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	gw.Write(rawJSON)
	gw.Close()

	encryptedEnvelope, err := crypto.EncryptPayload(pair.PublicKey, gzBuf.Bytes())
	if err != nil {
		t.Fatalf("EncryptPayload failed: %v", err)
	}

	// 3. Upload encrypted pack
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "submission.enc")
	part.Write(encryptedEnvelope)
	writer.Close()

	uploadReq := httptest.NewRequest("POST", "/api/v1/moderate/upload", &body)
	uploadReq.Header.Set("Content-Type", writer.FormDataContentType())
	uploadRec := httptest.NewRecorder()
	r.ServeHTTP(uploadRec, uploadReq)

	if uploadRec.Code != http.StatusOK {
		t.Fatalf("expected upload status 200, got %d (%s)", uploadRec.Code, uploadRec.Body.String())
	}

	var uploadResp map[string]interface{}
	_ = json.Unmarshal(uploadRec.Body.Bytes(), &uploadResp)
	if uploadResp["staged_count"] != float64(2) {
		t.Fatalf("expected 2 staged entries, got %v", uploadResp["staged_count"])
	}

	// 4. Fetch staged entries
	entriesReq := httptest.NewRequest("GET", "/api/v1/moderate/entries?status=pending", nil)
	entriesRec := httptest.NewRecorder()
	r.ServeHTTP(entriesRec, entriesReq)

	if entriesRec.Code != http.StatusOK {
		t.Fatalf("expected entries status 200, got %d", entriesRec.Code)
	}

	var entriesResp struct {
		Entries []db.StagedEntry `json:"entries"`
		Count   int              `json:"count"`
	}
	_ = json.Unmarshal(entriesRec.Body.Bytes(), &entriesResp)
	if entriesResp.Count != 2 {
		t.Fatalf("expected 2 entries, got %d", entriesResp.Count)
	}

	// 5. Approve entry 1
	approveBody, _ := json.Marshal(map[string]interface{}{
		"ids": []int64{entriesResp.Entries[0].ID},
	})
	approveReq := httptest.NewRequest("POST", "/api/v1/moderate/approve", bytes.NewReader(approveBody))
	approveReq.Header.Set("Content-Type", "application/json")
	approveRec := httptest.NewRecorder()
	r.ServeHTTP(approveRec, approveReq)

	if approveRec.Code != http.StatusOK {
		t.Fatalf("expected approve status 200, got %d", approveRec.Code)
	}

	// 6. Verify entry was merged into live cache
	var countInLive int
	_ = database.QueryRow("SELECT COUNT(*) FROM cache_entries WHERE hash = ?", entriesResp.Entries[0].Hash).Scan(&countInLive)
	if countInLive != 1 {
		t.Fatalf("expected entry in live cache, got count %d", countInLive)
	}
}
