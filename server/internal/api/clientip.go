package api

// Who is calling, and how often.
//
// Both halves of this file are one decision: behind a platform edge, the only
// honest client address is the leftmost X-Forwarded-For entry, and a rate
// limiter keyed on anything else is worse than none -- it would bucket every
// user in the world together under one proxy address and lock them out as a set.

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ClientIP returns the caller's address for logging, for auth_sessions.ip and
// for the per-IP bucket.
//
// The rule is leftmost-X-Forwarded-For, and it is platform-specific: Railway's
// edge strips any client-supplied XFF and writes the real client address first,
// appending its own hops after it (an observed chain is
// "<client-ip>, <fastly-edge-ip>"). X-Real-IP is deliberately never read -- it
// currently carries the CDN edge, not the client.
//
// "Rightmost non-internal" is the usual advice and is wrong here: the CDN hop is
// a public address too, so that rule would key the bucket on the proxy, which is
// exactly the bug this exists to avoid. When the leftmost entry is not a public
// address the header is not trustworthy at all, and the answer is RemoteAddr
// plus a loud log line -- never a second guess further down the chain.
//
// This trusts the header without a proxy allowlist, which is sound only because
// nothing can reach the process except through the platform edge. That
// assumption is checked in deployment with a forged-header request from
// outside; if a spoofed value ever survives, a trusted-proxy list has to land
// before the service is shared.
//
// It resolves the address at most once per request. The rule writes a log line
// for a header it will not trust, and several layers ask for the address on the
// same request -- the bucket in front of /api/auth, the mail bucket behind it,
// the audit column on login -- so without this every one of those layers would
// repeat the same complaint. requestLogger seeds the context; a request that
// never went through it (a unit test, a handler called directly) still gets an
// answer, it is simply computed on the spot.
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	return resolveClientIP(r)
}

// clientIPKey carries the resolved address on the request context.
type clientIPKey struct{}

// withResolvedClientIP resolves the address and returns a request carrying it.
// Called once, by requestLogger, after the request logger is in place so the
// untrusted-header line lands with a request_id like every other line.
func withResolvedClientIP(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), clientIPKey{}, resolveClientIP(r)))
}

func resolveClientIP(r *http.Request) string {
	direct := remoteHost(r)

	forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if forwarded == "" {
		return direct
	}

	leftmost := strings.TrimSpace(strings.SplitN(forwarded, ",", 2)[0])
	ip := net.ParseIP(leftmost)
	if ip != nil && isPublicIP(ip) {
		return ip.String()
	}

	LoggerFrom(r.Context()).Error("untrustworthy X-Forwarded-For; keying on the peer address instead",
		"leftmost", leftmost, "remote_addr", direct)
	return direct
}

// remoteHost is the peer address with its port removed.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}

// isPublicIP reports whether an address could belong to a real client on the
// internet. Loopback, link-local, multicast and the private ranges cannot,
// which is what makes them evidence that the header was not written by the edge.
func isPublicIP(ip net.IP) bool {
	return !ip.IsUnspecified() &&
		!ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast()
}

// ---------------------------------------------------------------- the bucket --

// DefaultAuthRatePerMin is the auth-surface allowance when nothing configures
// it (DRIVE_AUTH_RATE_PER_MIN). Ten a minute is far above anything a person
// does -- a signup, a verification, a login -- and far below what a script needs
// to be worth running. The burst is twice the rate, so a page that fires two
// requests back to back is never punished for it.
const DefaultAuthRatePerMin = 10

// DefaultMailRatePerHour is how many messages one client address may ask Drive
// to send to somebody else (DRIVE_MAIL_RATE_PER_HOUR), across password-reset
// and resend-verification together.
//
// Those two endpoints answer 200 for any address, so their per-recipient
// budgets are spent on behalf of whoever the caller names -- five requests and
// a stranger's inbox is silent for an hour. This is the ceiling on how fast one
// address can do that. Five an hour is more than a person who mistyped their
// own address needs and far less than a sweep through a mailing list.
const DefaultMailRatePerHour = 5

// burstFor is the burst allowance for a rate. One knob, not two: they only ever
// move together.
func burstFor(perMinute float64) float64 { return perMinute * 2 }

// ipLimiter is a token bucket per client address.
//
// It is defence in depth and deliberately in-memory: the durable budgets in the
// throttle table are the security-grade ones, keyed by identity, and a restart
// resetting an abuse counter is acceptable. What this adds is a cheap ceiling in
// front of them, so a flood is refused before it reaches a database round trip
// or an Argon2 slot.
type ipLimiter struct {
	rate  float64 // tokens per second
	burst float64

	mu        sync.Mutex
	buckets   map[string]*ipBucket
	lastSweep time.Time

	// now is injectable so the tests move time instead of sleeping through it.
	now func() time.Time
}

type ipBucket struct {
	tokens float64
	seen   time.Time
}

func newIPLimiter(perMinute, burst float64) *ipLimiter {
	return &ipLimiter{
		rate:    perMinute / 60,
		burst:   burst,
		buckets: map[string]*ipBucket{},
		now:     time.Now,
	}
}

// allow spends a token for key, reporting false when the bucket is empty.
//
// An empty key -- a request whose peer address could not be read at all -- is
// allowed through rather than bucketed: it would otherwise be one shared bucket
// for every such request, which is the proxy-keyed bug in miniature.
func (l *ipLimiter) allow(key string) bool {
	if key == "" {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &ipBucket{tokens: l.burst}
		l.buckets[key] = b
	} else {
		b.tokens = min(l.burst, b.tokens+now.Sub(b.seen).Seconds()*l.rate)
	}
	b.seen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepInterval is how often idle buckets are dropped.
const sweepInterval = time.Minute

// sweepLocked drops buckets that have refilled to full.
//
// This is lossless, which is what makes it safe to do on a timer rather than
// under a size cap: a full bucket and an address that has never been seen are
// the same thing, so forgetting one costs no enforcement. The map therefore
// holds only addresses that have spent tokens inside the last refill window,
// and an unauthenticated caller cannot grow it without bound.
func (l *ipLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now
	for key, b := range l.buckets {
		if b.tokens+now.Sub(b.seen).Seconds()*l.rate >= l.burst {
			delete(l.buckets, key)
		}
	}
}

// RateLimitAuth refuses a caller that is hammering the auth surface.
//
// It sits in front of everything under /api/auth, which is the entire
// unauthenticated write surface: signup and login both reach Argon2, and signup
// additionally reaches the mail sender.
func (s *Server) RateLimitAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r)
		if s.AuthRate != nil && !s.AuthRate.allow(ip) {
			LoggerFrom(r.Context()).Warn("auth request refused by the per-IP bucket",
				"client_ip", ip, "path", r.URL.Path)
			WriteErr(w, r, http.StatusTooManyRequests, CodeRateLimited,
				"too many requests. Try again in a minute.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
