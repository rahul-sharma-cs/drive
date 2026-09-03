package api

// The public share surface's chain: the headers every answer under /api/s
// carries and the bucket in front of it. Both are properties of the chain
// rather than of any handler, so that a refusal written by WriteErr --
// and the group's own 404 for a path no route claims -- carries them too.

import (
	"net/http"
	"net/url"
	"strings"

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

// shareHeadersUnderAPI is shareHeaders on the /api chain, for the paths the
// share group claims: /api/s and everything below it. It sits above
// RequireClientHeader and sessionLoader because two answers are written
// there -- the 403 for a share POST without X-Drive-Client, and the 503 when
// a drive_session cookie cannot be looked up -- and applied at the group they
// would go out bare. Every other /api path passes through untouched.
func shareHeadersUnderAPI(next http.Handler) http.Handler {
	marked := shareHeaders(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; p == "/api/s" || strings.HasPrefix(p, "/api/s/") {
			marked.ServeHTTP(w, r)
			return
		}
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

// rateLimitShareDownload is rateLimitShare for the one browser navigation in
// the group -- rateLimitBrowser's shape: the same bucket and the same line,
// refused with a redirect back to the page instead of the envelope, because
// a person following a link cannot act on JSON. No reason on the redirect:
// the page's own /meta fetch meets the same bucket and draws the
// rate-limited card, or the file once the address has a token again.
func (s *Server) rateLimitShareDownload(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r)
		if s.ShareRate != nil && !s.ShareRate.allow(ip) {
			LoggerFrom(r.Context()).Warn("share request refused by the per-IP bucket",
				"client_ip", ip, "route", chi.RouteContext(r.Context()).RoutePattern())
			shareRedirect(w, "/s/"+url.PathEscape(chi.URLParam(r, "token")))
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

// mountShare is the /api/s group, where the public share routes live. Its
// own 404 and 405 are here so an unmatched subpath cannot fall through to
// /api's bare JSON 404; the headers on all of it, those included, come from
// shareHeadersUnderAPI at the top of the /api chain.
//
// The buckets are attached per route in mountShareGuest rather than here:
// group middleware runs before the subrouter has matched anything, so a
// refusal logged from this level could only name /api/s/* -- per route, the
// line names the real pattern, and the token still never appears.
func (s *Server) mountShare(r chi.Router) {
	r.Route("/s", func(r chi.Router) {
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
