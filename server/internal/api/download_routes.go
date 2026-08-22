package api

// Downloads and previews: the routes that hand a client bytes, and they do so by
// getting out of the way. The server authorizes, signs a URL and points at it;
// the transfer is browser-to-store, so a 50 GB download costs this process one
// round trip and no egress.
//
// The two differ in exactly one thing, and it is the thing that matters: a
// download is signed as an attachment of application/octet-stream, which no
// browser will ever execute, while a preview is signed to render in place. That
// is only safe for types the server itself picked, so a preview asks
// upload.PreviewContentType first and refuses everything it does not recognise.

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rahul-sharma-cs/drive/server/internal/node"
	"github.com/rahul-sharma-cs/drive/server/internal/upload"
)

func (s *Server) mountDownload(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.RequireAuth)

		r.Get("/files/{id}/download", s.downloadFile)
		r.Get("/files/{id}/preview", s.previewFile)
	})
}

// downloadLink is ?format=json's body: the redirect's Location, spelled out.
type downloadLink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// previewLink is the preview body. mime is the type the URL will actually be
// served as -- the allowlist's own constant, never the stored one -- so the
// client can decide which element to render into without guessing again.
type previewLink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	Mime      string    `json:"mime"`
}

// downloadFile answers 302 with a presigned GET, or ?format=json with the same
// URL in a body.
//
// The redirect target is a bearer credential for one object for one hour, so it
// must never be cached by a proxy or written into a shared history: the response
// is marked no-store, and the URL carries the forced attachment disposition and
// octet-stream type rather than trusting anything the uploader supplied.
//
// ?format=json exists because a browser cannot read a cross-origin redirect it
// did not navigate to: the zip builder needs the URL itself to fetch with, and
// an <a href> to the 302 cannot give it one. Both forms sign the same way --
// same overrides, same TTL -- because a JSON URL that skipped the attachment
// disposition would be an inline HTML URL by another name.
func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format != "" && format != "json" {
		WriteErr(w, r, http.StatusUnprocessableEntity, CodeInvalid, `format must be "json" if present`)
		return
	}

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
	if format == "json" {
		WriteJSON(w, http.StatusOK, downloadLink{URL: signed.URL, ExpiresAt: signed.ExpiresAt})
		return
	}
	http.Redirect(w, r, signed.URL, http.StatusFound)
}

// previewFile answers {url, expires_at, mime} with a presigned inline GET, or
// 415 when the file's type is not one a browser may be asked to render.
//
// There is no redirect form on purpose: the client needs the type it is about
// to get in order to pick an element for it, and a 302 would leave it sniffing.
// The URL is the same kind of short-lived credential the download hands out and
// is marked no-store for the same reason.
func (s *Server) previewFile(w http.ResponseWriter, r *http.Request) {
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

	// file.Mime is the uploader's claim and is used only as a lookup key: what
	// gets signed is contentType, which came out of the allowlist.
	contentType, previewable := upload.PreviewContentType(file.Mime)
	if !previewable {
		WriteErr(w, r, http.StatusUnsupportedMediaType, CodeUnsupported, "no preview for this file type")
		return
	}

	signed, err := s.presigner().PreviewURL(r.Context(), file.ObjectKey, file.Name, contentType)
	if err != nil {
		LoggerFrom(r.Context()).Error("presigning a preview", "error", err, "node_id", file.NodeID)
		WriteErr(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
		return
	}

	LoggerFrom(r.Context()).Info("preview", "node_id", file.NodeID, "user_id", u.ID, "served_as", contentType)
	w.Header().Set("Cache-Control", "private, no-store")
	WriteJSON(w, http.StatusOK, previewLink{URL: signed.URL, ExpiresAt: signed.ExpiresAt, Mime: contentType})
}
