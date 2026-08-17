package api

// The client-address rule and the bucket built on it.
//
// These are the two pieces the deployment's abuse controls stand on, and both
// fail silently when they are wrong: a limiter keyed on the proxy locks out
// every user as one, and a limiter that believes a forged header is no limiter
// at all. Neither shows up as a failing request in normal use.
//
// Every address below is from RFC 5737's documentation ranges, standing in for
// a client (203.0.113.7, 198.51.100.x) and for the CDN hop the edge appends
// after it (203.0.113.100, .200). They are deliberately not the real
// infrastructure addresses: nothing observed from a live system belongs in a
// tracked file, and TEST-NET is public as far as isPublicIP is concerned, which
// is the only property these cases depend on.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIPTakesTheLeftmostForwardedAddress(t *testing.T) {
	cases := []struct {
		what       string
		remoteAddr string
		forwarded  string
		want       string
	}{
		{
			"no header at all -- the direct case, and every local run",
			"203.0.113.7:41234", "", "203.0.113.7",
		},
		{
			"one entry: the edge wrote the client and nothing else",
			"10.0.0.9:80", "203.0.113.7", "203.0.113.7",
		},
		{
			"two entries: the CDN hop is appended AFTER the client, so leftmost wins",
			"10.0.0.9:80", "203.0.113.7, 203.0.113.100", "203.0.113.7",
		},
		{
			"rightmost-non-internal would pick the CDN here, which is the bug this avoids",
			"10.0.0.9:80", "198.51.100.22, 203.0.113.200, 203.0.113.100", "198.51.100.22",
		},
		{
			"whitespace around the entries is not part of the address",
			"10.0.0.9:80", "  203.0.113.7 , 203.0.113.100 ", "203.0.113.7",
		},
		{
			"an IPv6 client survives its own formatting",
			"10.0.0.9:80", "2001:db8::1, 203.0.113.100", "2001:db8::1",
		},
		{
			"a private leftmost is not something the edge writes: fall back to the peer",
			"203.0.113.9:41234", "10.1.2.3", "203.0.113.9",
		},
		{
			"loopback is the same story",
			"203.0.113.9:41234", "127.0.0.1, 203.0.113.7", "203.0.113.9",
		},
		{
			"nonsense in the header is never parsed into a key",
			"203.0.113.9:41234", "not-an-ip", "203.0.113.9",
		},
		{
			"an empty first entry does not silently promote the second",
			"203.0.113.9:41234", ", 203.0.113.7", "203.0.113.9",
		},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
			r.RemoteAddr = c.remoteAddr
			if c.forwarded != "" {
				r.Header.Set("X-Forwarded-For", c.forwarded)
			}
			if got := ClientIP(r); got != c.want {
				t.Errorf("ClientIP = %q, want %q", got, c.want)
			}
		})
	}
}

// X-Real-IP currently carries the CDN edge on the platform this deploys to, so
// reading it would key every proxied request on one address.
func TestClientIPIgnoresXRealIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	r.RemoteAddr = "203.0.113.9:41234"
	r.Header.Set("X-Real-IP", "203.0.113.100")

	if got := ClientIP(r); got != "203.0.113.9" {
		t.Errorf("ClientIP = %q, want the peer address -- X-Real-IP must not be read", got)
	}
}

// ------------------------------------------------------------------ bucket --

func TestIPLimiterSpendsABurstThenRefills(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	l := newIPLimiter(10, 20)
	l.now = func() time.Time { return now }

	for i := 1; i <= 20; i++ {
		if !l.allow("203.0.113.7") {
			t.Fatalf("request %d of the burst was refused", i)
		}
	}
	if l.allow("203.0.113.7") {
		t.Fatal("the 21st request in the same instant was allowed")
	}

	// A second caller has its own bucket -- a limiter that pooled them would be
	// one address locking out everybody else.
	if !l.allow("198.51.100.22") {
		t.Error("a different address was refused because the first one spent its burst")
	}

	// Ten a minute: six seconds buys exactly one token.
	now = now.Add(6 * time.Second)
	if !l.allow("203.0.113.7") {
		t.Error("no token had refilled after six seconds")
	}
	if l.allow("203.0.113.7") {
		t.Error("six seconds refilled more than the one token the rate allows")
	}
}

// The map must not grow with every address that ever knocks, and the sweep is
// safe precisely because a refilled bucket carries no information: it is
// indistinguishable from an address that has never been seen.
func TestIPLimiterForgetsRefilledBuckets(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	l := newIPLimiter(10, 20)
	l.now = func() time.Time { return now }

	l.allow("203.0.113.7")    // one token spent, refills in six seconds
	for i := 0; i < 20; i++ { // spends the whole burst
		l.allow("198.51.100.22")
	}
	if len(l.buckets) != 2 {
		t.Fatalf("tracking %d addresses, want 2", len(l.buckets))
	}

	// Past the sweep interval, but not long enough for the spent bucket to refill.
	now = now.Add(90 * time.Second)
	l.allow("192.0.2.5")

	if _, still := l.buckets["203.0.113.7"]; still {
		t.Error("a bucket that had refilled to full was kept")
	}
	if _, still := l.buckets["198.51.100.22"]; !still {
		t.Error("a bucket with tokens still owing was dropped -- its limit would reset")
	}
}
