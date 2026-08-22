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
	"unicode/utf8"

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
	// gate, when non-nil, holds every Send until it is closed. It is how a test
	// observes that a response arrived while a send was still in flight, which
	// is the only way to see the difference between a send that is dispatched
	// off the request goroutine and one that is not.
	gate chan struct{}
}

type authMail struct{ To, Subject, Body string }

func (s *authRecordingSender) Send(_ context.Context, to, subject, body string) error {
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		<-gate // never under the lock: all() has to stay readable meanwhile
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, authMail{To: to, Subject: subject, Body: body})
	return nil
}

// hold makes every subsequent Send block until release is called.
func (s *authRecordingSender) hold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gate == nil {
		s.gate = make(chan struct{})
	}
}

// release lets the held sends through. It is idempotent, so a test can both
// call it at the point it means to and register it as a cleanup -- a t.Fatal
// between the two would otherwise leave a goroutine parked on the gate forever.
func (s *authRecordingSender) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gate != nil {
		close(s.gate)
		s.gate = nil
	}
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

// waitForTo returns the most recent message sent to an address with a given
// subject, failing the test if none arrives. Like last, it polls, because every
// send in this package is dispatched off the request goroutine on purpose.
func (s *authRecordingSender) waitForTo(t *testing.T, to, subject string) authMail {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if all := s.matching(to, subject); len(all) > 0 {
			return all[len(all)-1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q mail to %s within 3s (sent: %d)", subject, to, len(s.all()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForCountTo blocks until exactly n messages of this subject have gone to
// an address.
func (s *authRecordingSender) waitForCountTo(t *testing.T, to, subject string, n int) []authMail {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		all := s.matching(to, subject)
		if len(all) >= n {
			return all
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d %q mails to %s within 3s, want %d", len(all), subject, to, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *authRecordingSender) matching(to, subject string) []authMail {
	var out []authMail
	for _, m := range s.all() {
		if m.To == to && m.Subject == subject {
			out = append(out, m)
		}
	}
	return out
}

// authSettle gives an off-request goroutine a bounded moment to do whatever it
// was going to do.
//
// It is only ever used before asserting that something did NOT happen. Anything
// that must happen is waited for on its own signal instead, so no assertion in
// this file passes merely because a sleep was long enough.
func authSettle() { time.Sleep(300 * time.Millisecond) }

const authTestBaseURL = "http://localhost:8081"

func authTestServer(t *testing.T) (http.Handler, *authRecordingSender, *pgxpool.Pool) {
	t.Helper()
	return authTestServerWithBaseURL(t, authTestBaseURL)
}

func authTestServerWithBaseURL(t *testing.T, baseURL string) (http.Handler, *authRecordingSender, *pgxpool.Pool) {
	t.Helper()
	return authTestServerWithConfig(t, &config.Config{BaseURL: baseURL})
}

// authTestServerWithConfig builds a server from an explicit config.
//
// Every server in this package gets one, and never the process environment:
// .env.test raises DRIVE_AUTH_RATE_PER_MIN to 100000 so the whole suite can run
// from one address, which would make any assertion about a bucket pass
// unconditionally. The buckets are per-Server, so each test also starts with
// full ones.
func authTestServerWithConfig(t *testing.T, cfg *config.Config) (http.Handler, *authRecordingSender, *pgxpool.Pool) {
	t.Helper()
	pool := authTestPool(t)
	sender := &authRecordingSender{}
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

	// Waited for by address, not by "the newest mail": a test that builds two
	// accounts against one sender would otherwise redeem the first one's token
	// twice and fail somewhere far from the cause.
	token := authTokenFromMail(t, sender.waitForTo(t, email, verifySubject).Body)
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
	return authLinkToken(t, body, "/verify?token=")
}

// authResetTokenFromMail does the same for the reset link, ${DRIVE_BASE_URL}
// /reset?token=<raw token>, which the SPA route /reset reads.
func authResetTokenFromMail(t *testing.T, body string) string {
	t.Helper()
	return authLinkToken(t, body, "/reset?token=")
}

func authLinkToken(t *testing.T, body, marker string) string {
	t.Helper()
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
	// Its own code, not the generic one. This is the single login refusal a
	// client is allowed to act on -- it offers to resend the link -- and the
	// SPA must not have to match on the English to find it.
	e := decodeErr(t, rec.Body.String())
	if e.Code != CodeEmailUnverified {
		t.Errorf("code %q, want %q", e.Code, CodeEmailUnverified)
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

// ------------------------------------------------------- the account itself --

// authLogin signs an existing account in and returns its session cookie.
func authLoginAs(t *testing.T, h http.Handler, email, password string) *http.Cookie {
	t.Helper()
	rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, password}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body.String())
	}
	return authCookie(t, rec)
}

// authSignedIn builds a verified account and signs it in.
func authSignedIn(t *testing.T, h http.Handler, sender *authRecordingSender) (string, *http.Cookie) {
	t.Helper()
	email := authVerifiedUser(t, h, sender)
	return email, authLoginAs(t, h, email, authTestPassword)
}

// authMeStatus is how a cookie is checked for life: 200 if the session is still
// there, 401 once it has been revoked.
func authMeStatus(t *testing.T, h http.Handler, cookie *http.Cookie) int {
	t.Helper()
	return authDo(t, h, http.MethodGet, "/api/auth/me", nil, cookie).Code
}

type authChangePasswordBody struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type authResetConfirmBody struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

type authSessionsDTO struct {
	Items []struct {
		ID         uuid.UUID  `json:"id"`
		CreatedAt  time.Time  `json:"created_at"`
		LastSeenAt *time.Time `json:"last_seen_at"`
		IP         *string    `json:"ip"`
		UserAgent  *string    `json:"user_agent"`
		Current    bool       `json:"current"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func authListSessionsOf(t *testing.T, h http.Handler, cookie *http.Cookie) authSessionsDTO {
	t.Helper()
	rec := authDo(t, h, http.MethodGet, "/api/auth/sessions", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/sessions: status %d, body %s", rec.Code, rec.Body.String())
	}
	var out authSessionsDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding the session list: %v (body %s)", err, rec.Body.String())
	}
	return out
}

func TestUpdateMeRenamesTheAccount(t *testing.T) {
	h, sender, _ := authTestServer(t)
	_, cookie := authSignedIn(t, h, sender)

	rec := authDo(t, h, http.MethodPatch, "/api/auth/me", map[string]string{"display_name": "  Renamed Person  "}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	// The response carries the new name, so the caller's cached copy -- and the
	// initials in its avatar -- refresh without a second round trip.
	var me authMeDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decoding the response: %v (body %s)", err, rec.Body.String())
	}
	if me.DisplayName != "Renamed Person" {
		t.Errorf("display_name = %q, want the trimmed %q", me.DisplayName, "Renamed Person")
	}

	// And it stuck.
	var stored authMeDTO
	next := authDo(t, h, http.MethodGet, "/api/auth/me", nil, cookie)
	if err := json.Unmarshal(next.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decoding /me: %v (body %s)", err, next.Body.String())
	}
	if stored.DisplayName != "Renamed Person" {
		t.Errorf("/me display_name = %q after the rename", stored.DisplayName)
	}

	for _, c := range []struct {
		name string
		body map[string]string
	}{
		{"blank", map[string]string{"display_name": "   "}},
		{"unknown field", map[string]string{"display_nam": "typo"}},
	} {
		if rec := authDo(t, h, http.MethodPatch, "/api/auth/me", c.body, cookie); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422 (body %s)", c.name, rec.Code, rec.Body.String())
		}
	}
}

// A password change signs out every OTHER browser and leaves this one alone.
// Both halves matter: signing out the rest is the whole point of changing a
// password after a scare, and signing out the person who just typed it is a
// bug that reads as one.
func TestChangePasswordKeepsThisSessionAndRevokesTheRest(t *testing.T) {
	h, sender, _ := authTestServer(t)
	email, mine := authSignedIn(t, h, sender)
	other := authLoginAs(t, h, email, authTestPassword)
	third := authLoginAs(t, h, email, authTestPassword)

	const newPassword = "a completely different passphrase"
	rec := authDo(t, h, http.MethodPost, "/api/auth/password",
		authChangePasswordBody{authTestPassword, newPassword}, mine)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}

	if got := authMeStatus(t, h, mine); got != http.StatusOK {
		t.Errorf("the caller's own session answered %d after changing its password, want 200", got)
	}
	for name, cookie := range map[string]*http.Cookie{"second": other, "third": third} {
		if got := authMeStatus(t, h, cookie); got != http.StatusUnauthorized {
			t.Errorf("the %s session answered %d after the password change, want 401", name, got)
		}
	}

	// The new password is the one that works.
	if rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, newPassword}, nil); rec.Code != http.StatusOK {
		t.Errorf("the new password did not sign in: status %d (body %s)", rec.Code, rec.Body.String())
	}
	if rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old password still signs in: status %d", rec.Code)
	}
}

// A wrong current password is charged to a budget of its own, keyed by user id.
//
// Charging the login budget instead -- which is keyed by email -- would mean
// ten mistyped current passwords locked the account out of signing in for
// fifteen minutes, and would hand anybody who stole a session an easy way to do
// it on purpose.
func TestChangePasswordChargesItsOwnBudgetNotLogin(t *testing.T) {
	h, sender, pool := authTestServer(t)
	email, cookie := authSignedIn(t, h, sender)

	var me authMeDTO
	if err := json.Unmarshal(authDo(t, h, http.MethodGet, "/api/auth/me", nil, cookie).Body.Bytes(), &me); err != nil {
		t.Fatalf("decoding /me: %v", err)
	}
	key := me.ID.String()

	before, err := auth.Count(t.Context(), pool, auth.ScopeLogin, email, auth.LoginFailWindow)
	if err != nil {
		t.Fatalf("counting the login budget: %v", err)
	}

	rec := authDo(t, h, http.MethodPost, "/api/auth/password",
		authChangePasswordBody{"not the current password", "a completely different passphrase"}, cookie)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}

	charged, err := auth.Count(t.Context(), pool, auth.ScopePasswordChange, key, auth.PasswordChangeFailWindow)
	if err != nil {
		t.Fatalf("counting the password-change budget: %v", err)
	}
	if charged != 1 {
		t.Errorf("the password-change budget counted %d, want 1", charged)
	}

	after, err := auth.Count(t.Context(), pool, auth.ScopeLogin, email, auth.LoginFailWindow)
	if err != nil {
		t.Fatalf("counting the login budget: %v", err)
	}
	if after != before {
		t.Errorf("the login budget went from %d to %d; a wrong current password must not spend it", before, after)
	}

	// And the account is not locked out of signing in.
	if rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil); rec.Code != http.StatusOK {
		t.Errorf("login after a failed password change: status %d (body %s)", rec.Code, rec.Body.String())
	}

	// The password itself is untouched.
	if rec := authDo(t, h, http.MethodPost, "/api/auth/password",
		authChangePasswordBody{authTestPassword, "a completely different passphrase"}, cookie); rec.Code != http.StatusNoContent {
		t.Errorf("the real current password was refused afterwards: status %d (body %s)", rec.Code, rec.Body.String())
	}
}

func TestSessionsListMarksTheCurrentSession(t *testing.T) {
	h, sender, _ := authTestServer(t)
	email, mine := authSignedIn(t, h, sender)
	other := authLoginAs(t, h, email, authTestPassword)

	list := authListSessionsOf(t, h, mine)
	if len(list.Items) != 2 {
		t.Fatalf("%d sessions listed, want 2", len(list.Items))
	}
	if list.NextCursor != nil {
		t.Errorf("next_cursor = %v, want null -- the list is one page", *list.NextCursor)
	}

	current := 0
	for _, item := range list.Items {
		if item.Current {
			current++
		}
		if item.CreatedAt.IsZero() {
			t.Error("a session row carries no created_at")
		}
	}
	if current != 1 {
		t.Errorf("%d sessions are marked current, want exactly 1", current)
	}

	// Revoking the other one takes it off the list and kills its cookie.
	var otherID uuid.UUID
	for _, item := range list.Items {
		if !item.Current {
			otherID = item.ID
		}
	}
	rec := authDo(t, h, http.MethodDelete, "/api/auth/sessions/"+otherID.String(), nil, mine)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoking a session: status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := authMeStatus(t, h, other); got != http.StatusUnauthorized {
		t.Errorf("the revoked session answered %d, want 401", got)
	}
	if got := authMeStatus(t, h, mine); got != http.StatusOK {
		t.Errorf("the caller's own session answered %d, want 200", got)
	}
	if n := len(authListSessionsOf(t, h, mine).Items); n != 1 {
		t.Errorf("%d sessions listed after revoking one, want 1", n)
	}
}

// The owner predicate on the revoke is the authorization. Without it, anybody
// holding a session id could sign anybody else out.
func TestRevokingSomebodyElsesSessionIsNotFound(t *testing.T) {
	h, sender, _ := authTestServer(t)
	_, mine := authSignedIn(t, h, sender)
	_, theirs := authSignedIn(t, h, sender)

	var victim uuid.UUID
	for _, item := range authListSessionsOf(t, h, theirs).Items {
		if item.Current {
			victim = item.ID
		}
	}
	if victim == uuid.Nil {
		t.Fatal("the other account's session was not listed")
	}

	rec := authDo(t, h, http.MethodDelete, "/api/auth/sessions/"+victim.String(), nil, mine)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if got := authMeStatus(t, h, theirs); got != http.StatusOK {
		t.Errorf("the other account's session answered %d; it was revoked by a stranger", got)
	}

	// A malformed id is the same 404: an id that is not a uuid and an id that
	// is not yours must not be distinguishable.
	if rec := authDo(t, h, http.MethodDelete, "/api/auth/sessions/not-a-uuid", nil, mine); rec.Code != http.StatusNotFound {
		t.Errorf("a malformed id answered %d, want 404", rec.Code)
	}
}

func TestLogoutAllRevokesEverySessionIncludingThisOne(t *testing.T) {
	h, sender, _ := authTestServer(t)
	email, mine := authSignedIn(t, h, sender)
	other := authLoginAs(t, h, email, authTestPassword)

	rec := authDo(t, h, http.MethodPost, "/api/auth/logout-all", nil, mine)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	for name, cookie := range map[string]*http.Cookie{"this": mine, "other": other} {
		if got := authMeStatus(t, h, cookie); got != http.StatusUnauthorized {
			t.Errorf("the %s session answered %d after logout-all, want 401", name, got)
		}
	}

	// And the cookie is cleared, so the browser stops sending a dead credential.
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie && c.Value == "" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout-all did not clear the session cookie")
	}
}

// -------------------------------------------------- reset and resend --------

// A reset for an address nobody owns must cost nothing.
//
// Everything conditional runs off the request goroutine, so the response says
// the same thing either way -- and the service-wide daily budget, which is the
// scarce thing a stranger could spend, is only charged once there is somebody
// to send to. A sweep through addresses that do not exist must not be able to
// silence the real users' mail.
func TestPasswordResetForAnUnknownAddressCostsNothing(t *testing.T) {
	h, sender, pool := authTestServerWithConfig(t,
		&config.Config{BaseURL: authTestBaseURL, EmailDailyCap: 10})
	known := authVerifiedUser(t, h, sender)
	unknown := authTestEmail(t)

	// One row for the whole service, so the test owns the scope and clears it
	// after the signup mail above rather than keying it.
	if _, err := pool.Exec(t.Context(), `DELETE FROM throttle WHERE scope = $1`, auth.ScopeEmailSendGlobal); err != nil {
		t.Fatalf("clearing the global mail budget: %v", err)
	}

	miss := authDo(t, h, http.MethodPost, "/api/auth/password-reset", map[string]string{"email": unknown}, nil)
	hit := authDo(t, h, http.MethodPost, "/api/auth/password-reset", map[string]string{"email": known}, nil)

	if miss.Code != http.StatusOK || hit.Code != http.StatusOK {
		t.Fatalf("statuses %d (unknown) and %d (known), want 200 for both", miss.Code, hit.Code)
	}
	if miss.Body.String() != hit.Body.String() {
		t.Errorf("the bodies differ:\nunknown: %s\n  known: %s", miss.Body.String(), hit.Body.String())
	}

	// Wait on the send that must happen, then give the one that must not a
	// moment to prove it did not.
	sender.waitForTo(t, known, resetSubject)
	authSettle()

	spent, err := auth.Count(t.Context(), pool, auth.ScopeEmailSendGlobal, auth.GlobalKey, auth.EmailSendGlobalWindow)
	if err != nil {
		t.Fatalf("reading the global mail budget: %v", err)
	}
	if spent != 1 {
		t.Errorf("the service-wide budget counted %d for one real address and one unknown one, want 1", spent)
	}

	stray, err := auth.Count(t.Context(), pool, auth.ScopeEmailSendReset, unknown, auth.EmailSendWindow)
	if err != nil {
		t.Fatalf("reading the unknown address's budget: %v", err)
	}
	if stray != 0 {
		t.Errorf("the unknown address's own budget counted %d, want 0", stray)
	}
	if n := len(sender.matching(unknown, resetSubject)); n != 0 {
		t.Errorf("%d reset mails went to an address with no account", n)
	}
}

// Reset and resend-verification are charged to separate per-recipient budgets.
//
// Sharing one would make each a way to suppress the other: five reset requests
// against a stranger's address would swallow the verification mail they are
// sitting there waiting for.
func TestResetAndVerifyBudgetsDoNotShareAScope(t *testing.T) {
	h, sender, pool := authTestServer(t)

	// An account that has signed up but never confirmed -- the one that still
	// needs its verification link.
	email := authTestEmail(t)
	if rec := authDo(t, h, http.MethodPost, "/api/auth/signup",
		authSignupBody{email, authTestPassword, "Test User"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("signup: status %d, body %s", rec.Code, rec.Body.String())
	}
	sender.waitForTo(t, email, verifySubject)

	// Spend the whole reset budget through the real endpoint. Five an hour is
	// also exactly what the per-IP mail bucket allows, which is why the resend
	// below goes through a second server: same database, its own bucket.
	for i := range auth.EmailSendLimit {
		rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset", map[string]string{"email": email}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("reset %d: status %d, body %s", i+1, rec.Code, rec.Body.String())
		}
	}
	sender.waitForCountTo(t, email, resetSubject, auth.EmailSendLimit)

	spent, err := auth.Count(t.Context(), pool, auth.ScopeEmailSendReset, email, auth.EmailSendWindow)
	if err != nil {
		t.Fatalf("reading the reset budget: %v", err)
	}
	if spent != auth.EmailSendLimit {
		t.Fatalf("the reset budget counted %d, want the limit %d", spent, auth.EmailSendLimit)
	}
	stray, err := auth.Count(t.Context(), pool, auth.ScopeEmailSendVerify, email, auth.EmailSendWindow)
	if err != nil {
		t.Fatalf("reading the verify budget: %v", err)
	}
	if stray != 0 {
		t.Errorf("five resets spent %d of the verification budget, want 0", stray)
	}

	// The verification link still goes out, and still works.
	h2, sender2, _ := authTestServer(t)
	if rec := authDo(t, h2, http.MethodPost, "/api/auth/resend-verification",
		map[string]string{"email": email}, nil); rec.Code != http.StatusOK {
		t.Fatalf("resend-verification: status %d, body %s", rec.Code, rec.Body.String())
	}
	m := sender2.waitForTo(t, email, verifySubject)

	token := authTokenFromMail(t, m.Body)
	if rec := authDo(t, h2, http.MethodPost, "/api/auth/verify-email",
		map[string]string{"token": token}, nil); rec.Code != http.StatusOK {
		t.Fatalf("the resent link did not verify: status %d, body %s", rec.Code, rec.Body.String())
	}
}

// A reset link is good for an hour, not for the two days a verification link
// gets. A verification link only proves an address; a reset link is the
// account.
func TestResetTokenDiesAfterAnHour(t *testing.T) {
	h, sender, pool := authTestServer(t)
	email := authVerifiedUser(t, h, sender)

	if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset",
		map[string]string{"email": email}, nil); rec.Code != http.StatusOK {
		t.Fatalf("password-reset: status %d, body %s", rec.Code, rec.Body.String())
	}
	token := authResetTokenFromMail(t, sender.waitForTo(t, email, resetSubject).Body)

	// Move the row ninety minutes into the past, whole. created_at and
	// expires_at shift together, so what this tests is the interval the server
	// chose and not a value the test wrote.
	const backdate = `
		UPDATE email_tokens
		   SET created_at = created_at - interval '90 minutes',
		       expires_at = expires_at - interval '90 minutes'
		 WHERE token_hash = $1`
	if _, err := pool.Exec(t.Context(), backdate, auth.HashToken(token)); err != nil {
		t.Fatalf("backdating the token: %v", err)
	}

	const newPassword = "a completely different passphrase"
	rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset/confirm",
		authResetConfirmBody{token, newPassword}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a 90-minute-old reset link answered %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}
	if got := decodeErr(t, rec.Body.String()).Code; got != CodeInvalid {
		t.Errorf("code %q, want %q", got, CodeInvalid)
	}

	// And nothing changed: the old password still works, the new one does not.
	if rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil); rec.Code != http.StatusOK {
		t.Errorf("the original password stopped working: status %d", rec.Code)
	}
	if rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, newPassword}, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("an expired link changed the password anyway: login with it answered %d", rec.Code)
	}
}

// Confirming a reset ends every session and spends every other live link.
//
// Both are the same reasoning: a reset is what somebody does when they think
// another person is in their account, so nothing that existed before it may
// still work afterwards -- not a session, and not a second link an attacker
// asked for while the real user was reading their mail.
func TestResetConfirmRevokesEverySessionAndSpendsTheOtherLinks(t *testing.T) {
	h, sender, _ := authTestServer(t)
	email, mine := authSignedIn(t, h, sender)
	other := authLoginAs(t, h, email, authTestPassword)

	for i := range 2 {
		if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset",
			map[string]string{"email": email}, nil); rec.Code != http.StatusOK {
			t.Fatalf("password-reset %d: status %d, body %s", i+1, rec.Code, rec.Body.String())
		}
	}
	mails := sender.waitForCountTo(t, email, resetSubject, 2)
	first := authResetTokenFromMail(t, mails[0].Body)
	second := authResetTokenFromMail(t, mails[1].Body)

	const newPassword = "a completely different passphrase"
	rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset/confirm",
		authResetConfirmBody{second, newPassword}, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}

	for name, cookie := range map[string]*http.Cookie{"first": mine, "second": other} {
		if got := authMeStatus(t, h, cookie); got != http.StatusUnauthorized {
			t.Errorf("the %s session answered %d after a reset, want 401", name, got)
		}
	}

	// The sibling link is spent, so an older mail cannot be replayed against
	// the password that was just set.
	if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset/confirm",
		authResetConfirmBody{first, "yet another passphrase entirely"}, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("the other live reset link still worked: status %d (body %s)", rec.Code, rec.Body.String())
	}
	// Nor can the one that was used.
	if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset/confirm",
		authResetConfirmBody{second, "yet another passphrase entirely"}, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("the redeemed link worked twice: status %d", rec.Code)
	}

	if rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, newPassword}, nil); rec.Code != http.StatusOK {
		t.Errorf("the new password did not sign in: status %d (body %s)", rec.Code, rec.Body.String())
	}
	if rec := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old password still signs in: status %d", rec.Code)
	}
}

// A reset also confirms the address: whoever read that mailbox has just proven
// exactly what the verification link proves, and an unverified account that
// reset its password could otherwise never sign in at all.
func TestResetConfirmVerifiesTheAddress(t *testing.T) {
	h, sender, _ := authTestServer(t)
	email := authTestEmail(t)
	if rec := authDo(t, h, http.MethodPost, "/api/auth/signup",
		authSignupBody{email, authTestPassword, "Test User"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("signup: status %d, body %s", rec.Code, rec.Body.String())
	}
	sender.waitForTo(t, email, verifySubject)

	if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset",
		map[string]string{"email": email}, nil); rec.Code != http.StatusOK {
		t.Fatalf("password-reset: status %d, body %s", rec.Code, rec.Body.String())
	}
	token := authResetTokenFromMail(t, sender.waitForTo(t, email, resetSubject).Body)

	const newPassword = "a completely different passphrase"
	if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset/confirm",
		authResetConfirmBody{token, newPassword}, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("confirm: status %d, body %s", rec.Code, rec.Body.String())
	}

	login := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, newPassword}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("an account that reset its password cannot sign in: status %d (body %s)", login.Code, login.Body.String())
	}
	var me authMeDTO
	if err := json.Unmarshal(login.Body.Bytes(), &me); err != nil {
		t.Fatalf("decoding the login response: %v", err)
	}
	if me.EmailVerifiedAt == nil {
		t.Error("email_verified_at is still null after a reset")
	}
}

// Resend-verification has nothing to send to somebody who already confirmed.
// Sending anyway would be a way to mail a stranger on demand, forever.
func TestResendVerificationIsSilentForAVerifiedAddress(t *testing.T) {
	h, sender, pool := authTestServer(t)
	verified := authVerifiedUser(t, h, sender)

	// A pending account too. Its mail is the signal that the send path really
	// ran, so the assertion below is not just a race against a sleep.
	pending := authTestEmail(t)
	if rec := authDo(t, h, http.MethodPost, "/api/auth/signup",
		authSignupBody{pending, authTestPassword, "Test User"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("signup: status %d, body %s", rec.Code, rec.Body.String())
	}
	sender.waitForTo(t, pending, verifySubject)

	for _, email := range []string{verified, pending} {
		if rec := authDo(t, h, http.MethodPost, "/api/auth/resend-verification",
			map[string]string{"email": email}, nil); rec.Code != http.StatusOK {
			t.Fatalf("resend for %s: status %d, body %s", email, rec.Code, rec.Body.String())
		}
	}

	// Two mails to the pending address (signup's and the resend) is the proof
	// the resend path works at all.
	sender.waitForCountTo(t, pending, verifySubject, 2)
	authSettle()

	if n := len(sender.matching(verified, verifySubject)); n != 1 {
		t.Errorf("%d verification mails to an address that is already verified, want only the one from signup", n)
	}
	spent, err := auth.Count(t.Context(), pool, auth.ScopeEmailSendVerify, verified, auth.EmailSendWindow)
	if err != nil {
		t.Fatalf("reading the verify budget: %v", err)
	}
	if spent != 0 {
		t.Errorf("a verified address's resend budget counted %d, want 0", spent)
	}
}

// The response to a reset request must not wait on the send.
//
// The 200 for a known address and the 200 for an unknown one are identical by
// construction, so latency is the only channel left -- and everything that only
// the "this address has an account" branch does (a lookup, two budget checks, a
// token INSERT, a blocking SMTP round trip) is exactly the work that would show
// up in it. Nothing else in this package can see the difference between a send
// that is dispatched off the request goroutine and one that is not: the
// recording sender returns instantly either way. Holding it is what makes the
// difference observable.
func TestPasswordResetAnswersWhileTheSendIsStillInFlight(t *testing.T) {
	h, sender, _ := authTestServer(t)
	// Built before the gate goes up: this one needs its mail to arrive.
	email := authVerifiedUser(t, h, sender)

	sender.hold()
	t.Cleanup(sender.release)

	body, err := json.Marshal(map[string]string{"email": email})
	if err != nil {
		t.Fatalf("marshalling the request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/password-reset", strings.NewReader(string(body)))
	req.Header.Set(ClientHeader, "web")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	// Served on a goroutine of its own, because the whole question is whether
	// this call returns while a send is parked.
	answered := make(chan int, 1)
	go func() {
		h.ServeHTTP(rec, req)
		answered <- rec.Code
	}()

	select {
	case code := <-answered:
		if code != http.StatusOK {
			t.Fatalf("status %d, want 200 (body %s)", code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("POST /password-reset had not answered two seconds into a held send: the send is on the response path, and its latency is an account-existence oracle")
	}

	// And the message really was on its way, so the 200 above is not the
	// endpoint quietly doing nothing.
	sender.release()
	sender.waitForTo(t, email, resetSubject)
}

// A link is only good for the purpose it was minted for.
//
// Both endpoints redeem out of one table, so the purpose predicate is the only
// thing keeping them apart -- and they are not equals. A verification link goes
// to an address that has proven nothing yet, and resend-verification will mail
// one to anybody who names that address; if it could also be spent at
// /password-reset/confirm, "resend my verification email" would be "take over
// this account".
func TestALinkCannotBeSpentForAnotherPurpose(t *testing.T) {
	h, sender, _ := authTestServer(t)

	t.Run("a verification link cannot set a password", func(t *testing.T) {
		email := authTestEmail(t)
		if rec := authDo(t, h, http.MethodPost, "/api/auth/signup",
			authSignupBody{email, authTestPassword, "Test User"}, nil); rec.Code != http.StatusOK {
			t.Fatalf("signup: status %d, body %s", rec.Code, rec.Body.String())
		}
		verifyToken := authTokenFromMail(t, sender.waitForTo(t, email, verifySubject).Body)

		const newPassword = "a completely different passphrase"
		rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset/confirm",
			authResetConfirmBody{verifyToken, newPassword}, nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("a verification link was accepted as a reset link: status %d, want 422 (body %s)", rec.Code, rec.Body.String())
		}
		if got := decodeErr(t, rec.Body.String()).Code; got != CodeInvalid {
			t.Errorf("code %q, want %q", got, CodeInvalid)
		}

		// The refusal spent nothing: the link still does its own job.
		if rec := authDo(t, h, http.MethodPost, "/api/auth/verify-email",
			map[string]string{"token": verifyToken}, nil); rec.Code != http.StatusOK {
			t.Fatalf("the refused reset burnt the verification link: status %d (body %s)", rec.Code, rec.Body.String())
		}
		// And the password is still the one signup set.
		if rec := authDo(t, h, http.MethodPost, "/api/auth/login",
			authLoginBody{email, authTestPassword}, nil); rec.Code != http.StatusOK {
			t.Errorf("the original password stopped working: status %d (body %s)", rec.Code, rec.Body.String())
		}
		if rec := authDo(t, h, http.MethodPost, "/api/auth/login",
			authLoginBody{email, newPassword}, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("the password the refused request named signs in: status %d", rec.Code)
		}
	})

	t.Run("a reset link cannot verify the address", func(t *testing.T) {
		email := authTestEmail(t)
		if rec := authDo(t, h, http.MethodPost, "/api/auth/signup",
			authSignupBody{email, authTestPassword, "Test User"}, nil); rec.Code != http.StatusOK {
			t.Fatalf("signup: status %d, body %s", rec.Code, rec.Body.String())
		}
		sender.waitForTo(t, email, verifySubject)

		if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset",
			map[string]string{"email": email}, nil); rec.Code != http.StatusOK {
			t.Fatalf("password-reset: status %d, body %s", rec.Code, rec.Body.String())
		}
		resetToken := authResetTokenFromMail(t, sender.waitForTo(t, email, resetSubject).Body)

		if rec := authDo(t, h, http.MethodPost, "/api/auth/verify-email",
			map[string]string{"token": resetToken}, nil); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("a reset link was accepted as a verification link: status %d, want 422 (body %s)", rec.Code, rec.Body.String())
		}

		// The address is still unconfirmed.
		login := authDo(t, h, http.MethodPost, "/api/auth/login", authLoginBody{email, authTestPassword}, nil)
		if login.Code != http.StatusUnauthorized {
			t.Fatalf("login answered %d, want 401 -- the reset link confirmed the address (body %s)", login.Code, login.Body.String())
		}
		if got := decodeErr(t, login.Body.String()).Code; got != CodeEmailUnverified {
			t.Errorf("login code %q, want %q", got, CodeEmailUnverified)
		}

		// And the reset link was not spent by the refusal.
		if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset/confirm",
			authResetConfirmBody{resetToken, "a completely different passphrase"}, nil); rec.Code != http.StatusNoContent {
			t.Fatalf("the refused verification burnt the reset link: status %d (body %s)", rec.Code, rec.Body.String())
		}
	})
}

// A password change is one transaction with three effects, and this is the test
// that says all three land.
//
// Two of them used to be a best-effort DELETE issued after the UPDATE with its
// error only logged: a revoke that failed answered 204 and left every session
// the person was worried about alive. The third is the one nothing did at all --
// a reset link already sitting in a mailbox is a standing offer to undo the
// change, whether the user asked for it before they remembered the old password
// or somebody else asked for it while they were reading their mail.
func TestChangePasswordIsOneTransactionWithThreeEffects(t *testing.T) {
	h, sender, _ := authTestServer(t)
	email, mine := authSignedIn(t, h, sender)
	other := authLoginAs(t, h, email, authTestPassword)

	if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset",
		map[string]string{"email": email}, nil); rec.Code != http.StatusOK {
		t.Fatalf("password-reset: status %d, body %s", rec.Code, rec.Body.String())
	}
	resetToken := authResetTokenFromMail(t, sender.waitForTo(t, email, resetSubject).Body)

	const newPassword = "a completely different passphrase"
	if rec := authDo(t, h, http.MethodPost, "/api/auth/password",
		authChangePasswordBody{authTestPassword, newPassword}, mine); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}

	if got := authMeStatus(t, h, mine); got != http.StatusOK {
		t.Errorf("the caller's own session answered %d after changing its password, want 200", got)
	}
	if got := authMeStatus(t, h, other); got != http.StatusUnauthorized {
		t.Errorf("the other session answered %d after the password change, want 401", got)
	}

	// The link is dead, so nobody holding it can put the old password back.
	rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset/confirm",
		authResetConfirmBody{resetToken, authTestPassword}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a reset link minted before the password change still redeems: status %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := authDo(t, h, http.MethodPost, "/api/auth/login",
		authLoginBody{email, authTestPassword}, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old password signs in again: status %d -- the stale link undid the change", rec.Code)
	}
	if rec := authDo(t, h, http.MethodPost, "/api/auth/login",
		authLoginBody{email, newPassword}, nil); rec.Code != http.StatusOK {
		t.Errorf("the new password does not sign in: status %d (body %s)", rec.Code, rec.Body.String())
	}
}

// Resetting the password clears the login lockout for the address.
//
// Without it the two controls contradict each other, and on the one path a real
// user takes: guessing a password you have forgotten ten times is exactly how
// the login budget gets spent, and the reset link is what the refusal tells you
// to use next. The first sign-in with the new password would then be refused for
// the rest of the fifteen minutes -- by a counter guarding a password that no
// longer exists.
func TestResettingThePasswordClearsTheLoginLockout(t *testing.T) {
	h, sender, pool := authTestServer(t)
	email := authVerifiedUser(t, h, sender)

	for i := range auth.LoginFailLimit {
		if rec := authDo(t, h, http.MethodPost, "/api/auth/login",
			authLoginBody{email, "not the password"}, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401 (body %s)", i+1, rec.Code, rec.Body.String())
		}
	}
	spent, err := auth.Count(t.Context(), pool, auth.ScopeLogin, email, auth.LoginFailWindow)
	if err != nil {
		t.Fatalf("counting the login budget: %v", err)
	}
	if spent < auth.LoginFailLimit {
		t.Fatalf("the login budget counted %d after %d failures; the lockout is not in force and the test would prove nothing", spent, auth.LoginFailLimit)
	}

	if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset",
		map[string]string{"email": email}, nil); rec.Code != http.StatusOK {
		t.Fatalf("password-reset: status %d, body %s", rec.Code, rec.Body.String())
	}
	token := authResetTokenFromMail(t, sender.waitForTo(t, email, resetSubject).Body)

	const newPassword = "a completely different passphrase"
	if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset/confirm",
		authResetConfirmBody{token, newPassword}, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("confirm: status %d, body %s", rec.Code, rec.Body.String())
	}

	if rec := authDo(t, h, http.MethodPost, "/api/auth/login",
		authLoginBody{email, newPassword}, nil); rec.Code != http.StatusOK {
		t.Fatalf("the first sign-in with the new password answered %d, want 200: the lockout from the guesses that led to the reset is still in force (body %s)", rec.Code, rec.Body.String())
	}

	left, err := auth.Count(t.Context(), pool, auth.ScopeLogin, email, auth.LoginFailWindow)
	if err != nil {
		t.Fatalf("counting the login budget: %v", err)
	}
	if left != 0 {
		t.Errorf("the login budget still counts %d after the reset, want 0", left)
	}
}

// A verification link gets the two days EmailTokenTTL names, not the hour a
// reset link gets.
//
// The pair is the design -- a verification link only proves an address, a reset
// link is the account -- so both halves need pinning. The reset test kills a
// 90-minute-old link; this one keeps one alive, so a tokenTTL that stopped
// telling the purposes apart could not pass both.
func TestVerificationTokenGetsTheLongerLifetime(t *testing.T) {
	h, sender, pool := authTestServer(t)
	email := authTestEmail(t)
	if rec := authDo(t, h, http.MethodPost, "/api/auth/signup",
		authSignupBody{email, authTestPassword, "Test User"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("signup: status %d, body %s", rec.Code, rec.Body.String())
	}
	token := authTokenFromMail(t, sender.waitForTo(t, email, verifySubject).Body)

	// The lifetime the row was written with, not one the test chose.
	var seconds float64
	if err := pool.QueryRow(t.Context(),
		`SELECT EXTRACT(EPOCH FROM (expires_at - created_at))::float8 FROM email_tokens WHERE token_hash = $1`,
		auth.HashToken(token),
	).Scan(&seconds); err != nil {
		t.Fatalf("reading the token's lifetime: %v", err)
	}
	if got := time.Duration(seconds * float64(time.Second)); (got - auth.EmailTokenTTL).Abs() > time.Second {
		t.Errorf("the verification token lives %v, want %v", got, auth.EmailTokenTTL)
	}

	// And it still redeems well past the hour a reset link would have had. Both
	// timestamps move together, so what this reads back is the interval the
	// server chose.
	const backdate = `
		UPDATE email_tokens
		   SET created_at = created_at - interval '90 minutes',
		       expires_at = expires_at - interval '90 minutes'
		 WHERE token_hash = $1`
	if _, err := pool.Exec(t.Context(), backdate, auth.HashToken(token)); err != nil {
		t.Fatalf("backdating the token: %v", err)
	}
	if rec := authDo(t, h, http.MethodPost, "/api/auth/verify-email",
		map[string]string{"token": token}, nil); rec.Code != http.StatusOK {
		t.Fatalf("a 90-minute-old verification link answered %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// The session list caps the User-Agent in RUNES, not bytes.
//
// It is attacker-controlled text of unbounded length that the account page
// echoes back, so it has to be cut somewhere -- but a cut counted in bytes lands
// in the middle of a multi-byte character, and what comes back is then neither
// the browser's name nor valid UTF-8.
func TestSessionListCapsTheUserAgentInRunes(t *testing.T) {
	h, sender, _ := authTestServer(t)
	email := authVerifiedUser(t, h, sender)

	// 300 three-byte runes, so the string is 900 bytes: a cut at byte 200 lands
	// two bytes into a character (200 is not a multiple of 3) rather than on a
	// boundary by luck, and what a byte-counted cap would produce is both the
	// wrong length and invalid UTF-8.
	ua := strings.Repeat("日", 300)
	if n := len([]rune(ua)); n != 300 {
		t.Fatalf("the test's User-Agent is %d runes, want 300", n)
	}
	if utf8.ValidString(ua[:maxUserAgent]) {
		t.Fatalf("the test's User-Agent is rune-aligned at byte %d; it must not be, or a byte-counted cap would look correct here", maxUserAgent)
	}

	body, err := json.Marshal(authLoginBody{email, authTestPassword})
	if err != nil {
		t.Fatalf("marshalling the login body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(string(body)))
	req.Header.Set(ClientHeader, "web")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body.String())
	}

	list := authListSessionsOf(t, h, authCookie(t, rec))
	if len(list.Items) != 1 {
		t.Fatalf("%d sessions listed, want 1", len(list.Items))
	}
	got := list.Items[0].UserAgent
	if got == nil {
		t.Fatal("the session row carries no user_agent")
	}
	if n := len([]rune(*got)); n != maxUserAgent {
		t.Errorf("user_agent is %d runes, want %d -- a byte-counted cut of a multi-byte string lands here", n, maxUserAgent)
	}
	if want := string([]rune(ua)[:maxUserAgent]); *got != want {
		t.Errorf("user_agent is not the first %d runes of what was sent; it came back as %q", maxUserAgent, *got)
	}
}

func TestResetAndResendRejectMalformedAddresses(t *testing.T) {
	h, sender, _ := authTestServer(t)

	for _, path := range []string{"/api/auth/password-reset", "/api/auth/resend-verification"} {
		for _, bad := range []string{"", "nope", "Someone <a@b.test>", "a@b.test\r\nBcc: evil@x.test"} {
			rec := authDo(t, h, http.MethodPost, path, map[string]string{"email": bad}, nil)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s with %q: status %d, want 422 (body %s)", path, bad, rec.Code, rec.Body.String())
			}
		}
	}
	authSettle()
	if n := len(sender.all()); n != 0 {
		t.Errorf("%d mails sent for rejected requests, want 0", n)
	}
}
