package testutil

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"
)

// Time control.
//
// Every durable deadline Drive has -- session expiry, email-token expiry,
// throttle windows, upload-session sliding expiry, blob grace, trash retention
// -- is a timestamptz in Postgres compared against now(). So a test moves time
// by moving the row, never by sleeping: the whole suite runs in seconds and a
// 30-day expiry is as testable as a 30-second one.
//
// The one thing that is not a stored timestamp is the GC's own schedule, which
// is why the collector exports a single-pass entry point -- see RunGCOnce
// below.

// identifier guards the table and column names Backdate interpolates. They are
// never caller data in practice, but a typo that turned into SQL would be an
// unpleasant way to find out.
var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Backdate subtracts d from a timestamp column for every row matching where,
// and returns how many rows moved.
//
// where is a SQL predicate with $1, $2, ... placeholders, e.g.
//
//	Backdate(t, pool, "auth_sessions", "expires_at", 40*24*time.Hour, "user_id = $1", userID)
//
// which expires a 30-day session without waiting 30 days.
func Backdate(t testing.TB, pool queryExecer, table, column string, d time.Duration, where string, args ...any) int64 {
	t.Helper()
	if !identifier.MatchString(table) || !identifier.MatchString(column) {
		t.Fatalf("testutil: Backdate: %q.%q is not a plain identifier", table, column)
	}
	sql := fmt.Sprintf(
		`UPDATE %s SET %s = %s - make_interval(secs => $%d) WHERE %s`,
		table, column, column, len(args)+1, where)

	tag, err := pool.Exec(context.Background(), sql, append(args, d.Seconds())...)
	if err != nil {
		t.Fatalf("testutil: backdating %s.%s: %v", table, column, err)
	}
	return tag.RowsAffected()
}

// ExpireSessions pushes every auth session of a user past its expiry, so the
// next request with that cookie is anonymous. The 30-day sliding TTL means the
// shift has to clear it comfortably.
func ExpireSessions(t testing.TB, pool queryExecer, userID any) {
	t.Helper()
	if n := Backdate(t, pool, "auth_sessions", "expires_at", 31*24*time.Hour, "user_id = $1", userID); n == 0 {
		t.Fatalf("testutil: ExpireSessions: no auth_sessions rows for %v", userID)
	}
}

// LapseThrottleWindow moves a throttle budget's window into the past, which is
// how a test gets past a durable lockout (login, OTP, share password) without
// sleeping the window out. The scope and key are the ones stored in the
// throttle table, e.g. ("login", "someone@drive.test").
func LapseThrottleWindow(t testing.TB, pool queryExecer, scope, key string, d time.Duration) {
	t.Helper()
	if n := Backdate(t, pool, "throttle", "window_start", d, "scope = $1 AND key = $2", scope, key); n == 0 {
		t.Fatalf("testutil: LapseThrottleWindow: no throttle row for scope=%q key=%q", scope, key)
	}
}

// RunGCOnce is the GC loop's single synchronous pass, so a suite triggers
// collection on demand instead of waiting out the hourly schedule.
//
// Phase 2 builds the loop in internal/gc and the integration suite's TestMain
// assigns it here:
//
//	testutil.RunGCOnce = func(ctx context.Context) error { return gc.RunOnce(ctx, deps) }
//
// It is a variable rather than a direct call so this package does not depend on
// a package that does not exist yet, and so the wiring stays one line.
var RunGCOnce func(context.Context) error

// GC runs one garbage-collection pass. It fails the test with a specific
// message rather than silently doing nothing if the hook was never wired.
func GC(t testing.TB, ctx context.Context) {
	t.Helper()
	if RunGCOnce == nil {
		t.Fatal("testutil.GC: RunGCOnce is not wired -- Phase 2 must assign testutil.RunGCOnce in the suite's TestMain")
	}
	if err := RunGCOnce(ctx); err != nil {
		t.Fatalf("testutil.GC: %v", err)
	}
}
