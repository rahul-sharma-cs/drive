package upload

// The finalize path against the real drive-test stack: real Postgres, real
// Garage, real multipart uploads completed for real.
//
// Nothing here is mocked and nothing sleeps. Both crash windows -- a finalizer
// that died after CompleteMultipartUpload, a second complete arriving while the
// first runs -- are constructed with SQL state injection plus out-of-band S3
// calls, because SIGKILL cannot be aimed at a millisecond.

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/db"
	"github.com/rahul-sharma-cs/drive/server/internal/node"
)

// finTestPartSize is well under the 10 MiB Garage block size the server config
// enforces. CompleteMultipartUpload is the one call with a minimum part size,
// and these tests exercise it -- so if Garage ever starts enforcing S3's 5 MiB
// floor, the multi-part cases below are where it will show up.
const finTestPartSize int64 = 1 << 20

// ------------------------------------------------------------- test harness --

func finEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var (
	finOnce    sync.Once
	finPool    *pgxpool.Pool
	finS3      *s3.Client
	finBucket  string
	finInitErr error
)

func finSetup(t *testing.T) (*pgxpool.Pool, *s3.Client, string) {
	t.Helper()
	finOnce.Do(func() {
		dsn := finEnv("DRIVE_DB_DSN", "postgres://drive:drive@localhost:55433/drive?sslmode=disable")
		if strings.Contains(dsn, ":55432") {
			finInitErr = fmt.Errorf("DRIVE_DB_DSN points at the dev stack (%s); tests run against drive-test on :55433", dsn)
			return
		}
		cfg := &config.Config{
			S3Endpoint:  finEnv("DRIVE_S3_ENDPOINT", "http://localhost:3910"),
			S3Bucket:    finEnv("DRIVE_S3_BUCKET", "drive-blobs"),
			S3AccessKey: finEnv("DRIVE_S3_ACCESS_KEY", "drivetestkey0001"),
			S3SecretKey: finEnv("DRIVE_S3_SECRET_KEY", "drivetestsecretkey0001"),
		}
		if strings.Contains(cfg.S3Endpoint, ":3900") {
			finInitErr = fmt.Errorf("DRIVE_S3_ENDPOINT points at the dev stack (%s); tests run against drive-test on :3910", cfg.S3Endpoint)
			return
		}
		ctx := context.Background()
		if finPool, finInitErr = db.Connect(ctx, dsn); finInitErr != nil {
			return
		}
		if finInitErr = db.Migrate(ctx, finPool); finInitErr != nil {
			return
		}
		finS3, _, finInitErr = blob.New(ctx, cfg)
		finBucket = cfg.S3Bucket
	})
	if finInitErr != nil {
		t.Fatalf("drive-test stack: %v (is `docker compose -p drive-test --env-file .env.test up -d` running?)", finInitErr)
	}
	return finPool, finS3, finBucket
}

// fixture is one test's private user, root folder and finalizer.
type fixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	s3     *s3.Client
	bucket string
	fin    *Finalizer
	store  *Store
	nodes  *node.Store
	user   uuid.UUID
	root   uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool, s3c, bucket := finSetup(t)
	ctx := context.Background()

	f := &fixture{
		t: t, ctx: ctx, pool: pool, s3: s3c, bucket: bucket,
		fin:   NewFinalizer(pool, s3c, bucket, slog.New(slog.DiscardHandler)),
		store: NewStore(pool),
		nodes: node.NewStore(pool),
		user:  uuid.New(),
		root:  uuid.New(),
	}
	const insertUser = `INSERT INTO users (id, email, password_hash, display_name, email_verified_at)
		VALUES ($1, $2, 'x', 'Finalize Test', now())`
	if _, err := pool.Exec(ctx, insertUser, f.user, "fin-"+f.user.String()+"@drive.test"); err != nil {
		t.Fatalf("creating the test user: %v", err)
	}
	const insertRoot = `INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		VALUES ($1, $2, NULL, 'folder', 'My Drive')`
	if _, err := pool.Exec(ctx, insertRoot, f.root, f.user); err != nil {
		t.Fatalf("creating the root folder: %v", err)
	}
	return f
}

// folder creates a live folder under the root.
func (f *fixture) folder(name string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name) VALUES ($1, $2, $3, 'folder', $4)`,
		id, f.user, f.root, name); err != nil {
		f.t.Fatalf("creating folder %q: %v", name, err)
	}
	return id
}

// file publishes a file node the way testutil does, so a name collision at
// complete time has something real to collide with.
func (f *fixture) file(parentID uuid.UUID, name string, content []byte) uuid.UUID {
	f.t.Helper()
	fixture, err := blob.PutFixture(f.ctx, f.s3, f.bucket, content)
	if err != nil {
		f.t.Fatalf("putting fixture %q: %v", name, err)
	}
	blobID, nodeID := uuid.New(), uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO blobs (id, object_key, size, sha256, etag, refcount) VALUES ($1,$2,$3,$4,$5,1)`,
		blobID, fixture.ObjectKey, fixture.Size, fixture.SHA256, fixture.ETag); err != nil {
		f.t.Fatalf("inserting blob for %q: %v", name, err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name, blob_id, size, mime)
		 VALUES ($1,$2,$3,'file',$4,$5,$6,'application/octet-stream')`,
		nodeID, f.user, parentID, name, blobID, fixture.Size); err != nil {
		f.t.Fatalf("inserting node %q: %v", name, err)
	}
	return nodeID
}

// upload builds a session whose parts are really in Garage and really in the
// ledger: the exact state a client hands to complete.
func (f *fixture) upload(parentID uuid.UUID, name string, data []byte, policy string) (*Session, []byte) {
	f.t.Helper()

	partSize, partsTotal, err := ResolvePartSize(int64(len(data)), finTestPartSize)
	if err != nil {
		f.t.Fatalf("resolving the part size: %v", err)
	}

	sess := &Session{
		ID:          uuid.New(),
		UserID:      f.user,
		ParentID:    &parentID,
		FileName:    name,
		FileSize:    int64(len(data)),
		Fingerprint: "fp-" + uuid.NewString(),
		ObjectKey:   NewObjectKey(),
		PartSize:    partSize,
		PartsTotal:  partsTotal,
		Mode:        ModeDirect,
	}
	if policy != "" {
		sess.ConflictPolicy = &policy
	}

	if len(data) > 0 {
		uploadID, err := (&Presigner{S3: f.s3, Bucket: f.bucket}).CreateMultipart(f.ctx, sess.ObjectKey)
		if err != nil {
			f.t.Fatalf("creating the multipart upload: %v", err)
		}
		sess.S3UploadID = &uploadID
	}
	if err := f.store.Insert(f.ctx, sess); err != nil {
		f.t.Fatalf("inserting the session: %v", err)
	}

	for n := 1; n <= partsTotal; n++ {
		lo := int64(n-1) * partSize
		hi := min(lo+partSize, int64(len(data)))
		f.putPart(sess, n, data[lo:hi])
	}

	sum := sha256.Sum256(data)
	return sess, sum[:]
}

// putPart uploads one part for real and confirms it into the ledger, exactly as
// the browser and the confirm endpoint would.
func (f *fixture) putPart(sess *Session, n int, chunk []byte) {
	f.t.Helper()
	num := int32(n)
	out, err := f.s3.UploadPart(f.ctx, &s3.UploadPartInput{
		Bucket:     aws.String(f.bucket),
		Key:        aws.String(sess.ObjectKey),
		UploadId:   sess.S3UploadID,
		PartNumber: &num,
		Body:       bytes.NewReader(chunk),
	})
	if err != nil {
		f.t.Fatalf("uploading part %d: %v", n, err)
	}
	sum := md5.Sum(chunk)
	digest := hex.EncodeToString(sum[:])
	etag := NormalizeETag(aws.ToString(out.ETag))
	if etag != digest {
		f.t.Fatalf("part %d: Garage returned ETag %q, the client computed MD5 %q", n, etag, digest)
	}
	if err := f.store.ConfirmPart(f.ctx, sess.ID, Part{
		Number: n, Size: int64(len(chunk)), ETag: etag, MD5: &digest,
	}); err != nil {
		f.t.Fatalf("confirming part %d: %v", n, err)
	}
}

// reload re-reads a session row.
func (f *fixture) reload(id uuid.UUID) *Session {
	f.t.Helper()
	sess, err := f.store.Get(f.ctx, f.user, id)
	if err != nil {
		f.t.Fatalf("re-reading session %s: %v", id, err)
	}
	return sess
}

// nodeRow reads back what a publish actually wrote.
func (f *fixture) nodeRow(id uuid.UUID) (name string, parent uuid.UUID, size int64, deleted bool) {
	f.t.Helper()
	var (
		p   *uuid.UUID
		del *time.Time
		sz  *int64
	)
	if err := f.pool.QueryRow(f.ctx,
		`SELECT name, parent_id, size, deleted_at FROM nodes WHERE id = $1`, id,
	).Scan(&name, &p, &sz, &del); err != nil {
		f.t.Fatalf("reading node %s: %v", id, err)
	}
	if p != nil {
		parent = *p
	}
	if sz != nil {
		size = *sz
	}
	return name, parent, size, del != nil
}

// object downloads a published object so a test can prove the bytes are right.
func (f *fixture) object(key string) []byte {
	f.t.Helper()
	out, err := f.s3.GetObject(f.ctx, &s3.GetObjectInput{
		Bucket: aws.String(f.bucket), Key: aws.String(key),
	})
	if err != nil {
		f.t.Fatalf("downloading %s: %v", key, err)
	}
	defer out.Body.Close()
	raw, err := io.ReadAll(out.Body)
	if err != nil {
		f.t.Fatalf("reading %s: %v", key, err)
	}
	return raw
}

func finData(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	return data
}

// ------------------------------------------------------------- happy path ----

func TestCompletePublishesTheFile(t *testing.T) {
	f := newFixture(t)
	dest := f.folder("uploads")
	data := finData(int(finTestPartSize) + 4096) // two parts, the last one short
	sess, sum := f.upload(dest, "report.pdf", data, "")

	res, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Name != "report.pdf" {
		t.Errorf("published as %q, want report.pdf", res.Name)
	}
	if res.ParentID != dest {
		t.Errorf("published into %s, want %s", res.ParentID, dest)
	}
	if res.Reparented {
		t.Error("reported as re-parented, but the destination was live")
	}

	name, parent, size, deleted := f.nodeRow(res.NodeID)
	if name != "report.pdf" || parent != dest || size != int64(len(data)) || deleted {
		t.Errorf("node row: name=%q parent=%s size=%d deleted=%v", name, parent, size, deleted)
	}
	if got := f.reload(sess.ID); got.Status != StatusDone || got.NodeID == nil || *got.NodeID != res.NodeID {
		t.Errorf("session ended as %q node_id=%v", got.Status, got.NodeID)
	}
	if got := f.object(sess.ObjectKey); !bytes.Equal(got, data) {
		t.Errorf("stored object is %d bytes, uploaded %d", len(got), len(data))
	}

	// The blob row carries the client's digest for the V2 scrub job.
	var stored []byte
	if err := f.pool.QueryRow(f.ctx,
		`SELECT b.sha256 FROM blobs b JOIN nodes n ON n.blob_id = b.id WHERE n.id = $1`, res.NodeID,
	).Scan(&stored); err != nil {
		t.Fatalf("reading the blob: %v", err)
	}
	if !bytes.Equal(stored, sum) {
		t.Errorf("blob sha256 is %x, want %x", stored, sum)
	}
}

// A retried complete on a published session returns the same node rather than
// publishing a second one. The engine re-sends complete after a timeout.
func TestCompleteIsIdempotentOnceDone(t *testing.T) {
	f := newFixture(t)
	dest := f.folder("uploads")
	sess, sum := f.upload(dest, "again.bin", finData(2048), "")

	first, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum)
	if err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	second, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum)
	if err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	if first.NodeID != second.NodeID {
		t.Errorf("second complete published %s, the first published %s", second.NodeID, first.NodeID)
	}
	var files int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM nodes WHERE parent_id = $1 AND deleted_at IS NULL`, dest,
	).Scan(&files); err != nil {
		t.Fatalf("counting children: %v", err)
	}
	if files != 1 {
		t.Errorf("%d files in the destination, want 1", files)
	}
}

// The 0-byte special case: an empty file skips CreateMultipartUpload entirely
// (Garage hard-rejects an empty part list at complete), so no multipart upload
// ever existed and complete is a zero-byte PutObject plus the same atomic
// publish.
func TestCompleteZeroByteFile(t *testing.T) {
	f := newFixture(t)
	dest := f.folder("uploads")
	sess, sum := f.upload(dest, "empty.txt", nil, "")
	if sess.S3UploadID != nil {
		t.Fatal("a 0-byte session opened a multipart upload")
	}

	res, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, _, size, _ := f.nodeRow(res.NodeID); size != 0 {
		t.Errorf("published size %d, want 0", size)
	}
	if got := f.object(sess.ObjectKey); len(got) != 0 {
		t.Errorf("stored object is %d bytes, want 0", len(got))
	}
}

// ------------------------------------------------------------ serialization --

// A second complete arriving while the first is mid-flight gets 409 in_progress
// so the client polls instead of racing. Constructed, not raced: a live
// 'completing' row with a fresh updated_at is exactly the state the first
// finalizer leaves behind while it talks to Garage.
func TestConcurrentCompleteIsRefusedAsInProgress(t *testing.T) {
	f := newFixture(t)
	dest := f.folder("uploads")
	sess, sum := f.upload(dest, "busy.bin", finData(4096), "")

	if _, err := f.pool.Exec(f.ctx,
		`UPDATE upload_sessions SET status = 'completing', updated_at = now() WHERE id = $1`, sess.ID); err != nil {
		t.Fatalf("injecting the completing state: %v", err)
	}

	_, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum)
	if !errors.Is(err, ErrInProgress) {
		t.Fatalf("Complete returned %v, want ErrInProgress", err)
	}
	if got := f.reload(sess.ID); got.Status != StatusCompleting {
		t.Errorf("session is %q, want it left in completing", got.Status)
	}
}

// A finalizer that died before publishing must not strand the client on 409
// forever: once the session has been untouched for StaleFinalize, the next
// complete takes it over. Here the bytes are all still in the multipart, so the
// takeover runs the whole flow.
func TestStaleCompletingIsTakenOverByARetriedComplete(t *testing.T) {
	f := newFixture(t)
	dest := f.folder("uploads")
	data := finData(3000)
	sess, sum := f.upload(dest, "abandoned.bin", data, "")

	if _, err := f.pool.Exec(f.ctx,
		`UPDATE upload_sessions SET status = 'completing',
		        updated_at = now() - make_interval(secs => $2) WHERE id = $1`,
		sess.ID, (StaleFinalize + time.Minute).Seconds()); err != nil {
		t.Fatalf("injecting the stale completing state: %v", err)
	}

	res, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := f.object(sess.ObjectKey); !bytes.Equal(got, data) {
		t.Errorf("published object does not match the uploaded bytes")
	}
	if got := f.reload(sess.ID); got.Status != StatusDone {
		t.Errorf("session is %q, want done", got.Status)
	}
	_ = res
}

// ------------------------------------------------------------ verify / drift --

// The ledger claiming an ETag Garage does not have is the corruption case
// complete exists to catch. The offending row is deleted so the next handshake
// re-requests exactly that part, and the session goes back to active.
func TestLedgerMismatchDeletesOffendingRowsAndReturnsToActive(t *testing.T) {
	f := newFixture(t)
	dest := f.folder("uploads")
	data := finData(int(finTestPartSize) + 1024)
	sess, sum := f.upload(dest, "drifted.bin", data, "")

	if _, err := f.pool.Exec(f.ctx,
		`UPDATE upload_parts SET etag = 'ffffffffffffffffffffffffffffffff'
		  WHERE session_id = $1 AND part_number = 2`, sess.ID); err != nil {
		t.Fatalf("corrupting the ledger: %v", err)
	}

	_, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum)
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("Complete returned %v, want ErrVerify", err)
	}
	if got := f.reload(sess.ID); got.Status != StatusActive {
		t.Errorf("session is %q, want it rolled back to active", got.Status)
	}
	parts, err := f.store.Parts(f.ctx, sess.ID)
	if err != nil {
		t.Fatalf("reading the ledger: %v", err)
	}
	if got := PartNumbers(parts); len(got) != 1 || got[0] != 1 {
		t.Errorf("ledger holds parts %v, want only part 1 to survive", got)
	}
	// And the multipart is untouched, so re-PUTting part 2 finishes the upload.
	f.putPart(sess, 2, data[finTestPartSize:])
	if _, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum); err != nil {
		t.Fatalf("Complete after re-sending part 2: %v", err)
	}
}

// A short final part passes confirm (it is allowed to be under part_size) but
// must not pass complete: the totals do not reach the declared file size.
func TestShortFinalPartFailsTheTotalCheck(t *testing.T) {
	f := newFixture(t)
	dest := f.folder("uploads")
	data := finData(4096)
	sess, sum := f.upload(dest, "short.bin", data, "")

	// Re-upload the only part with fewer bytes, and re-confirm it. Both the
	// store and the ledger now agree on a file that is too small.
	f.putPart(sess, 1, data[:1000])

	_, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum)
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("Complete returned %v, want ErrVerify", err)
	}
	if got := f.reload(sess.ID); got.Status != StatusActive {
		t.Errorf("session is %q, want active", got.Status)
	}
	if parts, _ := f.store.Parts(f.ctx, sess.ID); len(parts) != 0 {
		t.Errorf("ledger still holds %d parts, want the short final part deleted", len(parts))
	}
}

// A ledger missing a part fails the count check. Nothing is deleted, because
// there is nothing to delete -- the handshake will re-issue it.
func TestMissingPartFailsVerify(t *testing.T) {
	f := newFixture(t)
	dest := f.folder("uploads")
	data := finData(int(finTestPartSize) + 512)
	sess, sum := f.upload(dest, "gap.bin", data, "")

	if _, err := f.pool.Exec(f.ctx,
		`DELETE FROM upload_parts WHERE session_id = $1 AND part_number = 1`, sess.ID); err != nil {
		t.Fatalf("deleting a ledger row: %v", err)
	}
	if _, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum); !errors.Is(err, ErrVerify) {
		t.Fatalf("Complete returned %v, want ErrVerify", err)
	}
	if got := f.reload(sess.ID); got.Status != StatusActive {
		t.Errorf("session is %q, want active", got.Status)
	}
	if parts, _ := f.store.Parts(f.ctx, sess.ID); len(parts) != 1 {
		t.Errorf("ledger holds %d parts, want part 2 untouched", len(parts))
	}
}

// ------------------------------------------------------------- publish rules --

// Completes fire unattended, so a name taken while the transfer ran auto-renames
// rather than failing.
func TestCompleteAutoRenamesOnANameConflict(t *testing.T) {
	f := newFixture(t)
	dest := f.folder("uploads")
	f.file(dest, "notes.txt", []byte("the incumbent"))
	sess, sum := f.upload(dest, "notes.txt", finData(2048), "")

	res, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Name != "notes (1).txt" {
		t.Errorf("published as %q, want %q", res.Name, "notes (1).txt")
	}
	var live int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM nodes WHERE parent_id = $1 AND deleted_at IS NULL`, dest).Scan(&live); err != nil {
		t.Fatalf("counting children: %v", err)
	}
	if live != 2 {
		t.Errorf("%d live children, want both files present", live)
	}
}

// conflict_policy=replace trashes the incumbent in the same transaction and
// takes its name.
func TestCompleteWithReplaceTrashesTheExistingNode(t *testing.T) {
	f := newFixture(t)
	dest := f.folder("uploads")
	old := f.file(dest, "notes.txt", []byte("the incumbent"))
	sess, sum := f.upload(dest, "notes.txt", finData(2048), node.PolicyReplace)

	res, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Name != "notes.txt" {
		t.Errorf("published as %q, want notes.txt", res.Name)
	}
	if _, _, _, deleted := f.nodeRow(old); !deleted {
		t.Error("the replaced node is still live")
	}
	if _, _, _, deleted := f.nodeRow(res.NodeID); deleted {
		t.Error("the published node was trashed")
	}
}

// The destination is re-authorized at publish time, not trusted from create: a
// 50 GB upload outlives the folder it was aimed at.
func TestCompleteReParentsWhenTheDestinationWasTrashed(t *testing.T) {
	f := newFixture(t)
	dest := f.folder("doomed")
	sess, sum := f.upload(dest, "orphan.bin", finData(2048), "")

	if err := f.nodes.Trash(f.ctx, f.user, dest); err != nil {
		t.Fatalf("trashing the destination: %v", err)
	}

	res, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !res.Reparented {
		t.Error("the result does not report a re-parent")
	}
	if res.ParentID != f.root {
		t.Errorf("published into %s, want the user's root %s", res.ParentID, f.root)
	}
}

// The purge variant: the parent row is gone entirely and the FK set the
// session's parent_id to NULL.
func TestCompleteReParentsWhenTheDestinationWasPurged(t *testing.T) {
	f := newFixture(t)
	dest := f.folder("doomed")
	sess, sum := f.upload(dest, "orphan.bin", finData(2048), "")

	if err := f.nodes.Trash(f.ctx, f.user, dest); err != nil {
		t.Fatalf("trashing the destination: %v", err)
	}
	if err := f.nodes.Purge(f.ctx, f.user, dest); err != nil {
		t.Fatalf("purging the destination: %v", err)
	}
	// Purge marks sessions aimed at the purged folder aborted; the client's
	// complete is still in flight, so put it back where it was.
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE upload_sessions SET status = 'active' WHERE id = $1`, sess.ID); err != nil {
		t.Fatalf("restoring the session state: %v", err)
	}

	res, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !res.Reparented || res.ParentID != f.root {
		t.Errorf("published into %s (reparented=%v), want the user's root %s", res.ParentID, res.Reparented, f.root)
	}
}

// ----------------------------------------------------------------- refusals --

func TestCompleteRefusesAnotherUsersSession(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	sess, sum := f.upload(f.folder("uploads"), "mine.bin", finData(1024), "")

	if _, err := f.fin.Complete(f.ctx, other.user, sess.ID, sum); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Complete for a foreign user returned %v, want ErrNotFound", err)
	}
}

func TestCompleteRefusesAnExpiredSession(t *testing.T) {
	f := newFixture(t)
	sess, sum := f.upload(f.folder("uploads"), "stale.bin", finData(1024), "")

	if _, err := f.pool.Exec(f.ctx,
		`UPDATE upload_sessions SET expires_at = now() - interval '1 hour' WHERE id = $1`, sess.ID); err != nil {
		t.Fatalf("expiring the session: %v", err)
	}
	if _, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum); !errors.Is(err, ErrExpired) {
		t.Fatalf("Complete returned %v, want ErrExpired", err)
	}
}

// Entering complete slides the sliding expiry, so a long CompleteMultipartUpload
// can never be collected at the line.
func TestCompleteSlidesTheSessionExpiry(t *testing.T) {
	f := newFixture(t)
	sess, sum := f.upload(f.folder("uploads"), "slide.bin", finData(1024), "")

	if _, err := f.pool.Exec(f.ctx,
		`UPDATE upload_sessions SET expires_at = now() + interval '1 minute' WHERE id = $1`, sess.ID); err != nil {
		t.Fatalf("shortening the expiry: %v", err)
	}
	if _, err := f.fin.Complete(f.ctx, f.user, sess.ID, sum); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := f.reload(sess.ID); got.ExpiresAt.Before(time.Now().Add(TTL - time.Hour)) {
		t.Errorf("expiry is %s, want it slid a full TTL forward", got.ExpiresAt)
	}
}

// ------------------------------------------------------------- unit coverage --

func TestVerifyLedger(t *testing.T) {
	etag := func(s string) *string { return &s }
	sess := &Session{FileSize: 300, PartSize: 200, PartsTotal: 2}
	ledger := []Part{
		{Number: 1, Size: 200, ETag: "aa", MD5: etag("aa")},
		{Number: 2, Size: 100, ETag: "bb", MD5: etag("bb")},
	}
	remote := []Part{{Number: 1, Size: 200, ETag: "aa"}, {Number: 2, Size: 100, ETag: "bb"}}

	if bad, ok := verifyLedger(sess, ledger, remote); !ok {
		t.Errorf("a matching ledger failed verification (offending %v)", bad)
	}

	wrongETag := []Part{{Number: 1, Size: 200, ETag: "aa"}, {Number: 2, Size: 100, ETag: "cc"}}
	bad, ok := verifyLedger(sess, ledger, wrongETag)
	if ok || len(bad) != 1 || bad[0] != 2 {
		t.Errorf("ETag drift gave offending=%v ok=%v, want just part 2", bad, ok)
	}

	short := &Session{FileSize: 400, PartSize: 200, PartsTotal: 2}
	bad, ok = verifyLedger(short, ledger, remote)
	if ok || len(bad) != 1 || bad[0] != 2 {
		t.Errorf("a short total gave offending=%v ok=%v, want the final part", bad, ok)
	}

	bad, ok = verifyLedger(sess, ledger[:1], remote[:1])
	if ok || bad != nil {
		t.Errorf("a missing part gave offending=%v ok=%v, want no deletions and a refusal", bad, ok)
	}
}

func TestEscapeLike(t *testing.T) {
	if got := escapeLike(`100%_of\it`); got != `100\%\_of\\it` {
		t.Errorf("escapeLike = %q", got)
	}
}

// ------------------------------------------------------------- the ledger --

// One object key belongs to one session, and the database says so.
//
// The GC's orphan-multipart claim asks only "does an active or completing
// session own this key?", never which upload id, because R2 re-mints the
// UploadId on every response and a claim comparing ids could never match there.
// That narrower predicate is sound only while a key is unique: a second session
// on the same key would let a dead one vouch for a live one's multipart. Nothing
// can produce a duplicate today -- one call site, a fresh uuid, and no path
// rewrites a key -- and migration 0004 is what keeps it that way, failing at the
// INSERT instead of quietly at the next sweep.
func TestObjectKeyIsUniquePerSession(t *testing.T) {
	f := newFixture(t)
	parent := f.folder("keys")

	// Each session gets its own fingerprint, or the active-fingerprint index
	// fires first and the answer is a resume rather than a refusal.
	session := func(key string) *Session {
		return &Session{
			ID:          uuid.New(),
			UserID:      f.user,
			ParentID:    &parent,
			FileName:    "same-key.bin",
			FileSize:    1024,
			Fingerprint: "fp-" + uuid.NewString(),
			ObjectKey:   key,
			PartSize:    finTestPartSize,
			PartsTotal:  1,
			Mode:        ModeDirect,
		}
	}

	key := NewObjectKey()
	if err := f.store.Insert(f.ctx, session(key)); err != nil {
		t.Fatalf("inserting the first session: %v", err)
	}

	err := f.store.Insert(f.ctx, session(key))
	if err == nil {
		t.Fatal("a second session took an object key another session already owns; the GC's claim can no longer tell them apart")
	}
	// And it is not the create race, which is a resume and names a different
	// index entirely -- reporting one as the other would turn a bug into a
	// handshake the client retries forever.
	if errors.Is(err, ErrRace) {
		t.Fatalf("a duplicate object key was reported as the create race: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("err = %v, want a unique violation", err)
	}
	if pgErr.ConstraintName != "upload_sessions_object_key_key" {
		t.Errorf("violated constraint = %q, want the object-key one", pgErr.ConstraintName)
	}

	// A key of its own is still accepted: the constraint pins reuse, not
	// creation.
	if err := f.store.Insert(f.ctx, session(NewObjectKey())); err != nil {
		t.Fatalf("a session with its own key was refused: %v", err)
	}
}
