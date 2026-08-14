package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestNewTokenIs256BitsOfBase64URL(t *testing.T) {
	raw, sum, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("token %q is not unpadded base64url: %v", raw, err)
	}
	if len(decoded) != 32 {
		t.Errorf("token carries %d bytes of entropy, want 32 (256 bits)", len(decoded))
	}

	want := sha256.Sum256([]byte(raw))
	if !bytes.Equal(sum, want[:]) {
		t.Errorf("returned hash is not sha256 of the raw token string")
	}
}

func TestNewTokenIsUnpredictable(t *testing.T) {
	seen := make(map[string]bool, 128)
	for range 128 {
		raw, _, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seen[raw] {
			t.Fatalf("NewToken repeated %q", raw)
		}
		seen[raw] = true
	}
}

func TestHashTokenMatchesNewToken(t *testing.T) {
	raw, sum, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if !bytes.Equal(HashToken(raw), sum) {
		t.Error("HashToken(raw) != the hash NewToken returned -- lookup by hash would never find the row")
	}
	if bytes.Equal(HashToken(raw), HashToken(raw+"x")) {
		t.Error("HashToken collides on distinct inputs")
	}
}
