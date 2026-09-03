package api

// Share links, both halves. The owner's: make one, list them, change one,
// stop one -- every owner route is owner-scoped in SQL (created_by =
// caller), so another user's share id is 404 exactly like an unknown one.
// The public half: the five /api/s/{token}/* routes anyone holding the URL
// drives, registered into mountShare's group in share_headers.go, which
// carries the headers every answer under /api/s needs. The public handlers
// never read the user the chain may have loaded -- an owner's own
// drive_session grants a share page nothing, and a guest cookie grants the
// app nothing.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/node"
	"github.com/rahul-sharma-cs/drive/server/internal/share"
	"github.com/rahul-sharma-cs/drive/server/internal/upload"
)

// mountShares registers the owner's routes. The two writers sit in the
// per-IP auth bucket although they are authenticated, for the reason
// mountAuth gives for POST /password: they reach Argon2 with a caller-chosen
// password, and the limiter's four slots are shared with everybody's login.
func (s *Server) mountShares(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.RequireAuth)

		r.Get("/shares", s.listShares)
		r.Delete("/shares/{id}", s.revokeShare)
		r.With(s.RateLimitAuth).Post("/shares", s.createShare)
		r.With(s.RateLimitAuth).Patch("/shares/{id}", s.updateShare)
	})
}

// ShareDTO is the owner's view of a link: no token, no hash. node.parent_id is
// what the shared-links page builds its row link from; node_live is false
// while the file sits in the trash, which is when the link is inert.
type ShareDTO struct {
	ID            uuid.UUID    `json:"id"`
	Node          ShareNodeDTO `json:"node"`
	NodeLive      bool         `json:"node_live"`
	HasPassword   bool         `json:"has_password"`
	ExpiresAt     *time.Time   `json:"expires_at"`
	MaxDownloads  *int         `json:"max_downloads"`
	DownloadCount int          `json:"download_count"`
	CreatedAt     time.Time    `json:"created_at"`
}

// ShareNodeDTO names the shared file.
type ShareNodeDTO struct {
	ID       uuid.UUID  `json:"id"`
	ParentID *uuid.UUID `json:"parent_id"`
	Name     string     `json:"name"`
	Size     *int64     `json:"size"`
	Mime     *string    `json:"mime"`
}

// ShareDTOFrom converts a stored share to the wire shape.
func ShareDTOFrom(sh share.Share) ShareDTO {
	return ShareDTO{
		ID: sh.ID,
		Node: ShareNodeDTO{
			ID: sh.Node.ID, ParentID: sh.Node.ParentID, Name: sh.Node.Name,
			Size: sh.Node.Size, Mime: sh.Node.Mime,
		},
		NodeLive:      sh.NodeLive,
		HasPassword:   sh.HasPassword,
		ExpiresAt:     sh.ExpiresAt,
		MaxDownloads:  sh.MaxDownloads,
		DownloadCount: sh.DownloadCount,
		CreatedAt:     sh.CreatedAt,
	}
}

// shareResponse is the body of create, regenerate and settings. url is set
// only by the first two -- the only moments the server holds the token -- and
// the settings answer is {share} alone.
type shareResponse struct {
	Share ShareDTO `json:"share"`
	URL   string   `json:"url,omitempty"`
}

// shareURL is the link itself: the deployment's base URL and never r.Host.
func (s *Server) shareURL(token string) string {
	return s.baseURL() + "/s/" + token
}

// WriteShareErr maps the share package's sentinels -- and the node package's
// miss, which is what a create against a node the caller cannot see returns
// -- onto the canonical codes.
func WriteShareErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, node.ErrNotFound):
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such node")
	case errors.Is(err, share.ErrNotFound):
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such share")
	case errors.Is(err, share.ErrExists):
		WriteErr(w, r, http.StatusConflict, CodeExists, "this file already has a share link")
	case errors.Is(err, share.ErrUnsupported):
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeUnsupported, err.Error())
	default:
		LoggerFrom(r.Context()).Error("share operation failed", "error", err)
		WriteErr(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
	}
}

func (s *Server) shares() *share.Store { return share.NewStore(s.DB) }

// shareID reads the {id} path parameter; an unparseable one is a miss, as
// pathID's is.
func shareID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such share")
		return uuid.Nil, false
	}
	return id, true
}

// checkShareSettings validates the three settings an owner can set. Each is
// optional here; the PATCH handler has already insisted on every key being
// present before it gets this far.
func checkShareSettings(expiresAt *time.Time, password *string, maxDownloads *int) error {
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return errors.New("expires_at: must be in the future")
	}
	if password != nil {
		if err := checkPassword(*password); err != nil {
			return err
		}
	}
	if maxDownloads != nil && (*maxDownloads < 1 || *maxDownloads > share.MaxDownloadsCap) {
		return fmt.Errorf("max_downloads: want 1 to %d", share.MaxDownloadsCap)
	}
	return nil
}

// hashSharePassword turns a validated password into the stored change,
// hashing under the Argon2 limiter. The false return means the refusal was
// already written.
func (s *Server) hashSharePassword(w http.ResponseWriter, r *http.Request, password string) (share.PasswordChange, bool) {
	hash, err := s.Argon2.Hash(password)
	if err != nil {
		if errors.Is(err, auth.ErrBusy) {
			s.authBusy(w, r)
		} else {
			s.authFailed(w, r, "hashing a share password", err)
		}
		return share.PasswordChange{}, false
	}
	return share.SetPassword(hash), true
}

type createShareReq struct {
	NodeID       *uuid.UUID `json:"node_id"`
	Mode         string     `json:"mode"`
	Permission   string     `json:"permission"`
	ExpiresAt    *time.Time `json:"expires_at"`
	Password     *string    `json:"password"`
	MaxDownloads *int       `json:"max_downloads"`
}

// POST /api/shares
func (s *Server) createShare(w http.ResponseWriter, r *http.Request) {
	var req createShareReq
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "malformed request body")
		return
	}
	if req.NodeID == nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "node_id is required")
		return
	}
	// mode and permission are accepted so a client may say what it means, and
	// refused unless it means the one thing this phase serves.
	if req.Mode != "" && req.Mode != share.ModePublic {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeUnsupported, `mode: only "public" is supported`)
		return
	}
	if req.Permission != "" && req.Permission != share.PermissionView {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeUnsupported, `permission: only "view" is supported`)
		return
	}
	if err := checkShareSettings(req.ExpiresAt, req.Password, req.MaxDownloads); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}
	// A null password on create is the same as no password -- the SPA sends
	// every optional key explicitly.
	set := share.Settings{ExpiresAt: req.ExpiresAt, MaxDownloads: req.MaxDownloads}
	if req.Password != nil {
		var ok bool
		if set.Password, ok = s.hashSharePassword(w, r, *req.Password); !ok {
			return
		}
	}

	u := MustUser(r.Context())
	sh, token, err := s.shares().Create(r.Context(), u.ID, *req.NodeID, set)
	if err != nil {
		WriteShareErr(w, r, err)
		return
	}
	LoggerFrom(r.Context()).Info("share created", "share_id", sh.ID, "node_id", sh.Node.ID, "user_id", u.ID)
	WriteJSON(w, http.StatusCreated, shareResponse{Share: ShareDTOFrom(sh), URL: s.shareURL(token)})
}

// GET /api/shares?cursor&limit&node_id
func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	cursor, limit, err := Page(r)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}
	var nodeID *uuid.UUID
	if raw := strings.TrimSpace(r.URL.Query().Get("node_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "node_id: not a UUID")
			return
		}
		nodeID = &id
	}
	var after *share.Cursor
	if cursor != "" {
		var c share.Cursor
		if err := DecodeCursor(cursor, &c); err != nil {
			WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "cursor: not one of ours")
			return
		}
		after = &c
	}

	items, next, err := s.shares().List(r.Context(), MustUser(r.Context()).ID, nodeID, after, limit)
	if err != nil {
		WriteShareErr(w, r, err)
		return
	}
	dtos := make([]ShareDTO, 0, len(items))
	for _, sh := range items {
		dtos = append(dtos, ShareDTOFrom(sh))
	}
	var nextCursor string
	if next != nil {
		nextCursor = EncodeCursor(next)
	}
	WriteJSON(w, http.StatusOK, NewList(dtos, nextCursor))
}

// updateShareReq is decoded into raw messages on purpose. The repo's PATCH
// idiom -- *T, nil meaning absent -- cannot also mean "clear", and the
// settings body needs all three meanings: expires_at and max_downloads are
// required keys where null clears, and password is optional -- absent keeps
// the current one, null clears it, a string replaces it. An owner editing an
// expiry cannot re-type a password that exists nowhere but as a hash, which
// is why the password key alone may be left out. Raw messages keep absent
// and null apart.
type updateShareReq struct {
	Action       json.RawMessage `json:"action"`
	ExpiresAt    json.RawMessage `json:"expires_at"`
	Password     json.RawMessage `json:"password"`
	MaxDownloads json.RawMessage `json:"max_downloads"`
}

// PATCH /api/shares/{id} -- either {action:"regenerate"} or the full settings
// triple, never both.
func (s *Server) updateShare(w http.ResponseWriter, r *http.Request) {
	id, ok := shareID(w, r)
	if !ok {
		return
	}
	var req updateShareReq
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "malformed request body")
		return
	}
	hasSettings := len(req.ExpiresAt) > 0 || len(req.Password) > 0 || len(req.MaxDownloads) > 0

	if len(req.Action) > 0 {
		if hasSettings {
			WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "action cannot be combined with settings")
			return
		}
		var action string
		if err := json.Unmarshal(req.Action, &action); err != nil || action != "regenerate" {
			WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, `action: want "regenerate"`)
			return
		}
		s.regenerateShare(w, r, id)
		return
	}

	if len(req.ExpiresAt) == 0 || len(req.MaxDownloads) == 0 {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid,
			"expires_at and max_downloads are required (null clears one)")
		return
	}
	var (
		expiresAt    *time.Time
		password     *string
		maxDownloads *int
	)
	if json.Unmarshal(req.ExpiresAt, &expiresAt) != nil ||
		json.Unmarshal(req.MaxDownloads, &maxDownloads) != nil ||
		(len(req.Password) > 0 && json.Unmarshal(req.Password, &password) != nil) {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "malformed request body")
		return
	}
	if err := checkShareSettings(expiresAt, password, maxDownloads); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}
	set := share.Settings{ExpiresAt: expiresAt, MaxDownloads: maxDownloads}
	switch {
	case len(req.Password) == 0:
		// Absent: the current password, whatever it is, stays.
	case password == nil:
		set.Password = share.ClearPassword()
	default:
		var ok bool
		if set.Password, ok = s.hashSharePassword(w, r, *password); !ok {
			return
		}
	}

	sh, err := s.shares().Settings(r.Context(), MustUser(r.Context()).ID, id, set)
	if err != nil {
		WriteShareErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, shareResponse{Share: ShareDTOFrom(sh)})
}

// regenerateShare is PATCH's {action:"regenerate"} arm: a new token, the old
// one dead, the count back at zero, and the URL shown once more.
func (s *Server) regenerateShare(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	u := MustUser(r.Context())
	sh, token, err := s.shares().Regenerate(r.Context(), u.ID, id)
	if err != nil {
		WriteShareErr(w, r, err)
		return
	}
	LoggerFrom(r.Context()).Info("share regenerated", "share_id", sh.ID, "user_id", u.ID)
	WriteJSON(w, http.StatusOK, shareResponse{Share: ShareDTOFrom(sh), URL: s.shareURL(token)})
}

// DELETE /api/shares/{id} -- the revoke. The row stays for the access log.
func (s *Server) revokeShare(w http.ResponseWriter, r *http.Request) {
	id, ok := shareID(w, r)
	if !ok {
		return
	}
	u := MustUser(r.Context())
	if err := s.shares().Revoke(r.Context(), u.ID, id); err != nil {
		WriteShareErr(w, r, err)
		return
	}
	LoggerFrom(r.Context()).Info("share revoked", "share_id", id, "user_id", u.ID)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- recipients --

// mountShareGuest registers the five public routes into mountShare's /api/s
// group. The share bucket sits in front of the four page routes -- refusing
// /download, the one browser navigation, with a redirect rather than the
// envelope; /password is the one unauthenticated route here that reaches
// Argon2, so it spends the auth allowance instead, exactly like login --
// through its own wrapper, because RateLimitAuth's refusal line logs the
// path and this path carries the credential.
func (s *Server) mountShareGuest(r chi.Router) {
	r.With(s.rateLimitShare).Get("/{token}/meta", s.shareMeta)
	r.With(s.rateLimitShare).Post("/{token}/session", s.shareSession)
	r.With(s.rateLimitSharePassword).Post("/{token}/password", s.sharePassword)
	r.With(s.rateLimitShareDownload).Get("/{token}/download", s.shareDownload)
	r.With(s.rateLimitShare).Get("/{token}/preview", s.sharePreview)
}

// The constant reasons a share refusal is logged under. A handler line
// carries one of these, the share id when there is one, and the route
// pattern -- never the token, the password or the cookie value: a
// passwordless token is the entire credential, and these lines are what
// production writes. The four dead states log as the state's own name
// (revoked, expired, trashed, purged).
const (
	shareReasonNotFound           = "not_found"
	shareReasonNoSession          = "no_session"
	shareReasonForeignSession     = "foreign_session"
	shareReasonBadPassword        = "bad_password"
	shareReasonLocked             = "locked"
	shareReasonBusy               = "busy"
	shareReasonExhausted          = "exhausted"
	shareReasonUnsupportedPreview = "unsupported_preview"
)

// shareNotFoundMsg is the one identical 404 every unknown or dead link gets:
// unknown, revoked, expired, trashed and purged must not be distinguishable
// from outside.
const shareNotFoundMsg = "no such share"

// shareRefused is the refusal line. The route pattern stands in for the
// path, which carries the token; shareID is nil only for an unknown token,
// where there is nothing to name.
func shareRefused(r *http.Request, shareID *uuid.UUID, reason string) {
	l := LoggerFrom(r.Context()).With(
		"reason", reason, "route", chi.RouteContext(r.Context()).RoutePattern())
	if shareID != nil {
		l = l.With("share_id", *shareID)
	}
	l.Info("share request refused")
}

// shareFailed logs the real cause and tells the caller nothing about it.
func (s *Server) shareFailed(w http.ResponseWriter, r *http.Request, what string, err error) {
	LoggerFrom(r.Context()).Error("share: "+what, "error", err)
	WriteErr(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
}

// shareAccess writes one access-log row, outside any transaction that can
// roll back -- a denied row for a spent cap must survive CountOnce's
// rollback. A failed write never fails the request that caused it: the
// answer was already decided, and the log is the owner's record, not a gate.
// Mode 1 always passes a nil email; a share handler never reads a cookie
// that could name anyone.
func (s *Server) shareAccess(r *http.Request, shareID uuid.UUID, action string) {
	if err := s.shares().Log(r.Context(), &shareID, nil, action, ClientIP(r), r.UserAgent()); err != nil {
		LoggerFrom(r.Context()).Error("writing the share access log",
			"error", err, "action", action, "share_id", shareID)
	}
}

// liveShare resolves {token} and refuses everything that is not a live link
// with the identical 404 -- writing the denied row the owner is owed when a
// share row exists to attribute it to, and nothing at all for an unknown
// token, so a scan cannot fill the table. false means the refusal (or the
// 500) is already written.
func (s *Server) liveShare(w http.ResponseWriter, r *http.Request) (*share.Resolved, bool) {
	res, err := s.shares().Resolve(r.Context(), auth.HashToken(chi.URLParam(r, "token")))
	switch {
	case errors.Is(err, share.ErrNotFound):
		shareRefused(r, nil, shareReasonNotFound)
	case err != nil:
		s.shareFailed(w, r, "resolving a link", err)
		return nil, false
	case res.State != share.StateLive:
		shareRefused(r, &res.ShareID, string(res.State))
		s.shareAccess(r, res.ShareID, share.ActionDenied)
	default:
		return res, true
	}
	WriteErr(w, r, http.StatusNotFound, CodeNotFound, shareNotFoundMsg)
	return nil, false
}

// ------------------------------------------------------------- guest cookie --

// guestCookieName is per share on purpose: two links open in two tabs must
// not share a session, and the name is how a handler asks for this share's
// cookie and no other's -- the handler resolves the share by token first and
// only then knows which name to read.
func guestCookieName(shareID uuid.UUID) string { return "gs_" + shareID.String() }

// setGuestCookie writes (or re-writes, on a slide) the guest cookie. The
// path keeps it off everything that is not a share API call; Lax plus the
// X-Drive-Client requirement on the POSTs and /preview is the CSRF posture,
// and /download's residual without the header is accepted and named in the
// protocol.
func (s *Server) setGuestCookie(w http.ResponseWriter, shareID uuid.UUID, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name:     guestCookieName(shareID),
		Value:    raw,
		Path:     "/api/s/",
		MaxAge:   int(share.GuestSessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookies(),
	})
}

// guestFrom resolves this share's cookie to its live guest session. reason
// is the log constant when there is none: no_session for a missing cookie,
// foreign_session for one whose row is not a live session of THIS share --
// another share's, a forged name or a lapsed one alike, which the store
// keeps deliberately indistinguishable. A guest of share A really does
// present its cookie at share B's routes -- the path matches -- and the row
// behind it still says share_id = A, so it answers nothing here however the
// cookie was named.
func (s *Server) guestFrom(r *http.Request, shareID uuid.UUID) (g share.Guest, raw, reason string, err error) {
	c, cerr := r.Cookie(guestCookieName(shareID))
	if cerr != nil || c.Value == "" {
		return share.Guest{}, "", shareReasonNoSession, nil
	}
	g, err = s.shares().GuestFor(r.Context(), shareID, c.Value)
	if errors.Is(err, share.ErrNotFound) {
		return share.Guest{}, "", shareReasonForeignSession, nil
	}
	if err != nil {
		return share.Guest{}, "", "", err
	}
	return g, c.Value, "", nil
}

// slideGuest extends the session to now()+30m and re-sets the cookie, so 30
// minutes means 30 minutes of inactivity rather than of visit -- what makes
// the TTL survivable next to a 1 h presign. Best effort on purpose: the URL
// was already earned, and a session that lapsed in the same instant only
// means the next request re-asks.
func (s *Server) slideGuest(w http.ResponseWriter, r *http.Request, shareID uuid.UUID, g share.Guest, raw string) {
	if _, err := s.shares().ReuseGuest(r.Context(), g.ID); err != nil {
		if !errors.Is(err, share.ErrNotFound) {
			LoggerFrom(r.Context()).Warn("sliding a guest session", "error", err, "share_id", shareID)
		}
		return
	}
	s.setGuestCookie(w, shareID, raw)
}

// mintOrReuseGuest answers /session and a passed password gate, idempotent
// per browser: a live session presented back is slid and re-set -- no row
// inserted, no view logged -- so a reload is not a second visitor. Only a
// browser with none mints, which is what keeps the view log at one row per
// gate pass and an unauthenticated caller's writes bounded.
func (s *Server) mintOrReuseGuest(w http.ResponseWriter, r *http.Request, shareID uuid.UUID) {
	g, raw, reason, err := s.guestFrom(r, shareID)
	if err != nil {
		s.shareFailed(w, r, "reading the guest session", err)
		return
	}
	if reason == "" {
		_, err := s.shares().ReuseGuest(r.Context(), g.ID)
		switch {
		case err == nil:
			s.setGuestCookie(w, shareID, raw)
			w.WriteHeader(http.StatusNoContent)
			return
		case !errors.Is(err, share.ErrNotFound):
			s.shareFailed(w, r, "sliding the guest session", err)
			return
		}
		// The row lapsed between the read and the slide; mint a fresh one.
	}
	fresh, _, err := s.shares().MintGuest(r.Context(), shareID)
	if err != nil {
		s.shareFailed(w, r, "minting a guest session", err)
		return
	}
	s.setGuestCookie(w, shareID, fresh)
	s.shareAccess(r, shareID, share.ActionView)
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------- routes --

// shareMetaResponse is GET /meta's body: what the page shows before any
// gate. preview is the server's answer for this file on a share page -- the
// allowlist minus PDF -- and the two per-browser answers: session, whether
// this browser already holds a live guest session of this share (a reload
// of a gated page skips the gate), and exhausted, the cap's state for THIS
// browser, so the person who legitimately spent the one allowed download is
// not locked out of their own page by a reload.
type shareMetaResponse struct {
	Name             string     `json:"name"`
	Size             int64      `json:"size"`
	Mime             string     `json:"mime"`
	RequiresPassword bool       `json:"requires_password"`
	ExpiresAt        *time.Time `json:"expires_at"`
	Exhausted        bool       `json:"exhausted"`
	Session          bool       `json:"session"`
	Preview          bool       `json:"preview"`
}

// GET /api/s/{token}/meta. It never sets a cookie, never slides one and
// never logs a view -- the page's own load must cost the visitor nothing --
// but it may READ the guest cookie, because session and exhausted are
// per-browser answers. One lookup serves both: session is the cookie
// resolving to a live session of this share, not the cookie being there.
func (s *Server) shareMeta(w http.ResponseWriter, r *http.Request) {
	res, ok := s.liveShare(w, r)
	if !ok {
		return
	}
	g, _, reason, err := s.guestFrom(r, res.ShareID)
	if err != nil {
		s.shareFailed(w, r, "reading the guest session", err)
		return
	}
	session := reason == ""
	exhausted := res.MaxDownloads != nil && res.DownloadCount >= *res.MaxDownloads
	if exhausted && session && g.DownloadedAt != nil {
		exhausted = false
	}
	_, preview := upload.SharePreviewContentType(res.Mime)
	WriteJSON(w, http.StatusOK, shareMetaResponse{
		Name:             res.Name,
		Size:             res.Size,
		Mime:             res.Mime,
		RequiresPassword: res.PasswordHash != nil,
		ExpiresAt:        res.ExpiresAt,
		Exhausted:        exhausted,
		Session:          session,
		Preview:          preview,
	})
}

// POST /api/s/{token}/session -- passwordless shares only; a password share
// mints through /password, and answers 401 here so the gate cannot be walked
// around.
func (s *Server) shareSession(w http.ResponseWriter, r *http.Request) {
	res, ok := s.liveShare(w, r)
	if !ok {
		return
	}
	if res.PasswordHash != nil {
		WriteErr(w, r, http.StatusUnauthorized, CodeUnauthorized, "this link asks for its password")
		return
	}
	s.mintOrReuseGuest(w, r, res.ShareID)
}

// POST /api/s/{token}/password. Two durable budgets are consulted before
// Argon2 runs -- a locked caller must not cost a hash -- and a wrong guess
// charges both. share_id:ip is the recipient's: keyed on the share alone,
// anyone holding a public link could lock its real recipient out with ten
// wrong passwords. share_id alone is the ceiling: without it a guesser
// rotating addresses meets no per-link bound at all. A right guess clears
// neither -- the windows lapse on their own.
func (s *Server) sharePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "expected {password}")
		return
	}
	res, ok := s.liveShare(w, r)
	if !ok {
		return
	}
	if res.PasswordHash == nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "this link has no password")
		return
	}

	ctx := r.Context()
	budgets := []struct {
		scope, key string
		limit      int
	}{
		{auth.ScopeSharePasswordShare, res.ShareID.String(), auth.SharePasswordShareFailLimit},
		{auth.ScopeSharePassword, res.ShareID.String() + ":" + ClientIP(r), auth.SharePasswordFailLimit},
	}
	for _, b := range budgets {
		allowed, err := auth.Allowed(ctx, s.DB, b.scope, b.key, b.limit, auth.SharePasswordFailWindow)
		if err != nil {
			s.shareFailed(w, r, "checking the password budget", err)
			return
		}
		if !allowed {
			shareRefused(r, &res.ShareID, shareReasonLocked)
			s.shareAccess(r, res.ShareID, share.ActionDenied)
			WriteErr(w, r, http.StatusTooManyRequests, CodeRateLimited,
				"too many attempts. Try again in a few minutes.")
			return
		}
	}

	right, err := s.Argon2.Verify(*res.PasswordHash, req.Password)
	if errors.Is(err, auth.ErrBusy) {
		// authBusy's answer through this group's own line: that helper logs
		// r.URL.Path, and this path carries the credential.
		shareRefused(r, &res.ShareID, shareReasonBusy)
		WriteErr(w, r, http.StatusTooManyRequests, CodeRateLimited,
			"we are busy right now. Try again in a moment.")
		return
	}
	if err != nil {
		s.shareFailed(w, r, "verifying a share password", err)
		return
	}
	if !right {
		for _, b := range budgets {
			if _, err := auth.Bump(ctx, s.DB, b.scope, b.key, auth.SharePasswordFailWindow); err != nil {
				LoggerFrom(ctx).Error("charging the share password budget", "error", err, "scope", b.scope)
			}
		}
		shareRefused(r, &res.ShareID, shareReasonBadPassword)
		s.shareAccess(r, res.ShareID, share.ActionDenied)
		WriteErr(w, r, http.StatusUnauthorized, CodeUnauthorized, "that password is not right")
		return
	}
	s.mintOrReuseGuest(w, r, res.ShareID)
}

// shareRedirect is a 302 with no body and no Cache-Control write of its own:
// the group's shareHeaders already marked every answer here private,
// no-store, and a body would carry the URL onward.
func shareRedirect(w http.ResponseWriter, to string) {
	w.Header().Set("Location", to)
	w.WriteHeader(http.StatusFound)
}

// GET /api/s/{token}/download -- a browser navigation, so every refusal is a
// redirect back to the page, never JSON: a person following a link cannot
// act on an error envelope. It cannot carry X-Drive-Client either, and
// relies on SameSite=Lax plus once-per-session counting: a cross-site click
// can spend a slot the victim's own session would have spent anyway --
// accepted, and named in the protocol so nobody re-derives it.
func (s *Server) shareDownload(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	back := func(reason string) {
		shareRedirect(w, "/s/"+url.PathEscape(token)+"?reason="+reason)
	}

	res, err := s.shares().Resolve(r.Context(), auth.HashToken(token))
	switch {
	case errors.Is(err, share.ErrNotFound):
		shareRefused(r, nil, shareReasonNotFound)
		back("gone")
		return
	case err != nil:
		s.shareFailed(w, r, "resolving a link", err)
		return
	case res.State != share.StateLive:
		shareRefused(r, &res.ShareID, string(res.State))
		s.shareAccess(r, res.ShareID, share.ActionDenied)
		back("gone")
		return
	}

	g, raw, reason, err := s.guestFrom(r, res.ShareID)
	if err != nil {
		s.shareFailed(w, r, "reading the guest session", err)
		return
	}
	if reason != "" {
		shareRefused(r, &res.ShareID, reason)
		back("session")
		return
	}

	exhausted, err := s.shares().CountOnce(r.Context(), g.ID, res.ShareID)
	if errors.Is(err, share.ErrNotFound) {
		// The row went between guestFrom and the count's lock -- a revoke,
		// a regenerate, a password change or the sweep -- so there is no
		// session to count against and nothing is issued.
		shareRefused(r, &res.ShareID, shareReasonForeignSession)
		back("session")
		return
	}
	if err != nil {
		s.shareFailed(w, r, "counting the download", err)
		return
	}
	if exhausted {
		shareRefused(r, &res.ShareID, shareReasonExhausted)
		s.shareAccess(r, res.ShareID, share.ActionDenied)
		back("exhausted")
		return
	}

	signed, err := s.presigner().GetURL(r.Context(), res.ObjectKey, res.Name)
	if err != nil {
		s.shareFailed(w, r, "presigning a share download", err)
		return
	}
	s.shareAccess(r, res.ShareID, share.ActionDownload)
	s.slideGuest(w, r, res.ShareID, g, raw)
	LoggerFrom(r.Context()).Info("share download issued", "share_id", res.ShareID, "size", res.Size)
	shareRedirect(w, signed.URL)
}

// GET /api/s/{token}/preview -- an XHR, so refusals are JSON. It requires
// X-Drive-Client explicitly: RequireClientHeader exempts GETs, and this is a
// state-touching GET (it issues a URL and slides the session), so the gate
// is enforced here at no cost -- the SPA sends the header on GETs anyway.
// It never increments the cap, but a spent link refuses any browser that has
// not itself counted, so a dead budget cannot go on handing out bytes.
func (s *Server) sharePreview(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(ClientHeader) == "" {
		WriteErr(w, r, http.StatusForbidden, CodeInvalid, "missing X-Drive-Client header")
		return
	}
	res, ok := s.liveShare(w, r)
	if !ok {
		return
	}
	g, raw, reason, err := s.guestFrom(r, res.ShareID)
	if err != nil {
		s.shareFailed(w, r, "reading the guest session", err)
		return
	}
	if reason != "" {
		shareRefused(r, &res.ShareID, reason)
		WriteErr(w, r, http.StatusUnauthorized, CodeUnauthorized, "no live session for this link")
		return
	}
	// res.Mime is the uploader's claim and only ever a lookup key; what gets
	// signed is the allowlist's own constant, and a share page's allowlist
	// additionally refuses PDF -- a stranger's document in an unsandboxed
	// frame is not the bargain the owner's own preview accepted.
	contentType, previewable := upload.SharePreviewContentType(res.Mime)
	if !previewable {
		shareRefused(r, &res.ShareID, shareReasonUnsupportedPreview)
		WriteErr(w, r, http.StatusUnsupportedMediaType, CodeUnsupported, "no preview for this file type")
		return
	}
	if res.MaxDownloads != nil && res.DownloadCount >= *res.MaxDownloads && g.DownloadedAt == nil {
		shareRefused(r, &res.ShareID, shareReasonExhausted)
		s.shareAccess(r, res.ShareID, share.ActionDenied)
		WriteErr(w, r, http.StatusForbidden, CodeExhausted, "this link has reached its download limit")
		return
	}
	signed, err := s.presigner().PreviewURL(r.Context(), res.ObjectKey, res.Name, contentType)
	if err != nil {
		s.shareFailed(w, r, "presigning a share preview", err)
		return
	}
	s.shareAccess(r, res.ShareID, share.ActionDownload)
	s.slideGuest(w, r, res.ShareID, g, raw)
	WriteJSON(w, http.StatusOK, previewLink{URL: signed.URL, ExpiresAt: signed.ExpiresAt, Mime: contentType})
}
