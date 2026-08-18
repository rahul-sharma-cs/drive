package api

// The storage meter's endpoint. What matters here is that the number it
// reports is the same number the upload path refuses on -- a meter that
// disagrees with the refusal is worse than no meter.

import (
	"net/http"
	"testing"

	"github.com/rahul-sharma-cs/drive/server/internal/config"
)

func TestUsageReportsTheOwnersBytesAndTheirQuota(t *testing.T) {
	const quota = 10 << 20
	h, pool := capsServer(t, func(c *config.Config) {
		c.UserQuota = quota
		c.MaxFileSize = 4 << 20
	})
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)
	folder := nodeMkFolder(t, pool, owner, owner.RootID, "docs")

	read := func(who *http.Cookie) usageResponse {
		t.Helper()
		rec := authDo(t, h, http.MethodGet, "/api/usage", nil, who)
		nodeWant(t, rec, http.StatusOK, "")
		var out usageResponse
		nodeDecode(t, rec, &out)
		return out
	}

	before := read(owner.Cookie)
	if before.Quota == nil || *before.Quota != quota {
		t.Fatalf("quota = %v, want %d", before.Quota, quota)
	}
	if before.MaxFile == nil || *before.MaxFile != 4<<20 {
		t.Fatalf("max_file_size = %v, want %d", before.MaxFile, 4<<20)
	}

	// nodeMkFile writes an 11-byte file, so the owner's number moves by 11 and
	// nobody else's does.
	strangerBefore := read(stranger.Cookie)
	nodeMkFile(t, pool, owner, folder, "notes.txt")

	if after := read(owner.Cookie); after.Used != before.Used+11 {
		t.Errorf("used = %d, want %d (it must count the owner's own files)", after.Used, before.Used+11)
	}
	if after := read(stranger.Cookie); after.Used != strangerBefore.Used {
		t.Errorf("another user's usage moved to %d from %d -- usage is per owner", after.Used, strangerBefore.Used)
	}
}

// Zero means "no cap" everywhere else in the config, so the wire has to say
// that rather than reporting a quota of zero, which renders as a full meter
// over an empty allowance.
func TestUsageReportsNoQuotaAsNull(t *testing.T) {
	h, pool := capsServer(t, func(c *config.Config) { c.UserQuota = 0; c.MaxFileSize = 0 })
	owner := nodeNewUser(t, pool)

	rec := authDo(t, h, http.MethodGet, "/api/usage", nil, owner.Cookie)
	nodeWant(t, rec, http.StatusOK, "")
	var out usageResponse
	nodeDecode(t, rec, &out)

	if out.Quota != nil {
		t.Errorf("quota = %d, want null", *out.Quota)
	}
	if out.MaxFile != nil {
		t.Errorf("max_file_size = %d, want null", *out.MaxFile)
	}
}

func TestUsageRequiresAuth(t *testing.T) {
	h, _ := capsServer(t, func(c *config.Config) {})

	rec := authDo(t, h, http.MethodGet, "/api/usage", nil, nil)
	nodeWant(t, rec, http.StatusUnauthorized, CodeUnauthorized)
}
