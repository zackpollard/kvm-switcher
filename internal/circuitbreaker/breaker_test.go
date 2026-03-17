package circuitbreaker

import (
	"testing"
	"time"
)

func TestBreaker_StartsClosedAndAllows(t *testing.T) {
	b := New(3, 60*time.Second)

	if s := b.State(); s != StateClosed {
		t.Errorf("initial state = %q, want closed", s)
	}
	if !b.Allow() {
		t.Error("Allow should return true when closed")
	}
}

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	b := New(3, 60*time.Second)

	b.RecordFailure()
	b.RecordFailure()
	if s := b.State(); s != StateClosed {
		t.Errorf("state after 2 failures = %q, want closed", s)
	}

	b.RecordFailure()
	if s := b.State(); s != StateOpen {
		t.Errorf("state after 3 failures = %q, want open", s)
	}

	if b.Allow() {
		t.Error("Allow should return false when open")
	}
}

func TestBreaker_TransitionsToHalfOpen(t *testing.T) {
	b := New(3, 10*time.Millisecond)

	// Open the circuit
	for i := 0; i < 3; i++ {
		b.RecordFailure()
	}
	if s := b.State(); s != StateOpen {
		t.Fatalf("state = %q, want open", s)
	}

	// Wait for reset timeout
	time.Sleep(15 * time.Millisecond)

	if s := b.State(); s != StateHalfOpen {
		t.Errorf("state after timeout = %q, want half-open", s)
	}
	if !b.Allow() {
		t.Error("Allow should return true when half-open")
	}
}

func TestBreaker_SuccessResetsToClose(t *testing.T) {
	b := New(3, 10*time.Millisecond)

	// Open the circuit
	for i := 0; i < 3; i++ {
		b.RecordFailure()
	}

	// Wait for half-open
	time.Sleep(15 * time.Millisecond)
	b.State() // trigger transition

	// Success should close it
	b.RecordSuccess()
	if s := b.State(); s != StateClosed {
		t.Errorf("state after success = %q, want closed", s)
	}
}

func TestBreaker_FailureInHalfOpenReopens(t *testing.T) {
	b := New(1, 10*time.Millisecond)

	b.RecordFailure() // opens immediately (threshold=1)
	time.Sleep(15 * time.Millisecond)

	if s := b.State(); s != StateHalfOpen {
		t.Fatalf("state = %q, want half-open", s)
	}

	b.RecordFailure() // should reopen
	if s := b.State(); s != StateOpen {
		t.Errorf("state after failure in half-open = %q, want open", s)
	}
}

func TestBreaker_SuccessResetsFailureCount(t *testing.T) {
	b := New(3, 60*time.Second)

	b.RecordFailure()
	b.RecordFailure()
	b.RecordSuccess() // resets

	// Should need 3 more failures to open
	b.RecordFailure()
	b.RecordFailure()
	if s := b.State(); s != StateClosed {
		t.Errorf("state = %q, want closed (counter was reset)", s)
	}
}
