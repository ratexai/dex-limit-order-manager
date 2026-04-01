package positionmanager

import (
	"sync"
	"time"
)

// CircuitState represents the state of a circuit breaker.
type CircuitState uint8

const (
	CircuitClosed   CircuitState = iota // Normal operation.
	CircuitOpen                         // Tripped — rejecting executions.
	CircuitHalfOpen                     // Probing — allowing one execution.
)

func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "CLOSED"
	case CircuitOpen:
		return "OPEN"
	case CircuitHalfOpen:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}

// CircuitBreaker prevents cascading failures by pausing execution
// after consecutive failures exceed a threshold.
//
// Usage: the host configures it via Config and the Manager checks it
// before each swap execution. The breaker is per-chain.
//
//	breaker := NewCircuitBreaker(5, 30*time.Second) // 5 failures → 30s cooldown
type CircuitBreaker struct {
	mu           sync.Mutex
	state        CircuitState
	failures     int
	threshold    int           // Consecutive failures to trip.
	cooldown     time.Duration // How long to stay open.
	lastFailedAt time.Time
}

// NewCircuitBreaker creates a circuit breaker.
// threshold: number of consecutive failures to trip the breaker.
// cooldown: duration to stay open before transitioning to half-open.
func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:     CircuitClosed,
		threshold: threshold,
		cooldown:  cooldown,
	}
}

// Allow checks if an execution is permitted. Returns true if the circuit
// is closed or half-open (probe). Returns false if open (cooling down).
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		if time.Since(cb.lastFailedAt) >= cb.cooldown {
			cb.state = CircuitHalfOpen
			return true
		}
		return false
	case CircuitHalfOpen:
		return true // Allow probe.
	}
	return true
}

// RecordSuccess records a successful execution, resetting the breaker.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.state = CircuitClosed
}

// RecordFailure records a failed execution. If consecutive failures
// exceed the threshold, the breaker trips to Open state.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures++
	cb.lastFailedAt = time.Now()

	if cb.failures >= cb.threshold {
		cb.state = CircuitOpen
	}
}

// State returns the current circuit state.
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	// Check for auto-transition from Open to HalfOpen.
	if cb.state == CircuitOpen && time.Since(cb.lastFailedAt) >= cb.cooldown {
		cb.state = CircuitHalfOpen
	}
	return cb.state
}

// Failures returns the current consecutive failure count.
func (cb *CircuitBreaker) Failures() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.failures
}
