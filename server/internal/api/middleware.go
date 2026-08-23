package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SessionCookie is the auth cookie's name. Its value is a raw 256-bit token
// whose sha256 lands in auth_sessions.token_hash.
const SessionCookie = "drive_session"

// SessionTTL is the sliding lifetime of an auth session; the cookie's MaxAge
// and auth_sessions.expires_at both use it.
const SessionTTL = 30 * 24 * time.Hour

// ClientHeader marks a request as coming from our own SPA. Together with
// SameSite=Lax cookies and the absence of cross-origin credentialed CORS, it is
// the CSRF scheme.
const ClientHeader = "X-Drive-Client"

// User is the authenticated caller, loaded by sessionLoader.
type User struct {
	ID              uuid.UUID
	Email           string
	DisplayName     string
	RootID          uuid.UUID
	EmailVerifiedAt *time.Time
	// HasPassword says whether users.password_hash is set. It is the boolean
	// and not the hash on purpose: the account screen needs to know which
	// password section to render, and nothing in a request needs the credential
	// itself -- the one handler that does reads it from the database.
	HasPassword bool
	// SessionID is the auth_sessions row this request arrived on. The session
	// list marks it "current", a password change is the one session it keeps,
	// and revoking it is what clears the caller's own cookie. It is uuid.Nil
	// for a user put on the context by anything but the session loader.
	SessionID uuid.UUID
}

// Verified reports whether the account finished email verification. Login is
// refused without it.
func (u *User) Verified() bool { return u != nil && u.EmailVerifiedAt != nil }

type userKey struct{}
type loggerKey struct{}

// UserFromCtx returns the authenticated user, if the session loader found one.
func UserFromCtx(ctx context.Context) (*User, bool) {
	u, ok := ctx.Value(userKey{}).(*User)
	return u, ok && u != nil
}

// MustUser returns the authenticated user and panics if there is none. Only
// call it from handlers mounted behind RequireAuth; the panic is a programming
// error, and the recoverer turns it into a 500.
func MustUser(ctx context.Context) *User {
	u, ok := UserFromCtx(ctx)
	if !ok {
		panic("api.MustUser: no user in context (handler is not behind RequireAuth)")
	}
	return u
}

// WithUser puts a user in the context. Exported so tests -- and any later
// authenticator, such as bearer tokens -- can populate the same slot.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// LoggerFrom returns the request-scoped logger, which carries request_id.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// requestLogger stamps the chi request id onto a logger stored in the request
// context, so every line a handler writes carries it, and logs one line per
// request.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := s.Log
		if base == nil {
			base = slog.Default()
		}
		l := base.With("request_id", middleware.GetReqID(r.Context()))
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()

		r = r.WithContext(context.WithValue(r.Context(), loggerKey{}, l))
		// The caller's address is resolved here, once, and everything
		// downstream reads that answer: the rule logs when it will not trust a
		// forwarded header, and three separate layers ask for the address on a
		// single request. The logger goes on the context first so that line
		// carries the request id.
		r = withResolvedClientIP(r)

		next.ServeHTTP(ww, r)

		l.Debug("request",
			"method", r.Method,
			"path", redactSharePath(r.URL.Path),
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// redactSharePath replaces the token segment of a share URL -- /api/s/{token}
// and the page's /s/{token} -- with a placeholder, and leaves every other path
// alone. The segments after the token stay, so the line still says which route
// it was.
//
// A passwordless share token is the entire credential, and the request logger
// writes every path at Debug. Leading slashes are trimmed before the prefix is
// checked for the reason spaHandler gives: chi does not clean the path, so
// //s/{token} arrives with both slashes and serves the share page all the same.
func redactSharePath(path string) string {
	name := strings.TrimLeft(path, "/")
	var prefix string
	switch {
	case strings.HasPrefix(name, "api/s/"):
		prefix = "api/s/"
	case strings.HasPrefix(name, "s/"):
		prefix = "s/"
	default:
		return path
	}
	rest := name[len(prefix):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[i:]
	} else {
		rest = ""
	}
	return "/" + prefix + "{redacted}" + rest
}

// recoverer turns a panic into a logged 500 carrying the JSON error envelope.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			LoggerFrom(r.Context()).Error("panic", "value", rec, "stack", string(debug.Stack()))
			WriteErr(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}

// RequireClientHeader enforces X-Drive-Client on every /api mutation, including
// the public share POSTs -- the SPA share page sends it too. GETs and HEADs are
// exempt.
//
// Bearer-authenticated requests would be exempt (no cookie, so no CSRF
// surface), if bearer auth ever ships.
func RequireClientHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if r.Header.Get(ClientHeader) == "" {
				WriteErr(w, r, http.StatusForbidden, CodeInvalid, "missing X-Drive-Client header")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sessionLoader resolves the drive_session cookie into a User on the request
// context. It never rejects: anonymous requests continue without a user so the
// public share routes can share this chain. RequireAuth is what refuses them.
func (s *Server) sessionLoader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookie)
		if err != nil || cookie.Value == "" || s.DB == nil {
			next.ServeHTTP(w, r)
			return
		}

		sum := sha256.Sum256([]byte(cookie.Value))
		ctx := r.Context()

		const q = `
			SELECT s.id, u.id, u.email, u.display_name, u.email_verified_at,
			       u.password_hash IS NOT NULL, root.id
			  FROM auth_sessions s
			  JOIN users u ON u.id = s.user_id
			  LEFT JOIN nodes root
			    ON root.owner_id = u.id AND root.parent_id IS NULL
			 WHERE s.token_hash = $1 AND s.expires_at > now()`

		var (
			u      User
			rootID *uuid.UUID
		)
		if err := s.DB.QueryRow(ctx, q, sum[:]).Scan(
			&u.SessionID, &u.ID, &u.Email, &u.DisplayName, &u.EmailVerifiedAt, &u.HasPassword, &rootID,
		); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				// The cookie may be perfectly good; we simply could not look it
				// up. Continuing anonymously turns that into a 401 at
				// RequireAuth, and 401 means "your credentials are bad" -- a
				// terminal answer neither client retries, which ends a 50 GB
				// upload with no recovery path over a transient database
				// hiccup. 503 says try again, which is the truth.
				LoggerFrom(ctx).Error("session lookup failed", "error", err)
				WriteErr(w, r, http.StatusServiceUnavailable, CodeInternal, "internal server error")
				return
			}
			// No such session: a signed-out or stale cookie. The request
			// continues anonymously; RequireAuth is what refuses it.
			next.ServeHTTP(w, r)
			return
		}
		if rootID != nil {
			u.RootID = *rootID
		}

		// Slide the 30-day expiry and refresh last_seen_at, at most hourly, in
		// one conditional write.
		const slide = `
			UPDATE auth_sessions
			   SET expires_at = now() + interval '30 days', last_seen_at = now()
			 WHERE id = $1
			   AND (last_seen_at IS NULL OR last_seen_at < now() - interval '1 hour')`
		if _, err := s.DB.Exec(ctx, slide, u.SessionID); err != nil {
			LoggerFrom(ctx).Warn("sliding session", "error", err)
		}

		next.ServeHTTP(w, r.WithContext(WithUser(ctx, &u)))
	})
}

// RequireAuth rejects anonymous requests. Routes that serve both signed-in
// users and share guests are mounted without it.
func (s *Server) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserFromCtx(r.Context()); !ok {
			WriteErr(w, r, http.StatusUnauthorized, CodeUnauthorized, "sign in to continue")
			return
		}
		next.ServeHTTP(w, r)
	})
}
