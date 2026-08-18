// Package auth owns Drive's authentication primitives: password hashing,
// opaque tokens, login sessions, and the durable windowed throttle that every
// security budget in the product is counted in.
//
// Nothing here knows about HTTP. Package api's auth_routes.go composes these
// into handlers. Share passwords, OTP budgets and personal access tokens
// would all reuse the same primitives if they ship.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters, fixed for every password Drive stores -- user accounts
// and share passwords alike. They are a compatibility contract:
// changing them silently invalidates nothing (the PHC string carries its own
// parameters) but every new hash diverges from every old one, so move them only
// deliberately and with a rehash-on-login path.
const (
	argonMemory  uint32 = 19456 // KiB
	argonTime    uint32 = 2
	argonThreads uint8  = 1
	argonSaltLen        = 16
	argonKeyLen  uint32 = 32
)

// ErrBadHash means the stored string is not a PHC hash this package produced.
// It is deliberately distinct from "wrong password": a wrong password is a
// normal false, a bad hash is a corrupted row or a bug.
var ErrBadHash = errors.New("auth: not a valid argon2id PHC hash")

// phcEncoding is the PHC standard's unpadded base64 for salt and tag.
var phcEncoding = base64.RawStdEncoding

// HashPassword returns the PHC string for plain:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<tag>
//
// Every call uses a fresh 16-byte salt, so hashing the same password twice
// never yields the same string.
func HashPassword(plain string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: reading salt: %w", err)
	}
	tag := argon2.IDKey([]byte(plain), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		phcEncoding.EncodeToString(salt), phcEncoding.EncodeToString(tag),
	), nil
}

// VerifyPassword reports whether plain hashes to phc. The comparison is
// constant time. A mismatch is (false, nil); only a hash this package could
// never have written is an error.
func VerifyPassword(phc, plain string) (bool, error) {
	salt, want, memory, time, threads, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(plain), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// parsePHC pulls the salt, the tag and the cost parameters out of a PHC string.
// The parameters are read from the string rather than assumed, so a hash
// written under different costs still verifies if the costs are ever retuned.
func parsePHC(phc string) (salt, tag []byte, memory, time uint32, threads uint8, err error) {
	fail := func(why string) (_, _ []byte, _, _ uint32, _ uint8, err error) {
		return nil, nil, 0, 0, 0, fmt.Errorf("%w: %s", ErrBadHash, why)
	}

	// A well-formed string starts with '$', so Split yields a leading "".
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" {
		return fail("want 5 $-separated fields")
	}
	if parts[1] != "argon2id" {
		return fail(fmt.Sprintf("variant %q is not argon2id", parts[1]))
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return fail("unreadable version field")
	}
	if version != argon2.Version {
		return fail(fmt.Sprintf("version %d, want %d", version, argon2.Version))
	}

	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return fail("unreadable m,t,p parameters")
	}
	if m == 0 || t == 0 || p == 0 {
		return fail("zero cost parameter")
	}

	s, err := phcEncoding.DecodeString(parts[4])
	if err != nil {
		return fail("salt is not unpadded base64")
	}
	if len(s) != argonSaltLen {
		return fail(fmt.Sprintf("salt is %d bytes, want %d", len(s), argonSaltLen))
	}

	k, err := phcEncoding.DecodeString(parts[5])
	if err != nil {
		return fail("tag is not unpadded base64")
	}
	if len(k) != int(argonKeyLen) {
		return fail(fmt.Sprintf("tag is %d bytes, want %d", len(k), argonKeyLen))
	}

	return s, k, m, t, p, nil
}
