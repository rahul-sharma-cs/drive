package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/testutil"
)

// An account that has not confirmed its address cannot sign in -- asserted end
// to end over HTTP against the real binary, not against the auth package.
//
// The account exists and the password is right; only email_verified_at is
// missing. So this also pins the order the login handler checks things in: the
// distinct "verify your email" answer is only ever shown to somebody who
// already proved they know the password, and it never charges the lockout
// budget.
//
// It carries its own code rather than the generic one, because it is the single
// login refusal the client acts on -- it offers to resend the link -- and a
// client matching on the English would break the next time the wording moved.
func TestUnverifiedEmailCannotLogIn(t *testing.T) {
	user := H.NewUnverifiedUser(t)

	resp := user.Post(t, "/api/auth/login", map[string]any{
		"email":    user.Email,
		"password": testutil.FixturePassword,
	}).Expect(http.StatusUnauthorized)

	if resp.Code() != "email_unverified" {
		t.Errorf("code = %q, want email_unverified", resp.Code())
	}
	if !strings.Contains(strings.ToLower(resp.Message()), "verify your email") {
		t.Errorf("message = %q, want it to name email verification", resp.Message())
	}

	// No session was minted: the request that failed left nothing behind.
	if n := H.CountRows(t, "auth_sessions",
		"user_id = (SELECT id FROM users WHERE email = $1)", user.Email); n != 0 {
		t.Errorf("%d auth session(s) exist for an unverified account, want 0", n)
	}
	user.Get(t, "/api/auth/me").Expect(http.StatusUnauthorized)

	// Confirming the address is the only thing that was missing.
	H.MarkVerified(t, user.Email)
	user.Login(t)
	user.Get(t, "/api/auth/me").Expect(http.StatusOK)
}

// Signup creates the account and exactly one root folder, which is what /me
// returns as root_id and what the file browser opens on.
func TestSignupCreatesExactlyOneRootFolder(t *testing.T) {
	user := H.NewUser(t)

	var me struct {
		ID     string `json:"id"`
		RootID string `json:"root_id"`
	}
	user.Get(t, "/api/auth/me").Expect(http.StatusOK).JSON(&me)
	if me.RootID != user.RootID.String() {
		t.Errorf("/me root_id = %s, want %s", me.RootID, user.RootID)
	}

	if n := H.CountRows(t, "nodes", "owner_id = $1 AND parent_id IS NULL", user.ID); n != 1 {
		t.Errorf("%d root folder(s) for the new account, want 1", n)
	}

	// The root is not a normal node: it cannot be trashed.
	user.Delete(t, "/api/nodes/"+user.RootID.String()).Expect(http.StatusUnprocessableEntity)
}

// Logging out revokes the session server-side, not just in the browser: the
// same cookie replayed afterwards is anonymous.
func TestLogoutRevokesTheSessionServerSide(t *testing.T) {
	user := H.NewUser(t)
	user.Get(t, "/api/auth/me").Expect(http.StatusOK)

	var before int
	if err := H.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM auth_sessions WHERE user_id = $1`, user.ID).Scan(&before); err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	if before != 1 {
		t.Fatalf("%d session(s) before logout, want 1", before)
	}

	user.Post(t, "/api/auth/logout", nil).Expect(http.StatusNoContent)

	if n := H.CountRows(t, "auth_sessions", "user_id = $1", user.ID); n != 0 {
		t.Errorf("%d session(s) survive logout, want 0", n)
	}
	user.Get(t, "/api/auth/me").Expect(http.StatusUnauthorized)
}

// -------------------------------------------------------- abuse controls --
//
// The two limits below are configured out of the way for the whole suite:
// .env.test raises DRIVE_AUTH_RATE_PER_MIN and DRIVE_MAIL_RATE_PER_HOUR to
// 100000 (every test runs from one address, so real allowances would refuse the
// run itself) and sets DRIVE_EMAIL_DAILY_CAP=0 (Mailpit has no vendor quota to
// protect). internal/api covers the behaviour with hand-built configs, but that
// leaves the wiring dark end to end: nothing proved that these variables reach
// the limiter at all in the real binary. A variable typo'd here would look
// exactly like a limit nobody hit.
//
// Each test gets its own server carrying its own value, so the suite's generous
// limits are untouched for everybody else.

// DRIVE_AUTH_RATE_PER_MIN reaches the per-IP bucket in a real process.
func TestAuthRateLimitIsReadFromTheEnvironment(t *testing.T) {
	child := H.SpawnServerWithEnv(t, "DRIVE_AUTH_RATE_PER_MIN=1")
	anon := H.Anonymous(t).At(child.URL)

	// One a minute, burst twice the rate: two requests pass, the third does
	// not. verify-email with a junk token is the cheapest thing on the
	// unauthenticated surface -- what is being measured is the bucket, not a
	// handler.
	spend := func() *testutil.Resp {
		return anon.Post(t, "/api/auth/verify-email", map[string]any{"token": "nope"})
	}
	for i := 1; i <= 2; i++ {
		if resp := spend(); resp.Status == http.StatusTooManyRequests {
			t.Fatalf("request %d of the burst was refused; the burst is twice the rate", i)
		}
	}
	refused := spend().Expect(http.StatusTooManyRequests)
	if refused.Code() != "rate_limited" {
		t.Errorf("code = %q, want rate_limited", refused.Code())
	}
}

// DRIVE_EMAIL_DAILY_CAP reaches the service-wide send budget in a real process.
//
// This is the budget that protects a vendor quota: once it is spent, nobody can
// verify an address until the window rolls. It is charged before the decision to
// send, so a suppressed attempt still counts -- which is the direction to be
// wrong in when the alternative is a quota that takes verification mail down for
// everyone.
func TestServiceWideMailBudgetIsReadFromTheEnvironment(t *testing.T) {
	ctx := context.Background()

	// One row for the whole service, so this test owns the scope rather than a
	// key of its own and clears it before counting.
	if _, err := H.Pool.Exec(ctx, `DELETE FROM throttle WHERE scope = $1`, auth.ScopeEmailSendGlobal); err != nil {
		t.Fatalf("clearing the global mail budget: %v", err)
	}

	const dailyCap = 2
	child := H.SpawnServerWithEnv(t, "DRIVE_EMAIL_DAILY_CAP=2")
	anon := H.Anonymous(t).At(child.URL)

	const attempts = dailyCap + 1
	var addresses []string
	for i := 0; i < attempts; i++ {
		email := fmt.Sprintf("cap%d-%d@drive.test", time.Now().UnixNano(), i)
		addresses = append(addresses, email)
		// Every signup succeeds: a suppressed message must tell the caller
		// nothing, and the account exists either way.
		anon.Post(t, "/api/auth/signup", map[string]any{
			"email":        email,
			"password":     testutil.FixturePassword,
			"display_name": "Budget Test",
		}).Expect(http.StatusOK)
	}

	// The sends are dispatched off the request goroutine, so wait on the budget
	// rather than on a clock. Charge-first means all three attempts are counted
	// whether or not anything went out, which is also what makes the mail
	// assertion below safe to make: once the count is in, every attempt is done.
	deadline := time.Now().Add(10 * time.Second)
	for {
		n, err := auth.Count(ctx, H.Pool, auth.ScopeEmailSendGlobal, auth.GlobalKey, auth.EmailSendGlobalWindow)
		if err != nil {
			t.Fatalf("reading the global mail budget: %v", err)
		}
		if n == attempts {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the global budget counted %d of %d attempts -- DRIVE_EMAIL_DAILY_CAP never reached the sender", n, attempts)
		}
		time.Sleep(20 * time.Millisecond)
	}

	sent := 0
	for i, email := range addresses {
		msg, err := H.Mailpit.LatestTo(ctx, email)
		if err == nil && msg != nil {
			sent++
			continue
		}
		if i < dailyCap {
			t.Errorf("no verification mail for %s, which was inside the cap of %d", email, dailyCap)
		}
	}
	if sent != dailyCap {
		t.Errorf("%d messages went out under a cap of %d", sent, dailyCap)
	}
}
