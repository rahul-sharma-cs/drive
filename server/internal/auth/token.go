package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// tokenBytes is the entropy behind every opaque token Drive hands out: session
// cookies and email verification links now, share tokens and guest sessions in
// Phase 3.
const tokenBytes = 32 // 256 bits

// NewToken mints one opaque token: the raw string that goes to the client and
// the sha256 that is all the database ever sees.
//
// sha256 (not Argon2id) is the right choice here and must not be "fixed": the
// input is 256 bits of uniform randomness, so there is nothing to brute-force
// and nothing to salt.
func NewToken() (raw string, hash []byte, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("auth: reading token bytes: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, HashToken(raw), nil
}

// HashToken hashes a raw token for storage or lookup. It hashes the encoded
// string, not the bytes behind it, so callers never have to decode.
func HashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
