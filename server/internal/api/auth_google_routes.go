package api

// Sign in with Google: two browser navigations and the account screen's view of
// what they linked.
//
// These two are unlike everything else under /api. They are GETs a browser
// follows, not calls a client makes, so they answer with redirects and empty
// bodies rather than the JSON envelope -- a person who followed a link and was
// shown `{"code":"rate_limited"}` has been given nothing they can act on.

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/oidcauth"
)

// OAuthCookie carries one sign-in in progress: the state, the nonce and the
// PKCE verifier, in a cookie scoped to the two routes that read it.
//
// It is not signed or encrypted and does not need to be. The server never
// trusts it for identity -- only compares it to the query and to the verified
// ID token -- so forging one gains an attacker nothing but a flow of their own.
const OAuthCookie = "drive_oauth"

// oauthCookiePath scopes the flow cookie to the two routes that read it, so it
// is not attached to every request the SPA makes.
const oauthCookiePath = "/api/auth/google"

// oauthFlowTTL is how long a sign-in may take.
//
// Fifteen minutes rather than ten because of what the cookie has to survive: a
// consent screen, an account chooser, and possibly a fresh provider password
// prompt on a phone.
const oauthFlowTTL = 15 * time.Minute

// Where a refused sign-in lands. Relative, always: a relative reference cannot
// be an open redirect however it was built, and it does not depend on
// DRIVE_BASE_URL being right for the landing to work.
const (
	loginPath         = "/login"
	loginGoogleError  = "/login?error=google"
	loginGoogleClosed = "/login?error=google_closed"
	afterSignIn       = "/"
)

// The constant reasons a refusal is logged under.
//
// The redirect is identical for all of them by design, so without a reason in
// the log a failed sign-in leaves no trace of which check refused it. It has to
// be its own Warn line rather than the request logger's: that one is Debug and
// production runs at info, so in production it is not written at all.
const (
	reasonNotConfigured  = "not_configured"
	reasonDiscovery      = "discovery_failed"
	reasonInternal       = "internal"
	reasonNoCookie       = "no_cookie"
	reasonBadState       = "bad_state"
	reasonNoCode         = "no_code"
	reasonProviderError  = "provider_error"
	reasonExchangeFailed = "exchange_failed"
	reasonVerifyFailed   = "verify_failed"
	reasonBadNonce       = "bad_nonce"
	reasonEmailUnverif   = "email_unverified"
	reasonBadEmail       = "bad_email"
	reasonSignupsClosed  = "signups_closed"
	reasonAlreadyLinked  = "already_linked"
	reasonLinkFailed     = "link_failed"
	reasonRateLimited    = "rate_limited"
)

// knownOAuthErrors is every error code RFC 6749 §4.1.2.1 and OpenID Connect
// Core §3.1.2.6 define. Anything else the provider sends is logged as
// "unknown".
//
// The parameter arrives on a URL a browser was sent to, so its contents are
// whatever the last hop chose to put there: without this, a caller who
// navigates to the callback with ?error=<ten kilobytes> writes ten kilobytes
// into the log for the cost of one request. The set is closed rather than
// length-capped because a truncated unknown string is still attacker-chosen
// text in a log line, and there is nothing to learn from it that the reason
// constant does not already say.
var knownOAuthErrors = map[string]bool{
	"access_denied":             true,
	"invalid_request":           true,
	"unauthorized_client":       true,
	"unsupported_response_type": true,
	"invalid_scope":             true,
	"server_error":              true,
	"temporarily_unavailable":   true,
	"interaction_required":      true,
	"login_required":            true,
	"consent_required":          true,
}

// oauthErrorCode is the provider's error parameter if it is one of the codes
// the specifications define, and "unknown" otherwise.
func oauthErrorCode(raw string) string {
	if knownOAuthErrors[raw] {
		return raw
	}
	return "unknown"
}

// oauthFlow is the cookie's payload.
type oauthFlow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
}

// authIdentityDTO is one row of GET /auth/identities. The subject is
// deliberately absent: it is the provider's internal id for a person and the
// account screen has no use for it.
type authIdentityDTO struct {
	ID          uuid.UUID  `json:"id"`
	Provider    string     `json:"provider"`
	EmailAtLink string     `json:"email_at_link"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
}

// ------------------------------------------------------------------ start ----

// authGoogleStart mints a flow and sends the browser to the provider.
//
// There is no `next` parameter and one is not read: an allowlist would be a
// mechanism to get wrong for a feature nobody asked for, and without it the
// only landing this flow has is the relative "/".
func (s *Server) authGoogleStart(w http.ResponseWriter, r *http.Request) {
	if s.Google == nil || !s.Cfg.UseGoogle() {
		// Mounted anyway, so that a deployment with no client configured
		// answers a browser that followed a link with something a browser can
		// act on. Unmounted, this would be /api's JSON 404.
		s.googleRefuse(w, r, reasonNotConfigured, nil)
		return
	}

	// The two minters read crypto/rand, and a failure there is this process's
	// problem and not the provider's -- logging it as a discovery failure would
	// send whoever reads the line to look at somebody else's server.
	state, _, err := auth.NewToken()
	if err != nil {
		s.googleRefuse(w, r, reasonInternal, err)
		return
	}
	nonce, _, err := auth.NewToken()
	if err != nil {
		s.googleRefuse(w, r, reasonInternal, err)
		return
	}
	verifier := oidcauth.NewVerifier()

	url, err := s.Google.AuthCodeURL(r.Context(), state, nonce, verifier)
	if err != nil {
		s.googleRefuse(w, r, reasonDiscovery, err)
		return
	}

	s.setOAuthCookie(w, oauthFlow{State: state, Nonce: nonce, Verifier: verifier})
	redirect(w, url)
}

// --------------------------------------------------------------- callback ----

// authGoogleCallback finishes the sign-in.
//
// It validates in one order and stops at the first failure: the provider's own
// error, the cookie, the state, the code, the exchange, the ID token, the
// nonce, the claims. Nothing touches the database until all of it has passed.
//
// Every refusal answers identically -- 302, Location: /login?error=google, an
// empty body, the flow cookie cleared -- with two named exceptions: the user
// pressing Cancel, which is not a failure and lands on a plain /login; and
// closed signups, which get their own parameter because POST /auth/signup
// already says so publicly and a generic error there would strand somebody on
// a message no amount of retrying can clear.
func (s *Server) authGoogleCallback(w http.ResponseWriter, r *http.Request) {
	if s.Google == nil || !s.Cfg.UseGoogle() {
		s.googleRefuse(w, r, reasonNotConfigured, nil)
		return
	}

	q := r.URL.Query()

	if provErr := q.Get("error"); provErr != "" {
		if provErr == "access_denied" {
			// Cancel. Nothing went wrong, so nothing is reported: the person is
			// simply back where they started.
			s.clearOAuthCookie(w)
			redirect(w, loginPath)
			return
		}
		// Only the codes the specifications define are logged verbatim. The
		// parameter is on a URL, and a URL is not something the provider is the
		// only one who can write.
		s.googleRefuse(w, r, reasonProviderError, errors.New(oauthErrorCode(provErr)))
		return
	}

	flow, ok := s.readOAuthCookie(r)
	if !ok {
		// Also what a replayed callback gets: the cookie was cleared on the
		// first outcome, whatever that outcome was.
		s.googleRefuse(w, r, reasonNoCookie, nil)
		return
	}
	// Constant time: the state is a secret this request is being asked to
	// prove it knows.
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(flow.State)) != 1 {
		s.googleRefuse(w, r, reasonBadState, nil)
		return
	}
	code := q.Get("code")
	if code == "" {
		s.googleRefuse(w, r, reasonNoCode, nil)
		return
	}

	claims, err := s.Google.Exchange(r.Context(), code, flow.Verifier, flow.Nonce)
	if err != nil {
		s.googleRefuse(w, r, reasonFor(err), err)
		return
	}

	email, err := canonicalEmail(claims.Email)
	if err != nil {
		s.googleRefuse(w, r, reasonBadEmail, err)
		return
	}

	acct, created, err := auth.SignInWithIdentity(r.Context(), s.DB,
		auth.ProviderGoogle, claims.Subject, email, googleDisplayName(claims.Name, email), s.signupsOpen())
	switch {
	case errors.Is(err, auth.ErrSignupsClosed):
		LoggerFrom(r.Context()).Warn("google sign-in refused", "reason", reasonSignupsClosed)
		s.clearOAuthCookie(w)
		redirect(w, loginGoogleClosed)
		return
	case errors.Is(err, auth.ErrIdentityAlreadyLinked):
		// A second provider account offered for an account that already has
		// one. The answer is the generic redirect like every other refusal, but
		// the log says what it actually was rather than calling a permanent
		// conflict a race.
		s.googleRefuse(w, r, reasonAlreadyLinked, nil)
		return
	case err != nil:
		s.googleRefuse(w, r, reasonLinkFailed, err)
		return
	}

	raw, sess, err := auth.CreateSession(r.Context(), s.DB, acct.ID, ClientIP(r), r.UserAgent())
	if err != nil {
		s.googleRefuse(w, r, reasonLinkFailed, err)
		return
	}

	// Byte for byte what a password login mints: same cookie, same
	// auth_sessions row, same 30-day slide, same entry in the sessions list.
	s.clearOAuthCookie(w)
	s.setSessionCookie(w, raw)
	LoggerFrom(r.Context()).Info("google sign-in",
		"user_id", acct.ID, "session_id", sess.ID, "provider", auth.ProviderGoogle, "created", created)
	redirect(w, afterSignIn)
}

// reasonFor maps an oidcauth failure to its constant log reason. The caller's
// answer is the same for all of them; only the log line differs.
func reasonFor(err error) string {
	switch {
	case errors.Is(err, oidcauth.ErrDiscovery):
		return reasonDiscovery
	case errors.Is(err, oidcauth.ErrExchange):
		return reasonExchangeFailed
	case errors.Is(err, oidcauth.ErrNonce):
		return reasonBadNonce
	case errors.Is(err, oidcauth.ErrEmailUnverified):
		return reasonEmailUnverif
	default:
		return reasonVerifyFailed
	}
}

// googleDisplayName is the name claim cleaned, falling back to the address's
// local part when the provider sent none or it cleaned away to nothing.
//
// A long name is cut to the limit rather than dropped. cleanDisplayName refuses
// anything over maxDisplayName, and the fallback for a refusal is the local
// part -- so without the cut, somebody whose provider profile carries a long
// name would be called by their email address instead of the first hundred
// characters of the name they chose. Runes, not bytes: half a character is not
// a name.
func googleDisplayName(name, email string) string {
	if runes := []rune(name); len(runes) > maxDisplayName {
		name = string(runes[:maxDisplayName])
	}
	if cleaned, err := cleanDisplayName(name); err == nil {
		return cleaned
	}
	local := email
	if at := strings.LastIndex(email, "@"); at > 0 {
		local = email[:at]
	}
	if cleaned, err := cleanDisplayName(local); err == nil {
		return cleaned
	}
	return "Drive user"
}

// googleRefuse is the one refusal: a logged reason, the flow cookie cleared,
// and a redirect that says nothing about which check failed.
func (s *Server) googleRefuse(w http.ResponseWriter, r *http.Request, reason string, err error) {
	l := LoggerFrom(r.Context())
	// The error is wrapped by the packages that produced it and carries no
	// code, state, nonce, verifier or token -- those never leave this file's
	// local variables.
	if err != nil {
		l.Warn("google sign-in refused", "reason", reason, "error", err)
	} else {
		l.Warn("google sign-in refused", "reason", reason)
	}
	s.clearOAuthCookie(w)
	redirect(w, loginGoogleError)
}

// rateLimitBrowser is RateLimitAuth's sibling for the two browser navigations:
// the same numbers, its own bucket, refused with a redirect instead of the JSON
// envelope.
//
// Its own bucket because these two are the only routes on the auth surface a
// stranger can make a browser visit without the browser's owner doing anything:
// a link, an <img src>, a crawler following /login. Sharing the login bucket
// would let any of those spend an address's whole sign-in allowance, and the
// person at that address -- an office, a NAT, a phone network -- would find the
// password form refusing them for something they never did.
//
// The nil guard is not decoration -- bare &Server{} literals exist in this
// package's tests, and clientip.go carries the same guard for the same reason.
func (s *Server) rateLimitBrowser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r)
		if s.BrowserRate != nil && !s.BrowserRate.allow(ip) {
			LoggerFrom(r.Context()).Warn("google sign-in refused",
				"reason", reasonRateLimited, "client_ip", ip, "path", r.URL.Path)
			s.clearOAuthCookie(w)
			redirect(w, loginGoogleError)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ------------------------------------------------------------- identities ----

// authListIdentities lists what the account is linked to. One page, like the
// sessions list: a person has at most one of these.
func (s *Server) authListIdentities(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())

	identities, err := auth.ListIdentities(r.Context(), s.DB, u.ID)
	if err != nil {
		s.authFailed(w, r, "listing the identities", err)
		return
	}

	items := make([]authIdentityDTO, 0, len(identities))
	for _, i := range identities {
		items = append(items, authIdentityDTO{
			ID:          i.ID,
			Provider:    i.Provider,
			EmailAtLink: i.EmailAtLink,
			CreatedAt:   i.CreatedAt,
			LastLoginAt: i.LastLoginAt,
		})
	}
	WriteJSON(w, http.StatusOK, NewList(items, ""))
}

// authDeleteIdentity unlinks one, unless it is the only way in.
//
// An id that belongs to somebody else is a 404, exactly as an id that does not
// exist: the two must not be distinguishable.
func (s *Server) authDeleteIdentity(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such linked account")
		return
	}

	deleted, err := auth.DeleteIdentity(r.Context(), s.DB, u.ID, id)
	if errors.Is(err, auth.ErrLastSignInMethod) {
		WriteErr(w, r, http.StatusConflict, CodeUnsupported,
			"that is the only way you can sign in — set a password first")
		return
	}
	if err != nil {
		s.authFailed(w, r, "unlinking the identity", err)
		return
	}
	if !deleted {
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such linked account")
		return
	}

	LoggerFrom(r.Context()).Info("identity unlinked", "user_id", u.ID, "identity_id", id)
	w.WriteHeader(http.StatusNoContent)
}

// ----------------------------------------------------------------- cookies ---

func (s *Server) setOAuthCookie(w http.ResponseWriter, f oauthFlow) {
	raw, err := json.Marshal(f)
	if err != nil {
		// Three strings cannot fail to encode; if they somehow did, the flow
		// would be unfinishable anyway and the callback's missing-cookie branch
		// is the honest outcome.
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     OAuthCookie,
		Value:    base64.RawURLEncoding.EncodeToString(raw),
		Path:     oauthCookiePath,
		MaxAge:   int(oauthFlowTTL.Seconds()),
		HttpOnly: true,
		// Lax is required and sufficient: the return from the provider is a
		// top-level cross-site GET navigation, which Lax permits and Strict
		// would drop -- taking every sign-in with it.
		SameSite: http.SameSiteLaxMode,
		// The __Host- prefix stays off for the same reason the session cookie
		// documents: Chrome rejects it on http://localhost, and local
		// development is http.
		Secure: s.secureCookies(),
	})
}

func (s *Server) clearOAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     OAuthCookie,
		Value:    "",
		Path:     oauthCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookies(),
	})
}

func (s *Server) readOAuthCookie(r *http.Request) (oauthFlow, bool) {
	c, err := r.Cookie(OAuthCookie)
	if err != nil || c.Value == "" {
		return oauthFlow{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return oauthFlow{}, false
	}
	var f oauthFlow
	if err := json.Unmarshal(raw, &f); err != nil {
		return oauthFlow{}, false
	}
	if f.State == "" || f.Nonce == "" || f.Verifier == "" {
		return oauthFlow{}, false
	}
	return f, true
}

// redirect writes a 302 with no body.
//
// No body, because the callback's response would otherwise carry the URL into a
// Referer; and the header is set directly rather than through http.Redirect so
// that what is asserted in a test is exactly what is sent.
func redirect(w http.ResponseWriter, to string) {
	w.Header().Set("Location", to)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
}
