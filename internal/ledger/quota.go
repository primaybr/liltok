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

// newTokenBucket returns a bucket refilled at capacityPerMin per minute, or nil (no limit) when
// capacityPerMin <= 0.
func newTokenBucket(capacityPerMin int) *tokenBucket {
	if capacityPerMin <= 0 {
		return nil
	}
	capF := float64(capacityPerMin)
	return &tokenBucket{
		capacity:   capF,
		tokens:     capF,
		refillRate: capF / 60.0,
		lastRefill: time.Now(),
	}
}

func (tb *tokenBucket) refill(now time.Time) {
	tb.tokens += now.Sub(tb.lastRefill).Seconds() * tb.refillRate
	tb.lastRefill = now
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}
}

// cost caps amount at the bucket's capacity, so a request larger than the whole per-minute budget
// is admitted when the bucket is full (and drains it) instead of never fitting.
func (tb *tokenBucket) cost(amount float64) float64 {
	if amount > tb.capacity {
		return tb.capacity
	}
	return amount
}

type keyLimiter struct {
	rpmBucket *tokenBucket // nil when the key has no RPM limit
	tpmBucket *tokenBucket // nil when the key has no TPM limit
}

// QuotaEnforcer manages rate limiting (RPM/TPM) and monthly dollar budget enforcement.
type QuotaEnforcer struct {
	mu       sync.Mutex
	limiters map[string]*keyLimiter
	now      func() time.Time
}

// NewQuotaEnforcer creates a new QuotaEnforcer.
func NewQuotaEnforcer() *QuotaEnforcer {
	return &QuotaEnforcer{
		limiters: make(map[string]*keyLimiter),
		now:      time.Now,
	}
}

// CheckRateLimit verifies that a request with estimatedTokens prompt tokens fits the key's RPM and
// TPM limits. Both buckets are checked before either is charged, so a request rejected for TPM
// does not use up RPM. A limit of 0 or less means no limit.
func (qe *QuotaEnforcer) CheckRateLimit(key *APIKey, estimatedTokens int) (bool, string) {
	if key == nil {
		return true, ""
	}

	qe.mu.Lock()
	defer qe.mu.Unlock()
	limiter, exists := qe.limiters[key.ID]
	if !exists {
		limiter = &keyLimiter{
			rpmBucket: newTokenBucket(key.RPM),
			tpmBucket: newTokenBucket(key.TPM),
		}
		qe.limiters[key.ID] = limiter
	}

	now := qe.now()
	tokenCost := float64(estimatedTokens)
	if tokenCost < 1.0 {
		tokenCost = 1.0
	}
	if rpm := limiter.rpmBucket; rpm != nil {
		rpm.refill(now)
		if rpm.tokens < 1.0 {
			return false, "RPM (requests per minute) limit exceeded"
		}
	}
	if tpm := limiter.tpmBucket; tpm != nil {
		tpm.refill(now)
		tokenCost = tpm.cost(tokenCost)
		if tpm.tokens < tokenCost {
			return false, "TPM (tokens per minute) limit exceeded"
		}
	}

	if limiter.rpmBucket != nil {
		limiter.rpmBucket.tokens--
	}
	if limiter.tpmBucket != nil {
		limiter.tpmBucket.tokens -= tokenCost
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
