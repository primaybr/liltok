package ledger

import (
	"sync"
	"testing"
	"time"
)

func enforcerAt(start time.Time) (*QuotaEnforcer, *time.Time) {
	qe := NewQuotaEnforcer()
	now := start
	qe.now = func() time.Time { return now }
	return qe, &now
}

func TestCheckRateLimitChargesEstimatedTokens(t *testing.T) {
	qe, now := enforcerAt(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	key := &APIKey{ID: "k", RPM: 100, TPM: 1000}

	if ok, _ := qe.CheckRateLimit(key, 600); !ok {
		t.Fatal("first 600-token request must fit a 1000 TPM budget")
	}
	if ok, reason := qe.CheckRateLimit(key, 600); ok || reason != "TPM (tokens per minute) limit exceeded" {
		t.Fatalf("second 600-token request = %v %q, want a TPM rejection", ok, reason)
	}
	// 1000 TPM refills about 16.7 tokens per second; 200 more are needed, so 12 s is enough.
	*now = now.Add(12 * time.Second)
	if ok, _ := qe.CheckRateLimit(key, 600); !ok {
		t.Fatal("request must fit once the bucket has refilled")
	}
}

func TestCheckRateLimitTPMRejectionKeepsRPM(t *testing.T) {
	qe, _ := enforcerAt(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	key := &APIKey{ID: "k", RPM: 2, TPM: 100}

	if ok, _ := qe.CheckRateLimit(key, 100); !ok {
		t.Fatal("first request must be allowed")
	}
	for i := 0; i < 3; i++ {
		if ok, _ := qe.CheckRateLimit(key, 100); ok {
			t.Fatal("TPM is exhausted; request must be rejected")
		}
	}
	// The TPM rejections did not spend RPM, so one small request still fits RPM 2 once TPM has room.
	qe.limiters["k"].tpmBucket.tokens = 100
	if ok, reason := qe.CheckRateLimit(key, 10); !ok {
		t.Fatalf("second request rejected (%s); TPM rejections must not consume RPM", reason)
	}
	if ok, reason := qe.CheckRateLimit(key, 10); ok || reason != "RPM (requests per minute) limit exceeded" {
		t.Fatalf("third request = %v %q, want an RPM rejection", ok, reason)
	}
}

func TestCheckRateLimitOversizedRequestDrainsFullBucket(t *testing.T) {
	qe, now := enforcerAt(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	key := &APIKey{ID: "k", RPM: 100, TPM: 1000}

	if ok, _ := qe.CheckRateLimit(key, 150000); !ok {
		t.Fatal("a request larger than the whole TPM budget must be admitted when the bucket is full")
	}
	if ok, _ := qe.CheckRateLimit(key, 10); ok {
		t.Fatal("the oversized request must drain the bucket")
	}
	*now = now.Add(time.Minute)
	if ok, _ := qe.CheckRateLimit(key, 150000); !ok {
		t.Fatal("after a full minute the oversized request must fit again")
	}
}

func TestCheckRateLimitZeroMeansUnlimited(t *testing.T) {
	qe, _ := enforcerAt(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	key := &APIKey{ID: "k", RPM: 0, TPM: 0}
	for i := 0; i < 1000; i++ {
		if ok, reason := qe.CheckRateLimit(key, 1_000_000); !ok {
			t.Fatalf("request %d rejected (%s); limits of 0 mean no limit", i+1, reason)
		}
	}
}

func TestCheckRateLimitConcurrentRequestsStayWithinBudget(t *testing.T) {
	qe, _ := enforcerAt(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	key := &APIKey{ID: "k", RPM: 50, TPM: 1_000_000}

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := qe.CheckRateLimit(key, 10); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 50 {
		t.Fatalf("%d of 200 concurrent requests allowed, want exactly the RPM budget of 50", allowed)
	}
}
