// Package api is Drive's HTTP surface: the router, the shared request/response
// helpers every handler uses, and the middleware chain that authenticates them.
//
// The helpers below are the internal contract every feature file in this
// package writes against (auth, nodes, trash, search, uploads). Handlers
// use them rather than writing JSON by hand, so the wire format stays uniform:
// snake_case bodies, RFC3339 timestamps, one error envelope, one list envelope.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/mail"
	"github.com/rahul-sharma-cs/drive/server/internal/node"
	"github.com/rahul-sharma-cs/drive/server/internal/oidcauth"
	"github.com/rahul-sharma-cs/drive/server/web"
)

// Canonical error codes. These are the only values that appear in an error
// envelope's "code" for a 4xx; the client's state machine switches on them.
const (
	CodeInvalid      = "invalid"
	CodeUnauthorized = "unauthorized"
	// CodeEmailUnverified is login's one non-generic refusal: the password was
	// right but the address has never been confirmed. It is a code rather than
	// a message the client matches on, so the "resend the link" button is
	// driven by the contract instead of by English.
	CodeEmailUnverified = "email_unverified"
	CodeNotFound        = "not_found"
	CodeNameConflict    = "name_conflict"
	CodeCycle           = "cycle"
	CodeRateLimited     = "rate_limited"
	CodeSessionExpired  = "session_expired"
	CodeInProgress      = "in_progress"
	CodeUnsupported     = "unsupported"

	// CodeInternal is the one code outside the canonical list: it is reserved
	// for 5xx responses (panics), which no client decision depends on.
	CodeInternal = "internal"
)

// maxBodyBytes caps every JSON request body.
const maxBodyBytes = 1 << 20 // 1 MiB

// Pagination bounds for ?cursor=&limit=.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

var (
	// ErrBadJSON is what ReadJSON returns for anything it could not decode;
	// handlers turn it into 422 {code:"invalid"}.
	ErrBadJSON = errors.New("malformed JSON body")
	// ErrBadCursor is returned by DecodeCursor and Page for an opaque cursor
	// that is not one of ours.
	ErrBadCursor = errors.New("invalid cursor")
)

// Server carries everything a handler needs. One instance per process.
type Server struct {
	Cfg     *config.Config
	DB      *pgxpool.Pool
	Log     *slog.Logger
	Mail    mail.Sender
	S3      *s3.Client
	Presign *s3.PresignClient

	// Argon2 bounds how many password hashes or verifications run at once.
	// Every handler that touches a password goes through it.
	Argon2 *auth.Limiter
	// AuthRate is the per-IP token bucket in front of /api/auth.
	AuthRate *ipLimiter
	// BrowserRate is the same allowance again, spent separately, in front of
	// the two Google routes. They are the only auth routes a stranger can make
	// somebody's browser visit, so they must not be able to empty the bucket
	// the password form depends on.
	BrowserRate *ipLimiter
	// MailRate is the per-IP bucket in front of the two endpoints that mail an
	// address the caller does not have to own: password-reset and
	// resend-verification.
	MailRate *ipLimiter
	// Google is the OIDC client, or nil when no Google client is configured --
	// which is a deployment that offers password sign-in only. Building it
	// makes no network request: discovery happens on the first sign-in, so a
	// provider that is unreachable cannot stop the server booting.
	Google *oidcauth.Provider
	// BulkBudget is the wall clock one bulk trash request spends before it
	// stops and reports what it has not reached yet. Zero means
	// DefaultBulkBudget; a test that wants the budget to run out sets it small.
	BulkBudget time.Duration
}

// New builds the server. Dependencies are passed in rather than constructed so
// tests can supply exactly the ones a case touches.
//
// The limiters are built here rather than passed: they carry no external
// dependency, they must be shared by every request in the process, and a test
// that wants a saturated one can replace the field on the returned Server.
func New(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger, sender mail.Sender, s3c *s3.Client, presign *s3.PresignClient) *Server {
	argon2Limit, authRate, mailRate := 0, 0, 0
	if cfg != nil {
		argon2Limit, authRate, mailRate = cfg.Argon2Limit, cfg.AuthRatePerMin, cfg.MailRatePerHour
	}
	if authRate < 1 {
		authRate = DefaultAuthRatePerMin
	}
	if mailRate < 1 {
		mailRate = DefaultMailRatePerHour
	}
	// The redirect URI is derived from the deployment's own base URL and
	// nothing else -- never from r.Host, which a caller controls. One source of
	// truth cannot drift out of step with itself, which is why there is no
	// variable for it.
	var google *oidcauth.Provider
	if cfg.UseGoogle() {
		google = oidcauth.New(oidcauth.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			Issuer:       cfg.GoogleIssuer,
			RedirectURL:  strings.TrimSuffix(cfg.BaseURL, "/") + "/api/auth/google/callback",
		})
	}

	return &Server{
		Cfg: cfg, DB: pool, Log: log, Mail: sender, S3: s3c, Presign: presign,
		Google:      google,
		Argon2:      auth.NewLimiter(argon2Limit),
		AuthRate:    newIPLimiter(float64(authRate), burstFor(float64(authRate))),
		BrowserRate: newIPLimiter(float64(authRate), burstFor(float64(authRate))),
		// The mail bucket's burst is the allowance itself, not twice it: this
		// one is a budget for the hour, and a burst on top of it would simply
		// be a different, larger number nobody chose.
		MailRate: newIPLimiter(float64(mailRate)/60, float64(mailRate)),
	}
}

// Routes builds the whole handler tree.
//
// Chain order is fixed: request id, then the slog request logger (so every
// line carries request_id), then panic recovery, then -- inside /api only --
// the X-Drive-Client check and the session loader. The session loader does not
// reject anonymous requests; RequireAuth does, which is what lets the public
// share routes share this chain.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(s.requestLogger)
	r.Use(s.recoverer)

	r.Get("/healthz", s.healthz)
	r.Get("/livez", s.livez)

	r.Route("/api", func(r chi.Router) {
		r.Use(RequireClientHeader)
		r.Use(s.sessionLoader)

		s.mountAuth(r)
		s.mountNodes(r)
		s.mountTrash(r)
		s.mountSearch(r)
		s.mountUsage(r)
		s.mountDownload(r)
		s.mountUploads(r)
		s.mountUploadComplete(r)

		// Unmatched /api paths answer with the JSON envelope, never the SPA.
		// This is also what keeps the chain above alive: chi skips a mux's
		// middleware entirely when the mux has no routes at all, and the
		// feature files register theirs later.
		unmatched := func(w http.ResponseWriter, r *http.Request) {
			WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such endpoint")
		}
		r.Handle("/*", http.HandlerFunc(unmatched))
		r.NotFound(unmatched)
		r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			WriteErr(w, r, http.StatusMethodNotAllowed, CodeInvalid, "method not allowed")
		})
	})

	// Everything else is the SPA.
	r.NotFound(s.spaHandler())
	r.MethodNotAllowed(s.spaHandler())

	return r
}

// healthz is the readiness probe make e2e and the integration harness wait on.
// It is unauthenticated and deliberately not under /api.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if s.DB == nil {
		http.Error(w, "no database", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.DB.Ping(ctx); err != nil {
		LoggerFrom(r.Context()).Warn("healthz: database unreachable", "error", err)
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// livez says the process is up and serving. It touches nothing.
//
// It is separate from healthz on purpose. A platform health check that fails
// restarts the container, so wiring it to a database ping means a brief
// database blip becomes a restart loop -- exactly when a running process would
// have recovered on its own. healthz keeps the database ping, because what the
// test harness and `make e2e` wait for is "ready to serve requests", which is a
// different question.
func (s *Server) livez(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// spaHandler serves the embedded SPA, falling back to index.html so client-side
// routes (/s/{token}, /verify, ...) resolve on a hard refresh.
func (s *Server) spaHandler() http.HandlerFunc {
	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic(fmt.Sprintf("embedded SPA: %v", err))
	}
	files := http.FileServer(http.FS(dist))

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			WriteErr(w, r, http.StatusNotFound, CodeNotFound, "not found")
			return
		}
		// TrimLeft, not TrimPrefix: chi does not clean the path, so
		// "//assets/x.js" arrives with both slashes and trimming one would
		// leave a name that no longer starts with the prefix -- which is the
		// check the fallback below turns on.
		name := strings.TrimLeft(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(dist, name); err != nil {
			// Under assets/ the fallback is wrong. Every name there is written
			// by the build and carries a content hash, so a miss is a missing
			// file and never a client-side route -- and answering it with the
			// entry document means a <script src> gets 200 text/html, which
			// surfaces as a MIME-type console error instead of the 404 it is.
			if strings.HasPrefix(name, assetPrefix) {
				// no-store because a 404 is heuristically cacheable: a rollback
				// that brings the file back must not meet a stored miss.
				w.Header().Set("Cache-Control", "no-store")
				WriteErr(w, r, http.StatusNotFound, CodeNotFound, "not found")
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = "/"
			name = "index.html"
		}
		w.Header().Set("Cache-Control", spaCacheControl(name))
		files.ServeHTTP(w, r)
	}
}

// assetPrefix is the build's fingerprinted output directory. Vite writes every
// hashed file under it, and nothing else is served from there.
const assetPrefix = "assets/"

// spaCacheControl decides how long a served SPA file may be cached.
//
// The build's asset names carry a content hash, so they can be kept forever;
// the entry document names them and must not be, or a browser goes on serving
// the previous release's HTML -- pointing at asset files that no longer exist
// -- for as long as its heuristic cache decides to. Note the caller passes the
// name it actually serves, so a client-side route that falls back to index.html
// is answered as the document, not as whatever the URL looked like.
func spaCacheControl(name string) string {
	if strings.HasPrefix(name, assetPrefix) {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

// ---------------------------------------------------------------- responses --

// WriteJSON writes v as the response body with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Error("writing response body", "error", err)
	}
}

// ErrorBody is the error envelope: {code, message, details?}.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// WriteErr writes the canonical error envelope.
func WriteErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	WriteErrDetails(w, r, status, code, msg, nil)
}

// WriteErrDetails writes the error envelope with a details payload.
func WriteErrDetails(w http.ResponseWriter, r *http.Request, status int, code, msg string, details any) {
	if status >= http.StatusInternalServerError {
		LoggerFrom(r.Context()).Error("request failed", "status", status, "code", code, "message", msg)
	} else {
		LoggerFrom(r.Context()).Debug("request rejected", "status", status, "code", code, "message", msg)
	}
	WriteJSON(w, status, ErrorBody{Code: code, Message: msg, Details: details})
}

// List is the list envelope: {"items": [...], "next_cursor": string|null}.
type List[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// NewList builds a list envelope, guaranteeing items serializes as [] and not
// null, and next_cursor as null when there is no further page.
func NewList[T any](items []T, next string) List[T] {
	if items == nil {
		items = []T{}
	}
	l := List[T]{Items: items}
	if next != "" {
		l.NextCursor = &next
	}
	return l
}

// NodeDTO is the canonical node shape on the wire. Folders carry size null.
//
// The last two fields are context, not state, and each is filled in by exactly
// one listing: DeletedAt by the trash (where "when did I delete this" is the
// column the user reads), ItemCount by a children page (where a folder row
// says how much is inside). Both are omitempty, so every other response --
// a live node, a created folder, a moved one -- serializes byte for byte as it
// did before they existed.
type NodeDTO struct {
	ID          uuid.UUID  `json:"id"`
	ParentID    *uuid.UUID `json:"parent_id"`
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	Size        *int64     `json:"size"`
	Mime        *string    `json:"mime"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	TrashedRoot bool       `json:"trashed_root,omitempty"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
	ItemCount   *int       `json:"item_count,omitempty"`
}

// ----------------------------------------------------------------- requests --

// ReadJSON decodes the request body into dst under a 1 MiB cap, rejecting
// unknown fields and trailing content.
func ReadJSON(r *http.Request, dst any) error {
	// A nil ResponseWriter is fine here: MaxBytesReader only uses it to hint
	// the server that the request was too large, and we answer with our own
	// envelope anyway.
	limited := http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", ErrBadJSON, err)
	}
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: unexpected trailing content", ErrBadJSON)
	}
	return nil
}

// Page reads the standard list parameters: an opaque cursor and a limit that
// defaults to 50 and is clamped at 200.
func Page(r *http.Request) (cursor string, limit int, err error) {
	q := r.URL.Query()
	cursor = q.Get("cursor")
	limit = DefaultLimit

	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 {
			return "", 0, fmt.Errorf("limit: want a positive integer, got %q", raw)
		}
		limit = min(n, MaxLimit)
	}
	return cursor, limit, nil
}

// SortParams reads a children listing's ?sort= and ?dir=, defaulting to name
// ascending. The strings are validated against the node package's fixed
// vocabulary here, at the edge, so nothing further in never sees an
// unrecognized one; the error is a 422 like any other bad parameter.
func SortParams(r *http.Request) (node.ChildSort, error) {
	q := r.URL.Query()
	return node.NewChildSort(strings.TrimSpace(q.Get("sort")), strings.TrimSpace(q.Get("dir")))
}

// EncodeCursor packs v into an opaque pagination cursor.
func EncodeCursor(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		slog.Default().Error("encoding cursor", "error", err)
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeCursor unpacks a cursor produced by EncodeCursor. Anything else -- a
// truncated string, a hand-edited one, JSON of the wrong shape -- is
// ErrBadCursor, which handlers report as 422 {code:"invalid"}.
func DecodeCursor(s string, dst any) error {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return fmt.Errorf("%w: not base64url", ErrBadCursor)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", ErrBadCursor, err)
	}
	return nil
}
