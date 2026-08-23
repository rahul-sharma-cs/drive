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

// The scheme is not decoration: url.Parse never fails on a value without one,
// and everything it produces instead has an empty Hostname().
//
// "garage:3900" parses as the scheme "garage" with the opaque part "3900";
// "store.example/bucket" parses as a bare path. The guard used to accept an
// empty hostname as "local", so the single most likely typo -- a hosted
// endpoint pasted without its https:// -- was the single value it let through,
// straight into overwriting that store's CORS rule with one trusting localhost.
func TestInfraInitRefusesASchemelessEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"garage:3900",
		"localhost:3900",
		"0123456789abcdef0123456789abcdef.r2.example",
		"store.example/drive-blobs",
		"//localhost:3900",
		"ftp://localhost:3900",
		"http:///drive-blobs", // a scheme, but still no host
		"",
	} {
		err := guardLocalEndpoint(endpoint)
		if err == nil {
			t.Errorf("guardLocalEndpoint(%q) = nil; a value with no hostname cannot be checked", endpoint)
			continue
		}
		if !strings.Contains(err.Error(), "DRIVE_S3_ENDPOINT") {
			t.Errorf("guardLocalEndpoint(%q) = %q, want it to name the variable", endpoint, err)
		}
	}

	// And the same hosts still pass once they carry a scheme, so the guard did
	// not simply get stricter about everything.
	for _, endpoint := range []string{"http://garage:3900", "http://localhost:3900", "https://localhost"} {
		if err := guardLocalEndpoint(endpoint); err != nil {
			t.Errorf("guardLocalEndpoint(%q): %v", endpoint, err)
		}
	}
}
