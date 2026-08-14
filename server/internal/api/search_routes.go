package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rahul-sharma-cs/drive/server/internal/node"
)

func (s *Server) mountSearch(r chi.Router) {
	r.With(s.RequireAuth).Get("/search", s.search)
}

// GET /api/search?q=&type=&after=&before=&min_size=&max_size=
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	u := MustUser(r.Context())

	q, err := parseSearchQuery(r)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}

	rawCursor, limit, err := Page(r)
	if err != nil {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, err.Error())
		return
	}
	var cur *node.SearchCursor
	if rawCursor != "" {
		var c node.SearchCursor
		if err := DecodeCursor(rawCursor, &c); err != nil {
			WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, "invalid cursor")
			return
		}
		cur = &c
	}

	items, next, err := node.NewStore(s.DB).Search(r.Context(), u.ID, q, cur, limit)
	if err != nil {
		LoggerFrom(r.Context()).Error("search failed", "error", err)
		WriteErr(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
		return
	}

	encoded := ""
	if next != nil {
		encoded = EncodeCursor(next)
	}
	WriteJSON(w, http.StatusOK, NewList(itemDTOs(items), encoded))
}

// parseSearchQuery validates the filter parameters. Timestamps are RFC3339 and
// bound updated_at inclusively; sizes are inclusive byte counts. Every filter
// is optional and they AND together.
func parseSearchQuery(r *http.Request) (node.SearchQuery, error) {
	v := r.URL.Query()
	q := node.SearchQuery{Q: v.Get("q")}

	switch kind := v.Get("type"); kind {
	case "", node.KindFile, node.KindFolder:
		q.Kind = kind
	default:
		return q, fmt.Errorf("type: want 'file' or 'folder', got %q", kind)
	}

	var err error
	if q.After, err = searchTime(v.Get("after"), "after"); err != nil {
		return q, err
	}
	if q.Before, err = searchTime(v.Get("before"), "before"); err != nil {
		return q, err
	}
	if q.MinSize, err = searchSize(v.Get("min_size"), "min_size"); err != nil {
		return q, err
	}
	if q.MaxSize, err = searchSize(v.Get("max_size"), "max_size"); err != nil {
		return q, err
	}
	return q, nil
}

func searchTime(raw, name string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("%s: want an RFC3339 timestamp, got %q", name, raw)
	}
	return &t, nil
}

func searchSize(raw, name string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("%s: want a byte count >= 0, got %q", name, raw)
	}
	return &n, nil
}
