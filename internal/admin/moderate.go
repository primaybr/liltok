package admin

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/primaybr/liltok/internal/crypto"
	"github.com/primaybr/liltok/internal/db"
)

// HandleModerateStatus returns whether maintainer mode is active, key status, and queue counts.
func (h *AdminHandler) HandleModerateStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg == nil || !h.cfg.Maintainer.Enabled {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled": false,
		})
		return
	}

	hasKey := false
	if h.cfg.Maintainer.PrivateKeyFile != "" {
		if data, err := os.ReadFile(h.cfg.Maintainer.PrivateKeyFile); err == nil && len(bytes.TrimSpace(data)) > 0 {
			if _, err := crypto.ParsePrivateKey(string(data)); err == nil {
				hasKey = true
			}
		}
	}

	pending, approved, rejected, _ := h.database.CountStagedEntries()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":        true,
		"has_key":        hasKey,
		"key_file":       h.cfg.Maintainer.PrivateKeyFile,
		"pending_count":  pending,
		"approved_count": approved,
		"rejected_count": rejected,
	})
}

// HandleModerateUpload accepts encrypted (.enc) or gzip (.json.gz) cache packs and stages them.
func (h *AdminHandler) HandleModerateUpload(w http.ResponseWriter, r *http.Request) {
	if h.cfg == nil || !h.cfg.Maintainer.Enabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "maintainer moderation mode is disabled"})
		return
	}

	// Max 32MB payload
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to parse multipart form"})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file field"})
		return
	}
	defer file.Close()

	rawBytes, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read uploaded file"})
		return
	}

	var jsonBytes []byte

	// Check if file is an encrypted LTC1 envelope
	if len(rawBytes) >= 4 && string(rawBytes[:4]) == crypto.EnvelopeMagic {
		keyPath := h.cfg.Maintainer.PrivateKeyFile
		if keyPath == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "maintainer private key file is not configured"})
			return
		}

		keyData, err := os.ReadFile(keyPath)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("failed to read private key at %s: %v", keyPath, err),
			})
			return
		}

		privKey, err := crypto.ParsePrivateKey(string(keyData))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("invalid private key in %s: %v", keyPath, err),
			})
			return
		}

		decrypted, err := crypto.DecryptPayload(privKey, rawBytes)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": fmt.Sprintf("decryption failed: %v", err),
			})
			return
		}

		// Decrypted content is gzipped JSON
		gzReader, err := gzip.NewReader(bytes.NewReader(decrypted))
		if err != nil {
			// Might be raw JSON if sender skipped gzip
			jsonBytes = decrypted
		} else {
			defer gzReader.Close()
			decompressed, err := io.ReadAll(gzReader)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "failed to decompress decrypted payload"})
				return
			}
			jsonBytes = decompressed
		}
	} else {
		// Not encrypted: check if standard gzip
		gzReader, err := gzip.NewReader(bytes.NewReader(rawBytes))
		if err == nil {
			defer gzReader.Close()
			decompressed, err := io.ReadAll(gzReader)
			if err == nil {
				jsonBytes = decompressed
			} else {
				jsonBytes = rawBytes
			}
		} else {
			jsonBytes = rawBytes
		}
	}

	var items []db.StarterCacheItem
	if err := json.Unmarshal(jsonBytes, &items); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("failed to parse JSON cache entries: %v", err),
		})
		return
	}

	inserted, duplicates, err := h.database.InsertStagedEntries(items, header.Filename)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("failed to stage entries: %v", err),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":      true,
		"filename":     header.Filename,
		"total_parsed": len(items),
		"staged_count": inserted,
		"duplicates":   duplicates,
	})
}

// HandleModerateEntries returns paginated candidate entries for review.
func (h *AdminHandler) HandleModerateEntries(w http.ResponseWriter, r *http.Request) {
	if h.cfg == nil || !h.cfg.Maintainer.Enabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "maintainer moderation mode is disabled"})
		return
	}

	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}

	limit := 50
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if val, err := strconv.Atoi(l); err == nil && val > 0 {
			limit = val
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if val, err := strconv.Atoi(o); err == nil && val >= 0 {
			offset = val
		}
	}

	entries, err := h.database.GetStagedEntries(status, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("failed to retrieve staged entries: %v", err),
		})
		return
	}

	if entries == nil {
		entries = []db.StagedEntry{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries,
		"count":   len(entries),
		"status":  status,
	})
}

type moderateActionRequest struct {
	IDs []int64 `json:"ids"`
}

// HandleModerateApprove merges approved staged entries into the active database.
func (h *AdminHandler) HandleModerateApprove(w http.ResponseWriter, r *http.Request) {
	if h.cfg == nil || !h.cfg.Maintainer.Enabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "maintainer moderation mode is disabled"})
		return
	}

	var req moderateActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json payload"})
		return
	}

	if len(req.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no entry IDs specified"})
		return
	}

	merged, err := h.database.ApproveStagedEntries(r.Context(), req.IDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("approval error: %v", err),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":        true,
		"approved_count": merged,
	})
}

// HandleModerateReject marks staged entries as rejected.
func (h *AdminHandler) HandleModerateReject(w http.ResponseWriter, r *http.Request) {
	if h.cfg == nil || !h.cfg.Maintainer.Enabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "maintainer moderation mode is disabled"})
		return
	}

	var req moderateActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json payload"})
		return
	}

	if len(req.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no entry IDs specified"})
		return
	}

	if err := h.database.RejectStagedEntries(r.Context(), req.IDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("rejection error: %v", err),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":        true,
		"rejected_count": len(req.IDs),
	})
}

// HandleModerateClear cleans up entries from staging.
func (h *AdminHandler) HandleModerateClear(w http.ResponseWriter, r *http.Request) {
	if h.cfg == nil || !h.cfg.Maintainer.Enabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "maintainer moderation mode is disabled"})
		return
	}

	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = "all"
	}

	cleared, err := h.database.ClearStagedEntries(r.Context(), status)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("clear error: %v", err),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":       true,
		"cleared_count": cleared,
	})
}
