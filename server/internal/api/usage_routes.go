package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) mountUsage(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.RequireAuth)

		r.Get("/usage", s.usage)
	})
}

// usageResponse is what a storage meter needs and nothing more.
//
// Quota and MaxFile are pointers because zero is a real configuration meaning
// "no cap at all": sending 0 would render as a full meter over an empty
// allowance, and null lets the client say "no limit" honestly instead.
type usageResponse struct {
	Used    int64  `json:"used"`
	Quota   *int64 `json:"quota"`
	MaxFile *int64 `json:"max_file_size"`
}

// GET /api/usage
//
// Used is the caller's own bytes, counted the same way the upload path counts
// them when it decides whether to accept a new file: published files including
// trashed ones -- the trash keeps its bytes until it is purged and collected --
// plus the declared size of that user's uploads still in flight. Anything else
// would show a number that disagrees with the refusal they just got.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	used, err := s.uploads().Usage(r.Context(), MustUser(r.Context()).ID)
	if err != nil {
		WriteErr(w, r, http.StatusInternalServerError, CodeInternal, "could not read storage usage")
		return
	}

	out := usageResponse{Used: used.User}
	if s.Cfg.UserQuota > 0 {
		q := s.Cfg.UserQuota
		out.Quota = &q
	}
	if s.Cfg.MaxFileSize > 0 {
		m := s.Cfg.MaxFileSize
		out.MaxFile = &m
	}
	WriteJSON(w, http.StatusOK, out)
}
