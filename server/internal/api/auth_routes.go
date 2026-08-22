package api

// The auth surface: signup, email verification, login, logout, the account
// itself (profile, password, sessions), password reset and resend-verification.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
)

// Input bounds. Nothing external dictates these numbers; they are the smallest
// limits that keep a hostile body from reaching Argon2 or a mail header.
const (
	maxEmailLen    = 254 // RFC 5321's maximum path length
	minPasswordLen = 8
	maxPasswordLen = 128
	maxDisplayName = 100 // runes
)

// The verification mail's subject and link path are fixed verbatim: the SPA
// route /verify reads the token out of the query and posts it back to
// POST /api/auth/verify-email, and the integration suite matches the subject
// exactly. Changing either breaks a loop that spans mail, SPA and API.
const (
	verifySubject   = "Verify your Drive account"
	verifyPathQuery = "/verify?token="
)

// The reset mail's subject and link path, fixed the same way: the SPA route
// /reset reads the token out of the query and posts it to
// POST /api/auth/password-reset/confirm.
const (
	resetSubject   = "Reset your Drive password"
	resetPathQuery = "/reset?token="
)

// maxUserAgent bounds what the session list echoes back. A User-Agent is
// attacker-controlled text of unbounded length; the list shows enough of it to
// recognise a browser and not a paragraph.
const maxUserAgent = 200 // runes

// mailSendTimeout bounds one detached mail goroutine end to end -- the lookup,
// the budgets, the token and the SMTP round trip. It is generous, because it is
// not a latency budget: nothing waits on it. It exists so that a peer which
// never answers cannot pin the goroutine forever.
const mailSendTimeout = 30 * time.Second

// mountAuth splits /api/auth in two.
//
// The per-IP bucket belongs in front of the unauthenticated half: that is where
// a stranger reaches Argon2 and the mail sender, and where the caller's address
// is the only identity there is. Behind RequireAuth there is a much better
// identity -- the session -- and the durable budgets are keyed by it, so the
// bucket buys nothing there and costs something real: a phone, an office and a
// laptop behind one NAT share an address, and /me is polled by every tab.
//
// POST /password is the exception. It reaches Argon2 twice with a caller-chosen
// password, so it keeps the bucket even though it is authenticated.
func (s *Server) mountAuth(r chi.Router) {
	r.Route("/auth", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(s.RateLimitAuth)

			r.Post("/signup", s.authSignup)
			r.Post("/verify-email", s.authVerifyEmail)
			r.Post("/login", s.authLogin)
			r.Post("/logout", s.authLogout)
			r.Post("/password-reset", s.authPasswordReset)
			r.Post("/password-reset/confirm", s.authPasswordResetConfirm)
			r.Post("/resend-verification", s.authResendVerification)
		})

		r.Group(func(r chi.Router) {
			r.Use(s.RequireAuth)

			// If bearer tokens ever ship, /auth/me is the one /auth route they
			// may call, and would answer with the token's own metadata too.
			r.Get("/me", s.authMe)
			r.Patch("/me", s.authUpdateMe)
			r.Get("/sessions", s.authListSessions)
			r.Delete("/sessions/{id}", s.authDeleteSession)
			r.Post("/logout-all", s.authLogoutAll)
			r.With(s.RateLimitAuth).Post("/password", s.authChangePassword)
		})
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

// authEmailRequest is the body of the two endpoints that mail an address the
// caller does not have to own: password-reset and resend-verification.
type authEmailRequest struct {
	Email string `json:"email"`
}

type authUpdateMeRequest struct {
	DisplayName string `json:"display_name"`
}

type authChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type authResetConfirmRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// authSessionDTO is one row of GET /auth/sessions.
type authSessionDTO struct {
	ID         uuid.UUID  `json:"id"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	IP         *string    `json:"ip"`
	UserAgent  *string    `json:"user_agent"`
	// Current marks the session this request arrived on, which the UI must not
	// offer to revoke as if it were somebody else's.
	Current bool `json:"current"`
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
		// Off the request goroutine, the same shape the share-OTP path uses.
		// Hashing alone does not buy timing parity: the free-address
		// branch also runs a budget SELECT, a token INSERT, a throttle upsert
		// and a blocking SMTP round trip, and the taken-address branch runs
		// none of it. Keeping every conditional statement off the response path
		// is what makes the identical 200 bodies actually indistinguishable.
		// WithoutCancel: the send must survive the request completing.
		go s.sendVerificationMail(context.WithoutCancel(ctx), LoggerFrom(ctx), acct)
	}

	WriteJSON(w, http.StatusOK, authOK)
}

// sendVerificationMail is signup's send. It keeps the plain email_send scope:
// the budget a person spends on their own address at signup is not the one a
// stranger can spend on it through resend-verification.
//
// It is detached from its request exactly as mailAccountLink is, and carries the
// same two guards for the same reasons: out here a panic has no request to fail
// and takes the process with it, and an SMTP peer that never answers has nothing
// else to end it.
func (s *Server) sendVerificationMail(ctx context.Context, log *slog.Logger, acct *auth.Account) {
	defer func() {
		if p := recover(); p != nil {
			log.Error("the detached mail goroutine panicked",
				"panic", p, "purpose", auth.PurposeVerify, "stack", string(debug.Stack()))
		}
	}()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mailSendTimeout)
	defer cancel()

	s.sendAccountLink(ctx, log, acct, auth.PurposeVerify, auth.ScopeEmailSend)
}

// sendAccountLink mints a link token and mails it, subject to the per-address
// budget in scope and the service-wide daily cap. Nothing here can fail a
// request: every caller runs it off the request goroutine, because a caller
// must not learn from the response -- or from its latency -- whether a message
// went out.
func (s *Server) sendAccountLink(ctx context.Context, log *slog.Logger, acct *auth.Account, purpose, scope string) {
	log = log.With("user_id", acct.ID, "purpose", purpose)

	allowed, err := auth.Allowed(ctx, s.DB, scope, acct.Email, auth.EmailSendLimit, auth.EmailSendWindow)
	if err != nil {
		log.Error("checking the mail budget", "error", err)
		return
	}
	if !allowed {
		log.Warn("mail suppressed: address is over its send budget", "scope", scope)
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
			log.Warn("mail suppressed: the service-wide daily send budget is spent",
				"spent", spent, "cap", dailyCap)
			return
		}
	}

	token, err := auth.CreateEmailToken(ctx, s.DB, acct.ID, purpose)
	if err != nil {
		log.Error("creating the link token", "error", err)
		return
	}
	if _, err := auth.Bump(ctx, s.DB, scope, acct.Email, auth.EmailSendWindow); err != nil {
		log.Error("charging the mail budget", "error", err)
		return
	}

	if s.Mail == nil {
		log.Error("no mail sender configured; the link cannot be delivered")
		return
	}
	subject, body := accountMail(purpose, s.baseURL(), token)
	if err := s.Mail.Send(ctx, acct.Email, subject, body); err != nil {
		log.Error("sending the mail", "error", err)
	}
}

// accountMail is the message for one link purpose. Both subjects and both link
// paths are fixed contracts with the SPA and the integration suite.
func accountMail(purpose, baseURL, token string) (subject, body string) {
	if purpose == auth.PurposeReset {
		return resetSubject, fmt.Sprintf(
			"Somebody asked to reset the password on your Drive account.\n\nChoose a new one here -- the link works once, and only for the next hour:\n\n%s\n\nIf that was not you, ignore this email. Your password has not changed.\n",
			baseURL+resetPathQuery+token,
		)
	}
	return verifySubject, fmt.Sprintf(
		"Welcome to Drive.\n\nConfirm this address to finish setting up your account:\n\n%s\n\nIf you did not create a Drive account, ignore this email.\n",
		baseURL+verifyPathQuery+token,
	)
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
		// Its own code, not the generic one: this is the single refusal the
		// client is allowed to act on -- it offers to resend the link -- and a
		// client that had to match the English would break the moment the
		// wording changed.
		WriteErr(w, r, http.StatusUnauthorized, CodeEmailUnverified,
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
	// If bearer tokens ever ship, a token-authenticated caller gets its own
	// {id, name, scopes, expires_at} alongside this.
	WriteJSON(w, http.StatusOK, meDTO(u))
}

// authUpdateMe renames the account. It is the whole of the profile form: email
// is the login and is not editable here.
func (s *Server) authUpdateMe(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())

	var req authUpdateMeRequest
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "expected {display_name}")
		return
	}
	name, err := cleanDisplayName(req.DisplayName)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}

	if err := auth.SetDisplayName(r.Context(), s.DB, u.ID, name); err != nil {
		s.authFailed(w, r, "renaming the account", err)
		return
	}

	// The updated account, so the caller's cached copy -- and the initials in
	// its avatar -- refresh without a second round trip.
	updated := *u
	updated.DisplayName = name
	WriteJSON(w, http.StatusOK, meDTO(&updated))
}

// authChangePassword replaces the password of a signed-in caller who can prove
// the current one.
//
// The failure budget is keyed by user id under its own scope. Charging the
// login budget instead -- which is keyed by email -- would mean ten mistyped
// current passwords locked the account out of signing in, and would hand anyone
// who stole a session an easy way to do it deliberately.
func (s *Server) authChangePassword(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())
	ctx := r.Context()

	var req authChangePasswordRequest
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "expected {current_password, new_password}")
		return
	}
	if err := checkPassword(req.NewPassword); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}

	key := u.ID.String()
	allowed, err := auth.Allowed(ctx, s.DB, auth.ScopePasswordChange, key,
		auth.PasswordChangeFailLimit, auth.PasswordChangeFailWindow)
	if err != nil {
		s.authFailed(w, r, "checking the password-change budget", err)
		return
	}
	if !allowed {
		WriteErr(w, r, http.StatusTooManyRequests, CodeRateLimited,
			"too many attempts. Try again in a few minutes.")
		return
	}

	// The stored hash is deliberately not on the request-scoped user, so it is
	// read here rather than carried through every request in the process.
	acct, err := auth.FindUserByID(ctx, s.DB, u.ID)
	if err != nil {
		s.authFailed(w, r, "looking the account up", err)
		return
	}
	if acct == nil {
		// The session outlived its user. Nothing to change, and nothing the
		// caller can do about it.
		WriteErr(w, r, http.StatusUnauthorized, CodeUnauthorized, "sign in to continue")
		return
	}

	ok, err := s.Argon2.Verify(acct.PasswordHash, req.CurrentPassword)
	if errors.Is(err, auth.ErrBusy) {
		s.authBusy(w, r)
		return
	}
	if err != nil {
		s.authFailed(w, r, "verifying the password", err)
		return
	}
	if !ok {
		if _, err := auth.Bump(ctx, s.DB, auth.ScopePasswordChange, key, auth.PasswordChangeFailWindow); err != nil {
			LoggerFrom(ctx).Error("charging the password-change budget", "error", err)
		}
		WriteErr(w, r, http.StatusUnauthorized, CodeUnauthorized, "that password is not right")
		return
	}

	hash, err := s.Argon2.Hash(req.NewPassword)
	if errors.Is(err, auth.ErrBusy) {
		s.authBusy(w, r)
		return
	}
	if err != nil {
		s.authFailed(w, r, "hashing the password", err)
		return
	}
	// One transaction: the new hash, the revoke of every OTHER session (this one
	// survives -- signing somebody out of the form they just submitted is a bug,
	// not a security control) and the spending of every live reset link. A
	// failure here is a 500 with nothing changed, because the alternative is a
	// 204 that tells the user they are safe while the sessions or the links they
	// were worried about still work.
	if err := auth.ChangePassword(ctx, s.DB, u.ID, hash, u.SessionID); err != nil {
		s.authFailed(w, r, "changing the password", err)
		return
	}

	LoggerFrom(ctx).Info("password changed", "user_id", u.ID)
	w.WriteHeader(http.StatusNoContent)
}

// authListSessions lists where the account is signed in. It is one page: a
// person has a handful of sessions, so next_cursor is always null and the
// envelope stays the same as every other list.
func (s *Server) authListSessions(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())

	sessions, err := auth.ListSessions(r.Context(), s.DB, u.ID)
	if err != nil {
		s.authFailed(w, r, "listing the sessions", err)
		return
	}

	items := make([]authSessionDTO, 0, len(sessions))
	for _, sess := range sessions {
		items = append(items, authSessionDTO{
			ID:         sess.ID,
			CreatedAt:  sess.CreatedAt,
			LastSeenAt: sess.LastSeenAt,
			IP:         sess.IP,
			UserAgent:  truncateRunes(sess.UserAgent, maxUserAgent),
			Current:    sess.ID == u.SessionID,
		})
	}
	WriteJSON(w, http.StatusOK, NewList(items, ""))
}

// authDeleteSession revokes one of the caller's own sessions. An id that is not
// theirs is 404, exactly as an id that does not exist: the two must not be
// distinguishable.
func (s *Server) authDeleteSession(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such session")
		return
	}

	deleted, err := auth.DeleteSessionByID(r.Context(), s.DB, u.ID, id)
	if err != nil {
		s.authFailed(w, r, "revoking the session", err)
		return
	}
	if !deleted {
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such session")
		return
	}

	// Revoking the session you are on is a signout. The row is already gone, so
	// the cookie is dead either way; clearing it keeps the browser from sending
	// a credential that can never work again.
	if id == u.SessionID {
		s.clearSessionCookie(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

// authLogoutAll revokes every session the account has, this one included.
func (s *Server) authLogoutAll(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())

	if err := auth.DeleteUserSessions(r.Context(), s.DB, u.ID); err != nil {
		s.authFailed(w, r, "revoking the sessions", err)
		return
	}
	s.clearSessionCookie(w)
	LoggerFrom(r.Context()).Info("signed out everywhere", "user_id", u.ID)
	w.WriteHeader(http.StatusNoContent)
}

// authPasswordReset mails a reset link, and answers 200 for every syntactically
// valid address whether or not it has an account.
func (s *Server) authPasswordReset(w http.ResponseWriter, r *http.Request) {
	s.requestAccountMail(w, r, auth.PurposeReset)
}

// authResendVerification mails the verification link again. Same shape as the
// reset request, and the same silence about who exists.
func (s *Server) authResendVerification(w http.ResponseWriter, r *http.Request) {
	s.requestAccountMail(w, r, auth.PurposeVerify)
}

// requestAccountMail is the request half of both mail-me-a-link endpoints.
//
// Everything that depends on whether the address has an account happens on a
// goroutine, and nothing on the response path does. That is the whole design:
// the lookup, the budget checks, the token insert and a blocking SMTP round
// trip are all work that only the "account exists" branch would do, so running
// any of it inline would make the response's latency the oracle the identical
// 200 bodies exist to prevent.
//
// What does run inline is the per-IP bucket, and it runs identically for both
// branches. Without it one address could spend anybody's per-recipient budget
// as fast as it could issue requests -- these are the two endpoints where the
// caller names somebody else's inbox.
func (s *Server) requestAccountMail(w http.ResponseWriter, r *http.Request, purpose string) {
	var req authEmailRequest
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "expected {email}")
		return
	}
	email, err := canonicalEmail(req.Email)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}

	if s.MailRate != nil && !s.MailRate.allow(ClientIP(r)) {
		LoggerFrom(r.Context()).Warn("mail request refused by the per-IP bucket",
			"client_ip", ClientIP(r), "path", r.URL.Path)
		WriteErr(w, r, http.StatusTooManyRequests, CodeRateLimited,
			"too many requests. Try again later.")
		return
	}

	ctx := r.Context()
	// WithoutCancel: the send must survive the request completing.
	go s.mailAccountLink(context.WithoutCancel(ctx), LoggerFrom(ctx), email, purpose)

	WriteJSON(w, http.StatusOK, authOK)
}

// mailAccountLink is the off-request half: look the address up, and send only
// if there is somebody to send to.
//
// It runs with no request left to fail, which is what the two guards at the top
// are about. A panic on a detached goroutine takes the process down with it and
// nothing above can recover it, and an SMTP peer that accepts a connection and
// then says nothing would otherwise hold this goroutine open for as long as the
// process lives -- there is no client timeout out here to end it.
func (s *Server) mailAccountLink(ctx context.Context, log *slog.Logger, email, purpose string) {
	defer func() {
		if p := recover(); p != nil {
			log.Error("the detached mail goroutine panicked",
				"panic", p, "purpose", purpose, "stack", string(debug.Stack()))
		}
	}()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mailSendTimeout)
	defer cancel()

	acct, err := auth.FindUserByEmail(ctx, s.DB, email)
	if err != nil {
		log.Error("looking the account up", "error", err, "purpose", purpose)
		return
	}
	if acct == nil {
		// No account: no token, no send, and -- deliberately -- no charge
		// against the service-wide daily cap either, so a sweep through
		// addresses that do not exist cannot spend the budget real users need.
		return
	}

	scope := auth.ScopeEmailSendReset
	if purpose == auth.PurposeVerify {
		if acct.Verified() {
			// Nothing to confirm. Resending would be a way to mail somebody
			// who already finished, on demand.
			return
		}
		scope = auth.ScopeEmailSendVerify
	}
	s.sendAccountLink(ctx, log, acct, purpose, scope)
}

// authPasswordResetConfirm redeems a reset link and sets the new password.
func (s *Server) authPasswordResetConfirm(w http.ResponseWriter, r *http.Request) {
	var req authResetConfirmRequest
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "expected {token, new_password}")
		return
	}

	const badToken = "this reset link is invalid or has expired"
	if strings.TrimSpace(req.Token) == "" {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, badToken)
		return
	}
	if err := checkPassword(req.NewPassword); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}

	// Hash BEFORE the token is consumed. A busy Argon2 limiter answers 429, and
	// a 429 after the burn would have spent the user's one link on a refusal
	// they are told to retry.
	hash, err := s.Argon2.Hash(req.NewPassword)
	if errors.Is(err, auth.ErrBusy) {
		s.authBusy(w, r)
		return
	}
	if err != nil {
		s.authFailed(w, r, "hashing the password", err)
		return
	}

	userID, err := auth.ResetPassword(r.Context(), s.DB, req.Token, hash)
	if errors.Is(err, auth.ErrInvalidToken) {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, badToken)
		return
	}
	if err != nil {
		s.authFailed(w, r, "resetting the password", err)
		return
	}

	// Every session went with the reset, this request's included if it had one.
	s.clearSessionCookie(w)
	LoggerFrom(r.Context()).Info("password reset", "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}

// meDTO is the /me shape, from either the request-scoped user or a copy of it.
func meDTO(u *User) authMeResponse {
	return authMeResponse{
		ID:              u.ID,
		Email:           u.Email,
		DisplayName:     u.DisplayName,
		RootID:          u.RootID,
		EmailVerifiedAt: u.EmailVerifiedAt,
	}
}

// truncateRunes caps a nullable string at n runes, cutting on a rune boundary
// so the result is still valid UTF-8.
func truncateRunes(s *string, n int) *string {
	if s == nil {
		return nil
	}
	runes := []rune(*s)
	if len(runes) <= n {
		return s
	}
	cut := string(runes[:n])
	return &cut
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

// canonicalEmail validates an address with net/mail.ParseAddress at the API
// boundary -- the boundary is where it belongs, so nothing downstream can carry
// a malformed address into a mail header -- and returns the bare address,
// lower-cased. A display part is refused: "Someone <a@b.test>" is a header, not
// a login.
//
// The lower-casing is load-bearing, not cosmetic. users.email is citext, so
// User@x.test and user@x.test are the same account -- but throttle.key is plain
// text matched exactly, so without one canonical form every case permutation of
// an address would open a fresh login-lockout budget against the same password.
//
// ParseAddress alone is not enough. It accepts "a@b" happily -- that is a legal
// address with a bare hostname for a domain -- so a typo'd domain used to reach
// the mailer, which then had nowhere to deliver, and the only sign anything was
// wrong was a verification link that never arrived. plausibleDomain closes that
// off with the same rule the sign-in and sign-up forms apply while the address
// is still being typed, so what the field flags is what the API refuses.
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
	lowered := strings.ToLower(addr.Address)
	at := strings.LastIndex(lowered, "@")
	if !plausibleDomain(lowered[at+1:]) {
		return "", errors.New("email: the part after the @ does not look like a mail domain")
	}
	return lowered, nil
}

// plausibleDomain reports whether host is shaped like a domain that could hold
// a mailbox: dot-separated labels, none of them empty, the last one two or more
// ASCII letters -- or an IDN TLD in its ASCII form, which idnTLD covers.
//
// Tightening this cannot lock anybody out of an existing account, because every
// address already stored got there through a real mailbox -- signup only
// completes after the verification link is clicked -- and a real mailbox has a
// dotted domain. The SPA applies the same rule while the address is still being
// typed, so the two never disagree about what is acceptable.
func plausibleDomain(host string) bool {
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" {
			return false
		}
	}
	last := labels[len(labels)-1]
	if idnTLD(last) {
		return true
	}
	if len(last) < 2 {
		return false
	}
	for _, r := range last {
		// Both cases, so this does not quietly depend on having been handed the
		// lower-cased form.
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// idnTLD reports whether label is an internationalised TLD in its ASCII form:
// the "xn--" prefix and at least one character more. It is the one real TLD
// family the letters-only rule above would refuse, because punycode carries
// digits and hyphens by construction -- xn--p1ai is .рф.
//
// Case-insensitive on both halves for the same reason plausibleDomain is: this
// must not quietly depend on having been handed the lower-cased form, and the
// SPA tests the address exactly as it was typed.
func idnTLD(label string) bool {
	const prefix = "xn--"
	if len(label) <= len(prefix) || !strings.EqualFold(label[:len(prefix)], prefix) {
		return false
	}
	for _, r := range label[len(prefix):] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
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
// otherwise ride into a mail header -- the name is interpolated into mail.
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
