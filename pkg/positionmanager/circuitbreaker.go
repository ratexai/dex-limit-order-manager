package positionmanager

import (
	"fmt"
	"sync"
	"time"
)

// CircuitState represents the state of a circuit breaker.
type CircuitState uint8

const (
	CircuitClosed   CircuitState = iota // Normal operation.
	CircuitOpen                         // Failing — reject calls.
	CircuitHalfOpen                     // Testing recovery.
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

// CircuitBreakerConfig configures the circuit breaker.
type CircuitBreakerConfig struct {
	// MaxFailures before opening the circuit.
	MaxFailures int

	// ResetTimeout is how long to wait in Open state before trying HalfOpen.
	ResetTimeout time.Duration

	// HalfOpenMaxAttempts is the number of trial calls allowed in HalfOpen state.
	HalfOpenMaxAttempts int
}

// DefaultCircuitBreakerConfig returns sensible defaults.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		MaxFailures:         5,
		ResetTimeout:        30 * time.Second,
		HalfOpenMaxAttempts: 2,
	}
}

// CircuitBreaker implements the circuit breaker pattern for the executor.
type CircuitBreaker struct {
	mu              sync.Mutex
	cfg             CircuitBreakerConfig
	state           CircuitState
	failures        int
	successes       int // Successes in half-open state.
	lastFailureTime time.Time
}

// NewCircuitBreaker creates a circuit breaker with the given configuration.
func NewCircuitBreaker(cfg CircuitBreakerConfig) *CircuitBreaker {
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = 5
	}
	if cfg.ResetTimeout <= 0 {
		cfg.ResetTimeout = 30 * time.Second
	}
	if cfg.HalfOpenMaxAttempts <= 0 {
		cfg.HalfOpenMaxAttempts = 2
	}
	return &CircuitBreaker{cfg: cfg, state: CircuitClosed}
}

// ErrCircuitOpen is returned when the circuit is open.
var ErrCircuitOpen = fmt.Errorf("circuit breaker is open")

// Allow checks whether a call is permitted.
// Returns an error if the circuit is open.
func (cb *CircuitBreaker) Allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitClosed:
		return nil
	case CircuitOpen:
		if time.Since(cb.lastFailureTime) >= cb.cfg.ResetTimeout {
			cb.state = CircuitHalfOpen
			cb.successes = 0
			return nil
		}
		return ErrCircuitOpen
	case CircuitHalfOpen:
		return nil
	}
	return nil
}

// RecordSuccess records a successful call.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CircuitHalfOpen:
		cb.successes++
		if cb.successes >= cb.cfg.HalfOpenMaxAttempts {
			cb.state = CircuitClosed
			cb.failures = 0
		}
	case CircuitClosed:
		cb.failures = 0
	}
}

// RecordFailure records a failed call.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures++
	cb.lastFailureTime = time.Now()

	switch cb.state {
	case CircuitClosed:
		if cb.failures >= cb.cfg.MaxFailures {
			cb.state = CircuitOpen
		}
	case CircuitHalfOpen:
		cb.state = CircuitOpen
		cb.successes = 0
	}
}

// State returns the current circuit state.
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// Failures returns the current failure count.
func (cb *CircuitBreaker) Failures() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.failures
}
