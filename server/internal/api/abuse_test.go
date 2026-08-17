package api

// The abuse controls where they actually run: in front of the handlers.
//
// Signup was, until these landed, an unmetered Argon2 amplifier -- 19 MiB and
// tens of milliseconds per call, reachable by anyone, with the only budget in
// the system charged after the hash and keyed by the email the caller chose.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		req.Header.Set("X-Forwarded-For", forwarded+", 151.101.1.1")
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
