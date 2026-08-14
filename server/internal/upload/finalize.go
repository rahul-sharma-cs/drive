package upload

// Finalize: turning a finished transfer into a file the user can see.
//
// Three rules govern this file.
//
// Everything that moves a session's status -- complete, the takeover of a dead
// finalizer, the rollback after a failed verify, GC's expiry abort -- runs
// under pg_advisory_xact_lock(LockKey(id)), taken BEFORE the row is touched,
// and every transition is a conditional UPDATE ... WHERE status = <expected>
// whose zero-row result means stop. Two finalizers can therefore never both
// publish.
//
// The lock is deliberately NOT held across the S3 calls. Claiming the session
// ('active' -> 'completing') is its own short transaction, so a second complete
// arriving mid-transfer sees 'completing' and gets 409 in_progress instead of
// blocking on a lock for the length of a 50 GB CompleteMultipartUpload. The
// 'completing' status is the mutex; the advisory lock only serializes the
// transitions at either end.
//
// Once CompleteMultipartUpload succeeds the multipart ceases to exist: ListParts
// and a retried complete both answer NoSuchUpload. So NoSuchUpload is never
// treated as failure until the object itself has been HEADed -- an object of the
// declared size means Garage's complete already happened and the only thing
// left is to publish. Such a session is never flipped back to 'active'.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/node"
)

// StaleFinalize is how long a 'completing' session with no node may sit before
// another caller takes its finalize over: a retried complete from the client,
// or the GC sweep. It bounds how long a client polls after the process that was
// finalizing its upload died.
const StaleFinalize = 15 * time.Minute

// renameAttempts bounds the auto-rename retry when a sibling takes the name
// between reading the free one and inserting. Restore hit exactly this race in
// Phase 1; here it must not surface at all, because a complete is usually
// unattended.
const renameAttempts = 4

var (
	// ErrInProgress is a finalizer already running for this session. The client
	// polls status rather than re-sending.
	ErrInProgress = errors.New("a finalizer is already running for this upload")
	// ErrVerify is the ledger disagreeing with what Garage actually holds. The
	// offending ledger rows are deleted first, so the next handshake re-requests
	// exactly those parts.
	ErrVerify = errors.New("the uploaded parts do not match what the store holds")
)

// Result is where a finished upload landed. ParentID is the folder it was
// actually published into, which is not the requested one when Reparented is
// true -- the destination was trashed or purged while the transfer ran.
type Result struct {
	NodeID     uuid.UUID
	Name       string
	ParentID   uuid.UUID
	Reparented bool
}

// Finalizer owns the complete/recover path. One per process is plenty.
type Finalizer struct {
	db     *pgxpool.Pool
	s3     *s3.Client
	bucket string
	log    *slog.Logger
	store  *Store
}

// NewFinalizer builds a finalizer over the pool and the object store.
func NewFinalizer(db *pgxpool.Pool, s3c *s3.Client, bucket string, log *slog.Logger) *Finalizer {
	if log == nil {
		log = slog.Default()
	}
	return &Finalizer{db: db, s3: s3c, bucket: bucket, log: log, store: NewStore(db)}
}

// Complete finalizes a session on the owner's behalf: the POST /uploads/{id}/
// complete path. sha256 is the client's whole-file digest, stored for the V2
// scrub job; nil is accepted only from Recover, which has no way to know it.
func (f *Finalizer) Complete(ctx context.Context, userID, id uuid.UUID, sha256 []byte) (Result, error) {
	sess, published, err := f.claim(ctx, &userID, id, false)
	if err != nil {
		return Result{}, err
	}
	if published != nil {
		return *published, nil
	}
	return f.finish(ctx, sess, sha256)
}

// Recover resumes a finalize that was abandoned mid-flight: a session stuck in
// 'completing' with no node, older than StaleFinalize. It is GC's entry point
// and takes no user, because nobody is asking.
//
// A session that does not need recovering is ErrInProgress, which the caller
// treats as "leave it alone".
func (f *Finalizer) Recover(ctx context.Context, id uuid.UUID) (Result, error) {
	sess, published, err := f.claim(ctx, nil, id, true)
	if err != nil {
		return Result{}, err
	}
	if published != nil {
		return *published, nil
	}
	return f.finish(ctx, sess, nil)
}

// ------------------------------------------------------------------- claim --

// claim is transaction one: take the advisory lock, decide what this session
// needs, and move it to 'completing' if it is ours to finalize.
//
// It returns either a session to finish, or the already-published result, or an
// error. takeover restricts it to the recovery case: only a stale 'completing'
// session qualifies, never an active one.
func (f *Finalizer) claim(ctx context.Context, userID *uuid.UUID, id uuid.UUID, takeover bool) (*Session, *Result, error) {
	tx, err := f.db.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("claiming upload session: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is a no-op

	// Advisory lock first, row second. The reverse order deadlocks against every
	// other transition, which all take them this way round.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, LockKey(id)); err != nil {
		return nil, nil, fmt.Errorf("locking upload session: %w", err)
	}

	const sel = `SELECT ` + sessionCols + ` FROM upload_sessions
		 WHERE id = $1 AND ($2::uuid IS NULL OR user_id = $2::uuid)
		 FOR UPDATE`
	sess, err := scanSession(tx.QueryRow(ctx, sel, id, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("claiming upload session: %w", err)
	}

	switch sess.Status {
	case StatusDone:
		res, err := publishedResult(ctx, tx, sess)
		if err != nil {
			return nil, nil, err
		}
		return nil, &res, nil

	case StatusAborted:
		return nil, nil, ErrExpired

	case StatusActive:
		if takeover {
			// GC never finalizes an active session; the expiry sweep owns those.
			return nil, nil, ErrInProgress
		}
		if sess.Expired() {
			return nil, nil, ErrExpired
		}
		// The CAS, and the sliding expiry with it: entering complete is an
		// authenticated touch, so a long CompleteMultipartUpload can never be
		// collected out from under the client.
		const cas = `UPDATE upload_sessions
			   SET status = 'completing', updated_at = now(),
			       expires_at = now() + make_interval(secs => $2)
			 WHERE id = $1 AND status = 'active'`
		tag, err := tx.Exec(ctx, cas, id, TTL.Seconds())
		if err != nil {
			return nil, nil, fmt.Errorf("claiming upload session: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil, nil, ErrInProgress
		}
		sess.Status = StatusCompleting

	case StatusCompleting:
		if sess.NodeID != nil {
			// Published but not yet marked done -- impossible in one transaction,
			// so treat it as a finalizer mid-flight rather than guessing.
			return nil, nil, ErrInProgress
		}
		if time.Since(sess.UpdatedAt) < StaleFinalize {
			return nil, nil, ErrInProgress
		}
		// The finalizer that claimed this is gone. Take it over, stamping
		// updated_at so a third caller sees a fresh one rather than piling on.
		const takeoverSQL = `UPDATE upload_sessions SET updated_at = now()
			 WHERE id = $1 AND status = 'completing' AND node_id IS NULL`
		tag, err := tx.Exec(ctx, takeoverSQL, id)
		if err != nil {
			return nil, nil, fmt.Errorf("claiming upload session: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil, nil, ErrInProgress
		}
		f.log.Info("upload finalize takeover", "session_id", id, "stale_for", time.Since(sess.UpdatedAt).String())

	default:
		return nil, nil, fmt.Errorf("upload session %s has unknown status %q", id, sess.Status)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("claiming upload session: %w", err)
	}
	f.log.Info("upload finalize claim", "session_id", id, "status", sess.Status, "takeover", takeover)
	return sess, nil, nil
}

// publishedResult reads back where a 'done' session put its file. A node_id
// that no longer resolves -- the file was purged after publishing, and the FK
// set the column to NULL -- is ErrNotFound, not a nil dereference.
func publishedResult(ctx context.Context, tx pgx.Tx, sess *Session) (Result, error) {
	if sess.NodeID == nil {
		return Result{}, ErrNotFound
	}
	var (
		name   string
		parent *uuid.UUID
	)
	err := tx.QueryRow(ctx, `SELECT name, parent_id FROM nodes WHERE id = $1`, *sess.NodeID).Scan(&name, &parent)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, ErrNotFound
		}
		return Result{}, fmt.Errorf("reading the published node: %w", err)
	}
	if parent == nil {
		return Result{}, ErrNotFound
	}
	return Result{
		NodeID:     *sess.NodeID,
		Name:       name,
		ParentID:   *parent,
		Reparented: sess.ParentID == nil || *sess.ParentID != *parent,
	}, nil
}

// ------------------------------------------------------------------ finish --

// finish is everything after the claim: get the bytes into one object, then
// publish it atomically.
func (f *Finalizer) finish(ctx context.Context, sess *Session, sha256 []byte) (Result, error) {
	// The 0-byte case never opened a multipart upload -- Garage rejects a
	// complete with an empty part list -- so it is a single PutObject.
	if sess.FileSize == 0 || sess.S3UploadID == nil {
		if sess.FileSize != 0 {
			return Result{}, fmt.Errorf("upload session %s has no multipart upload but declares %d bytes", sess.ID, sess.FileSize)
		}
		if _, err := f.s3.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(f.bucket),
			Key:    aws.String(sess.ObjectKey),
			Body:   bytes.NewReader(nil),
		}); err != nil {
			return Result{}, fmt.Errorf("storing the empty object for %s: %w", sess.ID, err)
		}
		etag, err := f.headSize(ctx, sess, 0)
		if err != nil {
			return Result{}, err
		}
		return f.publish(ctx, sess, etag, sha256)
	}

	remote, err := ListAllParts(ctx, f.s3, f.bucket, sess.ObjectKey, *sess.S3UploadID)
	if err != nil {
		if !IsNoSuchUpload(err) {
			return Result{}, err
		}
		// The multipart is gone. Either our own complete already succeeded and
		// this is a retry, or it was aborted. The object decides which.
		return f.afterMultipartVanished(ctx, sess, sha256)
	}

	ledger, err := f.store.Parts(ctx, sess.ID)
	if err != nil {
		return Result{}, err
	}
	if offending, ok := verifyLedger(sess, ledger, remote); !ok {
		f.log.Warn("upload finalize verify failed",
			"session_id", sess.ID, "ledger", len(ledger), "listed", len(remote),
			"parts_total", sess.PartsTotal, "offending", offending)
		if err := f.rollback(ctx, sess.ID, offending); err != nil {
			return Result{}, err
		}
		return Result{}, ErrVerify
	}

	completed := make([]types.CompletedPart, 0, len(ledger))
	for _, p := range ledger {
		completed = append(completed, types.CompletedPart{
			PartNumber: aws.Int32(int32(p.Number)),
			// Quoted, which is the form S3 and Garage hand out and expect back.
			ETag: aws.String(`"` + p.ETag + `"`),
		})
	}
	sort.Slice(completed, func(i, j int) bool {
		return aws.ToInt32(completed[i].PartNumber) < aws.ToInt32(completed[j].PartNumber)
	})

	if _, err := f.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(f.bucket),
		Key:             aws.String(sess.ObjectKey),
		UploadId:        sess.S3UploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}); err != nil {
		if !IsNoSuchUpload(err) {
			return Result{}, fmt.Errorf("completing multipart upload for %s: %w", sess.ID, err)
		}
		return f.afterMultipartVanished(ctx, sess, sha256)
	}
	f.log.Info("upload multipart completed",
		"session_id", sess.ID, "object_key", sess.ObjectKey, "parts", len(completed))

	etag, err := f.headSize(ctx, sess, sess.FileSize)
	if err != nil {
		return Result{}, err
	}
	return f.publish(ctx, sess, etag, sha256)
}

// afterMultipartVanished handles NoSuchUpload from either ListParts or a
// retried CompleteMultipartUpload. An object at the declared size means the
// complete already landed -- publish, and never flip back to 'active'. No
// object means the multipart was aborted and the bytes are gone for good.
func (f *Finalizer) afterMultipartVanished(ctx context.Context, sess *Session, sha256 []byte) (Result, error) {
	etag, err := f.headSize(ctx, sess, sess.FileSize)
	if err == nil {
		f.log.Info("upload finalize resumed after NoSuchUpload",
			"session_id", sess.ID, "object_key", sess.ObjectKey, "size", sess.FileSize)
		return f.publish(ctx, sess, etag, sha256)
	}
	// No object, or an object that is not the file we were promised: either way
	// the multipart is gone and nothing can rebuild it. Anything else -- a
	// timeout, a refused connection -- is transient and must not retire a
	// session that may still be publishable.
	if !isNotFound(err) && !errors.Is(err, ErrVerify) {
		return Result{}, err
	}
	f.log.Warn("upload abandoned: the multipart is gone and no whole object took its place",
		"session_id", sess.ID, "object_key", sess.ObjectKey, "reason", err)
	if err := f.abandon(ctx, sess); err != nil {
		return Result{}, err
	}
	return Result{}, ErrExpired
}

// headSize confirms the stored object is the size the session declared and
// returns its normalized ETag.
func (f *Finalizer) headSize(ctx context.Context, sess *Session, want int64) (string, error) {
	out, err := f.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(f.bucket),
		Key:    aws.String(sess.ObjectKey),
	})
	if err != nil {
		if isNotFound(err) {
			return "", err
		}
		return "", fmt.Errorf("heading %s: %w", sess.ObjectKey, err)
	}
	if got := aws.ToInt64(out.ContentLength); got != want {
		return "", fmt.Errorf("%w: stored object is %d bytes, the session declared %d", ErrVerify, got, want)
	}
	return NormalizeETag(aws.ToString(out.ETag)), nil
}

// verifyLedger checks the ledger against what Garage holds: every part present,
// each one the size and normalized ETag the store reports, and the sizes adding
// up to the declared file size.
//
// The offending part numbers are the ledger rows to delete, so the next
// handshake re-requests exactly those parts and nothing else. A short total with
// every part otherwise agreeing can only be the final part, which is the one
// part confirm accepts at less than part_size.
func verifyLedger(sess *Session, ledger, remote []Part) (offending []int, ok bool) {
	stored := make(map[int]Part, len(remote))
	for _, p := range remote {
		stored[p.Number] = p
	}

	var (
		total int64
		bad   []int
		have  = make(map[int]bool, len(ledger))
	)
	for _, p := range ledger {
		have[p.Number] = true
		r, present := stored[p.Number]
		if !present || r.Size != p.Size || r.ETag != NormalizeETag(p.ETag) {
			bad = append(bad, p.Number)
			continue
		}
		total += p.Size
	}
	if len(bad) > 0 {
		return bad, false
	}
	for n := 1; n <= sess.PartsTotal; n++ {
		if !have[n] {
			// Nothing to delete for a part the ledger never had.
			return nil, false
		}
	}
	if total != sess.FileSize {
		// Every ledger row satisfies CheckPart -- confirm refuses anything else,
		// and reconciliation refuses to adopt anything else -- so every non-final
		// part is exactly part_size and only the final part can be short. Blame
		// it on the measurement rather than by construction: naming PartsTotal
		// unconditionally deletes an innocent row, the handshake re-issues a part
		// that was already correct, and complete then fails identically forever.
		for _, p := range ledger {
			if p.Number == sess.PartsTotal && p.Size < sess.Remaining(p.Number) {
				return []int{sess.PartsTotal}, false
			}
		}
		// No row accounts for the shortfall. Deleting nothing is strictly safer
		// than guessing; the sizes came from Garage, so a re-listed session will
		// show the same thing rather than silently losing a good part.
		return nil, false
	}
	return nil, true
}

// rollback deletes the ledger rows that disagreed with the store and returns
// the session to 'active' so the client can re-upload exactly those parts.
func (f *Finalizer) rollback(ctx context.Context, id uuid.UUID, offending []int) error {
	tx, err := f.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("rolling back a failed finalize: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, LockKey(id)); err != nil {
		return fmt.Errorf("locking upload session: %w", err)
	}
	if len(offending) > 0 {
		const del = `DELETE FROM upload_parts WHERE session_id = $1 AND part_number = ANY($2::int[])`
		if _, err := tx.Exec(ctx, del, id, toInt32(offending)); err != nil {
			return fmt.Errorf("deleting mismatched parts: %w", err)
		}
	}
	const back = `UPDATE upload_sessions
		   SET status = 'active', updated_at = now(),
		       expires_at = now() + make_interval(secs => $2)
		 WHERE id = $1 AND status = 'completing' AND node_id IS NULL`
	if _, err := tx.Exec(ctx, back, id, TTL.Seconds()); err != nil {
		return fmt.Errorf("returning the session to active: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rolling back a failed finalize: %w", err)
	}
	f.log.Info("upload finalize rolled back", "session_id", id, "deleted_parts", offending)
	return nil
}

// abandon retires a session whose bytes are gone: neither the multipart nor the
// object exists any more.
func (f *Finalizer) abandon(ctx context.Context, sess *Session) error {
	tx, err := f.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("abandoning upload session: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, LockKey(sess.ID)); err != nil {
		return fmt.Errorf("locking upload session: %w", err)
	}
	const upd = `UPDATE upload_sessions SET status = 'aborted', updated_at = now()
		 WHERE id = $1 AND status = 'completing' AND node_id IS NULL`
	if _, err := tx.Exec(ctx, upd, sess.ID); err != nil {
		return fmt.Errorf("abandoning upload session: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("abandoning upload session: %w", err)
	}
	return f.DeleteOrphanObject(ctx, sess.ObjectKey)
}

// ----------------------------------------------------------------- publish --

// publish is transaction two, and it is atomic by construction: the blob row,
// the destination re-check, the name resolution, the node row and the session's
// move to 'done' either all land or none do. A file is never half-visible.
//
// The conditional UPDATE at the end is what makes a duplicate finalizer safe:
// the second one to reach here finds status already 'done' and returns the
// first one's result instead of publishing a second node.
func (f *Finalizer) publish(ctx context.Context, sess *Session, etag string, sha256 []byte) (Result, error) {
	tx, err := f.db.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("publishing upload: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, LockKey(sess.ID)); err != nil {
		return Result{}, fmt.Errorf("locking upload session: %w", err)
	}

	const sel = `SELECT ` + sessionCols + ` FROM upload_sessions WHERE id = $1 FOR UPDATE`
	cur, err := scanSession(tx.QueryRow(ctx, sel, sess.ID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Result{}, ErrNotFound
		}
		return Result{}, fmt.Errorf("publishing upload: %w", err)
	}
	switch cur.Status {
	case StatusDone:
		// Someone else published while we were talking to Garage.
		return publishedResult(ctx, tx, cur)
	case StatusCompleting:
	default:
		return Result{}, ErrExpired
	}

	blobID := uuid.New()
	const insertBlob = `INSERT INTO blobs (id, object_key, size, sha256, etag, refcount)
		VALUES ($1, $2, $3, $4, $5, 1)`
	if _, err := tx.Exec(ctx, insertBlob, blobID, sess.ObjectKey, sess.FileSize, sha256, etag); err != nil {
		return Result{}, fmt.Errorf("publishing upload: inserting the blob: %w", err)
	}

	// The destination is re-authorized here, not trusted from create: a 50 GB
	// upload easily outlives the folder it was aimed at. A destination that is
	// gone, trashed, no longer a folder or no longer ours sends the file to the
	// user's root, exactly as restore does.
	parentID, reparented, err := destination(ctx, tx, sess)
	if err != nil {
		return Result{}, err
	}

	name, err := node.Clean(sess.FileName)
	if err != nil {
		return Result{}, fmt.Errorf("publishing upload: %w", err)
	}
	policy := ""
	if sess.ConflictPolicy != nil {
		policy = *sess.ConflictPolicy
	}

	nodeID := uuid.New()
	final, err := insertPublishedNode(ctx, tx, sess, nodeID, blobID, parentID, name, policy)
	if err != nil {
		return Result{}, err
	}

	const finish = `UPDATE upload_sessions
		   SET status = 'done', node_id = $2, updated_at = now()
		 WHERE id = $1 AND status = 'completing' AND node_id IS NULL`
	tag, err := tx.Exec(ctx, finish, sess.ID, nodeID)
	if err != nil {
		return Result{}, fmt.Errorf("publishing upload: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Another finalizer got there first; discard everything this one built.
		return Result{}, ErrInProgress
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("publishing upload: %w", err)
	}

	f.log.Info("upload published",
		"session_id", sess.ID, "node_id", nodeID, "blob_id", blobID,
		"parent_id", parentID, "name", final, "size", sess.FileSize,
		"reparented", reparented, "renamed", final != name)

	return Result{NodeID: nodeID, Name: final, ParentID: parentID, Reparented: reparented}, nil
}

// destination re-checks the session's parent folder and falls back to the
// user's root.
//
// The check is deliberately the same one node.Store applies to every
// body-supplied parent -- live, a folder, owned by the caller -- written out
// here because that helper is unexported and this has to run inside the publish
// transaction.
func destination(ctx context.Context, tx pgx.Tx, sess *Session) (uuid.UUID, bool, error) {
	if sess.ParentID != nil {
		const q = `SELECT id FROM nodes
			 WHERE id = $1 AND owner_id = $2 AND kind = 'folder' AND deleted_at IS NULL`
		var id uuid.UUID
		err := tx.QueryRow(ctx, q, *sess.ParentID, sess.UserID).Scan(&id)
		if err == nil {
			return id, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, fmt.Errorf("re-checking the destination folder: %w", err)
		}
	}
	var root uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT id FROM nodes WHERE owner_id = $1 AND parent_id IS NULL`, sess.UserID,
	).Scan(&root); err != nil {
		return uuid.Nil, false, fmt.Errorf("locating the root folder: %w", err)
	}
	return root, true, nil
}

// insertPublishedNode resolves the name conflict and writes the node row,
// returning the name it actually took.
//
// replace trashes the colliding node in this same transaction; anything else
// auto-renames, because an unattended complete must never fail on a collision.
// The retry loop covers the sliver where a sibling takes the chosen name between
// the read and the insert.
func insertPublishedNode(ctx context.Context, tx pgx.Tx, sess *Session, nodeID, blobID, parentID uuid.UUID, name, policy string) (string, error) {
	const insert = `INSERT INTO nodes (id, owner_id, parent_id, kind, name, blob_id, size, mime)
		VALUES ($1, $2, $3, 'file', $4, $5, $6, $7)`

	replace := policy == node.PolicyReplace
	for attempt := 0; attempt < renameAttempts; attempt++ {
		final, err := resolvePublishName(ctx, tx, parentID, name, replace)
		if err != nil {
			return "", err
		}

		// A savepoint, so a losing race on the sibling-uniqueness index does not
		// poison the whole publish transaction.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return "", fmt.Errorf("publishing upload: %w", err)
		}
		_, err = sp.Exec(ctx, insert, nodeID, sess.UserID, parentID, final, blobID, sess.FileSize, sess.Mime)
		if err == nil {
			if err := sp.Commit(ctx); err != nil {
				return "", fmt.Errorf("publishing upload: %w", err)
			}
			return final, nil
		}
		_ = sp.Rollback(ctx)

		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "nodes_parent_name_idx" {
			return "", fmt.Errorf("publishing upload: inserting the node: %w", err)
		}
		// Someone took the name in between; the next pass renames around them.
		replace = false
	}
	return "", fmt.Errorf("publishing upload: could not find a free name for %q", name)
}

// resolvePublishName returns the name the new node should take in parentID.
//
// With replace, a colliding node (and its subtree, if it is a folder) is
// trashed here and the name is taken as-is. Otherwise the deterministic
// "name (1)" walk applies -- node.NextFreeName, the same function restore uses,
// so an auto-rename looks identical wherever it happens.
func resolvePublishName(ctx context.Context, tx pgx.Tx, parentID uuid.UUID, name string, replace bool) (string, error) {
	const collide = `SELECT id FROM nodes
		 WHERE parent_id = $1 AND deleted_at IS NULL AND lower(name) = lower($2)`
	var collision uuid.UUID
	err := tx.QueryRow(ctx, collide, parentID, name).Scan(&collision)
	if errors.Is(err, pgx.ErrNoRows) {
		return name, nil
	}
	if err != nil {
		return "", fmt.Errorf("checking for a name conflict: %w", err)
	}

	if replace {
		// The same recursive stamp node.Store uses, so replacing a folder-shaped
		// collision behaves exactly like trashing it would.
		const trash = `
			WITH RECURSIVE sub AS (
			    SELECT id FROM nodes WHERE id = $1 AND deleted_at IS NULL
			    UNION ALL
			    SELECT n.id FROM nodes n JOIN sub ON n.parent_id = sub.id
			     WHERE n.deleted_at IS NULL
			)
			UPDATE nodes
			   SET deleted_at = now(), updated_at = now(), trashed_root = (id = $1)
			 WHERE id IN (SELECT id FROM sub)`
		if _, err := tx.Exec(ctx, trash, collision); err != nil {
			return "", fmt.Errorf("replacing the colliding node: %w", err)
		}
		return name, nil
	}

	taken, err := takenSiblingNames(ctx, tx, parentID, name)
	if err != nil {
		return "", err
	}
	return node.NextFreeName(name, taken), nil
}

// takenSiblingNames returns every live sibling name that could block name or one
// of its numbered variants.
//
// The pattern is a prefix match on the stem, which is a superset of what
// NextFreeName cares about -- it ignores names that do not block -- and keeps
// this to one query with no extension bookkeeping.
func takenSiblingNames(ctx context.Context, tx pgx.Tx, parentID uuid.UUID, name string) ([]string, error) {
	stem := name
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem = name[:i]
	}
	const q = `SELECT name FROM nodes
		 WHERE parent_id = $1 AND deleted_at IS NULL AND lower(name) LIKE $2 ESCAPE '\'`
	rows, err := tx.Query(ctx, q, parentID, escapeLike(strings.ToLower(stem))+`%`)
	if err != nil {
		return nil, fmt.Errorf("listing sibling names: %w", err)
	}
	defer rows.Close()

	var taken []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("listing sibling names: %w", err)
		}
		taken = append(taken, n)
	}
	return taken, rows.Err()
}

// escapeLike neutralizes LIKE's wildcards in a filename, which may legitimately
// contain % and _.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ------------------------------------------------------------ abort / sweep --

// AbortSession retires a session under its advisory lock and discards whatever
// Garage still holds for it. It reports whether it actually changed anything.
//
// expiredOnly is GC's expiry sweep: only an 'active' session past its sliding
// expiry qualifies, so a session someone is still uploading to is never touched.
// Without it, any live-or-finalizing session is retired -- which is how a
// finalize that lost its bytes cleans up after itself.
func (f *Finalizer) AbortSession(ctx context.Context, id uuid.UUID, expiredOnly bool) (bool, error) {
	tx, err := f.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("aborting upload session: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, LockKey(id)); err != nil {
		return false, fmt.Errorf("locking upload session: %w", err)
	}

	const sel = `SELECT ` + sessionCols + ` FROM upload_sessions WHERE id = $1 FOR UPDATE`
	sess, err := scanSession(tx.QueryRow(ctx, sel, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("aborting upload session: %w", err)
	}
	if sess.Status != StatusActive && sess.Status != StatusCompleting {
		return false, nil
	}
	if expiredOnly && (sess.Status != StatusActive || sess.ExpiresAt.After(time.Now())) {
		return false, nil
	}

	const upd = `UPDATE upload_sessions SET status = 'aborted', updated_at = now()
		 WHERE id = $1 AND status = $2`
	tag, err := tx.Exec(ctx, upd, id, sess.Status)
	if err != nil {
		return false, fmt.Errorf("aborting upload session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("aborting upload session: %w", err)
	}

	if sess.S3UploadID != nil {
		if _, err := f.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(f.bucket),
			Key:      aws.String(sess.ObjectKey),
			UploadId: sess.S3UploadID,
		}); err != nil && !IsNoSuchUpload(err) {
			f.log.Warn("aborting the session's multipart upload",
				"error", err, "session_id", id, "object_key", sess.ObjectKey)
		}
	}
	// A session that ever reached 'completing' may have finished its complete
	// before dying, leaving a whole object behind that nothing references.
	if sess.Status == StatusCompleting {
		if err := f.DeleteOrphanObject(ctx, sess.ObjectKey); err != nil {
			f.log.Warn("deleting an abandoned object", "error", err, "object_key", sess.ObjectKey)
		}
	}
	return true, nil
}

// DeleteOrphanObject removes an object that no blob row references. The check
// is the whole point: an object a published blob points at must survive its
// session being aborted.
func (f *Finalizer) DeleteOrphanObject(ctx context.Context, key string) error {
	var referenced bool
	if err := f.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM blobs WHERE object_key = $1)`, key,
	).Scan(&referenced); err != nil {
		return fmt.Errorf("checking blob references for %s: %w", key, err)
	}
	if referenced {
		return nil
	}
	if _, err := f.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(f.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("deleting %s: %w", key, err)
	}
	return nil
}

// ------------------------------------------------------------ error shapes --

// IsNoSuchUpload reports the one S3 error the upload path reasons about: the
// multipart is gone, because it was aborted or because our own complete already
// succeeded.
//
// The wire code is checked alongside the typed error because the typed error is
// not reliably produced. aws-sdk-go-v2's ListParts deserializer carries no
// NoSuchUpload case at all -- its error switch is `default` only, so a vanished
// multipart arrives as a bare *smithy.GenericAPIError -- while
// AbortMultipartUpload's deserializer does carry it. Anything matching on the
// type alone is dead code on half the call sites, which is exactly how the
// resume handshake came to answer 500 where the contract says 410.
//
// Exported for that reason: this is the one definition both halves share.
func IsNoSuchUpload(err error) bool {
	var typed *types.NoSuchUpload
	if errors.As(err, &typed) {
		return true
	}
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "NoSuchUpload"
}

// isNotFound reports a missing object, which HEAD answers with a bare 404 and
// no body -- hence the typed NotFound rather than NoSuchKey.
func isNotFound(err error) bool {
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	return false
}
