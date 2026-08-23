package api

// The abuse controls where they actually run: in front of the handlers.
//
// Signup was, until these landed, an unmetered Argon2 amplifier -- 19 MiB and
// tens of milliseconds per call, reachable by anyone, with the only budget in
// the system charged after the hash and keyed by the email the caller chose.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
)

// abuseServer returns the Server itself as well as its handler, because both
// limiters live on it and a test that wants a saturated one has to say so.
func abuseServer(t *testing.T) (*Server, http.Handler, *authRecordingSender, *pgxpool.Pool) {
	t.Helper()
	pool := authTestPool(t)
	sender := &authRecordingSender{}
	s := New(&config.Config{BaseURL: authTestBaseURL}, pool, nil, sender, nil, nil)
	return s, s.Routes(), sender, pool
}

// abuseDo issues a request from a chosen address. The forwarded header is the
// deployed shape; RemoteAddr is the direct one.
func abuseDo(t *testing.T, h http.Handler, method, path string, body any, forwarded string) *httptest.ResponseRecorder {
	t.Helper()
	raw := ""
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the body: %v", err)
		}
		raw = string(encoded)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(raw))
	req.Header.Set(ClientHeader, "web")
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.9:41234" // the edge, as it looks from inside
	if forwarded != "" {
		req.Header.Set("X-Forwarded-For", forwarded+", 203.0.113.100")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// With every Argon2 slot taken, both password endpoints refuse immediately
// rather than piling more 19 MiB allocations on top of the ones in flight.
func TestAuthRefusesWhenEveryArgon2SlotIsTaken(t *testing.T) {
	s, h, _, _ := abuseServer(t)

	s.Argon2 = auth.NewLimiter(1)
	if !s.Argon2.Acquire() {
		t.Fatal("could not take the single Argon2 slot")
	}
	t.Cleanup(s.Argon2.Release)

	for _, c := range []struct {
		what string
		path string
		body any
	}{
		{"signup", "/api/auth/signup", authSignupBody{authTestEmail(t), authTestPassword, "Test User"}},
		{"login", "/api/auth/login", authLoginBody{authTestEmail(t), authTestPassword}},
	} {
		t.Run(c.what, func(t *testing.T) {
			rec := abuseDo(t, h, http.MethodPost, c.path, c.body, "203.0.113.7")
			nodeWant(t, rec, http.StatusTooManyRequests, CodeRateLimited)
		})
	}

	// And with the slot back, the same request goes through -- the refusal is a
	// ceiling, not a broken endpoint.
	s.Argon2.Release()
	rec := abuseDo(t, h, http.MethodPost, "/api/auth/signup",
		authSignupBody{authTestEmail(t), authTestPassword, "Test User"}, "203.0.113.7")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d once a slot was free, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// The same ceiling on the two password endpoints a stranger cannot reach --
// they need a session or a link -- and which therefore were not covered above.
//
// /password-reset/confirm is the one worth the test. It hashes BEFORE it
// consumes the link, precisely so that a refusal here cannot burn it, and the
// assertion that the very same link still redeems afterwards is what says that
// order is still the order: a 429 that spent the user's one link would be
// telling them to retry with a credential that no longer exists.
func TestPasswordEndpointsRefuseWhenEveryArgon2SlotIsTaken(t *testing.T) {
	s, h, sender, _ := abuseServer(t)
	email := authVerifiedUser(t, h, sender)
	cookie := authLoginAs(t, h, email, authTestPassword)

	if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset",
		map[string]string{"email": email}, nil); rec.Code != http.StatusOK {
		t.Fatalf("password-reset: status %d, body %s", rec.Code, rec.Body.String())
	}
	token := authResetTokenFromMail(t, sender.waitForTo(t, email, resetSubject).Body)

	// One slot, and it is taken. Everything until the release below has to be
	// refused without reaching Argon2.
	s.Argon2 = auth.NewLimiter(1)
	if !s.Argon2.Acquire() {
		t.Fatal("could not take the single Argon2 slot")
	}

	const changed = "a completely different passphrase"
	nodeWant(t, authDo(t, h, http.MethodPost, "/api/auth/password",
		authChangePasswordBody{authTestPassword, changed}, cookie),
		http.StatusTooManyRequests, CodeRateLimited)

	const afterReset = "yet another passphrase entirely"
	nodeWant(t, authDo(t, h, http.MethodPost, "/api/auth/password-reset/confirm",
		authResetConfirmBody{token, afterReset}, nil),
		http.StatusTooManyRequests, CodeRateLimited)

	s.Argon2.Release()

	// The link survived its refusal.
	if rec := authDo(t, h, http.MethodPost, "/api/auth/password-reset/confirm",
		authResetConfirmBody{token, afterReset}, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("the reset link did not survive the 429: status %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	// And the refused change changed nothing.
	if rec := authDo(t, h, http.MethodPost, "/api/auth/login",
		authLoginBody{email, changed}, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the password the refused change named signs in: status %d", rec.Code)
	}
	if rec := authDo(t, h, http.MethodPost, "/api/auth/login",
		authLoginBody{email, afterReset}, nil); rec.Code != http.StatusOK {
		t.Errorf("the password the reset set does not sign in: status %d (body %s)", rec.Code, rec.Body.String())
	}
}

// The per-IP bucket, keyed by the address the edge reports. Its whole point is
// that it is per-address: a shared bucket would mean one attacker locking every
// user out of signing in.
func TestAuthBurstIsRefusedPerAddress(t *testing.T) {
	_, h, _, _ := abuseServer(t)

	// The burst is spent on a request that costs nothing downstream, so what is
	// being measured is the bucket and not any handler.
	spend := func(from string) int {
		return abuseDo(t, h, http.MethodPost, "/api/auth/verify-email",
			map[string]string{"token": "nope"}, from).Code
	}

	for i := 1; i <= int(burstFor(DefaultAuthRatePerMin)); i++ {
		if got := spend("203.0.113.7"); got == http.StatusTooManyRequests {
			t.Fatalf("request %d of the burst was refused (burst is %d)", i, int(burstFor(DefaultAuthRatePerMin)))
		}
	}
	if got := spend("203.0.113.7"); got != http.StatusTooManyRequests {
		t.Fatalf("status %d past the burst, want 429", got)
	}
	if got := spend("198.51.100.22"); got == http.StatusTooManyRequests {
		t.Error("a second address was refused because the first one spent its burst")
	}
}

// The signed-in half of /api/auth is not in the per-IP bucket, and GET /me is
// the reason it matters.
//
// The SPA refetches it, and an office, a household or a phone network is one
// address to the edge -- so a bucket sized for "a person signing in" locks a
// building out of its own account pages. Behind RequireAuth there is a far
// better identity than the address anyway, and every durable budget uses it.
//
// The allowance is 10 a minute with a burst of 20, so the twenty-first request
// is the one that proves it: under the old routing it is a 429.
func TestMeIsNotInThePerIPBucket(t *testing.T) {
	// This server's config is explicit rather than the environment's:
	// .env.test raises DRIVE_AUTH_RATE_PER_MIN to 100000 so the whole suite can
	// share one address, which would make this assertion pass unconditionally.
	h, sender, _ := authTestServer(t)
	_, cookie := authSignedIn(t, h, sender)

	// Signup, verify and login have already spent three of the twenty tokens on
	// this address, so a /me that shared the bucket would run out well before
	// the count below finishes.
	const calls = 21
	for i := 1; i <= calls; i++ {
		rec := authDo(t, h, http.MethodGet, "/api/auth/me", nil, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /me #%d of %d answered %d, want 200 (body %s)", i, calls, rec.Code, rec.Body.String())
		}
	}

	// The unauthenticated half is still bucketed, from the same address.
	for range int(burstFor(DefaultAuthRatePerMin)) {
		authDo(t, h, http.MethodPost, "/api/auth/verify-email", map[string]string{"token": "nope"}, nil)
	}
	spent := authDo(t, h, http.MethodPost, "/api/auth/verify-email", map[string]string{"token": "nope"}, nil)
	if spent.Code != http.StatusTooManyRequests {
		t.Errorf("verify-email answered %d past the burst, want 429 -- the bucket is gone entirely", spent.Code)
	}
	// And /me still answers, because it never shared that bucket.
	if got := authDo(t, h, http.MethodGet, "/api/auth/me", nil, cookie).Code; got != http.StatusOK {
		t.Errorf("GET /me answered %d once the unauthenticated bucket was empty, want 200", got)
	}
}

// The per-IP mail bucket. These two endpoints are the only ones where the
// caller names somebody else's inbox, and both answer 200 for any address, so
// without a ceiling one address could spend anybody's per-recipient budget as
// fast as it could issue requests.
func TestMailRequestsAreBucketedPerAddress(t *testing.T) {
	_, h, _, _ := abuseServer(t)

	ask := func(from string) int {
		return abuseDo(t, h, http.MethodPost, "/api/auth/password-reset",
			map[string]string{"email": "nobody-" + uuid.NewString() + "@drive.test"}, from).Code
	}

	for i := 1; i <= DefaultMailRatePerHour; i++ {
		if got := ask("203.0.113.7"); got != http.StatusOK {
			t.Fatalf("reset request %d of %d answered %d, want 200", i, DefaultMailRatePerHour, got)
		}
	}
	if got := ask("203.0.113.7"); got != http.StatusTooManyRequests {
		t.Fatalf("reset request %d answered %d, want 429", DefaultMailRatePerHour+1, got)
	}
	if got := ask("198.51.100.22"); got != http.StatusOK {
		t.Errorf("a second address answered %d because the first spent its allowance", got)
	}

	// Resend-verification shares the same bucket: they are one budget for
	// "mail somebody on my say-so", not two ways to spend it.
	if got := abuseDo(t, h, http.MethodPost, "/api/auth/resend-verification",
		map[string]string{"email": "nobody@drive.test"}, "203.0.113.7").Code; got != http.StatusTooManyRequests {
		t.Errorf("resend-verification answered %d from an address that had spent its mail allowance, want 429", got)
	}
}

// ------------------------------------------------------------ log volume --

// recordingLog is a slog.Handler that keeps every record's level and message,
// so a test can count lines instead of eyeballing them. The mutex is not
// decoration: the mail endpoints dispatch their send off the request goroutine
// with the same request logger.
type recordingLog struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingLog) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingLog) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingLog) WithGroup(string) slog.Handler      { return h }

// count returns how many records carry exactly this level and message.
func (h *recordingLog) count(level slog.Level, msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level && r.Message == msg {
			n++
		}
	}
	return n
}

// A request whose forwarded header cannot be trusted says so once, not twice.
//
// ClientIP logs when it refuses to believe the header, and both bucket call
// sites used to resolve the address a second time inside the refusal branch --
// so precisely the requests that arrive during a flood wrote the line twice.
// The cost is log volume on the one platform where log volume is billed, and it
// hides the count: two lines per request makes an attack look twice its size.
func TestAnUntrustedForwardedHeaderIsLoggedOncePerRequest(t *testing.T) {
	rec := &recordingLog{}
	pool := authTestPool(t)
	s := New(&config.Config{BaseURL: authTestBaseURL}, pool, slog.New(rec),
		&authRecordingSender{}, nil, nil)
	h := s.Routes()

	// A private leftmost entry is not something the edge writes, so every
	// request below distrusts the header -- and each must account for exactly
	// one line.
	const forged = "10.1.2.3"
	requests := 0

	// The mail bucket first, while the auth bucket still has tokens: past its
	// allowance, so its own refusal branch runs.
	for i := 0; i <= DefaultMailRatePerHour; i++ {
		abuseDo(t, h, http.MethodPost, "/api/auth/password-reset",
			map[string]string{"email": "nobody-" + uuid.NewString() + "@drive.test"}, forged)
		requests++
	}
	// Then the auth bucket, spent past its burst.
	for i := 0; i < int(burstFor(DefaultAuthRatePerMin)); i++ {
		abuseDo(t, h, http.MethodPost, "/api/auth/verify-email",
			map[string]string{"token": "nope"}, forged)
		requests++
	}

	// Both refusal branches really ran; otherwise this test would pass on a
	// build where neither was reached.
	if n := rec.count(slog.LevelWarn, "mail request refused by the per-IP bucket"); n == 0 {
		t.Fatal("the mail bucket never refused anything; its call site was not exercised")
	}
	if n := rec.count(slog.LevelWarn, "auth request refused by the per-IP bucket"); n == 0 {
		t.Fatal("the auth bucket never refused anything; its call site was not exercised")
	}

	const line = "untrustworthy X-Forwarded-For; keying on the peer address instead"
	if got := rec.count(slog.LevelError, line); got != requests {
		t.Errorf("%d %q lines for %d requests, want one each", got, line, requests)
	}
}

// auth_sessions.ip is the audit trail. Behind a proxy it recorded the proxy
// until the leftmost-forwarded rule landed, which made it worthless and would
// have keyed the limiter on the edge too.
func TestLoginRecordsTheForwardedClientAddress(t *testing.T) {
	_, h, sender, pool := abuseServer(t)
	email := authVerifiedUser(t, h, sender)

	rec := abuseDo(t, h, http.MethodPost, "/api/auth/login",
		authLoginBody{email, authTestPassword}, "198.51.100.42")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body.String())
	}

	var ip string
	if err := pool.QueryRow(context.Background(),
		`SELECT coalesce(host(s.ip), '') FROM auth_sessions s JOIN users u ON u.id = s.user_id
		  WHERE u.email = $1 ORDER BY s.created_at DESC LIMIT 1`, email).Scan(&ip); err != nil {
		t.Fatalf("reading the session row: %v", err)
	}
	if ip != "198.51.100.42" {
		t.Errorf("auth_sessions.ip = %q, want the client address the edge reported", ip)
	}
}

// The service-wide daily mail budget. The per-recipient budget cannot do this
// job: a thousand addresses each well inside their personal allowance still
// empty the sending account's quota, and a spent quota means no real user can
// verify an address until tomorrow.
func TestVerificationMailStopsAtTheServiceWideDailyCap(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()

	// This budget is one row for the whole service, so the test owns the scope
	// rather than a key of its own and clears it before counting.
	if _, err := pool.Exec(ctx, `DELETE FROM throttle WHERE scope = $1`, auth.ScopeEmailSendGlobal); err != nil {
		t.Fatalf("clearing the global mail budget: %v", err)
	}

	sender := &authRecordingSender{}
	s := New(&config.Config{BaseURL: authTestBaseURL, EmailDailyCap: 2}, pool, nil, sender, nil, nil)
	h := s.Routes()

	const attempts = 3
	for i := 0; i < attempts; i++ {
		rec := abuseDo(t, h, http.MethodPost, "/api/auth/signup",
			authSignupBody{authTestEmail(t), authTestPassword, "Test User"}, "203.0.113.7")
		// Every signup still succeeds: a suppressed message must not tell the
		// caller anything, and the account exists either way.
		if rec.Code != http.StatusOK {
			t.Fatalf("signup %d: status %d, body %s", i+1, rec.Code, rec.Body.String())
		}
	}

	// The sends are dispatched off the request goroutine, so wait on the budget
	// rather than on a clock: charge-first means all three attempts are counted
	// whether or not they sent anything.
	deadline := time.Now().Add(3 * time.Second)
	for {
		n, err := auth.Count(ctx, pool, auth.ScopeEmailSendGlobal, auth.GlobalKey, auth.EmailSendGlobalWindow)
		if err != nil {
			t.Fatalf("reading the global mail budget: %v", err)
		}
		if n == attempts {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the global budget counted %d of %d attempts", n, attempts)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := len(sender.all()); got != 2 {
		t.Errorf("%d messages went out under a cap of 2", got)
	}
}

// DRIVE_SIGNUP_MODE. Gate A deploys "closed", so this is the switch that lets a
// live URL exist before anyone is invited to use it.
func TestSignupModeGatesAccountCreation(t *testing.T) {
	pool := authTestPool(t)

	for _, c := range []struct {
		mode   string
		status int
	}{
		{config.SignupOpen, http.StatusOK},
		{"", http.StatusOK}, // unset is open
		{config.SignupClosed, http.StatusForbidden},
		// No invite system exists, so invite-only is a deployment nobody can
		// join. Answering anything friendlier than closed would be a claim the
		// server cannot back up.
		{config.SignupInvite, http.StatusForbidden},
	} {
		t.Run("mode="+c.mode, func(t *testing.T) {
			sender := &authRecordingSender{}
			h := New(&config.Config{BaseURL: authTestBaseURL, SignupMode: c.mode}, pool, nil, sender, nil, nil).Routes()

			email := authTestEmail(t)
			rec := abuseDo(t, h, http.MethodPost, "/api/auth/signup",
				authSignupBody{email, authTestPassword, "Test User"}, "203.0.113.7")
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, c.status, rec.Body.String())
			}
			if c.status != http.StatusOK {
				var n int
				if err := pool.QueryRow(context.Background(),
					`SELECT count(*) FROM users WHERE email = $1`, email).Scan(&n); err != nil {
					t.Fatalf("counting the account: %v", err)
				}
				if n != 0 {
					t.Error("a refused signup created an account anyway")
				}
			}
		})
	}
}
