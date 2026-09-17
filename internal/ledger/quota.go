package ledger

import (
	"sync"
	"time"
)

type tokenBucket struct {
	capacity   float64
	tokens     float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

func newTokenBucket(capacityPerMin int) *tokenBucket {
	capF := float64(capacityPerMin)
	return &tokenBucket{
		capacity:   capF,
		tokens:     capF,
		refillRate: capF / 60.0,
		lastRefill: time.Now(),
	}
}

func (tb *tokenBucket) consume(amount float64) bool {
	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.lastRefill = now

	// Refill
	tb.tokens += elapsed * tb.refillRate
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}

	if tb.tokens >= amount {
		tb.tokens -= amount
		return true
	}
	return false
}

type keyLimiter struct {
	rpmBucket *tokenBucket
	tpmBucket *tokenBucket
}

// QuotaEnforcer manages rate limiting (RPM/TPM) and monthly dollar budget enforcement.
type QuotaEnforcer struct {
	mu       sync.Mutex
	limiters map[string]*keyLimiter
}

// NewQuotaEnforcer creates a new QuotaEnforcer.
func NewQuotaEnforcer() *QuotaEnforcer {
	return &QuotaEnforcer{
		limiters: make(map[string]*keyLimiter),
	}
}

// CheckRateLimit verifies if a request conforms to the key's RPM and TPM limits.
func (qe *QuotaEnforcer) CheckRateLimit(key *APIKey, estimatedTokens int) (bool, string) {
	if key == nil {
		return true, ""
	}

	qe.mu.Lock()
	limiter, exists := qe.limiters[key.ID]
	if !exists {
		limiter = &keyLimiter{
			rpmBucket: newTokenBucket(key.RPM),
			tpmBucket: newTokenBucket(key.TPM),
		}
		qe.limiters[key.ID] = limiter
	}
	qe.mu.Unlock()

	// Check RPM
	if !limiter.rpmBucket.consume(1.0) {
		return false, "RPM (requests per minute) limit exceeded"
	}

	// Check TPM
	tokenCost := float64(estimatedTokens)
	if tokenCost < 1.0 {
		tokenCost = 1.0
	}
	if !limiter.tpmBucket.consume(tokenCost) {
		return false, "TPM (tokens per minute) limit exceeded"
	}

	return true, ""
}

// CheckBudget evaluates whether the virtual key has exceeded its monthly spend cap.
func (qe *QuotaEnforcer) CheckBudget(key *APIKey) (bool, string) {
	if key == nil {
		return true, ""
	}

	if key.MonthlyBudgetUSD > 0 && key.CurrentSpendUSD >= key.MonthlyBudgetUSD {
		return false, "monthly budget quota exceeded; cache hits permitted but upstream requests blocked"
	}

	return true, ""
}
