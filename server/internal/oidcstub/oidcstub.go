// Package oidcstub is a fake OpenID Connect provider: discovery, a JWKS, an
// authorization endpoint and a token endpoint, over plain http on loopback.
//
// It exists so the sign-in flow can be driven end to end without a network and
// without a real Google client, and so the negative cases -- a forged
// signature, an expired token, a wrong audience, a mismatched nonce, `alg:
// none`, an HMAC signed with the RSA public key -- can be produced on demand
// rather than described in a comment.
//
// It signs ID tokens by hand. That is deliberate and is not a duplication of
// the verifier: a signer written here and a verifier written by go-oidc are two
// independent implementations, and a test only passes when both agree. A stub
// that reused the verifier's own code would prove nothing.
//
// It imports no testing package, because it has two consumers: the Go suite
// wraps it in httptest, and server/cmd/oidcstub serves it for `make e2e`.
//
// This is a fixture. It mints signed identity assertions for anything that
// asks, so it belongs on loopback and nowhere else -- the binary enforces that;
// this package cannot, since it only ever hands back a handler.
package oidcstub

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Paths the stub serves. They are published through the discovery document, so
// nothing but the discovery path itself is a contract with the client.
const (
	DiscoveryPath = "/.well-known/openid-configuration"
	JWKSPath      = "/jwks"
	AuthorizePath = "/authorize"
	TokenPath     = "/token"
	ControlPath   = "/control"
)

// Modes arm one bad ID token for the next authorization. The zero value --
// ModeGood -- issues a token that verifies.
const (
	ModeGood = ""
	// ModeForeignKey signs with a key that is not in the JWKS, under the kid
	// that is: the key lookup succeeds and the signature check is what fails.
	ModeForeignKey = "foreign_key"
	// ModeExpired issues a token whose exp is an hour in the past.
	ModeExpired = "expired"
	// ModeWrongAudience issues a token for a different client.
	ModeWrongAudience = "wrong_aud"
	// ModeBadNonce issues a token whose nonce claim is not the one the
	// authorization was handed.
	ModeBadNonce = "bad_nonce"
	// ModeAlgNone issues an unsigned token claiming alg "none".
	ModeAlgNone = "alg_none"
	// ModeHS256 issues an HS256 token whose HMAC secret is the RSA public key
	// -- the classic algorithm-confusion attack, which works against any
	// verifier that picks its algorithm from the header.
	ModeHS256 = "hs256"
	// ModeBareIssuer issues a token whose iss claim is the issuer with its
	// scheme stripped. A verifier must accept the discovery document's own
	// issuer string and nothing else, including a form of it that is one
	// prefix away.
	ModeBareIssuer = "bare_issuer"
	// ModeNoIDToken answers the token endpoint with no id_token at all.
	ModeNoIDToken = "no_id_token"
)

// Identity is what the next authorization signs a token about.
type Identity struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	// EmailVerifiedRaw, when non-empty, is written into the claim verbatim
	// instead of EmailVerified -- so a token can carry `"email_verified":
	// "true"`, the string, which is what a fail-closed claims decode has to
	// refuse.
	EmailVerifiedRaw string `json:"email_verified_raw"`
	// OmitEmailVerified drops the claim entirely.
	OmitEmailVerified bool `json:"omit_email_verified"`
	// OmitEmail drops the email claim entirely.
	OmitEmail bool `json:"omit_email"`
	// Mode arms one of the bad tokens above.
	Mode string `json:"mode"`
}

// defaultIdentity is who the stub is until POST /control says otherwise.
var defaultIdentity = Identity{
	Subject:       "stub-subject-0001",
	Email:         "stub-user@example.test",
	EmailVerified: true,
	Name:          "Stub User",
}

// Stub is the fake provider. Build one with New, tell it its own base URL with
// SetBaseURL, and serve it.
type Stub struct {
	clientID     string
	clientSecret string

	key      *rsa.PrivateKey
	foreign  *rsa.PrivateKey
	kid      string
	pubKeyHS []byte // the PEM of the public key, used as the HS256 secret

	mu       sync.Mutex
	baseURL  string
	identity Identity
	codes    map[string]*flow
}

// flow is one authorization in progress: what /authorize was handed and who
// /token will say it was.
type flow struct {
	state        string
	nonce        string
	challenge    string
	redirectURI  string
	identity     Identity
	used         bool
	authorizedAt time.Time
}

// New builds a stub with its own RSA-2048 signing key and a second key it never
// publishes, for the forged-signature case.
//
// clientID and clientSecret are what /token demands. Nothing here has a default
// for them: a stub that accepted any client would not be testing the exchange.
func New(clientID, clientSecret string) (*Stub, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("oidcstub: generating the signing key: %w", err)
	}
	foreign, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("oidcstub: generating the foreign key: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("oidcstub: encoding the public key: %w", err)
	}

	return &Stub{
		clientID:     clientID,
		clientSecret: clientSecret,
		key:          key,
		foreign:      foreign,
		kid:          "stub-key-0001",
		pubKeyHS:     pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}),
		identity:     defaultIdentity,
		codes:        map[string]*flow{},
	}, nil
}

// SetBaseURL tells the stub the origin it is reachable at.
//
// It has to be told rather than know: go-oidc refuses a discovery document
// whose issuer differs by a byte from the string it was asked to discover, and
// an httptest server does not know its own port until it is already listening.
// Everything the discovery document publishes -- the issuer and all three
// endpoints -- is built from exactly this string.
func (s *Stub) SetBaseURL(base string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.baseURL = strings.TrimSuffix(base, "/")
}

// Issuer is the string the stub publishes as its own issuer.
func (s *Stub) Issuer() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baseURL
}

// SetIdentity is the in-process form of POST /control.
func (s *Stub) SetIdentity(id Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identity = id
}

// Handler routes the stub's four endpoints plus /control.
func (s *Stub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(DiscoveryPath, s.discovery)
	mux.HandleFunc(JWKSPath, s.jwks)
	mux.HandleFunc(AuthorizePath, s.authorize)
	mux.HandleFunc(TokenPath, s.token)
	mux.HandleFunc(ControlPath, s.control)
	return mux
}

// ---------------------------------------------------------------- endpoints --

func (s *Stub) discovery(w http.ResponseWriter, _ *http.Request) {
	base := s.Issuer()
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + AuthorizePath,
		"token_endpoint":                        base + TokenPath,
		"jwks_uri":                              base + JWKSPath,
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "email", "profile"},
		"claims_supported":                      []string{"sub", "email", "email_verified", "name", "iss", "aud", "exp", "iat", "nonce"},
	})
}

func (s *Stub) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := s.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": s.kid,
			"n":   b64(pub.N.Bytes()),
			"e":   b64(big.NewInt(int64(pub.E)).Bytes()),
		}},
	})
}

// authorize records the flow and sends the browser back with a code.
//
// It checks only what a real provider would refuse outright -- an unknown
// client, a missing redirect_uri, a challenge method that is not S256. Whether
// the caller sent a state or a nonce is the caller's problem, and a stub that
// insisted on them would hide exactly the bug the state and nonce tests are
// looking for.
func (s *Stub) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != s.clientID {
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" {
		http.Error(w, "no redirect_uri", http.StatusBadRequest)
		return
	}
	if method := q.Get("code_challenge_method"); method != "" && method != "S256" {
		http.Error(w, "only S256 is supported", http.StatusBadRequest)
		return
	}

	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "no randomness", http.StatusInternalServerError)
		return
	}
	code := b64(raw)

	s.mu.Lock()
	s.codes[code] = &flow{
		state:        q.Get("state"),
		nonce:        q.Get("nonce"),
		challenge:    q.Get("code_challenge"),
		redirectURI:  redirectURI,
		identity:     s.identity,
		authorizedAt: time.Now(),
	}
	s.mu.Unlock()

	back, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "unparseable redirect_uri", http.StatusBadRequest)
		return
	}
	rq := back.Query()
	rq.Set("code", code)
	if state := q.Get("state"); state != "" {
		rq.Set("state", state)
	}
	back.RawQuery = rq.Encode()

	w.Header().Set("Location", back.String())
	w.WriteHeader(http.StatusFound)
}

// token exchanges a code, once.
//
// The burn is the point: it is what lets a replay test assert that the flow
// cookie was cleared rather than lean on a real provider's single-use codes.
func (s *Stub) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenErr(w, "invalid_request", "unparseable form")
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	if id != s.clientID || secret != s.clientSecret {
		tokenErr(w, "invalid_client", "bad client credentials")
		return
	}
	if r.PostFormValue("grant_type") != "authorization_code" {
		tokenErr(w, "unsupported_grant_type", "only authorization_code")
		return
	}

	code := r.PostFormValue("code")
	s.mu.Lock()
	f, known := s.codes[code]
	s.mu.Unlock()

	if !known {
		tokenErr(w, "invalid_grant", "unknown code")
		return
	}
	if f.challenge != "" {
		verifier := r.PostFormValue("code_verifier")
		if verifier == "" || s256(verifier) != f.challenge {
			tokenErr(w, "invalid_grant", "PKCE verifier does not match the challenge")
			return
		}
	}

	// The burn: one check-and-set under the lock, so two simultaneous exchanges
	// of one code cannot both come out the other side.
	//
	// It happens here rather than at the lookup because a code is spent by a
	// successful exchange, not by a rejected one -- and that order is what keeps
	// the PKCE case honest. x/oauth2 auto-detects the client-authentication
	// style by retrying a failed exchange the other way, so a stub that burnt on
	// the way in would answer the retry with "already redeemed", and a test
	// asserting that a wrong verifier is refused would be passing on the replay
	// guard instead of on PKCE.
	s.mu.Lock()
	alreadyUsed := f.used
	f.used = true
	s.mu.Unlock()
	if alreadyUsed {
		tokenErr(w, "invalid_grant", "code already redeemed")
		return
	}

	if f.identity.Mode == ModeNoIDToken {
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "stub-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
		return
	}

	idToken, err := s.mintIDToken(f)
	if err != nil {
		tokenErr(w, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": "stub-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

// control sets who the next authorization is about, and which bad token -- if
// any -- it issues. The body is an Identity.
func (s *Stub) control(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var id Identity
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&id); err != nil {
		http.Error(w, "expected an identity object", http.StatusBadRequest)
		return
	}
	s.SetIdentity(id)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// -------------------------------------------------------------------- JWTs --

// mintIDToken builds and signs the ID token for a flow, honouring its mode.
func (s *Stub) mintIDToken(f *flow) (string, error) {
	issuer := s.Issuer()
	now := time.Now()

	claims := map[string]any{
		"iss": issuer,
		"aud": s.clientID,
		"sub": f.identity.Subject,
		"exp": now.Add(time.Hour).Unix(),
		"iat": now.Unix(),
	}
	if f.nonce != "" {
		claims["nonce"] = f.nonce
	}
	if !f.identity.OmitEmail {
		claims["email"] = f.identity.Email
	}
	if !f.identity.OmitEmailVerified {
		if raw := f.identity.EmailVerifiedRaw; raw != "" {
			claims["email_verified"] = raw
		} else {
			claims["email_verified"] = f.identity.EmailVerified
		}
	}
	if f.identity.Name != "" {
		claims["name"] = f.identity.Name
	}

	switch f.identity.Mode {
	case ModeExpired:
		claims["exp"] = now.Add(-time.Hour).Unix()
		claims["iat"] = now.Add(-2 * time.Hour).Unix()
	case ModeWrongAudience:
		claims["aud"] = "some-other-client-0002"
	case ModeBadNonce:
		claims["nonce"] = "not-the-nonce-this-flow-was-given"
	case ModeBareIssuer:
		claims["iss"] = stripScheme(issuer)
	}

	body, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("oidcstub: encoding claims: %w", err)
	}

	switch f.identity.Mode {
	case ModeAlgNone:
		header, err := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
		if err != nil {
			return "", err
		}
		return b64(header) + "." + b64(body) + ".", nil

	case ModeHS256:
		header, err := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT", "kid": s.kid})
		if err != nil {
			return "", err
		}
		signing := b64(header) + "." + b64(body)
		mac := hmac.New(sha256.New, s.pubKeyHS)
		mac.Write([]byte(signing))
		return signing + "." + b64(mac.Sum(nil)), nil

	default:
		key := s.key
		if f.identity.Mode == ModeForeignKey {
			key = s.foreign
		}
		header, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": s.kid})
		if err != nil {
			return "", err
		}
		signing := b64(header) + "." + b64(body)
		sum := sha256.Sum256([]byte(signing))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			return "", fmt.Errorf("oidcstub: signing: %w", err)
		}
		return signing + "." + b64(sig), nil
	}
}

// ----------------------------------------------------------------- helpers --

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return b64(sum[:])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// stripScheme removes "https://" or "http://" from the front of a URL, which is
// how the bare-issuer case is built: a string one prefix away from the issuer
// the discovery document published.
func stripScheme(u string) string {
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(u, prefix) {
			return strings.TrimPrefix(u, prefix)
		}
	}
	return u
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func tokenErr(w http.ResponseWriter, code, description string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":             code,
		"error_description": description,
	})
}
