// Package uploadclient speaks Drive's upload protocol.
//
// It is a real client, not test scaffolding: the Phase 2 integration battery
// drives its uploads, and drive-mcp streams agent uploads through it. What it
// implements is the wire contract -- POST /uploads, the presigned part PUTs,
// per-part confirmation, the resume handshake, complete -- and nothing about
// the server's internals. It never imports internal/upload or internal/api, so
// it can be exercised against an httptest.Server with no database in sight.
//
// The design constraints worth knowing before reading further:
//
//   - A part is never held in memory. Sources are io.ReaderAt, parts stream
//     through MD5 in 8 MiB chunks, and the PUT body is an io.SectionReader over
//     the same source. A 50 GB upload costs a few tens of MiB of buffers.
//   - The fingerprint is a cross-language contract with the browser engine.
//     Its recipe is pinned byte for byte in hash.go; a silent mismatch breaks
//     resume with no error anywhere.
//   - Auth comes in two shapes and they are not interchangeable. Cookie auth
//     carries X-Drive-Client, because that header plus SameSite=Lax cookies is
//     the CSRF scheme. Bearer auth deliberately omits it -- a request with no
//     cookie has no CSRF surface, and Phase 6 asserts the header's absence.
package uploadclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CookieName is the session cookie the API sets. Duplicated rather than
// imported: internal/api is not importable from here in spirit (this package
// must stay buildable against the wire contract alone), and the name is part of
// that contract.
const CookieName = "drive_session"

// ClientHeader marks a request as coming from a first-party client. Cookie-auth
// mutations require it; bearer-auth requests must not send it.
const ClientHeader = "X-Drive-Client"

// Tunables mirroring the browser engine's state machine, which is tested
// against the same numbers. Changing one here without changing it there makes
// the two clients behave differently under the same failure.
const (
	// IntegrityBudget is how many times one part may fail on content grounds --
	// an ETag that does not match the MD5, a rejected confirmation, a hard HTTP
	// error -- before the upload gives up on it.
	IntegrityBudget = 8

	// MaxRehandshakes bounds *consecutive* expired-URL retries for one part. It
	// exists to catch host/VM clock skew, which otherwise presents as an
	// infinite loop of freshly-signed URLs that are already expired.
	MaxRehandshakes = 3

	// LowPool is the URL count at which the client refills proactively, before
	// anything has failed.
	LowPool = 2

	// DefaultConcurrency is how many parts are in flight at once.
	DefaultConcurrency = 3

	backoffBase = 1 * time.Second
	backoffCap  = 60 * time.Second

	// defaultPollInterval paces the status polling a 409 in_progress triggers.
	defaultPollInterval = 1 * time.Second
	// maxPollWait bounds that polling, so a finalizer that never finishes
	// surfaces as an error instead of hanging the caller forever.
	maxPollWait = 10 * time.Minute
)

// Client talks to one Drive server as one identity.
//
// The zero value is not usable; construct with New. A Client is safe for
// concurrent use.
type Client struct {
	base string
	http *http.Client
	auth authMode
	log  *slog.Logger

	concurrency int
	base_       time.Duration // backoff base
	cap_        time.Duration // backoff ceiling
	poll        time.Duration
	maxPoll     time.Duration

	// sleep and jitter are indirected so tests run in milliseconds instead of
	// minutes, and so a backoff sequence can be made deterministic.
	sleep  func(context.Context, time.Duration) error
	jitter func() float64

	randMu sync.Mutex
}

// authMode is how a request proves who it is. Exactly one form is active: the
// last auth option wins, because sending both a cookie and a bearer token is a
// configuration mistake, not a fallback chain.
type authMode struct {
	cookie string
	bearer string
}

func (a authMode) apply(r *http.Request) {
	if a.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+a.bearer)
		// No X-Drive-Client, deliberately: bearer requests are CSRF-exempt and
		// Phase 6's suite asserts the header is absent.
		return
	}
	if a.cookie != "" {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: a.cookie})
	}
	// The SPA sends this on every request, not only mutations; matching it
	// keeps the two clients indistinguishable to the middleware.
	r.Header.Set(ClientHeader, "web")
}

// Option configures a Client.
type Option func(*Client)

// WithCookieAuth authenticates as the browser does: the session cookie plus the
// X-Drive-Client header on every request. sessionToken is the raw value of the
// drive_session cookie handed out by POST /api/auth/login.
func WithCookieAuth(sessionToken string) Option {
	return func(c *Client) { c.auth = authMode{cookie: sessionToken} }
}

// WithBearerToken authenticates with a personal access token and sends no
// X-Drive-Client header. This is the form drive-mcp uses.
func WithBearerToken(tok string) Option {
	return func(c *Client) { c.auth = authMode{bearer: tok} }
}

// WithHTTPClient supplies the transport. The default has no timeout, because a
// single part PUT can legitimately run for minutes; bound requests with the
// context instead.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithPartConcurrency sets how many parts upload at once. Values below 1 are
// ignored.
func WithPartConcurrency(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.concurrency = n
		}
	}
}

// WithLogger sets the structured logger. The default discards.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// WithBackoff overrides the retry backoff base and ceiling. The production
// numbers are 1 s and 60 s; tests shrink them so a budget-exhaustion case runs
// in milliseconds.
func WithBackoff(base, ceiling time.Duration) Option {
	return func(c *Client) {
		if base > 0 {
			c.base_ = base
		}
		if ceiling > 0 {
			c.cap_ = ceiling
		}
	}
}

// WithPollInterval sets how often a 409 in_progress re-polls GET /uploads/{id}.
func WithPollInterval(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.poll = d
		}
	}
}

// New returns a Client for a Drive server.
//
// baseURL may be either the server origin ("http://localhost:8080") or the API
// root ("http://localhost:8080/api"); both resolve to the same endpoints. The
// first form is what drive-mcp's DRIVE_URL holds.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		base:        normalizeBase(baseURL),
		http:        &http.Client{},
		log:         slog.New(slog.DiscardHandler),
		concurrency: DefaultConcurrency,
		base_:       backoffBase,
		cap_:        backoffCap,
		poll:        defaultPollInterval,
		maxPoll:     maxPollWait,
	}
	c.sleep = sleepCtx
	c.jitter = func() float64 {
		c.randMu.Lock()
		defer c.randMu.Unlock()
		return rand.Float64()
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func normalizeBase(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if strings.HasSuffix(s, "/api") {
		return s
	}
	return s + "/api"
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoffFor returns the wait before attempt n (1-based): a jittered value in
// [exp/2, exp] where exp = min(cap, base * 2^(n-1)). Same shape as the browser
// engine's backoffDelay -- hours-long uploads need patient, non-synchronized
// retries.
func (c *Client) backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 30 {
		shift = 30
	}
	exp := c.base_ * (1 << uint(shift))
	if exp > c.cap_ || exp <= 0 {
		exp = c.cap_
	}
	half := exp / 2
	return half + time.Duration(c.jitter()*float64(half))
}

// -------------------------------------------------------------------- errors --

// The protocol's error codes, as Go sentinels. Every one of them wraps an
// *APIError carrying the server's code, status and message, so callers can
// match coarsely with errors.Is or read the detail with errors.As.
var (
	// ErrSessionExpired is 410 session_expired: the session is gone. The record
	// is worthless; create a fresh upload.
	ErrSessionExpired = errors.New("uploadclient: upload session expired")

	// ErrNameConflict is 409 name_conflict on create: the caller must pick a
	// conflict_policy.
	ErrNameConflict = errors.New("uploadclient: name conflict")

	// ErrVerifyFailed is the chimera refusal -- 409 invalid on the resume
	// handshake. The re-selected file is not the file whose parts are stored.
	ErrVerifyFailed = errors.New("uploadclient: part verification failed")

	// ErrInProgress is 409 in_progress: a finalizer holds the session. Poll,
	// never re-drive.
	ErrInProgress = errors.New("uploadclient: upload is being finalized")

	// ErrNotFound is 404.
	ErrNotFound = errors.New("uploadclient: not found")

	// ErrInvalid is a 422 the client cannot resolve by retrying.
	ErrInvalid = errors.New("uploadclient: invalid request")
)

// APIError is a non-2xx response carrying the API's error envelope.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.Path, e.Status, e.Code, msg)
}

// Is maps the wire codes onto the sentinels above.
//
// The one non-obvious mapping is ErrVerifyFailed: the contract spells the
// chimera refusal as 409 with code "invalid", which is the only place those two
// appear together -- in_progress is the other 409, and every other "invalid" is
// a 422.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrSessionExpired:
		return e.Status == http.StatusGone || e.Code == "session_expired"
	case ErrNameConflict:
		return e.Code == "name_conflict"
	case ErrInProgress:
		return e.Code == "in_progress"
	case ErrVerifyFailed:
		return e.Status == http.StatusConflict && e.Code == "invalid"
	case ErrNotFound:
		return e.Status == http.StatusNotFound || e.Code == "not_found"
	case ErrInvalid:
		// Deliberately status-only: the chimera refusal is also code "invalid",
		// and it is a 409 that must stay distinguishable from a 422.
		return e.Status == http.StatusUnprocessableEntity
	}
	return false
}

// --------------------------------------------------------------- wire shapes --

// PresignedPart is one presigned part URL.
type PresignedPart struct {
	PartNumber int       `json:"part_number"`
	URL        string    `json:"url"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Session status values.
const (
	StatusActive     = "active"
	StatusCompleting = "completing"
	StatusDone       = "done"
	StatusAborted    = "aborted"
)

// Status is the contract's status shape, returned by GET /uploads/{id} and
// embedded in every create and resume response.
type Status struct {
	UploadID         string    `json:"upload_id"`
	Mode             string    `json:"mode"`
	FileName         string    `json:"file_name"`
	FileSize         int64     `json:"file_size"`
	PartSize         int64     `json:"part_size"`
	PartsTotal       int       `json:"parts_total"`
	Fingerprint      string    `json:"fingerprint"`
	ParentID         *string   `json:"parent_id"`
	Status           string    `json:"status"`
	ConfirmedParts   []int     `json:"confirmed_parts"`
	NodeID           *string   `json:"node_id,omitempty"`
	SessionExpiresAt time.Time `json:"session_expires_at"`
}

// createReq is POST /uploads.
type createReq struct {
	FileName       string  `json:"file_name"`
	FileSize       int64   `json:"file_size"`
	Mime           string  `json:"mime"`
	ParentID       *string `json:"parent_id"`
	Fingerprint    string  `json:"fingerprint"`
	ConflictPolicy string  `json:"conflict_policy,omitempty"`
}

// createResp is the status shape plus a first batch of URLs -- or, when the
// matched session armed the chimera guard, an empty batch and the pins.
type createResp struct {
	Status
	Presigned   []PresignedPart `json:"presigned"`
	VerifyParts []int           `json:"verify_parts"`
}

// resumeResp is the status shape plus a URL for every missing part.
//
// Missing is a pointer because absent and empty mean different things: the
// server omits the field entirely while verification is armed, and an empty
// list would read as "nothing left to upload".
type resumeResp struct {
	Status
	Missing     *[]PresignedPart `json:"missing"`
	VerifyParts []int            `json:"verify_parts"`
}

type resumeReq struct {
	PartMD5s map[string]string `json:"part_md5s,omitempty"`
}

type confirmReq struct {
	ETag string `json:"etag"`
	MD5  string `json:"md5"`
	Size int64  `json:"size"`
}

type confirmResp struct {
	Confirmed        bool      `json:"confirmed"`
	SessionExpiresAt time.Time `json:"session_expires_at"`
}

type completeReq struct {
	SHA256 string `json:"sha256"`
}

type completeResp struct {
	NodeID   string  `json:"node_id"`
	Name     string  `json:"name"`
	ParentID *string `json:"parent_id"`
}

// ListPage is one page of GET /uploads.
type ListPage struct {
	Items      []Status `json:"items"`
	NextCursor *string  `json:"next_cursor"`
}

// ----------------------------------------------------------------- plumbing --

// maxErrBody bounds how much of an error response we read before giving up on
// finding an envelope in it.
const maxErrBody = 64 << 10

// do performs one JSON request. It returns the HTTP status so callers can tell
// 201-created from 200-matched, and decodes into out when out is non-nil.
func (c *Client) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("uploadclient: encoding %s %s: %w", method, path, err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, fmt.Errorf("uploadclient: building %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	c.auth.apply(req)

	res, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("uploadclient: %s %s: %w", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxErrBody))
		_ = res.Body.Close()
	}()

	if res.StatusCode >= 400 {
		return res.StatusCode, c.apiError(method, path, res)
	}
	if out == nil || res.StatusCode == http.StatusNoContent {
		return res.StatusCode, nil
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return res.StatusCode, fmt.Errorf("uploadclient: decoding %s %s: %w", method, path, err)
	}
	return res.StatusCode, nil
}

func (c *Client) apiError(method, path string, res *http.Response) error {
	e := &APIError{Method: method, Path: path, Status: res.StatusCode}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxErrBody))
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &env) == nil {
		e.Code, e.Message = env.Code, env.Message
	}
	if e.Code == "" {
		e.Code = "internal"
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
		}
	}
	return e
}

// ----------------------------------------------------------- simple methods --

// Status fetches GET /uploads/{id}. A 410 comes back as ErrSessionExpired.
func (c *Client) Status(ctx context.Context, uploadID string) (*Status, error) {
	var st Status
	if _, err := c.do(ctx, http.MethodGet, "/uploads/"+uploadID, nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Cancel aborts a session: DELETE /uploads/{id}.
func (c *Client) Cancel(ctx context.Context, uploadID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/uploads/"+uploadID, nil, nil)
	return err
}

// List returns one page of the caller's upload sessions. An empty cursor starts
// at the beginning.
func (c *Client) List(ctx context.Context, cursor string, limit int) (*ListPage, error) {
	path := "/uploads"
	q := make([]string, 0, 2)
	if cursor != "" {
		q = append(q, "cursor="+cursor)
	}
	if limit > 0 {
		q = append(q, fmt.Sprintf("limit=%d", limit))
	}
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	var page ListPage
	if _, err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}
