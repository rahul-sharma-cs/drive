package auth

import (
	"context"
	"fmt"
	"time"
)

// Throttle scopes. Every durable budget in Drive is a (scope, key) pair in the
// throttle table. Any later budget -- OTP sends, per-token limits -- adds its
// own scope here.
const (
	// ScopeLogin counts failed sign-ins, keyed by the submitted email.
	ScopeLogin = "login"
	// ScopePasswordChange counts wrong current-password guesses on an
	// authenticated password change, keyed by USER ID.
	//
	// It is deliberately not ScopeLogin. That budget is keyed by email, so
	// charging it here would let ten mistyped current passwords -- or anyone who
	// knows the address and has stolen a session -- lock the account out of
	// signing in for fifteen minutes. Different key, different budget, same
	// numbers.
	ScopePasswordChange = "password_change"
	// ScopeSharePassword counts wrong passwords on a share link, keyed by
	// share_id:ip -- the share alone would let anyone holding the link lock
	// its real recipient out.
	ScopeSharePassword = "share_password"
	// ScopeEmailSend counts outbound mail, keyed by recipient address. It is
	// signup's budget; the two purposes below have their own.
	ScopeEmailSend = "email_send"
	// ScopeEmailSendReset and ScopeEmailSendVerify are the per-recipient send
	// budgets for the two paths anybody can trigger against an address they do
	// not own: password-reset and resend-verification.
	//
	// They are separate scopes rather than one, because sharing a budget makes
	// each path a way to suppress the other: five reset requests against a
	// stranger's address would otherwise swallow the verification mail they are
	// waiting for, and vice versa.
	ScopeEmailSendReset  = "email_send:reset"
	ScopeEmailSendVerify = "email_send:verify"
	// ScopeEmailSendGlobal counts ALL outbound mail, service-wide. ScopeEmailSend
	// is per-recipient and so cannot protect the sending account's own daily
	// quota: a thousand addresses each under their personal budget still burn
	// through it in one afternoon, and a spent vendor quota takes verification
	// mail down for real users.
	ScopeEmailSendGlobal = "email_send_global"
)

// GlobalKey is the throttle key for budgets that are not keyed by anybody --
// one row per window for the whole service.
const GlobalKey = "all"

// The durable budgets. Both are auto-clearing: the window is measured
// against now(), so a lapsed window simply stops counting -- there is no
// permanent lockout and nothing has to reset anything.
const (
	LoginFailLimit  = 10
	LoginFailWindow = 15 * time.Minute

	// The same allowance for a wrong current password on an authenticated
	// change. Spending it costs the caller nothing but the change: they stay
	// signed in and can still log in elsewhere, which is exactly why it is not
	// the login budget.
	PasswordChangeFailLimit  = 10
	PasswordChangeFailWindow = 15 * time.Minute

	// The same allowance again for wrong passwords on a share link, spent per
	// (share, address): the address is in the key so a stranger holding a
	// public link cannot lock its real recipient out, and the share is in the
	// key so guessing at one link does not lock every link.
	SharePasswordFailLimit  = 10
	SharePasswordFailWindow = 15 * time.Minute

	EmailSendLimit  = 5
	EmailSendWindow = time.Hour

	// EmailSendGlobalWindow is the service-wide budget's rolling window. It is
	// exactly the collector's throttle retention, so a row stops counting at the
	// same moment the collector becomes free to delete it -- a shorter retention
	// would silently refund budget mid-window.
	EmailSendGlobalWindow = 24 * time.Hour
)

// Count returns how many events are recorded for (scope, key) inside the last
// window. It is the "may I proceed" question: callers compare it to the limit
// before doing the work, so a budget already spent blocks even a request that
// would have succeeded.
func Count(ctx context.Context, q Querier, scope, key string, window time.Duration) (int, error) {
	const sql = `
		SELECT COALESCE(sum(count), 0)::int
		  FROM throttle
		 WHERE scope = $1 AND key = $2
		   AND window_start > now() - make_interval(secs => $3)`

	var n int
	if err := q.QueryRow(ctx, sql, scope, key, window.Seconds()).Scan(&n); err != nil {
		return 0, fmt.Errorf("auth: counting throttle %s/%s: %w", scope, key, err)
	}
	return n, nil
}

// Bump records one event against (scope, key) and returns the new count for the
// active window.
//
// The window is anchored to its first event rather than to a wall-clock grid,
// so a burst of failures can never straddle a boundary and silently halve
// itself. Concurrency safety comes from the upsert: racing callers either
// contend on one row (INSERT ... ON CONFLICT DO UPDATE serialises them) or
// each open a row of their own, and Count sums every row still inside the
// window -- no increment is lost either way.
func Bump(ctx context.Context, q Querier, scope, key string, window time.Duration) (int, error) {
	const sql = `
		WITH anchor AS (
			SELECT window_start
			  FROM throttle
			 WHERE scope = $1 AND key = $2
			   AND window_start > now() - make_interval(secs => $3)
			 ORDER BY window_start
			 LIMIT 1
		)
		INSERT INTO throttle (scope, key, window_start, count)
		VALUES ($1, $2, COALESCE((SELECT window_start FROM anchor), now()), 1)
		ON CONFLICT (scope, key, window_start) DO UPDATE SET count = throttle.count + 1`

	if _, err := q.Exec(ctx, sql, scope, key, window.Seconds()); err != nil {
		return 0, fmt.Errorf("auth: bumping throttle %s/%s: %w", scope, key, err)
	}
	// A statement cannot read its own write, so the total is a second query.
	return Count(ctx, q, scope, key, window)
}

// Allowed reports whether another event fits in the budget for (scope, key).
func Allowed(ctx context.Context, q Querier, scope, key string, limit int, window time.Duration) (bool, error) {
	n, err := Count(ctx, q, scope, key, window)
	if err != nil {
		return false, err
	}
	return n < limit, nil
}
