package api

// The public share surface's chain: the headers every answer under /api/s
// carries and the bucket in front of it. Both are properties of the router
// group rather than of any handler, so that a refusal written by WriteErr --
// and the group's own 404 for a path no route claims -- carries them too.

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// shareHeaders marks every answer under /api/s as uncacheable, unindexable and
// not to be used as a referrer.
//
// The token is the whole credential and it is in the URL, so the three headers
// are about keeping that URL where it was sent: not in a shared cache, not in
// a search index, and not in the Referer of whatever the page loads next. They
// are written before the next handler runs because WriteErr sets no headers of
// its own, and a refusal is the answer most of these requests get.
func shareHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "private, no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		next.ServeHTTP(w, r)
	})
}

// rateLimitShare is RateLimitAuth for the public share routes: its own bucket,
// its own allowance, refused with the JSON envelope.
//
// Its own bucket because a share page is something a stranger can make a
// browser visit, and an office behind one NAT opening a few links must not
// find the sign-in form refusing it. Its own allowance because a page load is
// three requests and the auth number is ten.
//
// The refusal line names the route pattern and never r.URL.Path: the path
// carries the token. The nil guard is for the bare &Server{} literals in this
// package's tests, as in clientip.go.
func (s *Server) rateLimitShare(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r)
		if s.ShareRate != nil && !s.ShareRate.allow(ip) {
			LoggerFrom(r.Context()).Warn("share request refused by the per-IP bucket",
				"client_ip", ip, "route", chi.RouteContext(r.Context()).RoutePattern())
			WriteErr(w, r, http.StatusTooManyRequests, CodeRateLimited,
				"too many requests. Try again in a minute.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimitSharePassword charges the AUTH bucket with the share group's
// logging. POST /api/s/{token}/password is the one unauthenticated route in
// the group that reaches Argon2, so it is bounded exactly like login is --
// but RateLimitAuth's refusal line logs r.URL.Path, and this path carries
// the credential. Same bucket, same numbers, its own line.
func (s *Server) rateLimitSharePassword(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r)
		if s.AuthRate != nil && !s.AuthRate.allow(ip) {
			LoggerFrom(r.Context()).Warn("share password request refused by the per-IP bucket",
				"client_ip", ip, "route", chi.RouteContext(r.Context()).RoutePattern())
			WriteErr(w, r, http.StatusTooManyRequests, CodeRateLimited,
				"too many requests. Try again in a minute.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mountShare is the /api/s group, where the public share routes live. It
// carries shareHeaders on everything under it, including its own 404 and
// 405, so an unmatched subpath cannot fall through to /api's bare JSON 404
// without the headers.
//
// The buckets are attached per route in mountShareGuest rather than here:
// group middleware runs before the subrouter has matched anything, so a
// refusal logged from this level could only name /api/s/* -- per route, the
// line names the real pattern, and the token still never appears.
func (s *Server) mountShare(r chi.Router) {
	r.Route("/s", func(r chi.Router) {
		r.Use(shareHeaders)

		s.mountShareGuest(r)

		unmatched := func(w http.ResponseWriter, r *http.Request) {
			WriteErr(w, r, http.StatusNotFound, CodeNotFound, "no such endpoint")
		}
		r.Handle("/*", http.HandlerFunc(unmatched))
		r.NotFound(unmatched)
		r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			WriteErr(w, r, http.StatusMethodNotAllowed, CodeInvalid, "method not allowed")
		})
	})
}
