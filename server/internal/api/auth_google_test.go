package api

// Sign in with Google, and the thing it forces on the rest of the auth surface:
// an account whose password_hash is NULL.
//
// The nullable-password cases come first and are deliberately not driven
// through the OIDC flow. What they are testing is that every path which reads a
// password hash survives its absence, and the cheapest honest way to put an
// account in that state is to write one -- which is also what makes them
// readable when they fail.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
)

// ------------------------------------------------------- password-less user --

// googleOnlyAccount is an account exactly as a Google sign-in leaves it: no
// password, a verified address, a root folder. It returns the address and a
// live session cookie for it.
func googleOnlyAccount(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, string, *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	email := "google-" + uuid.NewString() + "@drive.test"
	userID := uuid.New()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx,
		`INSERT INTO users (id, email, password_hash, display_name, email_verified_at)
		 VALUES ($1, $2, NULL, 'Google User', now())`, userID, email); err != nil {
		t.Fatalf("inserting the password-less user: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		 VALUES ($1, $2, NULL, 'folder', $3)`, uuid.New(), userID, auth.RootFolderName); err != nil {
		t.Fatalf("inserting the root folder: %v", err)
	}
	raw, _, err := auth.CreateSession(ctx, tx, userID, "", "")
	if err != nil {
		t.Fatalf("creating the session: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	return userID, email, &http.Cookie{Name: SessionCookie, Value: raw}
}

// A password sign-in against an account that has no password is a wrong
// password, not a server error.
//
// This is the whole reason auth.Account.PasswordHash is a *string. Coalescing
// the NULL to "" would not fail the comparison -- it would fail to parse, which
// is ErrBadHash, which authLogin answers with a 500. That is a bug on its own
// and an account-existence oracle on top of it: 500 for every address that
// signs in with Google and 401 for every other address is a lookup anybody can
// run.
func TestPasswordLoginAgainstAGoogleOnlyAccountIsTheGeneric401(t *testing.T) {
	h, _, pool := authTestServer(t)
	_, email, _ := googleOnlyAccount(t, pool)

	rec := authDo(t, h, http.MethodPost, "/api/auth/login",
		authLoginBody{email, authTestPassword}, nil)
	nodeWant(t, rec, http.StatusUnauthorized, CodeUnauthorized)

	// Byte-identical to what an address with no account at all is told.
	unknown := authDo(t, h, http.MethodPost, "/api/auth/login",
		authLoginBody{authTestEmail(t), authTestPassword}, nil)
	nodeWant(t, unknown, http.StatusUnauthorized, CodeUnauthorized)
	if got, want := rec.Body.String(), unknown.Body.String(); got != want {
		t.Errorf("body for a Google-only account = %s, want the unknown-address body %s", got, want)
	}

	// And the attempt was charged to the login budget, exactly as an unknown
	// address is: a path that failed early enough to skip the charge would be
	// an unmetered way to probe addresses.
	if got := loginBudgetCount(t, pool, email); got != 1 {
		t.Errorf("login budget for a Google-only account = %d, want 1", got)
	}
}

// loginBudgetCount sums the failed-login counter for an address.
func loginBudgetCount(t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(count), 0) FROM throttle WHERE scope = $1 AND key = $2`,
		auth.ScopeLogin, email).Scan(&n)
	if err != nil {
		t.Fatalf("reading the login budget: %v", err)
	}
	return n
}

// Changing a password you have never had is refused before Argon2, with its own
// code, and with the one message that says what to do instead.
//
// It is not an oracle: the caller is authenticated as themselves and is being
// told a fact about their own account.
func TestChangePasswordOnAGoogleOnlyAccountIsRefusedBeforeArgon2(t *testing.T) {
	s, h, _, pool := abuseServer(t)
	userID, _, cookie := googleOnlyAccount(t, pool)

	// One Argon2 slot, and it is taken. Anything that reaches the limiter from
	// here answers 429; a 409 is proof it never got that far.
	s.Argon2 = auth.NewLimiter(1)
	if !s.Argon2.Acquire() {
		t.Fatal("could not take the single Argon2 slot")
	}
	defer s.Argon2.Release()

	rec := authDo(t, h, http.MethodPost, "/api/auth/password",
		authChangePasswordBody{"whatever they type", "a completely different passphrase"}, cookie)
	nodeWant(t, rec, http.StatusConflict, CodeUnsupported)

	// The refusal costs the caller nothing durable either: this is not a wrong
	// guess, so the password-change budget is untouched.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(count), 0) FROM throttle WHERE scope = $1 AND key = $2`,
		auth.ScopePasswordChange, userID.String()).Scan(&n); err != nil {
		t.Fatalf("reading the password-change budget: %v", err)
	}
	if n != 0 {
		t.Errorf("password-change budget = %d, want 0 -- a refusal is not a wrong guess", n)
	}
}

// has_password is what the account screen splits on, and it has to be right in
// both places a client can learn it: GET /me, and the body of the login that
// seeded the client's cache in the first place.
//
// The login half is the one that bites. The SPA caches that body with
// staleTime: Infinity and never refetches, so a field present on /me and absent
// from login is a field the whole session is wrong about -- every password user
// would be shown the "you sign in with Google" body until they hard-refreshed.
func TestHasPasswordRidesBothMeAndTheLoginBody(t *testing.T) {
	h, sender, pool := authTestServer(t)

	email, cookie := authSignedIn(t, h, sender)
	rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the login body: %v", err)
	}
	if got, ok := body["has_password"]; !ok || got != true {
		t.Errorf("login body has_password = %v (present: %t), want true -- the body is %s",
			got, ok, rec.Body.String())
	}
	if got := meHasPassword(t, h, cookie); !got {
		t.Error("GET /me has_password = false for an account with a password")
	}

	_, _, googleCookie := googleOnlyAccount(t, pool)
	if got := meHasPassword(t, h, googleCookie); got {
		t.Error("GET /me has_password = true for an account whose password_hash is NULL")
	}
}

// meHasPassword reads the flag off GET /auth/me.
func meHasPassword(t *testing.T, h http.Handler, cookie *http.Cookie) bool {
	t.Helper()
	rec := authDo(t, h, http.MethodGet, "/api/auth/me", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me: status %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		HasPassword *bool `json:"has_password"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding /me: %v", err)
	}
	if body.HasPassword == nil {
		t.Fatalf("GET /me carries no has_password field: %s", rec.Body.String())
	}
	return *body.HasPassword
}

// ------------------------------------------------------------- /providers ----

// Which sign-in methods exist is a static fact about the deployment, and the
// sign-in screen asks for it before it can render. Putting it behind the per-IP
// bucket would mean a caller who has just spent their allowance is served a
// screen missing half its buttons -- so the assertion is made from an address
// whose bucket is empty.
func TestProvidersReportsTheGoogleClientAndIsNotBucketed(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"unconfigured", &config.Config{BaseURL: authTestBaseURL}, false},
		{"configured", &config.Config{
			BaseURL:            authTestBaseURL,
			GoogleClientID:     "drive-test-client-0001",
			GoogleClientSecret: "drivetestsecret0001",
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, _ := authTestServerWithConfig(t, c.cfg)

			// Empty the bucket first, on the address this test speaks from.
			// .env.test raises the allowance to 100000, so the config above --
			// which carries no rate at all and therefore takes the default --
			// is what makes this reachable.
			for range int(burstFor(DefaultAuthRatePerMin)) + 1 {
				authDo(t, h, http.MethodPost, "/api/auth/verify-email", map[string]string{"token": "nope"}, nil)
			}
			if got := authDo(t, h, http.MethodPost, "/api/auth/verify-email",
				map[string]string{"token": "nope"}, nil).Code; got != http.StatusTooManyRequests {
				t.Fatalf("the bucket is not empty: verify-email answered %d, want 429", got)
			}

			rec := authDo(t, h, http.MethodGet, "/api/auth/providers", nil, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /providers with an empty bucket: status %d, body %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Google *bool `json:"google"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding /providers: %v", err)
			}
			if body.Google == nil {
				t.Fatalf("/providers carries no google field: %s", rec.Body.String())
			}
			if *body.Google != c.want {
				t.Errorf("google = %t, want %t", *body.Google, c.want)
			}
		})
	}
}
