package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

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
		r.Delete("/trash", s.emptyTrash)
		r.Post("/trash/restore", s.restoreNodes)
		r.Post("/trash/purge", s.purgeNodes)
		r.Delete("/nodes/{id}", s.trashNode)
		r.Post("/nodes/{id}/restore", s.restoreNode)
		r.Delete("/nodes/{id}/purge", s.purgeNode)
	})
}

// itemDTO maps a node row onto the canonical wire shape. Folders keep size and
// mime null; NodeDTO's pointer fields carry that through.
//
// This is the trash's converter and the only one that fills deleted_at in:
// "deleted 3 days ago" is a column of the trash listing, and a live node has no
// such moment. NodeDTOFrom leaves it nil, so a children page or a created
// folder serializes exactly as it always did.
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
		DeletedAt:   n.DeletedAt,
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

// ------------------------------------------------------------------- bulk ----

// Selecting forty things in the trash and restoring them is one gesture to the
// user and forty operations to the server. The bulk routes below do exactly
// that -- forty single-id operations, each in its own transaction -- rather
// than one transaction over the lot, because the outcomes differ per id: a
// restore whose name is taken auto-renames, one whose id someone else already
// purged is a miss, and neither is a reason to undo the thirty-eight that
// worked. Every id therefore gets its own status and the response is a 200
// even when half of it failed.
//
// The other half of the design is the budget. Purging a deep tree is unbounded
// work, and a platform's edge will cut the connection long before Postgres
// gives up, so a bulk call stops at DefaultBulkBudget and answers the ids it
// never reached as "pending" with remaining=true. The client loops until
// remaining is false. (One single huge root can still overrun it -- that is
// the existing single-purge behaviour, unchanged here.)
const (
	// BulkIDLimit is the most ids one bulk restore or purge accepts, matching
	// MaxLimit so a client can act on exactly the page it was shown.
	BulkIDLimit = MaxLimit
	// DefaultBulkBudget is the wall clock a bulk trash request spends before
	// reporting the rest as pending.
	DefaultBulkBudget = 25 * time.Second
	// emptyTrashPage is how many trashed roots one pass of DELETE /api/trash
	// reads at a time.
	emptyTrashPage = 200
)

// The per-id outcomes. These four plus "pending" are the whole vocabulary.
const (
	bulkOK           = "ok"
	bulkNotFound     = "not_found"
	bulkNameConflict = "name_conflict"
	bulkPending      = "pending"
	bulkError        = "error"
)

type bulkIDsReq struct {
	IDs []uuid.UUID `json:"ids"`
}

// BulkResult is one id's outcome.
type BulkResult struct {
	ID     uuid.UUID `json:"id"`
	Status string    `json:"status"`
}

// BulkResponse is what both bulk routes answer: one result per distinct id, in
// the order they were asked for, and whether anything is left to do.
type BulkResponse struct {
	Results   []BulkResult `json:"results"`
	Remaining bool         `json:"remaining"`
}

// emptyTrashResponse is DELETE /api/trash's answer.
type emptyTrashResponse struct {
	Purged    int  `json:"purged"`
	Remaining bool `json:"remaining"`
}

// bulkDeadline is when the current request stops starting new work.
func (s *Server) bulkDeadline() time.Time {
	budget := s.BulkBudget
	if budget <= 0 {
		budget = DefaultBulkBudget
	}
	return time.Now().Add(budget)
}

// POST /api/trash/restore -- restore up to BulkIDLimit trashed roots.
func (s *Server) restoreNodes(w http.ResponseWriter, r *http.Request) {
	store := node.NewStore(s.DB)
	s.bulkTrashOp(w, r, func(ctx context.Context, owner, id uuid.UUID) error {
		_, err := store.Restore(ctx, owner, id)
		return err
	})
}

// POST /api/trash/purge -- permanently delete up to BulkIDLimit trashed roots.
func (s *Server) purgeNodes(w http.ResponseWriter, r *http.Request) {
	s.bulkTrashOp(w, r, node.NewStore(s.DB).Purge)
}

// bulkTrashOp runs one single-id trash operation over a deduplicated list of
// ids under the shared budget.
func (s *Server) bulkTrashOp(w http.ResponseWriter, r *http.Request, op func(context.Context, uuid.UUID, uuid.UUID) error) {
	u := MustUser(r.Context())

	var req bulkIDsReq
	if err := ReadJSON(r, &req); err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "malformed request body")
		return
	}
	// Deduplicated before the limit is applied, in that order: the contract is
	// up to BulkIDLimit *distinct* ids, and an id sent twice is one selected
	// row, not two operations. Checking first would refuse a client whose
	// selection was inside the limit all along.
	ids := make([]uuid.UUID, 0, len(req.IDs))
	seen := make(map[uuid.UUID]struct{}, len(req.IDs))
	for _, id := range req.IDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 || len(ids) > BulkIDLimit {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid,
			fmt.Sprintf("ids: want 1 to %d distinct node ids, got %d", BulkIDLimit, len(ids)))
		return
	}

	deadline := s.bulkDeadline()
	results := make([]BulkResult, 0, len(ids))
	remaining := false

	for _, id := range ids {
		// Checked before each root rather than after: what the budget bounds is
		// work started, and starting one more purge is what would overrun it.
		if !time.Now().Before(deadline) {
			results = append(results, BulkResult{ID: id, Status: bulkPending})
			remaining = true
			continue
		}
		results = append(results, BulkResult{ID: id, Status: bulkStatus(r, op(r.Context(), u.ID, id))})
	}

	WriteJSON(w, http.StatusOK, BulkResponse{Results: results, Remaining: remaining})
}

// bulkStatus maps one operation's error onto its per-id status.
func bulkStatus(r *http.Request, err error) string {
	switch {
	case err == nil:
		return bulkOK
	case errors.Is(err, node.ErrNotFound):
		return bulkNotFound
	case errors.Is(err, node.ErrNameConflict):
		return bulkNameConflict
	case errors.Is(err, node.ErrRootNode):
		// A client that sent its own root folder's id. It is a client bug rather
		// than a server one, but the id genuinely was not acted on, so it is an
		// error like any other -- just not one worth a log line.
		return bulkError
	default:
		// One id of many, and the caller already sees it as "error": a failure
		// here is worth noticing, not worth paging anyone. A cancelled request
		// is not even that -- it means the browser went away mid-selection.
		if !errors.Is(err, context.Canceled) {
			LoggerFrom(r.Context()).Warn("bulk trash operation failed", "error", err)
		}
		return bulkError
	}
}

// DELETE /api/trash -- purge every trashed root, a page at a time.
//
// Idempotent and resumable: it takes no ids, so a client that gets
// remaining=true calls it again and it carries on with what is left. It reads a
// fresh page each pass rather than paginating with a cursor, because the pass
// deletes the rows it just read -- the next read is the next batch by
// construction.
//
// The answer is always a 200 carrying {purged, remaining}, including when
// something goes wrong mid-sweep. Everything purged before that point is
// committed, and reporting a 500 would throw the count away and tell the client
// to stop looping while its trash still had rows in it.
func (s *Server) emptyTrash(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())
	store := node.NewStore(s.DB)
	deadline := s.bulkDeadline()

	purged := 0
	remaining := false

pages:
	for {
		if !time.Now().Before(deadline) {
			remaining = true
			break
		}
		roots, err := store.TrashRootIDs(r.Context(), u.ID, emptyTrashPage)
		if err != nil {
			s.writeTrashErr(w, r, err)
			return
		}
		if len(roots) == 0 {
			break
		}

		// Purges and misses both count as progress. A miss means the root left
		// the trash by some other route, so the next read returns a different
		// page -- counting only purges would stop the sweep dead on a page the
		// hourly GC (or a second tab) had just emptied, leaving every root
		// behind it untouched and telling the client there was nothing left.
		advanced := 0
		for _, id := range roots {
			if !time.Now().Before(deadline) {
				remaining = true
				break
			}
			switch err := store.Purge(r.Context(), u.ID, id); {
			case err == nil:
				purged++
				advanced++
			case errors.Is(err, node.ErrNotFound):
				// Restored or purged from another tab between the read and the
				// write. It is out of the trash either way, and the next read
				// will not return it.
				advanced++
			default:
				// A cancelled request (the user navigated away) or a deadlock
				// against the GC's own purge. Neither is a reason to discard
				// what is already committed: stop here and say there is more.
				if !errors.Is(err, context.Canceled) {
					LoggerFrom(r.Context()).Warn("emptying the trash stopped early",
						"error", err, "node_id", id, "purged", purged)
				}
				remaining = true
				break pages
			}
		}
		// A page that neither purged nor lost a single root cannot do better by
		// being read again. Stop instead of spinning the budget away.
		if advanced == 0 {
			break
		}
	}

	WriteJSON(w, http.StatusOK, emptyTrashResponse{Purged: purged, Remaining: remaining})
}
