package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/rahul-sharma-cs/drive/server/internal/testutil"
)

// An account that has not confirmed its address cannot sign in -- asserted end
// to end over HTTP against the real binary, not against the auth package.
//
// The account exists and the password is right; only email_verified_at is
// missing. So this also pins the order the login handler checks things in: the
// distinct "verify your email" message is only ever shown to somebody who
// already proved they know the password, and it never charges the lockout
// budget.
func TestUnverifiedEmailCannotLogIn(t *testing.T) {
	user := H.NewUnverifiedUser(t)

	resp := user.Post(t, "/api/auth/login", map[string]any{
		"email":    user.Email,
		"password": testutil.FixturePassword,
	}).Expect(http.StatusUnauthorized)

	if resp.Code() != "unauthorized" {
		t.Errorf("code = %q, want unauthorized", resp.Code())
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
