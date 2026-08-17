package api

// Phase 1 auth agent owns this file: signup, email verification, login, logout
// and /me. Password reset and session management are Phase 5 if time remains
// (PLAN §Build order).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
)

// Input bounds. PLAN fixes none of these; they are the smallest limits that
// keep a hostile body from reaching Argon2 or a mail header.
const (
	maxEmailLen    = 254 // RFC 5321's maximum path length
	minPasswordLen = 8
	maxPasswordLen = 128
	maxDisplayName = 100 // runes
)

// verifySubject and verifyPathPrefix are fixed verbatim by PLAN §Mail
// construction. The SPA route /verify posts the token back to
// POST /api/auth/verify-email.
const (
	verifySubject   = "Verify your Drive account"
	verifyPathQuery = "/verify?token="
)

func (s *Server) mountAuth(r chi.Router) {
	r.Route("/auth", func(r chi.Router) {
		// The per-IP bucket covers the whole auth surface, /me included: it is
		// the only place an unauthenticated caller can reach Argon2 or the mail
		// sender, and a limiter with a hole in it is a limiter with a hole in it.
		r.Use(s.RateLimitAuth)

		r.Post("/signup", s.authSignup)
		r.Post("/verify-email", s.authVerifyEmail)
		r.Post("/login", s.authLogin)
		r.Post("/logout", s.authLogout)
		// Phase 6: /auth/me is the one /auth route a bearer token may call,
		// and answers with the token's {id, name, scopes, expires_at} as well.
		r.With(s.RequireAuth).Get("/me", s.authMe)
	})
}

// ------------------------------------------------------------------ shapes --

// authStatus is the body of every auth response that carries no data. Signup
// and verify-email both use it, and signup's is deliberately identical whether
// or not an account was created.
type authStatus struct {
	Status string `json:"status"`
}

var authOK = authStatus{Status: "ok"}

// authMeResponse is what GET /auth/me returns -- and what login returns, so a
// client never needs a second round trip to learn who it just became.
type authMeResponse struct {
	ID              uuid.UUID  `json:"id"`
	Email           string     `json:"email"`
	DisplayName     string     `json:"display_name"`
	RootID          uuid.UUID  `json:"root_id"`
	EmailVerifiedAt *time.Time `json:"email_verified_at"`
}

type authSignupRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type authLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type authTokenRequest struct {
	Token string `json:"token"`
}

// ---------------------------------------------------------------- handlers --

// authSignup creates an account, its root folder and a verification token, and
// mails the link.
//
// The response is byte-identical whether the address was free or already taken,
// and an address that is taken generates no mail at all -- signup must not be
// an oracle for "does this person have an account here".
func (s *Server) authSignup(w http.ResponseWriter, r *http.Request) {
	// Before the body is even read: a closed deployment does no work at all for
	// a signup, which is also what makes "closed" a usable emergency brake.
	if !s.signupsOpen() {
		WriteErr(w, r, http.StatusForbidden, CodeUnsupported,
			"Drive is not accepting new accounts right now")
		return
	}

	var req authSignupRequest
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "expected {email, password, display_name}")
		return
	}

	email, err := canonicalEmail(req.Email)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}
	if err := checkPassword(req.Password); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}
	displayName, err := cleanDisplayName(req.DisplayName)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}

	// Hash before the insert, unconditionally: the work happens for a taken
	// address too, so response time says nothing either.
	//
	// Through the limiter, so a burst of signups cannot put more Argon2 work in
	// flight than the process has memory for. Over the bound the answer is 429,
	// before any database work.
	hash, err := s.Argon2.Hash(req.Password)
	if errors.Is(err, auth.ErrBusy) {
		s.authBusy(w, r)
		return
	}
	if err != nil {
		s.authFailed(w, r, "hashing the password", err)
		return
	}

	ctx := r.Context()
	acct, created, err := auth.CreateUser(ctx, s.DB, email, hash, displayName)
	if err != nil {
		s.authFailed(w, r, "creating the account", err)
		return
	}
	if created {
		// Off the request goroutine, exactly as PLAN legislates for the OTP
		// path. Hashing alone does not buy timing parity: the free-address
		// branch also runs a budget SELECT, a token INSERT, a throttle upsert
		// and a blocking SMTP round trip, and the taken-address branch runs
		// none of it. Keeping every conditional statement off the response path
		// is what makes the identical 200 bodies actually indistinguishable.
		// WithoutCancel: the send must survive the request completing.
		go s.sendVerificationMail(context.WithoutCancel(ctx), LoggerFrom(ctx), acct)
	}

	WriteJSON(w, http.StatusOK, authOK)
}

// sendVerificationMail mints a verification token and mails the link, subject
// to the per-address email_send budget. Nothing here can fail the request: the
// account exists either way, and a caller must not learn from the response
// whether a message went out.
func (s *Server) sendVerificationMail(ctx context.Context, log *slog.Logger, acct *auth.Account) {
	allowed, err := auth.Allowed(ctx, s.DB, auth.ScopeEmailSend, acct.Email, auth.EmailSendLimit, auth.EmailSendWindow)
	if err != nil {
		log.Error("checking the mail budget", "error", err)
		return
	}
	if !allowed {
		log.Warn("verification mail suppressed: address is over its send budget", "user_id", acct.ID)
		return
	}

	// The service-wide daily budget, charged BEFORE the decision to send.
	//
	// Check-then-send races straight past the cap: two concurrent signups both
	// read 79, both conclude there is room, and both send. Charging first makes
	// the increment the serialization point -- the upsert takes the row lock --
	// and the post-increment count is then the honest answer to "was there room
	// for me". A suppressed attempt still spends budget, which is the correct
	// direction to be wrong in when the thing being protected is a hard vendor
	// quota that takes verification mail down for everyone when it runs out.
	if dailyCap := s.emailDailyCap(); dailyCap > 0 {
		spent, err := auth.Bump(ctx, s.DB, auth.ScopeEmailSendGlobal, auth.GlobalKey, auth.EmailSendGlobalWindow)
		if err != nil {
			log.Error("charging the service-wide mail budget", "error", err)
			return
		}
		if spent > dailyCap {
			log.Warn("verification mail suppressed: the service-wide daily send budget is spent",
				"user_id", acct.ID, "spent", spent, "cap", dailyCap)
			return
		}
	}

	token, err := auth.CreateEmailToken(ctx, s.DB, acct.ID, auth.PurposeVerify)
	if err != nil {
		log.Error("creating the verification token", "error", err, "user_id", acct.ID)
		return
	}
	if _, err := auth.Bump(ctx, s.DB, auth.ScopeEmailSend, acct.Email, auth.EmailSendWindow); err != nil {
		log.Error("charging the mail budget", "error", err)
		return
	}

	if s.Mail == nil {
		log.Error("no mail sender configured; the verification link cannot be delivered", "user_id", acct.ID)
		return
	}
	body := fmt.Sprintf(
		"Welcome to Drive.\n\nConfirm this address to finish setting up your account:\n\n%s\n\nIf you did not create a Drive account, ignore this email.\n",
		s.baseURL()+verifyPathQuery+token,
	)
	if err := s.Mail.Send(ctx, acct.Email, verifySubject, body); err != nil {
		log.Error("sending the verification mail", "error", err, "user_id", acct.ID)
	}
}

// authVerifyEmail redeems the link from the verification mail. Every reason a
// token cannot be redeemed -- unknown, expired, already clicked -- answers the
// same, so the endpoint is not a token oracle.
func (s *Server) authVerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req authTokenRequest
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "expected {token}")
		return
	}

	const badToken = "this verification link is invalid or has expired"
	if strings.TrimSpace(req.Token) == "" {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, badToken)
		return
	}

	userID, err := auth.ConsumeEmailToken(r.Context(), s.DB, req.Token, auth.PurposeVerify)
	if errors.Is(err, auth.ErrInvalidToken) {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, badToken)
		return
	}
	if err != nil {
		s.authFailed(w, r, "verifying the address", err)
		return
	}

	LoggerFrom(r.Context()).Info("email verified", "user_id", userID)
	WriteJSON(w, http.StatusOK, authOK)
}

// authLogin checks the password and mints a session.
//
// Order matters. The durable lockout is consulted first, so a spent budget
// refuses even the right password. The password is then checked before the
// verified-address check, so the one non-generic failure message
// ("verify your email first") is only ever shown to somebody who already knows
// the password -- it can never be used to enumerate accounts.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	var req authLoginRequest
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "expected {email, password}")
		return
	}

	const generic = "that email and password combination is not right"
	email, err := canonicalEmail(req.Email)
	if err != nil {
		WriteErr(w, r, http.StatusUnauthorized, CodeUnauthorized, generic)
		return
	}

	ctx := r.Context()
	allowed, err := auth.Allowed(ctx, s.DB, auth.ScopeLogin, email, auth.LoginFailLimit, auth.LoginFailWindow)
	if err != nil {
		s.authFailed(w, r, "checking the login budget", err)
		return
	}
	if !allowed {
		WriteErr(w, r, http.StatusTooManyRequests, CodeRateLimited,
			"too many sign-in attempts for this address. Try again in a few minutes.")
		return
	}

	acct, err := auth.FindUserByEmail(ctx, s.DB, email)
	if err != nil {
		s.authFailed(w, r, "looking the account up", err)
		return
	}

	// An unknown address still pays for a full Argon2 verification, so the
	// response time does not separate "no such user" from "wrong password".
	stored := decoyHash()
	if acct != nil {
		stored = acct.PasswordHash
	}
	ok, err := s.Argon2.Verify(stored, req.Password)
	if errors.Is(err, auth.ErrBusy) {
		s.authBusy(w, r)
		return
	}
	if err != nil {
		s.authFailed(w, r, "verifying the password", err)
		return
	}

	if acct == nil || !ok {
		if _, err := auth.Bump(ctx, s.DB, auth.ScopeLogin, email, auth.LoginFailWindow); err != nil {
			LoggerFrom(ctx).Error("charging the login budget", "error", err)
		}
		WriteErr(w, r, http.StatusUnauthorized, CodeUnauthorized, generic)
		return
	}

	// The credentials were right, so this is not a failed attempt and costs
	// nothing against the lockout budget.
	if !acct.Verified() {
		WriteErr(w, r, http.StatusUnauthorized, CodeUnauthorized,
			"verify your email first: check your inbox for the link we sent when you signed up")
		return
	}

	raw, sess, err := auth.CreateSession(ctx, s.DB, acct.ID, ClientIP(r), r.UserAgent())
	if err != nil {
		s.authFailed(w, r, "creating the session", err)
		return
	}
	s.setSessionCookie(w, raw)

	LoggerFrom(ctx).Info("login", "user_id", acct.ID, "session_id", sess.ID)
	WriteJSON(w, http.StatusOK, authMeResponse{
		ID:              acct.ID,
		Email:           acct.Email,
		DisplayName:     acct.DisplayName,
		RootID:          acct.RootID,
		EmailVerifiedAt: acct.EmailVerifiedAt,
	})
}

// authLogout revokes the session server-side and clears the cookie. It answers
// 204 whether or not the cookie identified anything: a client that has lost
// track of its session still ends up logged out.
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(SessionCookie); err == nil && cookie.Value != "" {
		if err := auth.DeleteSession(r.Context(), s.DB, cookie.Value); err != nil {
			s.authFailed(w, r, "revoking the session", err)
			return
		}
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// authMe returns the signed-in account and the id of its root folder, which is
// where the file browser starts.
func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())
	// Phase 6: when the caller authenticated with a bearer token, this response
	// additionally carries token: {id, name, scopes, expires_at}.
	WriteJSON(w, http.StatusOK, authMeResponse{
		ID:              u.ID,
		Email:           u.Email,
		DisplayName:     u.DisplayName,
		RootID:          u.RootID,
		EmailVerifiedAt: u.EmailVerifiedAt,
	})
}

// ----------------------------------------------------------------- cookies --

// setSessionCookie writes the drive_session cookie.
//
// Secure is set only for an https base URL: Safari drops Secure cookies on
// plain-http localhost, and local development is http. The __Host-/__Secure-
// prefixes are deliberately not used for the same reason -- Chrome rejects them
// on http://localhost.
func (s *Server) setSessionCookie(w http.ResponseWriter, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    raw,
		Path:     "/",
		MaxAge:   int(SessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookies(),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookies(),
	})
}

func (s *Server) secureCookies() bool {
	return strings.HasPrefix(strings.ToLower(s.baseURL()), "https://")
}

// signupsOpen reports whether POST /auth/signup creates accounts.
//
// Only "open" does. "invite" is accepted as a configuration value and behaves
// exactly like "closed" -- there is no invite system yet, so an invite-only
// deployment is one nobody can join, and answering anything friendlier than
// "closed" would be a claim the server cannot back up.
func (s *Server) signupsOpen() bool {
	return s.Cfg == nil || s.Cfg.SignupMode == "" || s.Cfg.SignupMode == config.SignupOpen
}

// emailDailyCap is the service-wide daily send budget, or 0 for no budget --
// which is what every local and test run gets, because Mailpit has no quota to
// protect.
func (s *Server) emailDailyCap() int {
	if s.Cfg == nil {
		return 0
	}
	return s.Cfg.EmailDailyCap
}

func (s *Server) baseURL() string {
	if s.Cfg == nil {
		return ""
	}
	return strings.TrimSuffix(s.Cfg.BaseURL, "/")
}

// --------------------------------------------------------------- validation --

// canonicalEmail validates an address the way PLAN §Mail construction requires
// -- net/mail.ParseAddress at the API boundary -- and returns the bare address,
// lower-cased. A display part is refused: "Someone <a@b.test>" is a header, not
// a login.
//
// The lower-casing is load-bearing, not cosmetic. users.email is citext, so
// User@x.test and user@x.test are the same account -- but throttle.key is plain
// text matched exactly, so without one canonical form every case permutation of
// an address would open a fresh login-lockout budget against the same password.
func canonicalEmail(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("email: required")
	}
	if len(trimmed) > maxEmailLen {
		return "", fmt.Errorf("email: longer than %d characters", maxEmailLen)
	}
	addr, err := mail.ParseAddress(trimmed)
	if err != nil {
		return "", errors.New("email: not a valid address")
	}
	if addr.Name != "" || addr.Address != trimmed {
		return "", errors.New("email: give the bare address, with no display name")
	}
	return strings.ToLower(addr.Address), nil
}

func checkPassword(p string) error {
	switch {
	case len(p) < minPasswordLen:
		return fmt.Errorf("password: must be at least %d characters", minPasswordLen)
	case len(p) > maxPasswordLen:
		return fmt.Errorf("password: must be at most %d characters", maxPasswordLen)
	}
	return nil
}

// cleanDisplayName trims the name and strips the control characters that could
// otherwise ride into a mail header -- the name appears in Phase 3's OTP mails.
func cleanDisplayName(raw string) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r == 0x7F || unicode.IsControl(r) {
			return -1
		}
		return r
	}, raw)
	cleaned = strings.TrimSpace(cleaned)

	if cleaned == "" {
		return "", errors.New("display_name: required")
	}
	if len([]rune(cleaned)) > maxDisplayName {
		return "", fmt.Errorf("display_name: longer than %d characters", maxDisplayName)
	}
	return cleaned, nil
}

// ------------------------------------------------------------------ helpers --

// decoyHash is a real Argon2id hash of a random password, verified against when
// no account matches so that an unknown address costs the same as a known one.
var decoyHash = sync.OnceValue(func() string {
	raw, _, err := auth.NewToken()
	if err != nil {
		return ""
	}
	h, err := auth.HashPassword(raw)
	if err != nil {
		return ""
	}
	return h
})

// authBusy refuses a request that could not get an Argon2 slot. It is a 429 and
// not a 503 because it is a per-caller ceiling, not an outage: the right move is
// to try again shortly, and the message says so.
func (s *Server) authBusy(w http.ResponseWriter, r *http.Request) {
	LoggerFrom(r.Context()).Warn("password work refused: every Argon2 slot is in use",
		"client_ip", ClientIP(r), "path", r.URL.Path)
	WriteErr(w, r, http.StatusTooManyRequests, CodeRateLimited,
		"we are busy right now. Try again in a moment.")
}

// authFailed logs the real cause and tells the client nothing about it.
func (s *Server) authFailed(w http.ResponseWriter, r *http.Request, what string, err error) {
	LoggerFrom(r.Context()).Error("auth: "+what, "error", err)
	WriteErr(w, r, http.StatusInternalServerError, CodeInternal, "something went wrong on our side")
}
