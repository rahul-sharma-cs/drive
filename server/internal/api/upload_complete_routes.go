package api

// POST /api/uploads/{id}/complete -- the one upload endpoint that publishes.
//
// The handler is deliberately thin: parse, validate, call the finalizer, map
// its sentinels onto the canonical codes. Every decision worth making -- the
// advisory locking, the ledger verify, the NoSuchUpload recovery, the atomic
// publish -- lives in internal/upload's finalize path, because the GC loop has
// to make the same decisions with no request in sight.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/upload"
)

// mountUploadComplete registers the complete endpoint. It is separate from
// mountUploads only because the two halves of the upload surface were built in
// parallel and chi panics if a pattern is registered twice.
func (s *Server) mountUploadComplete(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Post("/uploads/{id}/complete", s.completeUpload)
	})
}

func (s *Server) finalizer() *upload.Finalizer {
	return upload.NewFinalizer(s.DB, s.S3, s.Cfg.S3Bucket, s.Log)
}

type completeUploadReq struct {
	SHA256 string `json:"sha256"`
}

// completeResponse is the contract's `{node_id, name}`, with parent_id present
// only when the file did not land where it was aimed: the destination folder was
// trashed or purged mid-upload and it went to the user's root instead.
type completeResponse struct {
	NodeID   uuid.UUID  `json:"node_id"`
	Name     string     `json:"name"`
	ParentID *uuid.UUID `json:"parent_id,omitempty"`
}

// POST /api/uploads/{id}/complete
func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, ok := uploadPathID(w, r)
	if !ok {
		return
	}
	var req completeUploadReq
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "malformed request body")
		return
	}
	// The client hashes the whole file anyway -- the empty file included, which
	// has a perfectly good digest -- so an absent or malformed one is a client
	// bug worth surfacing rather than a NULL worth storing.
	sum, err := hex.DecodeString(strings.TrimSpace(req.SHA256))
	if err != nil || len(sum) != sha256.Size {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid,
			"sha256 must be a 64-character hex digest of the whole file")
		return
	}

	res, err := s.finalizer().Complete(ctx, MustUser(ctx).ID, id, sum)
	if err != nil {
		writeFinalizeErr(w, r, err)
		return
	}

	resp := completeResponse{NodeID: res.NodeID, Name: res.Name}
	if res.Reparented {
		parent := res.ParentID
		resp.ParentID = &parent
	}
	WriteJSON(w, http.StatusOK, resp)
}

// writeFinalizeErr adds the finalize path's two sentinels to the upload error
// map. Both are states the client's state machine already knows: in_progress
// means poll, invalid on complete means re-handshake and re-send the parts the
// verify just deleted.
func writeFinalizeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, upload.ErrInProgress):
		WriteErr(w, r, http.StatusConflict, CodeInProgress, "this upload is being finalized")
	case errors.Is(err, upload.ErrVerify):
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid,
			"the uploaded parts do not match the store; re-handshake and re-send the missing parts")
	default:
		writeUploadErr(w, r, err)
	}
}
