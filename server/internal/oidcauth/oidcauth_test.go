package oidcauth

// The verification seam on its own, against the fake provider.
//
// Everything here is also reachable through the HTTP handler, but not
// distinguishably: every refusal there is the same 302 to the same path. These
// cases say which check did the refusing, so a claims bug does not have to be
// diagnosed through a redirect.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/rahul-sharma-cs/drive/server/internal/oidcstub"
)

const (
	testClientID     = "drive-test-client-0001"
	testClientSecret = "drivetestsecret0001"
	testRedirectURL  = "http://localhost:8081/api/auth/google/callback"
)

// newStubProvider stands the fake provider up and points a Provider at it.
func newStubProvider(t *testing.T) (*oidcstub.Stub, *Provider) {
	t.Helper()
	stub, err := oidcstub.New(testClientID, testClientSecret)
	if err != nil {
		t.Fatalf("building the stub: %v", err)
	}
	ts := httptest.NewServer(stub.Handler())
	t.Cleanup(ts.Close)
	// The stub learns its own URL only now: go-oidc refuses a discovery
	// document whose issuer differs from the string it was given, and an
	// httptest server has no port until it is listening.
	stub.SetBaseURL(ts.URL)

	return stub, New(Config{
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		Issuer:       ts.URL,
		RedirectURL:  testRedirectURL,
	})
}

// authorize walks the browser hop: it fetches the authorization URL without
// following the redirect and reads the code and state back out of the Location.
func authorize(t *testing.T, p *Provider, state, nonce, verifier string) (code, gotState string) {
	t.Helper()
	raw, err := p.AuthCodeURL(context.Background(), state, nonce, verifier)
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(raw)
	if err != nil {
		t.Fatalf("GET the authorization URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("the stub answered %d, want 302", resp.StatusCode)
	}
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parsing the callback URL: %v", err)
	}
	return back.Query().Get("code"), back.Query().Get("state")
}

// The authorization URL carries every parameter the flow depends on, and one --
// prompt=select_account -- that only a comment would otherwise defend.
func TestAuthCodeURLCarriesTheWholeFlow(t *testing.T) {
	_, p := newStubProvider(t)
	verifier := oauth2.GenerateVerifier()

	raw, err := p.AuthCodeURL(context.Background(), "the-state", "the-nonce", verifier)
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing the authorization URL: %v", err)
	}
	q := u.Query()

	for _, c := range []struct{ key, want string }{
		{"client_id", testClientID},
		{"redirect_uri", testRedirectURL},
		{"response_type", "code"},
		{"scope", "openid email profile"},
		{"state", "the-state"},
		{"nonce", "the-nonce"},
		{"code_challenge_method", "S256"},
		// Without it the provider silently reuses whichever account the browser
		// is already signed into -- which on a shared machine signs somebody
		// into the wrong Drive, or creates one for them.
		{"prompt", "select_account"},
	} {
		if got := q.Get(c.key); got != c.want {
			t.Errorf("%s = %q, want %q", c.key, got, c.want)
		}
	}
	if q.Get("code_challenge") == "" {
		t.Error("no code_challenge in the authorization URL")
	}
	if q.Get("code_challenge") == verifier {
		t.Error("the code_challenge is the verifier itself -- S256 was not applied")
	}
	// No refresh token is asked for: Drive never calls the provider again.
	if got := q.Get("access_type"); got == "offline" {
		t.Error("access_type=offline asks for a refresh token nothing here would use")
	}
}

// The whole flow, end to end, with the claims coming out the other side.
func TestExchangeReturnsVerifiedClaims(t *testing.T) {
	stub, p := newStubProvider(t)
	stub.SetIdentity(oidcstub.Identity{
		Subject:       "subject-0001",
		Email:         "Person@Example.test",
		EmailVerified: true,
		Name:          "A Person",
	})

	verifier := oauth2.GenerateVerifier()
	code, state := authorize(t, p, "the-state", "the-nonce", verifier)
	if state != "the-state" {
		t.Errorf("the provider returned state %q, want the one it was given", state)
	}

	claims, err := p.Exchange(context.Background(), code, verifier, "the-nonce")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims.Subject != "subject-0001" {
		t.Errorf("subject = %q, want subject-0001", claims.Subject)
	}
	if claims.Email != "Person@Example.test" {
		t.Errorf("email = %q, want it verbatim -- canonicalising is the caller's job", claims.Email)
	}
	if !claims.EmailVerified {
		t.Error("email_verified = false on a token that said true")
	}
	if claims.Name != "A Person" {
		t.Errorf("name = %q, want %q", claims.Name, "A Person")
	}
}

// Every way an ID token can be unbelievable, and the reason each one lands on.
//
// The three signature cases are the ones worth naming. A token signed by a key
// that is not in the JWKS is the forgery; `alg: none` and an HMAC keyed with
// the RSA public key are the two classic confusions, and both are refused
// because the algorithm list is pinned to RS256 rather than read from the
// token's own header.
func TestExchangeRefusesEveryBadToken(t *testing.T) {
	for _, c := range []struct {
		name     string
		identity oidcstub.Identity
		want     error
	}{
		// Every row here carries email_verified: true, so the only thing that
		// can refuse it is the check the row is named after. A row that was
		// unverified as well would still fail when its own check was removed --
		// it would just fail for the wrong reason, and say so.
		{"a signature from a key the JWKS does not carry", oidcstub.Identity{EmailVerified: true, Mode: oidcstub.ModeForeignKey}, ErrVerify},
		{"an expired token", oidcstub.Identity{EmailVerified: true, Mode: oidcstub.ModeExpired}, ErrVerify},
		{"a token for another client", oidcstub.Identity{EmailVerified: true, Mode: oidcstub.ModeWrongAudience}, ErrVerify},
		{"an issuer one prefix away from the discovered one", oidcstub.Identity{EmailVerified: true, Mode: oidcstub.ModeBareIssuer}, ErrVerify},
		{"alg: none", oidcstub.Identity{EmailVerified: true, Mode: oidcstub.ModeAlgNone}, ErrVerify},
		{"HS256 keyed with the RSA public key", oidcstub.Identity{EmailVerified: true, Mode: oidcstub.ModeHS256}, ErrVerify},
		{"a nonce from another flow", oidcstub.Identity{EmailVerified: true, Mode: oidcstub.ModeBadNonce}, ErrNonce},
		{"no id_token at all", oidcstub.Identity{EmailVerified: true, Mode: oidcstub.ModeNoIDToken}, ErrExchange},
		{"email_verified false", oidcstub.Identity{EmailVerified: false}, ErrEmailUnverified},
		{"no email_verified claim", oidcstub.Identity{OmitEmailVerified: true}, ErrEmailUnverified},
		// The claim as a string is the fail-closed case: "true" is not a bool,
		// the decode fails, and the flow is refused rather than quietly reading
		// a non-empty string as truth.
		{"email_verified as the string \"true\"", oidcstub.Identity{EmailVerifiedRaw: "true"}, ErrVerify},
		{"a verified token with no email", oidcstub.Identity{EmailVerified: true, OmitEmail: true}, ErrVerify},
	} {
		t.Run(c.name, func(t *testing.T) {
			stub, p := newStubProvider(t)
			id := c.identity
			if id.Subject == "" {
				id.Subject = "subject-0002"
			}
			if id.Email == "" && !id.OmitEmail {
				id.Email = "person@example.test"
			}
			stub.SetIdentity(id)

			verifier := oauth2.GenerateVerifier()
			code, _ := authorize(t, p, "the-state", "the-nonce", verifier)

			_, err := p.Exchange(context.Background(), code, verifier, "the-nonce")
			if err == nil {
				t.Fatal("Exchange accepted the token")
			}
			if !errors.Is(err, c.want) {
				t.Errorf("error = %v, want one wrapping %v", err, c.want)
			}
		})
	}
}

// The issuer claim is held to the configured issuer exactly, apart from the one
// exception go-oidc makes for Google -- which this check makes too, so that the
// two agree.
//
// The library checks the issuer itself and then carves out that exception: with
// the issuer configured as Google's https:// form it also accepts the same host
// with the scheme stripped, because Google has been known to send that
// (oidc/verify.go, v3.20.0). Re-checking here more strictly than that would not
// hold anything the library does not already hold -- the token's signature was
// verified against the configured issuer's key set and its aud against this
// client -- it would only mean that the day Google sends the scheme-less form,
// every sign-in on the deployment fails with nothing to configure. Every other
// issuer stays exact, including a scheme-stripped one.
//
// The check is exercised directly rather than through the stub, because the
// stub cannot serve the host the exception is written for -- a flow through it
// would only ever prove go-oidc's own check again, which is what the
// bare-issuer row in the table above already does.
func TestTheIssuerCheckIsExactApartFromGoogle(t *testing.T) {
	const issuer = "https://accounts.example.test"
	const google = "https://accounts.google.com"

	for _, c := range []struct {
		name       string
		configured string
		iss        string
		ok         bool
	}{
		{"the configured issuer itself", issuer, issuer, true},
		// The scheme-less form of an issuer that is not Google: refused, because
		// the exception is Google's alone in the library and here.
		{"a scheme-stripped issuer that is not Google", issuer, "accounts.example.test", false},
		{"the wrong scheme", issuer, "http://accounts.example.test", false},
		{"a trailing slash", issuer, issuer + "/", false},
		{"another provider entirely", issuer, "https://accounts.evil.test", false},
		{"no issuer at all", issuer, "", false},
		{"Google, spelled the way the spec wants", google, google, true},
		// The one exception: the spelling Google has been observed to send.
		{"Google, scheme-stripped", google, "accounts.google.com", true},
		// And the exception is an equality, not a prefix match: a host that
		// merely starts with Google's, which is what a lookalike registration
		// buys an attacker, gets nothing from it.
		{"a host that only starts like Google's", google, "accounts.google.com.example.test", false},
		{"the scheme-less form with a trailing slash", google, "accounts.google.com/", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := New(Config{ClientID: testClientID, ClientSecret: testClientSecret, Issuer: c.configured})
			err := p.requireIssuer(c.iss)
			if c.ok && err != nil {
				t.Errorf("requireIssuer(%q) with %q configured = %v, want nil", c.iss, c.configured, err)
			}
			if !c.ok && !errors.Is(err, ErrVerify) {
				t.Errorf("requireIssuer(%q) with %q configured = %v, want one wrapping ErrVerify", c.iss, c.configured, err)
			}
		})
	}

	// And it is consulted on the exchange path, not merely defined. Discovery
	// is cached by the time the code is exchanged, so moving the configured
	// issuer afterwards leaves the verifier checking against the string the
	// discovery document published -- which the token still carries. Anything
	// that refuses it now is this check.
	stub, live := newStubProvider(t)
	stub.SetIdentity(oidcstub.Identity{
		Subject:       "subject-0003",
		Email:         "person@example.test",
		EmailVerified: true,
	})
	verifier := oauth2.GenerateVerifier()
	code, _ := authorize(t, live, "the-state", "the-nonce", verifier)

	live.cfg.Issuer = strings.TrimPrefix(live.cfg.Issuer, "http://")
	if _, err := live.Exchange(context.Background(), code, verifier, "the-nonce"); !errors.Is(err, ErrVerify) {
		t.Errorf("Exchange accepted a token whose issuer is not the configured one: %v", err)
	}
}

// PKCE: the code is worthless without the verifier that started the flow.
func TestExchangeRefusesAWrongPKCEVerifier(t *testing.T) {
	_, p := newStubProvider(t)

	verifier := oauth2.GenerateVerifier()
	code, _ := authorize(t, p, "the-state", "the-nonce", verifier)

	_, err := p.Exchange(context.Background(), code, oauth2.GenerateVerifier(), "the-nonce")
	if err == nil {
		t.Fatal("Exchange accepted a code with somebody else's verifier")
	}
	if !errors.Is(err, ErrExchange) {
		t.Errorf("error = %v, want one wrapping ErrExchange", err)
	}
}

// Codes are single-use at the provider, which is what makes a replayed callback
// unusable even before the flow cookie is considered.
func TestCodesAreBurntOnFirstUse(t *testing.T) {
	_, p := newStubProvider(t)

	verifier := oauth2.GenerateVerifier()
	code, _ := authorize(t, p, "the-state", "the-nonce", verifier)

	if _, err := p.Exchange(context.Background(), code, verifier, "the-nonce"); err != nil {
		t.Fatalf("the first exchange: %v", err)
	}
	if _, err := p.Exchange(context.Background(), code, verifier, "the-nonce"); err == nil {
		t.Fatal("the same code was redeemed twice")
	}
}

// ----------------------------------------------------------- discovery ------

// Nothing here talks to the provider until somebody signs in.
//
// The provider being unreachable at boot must not stop the server starting, so
// New does no I/O at all -- proven against an issuer that is not listening.
func TestNewDoesNoNetworkIO(t *testing.T) {
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	New(Config{ClientID: testClientID, ClientSecret: testClientSecret, Issuer: ts.URL})
	if got := hits.Load(); got != 0 {
		t.Errorf("New made %d requests to the provider, want 0", got)
	}
}

// A provider outage costs one round trip per window, not one per click.
func TestAFailedDiscoveryIsRememberedBriefly(t *testing.T) {
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	p := New(Config{ClientID: testClientID, ClientSecret: testClientSecret, Issuer: ts.URL})
	now := time.Now()
	p.now = func() time.Time { return now }

	if _, err := p.AuthCodeURL(context.Background(), "s", "n", "v"); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("the first attempt: error = %v, want one wrapping ErrDiscovery", err)
	}
	first := hits.Load()
	if first == 0 {
		t.Fatal("the first attempt never reached the provider")
	}

	for range 5 {
		if _, err := p.AuthCodeURL(context.Background(), "s", "n", "v"); !errors.Is(err, ErrDiscovery) {
			t.Fatalf("a repeat attempt: error = %v, want one wrapping ErrDiscovery", err)
		}
	}
	if got := hits.Load(); got != first {
		t.Errorf("the provider was asked %d more times inside the window, want 0", got-first)
	}

	// And once the window has passed, it is tried again -- nothing keeps the
	// failure alive after the provider recovers.
	now = now.Add(discoveryFailureTTL + time.Second)
	if _, err := p.AuthCodeURL(context.Background(), "s", "n", "v"); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("after the window: error = %v, want one wrapping ErrDiscovery", err)
	}
	if got := hits.Load(); got <= first {
		t.Error("the provider was never retried after the window elapsed")
	}
}

// Concurrent first sign-ins are coalesced into one discovery rather than queued
// behind it. A mutex here would park the whole auth surface behind one slow
// round trip during an outage, and nothing would ever end those requests.
func TestConcurrentFirstSignInsShareOneDiscovery(t *testing.T) {
	stub, err := oidcstub.New(testClientID, testClientSecret)
	if err != nil {
		t.Fatalf("building the stub: %v", err)
	}

	var discoveries atomic.Int64
	release := make(chan struct{})
	inner := stub.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == oidcstub.DiscoveryPath {
			discoveries.Add(1)
			// Held until every caller has arrived, so a second discovery would
			// have to be a second request rather than a fast repeat.
			<-release
		}
		inner.ServeHTTP(w, r)
	}))
	defer ts.Close()
	stub.SetBaseURL(ts.URL)

	p := New(Config{ClientID: testClientID, ClientSecret: testClientSecret, Issuer: ts.URL, RedirectURL: testRedirectURL})

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = p.AuthCodeURL(context.Background(), "s", "n", oauth2.GenerateVerifier())
		}()
	}

	// Every caller is now either in the flight or waiting on it.
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if got := discoveries.Load(); got != 1 {
		t.Errorf("%d discovery requests for %d concurrent sign-ins, want 1", got, callers)
	}
}

// The provider's responses are bounded in bytes, not only in time.
//
// go-oidc reads the discovery document and the JWKS with an unbounded
// io.ReadAll, and the ten-second timeout is no bound at all on a provider that
// is compromised, misbehaving, or reachable only through a broken TLS path: it
// can stream at line rate for the whole window into one allocation.
//
// The padded document is the stub's own, valid, with one enormous extra field,
// and the same server serves it unpadded to the second provider. So the only
// difference between the refusal and the success is the size, and the cap is
// the only thing that can be doing the refusing -- remove the wrapper and the
// first half passes.
func TestAnOversizedProviderResponseIsRefused(t *testing.T) {
	stub, err := oidcstub.New(testClientID, testClientSecret)
	if err != nil {
		t.Fatalf("building the stub: %v", err)
	}

	var pad atomic.Bool
	inner := stub.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oidcstub.DiscoveryPath || !pad.Load() {
			inner.ServeHTTP(w, r)
			return
		}
		// The real document, decoded and re-encoded with one huge field, so
		// what comes out is still valid JSON describing this issuer.
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Errorf("the stub's own discovery document did not decode: %v", err)
			return
		}
		doc["padding"] = strings.Repeat("a", maxProviderBody)
		out, err := json.Marshal(doc)
		if err != nil {
			t.Errorf("re-encoding the discovery document: %v", err)
			return
		}
		if len(out) <= maxProviderBody {
			t.Errorf("the padded document is %d bytes, which is inside the %d-byte cap", len(out), maxProviderBody)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	defer ts.Close()
	stub.SetBaseURL(ts.URL)

	cfg := Config{ClientID: testClientID, ClientSecret: testClientSecret, Issuer: ts.URL, RedirectURL: testRedirectURL}

	pad.Store(true)
	_, err = New(cfg).AuthCodeURL(context.Background(), "s", "n", "v")
	if !errors.Is(err, ErrDiscovery) {
		t.Fatalf("an oversized discovery document: error = %v, want one wrapping ErrDiscovery", err)
	}
	// Named, not merely wrapped: ErrDiscovery is also what an unreachable
	// provider and a malformed document produce, and this has to be the cap.
	if !strings.Contains(err.Error(), errBodyTooLarge.Error()) {
		t.Errorf("error = %v, want it to carry %q", err, errBodyTooLarge)
	}

	// The same document without the padding, on a fresh provider because the
	// failure above is remembered for half a minute.
	pad.Store(false)
	if _, err := New(cfg).AuthCodeURL(context.Background(), "s", "n", "v"); err != nil {
		t.Fatalf("a normal-sized discovery document was refused: %v", err)
	}
}

// The other unbounded read, on the other side of the flow: the JWKS.
//
// The cap lives on the client rather than at either call site, so the two are
// not the same test -- a wrap applied only where discovery is fetched would
// leave the key set unbounded and the case above would not notice. This one
// keeps discovery normal-sized and pads only the JWKS, so the refusal can come
// from nowhere else, and the same server serves it unpadded to a second
// provider to show the flow is otherwise fine.
func TestAnOversizedJWKSIsRefused(t *testing.T) {
	stub, err := oidcstub.New(testClientID, testClientSecret)
	if err != nil {
		t.Fatalf("building the stub: %v", err)
	}

	var pad atomic.Bool
	inner := stub.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oidcstub.JWKSPath || !pad.Load() {
			inner.ServeHTTP(w, r)
			return
		}
		// The real key set, decoded and re-encoded with one huge field, so what
		// comes out still carries the key the token was signed with: only the
		// size can be doing the refusing.
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Errorf("the stub's own JWKS did not decode: %v", err)
			return
		}
		doc["padding"] = strings.Repeat("a", maxProviderBody)
		out, err := json.Marshal(doc)
		if err != nil {
			t.Errorf("re-encoding the JWKS: %v", err)
			return
		}
		if len(out) <= maxProviderBody {
			t.Errorf("the padded key set is %d bytes, which is inside the %d-byte cap", len(out), maxProviderBody)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	defer ts.Close()
	stub.SetBaseURL(ts.URL)
	stub.SetIdentity(oidcstub.Identity{
		Subject:       "subject-0003",
		Email:         "person@example.test",
		EmailVerified: true,
	})

	cfg := Config{ClientID: testClientID, ClientSecret: testClientSecret, Issuer: ts.URL, RedirectURL: testRedirectURL}

	// The token is signed, the exchange succeeds, and the verification cannot
	// get the key to check it with.
	pad.Store(true)
	padded := New(cfg)
	verifier := oauth2.GenerateVerifier()
	code, _ := authorize(t, padded, "the-state", "the-nonce", verifier)
	_, err = padded.Exchange(context.Background(), code, verifier, "the-nonce")
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("an oversized key set: error = %v, want one wrapping ErrVerify", err)
	}
	// Named, not merely wrapped: every bad signature in this file lands on
	// ErrVerify too, and this has to be the cap.
	if !strings.Contains(err.Error(), errBodyTooLarge.Error()) {
		t.Errorf("error = %v, want it to carry %q", err, errBodyTooLarge)
	}

	// The same key set without the padding, on a fresh provider because go-oidc
	// keeps the one it fetched.
	pad.Store(false)
	plain := New(cfg)
	verifier = oauth2.GenerateVerifier()
	code, _ = authorize(t, plain, "the-state", "the-nonce", verifier)
	if _, err := plain.Exchange(context.Background(), code, verifier, "the-nonce"); err != nil {
		t.Fatalf("a normal-sized key set was refused: %v", err)
	}
}

// A guard on the stub itself: the discovery document has to publish exactly the
// base URL it was told, or go-oidc refuses it and every case above fails for a
// reason that has nothing to do with the code under test.
func TestStubPublishesTheIssuerItWasGiven(t *testing.T) {
	stub, err := oidcstub.New(testClientID, testClientSecret)
	if err != nil {
		t.Fatalf("building the stub: %v", err)
	}
	ts := httptest.NewServer(stub.Handler())
	defer ts.Close()
	stub.SetBaseURL(ts.URL)

	resp, err := http.Get(ts.URL + oidcstub.DiscoveryPath)
	if err != nil {
		t.Fatalf("fetching the discovery document: %v", err)
	}
	defer resp.Body.Close()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), `"issuer":"`+ts.URL+`"`) {
		t.Errorf("the discovery document does not publish %q: %s", ts.URL, body[:n])
	}
}
