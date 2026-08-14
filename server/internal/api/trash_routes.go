package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/node"
)

// Trash, restore and purge. The routes are registered flat rather than through
// r.Route("/nodes/{id}"), so they compose with the node handlers that
// node_routes.go registers into the same /api mux.
func (s *Server) mountTrash(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.RequireAuth)

		r.Get("/trash", s.listTrash)
		r.Delete("/nodes/{id}", s.trashNode)
		r.Post("/nodes/{id}/restore", s.restoreNode)
		r.Delete("/nodes/{id}/purge", s.purgeNode)
	})
}

// itemDTO maps a node row onto the canonical wire shape. Folders keep size and
// mime null; NodeDTO's pointer fields carry that through.
func itemDTO(n node.Node) NodeDTO {
	return NodeDTO{
		ID:          n.ID,
		ParentID:    n.ParentID,
		Kind:        n.Kind,
		Name:        n.Name,
		Size:        n.Size,
		Mime:        n.Mime,
		CreatedAt:   n.CreatedAt,
		UpdatedAt:   n.UpdatedAt,
		TrashedRoot: n.TrashedRoot,
	}
}

// itemDTOs maps a page of rows.
func itemDTOs(nodes []node.Node) []NodeDTO {
	out := make([]NodeDTO, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, itemDTO(n))
	}
	return out
}

// trashPathID reads the {id} path parameter as a uuid. A malformed id is
// reported as not found rather than invalid: node ids are opaque, and telling
// "not a uuid" apart from "not yours" leaks nothing worth leaking.
func trashPathID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	return id, err == nil
}

// writeTrashErr turns the node package's sentinels into the canonical envelope.
func (s *Server) writeTrashErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, node.ErrNotFound):
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such node")
	case errors.Is(err, node.ErrRootNode):
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid,
			"the root folder cannot be trashed, restored or purged")
	case errors.Is(err, node.ErrNameConflict):
		// Restore picks a free name and then writes, so a concurrent create or
		// upload-publish can take that name in between. The loser gets the
		// canonical retryable 409 rather than a 500.
		WriteErr(w, r, http.StatusConflict, CodeNameConflict,
			"a node with that name already exists in the destination")
	default:
		LoggerFrom(r.Context()).Error("trash operation failed", "error", err)
		WriteErr(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
	}
}

// GET /api/trash -- the trashed roots, most recent deletion first.
func (s *Server) listTrash(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())

	rawCursor, limit, err := Page(r)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}
	var cur *node.TrashCursor
	if rawCursor != "" {
		var c node.TrashCursor
		if err := DecodeCursor(rawCursor, &c); err != nil {
			WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "invalid cursor")
			return
		}
		cur = &c
	}

	items, next, err := node.NewStore(s.DB).ListTrash(r.Context(), u.ID, cur, limit)
	if err != nil {
		s.writeTrashErr(w, r, err)
		return
	}

	encoded := ""
	if next != nil {
		encoded = EncodeCursor(next)
	}
	WriteJSON(w, http.StatusOK, NewList(itemDTOs(items), encoded))
}

// DELETE /api/nodes/{id} -- move a node and its subtree to the trash.
func (s *Server) trashNode(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())
	id, ok := trashPathID(r)
	if !ok {
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such node")
		return
	}
	if err := node.NewStore(s.DB).Trash(r.Context(), u.ID, id); err != nil {
		s.writeTrashErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/nodes/{id}/restore -- bring a trashed root back, auto-renaming on
// a name collision and re-homing to the user's root when the original parent is
// gone. The response is the restored node, so the client sees the final name
// and parent.
func (s *Server) restoreNode(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())
	id, ok := trashPathID(r)
	if !ok {
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such node")
		return
	}
	restored, err := node.NewStore(s.DB).Restore(r.Context(), u.ID, id)
	if err != nil {
		s.writeTrashErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, itemDTO(restored))
}

// DELETE /api/nodes/{id}/purge -- permanent deletion. The blob objects survive
// until the GC sweep collects them at refcount 0.
func (s *Server) purgeNode(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())
	id, ok := trashPathID(r)
	if !ok {
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such node")
		return
	}
	if err := node.NewStore(s.DB).Purge(r.Context(), u.ID, id); err != nil {
		s.writeTrashErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
