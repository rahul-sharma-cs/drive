package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/testutil"
)

// The harness owns the server. This test proves the three mechanics the
// interruption battery is built on -- spawn on a private port, SIGKILL through
// the process handle, restart on the same port -- and the invariant that makes
// them safe to use: everything that matters is in Postgres, so a restarted
// server serves the same tree as the one that died.
func TestServerSurvivesSigkillAndRestart(t *testing.T) {
	srv := H.SpawnServer(t)
	ctx := context.Background()

	owner := H.NewUser(t)
	on := owner.At(srv.URL)

	folder := on.CreateFolder(t, owner.RootID, "written-before-the-kill")

	if !srv.Healthy(ctx) {
		t.Fatal("the spawned server is not healthy before the kill")
	}
	if err := srv.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if srv.Healthy(ctx) {
		t.Fatal("the server still answers /healthz after SIGKILL")
	}

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}

	got := on.Get(t, "/api/nodes/"+folder.ID.String()).Expect(http.StatusOK).Node()
	if got.Name != folder.Name {
		t.Errorf("after the restart the folder is %q, want %q", got.Name, folder.Name)
	}

	// The shared server was never touched: a private port is what keeps one
	// test's interruption out of every other test.
	if !H.Server.Healthy(ctx) {
		t.Error("the shared server died along with the spawned one")
	}
}

// A second server on a second port shares the one database, which is what lets
// the battery run several children without a schema per test.
func TestTwoServersShareTheDatabase(t *testing.T) {
	a := H.SpawnServer(t)
	b := H.SpawnServer(t)

	owner := H.NewUser(t)
	folder := owner.At(a.URL).CreateFolder(t, owner.RootID, "written-through-a")

	got := owner.At(b.URL).Get(t, "/api/nodes/"+folder.ID.String()).Expect(http.StatusOK).Node()
	if got.ID != folder.ID {
		t.Errorf("server b returned node %s, want %s", got.ID, folder.ID)
	}
}

// Time control: a 30-day session expires the moment its row says it has, with
// nothing sleeping. Every durable deadline in Drive works this way, which is
// why the battery can test multi-day resume windows in milliseconds.
func TestSessionExpiryIsControlledBySQL(t *testing.T) {
	user := H.NewUser(t)
	user.Get(t, "/api/auth/me").Expect(http.StatusOK)

	testutil.ExpireSessions(t, H.Pool, user.ID)

	resp := user.Get(t, "/api/auth/me").Expect(http.StatusUnauthorized)
	if resp.Code() != "unauthorized" {
		t.Errorf("code = %q, want unauthorized", resp.Code())
	}
}

// The other half of time control: a durable throttle window. Ten failures lock
// the address out, and backdating window_start -- not waiting fifteen minutes
// -- releases it.
func TestLoginLockoutReleasedByBackdatingTheWindow(t *testing.T) {
	user := H.NewUser(t)
	anon := H.Anonymous(t)

	wrong := map[string]any{"email": user.Email, "password": "not-the-password"}
	for i := range 10 {
		if resp := anon.Post(t, "/api/auth/login", wrong); resp.Status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401 (body %s)", i+1, resp.Status, resp.Body)
		}
	}

	// The budget is spent, so even the right password is refused.
	right := map[string]any{"email": user.Email, "password": testutil.FixturePassword}
	resp := anon.Post(t, "/api/auth/login", right).Expect(http.StatusTooManyRequests)
	if resp.Code() != "rate_limited" {
		t.Errorf("code = %q, want rate_limited", resp.Code())
	}

	testutil.LapseThrottleWindow(t, H.Pool, "login", user.Email, 20*time.Minute)

	anon.Post(t, "/api/auth/login", right).Expect(http.StatusOK)
}

// The authz matrix proves "nothing was written" with a digest. That proof is
// only worth anything if the digest actually moves when something is written --
// a digest that is constant would make every one of those assertions vacuous.
func TestDigestNoticesWrites(t *testing.T) {
	owner := H.NewUser(t)
	other := H.NewUser(t)

	base := testutil.Digest(t, H.Pool, owner.ID, other.ID)

	folder := owner.CreateFolder(t, owner.RootID, "digest-probe")
	afterCreate := testutil.Digest(t, H.Pool, owner.ID, other.ID)
	if afterCreate == base {
		t.Fatal("creating a folder did not change the digest")
	}

	owner.Patch(t, "/api/nodes/"+folder.ID.String(), map[string]any{"name": "digest-probe-2"}).
		Expect(http.StatusOK)
	if testutil.Digest(t, H.Pool, owner.ID, other.ID) == afterCreate {
		t.Error("renaming a folder did not change the digest")
	}

	// A blob refcount moves the digest too, which is what makes the rejected
	// copy rows meaningful.
	file := H.CreateFile(t, owner.ID, owner.RootID, "digest-probe.bin", []byte("bytes"))
	beforeCopy := testutil.Digest(t, H.Pool, owner.ID, other.ID)
	owner.Post(t, "/api/nodes/"+file.String()+"/copy", map[string]any{"parent_id": folder.ID}).
		Expect(http.StatusCreated)
	if testutil.Digest(t, H.Pool, owner.ID, other.ID) == beforeCopy {
		t.Error("copying a file did not change the digest")
	}
	if got := H.Refcount(t, file); got != 2 {
		t.Errorf("refcount after the copy = %d, want 2", got)
	}

	// And it is scoped: another user's writes are invisible to a digest taken
	// for this owner alone.
	ownerOnly := testutil.Digest(t, H.Pool, owner.ID)
	other.CreateFolder(t, other.RootID, "somebody-elses-folder")
	if testutil.Digest(t, H.Pool, owner.ID) != ownerOnly {
		t.Error("another user's write changed a digest scoped to this owner")
	}
}

// Global setup sweeps leftover multipart uploads, because a prior failed run --
// the kill -9 battery above all -- leaves initiated multiparts in Garage that
// no session row references and nothing else ever collects.
//
// Setup normally finds an empty bucket, so without this the sweep would only
// ever be exercised by an actual failed run. This creates one leftover and
// collects it. Never give this test t.Parallel(): it aborts every multipart
// in the bucket, and the upload tests hold live ones.
func TestSweepMultipartsCollectsAbandonedUploads(t *testing.T) {
	ctx := context.Background()
	bucket := H.Cfg.S3Bucket

	// No ContentType, matching the upload path: Garage skips the
	// response-content-* overrides on Range responses, so objects must carry
	// no renderable type of their own.
	created, err := H.S3.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("blobs/" + uuid.NewString()),
	})
	if err != nil {
		t.Fatalf("creating a multipart upload: %v", err)
	}

	swept, err := testutil.SweepMultiparts(ctx, H.S3, bucket)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if swept < 1 {
		t.Fatalf("swept %d uploads, want at least the one just created (%s)",
			swept, aws.ToString(created.UploadId))
	}

	left, err := H.S3.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("listing multipart uploads after the sweep: %v", err)
	}
	if len(left.Uploads) != 0 {
		t.Errorf("%d multipart upload(s) survived the sweep", len(left.Uploads))
	}
}

// testutil.RunGCOnce is assigned by this package's init (interruption_test.go),
// which is what lets any test in the suite force a collection instead of
// waiting out the hourly schedule. If that wiring ever disappears, every test
// that calls testutil.GC would otherwise fail with a confusing nil hook, so
// this one says it plainly and runs a real pass against an idle stack.
func TestGCHookIsNamedAndNotSilentlyMissing(t *testing.T) {
	if testutil.RunGCOnce != nil {
		testutil.GC(t, context.Background())
		return
	}
	t.Error("testutil.RunGCOnce is not wired -- this package's init must assign it")
}
