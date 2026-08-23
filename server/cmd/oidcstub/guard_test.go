package main

import (
	"strings"
	"testing"
)

// The fixture signs an identity assertion for any subject and address it is
// handed. Anywhere but loopback that is a machine that will vouch for anybody
// to anybody, so the guard is the whole safety story -- and unlike `make
// seed`'s, it has no -force, because there is no non-local use of this to
// escape to.
func TestOIDCStubServesOnlyLoopback(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:9099",
		"127.0.0.5:9099",
		"localhost:9099",
		"[::1]:9099",
	} {
		if err := guardLoopback(addr); err != nil {
			t.Errorf("guardLoopback(%q): %v", addr, err)
		}
	}

	// Documentation addresses and a .test hostname: a real one must never be
	// typed into a tracked file, and nothing here is about the value anyway --
	// only about its shape.
	for _, addr := range []string{
		// The mistake this exists to catch: a bare port binds every interface,
		// which on a laptop on a café network is the whole café.
		":9099",
		"0.0.0.0:9099",
		"[::]:9099",
		"192.0.2.10:9099",
		"stub.example.test:9099",
	} {
		err := guardLoopback(addr)
		if err == nil {
			t.Errorf("guardLoopback(%q) accepted a non-loopback address", addr)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("error for %q = %q, want it to say why", addr, err)
		}
	}

	// And an address that is not host:port at all is refused rather than
	// silently treated as a host.
	if err := guardLoopback("9099"); err == nil {
		t.Error("guardLoopback accepted a value that is not host:port")
	}
}

// Without both credentials the stub would accept any client, which would make
// every exchange assertion vacuous.
func TestOIDCStubNeedsAClient(t *testing.T) {
	for _, c := range [][2]string{{"", ""}, {"an-id", ""}, {"", "a-secret"}} {
		if err := run("127.0.0.1:0", c[0], c[1]); err == nil {
			t.Errorf("run with id=%q secret=%q started anyway", c[0], c[1])
		}
	}
}
