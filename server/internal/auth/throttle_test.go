package auth

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testWindow = 15 * time.Minute

// backdateThrottle ages every window row for (scope, key) by d. This is the
// time-control convention throughout the suite: durable time state lives in
// Postgres and every comparison is now() vs a stored value, so tests move the
// stored value instead of sleeping a wall-clock window.
func backdateThrottle(t *testing.T, pool *pgxpool.Pool, scope, key string, d time.Duration) {
	t.Helper()
	tag, err := pool.Exec(t.Context(),
		`UPDATE throttle SET window_start = window_start - make_interval(secs => $3) WHERE scope = $1 AND key = $2`,
		scope, key, d.Seconds())
	if err != nil {
		t.Fatalf("backdating throttle: %v", err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatalf("backdating throttle: no rows for scope %q key %q", scope, key)
	}
}

func TestBumpCountsInsideTheWindow(t *testing.T) {
	pool := testPool(t)
	key := uuid.NewString()

	n, err := Count(t.Context(), pool, ScopeLogin, key, testWindow)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Fatalf("a fresh key starts at %d, want 0", n)
	}

	for want := 1; want <= 3; want++ {
		got, err := Bump(t.Context(), pool, ScopeLogin, key, testWindow)
		if err != nil {
			t.Fatalf("Bump: %v", err)
		}
		if got != want {
			t.Errorf("Bump #%d returned %d, want %d", want, got, want)
		}
	}

	n, err = Count(t.Context(), pool, ScopeLogin, key, testWindow)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("Count after three bumps = %d, want 3", n)
	}
}

// The window is what makes the lockout auto-clearing: once its rows age out,
// the budget is spent from zero again. No permanent lockout.
func TestCountIgnoresWindowsThatHaveLapsed(t *testing.T) {
	pool := testPool(t)
	key := uuid.NewString()

	for range 10 {
		if _, err := Bump(t.Context(), pool, ScopeLogin, key, testWindow); err != nil {
			t.Fatalf("Bump: %v", err)
		}
	}
	backdateThrottle(t, pool, ScopeLogin, key, testWindow+time.Minute)

	n, err := Count(t.Context(), pool, ScopeLogin, key, testWindow)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Errorf("Count after the window lapsed = %d, want 0", n)
	}

	// A bump after the lapse starts a new window rather than resurrecting the
	// old one.
	got, err := Bump(t.Context(), pool, ScopeLogin, key, testWindow)
	if err != nil {
		t.Fatalf("Bump: %v", err)
	}
	if got != 1 {
		t.Errorf("first Bump in the new window returned %d, want 1", got)
	}
}

// Two tabs, two requests, one budget: no increment may be lost.
func TestBumpLosesNoIncrementUnderConcurrency(t *testing.T) {
	pool := testPool(t)
	key := uuid.NewString()

	const n = 25
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Bump(t.Context(), pool, ScopeLogin, key, testWindow); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Bump: %v", err)
	}

	got, err := Count(t.Context(), pool, ScopeLogin, key, testWindow)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != n {
		t.Errorf("Count after %d concurrent bumps = %d, want %d", n, got, n)
	}
}

func TestBudgetsAreIndependentPerScopeAndKey(t *testing.T) {
	pool := testPool(t)
	a, b := uuid.NewString(), uuid.NewString()

	if _, err := Bump(t.Context(), pool, ScopeLogin, a, testWindow); err != nil {
		t.Fatalf("Bump: %v", err)
	}

	for _, c := range []struct{ scope, key string }{
		{ScopeLogin, b},
		{ScopeEmailSend, a},
	} {
		n, err := Count(t.Context(), pool, c.scope, c.key, testWindow)
		if err != nil {
			t.Fatalf("Count(%s, %s): %v", c.scope, c.key, err)
		}
		if n != 0 {
			t.Errorf("Count(%s, %s) = %d, want 0 -- budgets bled across scope/key", c.scope, c.key, n)
		}
	}
}
