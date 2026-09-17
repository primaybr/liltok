package router

import (
	"testing"
	"time"
)

func TestCircuitBreakerStateTransitions(t *testing.T) {
	cb := NewCircuitBreaker("test-provider")
	cb.cooldownDuration = 50 * time.Millisecond // fast cooldown for test
	cb.currentCooldown = 50 * time.Millisecond

	// 1. Initially CLOSED and allows traffic
	state, failures := cb.State()
	if state != StateClosed || failures != 0 {
		t.Errorf("expected initial state CLOSED, got %s", state)
	}
	if !cb.Allow() {
		t.Errorf("expected Allow() to be true initially")
	}

	// 2. 4 failures should still remain CLOSED
	for i := 0; i < 4; i++ {
		cb.RecordFailure()
	}
	state, _ = cb.State()
	if state != StateClosed {
		t.Errorf("expected CLOSED after 4 failures, got %s", state)
	}

	// 3. 5th failure trips circuit to OPEN
	cb.RecordFailure()
	state, _ = cb.State()
	if state != StateOpen {
		t.Errorf("expected OPEN after 5 failures, got %s", state)
	}

	// 4. While OPEN and cooldown not elapsed, Allow() must be FALSE
	if cb.Allow() {
		t.Errorf("expected Allow() to be false while OPEN")
	}

	// 5. Wait for cooldown to elapse
	time.Sleep(60 * time.Millisecond)

	// 6. Allow() should now transition to HALF_OPEN and return TRUE for probe
	if !cb.Allow() {
		t.Errorf("expected Allow() to be true after cooldown")
	}
	state, _ = cb.State()
	if state != StateHalfOpen {
		t.Errorf("expected HALF_OPEN after cooldown, got %s", state)
	}

	// 7. Successful probe 1
	cb.RecordSuccess()
	state, _ = cb.State()
	if state != StateHalfOpen {
		t.Errorf("expected still HALF_OPEN after 1 success, got %s", state)
	}

	// 8. Successful probe 2 resets to CLOSED
	cb.RecordSuccess()
	state, failures = cb.State()
	if state != StateClosed || failures != 0 {
		t.Errorf("expected CLOSED and 0 failures after 2 successes, got %s", state)
	}
}

func TestCircuitBreakerImmediateTrip(t *testing.T) {
	cb := NewCircuitBreaker("test-provider")
	cb.TripImmediate()

	state, _ := cb.State()
	if state != StateOpen {
		t.Errorf("expected OPEN after TripImmediate, got %s", state)
	}
	if cb.Allow() {
		t.Errorf("expected Allow() false after immediate trip")
	}
}
