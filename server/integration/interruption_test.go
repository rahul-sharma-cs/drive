package integration

// The interruption battery: what survives a server that stops existing.
//
// Two shapes of crash live here, and they are tested two different ways on
// purpose.
//
// The first is timed, because it can be: SIGKILL the server mid-transfer. The
// bytes are going to Garage, not to the server, so the interesting window --
// a part PUT that landed while its confirmation was lost -- is seconds wide and
// can simply be arranged.
//
// The second is constructed, because it cannot be timed. The window between
// CompleteMultipartUpload returning and the publish transaction committing is
// milliseconds; no SIGKILL lands there on demand. PLAN §Testing 3 says so
// outright, so those cases put the world into that state by hand -- SQL state
// injection plus out-of-band S3 calls -- and then run one GC pass.

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/rahul-sharma-cs/drive/server/internal/gc"
	"github.com/rahul-sharma-cs/drive/server/internal/testutil"
)

// The GC hook PLAN §Testing 3 names, wired from this suite so no test ever
// waits an hour for a collection pass. It is an init rather than a line in
// TestMain because H does not exist yet at init time -- the closure reads it
// when it runs, which is always inside a test.
func init() {
	testutil.RunGCOnce = func(ctx context.Context) error {
		return gc.New(H.Pool, H.S3, H.Cfg.S3Bucket, slog.Default()).RunOnce(ctx)
	}
}

// TestInterruptionKill9MidUpload is the headline case: kill the server in the
// middle of a real transfer, bring it back, resume, and get the same bytes.
//
// It deliberately arranges the ugly variant of the window -- a part whose PUT
// reached Garage while its confirmation died with the server. The ledger and
// the store now disagree, which the handshake must reconcile in Garage's
// favour, and which arms the chimera guard on the way through.
func TestInterruptionKill9MidUpload(t *testing.T) {
	child := H.SpawnServer(t)
	owner := H.NewUser(t).At(child.URL)
	folder := owner.CreateFolder(t, owner.RootID, "kill-9")

	data := testutil.RandomBytes(loopFileSize, 21)
	up := H.NewUpload(t, owner, folder.ID, "survivor.bin", data)
	created := up.MustCreate(t, http.StatusCreated)
	if created.PartsTotal < 4 {
		t.Fatalf("parts_total %d is too few to interrupt meaningfully", created.PartsTotal)
	}

	up.UploadParts(t, 1, 2)

	// Part 3 lands in Garage but is never confirmed: this is the kill-9 window.
	orphan := up.PutPart(t, 3, up.URLFor(t, 3, false))
	if orphan.Status != http.StatusOK {
		t.Fatalf("part 3 PUT answered %d: %s", orphan.Status, orphan.Body)
	}

	if err := child.Kill(); err != nil {
		t.Fatalf("killing the server: %v", err)
	}
	if child.Healthy(context.Background()) {
		t.Fatal("the server answered /healthz after SIGKILL")
	}

	// The API vanishing is what the client has to survive. Confirmations fail
	// as transport errors while Garage keeps accepting bytes.
	if _, err := up.TryConfirm(t, 3, orphan.ETag, orphan.MD5, orphan.Size); err == nil {
		t.Fatal("a confirmation against a killed server succeeded")
	}
	// A URL from the create batch still works while the API is down: the bytes
	// never went through the server in the first place.
	later := up.PutPart(t, 4, up.URLFor(t, 4, false))
	if later.Status != http.StatusOK {
		t.Fatalf("a part PUT during the outage answered %d: %s", later.Status, later.Body)
	}

	// Everything durable is in Postgres, so a restart on the same port picks up
	// exactly where the ledger left off.
	if err := child.Start(context.Background()); err != nil {
		t.Fatalf("restarting the server: %v", err)
	}

	// The first handshake after the interruption reconciles and, because it
	// found drift, arms verification before it hands out a single URL.
	bounce := up.Resume(t, nil)
	if len(bounce.VerifyParts) == 0 {
		t.Fatal("reconciliation found drift but did not arm verification")
	}
	if bounce.Missing != nil {
		t.Fatalf("an armed resume leaked %d URLs", len(bounce.Missing))
	}
	if !covers(bounce.ConfirmedParts, 3, 4) {
		t.Fatalf("confirmed_parts %v after reconciliation, want parts 3 and 4 adopted from Garage",
			bounce.ConfirmedParts)
	}

	after := up.ResumeVerified(t)
	if len(after.VerifyParts) != 0 {
		t.Fatalf("verification stayed armed after the pins were proved: %v", after.VerifyParts)
	}
	for _, p := range after.Missing {
		if p.PartNumber <= 4 {
			t.Fatalf("resume re-offered part %d, which Garage already holds", p.PartNumber)
		}
	}

	up.UploadAll(t)
	done := up.MustComplete(t)
	if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the file that survived a kill -9 is not byte-identical")
	}
}

// TestInterruptionCrashAfterCompleteMultipart is the window PLAN calls out by
// name: CompleteMultipartUpload succeeded, the publish transaction did not.
//
// Constructed, never timed. Once Garage's complete lands the multipart ceases
// to exist -- ListParts and a retried complete both answer NoSuchUpload -- so
// recovery must HEAD the object, find it whole, and publish. Flipping such a
// session back to 'active' would ask the client to re-upload bytes that are
// already durable, into a multipart that no longer exists.
func TestInterruptionCrashAfterCompleteMultipart(t *testing.T) {
	ctx := context.Background()
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "post-complete")

	data := testutil.RandomBytes(25<<20, 22)
	up := H.NewUpload(t, owner, folder.ID, "landed.bin", data)
	up.MustCreate(t, http.StatusCreated)
	up.UploadAll(t)

	row := H.Session(t, up.ID)
	if row.S3UploadID == nil {
		t.Fatal("the session has no multipart upload to complete")
	}

	// Out of band, exactly as the dead process would have: a part's ETag is its
	// MD5, so the ledger's ETags are reproducible from the source bytes.
	etags := map[int]string{}
	for n := 1; n <= up.PartsTotal; n++ {
		etags[n] = up.PartMD5(t, n)
	}
	H.OOBCompleteMultipart(t, row.ObjectKey, *row.S3UploadID, etags)

	// ...and then the process died before it could publish.
	H.InjectCompleting(t, up.ID, 30*time.Minute)

	testutil.GC(t, ctx)

	recovered := H.Session(t, up.ID)
	if recovered.Status != "done" {
		t.Fatalf("session status %q after recovery, want done", recovered.Status)
	}
	if recovered.NodeID == nil {
		t.Fatal("recovery finished the session without publishing a node")
	}
	if got := testutil.SHA256Hex(H.DownloadNode(t, *recovered.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the recovered file is not byte-identical")
	}
	names := owner.Get(t, "/api/nodes/"+folder.ID.String()+"/children").Expect(http.StatusOK).List().Names()
	if !contains(names, "landed.bin") {
		t.Fatalf("the folder holds %v, want landed.bin", names)
	}

	// The client's retry of complete finds the published node rather than a
	// second publish.
	var again testutil.CompletedUpload
	up.Complete(t).Expect(http.StatusOK).JSON(&again)
	if again.NodeID != *recovered.NodeID {
		t.Fatalf("a retried complete published %s, want the recovered %s", again.NodeID, *recovered.NodeID)
	}
	if n := H.CountRows(t, "nodes", "parent_id = $1 AND deleted_at IS NULL", folder.ID); n != 1 {
		t.Fatalf("%d nodes published for one upload", n)
	}
}

// TestInterruptionCrashMidComplete is the other half of the same window: the
// finalizer claimed the session and died before it reached Garage at all. The
// multipart is still open, so recovery runs the complete itself.
func TestInterruptionCrashMidComplete(t *testing.T) {
	ctx := context.Background()
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "mid-complete")

	data := testutil.RandomBytes(25<<20, 23)
	up := H.NewUpload(t, owner, folder.ID, "claimed.bin", data)
	up.MustCreate(t, http.StatusCreated)
	up.UploadAll(t)

	// A claim that is young is somebody's live finalize, and the collector must
	// leave it alone.
	H.InjectCompleting(t, up.ID, time.Minute)
	testutil.GC(t, ctx)
	if s := H.Session(t, up.ID); s.Status != "completing" || s.NodeID != nil {
		t.Fatalf("the collector took over a fresh claim: %+v", s)
	}
	// A live finalize is also what a second complete must be told about.
	if code := up.Complete(t).Expect(http.StatusConflict).Code(); code != "in_progress" {
		t.Fatalf("complete against a claimed session answered %q, want in_progress", code)
	}

	// Old enough now: the process that held it is not coming back.
	H.InjectCompleting(t, up.ID, 30*time.Minute)
	testutil.GC(t, ctx)

	recovered := H.Session(t, up.ID)
	if recovered.Status != "done" || recovered.NodeID == nil {
		t.Fatalf("session %+v after recovery, want done with a node", recovered)
	}
	if got := testutil.SHA256Hex(H.DownloadNode(t, *recovered.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the file finished by the collector is not byte-identical")
	}
	if n := H.CountMultiparts(t); n != 0 {
		t.Fatalf("%d multipart upload(s) left open after recovery", n)
	}
}

// TestInterruptionExpiredSessionIsCollected checks the other direction: an
// upload nobody ever finished is retired, and its multipart with it, so an
// abandoned 50 GB transfer does not sit in Garage forever.
func TestInterruptionExpiredSessionIsCollected(t *testing.T) {
	ctx := context.Background()
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "abandoned")

	up := H.NewUpload(t, owner, folder.ID, "abandoned.bin", testutil.RandomBytes(smallFileSize, 24))
	up.MustCreate(t, http.StatusCreated)
	up.UploadPart(t, 1)

	before := H.CountMultiparts(t)
	H.ExpireUpload(t, up.ID)
	testutil.GC(t, ctx)

	if s := H.Session(t, up.ID); s.Status != "aborted" {
		t.Fatalf("an expired session is %q after a GC pass, want aborted", s.Status)
	}
	if after := H.CountMultiparts(t); after >= before {
		t.Fatalf("%d multipart uploads before collection, %d after: nothing was discarded", before, after)
	}
	up.TryResume(t, nil).Expect(http.StatusGone)
}

// covers reports whether every wanted part number is in the confirmed set.
func covers(confirmed []int, want ...int) bool {
	have := make(map[int]bool, len(confirmed))
	for _, n := range confirmed {
		have[n] = true
	}
	for _, n := range want {
		if !have[n] {
			return false
		}
	}
	return true
}
