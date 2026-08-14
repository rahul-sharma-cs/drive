package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
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

// WithUser puts a user in the context. Exported so tests and later phases
// (bearer-token auth) can populate the same slot.
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

		next.ServeHTTP(ww, r.WithContext(context.WithValue(r.Context(), loggerKey{}, l)))

		l.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
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
// Phase 6: requests authenticated by `Authorization: Bearer drv_...` are exempt
// (no cookie, so no CSRF surface).
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
			SELECT s.id, u.id, u.email, u.display_name, u.email_verified_at, root.id
			  FROM auth_sessions s
			  JOIN users u ON u.id = s.user_id
			  LEFT JOIN nodes root
			    ON root.owner_id = u.id AND root.parent_id IS NULL
			 WHERE s.token_hash = $1 AND s.expires_at > now()`

		var (
			sessionID uuid.UUID
			u         User
			rootID    *uuid.UUID
		)
		if err := s.DB.QueryRow(ctx, q, sum[:]).Scan(
			&sessionID, &u.ID, &u.Email, &u.DisplayName, &u.EmailVerifiedAt, &rootID,
		); err != nil {
			// Either way the request continues anonymously, but only one of
			// these is normal: an infrastructure failure that logs users out
			// must be visible in the request log.
			if !errors.Is(err, pgx.ErrNoRows) {
				LoggerFrom(ctx).Warn("session lookup failed", "error", err)
			}
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
		if _, err := s.DB.Exec(ctx, slide, sessionID); err != nil {
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
