package api

// Downloads: the one route that hands a client bytes, and it does so by getting
// out of the way. The server authorizes, signs a URL and redirects; the transfer
// is browser-to-store, so a 50 GB download costs this process one round trip and
// no egress.

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/rahul-sharma-cs/drive/server/internal/node"
)

func (s *Server) mountDownload(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.RequireAuth)

		r.Get("/files/{id}/download", s.downloadFile)
	})
}

// downloadFile answers 302 with a presigned GET.
//
// The redirect target is a bearer credential for one object for one hour, so it
// must never be cached by a proxy or written into a shared history: the response
// is marked no-store, and the URL carries the forced attachment disposition and
// octet-stream type rather than trusting anything the uploader supplied.
//
// A non-browser client would want ?redirect=0 returning {url, expires_at}
// instead of the 302, and a shorter TTL; neither exists yet.
func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	u := MustUser(r.Context())

	file, err := node.NewStore(s.DB).Download(r.Context(), u.ID, id)
	if err != nil {
		WriteNodeErr(w, r, err)
		return
	}

	signed, err := s.presigner().GetURL(r.Context(), file.ObjectKey, file.Name)
	if err != nil {
		LoggerFrom(r.Context()).Error("presigning a download", "error", err, "node_id", file.NodeID)
		WriteErr(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
		return
	}

	LoggerFrom(r.Context()).Info("download", "node_id", file.NodeID, "user_id", u.ID, "size", file.Size)
	w.Header().Set("Cache-Control", "private, no-store")
	http.Redirect(w, r, signed.URL, http.StatusFound)
}
