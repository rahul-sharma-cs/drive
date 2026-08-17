package auth

// The Argon2 admission control, and the load-bearing abuse control on the whole
// unauthenticated surface.
//
// Argon2id is deliberately expensive: each hash or verify holds 19 MiB for tens
// of milliseconds. That is exactly right against an offline attacker and exactly
// wrong as an unmetered service a stranger can call -- a few hundred concurrent
// signups is not a rate problem, it is an out-of-memory kill, and no request
// rate limit fixes it because a single burst is enough. Bounding how many run at
// once turns "the process dies" into "some callers get a 429", which is the only
// outcome worth having.
//
// The bound is a hard ceiling, not a queue: a caller that cannot get a slot is
// refused immediately rather than parked, because a queue in front of a
// deliberately slow function is just the memory problem again with extra
// waiting.

import "errors"

// DefaultArgon2Concurrency is how many Argon2 operations may run at once when
// nothing configures it. Four costs ~76 MiB at peak, which fits the smallest
// deployment this runs on with room for everything else.
const DefaultArgon2Concurrency = 4

// ErrBusy is returned when every Argon2 slot is taken. Handlers answer 429.
var ErrBusy = errors.New("auth: too many password operations in flight")

// Limiter bounds concurrent Argon2 work.
type Limiter struct {
	slots chan struct{}
}

// NewLimiter builds a limiter admitting n operations at once. n below 1 means
// the default -- a zero from an unset config must never mean "admit nothing".
func NewLimiter(n int) *Limiter {
	if n < 1 {
		n = DefaultArgon2Concurrency
	}
	return &Limiter{slots: make(chan struct{}, n)}
}

// Acquire takes a slot, or reports false immediately if none is free. Every
// successful Acquire must be paired with a Release.
func (l *Limiter) Acquire() bool {
	select {
	case l.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release returns a slot.
func (l *Limiter) Release() {
	select {
	case <-l.slots:
	default:
	}
}

// Hash runs HashPassword inside a slot, or returns ErrBusy.
func (l *Limiter) Hash(plain string) (string, error) {
	if !l.Acquire() {
		return "", ErrBusy
	}
	defer l.Release()
	return HashPassword(plain)
}

// Verify runs VerifyPassword inside a slot, or returns ErrBusy.
//
// Login's decoy verification for an unknown address goes through here too. It
// has to: the decoy exists so an unknown address costs the same as a known one,
// and a decoy that skipped the limiter would both leave the cheapest attack
// unbounded and make the two paths distinguishable by how they fail under load.
func (l *Limiter) Verify(phc, plain string) (bool, error) {
	if !l.Acquire() {
		return false, ErrBusy
	}
	defer l.Release()
	return VerifyPassword(phc, plain)
}

// Free reports how many slots are currently unused. It exists for tests and for
// a future metric; nothing in the request path reads it.
func (l *Limiter) Free() int { return cap(l.slots) - len(l.slots) }
