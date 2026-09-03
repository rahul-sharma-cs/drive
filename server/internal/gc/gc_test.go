package gc

// The collector against the real drive-test stack.
//
// Time is moved by backdating rows, never by sleeping -- every deadline the GC
// applies is a timestamptz compared against now(). The one exception is
// Garage's multipart Initiated timestamp, which lives in Garage and cannot be
// backdated at all; that is why the thresholds are Config fields, and why the
// orphan-multipart rule is tested from both sides (the real 24 h default must
// leave a fresh multipart alone; a zeroed threshold must collect an unclaimed
// one).
//
// The suite shares the drive-test database with every other package, so every
// assertion names its own rows. Counting anything globally would be measuring
// other suites' residue.

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/db"
	"github.com/rahul-sharma-cs/drive/server/internal/node"
	"github.com/rahul-sharma-cs/drive/server/internal/share"
	"github.com/rahul-sharma-cs/drive/server/internal/testutil"
	"github.com/rahul-sharma-cs/drive/server/internal/upload"
)

const gcTestPartSize int64 = 1 << 20

func gcEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var (
	gcOnce    sync.Once
	gcPool    *pgxpool.Pool
	gcS3      *s3.Client
	gcBucket  string
	gcInitErr error
)

func gcSetup(t *testing.T) (*pgxpool.Pool, *s3.Client, string) {
	t.Helper()
	gcOnce.Do(func() {
		dsn := gcEnv("DRIVE_DB_DSN", "postgres://drive:drive@localhost:55433/drive?sslmode=disable")
		if strings.Contains(dsn, ":55432") {
			gcInitErr = fmt.Errorf("DRIVE_DB_DSN points at the dev stack (%s); tests run against drive-test on :55433", dsn)
			return
		}
		cfg := &config.Config{
			S3Endpoint:  gcEnv("DRIVE_S3_ENDPOINT", "http://localhost:3910"),
			S3Bucket:    gcEnv("DRIVE_S3_BUCKET", "drive-blobs"),
			S3AccessKey: gcEnv("DRIVE_S3_ACCESS_KEY", "drivetestkey0001"),
			S3SecretKey: gcEnv("DRIVE_S3_SECRET_KEY", "drivetestsecretkey0001"),
		}
		if strings.Contains(cfg.S3Endpoint, ":3900") {
			gcInitErr = fmt.Errorf("DRIVE_S3_ENDPOINT points at the dev stack (%s); tests run against drive-test on :3910", cfg.S3Endpoint)
			return
		}
		ctx := context.Background()
		if gcPool, gcInitErr = db.Connect(ctx, dsn); gcInitErr != nil {
			return
		}
		if gcInitErr = db.Migrate(ctx, gcPool); gcInitErr != nil {
			return
		}
		gcS3, _, gcInitErr = blob.New(ctx, cfg)
		gcBucket = cfg.S3Bucket
	})
	if gcInitErr != nil {
		t.Fatalf("drive-test stack: %v (is `docker compose -p drive-test --env-file .env.test up -d` running?)", gcInitErr)
	}
	return gcPool, gcS3, gcBucket
}

// world is one test's private user plus a collector aimed at the shared stack.
type world struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	s3     *s3.Client
	bucket string
	gc     *GC
	nodes  *node.Store
	store  *upload.Store
	user   uuid.UUID
	root   uuid.UUID
}

func newWorld(t *testing.T) *world {
	t.Helper()
	pool, s3c, bucket := gcSetup(t)
	ctx := context.Background()

	w := &world{
		t: t, ctx: ctx, pool: pool, s3: s3c, bucket: bucket,
		gc:    New(pool, s3c, bucket, slog.New(slog.DiscardHandler)),
		nodes: node.NewStore(pool),
		store: upload.NewStore(pool),
		user:  uuid.New(),
		root:  uuid.New(),
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash, display_name, email_verified_at)
		 VALUES ($1, $2, 'x', 'GC Test', now())`,
		w.user, "gc-"+w.user.String()+"@drive.test"); err != nil {
		t.Fatalf("creating the test user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name) VALUES ($1, $2, NULL, 'folder', 'My Drive')`,
		w.root, w.user); err != nil {
		t.Fatalf("creating the root folder: %v", err)
	}
	return w
}

func (w *world) run() {
	w.t.Helper()
	if err := w.gc.RunOnce(w.ctx); err != nil {
		w.t.Fatalf("RunOnce: %v", err)
	}
}

func (w *world) folder(name string) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name) VALUES ($1, $2, $3, 'folder', $4)`,
		id, w.user, w.root, name); err != nil {
		w.t.Fatalf("creating folder %q: %v", name, err)
	}
	return id
}

// file publishes a real object plus its blob and node rows -- the shape the
// upload path leaves behind, which the refcount battery then takes apart.
func (w *world) file(parentID uuid.UUID, name string, content []byte) (nodeID, blobID uuid.UUID, key string) {
	w.t.Helper()
	fixture, err := blob.PutFixture(w.ctx, w.s3, w.bucket, content)
	if err != nil {
		w.t.Fatalf("putting fixture %q: %v", name, err)
	}
	blobID, nodeID = uuid.New(), uuid.New()
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO blobs (id, object_key, size, sha256, etag, refcount) VALUES ($1,$2,$3,$4,$5,1)`,
		blobID, fixture.ObjectKey, fixture.Size, fixture.SHA256, fixture.ETag); err != nil {
		w.t.Fatalf("inserting blob for %q: %v", name, err)
	}
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name, blob_id, size, mime)
		 VALUES ($1,$2,$3,'file',$4,$5,$6,'application/octet-stream')`,
		nodeID, w.user, parentID, name, blobID, fixture.Size); err != nil {
		w.t.Fatalf("inserting node %q: %v", name, err)
	}
	return nodeID, blobID, fixture.ObjectKey
}

// session inserts an upload session with a real multipart upload behind it and
// its parts really uploaded and confirmed.
func (w *world) session(parentID uuid.UUID, name string, data []byte) *upload.Session {
	w.t.Helper()
	partSize, partsTotal, err := upload.ResolvePartSize(int64(len(data)), gcTestPartSize)
	if err != nil {
		w.t.Fatalf("resolving the part size: %v", err)
	}
	sess := &upload.Session{
		ID:          uuid.New(),
		UserID:      w.user,
		ParentID:    &parentID,
		FileName:    name,
		FileSize:    int64(len(data)),
		Fingerprint: "fp-" + uuid.NewString(),
		ObjectKey:   upload.NewObjectKey(),
		PartSize:    partSize,
		PartsTotal:  partsTotal,
		Mode:        upload.ModeDirect,
	}
	if len(data) > 0 {
		uploadID, err := (&upload.Presigner{S3: w.s3, Bucket: w.bucket}).CreateMultipart(w.ctx, sess.ObjectKey)
		if err != nil {
			w.t.Fatalf("creating the multipart upload: %v", err)
		}
		sess.S3UploadID = &uploadID
	}
	if err := w.store.Insert(w.ctx, sess); err != nil {
		w.t.Fatalf("inserting the session: %v", err)
	}
	for n := 1; n <= partsTotal; n++ {
		lo := int64(n-1) * partSize
		hi := min(lo+partSize, int64(len(data)))
		w.putPart(sess, n, data[lo:hi])
	}
	return sess
}

func (w *world) putPart(sess *upload.Session, n int, chunk []byte) {
	w.t.Helper()
	num := int32(n)
	out, err := w.s3.UploadPart(w.ctx, &s3.UploadPartInput{
		Bucket:     aws.String(w.bucket),
		Key:        aws.String(sess.ObjectKey),
		UploadId:   sess.S3UploadID,
		PartNumber: &num,
		Body:       bytes.NewReader(chunk),
	})
	if err != nil {
		w.t.Fatalf("uploading part %d: %v", n, err)
	}
	sum := md5.Sum(chunk)
	digest := hex.EncodeToString(sum[:])
	if err := w.store.ConfirmPart(w.ctx, sess.ID, upload.Part{
		Number: n, Size: int64(len(chunk)), ETag: upload.NormalizeETag(aws.ToString(out.ETag)), MD5: &digest,
	}); err != nil {
		w.t.Fatalf("confirming part %d: %v", n, err)
	}
}

func (w *world) status(id uuid.UUID) (status string, nodeID *uuid.UUID) {
	w.t.Helper()
	if err := w.pool.QueryRow(w.ctx,
		`SELECT status, node_id FROM upload_sessions WHERE id = $1`, id).Scan(&status, &nodeID); err != nil {
		w.t.Fatalf("reading session %s: %v", id, err)
	}
	return status, nodeID
}

func (w *world) objectBytes(key string) ([]byte, error) {
	out, err := w.s3.GetObject(w.ctx, &s3.GetObjectInput{
		Bucket: aws.String(w.bucket), Key: aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (w *world) objectExists(key string) bool {
	w.t.Helper()
	_, err := w.s3.HeadObject(w.ctx, &s3.HeadObjectInput{
		Bucket: aws.String(w.bucket), Key: aws.String(key),
	})
	if err == nil {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return false
	}
	var api smithy.APIError
	if errors.As(err, &api) && (api.ErrorCode() == "NotFound" || api.ErrorCode() == "NoSuchKey") {
		return false
	}
	w.t.Fatalf("heading %s: %v", key, err)
	return false
}

func (w *world) multipartExists(key, uploadID string) bool {
	w.t.Helper()
	_, err := w.s3.ListParts(w.ctx, &s3.ListPartsInput{
		Bucket: aws.String(w.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	if err == nil {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == "NoSuchUpload" {
		return false
	}
	var gone *types.NoSuchUpload
	if errors.As(err, &gone) {
		return false
	}
	w.t.Fatalf("listing parts of %s: %v", uploadID, err)
	return false
}

func gcData(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*17 + 3)
	}
	return data
}

// ------------------------------------------------- the crash-after-complete --

// The nastiest window: CompleteMultipartUpload succeeded, the process
// died before the publish transaction. The multipart no longer exists, so
// ListParts answers NoSuchUpload -- and the recovery must HEAD the object,
// find it whole, and publish. Flipping such a session back to 'active' would
// tell the client to re-upload 50 GB that are already stored.
//
// Constructed, not timed: a real out-of-band CompleteMultipartUpload plus SQL
// state injection. The real window is milliseconds wide, so no SIGKILL lands
// in it on demand -- racing for it would buy a flake, not a proof.
func TestGCRecoversACrashAfterCompleteMultipartUpload(t *testing.T) {
	w := newWorld(t)
	dest := w.folder("uploads")
	data := gcData(int(gcTestPartSize) + 2048)
	sess := w.session(dest, "crashed.bin", data)

	// Garage's half of the finalize really happened.
	parts, err := upload.ListAllParts(w.ctx, w.s3, w.bucket, sess.ObjectKey, *sess.S3UploadID)
	if err != nil {
		t.Fatalf("listing parts: %v", err)
	}
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		n := int32(p.Number)
		completed = append(completed, types.CompletedPart{PartNumber: &n, ETag: aws.String(`"` + p.ETag + `"`)})
	}
	if _, err := w.s3.CompleteMultipartUpload(w.ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(w.bucket),
		Key:             aws.String(sess.ObjectKey),
		UploadId:        sess.S3UploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}); err != nil {
		t.Fatalf("completing the multipart out of band: %v", err)
	}
	if w.multipartExists(sess.ObjectKey, *sess.S3UploadID) {
		t.Fatal("the multipart still exists after CompleteMultipartUpload")
	}

	// Ours died right there.
	if _, err := w.pool.Exec(w.ctx,
		`UPDATE upload_sessions SET status = 'completing', node_id = NULL,
		        updated_at = now() - interval '30 minutes' WHERE id = $1`, sess.ID); err != nil {
		t.Fatalf("injecting the crashed state: %v", err)
	}

	w.run()

	status, nodeID := w.status(sess.ID)
	if status == upload.StatusActive {
		t.Fatal("the session was flipped back to active: the client would re-upload bytes Garage already holds")
	}
	if status != upload.StatusDone || nodeID == nil {
		t.Fatalf("session is %q node_id=%v, want done with a published node", status, nodeID)
	}

	var (
		name   string
		parent uuid.UUID
		blobID uuid.UUID
	)
	if err := w.pool.QueryRow(w.ctx,
		`SELECT name, parent_id, blob_id FROM nodes WHERE id = $1`, *nodeID,
	).Scan(&name, &parent, &blobID); err != nil {
		t.Fatalf("reading the published node: %v", err)
	}
	if name != "crashed.bin" || parent != dest {
		t.Errorf("published as %q under %s, want crashed.bin under %s", name, parent, dest)
	}
	got, err := w.objectBytes(sess.ObjectKey)
	if err != nil {
		t.Fatalf("downloading the published object: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("published object is %d bytes, uploaded %d", len(got), len(data))
	}

	// GC has no way to know the client's whole-file digest, so it publishes
	// without one rather than inventing it.
	var sum []byte
	if err := w.pool.QueryRow(w.ctx, `SELECT sha256 FROM blobs WHERE id = $1`, blobID).Scan(&sum); err != nil {
		t.Fatalf("reading the blob: %v", err)
	}
	if sum != nil {
		t.Errorf("blob sha256 is %x, want NULL for a GC-recovered publish", sum)
	}
}

// The other half of the NoSuchUpload rule: the multipart was aborted and no
// object was ever written, so there is nothing to publish. The session is
// retired, not resurrected.
func TestGCAbandonsACompletingSessionWhoseBytesAreGone(t *testing.T) {
	w := newWorld(t)
	sess := w.session(w.folder("uploads"), "lost.bin", gcData(4096))

	if _, err := w.s3.AbortMultipartUpload(w.ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(w.bucket), Key: aws.String(sess.ObjectKey), UploadId: sess.S3UploadID,
	}); err != nil {
		t.Fatalf("aborting the multipart: %v", err)
	}
	if _, err := w.pool.Exec(w.ctx,
		`UPDATE upload_sessions SET status = 'completing', node_id = NULL,
		        updated_at = now() - interval '30 minutes' WHERE id = $1`, sess.ID); err != nil {
		t.Fatalf("injecting the crashed state: %v", err)
	}

	w.run()

	status, nodeID := w.status(sess.ID)
	if status != upload.StatusAborted || nodeID != nil {
		t.Errorf("session is %q node_id=%v, want aborted with no node", status, nodeID)
	}
	if w.objectExists(sess.ObjectKey) {
		t.Error("the object key still exists after the session was abandoned")
	}
}

// A finalize that is merely slow -- claimed moments ago -- is left alone.
func TestGCLeavesAFreshFinalizeAlone(t *testing.T) {
	w := newWorld(t)
	sess := w.session(w.folder("uploads"), "busy.bin", gcData(2048))
	if _, err := w.pool.Exec(w.ctx,
		`UPDATE upload_sessions SET status = 'completing', updated_at = now() WHERE id = $1`, sess.ID); err != nil {
		t.Fatalf("injecting the completing state: %v", err)
	}

	w.run()

	if status, _ := w.status(sess.ID); status != upload.StatusCompleting {
		t.Errorf("session is %q, want it left in completing", status)
	}
}

// ---------------------------------------------------------- session expiry --

func TestGCAbortsSessionsPastTheirSlidingExpiry(t *testing.T) {
	w := newWorld(t)
	dest := w.folder("uploads")
	expired := w.session(dest, "abandoned.bin", gcData(2048))
	live := w.session(dest, "still-going.bin", gcData(2048))

	if n := testutil.Backdate(t, w.pool, "upload_sessions", "expires_at",
		upload.TTL+time.Hour, "id = $1", expired.ID); n != 1 {
		t.Fatalf("backdated %d rows, want 1", n)
	}

	w.run()

	if status, _ := w.status(expired.ID); status != upload.StatusAborted {
		t.Errorf("the expired session is %q, want aborted", status)
	}
	if w.multipartExists(expired.ObjectKey, *expired.S3UploadID) {
		t.Error("the expired session's multipart upload survived")
	}
	if status, _ := w.status(live.ID); status != upload.StatusActive {
		t.Errorf("the live session is %q, want it untouched", status)
	}
	if !w.multipartExists(live.ObjectKey, *live.S3UploadID) {
		t.Error("the live session's multipart upload was aborted")
	}
}

// ------------------------------------------------------- orphan multiparts --

// The create-then-insert race: a multipart exists for a few milliseconds before
// its session row does. The 24 h grace is the entire guard, so a fresh
// unclaimed multipart must survive a default pass.
func TestGCDoesNotAbortAYoungOrphanMultipart(t *testing.T) {
	w := newWorld(t)
	key := upload.NewObjectKey()
	uploadID, err := (&upload.Presigner{S3: w.s3, Bucket: w.bucket}).CreateMultipart(w.ctx, key)
	if err != nil {
		t.Fatalf("creating the multipart: %v", err)
	}
	t.Cleanup(func() {
		_, _ = w.s3.AbortMultipartUpload(context.Background(), &s3.AbortMultipartUploadInput{
			Bucket: aws.String(w.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		})
	})

	if w.gc.Cfg.OrphanAge != 24*time.Hour {
		t.Fatalf("default OrphanAge is %s, want 24h", w.gc.Cfg.OrphanAge)
	}
	w.run()

	if !w.multipartExists(key, uploadID) {
		t.Error("a multipart created seconds ago was collected; the create-then-insert race is unguarded")
	}
}

// Past the grace and claimed by nobody, the same multipart goes. Garage's
// Initiated timestamp cannot be backdated, so the threshold moves instead.
func TestGCAbortsAnAgedOrphanMultipart(t *testing.T) {
	w := newWorld(t)
	key := upload.NewObjectKey()
	uploadID, err := (&upload.Presigner{S3: w.s3, Bucket: w.bucket}).CreateMultipart(w.ctx, key)
	if err != nil {
		t.Fatalf("creating the multipart: %v", err)
	}
	claimed := w.session(w.folder("uploads"), "claimed.bin", gcData(2048))

	w.gc.Cfg.OrphanAge = 0
	w.run()

	if w.multipartExists(key, uploadID) {
		t.Error("the unclaimed multipart survived a pass past the orphan grace")
	}
	if !w.multipartExists(claimed.ObjectKey, *claimed.S3UploadID) {
		t.Error("a multipart an active session claims was aborted")
	}
}

// R2 re-mints the UploadId on every response: the id ListMultipartUploads
// reports for an upload is never the id CreateMultipartUpload returned for it,
// and both address the same upload (measured against R2 2026-08-17). A claim
// that compares the listed id to the stored one can therefore never match
// there, so every in-progress multipart past the orphan grace looks unclaimed
// and is aborted -- the collector destroying exactly the long-running resumable
// uploads this product exists for.
//
// Garage does not re-mint, so the divergence is injected rather than waited for:
// the session keeps its object key and its real multipart, and only the stored
// id is rewritten to one the listing will never report. Claiming on the object
// key alone is what survives it; with the two-column predicate this test aborts
// a live upload.
func TestGCKeepsAMultipartWhoseStoredUploadIDWasReminted(t *testing.T) {
	w := newWorld(t)
	sess := w.session(w.folder("uploads"), "long-running.bin", gcData(2048))
	live := *sess.S3UploadID

	if _, err := w.pool.Exec(w.ctx,
		`UPDATE upload_sessions SET s3_upload_id = $2 WHERE id = $1`,
		sess.ID, "reminted-"+uuid.NewString()); err != nil {
		t.Fatalf("re-minting the stored upload id: %v", err)
	}

	w.gc.Cfg.OrphanAge = 0
	w.run()

	if !w.multipartExists(sess.ObjectKey, live) {
		t.Error("an active session's multipart was aborted because its stored upload id no longer matched the listed one")
	}
}

// ------------------------------------------------------- the refcount battery --

// Copy shares a blob; purging one side must not touch the bytes the other side
// still needs. Only when the last reference is gone -- and the grace has passed
// -- does the object go, row first.
func TestGCDeletesOnlyBlobsNothingReferences(t *testing.T) {
	w := newWorld(t)
	dest := w.folder("uploads")
	content := gcData(5000)
	original, blobID, key := w.file(dest, "shared.bin", content)

	copied, err := w.nodes.Copy(w.ctx, w.user, original, dest, nil, node.PolicyRename)
	if err != nil {
		t.Fatalf("copying the file: %v", err)
	}
	if got := w.refcount(blobID); got != 2 {
		t.Fatalf("refcount is %d after the copy, want 2", got)
	}

	// Purge the original; the copy still points at the same object.
	if err := w.nodes.Trash(w.ctx, w.user, original); err != nil {
		t.Fatalf("trashing the original: %v", err)
	}
	if err := w.nodes.Purge(w.ctx, w.user, original); err != nil {
		t.Fatalf("purging the original: %v", err)
	}
	if got := w.refcount(blobID); got != 1 {
		t.Fatalf("refcount is %d after purging one side, want 1", got)
	}

	// Even past the grace, a referenced blob survives.
	testutil.Backdate(t, w.pool, "blobs", "created_at", 3*time.Hour, "id = $1", blobID)
	w.run()

	if got := w.refcount(blobID); got != 1 {
		t.Fatalf("the shared blob was collected while the copy still referenced it")
	}
	got, err := w.objectBytes(key)
	if err != nil {
		t.Fatalf("downloading through the surviving copy: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("the copy's bytes changed after the original was purged")
	}

	// Purge the copy too: nothing references the blob now.
	if err := w.nodes.Trash(w.ctx, w.user, copied.ID); err != nil {
		t.Fatalf("trashing the copy: %v", err)
	}
	if err := w.nodes.Purge(w.ctx, w.user, copied.ID); err != nil {
		t.Fatalf("purging the copy: %v", err)
	}
	if got := w.refcount(blobID); got != 0 {
		t.Fatalf("refcount is %d after purging both sides, want 0", got)
	}

	// The grace is real, and it runs from when the last reference went -- not
	// from created_at, which is still backdated three hours from the step above.
	// Under the old created_at semantics this blob was collectible the instant
	// the refcount hit zero, so an already-issued one-hour download URL could
	// 404 mid-transfer; this pass is what proves the deadline moved.
	if stamp := w.unreferencedAt(blobID); stamp == nil {
		t.Fatal("the purge that emptied the blob did not stamp unreferenced_at")
	}
	w.run()
	if got := w.refcount(blobID); got != 0 {
		t.Error("an unreferenced blob was collected inside the grace period")
	}

	testutil.Backdate(t, w.pool, "blobs", "unreferenced_at", 3*time.Hour, "id = $1", blobID)
	w.run()

	var alive int
	if err := w.pool.QueryRow(w.ctx, `SELECT count(*) FROM blobs WHERE id = $1`, blobID).Scan(&alive); err != nil {
		t.Fatalf("counting the blob row: %v", err)
	}
	if alive != 0 {
		t.Error("the blob row survived the sweep")
	}
	if w.objectExists(key) {
		t.Error("the object is still in Garage after its last reference went")
	}
}

// unreferencedAt reads the moment a blob's last reference went, or nil while it
// still has one (or once the row is gone).
func (w *world) unreferencedAt(blobID uuid.UUID) *time.Time {
	w.t.Helper()
	var at *time.Time
	if err := w.pool.QueryRow(w.ctx,
		`SELECT unreferenced_at FROM blobs WHERE id = $1`, blobID).Scan(&at); err != nil {
		return nil
	}
	return at
}

// refcount reads a blob's refcount, or -1 once the row is gone.
func (w *world) refcount(blobID uuid.UUID) int {
	w.t.Helper()
	var n int
	err := w.pool.QueryRow(w.ctx, `SELECT refcount FROM blobs WHERE id = $1`, blobID).Scan(&n)
	if err != nil {
		return -1
	}
	return n
}

// ------------------------------------------------------------------- trash --

func TestGCPurgesTrashPastTheRetentionWindow(t *testing.T) {
	w := newWorld(t)
	old := w.folder("old")
	oldFile, oldBlob, _ := w.file(old, "inside.bin", gcData(1024))
	recent := w.folder("recent")
	recentFile, _, _ := w.file(recent, "keep.bin", gcData(1024))

	for _, id := range []uuid.UUID{old, recent} {
		if err := w.nodes.Trash(w.ctx, w.user, id); err != nil {
			t.Fatalf("trashing %s: %v", id, err)
		}
	}
	testutil.Backdate(t, w.pool, "nodes", "deleted_at", 31*24*time.Hour, "id = $1", old)

	w.run()

	for _, id := range []uuid.UUID{old, oldFile} {
		var n int
		if err := w.pool.QueryRow(w.ctx, `SELECT count(*) FROM nodes WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("counting node %s: %v", id, err)
		}
		if n != 0 {
			t.Errorf("node %s survived the retention window", id)
		}
	}
	if got := w.refcount(oldBlob); got != 0 {
		t.Errorf("the purged file's blob is at refcount %d, want 0", got)
	}
	var kept int
	if err := w.pool.QueryRow(w.ctx, `SELECT count(*) FROM nodes WHERE id = $1`, recentFile).Scan(&kept); err != nil {
		t.Fatalf("counting the recent node: %v", err)
	}
	if kept != 1 {
		t.Error("recently trashed content was purged")
	}
}

// ------------------------------------------------------------- expiry rows --

func TestGCPrunesExpiredAndRetiredRows(t *testing.T) {
	w := newWorld(t)

	liveSession, deadSession := uuid.New(), uuid.New()
	for id, expires := range map[uuid.UUID]string{
		liveSession: "now() + interval '1 day'",
		deadSession: "now() - interval '1 minute'",
	} {
		if _, err := w.pool.Exec(w.ctx, fmt.Sprintf(
			`INSERT INTO auth_sessions (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, %s)`, expires),
			id, w.user, []byte(id.String())); err != nil {
			t.Fatalf("inserting an auth session: %v", err)
		}
	}

	deadToken := uuid.New()
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO email_tokens (id, user_id, purpose, token_hash, expires_at)
		 VALUES ($1, $2, 'verify', $3, now() - interval '1 minute')`,
		deadToken, w.user, []byte(deadToken.String())); err != nil {
		t.Fatalf("inserting an email token: %v", err)
	}

	throttleKey := "gc-" + uuid.NewString()
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO throttle (scope, key, window_start, count)
		 VALUES ('login', $1, now() - interval '2 days', 3)`, throttleKey); err != nil {
		t.Fatalf("inserting a throttle window: %v", err)
	}

	staleToken, freshToken := uuid.New(), uuid.New()
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO api_tokens (id, user_id, name, token_hash, scopes, expires_at, revoked_at)
		 VALUES ($1, $2, 'stale', $3, ARRAY['files:read'], now() + interval '1 day', now() - interval '40 days')`,
		staleToken, w.user, []byte(staleToken.String())); err != nil {
		t.Fatalf("inserting a revoked token: %v", err)
	}
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO api_tokens (id, user_id, name, token_hash, scopes, expires_at)
		 VALUES ($1, $2, 'fresh', $3, ARRAY['files:read'], now() + interval '1 day')`,
		freshToken, w.user, []byte(freshToken.String())); err != nil {
		t.Fatalf("inserting a live token: %v", err)
	}

	// The share sweeps: guest sessions go at expiry, and EVERY access-log
	// action -- view, download and denied alike -- goes at AccessLogAge.
	// A denied-only prune would retain anonymous visitors' IP addresses
	// forever.
	shareFile, _, _ := w.file(w.root, "gc-shared.bin", gcData(64))
	sh, _, err := share.NewStore(w.pool).Create(w.ctx, w.user, shareFile, share.Settings{})
	if err != nil {
		t.Fatalf("creating the share: %v", err)
	}
	liveGuest, deadGuest := uuid.New(), uuid.New()
	for id, expires := range map[uuid.UUID]string{
		liveGuest: "now() + interval '1 day'",
		deadGuest: "now() - interval '1 minute'",
	} {
		if _, err := w.pool.Exec(w.ctx, fmt.Sprintf(
			`INSERT INTO share_guest_sessions (id, share_id, token_hash, expires_at) VALUES ($1, $2, $3, %s)`, expires),
			id, sh.ID, []byte(id.String())); err != nil {
			t.Fatalf("inserting a guest session: %v", err)
		}
	}
	logRow := func(action, age string) int64 {
		var rowID int64
		if err := w.pool.QueryRow(w.ctx, fmt.Sprintf(
			`INSERT INTO share_access_log (share_id, action, at) VALUES ($1, $2, now() - interval '%s') RETURNING id`, age),
			sh.ID, action).Scan(&rowID); err != nil {
			t.Fatalf("inserting a %s row: %v", action, err)
		}
		return rowID
	}
	oldView := logRow("view", "91 days")
	oldDownload := logRow("download", "91 days")
	oldDenied := logRow("denied", "91 days")
	freshView := logRow("view", "89 days")
	freshDownload := logRow("download", "1 day")

	w.run()

	cases := []struct {
		what  string
		sql   string
		arg   any
		alive int
	}{
		{"the live auth session", `SELECT count(*) FROM auth_sessions WHERE id = $1`, liveSession, 1},
		{"the expired auth session", `SELECT count(*) FROM auth_sessions WHERE id = $1`, deadSession, 0},
		{"the expired email token", `SELECT count(*) FROM email_tokens WHERE id = $1`, deadToken, 0},
		{"the stale throttle window", `SELECT count(*) FROM throttle WHERE key = $1`, throttleKey, 0},
		{"the long-revoked API token", `SELECT count(*) FROM api_tokens WHERE id = $1`, staleToken, 0},
		{"the live API token", `SELECT count(*) FROM api_tokens WHERE id = $1`, freshToken, 1},
		{"the live guest session", `SELECT count(*) FROM share_guest_sessions WHERE id = $1`, liveGuest, 1},
		{"the expired guest session", `SELECT count(*) FROM share_guest_sessions WHERE id = $1`, deadGuest, 0},
		{"the 91-day-old view row", `SELECT count(*) FROM share_access_log WHERE id = $1`, oldView, 0},
		{"the 91-day-old download row", `SELECT count(*) FROM share_access_log WHERE id = $1`, oldDownload, 0},
		{"the 91-day-old denied row", `SELECT count(*) FROM share_access_log WHERE id = $1`, oldDenied, 0},
		{"the 89-day-old view row", `SELECT count(*) FROM share_access_log WHERE id = $1`, freshView, 1},
		{"the 1-day-old download row", `SELECT count(*) FROM share_access_log WHERE id = $1`, freshDownload, 1},
	}
	for _, c := range cases {
		var n int
		if err := w.pool.QueryRow(w.ctx, c.sql, c.arg).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", c.what, err)
		}
		if n != c.alive {
			t.Errorf("%s: %d rows, want %d", c.what, n, c.alive)
		}
	}
}

// ------------------------------------------------------------------ wiring --

// The collector is wired into three files this package does not own. Both
// one-liners are type-checked here so a signature change cannot silently break
// them: the harness hook the integration suite assigns, and the background loop
// cmd/drive starts.
func TestTheWiringOneLinersTypeCheck(t *testing.T) {
	pool, s3c, bucket := gcSetup(t)

	// server/integration/main_test.go, inside TestMain after testutil.Start():
	testutil.RunGCOnce = New(pool, s3c, bucket, slog.Default()).RunOnce

	// server/cmd/drive/main.go, after the S3 client is built:
	var loop func(context.Context) = New(pool, s3c, bucket, slog.Default()).Run
	if loop == nil || testutil.RunGCOnce == nil {
		t.Fatal("the wiring entry points are nil")
	}

	// And the hook really runs a pass, which is what the suites depend on.
	testutil.GC(t, context.Background())
}

// -------------------------------------------------------------------- unit --

func TestDefaultsAreTheProductionNumbers(t *testing.T) {
	d := Defaults()
	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"Interval", d.Interval, time.Hour},
		{"SessionStale", d.SessionStale, 15 * time.Minute},
		{"OrphanAge", d.OrphanAge, 24 * time.Hour},
		{"BlobGrace", d.BlobGrace, 2 * time.Hour},
		{"TrashAge", d.TrashAge, 30 * 24 * time.Hour},
		{"TokenAge", d.TokenAge, 30 * 24 * time.Hour},
		{"AccessLogAge", d.AccessLogAge, 90 * 24 * time.Hour},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
		}
	}
}
