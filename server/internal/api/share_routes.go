package api

// Share links, the owner's half: make one, list them, change one, stop one.
// Every route here is owner-scoped in SQL (created_by = caller), so another
// user's share id is 404 exactly like an unknown one. The public half -- the
// /api/s/{token}/* routes a recipient drives -- is registered inside
// mountShare's group in share_headers.go, which carries the headers and the
// bucket those routes need and these do not.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/node"
	"github.com/rahul-sharma-cs/drive/server/internal/share"
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
