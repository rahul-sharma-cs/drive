package api

// The upload session API: create, status, resume handshake, part confirmation,
// list and cancel. Complete lives with the finalize path.
//
// Two things about this file are load-bearing.
//
// The order inside create is the protocol. Destination authorization, then the
// active-session match, and only then the name-conflict check -- a matched
// session is a resume, and resumes must not be turned away by a collision the
// user already answered for, nor duplicated into a second multipart upload.
//
// No URL leaves here without the server being sure which file it is for. A
// create that matched a session with confirmed parts, and a resume whose
// reconciliation found drift, both arm the chimera guard: the response carries
// the pinned part numbers and an empty URL list until the client proves, with
// MD5s, that the file it is holding is the file those parts came from.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/node"
	"github.com/rahul-sharma-cs/drive/server/internal/upload"
)

// mountUploads registers the upload surface. Every route is cookie-authed and
// ownership-checked; POST /uploads/{id}/complete is registered by the finalize
// path, not here.
func (s *Server) mountUploads(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.RequireAuth)

		r.Post("/uploads", s.createUpload)
		r.Get("/uploads", s.listUploads)
		r.Get("/uploads/{id}", s.getUpload)
		r.Post("/uploads/{id}/resume", s.resumeUpload)
		r.Post("/uploads/{id}/parts/{n}", s.confirmPart)
		r.Delete("/uploads/{id}", s.deleteUpload)
	})
}

func (s *Server) uploads() *upload.Store { return upload.NewStore(s.DB) }

// withinCapacity refuses a new upload that would take the service, or this
// user, past its byte cap. It writes the refusal itself and reports false.
//
// The service-wide cap is the real cost control: the object store bills for
// what it holds and offers no spend limit of its own, so nothing but this
// stands between a stranger with a script and an unbounded invoice. The
// per-user quota is fairness, and is the one of the two that may be cut.
func (s *Server) withinCapacity(w http.ResponseWriter, r *http.Request, store *upload.Store, ownerID uuid.UUID, size int64) bool {
	limit, quota := s.Cfg.StorageCap, s.Cfg.UserQuota
	if limit <= 0 && quota <= 0 {
		return true
	}

	used, err := store.Usage(r.Context(), ownerID)
	if err != nil {
		writeUploadErr(w, r, err)
		return false
	}

	if limit > 0 && used.Total()+size > limit {
		LoggerFrom(r.Context()).Warn("upload refused: the service is at its storage cap",
			"stored", used.Stored, "in_flight", used.InFlight, "requested", size, "cap", limit)
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid,
			"Drive is out of storage space. Try again once something has been deleted.")
		return false
	}
	if quota > 0 && used.User+size > quota {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid,
			fmt.Sprintf("this upload would take you past your %s of storage. Empty the trash or delete something first.",
				humanBytes(quota)))
		return false
	}
	return true
}

// humanBytes renders a byte count the way a limit should read in a message to a
// person: whole units, no more precision than the number deserves.
func humanBytes(n int64) string {
	switch {
	case n >= config.GiB:
		return fmt.Sprintf("%.3g GiB", float64(n)/float64(config.GiB))
	case n >= config.MiB:
		return fmt.Sprintf("%.3g MiB", float64(n)/float64(config.MiB))
	case n >= config.KiB:
		return fmt.Sprintf("%.3g KiB", float64(n)/float64(config.KiB))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func (s *Server) presigner() *upload.Presigner {
	return &upload.Presigner{
		S3:      s.S3,
		Presign: s.Presign,
		Bucket:  s.Cfg.S3Bucket,
		TTL:     s.Cfg.PresignTTL,
	}
}

// ------------------------------------------------------------------- shapes --

// uploadStatus is the status shape the whole contract is phrased in: every
// upload response is this, sometimes with URLs beside it. It deliberately
// carries no verify_parts -- status is the shape "minus URLs and verify".
type uploadStatus struct {
	UploadID         uuid.UUID  `json:"upload_id"`
	Mode             string     `json:"mode"`
	FileName         string     `json:"file_name"`
	FileSize         int64      `json:"file_size"`
	PartSize         int64      `json:"part_size"`
	PartsTotal       int        `json:"parts_total"`
	Fingerprint      string     `json:"fingerprint"`
	ParentID         *uuid.UUID `json:"parent_id"`
	Status           string     `json:"status"`
	ConfirmedParts   []int      `json:"confirmed_parts"`
	NodeID           *uuid.UUID `json:"node_id,omitempty"`
	SessionExpiresAt time.Time  `json:"session_expires_at"`
}

// createResponse is the status shape plus the first few missing parts' URLs --
// or, when the match armed verification, an empty list and the pins.
type createResponse struct {
	uploadStatus
	Presigned   []upload.PresignedPart `json:"presigned"`
	VerifyParts []int                  `json:"verify_parts,omitempty"`
}

// resumeResponse is the status shape plus a URL for every missing part. When
// verification is armed, missing is absent entirely rather than empty: an empty
// list would read as "nothing left to upload".
type resumeResponse struct {
	uploadStatus
	Missing     *[]upload.PresignedPart `json:"missing,omitempty"`
	VerifyParts []int                   `json:"verify_parts,omitempty"`
}

func statusOf(sess *upload.Session, parts []upload.Part, expiresAt time.Time) uploadStatus {
	confirmed := upload.PartNumbers(parts)
	if confirmed == nil {
		confirmed = []int{}
	}
	return uploadStatus{
		UploadID:         sess.ID,
		Mode:             sess.Mode,
		FileName:         sess.FileName,
		FileSize:         sess.FileSize,
		PartSize:         sess.PartSize,
		PartsTotal:       sess.PartsTotal,
		Fingerprint:      sess.Fingerprint,
		ParentID:         sess.ParentID,
		Status:           sess.Status,
		ConfirmedParts:   confirmed,
		NodeID:           sess.NodeID,
		SessionExpiresAt: expiresAt,
	}
}

// ------------------------------------------------------------------ helpers --

// uploadPathID reads {id}. An unparseable id names no session, and saying so
// any more precisely would be enumeration signal.
func uploadPathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such upload")
		return uuid.Nil, false
	}
	return id, true
}

// loadUpload resolves the path id to a session the caller owns. Someone else's
// upload_id is a 404: an upload_id is an identifier, never a credential.
func (s *Server) loadUpload(w http.ResponseWriter, r *http.Request) (*upload.Session, bool) {
	id, ok := uploadPathID(w, r)
	if !ok {
		return nil, false
	}
	sess, err := s.uploads().Get(r.Context(), MustUser(r.Context()).ID, id)
	if err != nil {
		writeUploadErr(w, r, err)
		return nil, false
	}
	return sess, true
}

// writeUploadErr maps the upload package's sentinels onto the canonical codes.
func writeUploadErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, upload.ErrNotFound):
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such upload")
	case errors.Is(err, upload.ErrExpired):
		WriteErr(w, r, http.StatusGone, CodeSessionExpired, "this upload session has expired; start a fresh upload")
	case errors.Is(err, upload.ErrTooLarge):
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "file is too large to upload")
	case errors.Is(err, upload.ErrInvalid):
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "invalid upload request")
	default:
		LoggerFrom(r.Context()).Error("upload operation failed", "error", err)
		WriteErr(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
	}
}

// requireLive refuses a session that can no longer take parts. Expiry is a 410
// so the client discards its record and creates a fresh session; a session
// mid-finalize is a 409 so it polls instead.
func requireLive(w http.ResponseWriter, r *http.Request, sess *upload.Session) bool {
	if sess.Expired() {
		writeUploadErr(w, r, upload.ErrExpired)
		return false
	}
	if sess.Status != upload.StatusActive {
		WriteErr(w, r, http.StatusConflict, CodeInProgress, "this upload is being finalized")
		return false
	}
	return true
}

// readJSONOptional decodes a body that may legitimately be empty -- the resume
// handshake is usually called with nothing in it.
func readJSONOptional(r *http.Request, dst any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		return ErrBadJSON
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return ErrBadJSON
	}
	return nil
}

// ------------------------------------------------------------------- create --

type createUploadReq struct {
	FileName       string     `json:"file_name"`
	FileSize       int64      `json:"file_size"`
	Mime           string     `json:"mime"`
	ParentID       *uuid.UUID `json:"parent_id"`
	Fingerprint    string     `json:"fingerprint"`
	ConflictPolicy string     `json:"conflict_policy"`
}

// maxMime bounds the client-declared MIME we store. It is never trusted for
// serving -- downloads force application/octet-stream -- so length is the only
// thing worth checking.
const maxMime = 255

// POST /api/uploads
func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := MustUser(ctx)

	var req createUploadReq
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "malformed request body")
		return
	}
	if req.ParentID == nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "parent_id is required")
		return
	}
	name, err := node.Clean(req.FileName)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}
	fingerprint, err := upload.CleanFingerprint(req.Fingerprint)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "fingerprint is required")
		return
	}
	if req.FileSize < 0 {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "file_size must not be negative")
		return
	}
	if maxFile := s.Cfg.MaxFileSize; maxFile > 0 && req.FileSize > maxFile {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid,
			fmt.Sprintf("this file is %s; the limit is %s", humanBytes(req.FileSize), humanBytes(maxFile)))
		return
	}
	// reuse is the folder vocabulary; a colliding file is answered with
	// replace or rename, or with no policy at all and a prompt.
	policy := strings.TrimSpace(req.ConflictPolicy)
	if policy != "" && policy != node.PolicyReplace && policy != node.PolicyRename {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "conflict_policy must be replace or rename")
		return
	}
	mime := strings.TrimSpace(req.Mime)
	if len(mime) > maxMime {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "mime is too long")
		return
	}

	// 1. The destination is authorized independently of everything else:
	// exists, is a folder, is not trashed, belongs to the caller -- else 404.
	parent, err := s.nodes().Get(ctx, user.ID, *req.ParentID)
	if err != nil || parent.Kind != node.KindFolder {
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such folder")
		return
	}

	store := s.uploads()

	// 2. A match resumes; it never duplicates. This runs before the
	// name-conflict check on purpose.
	match, err := store.MatchActive(ctx, user.ID, *req.ParentID, fingerprint)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}
	if match != nil && match.Expired() {
		// The row is still 'active' but past its deadline, so it holds the
		// unique index hostage while being useless. Retire it and create anew;
		// GC would eventually do the same.
		s.retireExpired(r, store, user.ID, match)
		match = nil
	}
	if match != nil {
		s.writeMatched(w, r, store, match, policy, http.StatusOK)
		return
	}

	// 2b. Capacity, on the create-new path only.
	//
	// A resume never comes through here, and that is deliberate: its bytes are
	// already stored and already counted, so refusing it would strand them --
	// the upload could neither finish nor free anything. Only new volume is
	// refused.
	if !s.withinCapacity(w, r, store, user.ID, req.FileSize) {
		return
	}

	// 3. Only the create-new path checks for a name conflict. With a policy in
	// hand the collision is already answered; complete re-checks it anyway,
	// because a 50 GB upload outlives the answer.
	if policy == "" {
		taken, err := store.NameTaken(ctx, *req.ParentID, name)
		if err != nil {
			writeUploadErr(w, r, err)
			return
		}
		if taken {
			WriteErr(w, r, http.StatusConflict, CodeNameConflict, "a file with that name already exists here")
			return
		}
	}

	// 4. Part size, grown if the file would otherwise need more than 10,000
	// parts.
	partSize, partsTotal, err := upload.ResolvePartSize(req.FileSize, s.Cfg.PartSize)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}

	// 5. The multipart upload -- except for a 0-byte file, which never opens
	// one: Garage rejects a complete with an empty part list, so that case is
	// a single PutObject at finalize time.
	presigner := s.presigner()
	key := upload.NewObjectKey()
	var s3UploadID *string
	if req.FileSize > 0 {
		id, err := presigner.CreateMultipart(ctx, key)
		if err != nil {
			writeUploadErr(w, r, err)
			return
		}
		s3UploadID = &id
	}

	// 6. The session row.
	sess := &upload.Session{
		ID:          uuid.New(),
		UserID:      user.ID,
		ParentID:    req.ParentID,
		FileName:    name,
		FileSize:    req.FileSize,
		Fingerprint: fingerprint,
		S3UploadID:  s3UploadID,
		ObjectKey:   key,
		PartSize:    partSize,
		PartsTotal:  partsTotal,
		Mode:        upload.ModeDirect,
	}
	if mime != "" {
		sess.Mime = &mime
	}
	if policy != "" {
		sess.ConflictPolicy = &policy
	}

	if err := store.Insert(ctx, sess); err != nil {
		if !errors.Is(err, upload.ErrRace) {
			writeUploadErr(w, r, err)
			return
		}
		// Two tabs created at once and the other one won. Give back the
		// multipart upload we just opened and return the winning session --
		// the client gets a resume, not a second upload.
		if s3UploadID != nil {
			if err := presigner.AbortMultipart(ctx, key, *s3UploadID); err != nil {
				LoggerFrom(ctx).Warn("abandoning the losing multipart upload", "error", err, "object_key", key)
			}
		}
		winner, err := store.MatchActive(ctx, user.ID, *req.ParentID, fingerprint)
		if err != nil || winner == nil {
			LoggerFrom(ctx).Error("lost the create race but found no winner", "error", err)
			WriteErr(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
			return
		}
		s.writeMatched(w, r, store, winner, policy, http.StatusOK)
		return
	}

	LoggerFrom(ctx).Info("upload session create",
		"session_id", sess.ID, "user_id", user.ID, "parent_id", sess.ParentID,
		"file_size", sess.FileSize, "part_size", sess.PartSize, "parts_total", sess.PartsTotal,
		"matched", false)

	missing := upload.MissingParts(sess.PartsTotal, nil)
	presigned, err := s.presignSome(r, sess, missing, upload.PresignBatch)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, createResponse{
		uploadStatus: statusOf(sess, nil, sess.ExpiresAt),
		Presigned:    presigned,
	})
}

// retireExpired aborts a matched session that ran out its sliding expiry, so a
// fresh create can take its place under the unique index.
func (s *Server) retireExpired(r *http.Request, store *upload.Store, userID uuid.UUID, sess *upload.Session) {
	ctx := r.Context()
	before, err := store.Abort(ctx, userID, sess.ID)
	if err != nil {
		LoggerFrom(ctx).Warn("retiring an expired upload session", "error", err, "session_id", sess.ID)
		return
	}
	if before.Status == upload.StatusActive && before.S3UploadID != nil {
		if err := s.presigner().AbortMultipart(ctx, before.ObjectKey, *before.S3UploadID); err != nil {
			LoggerFrom(ctx).Warn("aborting an expired session's multipart upload",
				"error", err, "session_id", sess.ID)
		}
	}
}

// writeMatched answers a create that found an existing session.
//
// If the session already holds confirmed parts, this is a re-selection: the
// server cannot tell whether the file the user just picked is the same file,
// so it arms the chimera guard and returns no URLs at all. The client clears
// it through the resume handshake.
func (s *Server) writeMatched(w http.ResponseWriter, r *http.Request, store *upload.Store, sess *upload.Session, policy string, status int) {
	ctx := r.Context()

	if policy != "" && (sess.ConflictPolicy == nil || *sess.ConflictPolicy != policy) {
		if err := store.SetConflictPolicy(ctx, sess.ID, policy); err != nil {
			writeUploadErr(w, r, err)
			return
		}
		sess.ConflictPolicy = &policy
	}

	parts, err := store.Parts(ctx, sess.ID)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}
	// COALESCE inside Arm keeps an already-armed pair where it is; passing the
	// freshly computed pins only arms a session that was not armed before.
	armed, err := store.Arm(ctx, sess.ID, upload.PinnedParts(parts))
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}
	expires, err := store.Touch(ctx, sess.ID)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}

	LoggerFrom(ctx).Info("upload session create",
		"session_id", sess.ID, "user_id", sess.UserID, "parent_id", sess.ParentID,
		"file_size", sess.FileSize, "part_size", sess.PartSize, "parts_total", sess.PartsTotal,
		"matched", true, "confirmed", len(parts), "verify_parts", armed)

	resp := createResponse{
		uploadStatus: statusOf(sess, parts, expires),
		Presigned:    []upload.PresignedPart{},
	}
	if len(armed) > 0 {
		resp.VerifyParts = armed
		WriteJSON(w, status, resp)
		return
	}

	missing := upload.MissingParts(sess.PartsTotal, parts)
	presigned, err := s.presignSome(r, sess, missing, upload.PresignBatch)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}
	resp.Presigned = presigned
	WriteJSON(w, status, resp)
}

// presignSome signs URLs for at most limit of the missing parts.
func (s *Server) presignSome(r *http.Request, sess *upload.Session, missing []int, limit int) ([]upload.PresignedPart, error) {
	if sess.S3UploadID == nil || len(missing) == 0 {
		return []upload.PresignedPart{}, nil
	}
	if limit > 0 && len(missing) > limit {
		missing = missing[:limit]
	}
	return s.presigner().PartURLs(r.Context(), sess.ObjectKey, *sess.S3UploadID, missing)
}

// ------------------------------------------------------------------- status --

// GET /api/uploads/{id}
func (s *Server) getUpload(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	if sess.Expired() {
		writeUploadErr(w, r, upload.ErrExpired)
		return
	}

	store := s.uploads()
	parts, err := store.Parts(r.Context(), sess.ID)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}
	expires, err := store.Touch(r.Context(), sess.ID)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, statusOf(sess, parts, expires))
}

// GET /api/uploads
func (s *Server) listUploads(w http.ResponseWriter, r *http.Request) {
	cursor, limit, err := Page(r)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}
	var after *upload.ListCursor
	if cursor != "" {
		var c upload.ListCursor
		if err := DecodeCursor(cursor, &c); err != nil {
			WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "cursor: not one of ours")
			return
		}
		after = &c
	}

	store := s.uploads()
	sessions, next, err := store.List(r.Context(), MustUser(r.Context()).ID, after, limit)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}

	items := make([]uploadStatus, 0, len(sessions))
	for i := range sessions {
		parts, err := store.Parts(r.Context(), sessions[i].ID)
		if err != nil {
			writeUploadErr(w, r, err)
			return
		}
		items = append(items, statusOf(&sessions[i], parts, sessions[i].ExpiresAt))
	}

	var nextCursor string
	if next != nil {
		nextCursor = EncodeCursor(next)
	}
	WriteJSON(w, http.StatusOK, NewList(items, nextCursor))
}

// ------------------------------------------------------------------- resume --

type resumeReq struct {
	PartMD5s map[string]string `json:"part_md5s"`
}

// POST /api/uploads/{id}/resume
//
// The handshake does three things in order: reconcile the ledger against what
// Garage actually holds, settle the chimera guard, and hand back a fresh URL
// for every missing part. It is also the client's URL-refill path, called with
// no error in sight, which is why a refill must never be interrupted by a
// verification bounce that normal in-tab progress armed.
func (s *Server) resumeUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	sess, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	if sess.Expired() {
		writeUploadErr(w, r, upload.ErrExpired)
		return
	}
	var req resumeReq
	if err := readJSONOptional(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "malformed request body")
		return
	}

	store := s.uploads()

	// A session mid-finalize or already published has nothing to reconcile:
	// once CompleteMultipartUpload succeeds the multipart ceases to exist.
	if sess.Status != upload.StatusActive {
		parts, err := store.Parts(ctx, sess.ID)
		if err != nil {
			writeUploadErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, resumeResponse{uploadStatus: statusOf(sess, parts, sess.ExpiresAt)})
		return
	}

	// The handshake is an authenticated touch, so the sliding expiry moves
	// before anything else can return -- including the verification bounce,
	// which is otherwise a round trip that costs the client a day of headroom.
	expires, err := store.Touch(ctx, sess.ID)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}

	armed := sess.VerifyParts
	if sess.S3UploadID != nil {
		remote, err := upload.ListAllParts(ctx, s.S3, s.Cfg.S3Bucket, sess.ObjectKey, *sess.S3UploadID)
		if err != nil {
			// The multipart is gone -- GC or an operator aborted it -- so the
			// session cannot be resumed. Say so in the one code the client
			// already knows how to act on. The check has to be the finalize
			// path's: ListParts's deserializer never produces the typed
			// *types.NoSuchUpload, only a generic API error carrying the code,
			// so a typed-only check here is dead code and answers 500.
			if upload.IsNoSuchUpload(err) {
				writeUploadErr(w, r, upload.ErrExpired)
				return
			}
			writeUploadErr(w, r, err)
			return
		}
		// Only parts whose size makes this session's geometry add up may be
		// adopted. Garage stores a part at whatever size it is given -- a client
		// slicing at the environment default instead of the session's part_size
		// is the classic client mistake, and a PUT cut off
		// mid-body is the other one -- and adopting one counts it as confirmed
		// forever: it never appears in `missing` again, so the client is never
		// told to re-send it, while complete's total check fails on every
		// attempt. Filtering here leaves the bad part out of the ledger,
		// Reconcile's delete drops any stale row for it, and the client re-PUTs
		// over the same part number.
		usable := make([]upload.Part, 0, len(remote))
		for _, p := range remote {
			if sess.AdoptablePart(p.Number, p.Size) {
				usable = append(usable, p)
			}
		}
		if len(usable) < len(remote) {
			LoggerFrom(ctx).Warn("upload reconcile rejected remote parts",
				"session_id", sess.ID, "listed", len(remote), "usable", len(usable),
				"part_size", sess.PartSize, "parts_total", sess.PartsTotal)
		}
		adopted, dropped, err := store.Reconcile(ctx, sess.ID, usable)
		if err != nil {
			writeUploadErr(w, r, err)
			return
		}
		if len(adopted) > 0 || len(dropped) > 0 {
			LoggerFrom(ctx).Info("upload reconcile diff",
				"session_id", sess.ID, "listed", len(remote),
				"adopted", adopted, "dropped", dropped)
			// Drift means the ledger and Garage disagreed about which bytes
			// exist, so the file the client is holding gets verified before
			// any more of it is accepted.
			parts, err := store.Parts(ctx, sess.ID)
			if err != nil {
				writeUploadErr(w, r, err)
				return
			}
			if armed, err = store.Arm(ctx, sess.ID, upload.PinnedParts(parts)); err != nil {
				writeUploadErr(w, r, err)
				return
			}
		}
	}

	parts, err := store.Parts(ctx, sess.ID)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}

	if len(armed) > 0 {
		covered, ok := upload.VerifyPins(armed, parts, req.PartMD5s)
		if !covered {
			// The bounce: pins, no URLs. The client recomputes those parts'
			// MD5s from the file it just re-selected and calls again.
			WriteJSON(w, http.StatusOK, resumeResponse{
				uploadStatus: statusOf(sess, parts, expires),
				VerifyParts:  armed,
			})
			return
		}
		if !ok {
			LoggerFrom(ctx).Info("upload resume refused", "session_id", sess.ID, "verify_parts", armed)
			WriteErr(w, r, http.StatusConflict, CodeInvalid, "part verification failed")
			return
		}
		if err := store.ClearVerify(ctx, sess.ID); err != nil {
			writeUploadErr(w, r, err)
			return
		}
	}

	// Every missing part, not a batch: presigning is a local HMAC, and a client
	// that never runs out of URLs never stalls.
	presigned, err := s.presignSome(r, sess, upload.MissingParts(sess.PartsTotal, parts), 0)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, resumeResponse{
		uploadStatus: statusOf(sess, parts, expires),
		Missing:      &presigned,
	})
}

// ------------------------------------------------------------------ confirm --

type confirmPartReq struct {
	ETag string `json:"etag"`
	MD5  string `json:"md5"`
	Size int64  `json:"size"`
}

// POST /api/uploads/{id}/parts/{n}
func (s *Server) confirmPart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	sess, ok := s.loadUpload(w, r)
	if !ok {
		return
	}
	if !requireLive(w, r, sess) {
		return
	}

	n, err := strconv.Atoi(chi.URLParam(r, "n"))
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "part number must be an integer")
		return
	}
	var req confirmPartReq
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "malformed request body")
		return
	}
	// A wrong-sized part fails here, inside the client's retry budget, rather
	// than at complete when the whole file has already been transferred.
	if err := sess.CheckPart(n, req.Size); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid,
			"part "+strconv.Itoa(n)+" has the wrong size for this session")
		return
	}
	etag := upload.NormalizeETag(req.ETag)
	if etag == "" {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "etag is required")
		return
	}
	md5, err := upload.CleanMD5(req.MD5)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "md5 must be a 32-character hex digest")
		return
	}

	store := s.uploads()
	if err := store.ConfirmPart(ctx, sess.ID, upload.Part{
		Number: n, Size: req.Size, ETag: etag, MD5: &md5,
	}); err != nil {
		writeUploadErr(w, r, err)
		return
	}
	expires, err := store.Touch(ctx, sess.ID)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}

	LoggerFrom(ctx).Info("upload part confirm",
		"session_id", sess.ID, "part_number", n, "size", req.Size, "etag", etag)

	WriteJSON(w, http.StatusOK, map[string]any{
		"confirmed":          true,
		"session_expires_at": expires,
	})
}

// ------------------------------------------------------------------- cancel --

// DELETE /api/uploads/{id}
//
// Cancel takes the session's advisory lock, flips an active session to
// aborted, and discards the multipart upload. A session already done or
// aborted is a no-op; one mid-finalize is refused, because tearing down a
// multipart a finalizer is completing would lose the file. GC recovers a
// finalizer that died holding it.
func (s *Server) deleteUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, ok := uploadPathID(w, r)
	if !ok {
		return
	}
	before, err := s.uploads().Abort(ctx, MustUser(ctx).ID, id)
	if err != nil {
		writeUploadErr(w, r, err)
		return
	}

	switch before.Status {
	case upload.StatusCompleting:
		WriteErr(w, r, http.StatusConflict, CodeInProgress, "this upload is being finalized")
		return
	case upload.StatusActive:
		if before.S3UploadID != nil {
			if err := s.presigner().AbortMultipart(ctx, before.ObjectKey, *before.S3UploadID); err != nil {
				// The row is already aborted; GC's orphan sweep collects the
				// multipart. Losing the object is not worth failing a cancel.
				LoggerFrom(ctx).Warn("aborting a cancelled session's multipart upload",
					"error", err, "session_id", id)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
