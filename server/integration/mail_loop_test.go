package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSignupVerifyLoginThroughRealMail walks acceptance step 1 the way a person
// does: sign up, read the link out of the inbox, verify, sign in.
//
// It exists because the rest of the suite deliberately skips the mail loop --
// testutil.NewUser stamps email_verified_at with SQL -- and that blind spot let
// the server binary ship with no mail sender wired at all: every signup logged
// "no mail sender configured", returned 200, and left an account that could
// never be verified or used. Nothing in the suite noticed. Playwright covers
// this too, but that is two phases away, so the guard lives
// here as well: this test talks to the real binary over HTTP and the real
// Mailpit, so it fails the moment the wiring regresses.
func TestSignupVerifyLoginThroughRealMail(t *testing.T) {
	ctx := context.Background()
	anon := H.Anonymous(t)
	// Lowercase: users.email is citext and the API canonicalises, so /me echoes
	// the folded form back.
	email := strings.ToLower("mailloop-" + time.Now().Format("150405.000000000") + "@drive.test")
	since := time.Now().Add(-time.Minute)

	anon.Post(t, "/api/auth/signup", map[string]any{
		"email":        email,
		"password":     "drive-demo-1",
		"display_name": "Mail Loop",
	}).Expect(http.StatusOK)

	// Login must be refused until the address is verified.
	anon.Post(t, "/api/auth/login", map[string]any{
		"email": email, "password": "drive-demo-1",
	}).Expect(http.StatusUnauthorized)

	// The send is dispatched off the request goroutine on purpose (it would
	// otherwise be an account-existence timing oracle), so wait for it.
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	msg, err := H.Mailpit.WaitForLatestTo(waitCtx, email, since)
	if err != nil {
		t.Fatalf("no verification mail reached Mailpit for %s: %v", email, err)
	}

	if want := "Verify your Drive account"; msg.Subject != want {
		t.Errorf("subject = %q, want %q (the verification subject is fixed verbatim -- changing it breaks anyone filtering on it)", msg.Subject, want)
	}

	token := verifyTokenFrom(t, msg.Text)
	anon.Post(t, "/api/auth/verify-email", map[string]any{"token": token}).Expect(http.StatusOK)
	anon.Post(t, "/api/auth/login", map[string]any{
		"email": email, "password": "drive-demo-1",
	}).Expect(http.StatusOK)

	var me struct {
		Email           string `json:"email"`
		RootID          string `json:"root_id"`
		EmailVerifiedAt string `json:"email_verified_at"`
	}
	anon.Get(t, "/api/auth/me").Expect(http.StatusOK).JSON(&me)
	if me.Email != email {
		t.Errorf("me.email = %q, want %q", me.Email, email)
	}
	if me.RootID == "" {
		t.Error("me.root_id is empty: signup did not create the user's root folder")
	}
	if me.EmailVerifiedAt == "" {
		t.Error("me.email_verified_at is empty after a successful verify")
	}
}

// verifyTokenFrom pulls the raw token out of the verification mail's
// ${DRIVE_BASE_URL}/verify?token=<raw> link. That path and query name are fixed:
// the SPA route /verify reads the token from there and posts it back.
func verifyTokenFrom(t *testing.T, body string) string {
	t.Helper()
	const marker = "/verify?token="
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no %q link in the mail body:\n%s", marker, body)
	}
	token := body[i+len(marker):]
	if j := strings.IndexAny(token, "\r\n \t\"'<>"); j >= 0 {
		token = token[:j]
	}
	if token == "" {
		t.Fatalf("empty verification token in:\n%s", body)
	}
	return token
}
