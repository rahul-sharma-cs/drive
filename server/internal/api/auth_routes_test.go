package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/db"
)

// ---------------------------------------------------------------- harness ----

// authTestDSN is the drive-test stack's Postgres, verbatim from the committed
// .env.test. Tests never touch the dev stack on :55432.
const authTestDSN = "postgres://drive:drive@localhost:55433/drive?sslmode=disable"

var (
	authPoolOnce sync.Once
	authPool     *pgxpool.Pool
	authPoolErr  error
)

func authTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	authPoolOnce.Do(func() {
		dsn := os.Getenv("DRIVE_DB_DSN")
		if dsn == "" {
			dsn = authTestDSN
		}
		if strings.Contains(dsn, ":55432") {
			authPoolErr = fmt.Errorf("DRIVE_DB_DSN points at the dev stack (%s); tests run against drive-test on :55433", dsn)
			return
		}
		ctx := context.Background()
		if authPool, authPoolErr = db.Connect(ctx, dsn); authPoolErr != nil {
			return
		}
		authPoolErr = db.Migrate(ctx, authPool)
	})
	if authPoolErr != nil {
		t.Fatalf("drive-test database: %v (is `docker compose -p drive-test --env-file .env.test up -d` running?)", authPoolErr)
	}
	return authPool
}

// authRecordingSender captures what would have gone to Mailpit. The
// verification token exists nowhere else -- the database holds only its hash --
// so reading the mail is the only way to complete a signup, exactly as a real
// user does.
type authRecordingSender struct {
	mu   sync.Mutex
	sent []authMail
}

type authMail struct{ To, Subject, Body string }

func (s *authRecordingSender) Send(_ context.Context, to, subject, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, authMail{To: to, Subject: subject, Body: body})
	return nil
}

func (s *authRecordingSender) all() []authMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]authMail(nil), s.sent...)
}

// last returns the most recent message, failing the test if none arrives.
//
// It polls because signup dispatches the verification mail off the request
// goroutine on purpose: doing the send inline made response latency an
// account-existence oracle. Waiting is the honest way to observe that -- the
// test still fails if no mail is ever sent.
func (s *authRecordingSender) last(t *testing.T) authMail {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		all := s.all()
		if len(all) > 0 {
			return all[len(all)-1]
		}
		if time.Now().After(deadline) {
			t.Fatal("no mail was sent within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const authTestBaseURL = "http://localhost:8081"

func authTestServer(t *testing.T) (http.Handler, *authRecordingSender, *pgxpool.Pool) {
	t.Helper()
	return authTestServerWithBaseURL(t, authTestBaseURL)
}

func authTestServerWithBaseURL(t *testing.T, baseURL string) (http.Handler, *authRecordingSender, *pgxpool.Pool) {
	t.Helper()
	pool := authTestPool(t)
	sender := &authRecordingSender{}
	cfg := &config.Config{BaseURL: baseURL}
	return New(cfg, pool, nil, sender, nil, nil).Routes(), sender, pool
}

func authTestEmail(t *testing.T) string {
	t.Helper()
	return "api-" + uuid.NewString() + "@drive.test"
}

// do issues a request through the whole middleware chain, with the CSRF header
// every mutation needs.
func authDo(t *testing.T, h http.Handler, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshalling request body: %v", err)
		}
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set(ClientHeader, "web")
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func authCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie in the response (status %d, body %s)", SessionCookie, rec.Code, rec.Body.String())
	return nil
}

type authSignupBody struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type authLoginBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

const authTestPassword = "correct horse battery staple"

// authVerifiedUser runs the whole signup -> verify-email path and returns the
// address, its root folder id, and the sender that carried the link.
func authVerifiedUser(t *testing.T, h http.Handler, sender *authRecordingSender) string {
	t.Helper()
	email := authTestEmail(t)

	rec := authDo(t, h, http.MethodPost, "/api/auth/signup", authSignupBody{email, authTestPassword, "Test User"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("signup: status %d, body %s", rec.Code, rec.Body.String())
	}

	token := authTokenFromMail(t, sender.last(t).Body)
	rec = authDo(t, h, http.MethodPost, "/api/auth/verify-email", map[string]string{"token": token}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify-email: status %d, body %s", rec.Code, rec.Body.String())
	}
	return email
}

// authTokenFromMail pulls the raw token out of the verification link, whose
// shape is fixed: ${DRIVE_BASE_URL}/verify?token=<raw token>.
func authTokenFromMail(t *testing.T, body string) string {
	t.Helper()
	const marker = "/verify?token="
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no %q link in the mail body:\n%s", marker, body)
	}
	tok := body[i+len(marker):]
	if j := strings.IndexAny(tok, " \r\n\t"); j >= 0 {
		tok = tok[:j]
	}
	if tok == "" {
		t.Fatalf("empty token in the mail body:\n%s", body)
	}
	return tok
}

type authMeDTO struct {
	ID              uuid.UUID  `json:"id"`
	Email           string     `json:"email"`
	DisplayName     string     `json:"display_name"`
	RootID          uuid.UUID  `json:"root_id"`
	EmailVerifiedAt *time.Time `json:"email_verified_at"`
}

// ------------------------------------------------------------------ tests ----

// api owns the cookie MaxAge, auth owns the row's expires_at. They are separate
// constants in separate packages (auth cannot import api), so nothing but this
// assertion keeps a cookie from outliving its session or vice versa.
func TestSessionTTLsAgreeAcrossPackages(t *testing.T) {
	if SessionTTL != auth.SessionTTL {
		t.Fatalf("api.SessionTTL = %v, auth.SessionTTL = %v; the cookie and the row must expire together", SessionTTL, auth.SessionTTL)
	}
}

func TestSignupSendsTheVerificationMail(t *testing.T) {
	h, sender, _ := authTestServer(t)
	email := authTestEmail(t)

	rec := authDo(t, h, http.MethodPost, "/api/auth/signup", authSignupBody{email, authTestPassword, "Test User"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	m := sender.last(t)
	if m.To != email {
		t.Errorf("mail sent to %q, want %q", m.To, email)
	}
	if m.Subject != "Verify your Drive account" {
		t.Errorf("subject = %q, want %q", m.Subject, "Verify your Drive account")
	}
	if !strings.Contains(m.Body, authTestBaseURL+"/verify?token=") {
		t.Errorf("body does not carry %s/verify?token=<token>:\n%s", authTestBaseURL, m.Body)
	}

	// The raw token must be the mail's alone: the row holds only its hash.
	token := authTokenFromMail(t, m.Body)
	var leaked int
	if err := authTestPool(t).QueryRow(t.Context(),
		`SELECT count(*) FROM email_tokens WHERE encode(token_hash, 'escape') = $1`, token,
	).Scan(&leaked); err != nil {
		t.Fatalf("querying email_tokens: %v", err)
	}
	if leaked != 0 {
		t.Error("the raw verification token is stored in email_tokens")
	}
}

// Signup must not tell an attacker which addresses have accounts.
func TestSignupOnAnExistingAddressIsIndistinguishable(t *testing.T) {
	h, sender, pool := authTestServer(t)
	email := authTestEmail(t)
	body := authSignupBody{email, authTestPassword, "Test User"}

	first := authDo(t, h, http.MethodPost, "/api/auth/signup", body, nil)
	second := authDo(t, h, http.MethodPost, "/api/auth/signup", authSignupBody{email, "a different password", "Someone Else"}, nil)

	if first.Code != second.Code {
		t.Errorf("statuses differ: %d then %d", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("bodies differ:\n first: %s\nsecond: %s", first.Body.String(), second.Body.String())
	}
	if n := len(sender.all()); n != 1 {
		t.Errorf("%d mails sent for two signups on one address, want 1", n)
	}

	// The second signup must not have touched the account.
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE email = $1`, email).Scan(&count); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if count != 1 {
		t.Errorf("%d users for %s, want 1", count, email)
	}
}

func TestSignupRejectsBadInput(t *testing.T) {
	h, sender, _ := authTestServer(t)

	cases := []struct {
		name string
		body authSignupBody
	}{
		{"no email", authSignupBody{"", authTestPassword, "Test User"}},
		{"not an address", authSignupBody{"nope", authTestPassword, "Test User"}},
		{"address with a display part", authSignupBody{"Someone <a@b.test>", authTestPassword, "Test User"}},
		{"header injection in the address", authSignupBody{"a@b.test\r\nBcc: evil@x.test", authTestPassword, "Test User"}},
		{"password too short", authSignupBody{"short@drive.test", "1234567", "Test User"}},
		{"no password", authSignupBody{"nopass@drive.test", "", "Test User"}},
		{"no display name", authSignupBody{"noname@drive.test", authTestPassword, "   "}},
	}

	for _, c := range cases {
		rec := authDo(t, h, http.MethodPost, "/api/auth/signup", c.body, nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422 (body %s)", c.name, rec.Code, rec.Body.String())
		}
		if got := decodeErr(t, rec.Body.String()).Code; got != CodeInvalid {
			t.Errorf("%s: code %q, want %q", c.name, got, CodeInvalid)
		}
	}
	if n := len(sender.all()); n != 0 {
		t.Errorf("%d mails sent for rejected signups, want 0", n)
	}
}

// Login requires email_verified_at: an account that has not been through the
// mail loop cannot be signed into, however correct the password is.
func TestLoginIsRefusedUntilTheEmailIsVerified(t *testing.T) {
	h, sender, _ := authTestServer(t)
	email := authTestEmail(t)

	authDo(t, h, http.MethodPost, "/api/auth/signup", authSignupBody{email, authTestPassword, "Test User"}, nil)

	rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	e := decodeErr(t, rec.Body.String())
	if e.Code != CodeUnauthorized {
		t.Errorf("code %q, want %q", e.Code, CodeUnauthorized)
	}
	if !strings.Contains(strings.ToLower(e.Message), "verify") {
		t.Errorf("message %q does not tell the user to verify their address", e.Message)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie && c.Value != "" {
			t.Error("an unverified login set a session cookie")
		}
	}

	// Verifying then logging in works.
	token := authTokenFromMail(t, sender.last(t).Body)
	if rec := authDo(t, h, http.MethodPost, "/api/auth/verify-email", map[string]string{"token": token}, nil); rec.Code != http.StatusOK {
		t.Fatalf("verify-email: status %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil); rec.Code != http.StatusOK {
		t.Fatalf("login after verification: status %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestVerifyEmailRejectsUnusableTokens(t *testing.T) {
	h, sender, pool := authTestServer(t)
	email := authTestEmail(t)
	authDo(t, h, http.MethodPost, "/api/auth/signup", authSignupBody{email, authTestPassword, "Test User"}, nil)
	good := authTokenFromMail(t, sender.last(t).Body)

	for _, c := range []struct{ name, token string }{
		{"empty", ""},
		{"unknown", "SDCJcz1x2fZ1Zc1SXNvyaXhOaW5rd3ZjaGFyc2hlcmU"},
		{"truncated", good[:len(good)-1]},
	} {
		rec := authDo(t, h, http.MethodPost, "/api/auth/verify-email", map[string]string{"token": c.token}, nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422 (body %s)", c.name, rec.Code, rec.Body.String())
		}
	}

	// Expired, by backdating the row rather than sleeping: every expiry here is
	// a stored timestamp compared against now(), so a test can age it directly.
	if _, err := pool.Exec(t.Context(),
		`UPDATE email_tokens SET expires_at = now() - interval '1 minute' WHERE token_hash = $1`, auth.HashToken(good),
	); err != nil {
		t.Fatalf("backdating the token: %v", err)
	}
	if rec := authDo(t, h, http.MethodPost, "/api/auth/verify-email", map[string]string{"token": good}, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("expired token: status %d, want 422", rec.Code)
	}
}

func TestLoginSetsTheSessionCookie(t *testing.T) {
	h, sender, _ := authTestServer(t)
	email := authVerifiedUser(t, h, sender)

	rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	c := authCookie(t, rec)
	if !c.HttpOnly {
		t.Error("cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	// Safari drops Secure cookies on plain-http localhost, so Secure follows
	// the scheme of DRIVE_BASE_URL and nothing else.
	if c.Secure {
		t.Error("cookie is Secure on an http:// base URL")
	}
	if strings.HasPrefix(c.Name, "__Host-") || strings.HasPrefix(c.Name, "__Secure-") {
		t.Errorf("cookie name %q uses a prefix Chrome rejects on http://localhost", c.Name)
	}
	if want := int(SessionTTL.Seconds()); c.MaxAge != want {
		t.Errorf("MaxAge = %d, want %d", c.MaxAge, want)
	}
}

func TestLoginSetsASecureCookieOnAnHTTPSBaseURL(t *testing.T) {
	h, sender, _ := authTestServerWithBaseURL(t, "https://drive.example.com")
	email := authVerifiedUser(t, h, sender)

	rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if !authCookie(t, rec).Secure {
		t.Error("cookie is not Secure on an https:// base URL")
	}
}

func TestMeReturnsTheAccountAndItsRootFolder(t *testing.T) {
	h, sender, pool := authTestServer(t)
	email := authVerifiedUser(t, h, sender)

	login := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil)
	cookie := authCookie(t, login)

	rec := authDo(t, h, http.MethodGet, "/api/auth/me", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	var me authMeDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decoding /me: %v (body %s)", err, rec.Body.String())
	}
	if me.Email != email {
		t.Errorf("email = %q, want %q", me.Email, email)
	}
	if me.DisplayName != "Test User" {
		t.Errorf("display_name = %q, want %q", me.DisplayName, "Test User")
	}
	if me.EmailVerifiedAt == nil {
		t.Error("email_verified_at is null after verification")
	}
	if me.RootID == uuid.Nil {
		t.Fatal("root_id is missing")
	}

	var kind, name string
	if err := pool.QueryRow(t.Context(),
		`SELECT kind, name FROM nodes WHERE id = $1 AND owner_id = $2 AND parent_id IS NULL`, me.RootID, me.ID,
	).Scan(&kind, &name); err != nil {
		t.Fatalf("root_id does not name the caller's root folder: %v", err)
	}
	if kind != "folder" || name != "My Drive" {
		t.Errorf("root node = %s %q, want folder \"My Drive\"", kind, name)
	}

	// Login answers with the same shape, so the SPA needs no second request.
	if login.Body.String() != rec.Body.String() {
		t.Errorf("login body and /me body differ:\nlogin: %s\n  /me: %s", login.Body.String(), rec.Body.String())
	}
}

func TestMeRejectsAnonymousCallers(t *testing.T) {
	h, _, _ := authTestServer(t)

	rec := authDo(t, h, http.MethodGet, "/api/auth/me", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if got := decodeErr(t, rec.Body.String()).Code; got != CodeUnauthorized {
		t.Errorf("code %q, want %q", got, CodeUnauthorized)
	}
}

func TestLoginRejectsAWrongPasswordWithoutSayingWhy(t *testing.T) {
	h, sender, _ := authTestServer(t)
	email := authVerifiedUser(t, h, sender)

	wrongPassword := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, "not the password"}, nil)
	noSuchUser := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{authTestEmail(t), authTestPassword}, nil)

	for name, rec := range map[string]*httptest.ResponseRecorder{"wrong password": wrongPassword, "no such user": noSuchUser} {
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
		}
	}
	if wrongPassword.Body.String() != noSuchUser.Body.String() {
		t.Errorf("a wrong password and an unknown address answer differently:\n  wrong password: %s\n    no such user: %s",
			wrongPassword.Body.String(), noSuchUser.Body.String())
	}
}

// The login budget: scope 'login', keyed by email, 10 failures per 15 minutes,
// auto-clearing. Auto-clearing is the point -- a success outside an active
// window is never blocked, so there is no permanent lockout to unlock.
func TestLoginLocksOutAfterTenFailuresAndClearsWithTheWindow(t *testing.T) {
	h, sender, pool := authTestServer(t)
	email := authVerifiedUser(t, h, sender)

	for i := range auth.LoginFailLimit {
		rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, "not the password"}, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401 (body %s)", i+1, rec.Code, rec.Body.String())
		}
	}

	// The budget is spent, so even the right password is refused.
	rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password inside the lockout window: status %d, want 429 (body %s)", rec.Code, rec.Body.String())
	}
	if got := decodeErr(t, rec.Body.String()).Code; got != CodeRateLimited {
		t.Errorf("code %q, want %q", got, CodeRateLimited)
	}

	// Age the window out rather than sleeping it -- the window is a stored
	// timestamp compared against now(). The lockout must clear itself: there is
	// no unlock step anywhere.
	tag, err := pool.Exec(t.Context(),
		`UPDATE throttle SET window_start = window_start - make_interval(secs => $2)
		  WHERE scope = 'login' AND key = $1`,
		email, (auth.LoginFailWindow + time.Minute).Seconds())
	if err != nil {
		t.Fatalf("backdating the throttle window: %v", err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatal("no throttle rows for scope 'login' -- failures are not being counted durably")
	}

	rec = authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct password after the window lapsed: status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// users.email is citext, so User@x.test and user@x.test are one account. The
// throttle key is plain text, so unless the address is canonicalised first,
// every case permutation of an address buys a fresh 10-attempt budget against
// the same password.
func TestLoginLockoutCannotBeBypassedByChangingTheCase(t *testing.T) {
	h, sender, _ := authTestServer(t)
	email := authVerifiedUser(t, h, sender)

	for i := range auth.LoginFailLimit {
		rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{strings.ToUpper(email), "not the password"}, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d against the upper-case address: status %d, want 401 (body %s)", i+1, rec.Code, rec.Body.String())
		}
	}

	rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("failures spent under one casing did not lock the account under another: status %d, want 429 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestLogoutRevokesTheSessionAndClearsTheCookie(t *testing.T) {
	h, sender, pool := authTestServer(t)
	email := authVerifiedUser(t, h, sender)

	login := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil)
	cookie := authCookie(t, login)

	rec := authDo(t, h, http.MethodPost, "/api/auth/logout", nil, cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}

	cleared := authCookie(t, rec)
	if cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Errorf("logout cookie = {value:%q max_age:%d}, want an empty value and a negative MaxAge", cleared.Value, cleared.MaxAge)
	}

	var left int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM auth_sessions WHERE token_hash = $1`, auth.HashToken(cookie.Value),
	).Scan(&left); err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	if left != 0 {
		t.Error("the session row survived logout -- the cookie alone was cleared")
	}

	// The old cookie is now worthless, and logging out again still answers 204.
	if rec := authDo(t, h, http.MethodGet, "/api/auth/me", nil, cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("/me with a revoked cookie: status %d, want 401", rec.Code)
	}
	if rec := authDo(t, h, http.MethodPost, "/api/auth/logout", nil, cookie); rec.Code != http.StatusNoContent {
		t.Errorf("second logout: status %d, want 204", rec.Code)
	}
}

func TestAnExpiredSessionIsRejected(t *testing.T) {
	h, sender, pool := authTestServer(t)
	email := authVerifiedUser(t, h, sender)

	login := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil)
	cookie := authCookie(t, login)

	if _, err := pool.Exec(t.Context(),
		`UPDATE auth_sessions SET expires_at = now() - interval '1 minute' WHERE token_hash = $1`,
		auth.HashToken(cookie.Value),
	); err != nil {
		t.Fatalf("backdating expires_at: %v", err)
	}

	if rec := authDo(t, h, http.MethodGet, "/api/auth/me", nil, cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/me with an expired session: status %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
}

// The 30 days slide: an account in daily use never has to sign in again.
func TestUsingASessionSlidesItsExpiry(t *testing.T) {
	h, sender, pool := authTestServer(t)
	email := authVerifiedUser(t, h, sender)

	login := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil)
	cookie := authCookie(t, login)
	hash := auth.HashToken(cookie.Value)

	// Nearly expired, and last seen long enough ago that the hourly guard on
	// the slide does not suppress the write.
	if _, err := pool.Exec(t.Context(),
		`UPDATE auth_sessions SET expires_at = now() + interval '1 day', last_seen_at = now() - interval '2 hours'
		  WHERE token_hash = $1`, hash,
	); err != nil {
		t.Fatalf("backdating the session: %v", err)
	}

	if rec := authDo(t, h, http.MethodGet, "/api/auth/me", nil, cookie); rec.Code != http.StatusOK {
		t.Fatalf("/me: status %d, body %s", rec.Code, rec.Body.String())
	}

	var expiresAt time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT expires_at FROM auth_sessions WHERE token_hash = $1`, hash,
	).Scan(&expiresAt); err != nil {
		t.Fatalf("reading expires_at: %v", err)
	}
	if d := time.Until(expiresAt); d < SessionTTL-time.Hour {
		t.Errorf("session expires in %v after use, want it slid back to about %v", d, SessionTTL)
	}
}

// The mail budget: scope 'email_send', keyed by email, 5 per hour. Signup mail is charged to it,
// and a spent budget skips the send without changing what the caller sees.
func TestVerificationMailIsCappedPerAddress(t *testing.T) {
	h, sender, pool := authTestServer(t)
	email := authTestEmail(t)

	for range auth.EmailSendLimit {
		if _, err := auth.Bump(t.Context(), pool, auth.ScopeEmailSend, email, auth.EmailSendWindow); err != nil {
			t.Fatalf("Bump: %v", err)
		}
	}

	rec := authDo(t, h, http.MethodPost, "/api/auth/signup", authSignupBody{email, authTestPassword, "Test User"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 -- a spent mail budget must not be visible to the caller (body %s)", rec.Code, rec.Body.String())
	}
	if n := len(sender.all()); n != 0 {
		t.Errorf("%d mails sent past the budget, want 0", n)
	}

	// The account still exists, so the user is not locked out forever: once the
	// window lapses a resend works.
	var users int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE email = $1`, email).Scan(&users); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if users != 1 {
		t.Errorf("%d users created, want 1", users)
	}
}

// ------------------------------------------------- session loader failures --

// TestSessionLoaderDBFailureIsRetryable pins the difference between "we do not
// know who you are" and "we could not find out".
//
// sessionLoader used to treat every error from the session query as "no user",
// so a transient database error reached the client as 401 unauthorized. Neither
// client retries a 401 -- it means the credentials are bad -- so one hiccup
// during a 50 GB upload ends it with no recovery path. The answer has to be a
// retryable 5xx.
func TestSessionLoaderDBFailureIsRetryable(t *testing.T) {
	// A pool of this suite's own, closed on purpose: every query through it
	// fails at acquire time, which is what an unreachable database looks like
	// from inside a handler. The shared pool must not be touched -- closing it
	// would take the rest of the package down with it.
	broken, err := db.Connect(context.Background(), authTestDSN)
	if err != nil {
		t.Fatalf("drive-test database: %v", err)
	}
	broken.Close()

	h := New(&config.Config{BaseURL: authTestBaseURL}, broken, nil, nil, nil, nil).Routes()

	rec := authDo(t, h, http.MethodGet, "/api/auth/me", nil,
		&http.Cookie{Name: SessionCookie, Value: "a-cookie-we-cannot-look-up"})

	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("a database failure answered 401 (body %s); 401 is terminal for both clients", rec.Body)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 (body %s)", rec.Code, rec.Body)
	}
	if got := decodeErr(t, rec.Body.String()).Code; got != CodeInternal {
		t.Errorf("code = %q, want %q", got, CodeInternal)
	}

	// An anonymous request is unaffected: no cookie, no lookup, no dependency on
	// the database at this layer.
	anon := authDo(t, h, http.MethodGet, "/api/auth/me", nil, nil)
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous request answered %d, want 401 (body %s)", anon.Code, anon.Body)
	}
}
