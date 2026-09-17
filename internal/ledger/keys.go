package ledger

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/primaybr/liltok/internal/db"
)

var (
	ErrKeyNotFound = errors.New("virtual api key not found")
	ErrKeyInactive = errors.New("virtual api key is inactive or revoked")
)

// APIKey represents a virtual client API key managed by Liltok.
type APIKey struct {
	ID               string    `json:"id"`
	KeyHash          string    `json:"key_hash"`
	Name             string    `json:"name"`
	RPM              int       `json:"rpm_limit"`
	TPM              int       `json:"tpm_limit"`
	MonthlyBudgetUSD float64   `json:"monthly_budget_usd"`
	CurrentSpendUSD  float64   `json:"current_spend_usd"`
	IsActive         bool      `json:"is_active"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// KeyManager handles virtual API key generation, validation, and lifecycle.
type KeyManager struct {
	db *db.DB
}

// NewKeyManager creates a new KeyManager.
func NewKeyManager(database *db.DB) *KeyManager {
	return &KeyManager{db: database}
}

// HashKey computes SHA-256 digest of a raw API key.
func HashKey(rawKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawKey)))
	return hex.EncodeToString(sum[:])
}

// GenerateRawKey produces a cryptographically secure "lt-live-xxxx" key string.
func GenerateRawKey() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "lt-live-" + hex.EncodeToString(bytes), nil
}

// CreateKey creates a new virtual API key, persisting its hash to SQLite and returning the raw secret.
func (km *KeyManager) CreateKey(ctx context.Context, name string, budgetUSD float64, rpm, tpm int) (string, *APIKey, error) {
	if km.db == nil {
		return "", nil, errors.New("database not available")
	}

	rawKey, err := GenerateRawKey()
	if err != nil {
		return "", nil, fmt.Errorf("failed to generate raw key: %w", err)
	}

	keyHash := HashKey(rawKey)
	id := "key_" + uuid.New().String()[:8]

	if rpm <= 0 {
		rpm = 60
	}
	if tpm <= 0 {
		tpm = 100000
	}
	if budgetUSD < 0 {
		budgetUSD = 0.0
	}

	now := time.Now()
	_, err = km.db.ExecContext(ctx, `
		INSERT INTO api_keys (id, key_hash, name, rpm_limit, tpm_limit, monthly_budget_usd, current_spend_usd, is_active, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 0.0, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, id, keyHash, name, rpm, tpm, budgetUSD)
	if err != nil {
		return "", nil, fmt.Errorf("failed to store api key: %w", err)
	}

	key := &APIKey{
		ID:               id,
		KeyHash:          keyHash,
		Name:             name,
		RPM:              rpm,
		TPM:              tpm,
		MonthlyBudgetUSD: budgetUSD,
		CurrentSpendUSD:  0.0,
		IsActive:         true,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	return rawKey, key, nil
}

// ValidateKey hashes the provided raw key and verifies active status in SQLite.
func (km *KeyManager) ValidateKey(ctx context.Context, rawKey string) (*APIKey, error) {
	if km.db == nil {
		return nil, errors.New("database not available")
	}

	keyHash := HashKey(rawKey)
	row := km.db.QueryRowContext(ctx, `
		SELECT id, key_hash, name, rpm_limit, tpm_limit, monthly_budget_usd, current_spend_usd, is_active, created_at, updated_at
		FROM api_keys
		WHERE key_hash = ?
	`, keyHash)

	var k APIKey
	var createdAt, updatedAt string
	err := row.Scan(&k.ID, &k.KeyHash, &k.Name, &k.RPM, &k.TPM, &k.MonthlyBudgetUSD, &k.CurrentSpendUSD, &k.IsActive, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrKeyNotFound
		}
		return nil, err
	}

	if !k.IsActive {
		return nil, ErrKeyInactive
	}

	return &k, nil
}

// ListKeys returns all registered virtual API keys.
func (km *KeyManager) ListKeys(ctx context.Context) ([]*APIKey, error) {
	if km.db == nil {
		return nil, errors.New("database not available")
	}

	rows, err := km.db.QueryContext(ctx, `
		SELECT id, key_hash, name, rpm_limit, tpm_limit, monthly_budget_usd, current_spend_usd, is_active, created_at, updated_at
		FROM api_keys
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []*APIKey
	for rows.Next() {
		var k APIKey
		var createdAt, updatedAt string
		if err := rows.Scan(&k.ID, &k.KeyHash, &k.Name, &k.RPM, &k.TPM, &k.MonthlyBudgetUSD, &k.CurrentSpendUSD, &k.IsActive, &createdAt, &updatedAt); err == nil {
			keys = append(keys, &k)
		}
	}

	return keys, nil
}

// RevokeKey deactivates a virtual key by ID.
func (km *KeyManager) RevokeKey(ctx context.Context, id string) error {
	if km.db == nil {
		return errors.New("database not available")
	}

	res, err := km.db.ExecContext(ctx, `
		UPDATE api_keys
		SET is_active = 0, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, id)
	if err != nil {
		return err
	}

	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// UpdateSpend increments the current spend for an API key.
func (km *KeyManager) UpdateSpend(ctx context.Context, id string, additionalUSD float64) error {
	if km.db == nil || additionalUSD <= 0 {
		return nil
	}

	_, err := km.db.ExecContext(ctx, `
		UPDATE api_keys
		SET current_spend_usd = current_spend_usd + ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, additionalUSD, id)
	return err
}
