package auth

import (
	"errors"
	"testing"
)

// The bound is a hard ceiling, not a queue: over it, callers are refused rather
// than parked, because parking requests in front of a deliberately slow and
// memory-hungry function is the same out-of-memory risk with extra latency.
func TestLimiterRefusesRatherThanQueues(t *testing.T) {
	l := NewLimiter(2)

	if !l.Acquire() || !l.Acquire() {
		t.Fatal("a two-slot limiter refused its first two callers")
	}
	if l.Free() != 0 {
		t.Errorf("Free() = %d with both slots taken, want 0", l.Free())
	}
	if l.Acquire() {
		t.Fatal("a third caller got a slot from a two-slot limiter")
	}

	// A refused caller must not have consumed anything: releasing the two real
	// holders has to restore the full bound.
	l.Release()
	l.Release()
	if l.Free() != 2 {
		t.Errorf("Free() = %d after both holders released, want 2", l.Free())
	}
}

func TestLimiterHashAndVerifyReportBusy(t *testing.T) {
	l := NewLimiter(1)

	phc, err := l.Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash on a free limiter: %v", err)
	}
	ok, err := l.Verify(phc, "correct horse battery staple")
	if err != nil || !ok {
		t.Fatalf("Verify on a free limiter: ok=%v err=%v", ok, err)
	}

	// With the only slot held, both operations refuse.
	if !l.Acquire() {
		t.Fatal("could not take the single slot")
	}
	if _, err := l.Hash("anything"); !errors.Is(err, ErrBusy) {
		t.Errorf("Hash on a full limiter: %v, want ErrBusy", err)
	}
	if _, err := l.Verify(phc, "anything"); !errors.Is(err, ErrBusy) {
		t.Errorf("Verify on a full limiter: %v, want ErrBusy", err)
	}
	l.Release()

	if _, err := l.Hash("anything"); err != nil {
		t.Errorf("Hash after the slot was released: %v", err)
	}
}

// A zero from an unset config must never mean "admit nothing" -- that would take
// the whole auth surface down on a blank environment variable.
func TestLimiterClampsAnUnsetBound(t *testing.T) {
	for _, n := range []int{0, -1} {
		l := NewLimiter(n)
		if got := l.Free(); got != DefaultArgon2Concurrency {
			t.Errorf("NewLimiter(%d) admits %d at once, want the default %d",
				n, got, DefaultArgon2Concurrency)
		}
		if _, err := l.Hash("x"); err != nil {
			t.Errorf("NewLimiter(%d) refused the first caller: %v", n, err)
		}
	}
}
