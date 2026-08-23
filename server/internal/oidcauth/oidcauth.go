// Package oidcauth is Drive's side of an OpenID Connect authorization-code
// flow: the authorization URL, the code exchange, and the ID-token verification
// that turns a redirect back from an identity provider into a claim about a
// person.
//
// It is a package rather than three functions in the HTTP handler so that the
// part where a mistake is silent -- signature, issuer, audience, expiry, nonce
// -- can be tested against a fake provider directly, instead of being diagnosed
// through a 302 that says the same thing for every cause.
//
// Discovery is lazy and coalesced. Building the provider means an HTTP round
// trip to somebody else's server, and boot must not depend on a third party
// being reachable -- config.ValidateRuntime already declines to probe Resend
// for the same reason. So the first sign-in builds it, concurrent first
// sign-ins are coalesced into one attempt rather than queued behind a mutex,
// and a failure is remembered briefly so an outage costs one round trip per
// half-minute rather than one per click.
package oidcauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

// The refusal reasons, as distinguishable errors.
//
// The caller answers all of them identically -- one redirect, one empty body --
// but it logs which one, and without these it could not: a wrapped error string
// is not something a log line's constant reason can be derived from.
var (
	// ErrDiscovery means the provider's discovery document or JWKS could not be
	// fetched or did not describe the issuer we asked for.
	ErrDiscovery = errors.New("oidcauth: the provider could not be discovered")
	// ErrExchange means the authorization code could not be traded for tokens.
	ErrExchange = errors.New("oidcauth: the code exchange failed")
	// ErrVerify covers every reason the ID token is not believable: signature,
	// issuer, audience, expiry, or an unusable set of claims.
	ErrVerify = errors.New("oidcauth: the ID token did not verify")
	// ErrNonce means the token verified but belongs to a different flow.
	ErrNonce = errors.New("oidcauth: the ID token's nonce is not this flow's")
	// ErrEmailUnverified means the provider will not vouch for the address.
	ErrEmailUnverified = errors.New("oidcauth: the provider does not report this address as verified")
)

// discoveryFailureTTL is how long a failed discovery is remembered.
//
// It exists so a provider outage costs one round trip per window instead of one
// per click, and it is short so that nothing keeps the failure alive after the
// provider recovers.
const discoveryFailureTTL = 30 * time.Second

// httpTimeout bounds every request this package makes to the provider --
// discovery, the JWKS fetch and the token exchange. All three run on a request
// goroutine that nothing else will ever time out: the server sets
// ReadHeaderTimeout and no WriteTimeout, on purpose, because uploads take
// hours.
const httpTimeout = 10 * time.Second

// maxProviderBody bounds every response this package reads, because the timeout
// on its own does not: go-oidc reads both the discovery document and the JWKS
// with an unbounded io.ReadAll, so a provider that is compromised, misbehaving,
// or reachable only through a broken TLS path can stream at line rate for the
// whole ten seconds into one allocation. The issuer is operator-configured, so
// this is depth rather than a reachable attack -- and 1 MiB is two orders of
// magnitude above any real discovery document or key set.
const maxProviderBody = 1 << 20

// errBodyTooLarge is what a caller reading past the cap is told. It is not one
// of the package's exported reasons: it surfaces wrapped in whatever go-oidc
// makes of a failed read, which the discovery and verification paths already
// turn into ErrDiscovery and ErrVerify.
var errBodyTooLarge = errors.New("oidcauth: the provider's response exceeded the size limit")

// Config is what a deployment knows before it has ever talked to the provider.
type Config struct {
	ClientID     string
	ClientSecret string
	// Issuer is the discovery root. The verifier accepts the issuer string the
	// discovery document publishes and nothing else.
	Issuer string
	// RedirectURL is where the provider sends the browser back. It is derived
	// from the deployment's own base URL, never from a request header.
	RedirectURL string
	// HTTPClient is optional; nil means a client with httpTimeout.
	HTTPClient *http.Client
}

// Claims is the part of a verified ID token Drive acts on.
type Claims struct {
	// Subject is the provider's stable, never-reused id for this person. It is
	// the only field an account is looked up by after the first sign-in.
	Subject string
	Email   string
	// EmailVerified is a bool and is decoded as one. A provider that sends the
	// claim as a string, or omits it, fails the decode and the flow is refused:
	// the only safe reading of "we could not tell whether this address is
	// verified" is "it is not".
	EmailVerified bool
	Name          string
}

// Provider holds the lazily discovered provider and the two things built from
// it: the OAuth2 client and the ID-token verifier.
type Provider struct {
	cfg    Config
	client *http.Client

	group singleflight.Group

	mu       sync.Mutex
	ready    *discovered
	failedAt time.Time
	now      func() time.Time
}

type discovered struct {
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewVerifier mints a PKCE code verifier -- 32 octets of randomness, per RFC
// 7636. It is re-exported here so the OAuth2 library stays behind this package
// and the handler has one import for the whole flow.
func NewVerifier() string { return oauth2.GenerateVerifier() }

// New builds a Provider. It performs no network I/O: nothing here can stop the
// server from starting.
func New(cfg Config) *Provider {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	// Bounded in bytes as well as in time, and bounded here rather than at each
	// call site so that every request this package makes -- discovery, the JWKS
	// fetch and its refetches, the token exchange -- is covered by construction,
	// including the ones made inside go-oidc where there is no body to wrap. An
	// injected client is copied rather than mutated: it belongs to the caller.
	bounded := *client
	bounded.Transport = boundedTransport{base: client.Transport}
	return &Provider{cfg: cfg, client: &bounded, now: time.Now}
}

// boundedTransport caps the body of every response it carries.
type boundedTransport struct{ base http.RoundTripper }

func (t boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	// The RoundTripper contract promises a non-nil body, but this wraps
	// whatever base it was handed -- including a test double or a middleware
	// that does not keep the promise, where the wrap would panic on the first
	// read rather than fail the sign-in.
	if resp.Body != nil {
		resp.Body = &boundedBody{inner: resp.Body, left: maxProviderBody + 1}
	}
	return resp, nil
}

// boundedBody fails once the cap is passed rather than truncating there.
//
// A plain io.LimitReader would hand go-oidc a short document, which decodes as
// a JSON syntax error somewhere in the middle of a file that is fine at the
// source -- or, worse, as a key set that simply does not contain the kid. An
// error says what happened.
//
// left is the cap plus one, so a body up to one byte past maxProviderBody still
// reads to EOF -- net/http answers the read that finishes a response with
// (n>0, io.EOF) rather than a bare (n, nil), and the err == nil guard below
// does not fire on it. Only a body that keeps going past that is refused: the
// first size this rejects is the cap plus two.
type boundedBody struct {
	inner io.ReadCloser
	left  int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errBodyTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.inner.Read(p)
	b.left -= int64(n)
	if b.left <= 0 && err == nil {
		return n, errBodyTooLarge
	}
	return n, err
}

func (b *boundedBody) Close() error { return b.inner.Close() }

// AuthCodeURL is where the browser is sent to start a sign-in.
//
// prompt=select_account is not decoration. Without it the provider silently
// reuses whichever account the browser is already signed into, which on a
// shared machine means signing somebody into the wrong Drive -- or creating one
// for them.
//
// access_type stays online: no refresh token is asked for, none is stored, and
// the access token is discarded the moment the exchange returns, because Drive
// never calls the provider again after sign-in.
func (p *Provider) AuthCodeURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	return d.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "select_account"),
	), nil
}

// Exchange trades the code for tokens and returns the claims of a verified ID
// token, or the first reason it stopped.
//
// The order is the contract, and each step is only reached because the one
// before it held: exchange, then the response actually carries an id_token,
// then the token verifies (signature by kid against the issuer's JWKS,
// algorithm pinned, issuer and audience checked, expiry enforced), then the
// nonce is this flow's, then the claims decode, then the address is one the
// provider vouches for. Nothing touches a database until all of it has passed.
func (p *Provider) Exchange(ctx context.Context, code, verifier, nonce string) (*Claims, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}

	ctx = oidc.ClientContext(ctx, p.client)
	tok, err := d.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrExchange, err)
	}

	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return nil, fmt.Errorf("%w: the token response carried no id_token", ErrExchange)
	}

	idToken, err := d.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVerify, err)
	}
	if err := p.requireIssuer(idToken.Issuer); err != nil {
		return nil, err
	}

	// Constant time, because this is a secret comparison even though a mismatch
	// is not exploitable through timing on its own.
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return nil, ErrNonce
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: the claims did not decode: %w", ErrVerify, err)
	}
	if !claims.EmailVerified {
		return nil, ErrEmailUnverified
	}
	if claims.Email == "" {
		return nil, fmt.Errorf("%w: the token carries no email claim", ErrVerify)
	}
	if idToken.Subject == "" {
		return nil, fmt.Errorf("%w: the token carries no subject", ErrVerify)
	}

	return &Claims{
		Subject:       idToken.Subject,
		Email:         claims.Email,
		EmailVerified: true,
		Name:          claims.Name,
	}, nil
}

// The two spellings of Google's issuer, as go-oidc spells them
// (oidc/verify.go, v3.20.0).
const (
	issuerGoogle         = "https://accounts.google.com"
	issuerGoogleNoScheme = "accounts.google.com"
)

// requireIssuer re-checks the ID token's iss claim against the configured
// issuer, making the same single exception the library makes and no other.
//
// go-oidc checks the issuer already, and then carves out one exception: with
// the issuer configured as Google's https:// form it also accepts the same host
// with the scheme stripped, because Google has been known to send that. The
// re-check exists so the claim is held on this package's own terms rather than
// on a dependency's, but it deliberately agrees with the library on that one
// case. A stricter re-check would not be safer, it would only be brittler: the
// spelling of the claim is Google's to change, no deployment can configure
// around it, and disagreeing would turn a provider-side quirk into every
// Google sign-in on the deployment failing until somebody shipped code.
//
// The exception widens nothing. By the time this runs the token's signature has
// been verified against the key set fetched from the *configured* issuer's
// discovery document, and aud has been required to carry this deployment's
// client id -- so the token is one Google signed for this client. The exception
// can only fire on a deployment that configured Google's issuer, i.e. that had
// already decided to trust that key set; accepting a second spelling of the
// string it named adds no provider to that set. Every other issuer stays exact.
func (p *Provider) requireIssuer(iss string) error {
	if iss == p.cfg.Issuer {
		return nil
	}
	if p.cfg.Issuer == issuerGoogle && iss == issuerGoogleNoScheme {
		return nil
	}
	return fmt.Errorf("%w: the ID token's issuer is not the one this deployment was configured with", ErrVerify)
}

// discover returns the built provider, building it at most once.
//
// singleflight rather than a mutex: a mutex would park every concurrent
// sign-in behind one slow discovery for as long as the provider took to answer,
// and nothing else would ever end those requests. Coalescing means they all
// wait for the same one round trip and then all proceed -- or all fail
// together, which is the same answer they would each have got.
func (p *Provider) discover(ctx context.Context) (*discovered, error) {
	if d, err := p.cached(); d != nil || err != nil {
		return d, err
	}

	v, err, _ := p.group.Do("discover", func() (any, error) {
		// Re-check inside the flight: the winner of a race that has just
		// finished may have filled this in while we were queuing.
		if d, err := p.cached(); d != nil || err != nil {
			return d, err
		}

		// The discovery round trip: detached from whichever request happened to
		// trigger it, since every coalesced caller is waiting on this one and a
		// cancelled request must not take the others' answer with it, and
		// bounded, because nothing else would ever end it.
		discoverCtx := oidc.ClientContext(context.WithoutCancel(ctx), p.client)
		discoverCtx, cancel := context.WithTimeout(discoverCtx, httpTimeout)
		defer cancel()

		provider, err := oidc.NewProvider(discoverCtx, p.cfg.Issuer)
		if err != nil {
			p.rememberFailure()
			return nil, fmt.Errorf("%w: %w", ErrDiscovery, err)
		}

		d := &discovered{
			oauth: &oauth2.Config{
				ClientID:     p.cfg.ClientID,
				ClientSecret: p.cfg.ClientSecret,
				RedirectURL:  p.cfg.RedirectURL,
				Endpoint:     provider.Endpoint(),
				Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
			},
			// The algorithm list is pinned explicitly rather than left to
			// whatever the discovery document advertises or the library
			// defaults to. It is what refuses `alg: none` and an HMAC signed
			// with the RSA public key -- the two classic JWT confusions -- and
			// pinning it here means neither a provider's metadata nor a
			// dependency bump can widen it.
			//
			// VerifierContext, not Verifier: the verifier holds a remote key
			// set that refetches the JWKS whenever it meets an unknown kid, and
			// the context handed here is the one it keeps for the life of the
			// process. So it is Background carrying the bounded client, and
			// explicitly not discoverCtx: that one is cancelled by the defer a
			// few lines below, and a verifier built on a dead context works
			// only because go-oidc happens to re-wrap it per verification --
			// every refetch would otherwise fail on a context this function
			// cancelled on its way out.
			verifier: provider.VerifierContext(oidc.ClientContext(context.Background(), p.client), &oidc.Config{
				ClientID:             p.cfg.ClientID,
				SupportedSigningAlgs: []string{oidc.RS256},
			}),
		}
		p.succeed(d)
		return d, nil
	})
	if err != nil {
		return nil, err
	}
	d, _ := v.(*discovered)
	if d == nil {
		return nil, ErrDiscovery
	}
	return d, nil
}

// cached answers with the built provider, or with the remembered failure while
// it is still fresh, or with (nil, nil) meaning "go and try".
func (p *Provider) cached() (*discovered, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ready != nil {
		return p.ready, nil
	}
	if !p.failedAt.IsZero() && p.now().Sub(p.failedAt) < discoveryFailureTTL {
		return nil, fmt.Errorf("%w: a recent attempt failed; not retrying yet", ErrDiscovery)
	}
	return nil, nil
}

func (p *Provider) succeed(d *discovered) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ready = d
	p.failedAt = time.Time{}
}

func (p *Provider) rememberFailure() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failedAt = p.now()
}
