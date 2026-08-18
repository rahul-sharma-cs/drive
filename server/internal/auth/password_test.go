package auth

import (
	"regexp"
	"strings"
	"testing"
)

// The PHC string is the storage format for every password in Drive -- user
// accounts, and share passwords too if those ship -- so its shape is part of
// the contract, not an implementation detail.
var phcShape = regexp.MustCompile(`^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)

func TestHashPasswordProducesTheSpecifiedPHCString(t *testing.T) {
	got, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !phcShape.MatchString(got) {
		t.Errorf("hash = %q, want $argon2id$v=19$m=19456,t=2,p=1$<16-byte b64 salt>$<32-byte b64 tag>", got)
	}
}

func TestVerifyPasswordAcceptsTheRightPassword(t *testing.T) {
	const pw = "correct horse battery staple"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	ok, err := VerifyPassword(hash, pw)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword said no to the password it was hashed from")
	}
}

func TestVerifyPasswordRejectsTheWrongPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	for _, wrong := range []string{"", "correct horse battery stapl", "Correct horse battery staple", "correct horse battery staple "} {
		ok, err := VerifyPassword(hash, wrong)
		if err != nil {
			t.Fatalf("VerifyPassword(%q): unexpected error %v -- a wrong password is not a malformed hash", wrong, err)
		}
		if ok {
			t.Errorf("VerifyPassword accepted %q", wrong)
		}
	}
}

// The salt is what stops a stolen table from being one rainbow-table lookup.
func TestHashPasswordSaltsEveryHash(t *testing.T) {
	const pw = "same password twice"
	a, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	b, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if a == b {
		t.Fatalf("two hashes of the same password are identical (%q) -- the salt is not random", a)
	}

	// Both must still verify.
	for _, h := range []string{a, b} {
		ok, err := VerifyPassword(h, pw)
		if err != nil || !ok {
			t.Errorf("VerifyPassword(%q) = %v, %v; want true, nil", h, ok, err)
		}
	}
}

func TestVerifyPasswordRejectsMalformedPHCStrings(t *testing.T) {
	valid, err := HashPassword("anything")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(valid, "$") // ["", "argon2id", "v=19", "m=...", salt, tag]

	cases := []struct {
		name string
		phc  string
	}{
		{"empty", ""},
		{"not a PHC string at all", "hunter2"},
		{"bcrypt", "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"},
		{"wrong variant", "$argon2i$" + strings.Join(parts[2:], "$")},
		{"unsupported version", "$argon2id$v=16$" + strings.Join(parts[3:], "$")},
		{"missing fields", "$argon2id$v=19$m=19456,t=2,p=1$" + parts[4]},
		{"too many fields", valid + "$extra"},
		{"non-numeric memory", "$argon2id$v=19$m=lots,t=2,p=1$" + strings.Join(parts[4:], "$")},
		{"garbled parameter list", "$argon2id$v=19$m=19456,t=2$" + strings.Join(parts[4:], "$")},
		{"salt is not base64", "$argon2id$v=19$m=19456,t=2,p=1$!!!!!!!!!!!!!!!!!!!!!!$" + parts[5]},
		{"tag is not base64", "$argon2id$v=19$m=19456,t=2,p=1$" + parts[4] + "$!!!!"},
		{"salt truncated", "$argon2id$v=19$m=19456,t=2,p=1$" + parts[4][:20] + "$" + parts[5]},
		{"tag truncated", "$argon2id$v=19$m=19456,t=2,p=1$" + parts[4] + "$" + parts[5][:40]},
		{"leading separator missing", strings.TrimPrefix(valid, "$")},
	}

	for _, c := range cases {
		ok, err := VerifyPassword(c.phc, "anything")
		if err == nil {
			t.Errorf("%s: VerifyPassword(%q) returned no error -- a malformed hash must be distinguishable from a wrong password", c.name, c.phc)
		}
		if ok {
			t.Errorf("%s: VerifyPassword(%q) = true", c.name, c.phc)
		}
	}
}
