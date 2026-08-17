package main

import (
	"strings"
	"testing"
)

// infra-init writes a bucket CORS rule naming a localhost origin. Pointed at a
// hosted store, succeeding would be worse than failing: it would replace that
// store's own rule with one that trusts a developer's laptop.
func TestInfraInitRefusesARemoteObjectStore(t *testing.T) {
	for _, endpoint := range []string{
		"http://localhost:3900",
		"http://127.0.0.1:3910",
		"http://garage:3900",
	} {
		if err := guardLocalEndpoint(endpoint); err != nil {
			t.Errorf("guardLocalEndpoint(%q): %v", endpoint, err)
		}
	}

	// A fabricated account id. The real one must never appear in a tracked
	// file: it is half of a hosted endpoint, the repo is public, and a value
	// copied out of an env file into a test is exactly how that happens.
	const remote = "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com"
	err := guardLocalEndpoint(remote)
	if err == nil {
		t.Fatal("a hosted object store was accepted")
	}
	if !strings.Contains(err.Error(), "r2.cloudflarestorage.com") {
		t.Errorf("error = %q, want it to name the host it refused", err)
	}
}
