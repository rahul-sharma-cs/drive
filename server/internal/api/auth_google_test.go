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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/oidcstub"
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

// ------------------------------------------------------------- the flow ------

// googleTestServer builds a server wired to a fresh fake provider, and hands
// back everything a case here needs to reach into: the Server (for its
// buckets), its routes, the stub (to choose the identity), the pool, and the
// buffer its logs went to.
//
// The config is explicit rather than the environment's, for the reason
// authTestServerWithConfig documents: .env.test raises the bucket's allowance
// to 100000, which would make every assertion about a bucket pass regardless.
// The log level is info -- what a deployment runs at -- so a refusal that is
// only visible at debug counts as invisible.
func googleTestServer(t *testing.T, mut func(*config.Config)) (*Server, http.Handler, *oidcstub.Stub, *pgxpool.Pool, *bytes.Buffer) {
	t.Helper()

	stub, err := oidcstub.New(googleTestClientID, googleTestClientSecret)
	if err != nil {
		t.Fatalf("building the stub provider: %v", err)
	}
	ts := httptest.NewServer(stub.Handler())
	t.Cleanup(ts.Close)
	stub.SetBaseURL(ts.URL)

	cfg := &config.Config{
		BaseURL:            authTestBaseURL,
		GoogleClientID:     googleTestClientID,
		GoogleClientSecret: googleTestClientSecret,
		GoogleIssuer:       ts.URL,
	}
	if mut != nil {
		mut(cfg)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	pool := authTestPool(t)
	s := New(cfg, pool, logger, &authRecordingSender{}, nil, nil)
	return s, s.Routes(), stub, pool, &logs
}

// Fabricated credentials, matching the ones .env.test hands the stub binary.
const (
	googleTestClientID     = "drive-test-client-0001"
	googleTestClientSecret = "drivetestsecret0001"
)

// googleGet issues a browser-shaped GET: no client header (GETs are exempt),
// the test's own peer address, and whatever cookies the caller is carrying.
func googleGet(t *testing.T, h http.Handler, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = testClientAddr(t)
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// cookieNamed pulls one cookie out of a response, or nil.
func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// googleStart runs the first leg and returns the authorization URL and the flow
// cookie the browser would now be holding.
func googleStart(t *testing.T, h http.Handler, query string) (string, *http.Cookie) {
	t.Helper()
	rec := googleGet(t, h, "/api/auth/google/start"+query)
	if rec.Code != http.StatusFound {
		t.Fatalf("/start: status %d, want 302 (body %s)", rec.Code, rec.Body.String())
	}
	flow := cookieNamed(rec, OAuthCookie)
	if flow == nil {
		t.Fatalf("/start set no %s cookie", OAuthCookie)
	}
	return rec.Header().Get("Location"), flow
}

// googleAuthorize is the browser's hop to the provider: it follows nothing and
// reads the code and state back out of the Location the stub sends it to.
func googleAuthorize(t *testing.T, authURL string) (code, state string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatalf("GET the authorization URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("the provider answered %d, want 302", resp.StatusCode)
	}
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parsing the callback URL: %v", err)
	}
	return back.Query().Get("code"), back.Query().Get("state")
}

// googleSignIn walks the whole flow and returns the callback's response.
func googleSignIn(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	authURL, flow := googleStart(t, h, "")
	code, state := googleAuthorize(t, authURL)
	return googleGet(t, h, "/api/auth/google/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), flow)
}

// googleSignedIn walks the flow and insists it worked, returning the session
// cookie it minted.
func googleSignedIn(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := googleSignIn(t, h)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != afterSignIn {
		t.Fatalf("the callback answered %d → %q, want 302 → %q (body %s)",
			rec.Code, rec.Header().Get("Location"), afterSignIn, rec.Body.String())
	}
	session := cookieNamed(rec, SessionCookie)
	if session == nil {
		t.Fatal("a successful callback minted no session cookie")
	}
	return session
}

// googleIdentity is the stub identity for a throwaway address.
func googleIdentity(t *testing.T, subject string) oidcstub.Identity {
	t.Helper()
	return oidcstub.Identity{
		Subject:       subject,
		Email:         "g-" + uuid.NewString() + "@example.test",
		EmailVerified: true,
		Name:          "Google Person",
	}
}

// --------------------------------------------------------------- reading -----

func googleUserRow(t *testing.T, pool *pgxpool.Pool, email string) (id uuid.UUID, hasPassword bool, verified *time.Time, displayName string, found bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT id, password_hash IS NOT NULL, email_verified_at, display_name FROM users WHERE email = $1`,
		email).Scan(&id, &hasPassword, &verified, &displayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil, "", false
	}
	if err != nil {
		t.Fatalf("reading the user row for %s: %v", email, err)
	}
	return id, hasPassword, verified, displayName, true
}

type identityRow struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Subject     string
	EmailAtLink string
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

func googleIdentityRow(t *testing.T, pool *pgxpool.Pool, subject string) (identityRow, bool) {
	t.Helper()
	var i identityRow
	err := pool.QueryRow(context.Background(),
		`SELECT id, user_id, subject, email_at_link::text, created_at, last_login_at
		   FROM user_identities WHERE provider = 'google' AND subject = $1`, subject).
		Scan(&i.ID, &i.UserID, &i.Subject, &i.EmailAtLink, &i.CreatedAt, &i.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return identityRow{}, false
	}
	if err != nil {
		t.Fatalf("reading the identity row for %s: %v", subject, err)
	}
	return i, true
}

func googleCountIdentities(t *testing.T, pool *pgxpool.Pool, subject string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_identities WHERE provider = 'google' AND subject = $1`, subject).Scan(&n); err != nil {
		t.Fatalf("counting identities: %v", err)
	}
	return n
}

func googleCountUserIdentities(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_identities WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("counting the account's identities: %v", err)
	}
	return n
}

func googleCountUsers(t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE email = $1`, email).Scan(&n); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	return n
}

// ---------------------------------------------------------- create & link ----

// An address nobody here has gets an account, a root folder and a link, and the
// session it lands with is a session like any other.
func TestGoogleSignInCreatesAnAccount(t *testing.T) {
	_, h, stub, pool, _ := googleTestServer(t, nil)
	id := googleIdentity(t, "sub-create-"+uuid.NewString())
	// A name with a control character in it, because the display name is
	// interpolated into mail and cleanDisplayName is what stops that.
	id.Name = "Ada\r\n Lovelace"
	stub.SetIdentity(id)

	session := googleSignedIn(t, h)

	userID, hasPassword, verified, displayName, found := googleUserRow(t, pool, strings.ToLower(id.Email))
	if !found {
		t.Fatalf("no user row for %s", id.Email)
	}
	if hasPassword {
		t.Error("the new account has a password hash -- nobody ever chose one")
	}
	if verified == nil {
		t.Error("the new account is not marked verified, so it could never sign in with a password either")
	}
	if displayName != "Ada Lovelace" {
		t.Errorf("display name = %q, want the name claim through cleanDisplayName", displayName)
	}

	row, ok := googleIdentityRow(t, pool, id.Subject)
	if !ok {
		t.Fatal("no identity row was written")
	}
	if row.UserID != userID {
		t.Errorf("the identity points at %s, want the new user %s", row.UserID, userID)
	}
	if !strings.EqualFold(row.EmailAtLink, id.Email) {
		t.Errorf("email_at_link = %q, want the address the link was made with", row.EmailAtLink)
	}

	// The root folder is the assertion that matters here: the session loader
	// LEFT JOINs it, so an account without one signs in perfectly well and then
	// answers /me with the nil UUID -- a browser with nowhere to start.
	rec := authDo(t, h, http.MethodGet, "/api/auth/me", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me on the new session: status %d, body %s", rec.Code, rec.Body.String())
	}
	var me struct {
		RootID      uuid.UUID `json:"root_id"`
		Email       string    `json:"email"`
		HasPassword bool      `json:"has_password"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decoding /me: %v", err)
	}
	if me.RootID == uuid.Nil {
		t.Error("root_id is the nil UUID -- the account has no root folder")
	}
	if !strings.EqualFold(me.Email, id.Email) {
		t.Errorf("/me email = %q, want %q", me.Email, id.Email)
	}
	if me.HasPassword {
		t.Error("/me reports a password on an account created by Google")
	}
}

// A Google address that already has a Drive account links to it rather than
// colliding with it -- and the password that account already had still works.
//
// This is the other half of the rule the unverified case below tests. A
// verified account proved the address itself, before anybody linked anything,
// so its password is its own and linking does not touch it. Only an account the
// link is *activating* loses one.
func TestGoogleSignInLinksAVerifiedEmailToAnExistingAccount(t *testing.T) {
	_, h, stub, pool, _ := googleTestServer(t, nil)
	sender := &authRecordingSender{}
	_ = sender

	// A real account, made the ordinary way.
	passwordServer, passwordSender, _ := authTestServer(t)
	email := authVerifiedUser(t, passwordServer, passwordSender)

	id := googleIdentity(t, "sub-link-"+uuid.NewString())
	id.Email = email
	stub.SetIdentity(id)

	// Read before the link, so the assertion below is against what this account
	// actually called itself rather than a literal copied from a helper.
	_, _, _, nameBefore, _ := googleUserRow(t, pool, email)
	if nameBefore == "" || nameBefore == id.Name {
		t.Fatalf("the account's name is %q, so an unchanged-name assertion would be vacuous", nameBefore)
	}

	googleSignedIn(t, h)

	if n := googleCountUsers(t, pool, email); n != 1 {
		t.Errorf("%d user rows for %s, want 1 -- a second account was created instead of linked", n, email)
	}
	userID, hasPassword, verified, displayName, found := googleUserRow(t, pool, email)
	if !found {
		t.Fatalf("no user row for %s", email)
	}
	if !hasPassword {
		t.Error("the password hash was cleared by linking")
	}
	if verified == nil {
		t.Error("email_verified_at is null after a verified Google sign-in")
	}
	// The name goes the same way as the password. This account chose its own,
	// having proved the address before anybody linked anything, so a claim does
	// not get to rewrite it -- only an account a link is *activating* is renamed.
	if displayName != nameBefore {
		t.Errorf("display_name = %q after linking, want the account's own name %q", displayName, nameBefore)
	}
	row, ok := googleIdentityRow(t, pool, id.Subject)
	if !ok {
		t.Fatal("no identity row was written")
	}
	if row.UserID != userID {
		t.Errorf("the identity points at %s, want the existing user %s", row.UserID, userID)
	}

	// The whole point of leaving the hash alone.
	rec := authDo(t, passwordServer, http.MethodPost, "/api/auth/login",
		authLoginBody{email, authTestPassword}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("password login after linking: status %d, body %s", rec.Code, rec.Body.String())
	}
}

// An account that never clicked its verification link is rescued by a Google
// sign-in on the same address -- and loses the password nobody ever proved.
//
// The rescue and the discard are one rule. On an open-signup deployment the
// unverified row is not necessarily the address owner's: anybody can sign up an
// address they do not own, the row then sits there refusing to log in, and the
// verification mail goes to the person who does own it. This sign-in is the
// moment that row becomes a live account, so the credential attached to it goes
// with it -- otherwise whoever squatted the address now has a working password
// on somebody else's Drive, can rotate it, and can unlink the owner's Google
// account. Password reset has no equivalent hole: a reset replaces the hash
// instead of activating one.
func TestAGoogleLinkActivatesAnUnverifiedAccountWithoutItsPassword(t *testing.T) {
	_, h, stub, pool, _ := googleTestServer(t, nil)

	passwordServer, _, _ := authTestServer(t)
	email := authTestEmail(t)
	const squatterName = "Never Verified"
	rec := authDo(t, passwordServer, http.MethodPost, "/api/auth/signup",
		authSignupBody{email, authTestPassword, squatterName}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("signup: status %d, body %s", rec.Code, rec.Body.String())
	}
	_, squatted, verified, squattedName, _ := googleUserRow(t, pool, email)
	if verified != nil {
		t.Fatal("the account was already verified before the Google sign-in")
	}
	if !squatted {
		t.Fatal("the signup wrote no password hash, so everything below would be vacuous")
	}
	if squattedName != squatterName {
		t.Fatalf("display_name = %q after the signup, want %q -- the name assertions below would be vacuous", squattedName, squatterName)
	}

	id := googleIdentity(t, "sub-rescue-"+uuid.NewString())
	id.Email = email
	stub.SetIdentity(id)
	if id.Name == "" || id.Name == squatterName {
		t.Fatalf("the provider's name is %q, so the rename assertion would be vacuous", id.Name)
	}
	session := googleSignedIn(t, h)

	_, hasPassword, verified, displayName, _ := googleUserRow(t, pool, email)
	if verified == nil {
		t.Error("email_verified_at is still null after a verified Google sign-in")
	}
	if hasPassword {
		t.Error("the account kept the hash it was signed up with -- whoever chose it never proved the address")
	}
	// The name is the other thing the squatter chose. Rescuing the account
	// without it would leave somebody's Drive permanently labelled with
	// attacker-typed text until they renamed it themselves.
	if displayName == squatterName {
		t.Error("the rescued account still carries the name whoever squatted it typed at signup")
	}
	if displayName != id.Name {
		t.Errorf("display_name = %q after the rescue, want the provider's name %q", displayName, id.Name)
	}
	if meHasPassword(t, h, session) {
		t.Error("/me reports has_password on an account whose unproven hash was discarded")
	}

	// And the password that could not log in before the rescue still cannot,
	// told the same thing an address with no account at all is told.
	login := authDo(t, passwordServer, http.MethodPost, "/api/auth/login",
		authLoginBody{email, authTestPassword}, nil)
	nodeWant(t, login, http.StatusUnauthorized, CodeUnauthorized)
	unknown := authDo(t, passwordServer, http.MethodPost, "/api/auth/login",
		authLoginBody{authTestEmail(t), authTestPassword}, nil)
	if got, want := login.Body.String(), unknown.Body.String(); got != want {
		t.Errorf("the discarded password answers %s, want the unknown-address body %s", got, want)
	}
}

// A second Google account offered for an account that already has one is a
// permanent conflict, and is named as one.
//
// The unique index on (user_id, provider) refuses the row now and would refuse
// it on every retry, so treating it as a lost race costs a second transaction
// to reach the same refusal under a reason that points at the wrong thing. The
// caller still gets the generic redirect -- every refusal here does -- but the
// log line is the only trace there is, and it has to say what happened.
func TestASecondGoogleAccountForOneDriveAccountIsAlreadyLinked(t *testing.T) {
	_, h, stub, pool, logs := googleTestServer(t, nil)

	passwordServer, sender, _ := authTestServer(t)
	email := authVerifiedUser(t, passwordServer, sender)

	first := googleIdentity(t, "sub-linked-first-"+uuid.NewString())
	first.Email = email
	stub.SetIdentity(first)
	googleSignedIn(t, h)

	userID, _, _, _, found := googleUserRow(t, pool, email)
	if !found {
		t.Fatalf("no user row for %s", email)
	}
	if n := googleCountUserIdentities(t, pool, userID); n != 1 {
		t.Fatalf("%d identities after the first link, want 1", n)
	}

	// The same person, a different Google account, the same Drive address.
	second := googleIdentity(t, "sub-linked-second-"+uuid.NewString())
	second.Email = email
	stub.SetIdentity(second)

	rec := googleSignIn(t, h)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != loginGoogleError {
		t.Fatalf("the second link answered %d → %q, want 302 → %q",
			rec.Code, rec.Header().Get("Location"), loginGoogleError)
	}
	if cookieNamed(rec, SessionCookie) != nil {
		t.Error("a refused link minted a session")
	}
	if !strings.Contains(logs.String(), "reason="+reasonAlreadyLinked) {
		t.Errorf("the refusal carries no reason=%s line:\n%s", reasonAlreadyLinked, logs.String())
	}
	if strings.Contains(logs.String(), "reason="+reasonLinkFailed) {
		t.Errorf("a permanent conflict was reported as a failed link:\n%s", logs.String())
	}

	// Nothing was written, and the first link is untouched.
	if n := googleCountIdentities(t, pool, second.Subject); n != 0 {
		t.Errorf("%d identity rows for the second Google account, want 0", n)
	}
	if n := googleCountUserIdentities(t, pool, userID); n != 1 {
		t.Errorf("%d identities on the account, want the one it started with", n)
	}
	if row, ok := googleIdentityRow(t, pool, first.Subject); !ok || row.UserID != userID {
		t.Error("the original link did not survive the refused one")
	}
}

// After the first link the lookup is by subject alone, which is what makes a
// provider-side email change a non-event -- and what makes an email-based
// takeover impossible.
func TestGoogleSignInFindsTheAccountBySubjectAfterAnEmailChange(t *testing.T) {
	_, h, stub, pool, _ := googleTestServer(t, nil)

	first := googleIdentity(t, "sub-stable-"+uuid.NewString())
	stub.SetIdentity(first)
	googleSignedIn(t, h)

	original, _, _, _, found := googleUserRow(t, pool, strings.ToLower(first.Email))
	if !found {
		t.Fatal("the first sign-in created no account")
	}
	before, _ := googleIdentityRow(t, pool, first.Subject)

	// The same person, a different address on the provider's side.
	changed := first
	changed.Email = "changed-" + uuid.NewString() + "@example.test"
	stub.SetIdentity(changed)
	googleSignedIn(t, h)

	after, ok := googleIdentityRow(t, pool, first.Subject)
	if !ok {
		t.Fatal("the identity row disappeared")
	}
	if after.UserID != original {
		t.Errorf("the second sign-in landed on %s, want the same account %s", after.UserID, original)
	}
	if googleCountUsers(t, pool, changed.Email) != 0 {
		t.Error("a second account was created for the changed address")
	}
	if _, _, _, _, stillThere := googleUserRow(t, pool, strings.ToLower(first.Email)); !stillThere {
		t.Error("users.email was rewritten from the claim")
	}
	if !strings.EqualFold(after.EmailAtLink, first.Email) {
		t.Errorf("email_at_link = %q, want the address at link time %q -- it is an audit fact", after.EmailAtLink, first.Email)
	}
	if after.LastLoginAt == nil || before.LastLoginAt == nil || !after.LastLoginAt.After(*before.LastLoginAt) {
		t.Errorf("last_login_at did not move: %v → %v", before.LastLoginAt, after.LastLoginAt)
	}

	// And the sharper version: the provider now reports an address that belongs
	// to somebody else here. Still the subject's account, no takeover.
	victimServer, victimSender, _ := authTestServer(t)
	victim := authVerifiedUser(t, victimServer, victimSender)
	victimID, _, _, _, _ := googleUserRow(t, pool, victim)

	stolen := first
	stolen.Email = victim
	stub.SetIdentity(stolen)
	googleSignedIn(t, h)

	landed, _ := googleIdentityRow(t, pool, first.Subject)
	if landed.UserID != original {
		t.Errorf("the sign-in landed on %s, want the subject's own account %s", landed.UserID, original)
	}
	if landed.UserID == victimID {
		t.Error("a claim carrying somebody else's address moved the identity onto their account")
	}
	if n := googleCountIdentities(t, pool, first.Subject); n != 1 {
		t.Errorf("%d identity rows for one subject, want 1", n)
	}
}

// ------------------------------------------------------------- refusals ------

// Every way the callback can fail, and the one answer all of them give.
//
// The identical Location is the assertion: bad state, no cookie, a forged or
// expired or misaddressed token, an unverified address and a provider outage
// must be indistinguishable to the caller, and each must leave nothing behind
// in the database.
func TestGoogleCallbackRefusesEveryBadFlowIdentically(t *testing.T) {
	type breaker func(t *testing.T, h http.Handler, stub *oidcstub.Stub, id oidcstub.Identity) *httptest.ResponseRecorder

	// walk runs the flow with the identity armed as the case wants.
	walk := func(mode string) breaker {
		return func(t *testing.T, h http.Handler, stub *oidcstub.Stub, id oidcstub.Identity) *httptest.ResponseRecorder {
			id.Mode = mode
			stub.SetIdentity(id)
			return googleSignIn(t, h)
		}
	}

	cases := []struct {
		name   string
		run    breaker
		reason string
	}{
		{"a state the cookie does not carry", func(t *testing.T, h http.Handler, stub *oidcstub.Stub, id oidcstub.Identity) *httptest.ResponseRecorder {
			stub.SetIdentity(id)
			authURL, flow := googleStart(t, h, "")
			code, _ := googleAuthorize(t, authURL)
			return googleGet(t, h, "/api/auth/google/callback?code="+url.QueryEscape(code)+"&state=not-the-state", flow)
		}, reasonBadState},

		{"no flow cookie at all", func(t *testing.T, h http.Handler, stub *oidcstub.Stub, id oidcstub.Identity) *httptest.ResponseRecorder {
			stub.SetIdentity(id)
			authURL, _ := googleStart(t, h, "")
			code, state := googleAuthorize(t, authURL)
			return googleGet(t, h, "/api/auth/google/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state))
		}, reasonNoCookie},

		{"no code", func(t *testing.T, h http.Handler, stub *oidcstub.Stub, id oidcstub.Identity) *httptest.ResponseRecorder {
			stub.SetIdentity(id)
			authURL, flow := googleStart(t, h, "")
			_, state := googleAuthorize(t, authURL)
			return googleGet(t, h, "/api/auth/google/callback?state="+url.QueryEscape(state), flow)
		}, reasonNoCode},

		{"a nonce from another flow", walk(oidcstub.ModeBadNonce), reasonBadNonce},
		{"a signature from a key the JWKS does not carry", walk(oidcstub.ModeForeignKey), reasonVerifyFailed},
		{"an expired token", walk(oidcstub.ModeExpired), reasonVerifyFailed},
		{"a token for another client", walk(oidcstub.ModeWrongAudience), reasonVerifyFailed},
		{"alg: none", walk(oidcstub.ModeAlgNone), reasonVerifyFailed},
		{"HS256 keyed with the RSA public key", walk(oidcstub.ModeHS256), reasonVerifyFailed},
		// The scheme-less issuer: the verifier accepts the discovery document's
		// own issuer string and nothing else, and the refusal is a logged
		// verify_failed rather than anything silent.
		{"an issuer one prefix away from the discovered one", walk(oidcstub.ModeBareIssuer), reasonVerifyFailed},
		{"no id_token in the token response", walk(oidcstub.ModeNoIDToken), reasonExchangeFailed},
	}

	// The last two are claims rather than broken tokens: a perfectly signed
	// token can still say something Drive cannot act on.
	cases = append(cases, struct {
		name   string
		run    breaker
		reason string
	}{"an address the provider will not vouch for", func(t *testing.T, h http.Handler, stub *oidcstub.Stub, id oidcstub.Identity) *httptest.ResponseRecorder {
		id.EmailVerified = false
		stub.SetIdentity(id)
		return googleSignIn(t, h)
	}, reasonEmailUnverif})

	cases = append(cases, struct {
		name   string
		run    breaker
		reason string
	}{"an address that is not an address", func(t *testing.T, h http.Handler, stub *oidcstub.Stub, id oidcstub.Identity) *httptest.ResponseRecorder {
		// canonicalEmail is the same gate signup and login go through, and it
		// is applied here for the same reason: nothing downstream may carry a
		// malformed address into a mail header.
		id.Email = "not-an-address"
		stub.SetIdentity(id)
		return googleSignIn(t, h)
	}, reasonBadEmail})

	var locations []string
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, h, stub, pool, logs := googleTestServer(t, nil)
			id := googleIdentity(t, "sub-refuse-"+uuid.NewString())

			rec := c.run(t, h, stub, id)

			if rec.Code != http.StatusFound {
				t.Fatalf("status %d, want 302 (body %s)", rec.Code, rec.Body.String())
			}
			locations = append(locations, rec.Header().Get("Location"))
			if got := rec.Header().Get("Location"); got != loginGoogleError {
				t.Errorf("Location = %q, want %q", got, loginGoogleError)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("the refusal carried a body: %s", rec.Body.String())
			}
			if cookieNamed(rec, SessionCookie) != nil {
				t.Error("a refused sign-in minted a session cookie")
			}
			if cleared := cookieNamed(rec, OAuthCookie); cleared == nil || cleared.MaxAge >= 0 {
				t.Errorf("the flow cookie was not cleared: %v", cleared)
			}

			// Nothing was written.
			if n := googleCountIdentities(t, pool, id.Subject); n != 0 {
				t.Errorf("%d identity rows after a refusal, want 0", n)
			}
			if n := googleCountUsers(t, pool, strings.ToLower(id.Email)); n != 0 {
				t.Errorf("%d user rows after a refusal, want 0", n)
			}

			// And the log says which check refused it. The redirect is the same
			// for all of them, so this line is the only trace there is -- and it
			// has to be written at info, which is what a deployment runs at.
			if !strings.Contains(logs.String(), `reason=`+c.reason) {
				t.Errorf("the log carries no reason=%s line:\n%s", c.reason, logs.String())
			}
		})
	}

	// Every refusal answered with the byte-identical Location.
	for i, got := range locations {
		if got != locations[0] {
			t.Errorf("refusal %d answered %q, but the first answered %q -- the tuple diverges", i, got, locations[0])
		}
	}
}

// Cancel is not a failure. It lands on a plain /login with nothing to report,
// because nothing went wrong.
func TestGoogleCallbackTreatsCancelQuietly(t *testing.T) {
	_, h, _, _, _ := googleTestServer(t, nil)
	_, flow := googleStart(t, h, "")

	rec := googleGet(t, h, "/api/auth/google/callback?error=access_denied&state=whatever", flow)
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != loginPath {
		t.Errorf("Location = %q, want %q -- pressing Cancel is not an error", got, loginPath)
	}
	if cleared := cookieNamed(rec, OAuthCookie); cleared == nil || cleared.MaxAge >= 0 {
		t.Error("the flow cookie survived a cancelled sign-in")
	}
}

// The provider's error parameter is a query parameter, and a query parameter is
// not something the provider is the only one who can write.
//
// The callback is a URL anybody can send a browser to, so ?error= is
// attacker-chosen text arriving at a Warn line that production actually writes.
// Only the codes the specifications define are logged as themselves; everything
// else is the constant "unknown", which is all the line was ever going to be
// used for.
func TestAnUnknownProviderErrorIsNotLoggedVerbatim(t *testing.T) {
	_, h, _, _, logs := googleTestServer(t, nil)
	_, flow := googleStart(t, h, "")

	junk := strings.Repeat("A", 10*1024)
	rec := googleGet(t, h, "/api/auth/google/callback?error="+url.QueryEscape(junk)+"&state=whatever", flow)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != loginGoogleError {
		t.Fatalf("status %d → %q, want 302 → %q", rec.Code, rec.Header().Get("Location"), loginGoogleError)
	}

	written := logs.String()
	if strings.Contains(written, junk[:64]) {
		t.Errorf("the provider's error parameter reached the log, which is now %d bytes", len(written))
	}
	if !strings.Contains(written, "reason="+reasonProviderError) {
		t.Errorf("no %s line in the log:\n%s", reasonProviderError, written)
	}
	if !strings.Contains(written, "error=unknown") {
		t.Errorf("the log does not carry the bounded code:\n%s", written)
	}

	// A code the specifications do define is still written as itself -- that is
	// the whole diagnostic value of the line.
	_, knownH, _, _, knownLogs := googleTestServer(t, nil)
	_, knownFlow := googleStart(t, knownH, "")
	googleGet(t, knownH, "/api/auth/google/callback?error=server_error&state=whatever", knownFlow)
	if !strings.Contains(knownLogs.String(), "error=server_error") {
		t.Errorf("a code the provider is allowed to send was not logged:\n%s", knownLogs.String())
	}
}

// The flow cookie is cleared on success too, so a replayed callback finds no
// flow -- which is what the assertion reads, since the provider has already
// burnt the code.
func TestTheFlowCookieIsClearedOnSuccessAndAReplayIsRefused(t *testing.T) {
	_, h, stub, _, logs := googleTestServer(t, nil)
	stub.SetIdentity(googleIdentity(t, "sub-replay-"+uuid.NewString()))

	authURL, flow := googleStart(t, h, "")
	code, state := googleAuthorize(t, authURL)
	callback := "/api/auth/google/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)

	first := googleGet(t, h, callback, flow)
	if first.Code != http.StatusFound || first.Header().Get("Location") != afterSignIn {
		t.Fatalf("the first callback answered %d → %q", first.Code, first.Header().Get("Location"))
	}
	cleared := cookieNamed(first, OAuthCookie)
	if cleared == nil {
		t.Fatal("a successful callback did not clear the flow cookie")
	}
	if cleared.MaxAge >= 0 {
		t.Errorf("the clearing cookie has MaxAge %d, want a negative one", cleared.MaxAge)
	}
	if cleared.Path != oauthCookiePath {
		t.Errorf("the clearing cookie's path is %q, want %q -- a different path leaves the original in place",
			cleared.Path, oauthCookiePath)
	}

	// The browser now holds no flow. Replaying the exact callback is refused
	// for the cookie's absence, before the burnt code is ever offered.
	replay := googleGet(t, h, callback)
	if replay.Code != http.StatusFound || replay.Header().Get("Location") != loginGoogleError {
		t.Fatalf("the replay answered %d → %q, want 302 → %q",
			replay.Code, replay.Header().Get("Location"), loginGoogleError)
	}
	if cookieNamed(replay, SessionCookie) != nil {
		t.Error("the replay minted a second session")
	}
	if !strings.Contains(logs.String(), "reason="+reasonNoCookie) {
		t.Errorf("the replay was not refused for the missing cookie:\n%s", logs.String())
	}
}

// The flow cookie's own shape: scoped to the two routes that read it, not
// readable from script, and Lax -- which the return from the provider needs,
// because it is a top-level cross-site GET.
func TestTheFlowCookieIsScopedAndLax(t *testing.T) {
	_, h, _, _, _ := googleTestServer(t, nil)
	_, flow := googleStart(t, h, "")

	if flow.Path != oauthCookiePath {
		t.Errorf("path = %q, want %q", flow.Path, oauthCookiePath)
	}
	if !flow.HttpOnly {
		t.Error("the flow cookie is readable from script")
	}
	if flow.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax -- Strict would drop the return from the provider", flow.SameSite)
	}
	if flow.Secure {
		t.Error("Secure is set on an http base URL, where the browser would drop the cookie")
	}
	if flow.MaxAge != int(oauthFlowTTL.Seconds()) {
		t.Errorf("MaxAge = %d, want %d", flow.MaxAge, int(oauthFlowTTL.Seconds()))
	}

	// And on https it is Secure, which is the half that matters in production.
	_, secureH, _, _, _ := googleTestServer(t, func(c *config.Config) { c.BaseURL = "https://drive.example.test" })
	_, secureFlow := googleStart(t, secureH, "")
	if !secureFlow.Secure {
		t.Error("Secure is not set on an https base URL")
	}
}

// ---------------------------------------------------------- closed signups ---

// A closed deployment does not gain a back door, and says so rather than
// stranding somebody on an error no retry can clear -- POST /auth/signup
// already refuses publicly, so the fact is not a leak.
func TestClosedSignupsRefuseANewGoogleAccountButStillLink(t *testing.T) {
	closed := func(c *config.Config) { c.SignupMode = config.SignupClosed }

	t.Run("an unknown address", func(t *testing.T) {
		_, h, stub, pool, logs := googleTestServer(t, closed)
		id := googleIdentity(t, "sub-closed-"+uuid.NewString())
		stub.SetIdentity(id)

		rec := googleSignIn(t, h)
		if rec.Code != http.StatusFound {
			t.Fatalf("status %d, want 302", rec.Code)
		}
		if got := rec.Header().Get("Location"); got != loginGoogleClosed {
			t.Errorf("Location = %q, want %q", got, loginGoogleClosed)
		}
		if cookieNamed(rec, SessionCookie) != nil {
			t.Error("a refused sign-up minted a session")
		}
		if n := googleCountUsers(t, pool, strings.ToLower(id.Email)); n != 0 {
			t.Errorf("%d user rows on a closed deployment, want 0", n)
		}
		if n := googleCountIdentities(t, pool, id.Subject); n != 0 {
			t.Errorf("%d identity rows on a closed deployment, want 0", n)
		}
		if !strings.Contains(logs.String(), "reason="+reasonSignupsClosed) {
			t.Errorf("no signups_closed line in the log:\n%s", logs.String())
		}
	})

	t.Run("an address that already has an account", func(t *testing.T) {
		_, h, stub, pool, _ := googleTestServer(t, closed)
		passwordServer, sender, _ := authTestServer(t)
		email := authVerifiedUser(t, passwordServer, sender)

		id := googleIdentity(t, "sub-closed-link-"+uuid.NewString())
		id.Email = email
		stub.SetIdentity(id)

		googleSignedIn(t, h)
		if _, ok := googleIdentityRow(t, pool, id.Subject); !ok {
			t.Error("an existing account could not link on a closed deployment")
		}
	})
}

// ------------------------------------------------------------- the race ------

// Two callbacks for one new subject, at the same time: one account, one
// identity, and both people signed in.
//
// The loser of the insert race rolls back and re-runs the lookup once, which is
// what turns "somebody else won" into "sign in as what they committed". Without
// that re-run one of the two is told the sign-in failed.
func TestTwoConcurrentCallbacksForOneSubjectMakeOneAccount(t *testing.T) {
	_, h, stub, pool, _ := googleTestServer(t, nil)
	id := googleIdentity(t, "sub-race-"+uuid.NewString())
	stub.SetIdentity(id)

	// Two flows, both authorized before either is exchanged.
	type leg struct {
		callback string
		flow     *http.Cookie
	}
	legs := make([]leg, 2)
	for i := range legs {
		authURL, flow := googleStart(t, h, "")
		code, state := googleAuthorize(t, authURL)
		legs[i] = leg{
			callback: "/api/auth/google/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state),
			flow:     flow,
		}
	}

	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, len(legs))
	start := make(chan struct{})
	for i, l := range legs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodGet, l.callback, nil)
			req.RemoteAddr = testClientAddr(t)
			req.AddCookie(l.flow)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			recs[i] = rec
		}()
	}
	close(start)
	wg.Wait()

	if n := googleCountUsers(t, pool, strings.ToLower(id.Email)); n != 1 {
		t.Errorf("%d user rows, want exactly 1", n)
	}
	if n := googleCountIdentities(t, pool, id.Subject); n != 1 {
		t.Errorf("%d identity rows, want exactly 1", n)
	}

	for i, rec := range recs {
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != afterSignIn {
			t.Fatalf("callback %d answered %d → %q, want 302 → %q",
				i, rec.Code, rec.Header().Get("Location"), afterSignIn)
		}
		session := cookieNamed(rec, SessionCookie)
		if session == nil {
			t.Fatalf("callback %d minted no session", i)
		}
		// Both sessions are usable: a race that ended with one person told
		// "sign-in failed" is the bug this closes.
		if got := authDo(t, h, http.MethodGet, "/api/auth/me", nil, session).Code; got != http.StatusOK {
			t.Errorf("the session from callback %d answers /me with %d, want 200", i, got)
		}
	}
}

// ------------------------------------------------------ redirects & bucket ---

// There is no `next` parameter, on either route. An allowlist would be a
// mechanism to get wrong for a feature nobody asked for, and the relative
// landing cannot be pointed off-site whatever anything says.
func TestGoogleStartIgnoresNext(t *testing.T) {
	_, h, stub, _, _ := googleTestServer(t, nil)
	stub.SetIdentity(googleIdentity(t, "sub-next-"+uuid.NewString()))

	const evil = "https://evil.example.test/"
	authURL, flow := googleStart(t, h, "?next="+url.QueryEscape(evil))
	if strings.Contains(authURL, "evil.example.test") {
		t.Errorf("the authorization URL carries the next parameter: %s", authURL)
	}

	code, state := googleAuthorize(t, authURL)
	rec := googleGet(t, h, "/api/auth/google/callback?next="+url.QueryEscape(evil)+
		"&code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), flow)
	if got := rec.Header().Get("Location"); got != afterSignIn {
		t.Errorf("Location = %q, want the relative %q", got, afterSignIn)
	}
}

// The bucket in front of the two navigations refuses with a redirect, not the
// error envelope: a browser following a link cannot do anything with JSON.
func TestGoogleRoutesAreBucketedWithARedirect(t *testing.T) {
	_, h, _, _, _ := googleTestServer(t, nil)

	for i := 1; i <= int(burstFor(DefaultAuthRatePerMin)); i++ {
		if rec := googleGet(t, h, "/api/auth/google/start"); rec.Code != http.StatusFound {
			t.Fatalf("request %d of the burst answered %d, want 302", i, rec.Code)
		}
	}

	rec := googleGet(t, h, "/api/auth/google/start")
	if rec.Code != http.StatusFound {
		t.Fatalf("past the burst: status %d, want 302 -- a bucketed browser navigation must still be a redirect", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != loginGoogleError {
		t.Errorf("Location = %q, want %q", got, loginGoogleError)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("the refusal carried a body: %s", rec.Body.String())
	}
	// And the same for the callback, which is the one a browser arrives at
	// carrying a code it cannot get back.
	if got := googleGet(t, h, "/api/auth/google/callback?code=x&state=y").Code; got != http.StatusFound {
		t.Errorf("the bucketed callback answered %d, want 302", got)
	}
}

// The two browser navigations spend an allowance of their own, not the password
// form's.
//
// They are the only routes on the auth surface a stranger can make somebody's
// browser visit without that person doing anything -- a link, an <img src>, a
// crawler working through /login. If those hits came out of the login bucket,
// an address could be talked out of its own sign-in allowance, and the person
// behind it would find the password form refusing them for something they never
// did.
func TestTheBrowserNavigationsDoNotSpendTheLoginAllowance(t *testing.T) {
	_, h, _, _, _ := googleTestServer(t, nil)

	// One past the burst, so the bucket in front of /start is provably empty.
	for range int(burstFor(DefaultAuthRatePerMin)) + 1 {
		googleGet(t, h, "/api/auth/google/start")
	}
	if got := googleGet(t, h, "/api/auth/google/start").Header().Get("Location"); got != loginGoogleError {
		t.Fatalf("/start → %q past the burst, want the bucketed %q -- the bucket is not empty", got, loginGoogleError)
	}

	rec := authDo(t, h, http.MethodPost, "/api/auth/login",
		authLoginBody{authTestEmail(t), authTestPassword}, nil)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("login answered 429 from the address a crawler spent the browser bucket from: %s", rec.Body.String())
	}
	nodeWant(t, rec, http.StatusUnauthorized, CodeUnauthorized)
}

// A deployment with no Google client still answers both routes, and answers
// them the way a browser can use.
func TestGoogleRoutesAreMountedWhenUnconfigured(t *testing.T) {
	unconfigured := func(c *config.Config) {
		c.GoogleClientID = ""
		c.GoogleClientSecret = ""
	}
	_, h, _, _, logs := googleTestServer(t, unconfigured)

	for _, path := range []string{"/api/auth/google/start", "/api/auth/google/callback?code=x&state=y"} {
		rec := googleGet(t, h, path)
		if rec.Code != http.StatusFound {
			t.Fatalf("%s answered %d, want 302 -- an unmounted route is /api's JSON 404", path, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != loginGoogleError {
			t.Errorf("%s → %q, want %q", path, got, loginGoogleError)
		}
	}
	if !strings.Contains(logs.String(), "reason="+reasonNotConfigured) {
		t.Errorf("no not_configured line in the log:\n%s", logs.String())
	}
}

// ------------------------------------------------------------- log hygiene ---

// What the logs may and may not carry.
//
// The request logger writes r.URL.Path and not RawQuery, and these handlers log
// a user id, a provider and a constant reason. An authorization code in a log
// file is a credential in a log file.
func TestGoogleLogsCarryTheReasonAndNoSecrets(t *testing.T) {
	_, h, stub, _, logs := googleTestServer(t, nil)
	id := googleIdentity(t, "sub-logs-"+uuid.NewString())
	stub.SetIdentity(id)

	authURL, flow := googleStart(t, h, "")
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing the authorization URL: %v", err)
	}
	state := parsed.Query().Get("state")
	nonce := parsed.Query().Get("nonce")
	challenge := parsed.Query().Get("code_challenge")
	code, gotState := googleAuthorize(t, authURL)

	rec := googleGet(t, h, "/api/auth/google/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(gotState), flow)
	if rec.Code != http.StatusFound {
		t.Fatalf("the callback answered %d", rec.Code)
	}

	written := logs.String()
	if !strings.Contains(written, "google sign-in") {
		t.Errorf("a successful sign-in left no trace in the log:\n%s", written)
	}
	for _, secret := range []struct{ what, value string }{
		{"the authorization code", code},
		{"the state", state},
		{"the nonce", nonce},
		{"the PKCE challenge", challenge},
		{"the flow cookie", flow.Value},
	} {
		if secret.value == "" {
			t.Fatalf("%s was empty, so the assertion below would be vacuous", secret.what)
		}
		if strings.Contains(written, secret.value) {
			t.Errorf("%s is in the log:\n%s", secret.what, written)
		}
	}

	// And a refusal writes its reason at info -- the level a deployment runs
	// at. The request logger's own line is Debug, so it is not written there.
	_, refuseH, refuseStub, _, refuseLogs := googleTestServer(t, nil)
	bad := googleIdentity(t, "sub-logs-bad-"+uuid.NewString())
	bad.Mode = oidcstub.ModeForeignKey
	refuseStub.SetIdentity(bad)
	googleSignIn(t, refuseH)
	if !strings.Contains(refuseLogs.String(), "reason="+reasonVerifyFailed) {
		t.Errorf("a refusal left no reason at info:\n%s", refuseLogs.String())
	}
}

// ------------------------------------------------------------- identities ----

// The account screen's view of a link, and the one thing it must not let
// somebody do: unlink their only way in.
func TestIdentitiesListAndUnlink(t *testing.T) {
	_, h, stub, pool, _ := googleTestServer(t, nil)
	id := googleIdentity(t, "sub-unlink-"+uuid.NewString())
	stub.SetIdentity(id)
	session := googleSignedIn(t, h)

	rec := authDo(t, h, http.MethodGet, "/api/auth/identities", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /identities: status %d, body %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []struct {
			ID          uuid.UUID  `json:"id"`
			Provider    string     `json:"provider"`
			EmailAtLink string     `json:"email_at_link"`
			CreatedAt   time.Time  `json:"created_at"`
			LastLoginAt *time.Time `json:"last_login_at"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding /identities: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("%d identities, want 1: %s", len(list.Items), rec.Body.String())
	}
	if list.NextCursor != nil {
		t.Errorf("next_cursor = %v, want null -- the list is one page", *list.NextCursor)
	}
	row := list.Items[0]
	if row.Provider != auth.ProviderGoogle {
		t.Errorf("provider = %q, want %q", row.Provider, auth.ProviderGoogle)
	}
	if !strings.EqualFold(row.EmailAtLink, id.Email) {
		t.Errorf("email_at_link = %q, want %q", row.EmailAtLink, id.Email)
	}
	if row.LastLoginAt == nil {
		t.Error("last_login_at is null right after signing in with the link")
	}
	// The subject is the provider's internal id for a person and has no place
	// on an account screen.
	if strings.Contains(rec.Body.String(), id.Subject) {
		t.Errorf("the identities list leaks the provider subject: %s", rec.Body.String())
	}

	// It is the only way in, so it cannot be removed.
	del := authDo(t, h, http.MethodDelete, "/api/auth/identities/"+row.ID.String(), nil, session)
	nodeWant(t, del, http.StatusConflict, CodeUnsupported)
	if _, ok := googleIdentityRow(t, pool, id.Subject); !ok {
		t.Fatal("the identity was deleted anyway")
	}

	// Somebody else's identity id is a 404, exactly as an unknown one.
	_, _, otherSession := googleOnlyAccount(t, pool)
	nodeWant(t, authDo(t, h, http.MethodDelete, "/api/auth/identities/"+row.ID.String(), nil, otherSession),
		http.StatusNotFound, CodeNotFound)
	nodeWant(t, authDo(t, h, http.MethodDelete, "/api/auth/identities/"+uuid.NewString(), nil, session),
		http.StatusNotFound, CodeNotFound)

	// With a password on the account, it can go.
	userID, _, _, _, _ := googleUserRow(t, pool, strings.ToLower(id.Email))
	hash, err := auth.HashPassword(authTestPassword)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash); err != nil {
		t.Fatalf("setting a password: %v", err)
	}

	if got := authDo(t, h, http.MethodDelete, "/api/auth/identities/"+row.ID.String(), nil, session); got.Code != http.StatusNoContent {
		t.Fatalf("unlink with a password set: status %d, body %s", got.Code, got.Body.String())
	}
	if _, ok := googleIdentityRow(t, pool, id.Subject); ok {
		t.Error("the identity survived a successful unlink")
	}
	if rec := authDo(t, h, http.MethodPost, "/api/auth/login",
		authLoginBody{strings.ToLower(id.Email), authTestPassword}, nil); rec.Code != http.StatusOK {
		t.Fatalf("password login after unlinking: status %d, body %s", rec.Code, rec.Body.String())
	}
}

// The name claim is optional and is attacker-adjacent -- it is interpolated
// into mail headers -- so it goes through cleanDisplayName, and there is always
// something left to call the account.
func TestGoogleDisplayNameFallsBackToTheLocalPart(t *testing.T) {
	for _, c := range []struct {
		name, claim, email, want string
	}{
		{"a plain name", "Ada Lovelace", "ada@example.test", "Ada Lovelace"},
		{"a name that is only whitespace", "   ", "ada@example.test", "ada"},
		{"no name at all", "", "ada.lovelace@example.test", "ada.lovelace"},
		{"a name that cleans away entirely", "\r\n", "ada@example.test", "ada"},
		// Control characters are what cleanDisplayName exists for: the name
		// rides into a mail header.
		{"a name carrying a header injection", "Ada\r\nBcc: someone@example.test", "ada@example.test",
			"AdaBcc: someone@example.test"},
		// Nothing usable anywhere still has to produce a name, because
		// display_name is NOT NULL and the insert would otherwise fail.
		{"nothing usable at all", "", "@example.test", "@example.test"},
		// A name over the limit is cut to it rather than refused. Refusing it
		// falls through to the local part, which would call somebody by their
		// email address because their provider profile was long. Multi-byte
		// runes, so a cut made in bytes would show up as a wrong length here
		// and as half a character in the name.
		{"a name longer than the limit", strings.Repeat("é", maxDisplayName+50), "ada@example.test",
			strings.Repeat("é", maxDisplayName)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := googleDisplayName(c.claim, c.email); got != c.want {
				t.Errorf("googleDisplayName(%q, %q) = %q, want %q", c.claim, c.email, got, c.want)
			}
		})
	}
}
