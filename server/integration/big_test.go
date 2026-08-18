package integration

// The big runs. None of these belong in the per-loop battery -- they take
// minutes and tens of gigabytes -- so each one is behind an environment guard
// and skips by default:
//
//	DRIVE_TEST_BIG=1  multi-GB round trip + the >1,000-part pagination case
//	DRIVE_TEST_50G=1  the sparse 50 GB run, opt-in even among the big ones
//
// They exercise the same code paths the loop battery does. What they add is
// scale: enough parts to force ListParts pagination, and enough bytes that a
// mistake in slicing or offsets shows up as a digest mismatch instead of
// passing by luck.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/testutil"
)

// gib is the unit these fixtures are measured in.
const gib = 1 << 30

// requireBig skips unless the named guard is set, so `go test ./...` never
// walks into a twenty-minute upload by accident.
func requireBig(t *testing.T, envVar string) {
	t.Helper()
	if os.Getenv(envVar) != "1" {
		t.Skipf("set %s=1 to run this; it is not part of the per-loop battery", envVar)
	}
}

// fixtureDir is where generated big files live: gitignored, and reused across
// runs so a second invocation does not rebuild 50 GB.
func fixtureDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(H.RepoRoot, "e2e", "fixtures", "big")
}

// TestBigMultiGBRoundTrip is the multi-GB random-file run: real bytes, real
// parts, and a digest taken from what came back out of Garage rather than from
// what the client believed it sent.
func TestBigMultiGBRoundTrip(t *testing.T) {
	requireBig(t, "DRIVE_TEST_BIG")

	size := int64(2 * gib)
	if v := os.Getenv("DRIVE_TEST_BIG_SIZE"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("DRIVE_TEST_BIG_SIZE=%q is not a byte count: %v", v, err)
		}
		size = parsed
	}

	dir := fixtureDir(t)
	testutil.RequireFreeSpace(t, H.RepoRoot, 2*size)
	requireDockerSpace(t, 2*size)

	path := testutil.BigFixture(t, dir, fmt.Sprintf("random-%d.bin", size), size, false)
	src, actual := testutil.OpenFixture(t, path)

	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "big")
	up := H.NewUploadFrom(t, owner, folder.ID, "big-random.bin", src, actual)

	started := time.Now()
	created := up.MustCreate(t, 201)
	t.Logf("%d bytes, part_size %d, parts_total %d", actual, created.PartSize, created.PartsTotal)

	up.UploadAll(t)
	done := up.MustComplete(t)
	elapsed := time.Since(started)
	t.Logf("uploaded in %s (%.1f MiB/s)", elapsed.Round(time.Second),
		float64(actual)/(1<<20)/elapsed.Seconds())

	cleanupBig(t, owner, done.NodeID)

	if got, want := H.DigestNode(t, done.NodeID), up.SHA256(t); got != want {
		t.Fatalf("downloaded sha256 %s, want %s", got, want)
	}
}

// TestBigListPartsPagination is the >1,000-part case. Garage returns at most
// 1,000 parts per ListParts page, and reconciliation and the complete-time
// verify both walk that list -- a loop that stops at the first page would
// silently declare 1,000 of 1,126 parts complete.
//
// The fixture is sparse: 11 GiB of hole costs no disk, and the part count is
// what is under test, not the entropy.
func TestBigListPartsPagination(t *testing.T) {
	requireBig(t, "DRIVE_TEST_BIG")

	const size = 11 * gib // 1,126 parts at the test stack's 10 MiB part size
	dir := fixtureDir(t)
	requireDockerSpace(t, 2*gib) // zstd collapses the hole; this is headroom, not size

	path := testutil.BigFixture(t, dir, "sparse-11g.bin", size, true)
	src, actual := testutil.OpenFixture(t, path)

	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "pagination")
	up := H.NewUploadFrom(t, owner, folder.ID, "sparse-11g.bin", src, actual)

	created := up.MustCreate(t, 201)
	if created.PartsTotal <= 1000 {
		t.Fatalf("parts_total %d does not exceed one ListParts page; this case proves nothing",
			created.PartsTotal)
	}
	t.Logf("%d bytes, part_size %d, parts_total %d", actual, created.PartSize, created.PartsTotal)

	up.UploadAll(t)

	// The handshake reconciles against a paginated ListParts. Every part must
	// come back confirmed, including the ones past the first page.
	resumed := up.ResumeVerified(t)
	if len(resumed.ConfirmedParts) != created.PartsTotal {
		t.Fatalf("reconciliation saw %d of %d parts: the ListParts loop stopped early",
			len(resumed.ConfirmedParts), created.PartsTotal)
	}
	if len(resumed.Missing) != 0 {
		t.Fatalf("resume re-offered %d parts that Garage already holds", len(resumed.Missing))
	}

	done := up.MustComplete(t)
	cleanupBig(t, owner, done.NodeID)

	if got, want := H.DigestNode(t, done.NodeID), up.SHA256(t); got != want {
		t.Fatalf("downloaded sha256 %s, want %s", got, want)
	}
}

// TestBigFiftyGB is the acceptance walkthrough's 50 GB run. The file is sparse,
// created with /usr/bin/truncate, so it costs no disk on the host; Garage's
// zstd collapses the zeros on the way in.
func TestBigFiftyGB(t *testing.T) {
	requireBig(t, "DRIVE_TEST_50G")

	dir := fixtureDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	path := filepath.Join(dir, "sparse-50g.bin")

	// truncate's G is 1024-based, so the reuse check has to be too.
	if st, err := os.Stat(path); err != nil || st.Size() != 50*gib {
		cmd := exec.Command("/usr/bin/truncate", "-s", "50G", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("truncate -s 50G %s: %v\n%s", path, err, out)
		}
	}
	src, size := testutil.OpenFixture(t, path)

	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "fifty")
	up := H.NewUploadFrom(t, owner, folder.ID, "sparse-50g.bin", src, size)

	started := time.Now()
	created := up.MustCreate(t, 201)
	t.Logf("%d bytes, part_size %d, parts_total %d", size, created.PartSize, created.PartsTotal)

	up.UploadAll(t)
	done := up.MustComplete(t)
	elapsed := time.Since(started)
	t.Logf("50 GB in %s (%.1f MiB/s)", elapsed.Round(time.Second),
		float64(size)/(1<<20)/elapsed.Seconds())

	cleanupBig(t, owner, done.NodeID)

	if got, want := H.DigestNode(t, done.NodeID), up.SHA256(t); got != want {
		t.Fatalf("downloaded sha256 %s, want %s", got, want)
	}
}

// cleanupBig gives the bytes back: purge the node, age the blob past its grace,
// and run one collection pass. Without it a handful of big runs fills the
// Docker VM and every later suite fails for reasons that look like protocol
// bugs.
func cleanupBig(t *testing.T, owner *testutil.Client, nodeID uuid.UUID) {
	t.Helper()
	key := H.ObjectKeyOf(t, nodeID)
	t.Cleanup(func() {
		ctx := context.Background()
		purge(t, owner, nodeID)
		testutil.Backdate(t, H.Pool, "blobs", "unreferenced_at", blobGrace, "object_key = $1", key)
		testutil.GC(t, ctx)
		if H.ObjectExists(t, key) {
			t.Errorf("the big run's object %s was not reclaimed", key)
		}
	})
}

// requireDockerSpace checks the Docker VM's free space through a container that
// has a shell, which the Garage image does not. A big run needs twice the file
// free; the probe failing is logged, not fatal -- a missing docker
// CLI is not a reason to fail an upload test.
func requireDockerSpace(t *testing.T, want int64) {
	t.Helper()

	cmd := exec.Command("docker", "compose", "-p", "drive-test", "--env-file", ".env.test",
		"exec", "-T", "postgres", "df", "-k", "/var/lib/postgresql/data")
	cmd.Dir = H.RepoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("could not probe the Docker VM's free space: %v\n%s", err, out)
		return
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		t.Logf("unexpected df output:\n%s", out)
		return
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		t.Logf("unexpected df output:\n%s", out)
		return
	}
	availKB, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		t.Logf("unexpected df available column %q", fields[3])
		return
	}
	if free := availKB * 1024; free < want {
		t.Skipf("the Docker VM has %d bytes free, this run needs %d", free, want)
	}
}
