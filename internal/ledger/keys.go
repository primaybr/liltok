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

// Spend periods are calendar periods in UTC, stored as text so SQL can compare them directly.
const (
	spendDayLayout   = "2006-01-02"
	spendMonthLayout = "2006-01"
)

func spendDay(t time.Time) string   { return t.UTC().Format(spendDayLayout) }
func spendMonth(t time.Time) string { return t.UTC().Format(spendMonthLayout) }

// APIKey represents a virtual client API key managed by Liltok.
//
// CurrentSpendUSD is the spend of the UTC month named by SpendMonth and DailySpendUSD the spend of
// the UTC day named by DailySpendDate. Keys read through a KeyManager have spend from an earlier
// period already reported as 0.
type APIKey struct {
	ID               string    `json:"id"`
	KeyHash          string    `json:"key_hash"`
	Name             string    `json:"name"`
	RPM              int       `json:"rpm_limit"`
	TPM              int       `json:"tpm_limit"`
	MonthlyBudgetUSD float64   `json:"monthly_budget_usd"`
	CurrentSpendUSD  float64   `json:"current_spend_usd"`
	SpendMonth       string    `json:"spend_month"`
	DailyBudgetUSD   float64   `json:"daily_budget_usd"`
	DailySpendUSD    float64   `json:"daily_spend_usd"`
	DailySpendDate   string    `json:"daily_spend_date"`
	IsActive         bool      `json:"is_active"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// monthlySpendAt returns the key's spend in the UTC month containing now. An empty SpendMonth
// (period unknown) counts the spend as current, so a key built without it is never under-charged;
// stored keys always carry a month when they have spend.
func (k *APIKey) monthlySpendAt(now time.Time) float64 {
	if k.SpendMonth != "" && k.SpendMonth != spendMonth(now) {
		return 0
	}
	return k.CurrentSpendUSD
}

// dailySpendAt returns the key's spend on the UTC day containing now. An empty DailySpendDate
// counts the spend as current, as for monthlySpendAt; stored keys with daily spend carry a date.
func (k *APIKey) dailySpendAt(now time.Time) float64 {
	if k.DailySpendDate != "" && k.DailySpendDate != spendDay(now) {
		return 0
	}
	return k.DailySpendUSD
}

// KeyOptions are the limits a new virtual key is created with. Zero budgets mean no cap; zero or
// negative rate limits fall back to the defaults (60 RPM, 100000 TPM).
type KeyOptions struct {
	Name             string
	MonthlyBudgetUSD float64
	DailyBudgetUSD   float64
	RPM              int
	TPM              int
}

// KeyManager handles virtual API key generation, validation, and lifecycle.
type KeyManager struct {
	db  *db.DB
	now func() time.Time
}

// NewKeyManager creates a new KeyManager.
func NewKeyManager(database *db.DB) *KeyManager {
	return &KeyManager{db: database, now: time.Now}
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

// CreateKey creates a new virtual API key with a monthly budget and no daily cap, persisting its
// hash to SQLite and returning the raw secret.
func (km *KeyManager) CreateKey(ctx context.Context, name string, budgetUSD float64, rpm, tpm int) (string, *APIKey, error) {
	return km.CreateKeyWithOptions(ctx, KeyOptions{Name: name, MonthlyBudgetUSD: budgetUSD, RPM: rpm, TPM: tpm})
}

// CreateKeyWithOptions creates a new virtual API key with the given limits, persisting its hash to
// SQLite and returning the raw secret.
func (km *KeyManager) CreateKeyWithOptions(ctx context.Context, opts KeyOptions) (string, *APIKey, error) {
	if km.db == nil {
		return "", nil, errors.New("database not available")
	}

	rawKey, err := GenerateRawKey()
	if err != nil {
		return "", nil, fmt.Errorf("failed to generate raw key: %w", err)
	}

	keyHash := HashKey(rawKey)
	id := "key_" + uuid.New().String()[:8]

	if opts.RPM <= 0 {
		opts.RPM = 60
	}
	if opts.TPM <= 0 {
		opts.TPM = 100000
	}
	if opts.MonthlyBudgetUSD < 0 {
		opts.MonthlyBudgetUSD = 0.0
	}
	if opts.DailyBudgetUSD < 0 {
		opts.DailyBudgetUSD = 0.0
	}

	now := km.now()
	month := spendMonth(now)
	_, err = km.db.ExecContext(ctx, `
		INSERT INTO api_keys (id, key_hash, name, rpm_limit, tpm_limit, monthly_budget_usd, current_spend_usd, spend_month,
			daily_budget_usd, daily_spend_usd, daily_spend_date, is_active, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 0.0, ?, ?, 0.0, '', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, id, keyHash, opts.Name, opts.RPM, opts.TPM, opts.MonthlyBudgetUSD, month, opts.DailyBudgetUSD)
	if err != nil {
		return "", nil, fmt.Errorf("failed to store api key: %w", err)
	}

	key := &APIKey{
		ID:               id,
		KeyHash:          keyHash,
		Name:             opts.Name,
		RPM:              opts.RPM,
		TPM:              opts.TPM,
		MonthlyBudgetUSD: opts.MonthlyBudgetUSD,
		CurrentSpendUSD:  0.0,
		SpendMonth:       month,
		DailyBudgetUSD:   opts.DailyBudgetUSD,
		IsActive:         true,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	return rawKey, key, nil
}

const keyColumns = `id, key_hash, name, rpm_limit, tpm_limit, monthly_budget_usd, current_spend_usd, spend_month,
	daily_budget_usd, daily_spend_usd, daily_spend_date, is_active, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

// scanKey reads one row selected with keyColumns. Spend from an earlier month or day is reported
// as 0 with the period moved to the current one, so callers only ever see current spend.
func (km *KeyManager) scanKey(row rowScanner) (*APIKey, error) {
	var k APIKey
	var createdAt, updatedAt string
	if err := row.Scan(&k.ID, &k.KeyHash, &k.Name, &k.RPM, &k.TPM, &k.MonthlyBudgetUSD, &k.CurrentSpendUSD, &k.SpendMonth,
		&k.DailyBudgetUSD, &k.DailySpendUSD, &k.DailySpendDate, &k.IsActive, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	now := km.now()
	k.CurrentSpendUSD = k.monthlySpendAt(now)
	k.SpendMonth = spendMonth(now)
	k.DailySpendUSD = k.dailySpendAt(now)
	k.DailySpendDate = spendDay(now)
	return &k, nil
}

// ValidateKey hashes the provided raw key and verifies active status in SQLite.
func (km *KeyManager) ValidateKey(ctx context.Context, rawKey string) (*APIKey, error) {
	if km.db == nil {
		return nil, errors.New("database not available")
	}

	keyHash := HashKey(rawKey)
	row := km.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE key_hash = ?`, keyHash)

	k, err := km.scanKey(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrKeyNotFound
		}
		return nil, err
	}

	if !k.IsActive {
		return nil, ErrKeyInactive
	}

	return k, nil
}

// ListKeys returns all registered virtual API keys.
func (km *KeyManager) ListKeys(ctx context.Context) ([]*APIKey, error) {
	if km.db == nil {
		return nil, errors.New("database not available")
	}

	rows, err := km.db.QueryContext(ctx, `SELECT `+keyColumns+` FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []*APIKey
	for rows.Next() {
		if k, err := km.scanKey(rows); err == nil {
			keys = append(keys, k)
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

// UpdateSpend adds additionalUSD to the key's spend for the current UTC month and day. A counter
// whose period has ended restarts at additionalUSD. The whole change is one UPDATE (SQLite
// evaluates every SET expression against the row as it was), so concurrent calls cannot lose spend.
func (km *KeyManager) UpdateSpend(ctx context.Context, id string, additionalUSD float64) error {
	if km.db == nil || additionalUSD <= 0 {
		return nil
	}

	now := km.now()
	month, day := spendMonth(now), spendDay(now)
	_, err := km.db.ExecContext(ctx, `
		UPDATE api_keys
		SET current_spend_usd = CASE WHEN spend_month = ? THEN current_spend_usd + ? ELSE ? END,
			spend_month = ?,
			daily_spend_usd = CASE WHEN daily_spend_date = ? THEN daily_spend_usd + ? ELSE ? END,
			daily_spend_date = ?,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, month, additionalUSD, additionalUSD, month, day, additionalUSD, additionalUSD, day, id)
	return err
}
