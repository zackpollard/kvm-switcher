package circuitbreaker

import (
	"sync"
	"time"
)

// State represents the three circuit breaker states.
type State string

const (
	StateClosed   State = "closed"    // Normal operation
	StateOpen     State = "open"      // Failing, skip requests
	StateHalfOpen State = "half-open" // Testing if recovery happened
)

// Breaker implements a three-state circuit breaker.
type Breaker struct {
	mu           sync.Mutex
	state        State
	failures     int
	threshold    int
	resetTimeout time.Duration
	lastFailure  time.Time
}

// New creates a new Breaker.
// threshold is the number of consecutive failures before opening.
// resetTimeout is how long to wait before transitioning from open to half-open.
func New(threshold int, resetTimeout time.Duration) *Breaker {
	return &Breaker{
		state:        StateClosed,
		threshold:    threshold,
		resetTimeout: resetTimeout,
	}
}

// State returns the current circuit breaker state, transitioning from
// open to half-open if the reset timeout has elapsed.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == StateOpen && time.Since(b.lastFailure) >= b.resetTimeout {
		b.state = StateHalfOpen
	}
	return b.state
}

// Allow returns true if the request should proceed.
// Returns false when the circuit is open (caller should skip).
func (b *Breaker) Allow() bool {
	state := b.State()
	return state == StateClosed || state == StateHalfOpen
}

// RecordSuccess records a successful operation, resetting the breaker to closed.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures = 0
	b.state = StateClosed
}

// RecordFailure records a failed operation, potentially opening the circuit.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures++
	b.lastFailure = time.Now()

	if b.failures >= b.threshold {
		b.state = StateOpen
	}
}
