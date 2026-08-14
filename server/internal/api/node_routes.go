package api

// Nodes: the file tree's CRUD surface. Every handler here re-checks ownership
// of the node in the path, and the three that take a destination folder in the
// body (create folder, move, copy) authorize that destination independently
// before anything is written. Both checks answer 404, never 403: node ids are
// opaque UUIDs and the API must not confirm that someone else's exists.

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/node"
)

func (s *Server) mountNodes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.RequireAuth)

		r.Get("/nodes/{id}", s.getNode)
		r.Get("/nodes/{id}/children", s.listChildren)
		r.Post("/folders", s.createFolder)
		r.Patch("/nodes/{id}", s.updateNode)
		r.Post("/nodes/{id}/copy", s.copyNode)
	})
	// DELETE /nodes/{id} (into the trash) is registered by mountTrash, next to
	// restore and purge, which is where the rest of the trash model lives.
}

// NodeDTOFrom converts a stored node to the canonical wire shape. Exported so
// every route file in this package shares one converter.
func NodeDTOFrom(n node.Node) NodeDTO {
	dto := NodeDTO{
		ID:          n.ID,
		ParentID:    n.ParentID,
		Kind:        n.Kind,
		Name:        n.Name,
		Mime:        n.Mime,
		CreatedAt:   n.CreatedAt,
		UpdatedAt:   n.UpdatedAt,
		TrashedRoot: n.TrashedRoot,
	}
	// Folders carry no size, and the DTO serializes that as null.
	if n.Kind == node.KindFile {
		dto.Size = n.Size
	}
	return dto
}

// WriteNodeErr maps the node package's sentinel errors onto the canonical
// codes. Exported so every route file in this package maps them identically.
func WriteNodeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, node.ErrNotFound):
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such node")
	case errors.Is(err, node.ErrNameConflict):
		WriteErr(w, r, http.StatusConflict, CodeNameConflict, err.Error())
	case errors.Is(err, node.ErrCycle):
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeCycle, err.Error())
	case errors.Is(err, node.ErrUnsupported):
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeUnsupported, err.Error())
	case errors.Is(err, node.ErrInvalidName),
		errors.Is(err, node.ErrInvalidPolicy),
		errors.Is(err, node.ErrRootNode):
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
	default:
		LoggerFrom(r.Context()).Error("node operation failed", "error", err)
		WriteErr(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
	}
}

// pathID reads the {id} path parameter. An unparseable id is answered as a
// miss rather than a validation error -- it names no resource, and saying so
// would be one more bit of enumeration signal.
func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such node")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) nodes() *node.Store { return node.NewStore(s.DB) }

// GET /api/nodes/{id}
func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	n, err := s.nodes().Get(r.Context(), MustUser(r.Context()).ID, id)
	if err != nil {
		WriteNodeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, NodeDTOFrom(n))
}

// GET /api/nodes/{id}/children
func (s *Server) listChildren(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	cursor, limit, err := Page(r)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}

	var after *node.ChildCursor
	if cursor != "" {
		var c node.ChildCursor
		if err := DecodeCursor(cursor, &c); err != nil {
			WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "cursor: not one of ours")
			return
		}
		after = &c
	}

	items, next, err := s.nodes().Children(r.Context(), MustUser(r.Context()).ID, id, after, limit)
	if err != nil {
		WriteNodeErr(w, r, err)
		return
	}

	dtos := make([]NodeDTO, 0, len(items))
	for _, n := range items {
		dtos = append(dtos, NodeDTOFrom(n))
	}
	var nextCursor string
	if next != nil {
		nextCursor = EncodeCursor(next)
	}
	WriteJSON(w, http.StatusOK, NewList(dtos, nextCursor))
}

type createFolderReq struct {
	ParentID       *uuid.UUID `json:"parent_id"`
	Name           string     `json:"name"`
	ConflictPolicy string     `json:"conflict_policy"`
}

// folderResponse is the node DTO plus the reuse marker: a folder drop that
// runs twice gets the same folder back the second time with existing=true.
type folderResponse struct {
	NodeDTO
	Existing bool `json:"existing,omitempty"`
}

// POST /api/folders
func (s *Server) createFolder(w http.ResponseWriter, r *http.Request) {
	var req createFolderReq
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "malformed request body")
		return
	}
	if req.ParentID == nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "parent_id is required")
		return
	}

	n, existing, err := s.nodes().CreateFolder(r.Context(), MustUser(r.Context()).ID, *req.ParentID, req.Name, req.ConflictPolicy)
	if err != nil {
		WriteNodeErr(w, r, err)
		return
	}

	status := http.StatusCreated
	if existing {
		status = http.StatusOK
	}
	WriteJSON(w, status, folderResponse{NodeDTO: NodeDTOFrom(n), Existing: existing})
}

type updateNodeReq struct {
	Name           *string    `json:"name"`
	ParentID       *uuid.UUID `json:"parent_id"`
	ConflictPolicy string     `json:"conflict_policy"`
}

// PATCH /api/nodes/{id} -- rename, move, or both.
func (s *Server) updateNode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req updateNodeReq
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "malformed request body")
		return
	}
	if req.Name == nil && req.ParentID == nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "name or parent_id is required")
		return
	}

	n, err := s.nodes().Update(r.Context(), MustUser(r.Context()).ID, id, req.Name, req.ParentID, req.ConflictPolicy)
	if err != nil {
		WriteNodeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, NodeDTOFrom(n))
}

type copyNodeReq struct {
	ParentID       *uuid.UUID `json:"parent_id"`
	Name           *string    `json:"name"`
	ConflictPolicy string     `json:"conflict_policy"`
}

// POST /api/nodes/{id}/copy -- files only; no bytes are copied.
func (s *Server) copyNode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req copyNodeReq
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "malformed request body")
		return
	}
	if req.ParentID == nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "parent_id is required")
		return
	}

	n, err := s.nodes().Copy(r.Context(), MustUser(r.Context()).ID, id, *req.ParentID, req.Name, req.ConflictPolicy)
	if err != nil {
		WriteNodeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, NodeDTOFrom(n))
}
