package api

// The public share surface's chain properties: the headers on everything
// under /api/s (matched or not), the two buckets, and the request logger's
// redaction. The five routes' own behaviour lives in share_routes_test.go;
// what belongs here is what must hold whatever a handler does.

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/config"
)

// shareHeaderWant is the three headers every /api/s answer carries.
var shareHeaderWant = map[string]string{
	"Cache-Control":   "private, no-store",
	"Referrer-Policy": "no-referrer",
	"X-Robots-Tag":    "noindex, nofollow",
}

func assertShareHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	for name, want := range shareHeaderWant {
		if got := h.Get(name); got != want {
			t.Errorf("%s: %s = %q, want %q", what, name, got, want)
		}
	}
}

// debugLogServer builds a Server whose logger writes every level into the
// returned buffer, so a test can assert what a whole round trip put in the log.
func debugLogServer(t *testing.T, cfg *config.Config, pool *pgxpool.Pool) (*Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(cfg, pool, logger, nil, nil, nil), &logs
}

// Every answer under /api/s carries the three headers, including the group's
// own 404 for a path no route claims -- and unmatched subpaths still exist
// beside the five real routes. The rest of /api is untouched: its bare 404
// and its real routes carry neither of the two that would be wrong on a page
// the app itself navigates to.
func TestShareGroupAnswersEverythingWithTheShareHeaders(t *testing.T) {
	h := New(&config.Config{}, authTestPool(t), nil, nil, nil, nil).Routes()

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/s/anything/nope"},
		{http.MethodGet, "/api/s/anything"},
		{http.MethodGet, "/api/s"},
		{http.MethodPost, "/api/s/anything/session"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set(ClientHeader, "web")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		what := c.method + " " + c.path
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (body %s)", what, rec.Code, rec.Body.String())
		}
		if got := decodeErr(t, rec.Body.String()).Code; got != CodeNotFound {
			t.Errorf("%s: code = %q, want %q", what, got, CodeNotFound)
		}
		assertShareHeaders(t, what, rec.Header())
	}

	for _, path := range []string{"/api/nope", "/api/nodes/nope", "/api/shares"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		for _, name := range []string{"X-Robots-Tag", "Referrer-Policy"} {
			if got := rec.Header().Get(name); got != "" {
				t.Errorf("GET %s: %s = %q, want none outside /api/s", path, name, got)
			}
		}
	}
}

// The share bucket is its own: spent by the four page routes and nothing
// else, refused with the envelope and the headers -- a redirect back to the
// page on /download, the one navigation -- and every refusal line names the
// route pattern, never the token. /password sits in the AUTH bucket instead
// -- it reaches Argon2 -- through the group's own wrapper, because
// RateLimitAuth's line logs the path and this path carries the credential.
func TestShareBucketIsItsOwnAndNamesNoToken(t *testing.T) {
	s, logs := debugLogServer(t, &config.Config{}, authTestPool(t))

	// A small bucket on a frozen clock, so the refusal is deterministic and no
	// refill can race the loop.
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	s.ShareRate = newIPLimiter(60, 4)
	s.ShareRate.now = func() time.Time { return now }
	h := s.Routes()

	const token = "SECRETTOKEN0123456789abcdef"
	meta := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/s/"+token+"/meta", nil)
		req.RemoteAddr = "203.0.113.9:41234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	password := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/s/"+token+"/password",
			strings.NewReader(`{"password":"not the password"}`))
		req.Header.Set(ClientHeader, "web")
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.9:41234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for i := 1; i <= 4; i++ {
		if rec := meta(); rec.Code != http.StatusNotFound {
			t.Fatalf("request %d of the burst: status %d, want 404 (body %s)", i, rec.Code, rec.Body.String())
		}
	}
	rec := meta()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fifth request: status %d, want 429 (body %s)", rec.Code, rec.Body.String())
	}
	if got := decodeErr(t, rec.Body.String()).Code; got != CodeRateLimited {
		t.Errorf("code = %q, want %q", got, CodeRateLimited)
	}
	assertShareHeaders(t, "the refusal", rec.Header())

	// /download with the bucket still empty: the same refusal, but a
	// redirect back to the page -- a person following a link cannot act on
	// the envelope.
	req := httptest.NewRequest(http.MethodGet, "/api/s/"+token+"/download", nil)
	req.RemoteAddr = "203.0.113.9:41234"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("download with the share bucket empty: status %d, want 302 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/s/"+token {
		t.Errorf("download refusal: Location = %q, want /s/%s", got, token)
	}
	assertShareHeaders(t, "the download refusal", rec.Header())

	// The auth bucket never saw the address: a share page load must not spend
	// the sign-in allowance of whoever shares the NAT.
	s.AuthRate.mu.Lock()
	_, touched := s.AuthRate.buckets["203.0.113.9"]
	s.AuthRate.mu.Unlock()
	if touched {
		t.Error("the share requests spent from the auth bucket")
	}

	// /password is not in the share bucket: with that bucket empty it still
	// reaches its handler (404 -- the token is nobody's), because its budget
	// is the auth one.
	if rec := password(); rec.Code != http.StatusNotFound {
		t.Errorf("password with the share bucket empty: status %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	// And with the auth bucket drained it is refused -- by AuthRate, through
	// the group's own line.
	for s.AuthRate.allow("203.0.113.9") { //nolint:revive // draining the bucket
	}
	rec = password()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("password with the auth bucket empty: status %d, want 429 (body %s)", rec.Code, rec.Body.String())
	}
	assertShareHeaders(t, "the password refusal", rec.Header())

	out := logs.String()
	if !strings.Contains(out, "share request refused by the per-IP bucket") {
		t.Fatalf("the share refusal was not logged: %s", out)
	}
	if !strings.Contains(out, "route=/api/s/{token}/meta") {
		t.Errorf("the share refusal does not name the route pattern: %s", out)
	}
	if !strings.Contains(out, "route=/api/s/{token}/download") {
		t.Errorf("the download refusal does not name the route pattern: %s", out)
	}
	if !strings.Contains(out, "share password request refused by the per-IP bucket") {
		t.Fatalf("the password refusal was not logged: %s", out)
	}
	if !strings.Contains(out, "route=/api/s/{token}/password") {
		t.Errorf("the password refusal does not name the route pattern: %s", out)
	}
	if strings.Contains(out, token) {
		t.Errorf("the token reached the log: %s", out)
	}
}

// The share bucket's default, and that it is its own instance: a Server built
// from an empty config gets DefaultShareRatePerMin with twice that as burst, a
// configured one gets its number, and neither touches the auth bucket.
func TestShareRateDefaultsAndIsNotTheAuthBucket(t *testing.T) {
	s := New(&config.Config{}, nil, nil, nil, nil, nil)
	if s.ShareRate == nil {
		t.Fatal("New built no share bucket")
	}
	if s.ShareRate == s.AuthRate || s.ShareRate == s.BrowserRate || s.ShareRate == s.MailRate {
		t.Fatal("the share bucket is one of the others")
	}
	if s.ShareRate.rate != float64(DefaultShareRatePerMin)/60 || s.ShareRate.burst != burstFor(DefaultShareRatePerMin) {
		t.Errorf("default share bucket = %v/s burst %v, want %v/s burst %v",
			s.ShareRate.rate, s.ShareRate.burst, float64(DefaultShareRatePerMin)/60, burstFor(DefaultShareRatePerMin))
	}

	s = New(&config.Config{ShareRatePerMin: 7}, nil, nil, nil, nil, nil)
	if s.ShareRate.rate != float64(7)/60 || s.ShareRate.burst != burstFor(7) {
		t.Errorf("configured share bucket = %v/s burst %v, want %v/s burst %v",
			s.ShareRate.rate, s.ShareRate.burst, float64(7)/60, burstFor(7))
	}
	if s.AuthRate.rate != float64(DefaultAuthRatePerMin)/60 {
		t.Errorf("the share setting moved the auth bucket to %v/s", s.AuthRate.rate)
	}
}

// ------------------------------------------------------------- redaction --

// redactSharePath on its own: the two share shapes lose their token segment and
// keep the rest, a second leading slash is the same URL, and every other path
// comes back untouched.
func TestRedactSharePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/api/s/tok/meta", "/api/s/{redacted}/meta"},
		{"/api/s/tok/download", "/api/s/{redacted}/download"},
		{"/api/s/tok", "/api/s/{redacted}"},
		{"/api/s/tok/", "/api/s/{redacted}/"},
		{"/s/tok", "/s/{redacted}"},
		{"/s/tok/extra", "/s/{redacted}/extra"},
		{"//s/tok", "/s/{redacted}"},
		{"//api/s/tok/meta", "/api/s/{redacted}/meta"},

		{"/api/s", "/api/s"},
		{"/s", "/s"},
		{"/search", "/search"},
		{"/shared", "/shared"},
		{"/api/shares/0123", "/api/shares/0123"},
		{"/api/nodes/x", "/api/nodes/x"},
		{"/", "/"},
		{"", ""},
	}
	for _, c := range cases {
		if got := redactSharePath(c.in); got != c.want {
			t.Errorf("redactSharePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The request logger writes one line per request at Debug, path included, and
// a share URL's path is the credential. Both shapes -- the API's and the
// page's -- are redacted through the real chain, and the rest of the path
// stays so the line still says which route it was.
func TestRequestLoggerRedactsShareTokens(t *testing.T) {
	s, logs := debugLogServer(t, &config.Config{}, authTestPool(t))
	h := s.Routes()

	const token = "SECRETTOKEN0123456789abcdef"
	for _, path := range []string{"/api/s/" + token + "/meta", "/s/" + token, "//s/" + token} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}

	out := logs.String()
	if strings.Contains(out, token) {
		t.Fatalf("the token reached the log:\n%s", out)
	}
	if n := strings.Count(out, "path=/api/s/{redacted}/meta"); n != 1 {
		t.Errorf("%d lines carry the redacted API path, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "path=/s/{redacted}"); n != 2 {
		t.Errorf("%d lines carry the redacted page path, want 2 (one per leading-slash shape):\n%s", n, out)
	}
}
