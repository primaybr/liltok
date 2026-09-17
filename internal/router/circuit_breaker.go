package router

import (
	"fmt"
	"sync"
	"time"
)

// CircuitState represents the current state of a circuit breaker.
type CircuitState string

const (
	StateClosed   CircuitState = "CLOSED"
	StateOpen     CircuitState = "OPEN"
	StateHalfOpen CircuitState = "HALF_OPEN"
)

// CircuitBreaker manages provider health and trips on consecutive errors or rate limits.
type CircuitBreaker struct {
	mu sync.Mutex

	name             string
	state            CircuitState
	failureThreshold int           // Default 5
	successThreshold int           // Default 2
	cooldownDuration time.Duration // Default 30s
	currentCooldown  time.Duration

	consecutiveFailures  int
	consecutiveSuccesses int
	lastTrippedAt        time.Time
}

// NewCircuitBreaker initializes a circuit breaker for a provider.
func NewCircuitBreaker(name string) *CircuitBreaker {
	return &CircuitBreaker{
		name:             name,
		state:            StateClosed,
		failureThreshold: 5,
		successThreshold: 2,
		cooldownDuration: 30 * time.Second,
		currentCooldown:  30 * time.Second,
	}
}

// Allow checks if a request is permitted to proceed to this provider.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()

	switch cb.state {
	case StateClosed:
		return true
	case StateOpen:
		// Check if cooldown has elapsed
		if now.Sub(cb.lastTrippedAt) >= cb.currentCooldown {
			cb.state = StateHalfOpen
			cb.consecutiveSuccesses = 0
			return true // Allow probe request
		}
		return false
	case StateHalfOpen:
		// In HalfOpen, allow limited probe traffic
		return true
	default:
		return true
	}
}

// RecordSuccess registers a successful execution against this provider.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFailures = 0

	if cb.state == StateHalfOpen {
		cb.consecutiveSuccesses++
		if cb.consecutiveSuccesses >= cb.successThreshold {
			// Restore to healthy Closed state
			cb.state = StateClosed
			cb.currentCooldown = cb.cooldownDuration
			cb.consecutiveSuccesses = 0
		}
	}
}

// RecordFailure registers an execution failure or rate-limit response.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFailures++

	switch cb.state {
	case StateClosed:
		if cb.consecutiveFailures >= cb.failureThreshold {
			cb.trip()
		}
	case StateHalfOpen:
		// Probe failed -> trip back to Open with exponential backoff
		cb.currentCooldown *= 2
		if cb.currentCooldown > 5*time.Minute {
			cb.currentCooldown = 5 * time.Minute
		}
		cb.trip()
	}
}

// TripImmediate trips the circuit immediately (e.g. on 429 quota exhaustion).
func (cb *CircuitBreaker) TripImmediate() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.trip()
}

func (cb *CircuitBreaker) trip() {
	cb.state = StateOpen
	cb.lastTrippedAt = time.Now()
	cb.consecutiveSuccesses = 0
}

// State returns current state and consecutive error count.
func (cb *CircuitBreaker) State() (CircuitState, int) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state, cb.consecutiveFailures
}

// String returns a human-readable status description.
func (cb *CircuitBreaker) String() string {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return fmt.Sprintf("[%s] State: %s (Failures: %d)", cb.name, cb.state, cb.consecutiveFailures)
}
