// Package gc is Drive's background collector: the one place that deletes
// things nobody asked to delete.
//
// Two properties shape it.
//
// Every sweep is idempotent and driven entirely by stored timestamps compared
// against now(). Nothing here keeps state between passes, so a crash mid-sweep
// costs one pass, and a test moves a deadline by backdating a row rather than
// by waiting.
//
// Every deletion is ordered so that a crash between its steps leaks rather than
// loses. A blob's row is deleted before its object, so an interrupted pass
// leaves an unreferenced object -- which the next orphan sweep is free to
// remove -- and never a row pointing at bytes that are gone.
package gc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/node"
	"github.com/rahul-sharma-cs/drive/server/internal/upload"
)

// Config holds every deadline the collector applies. They are fields rather
// than constants for one reason that is not test convenience: Garage's
// multipart Initiated timestamp lives in Garage, not in Postgres, so the only
// way to exercise the 24 h orphan rule at all is to inject the threshold.
type Config struct {
	// Interval is how often the background loop runs a pass.
	Interval time.Duration
	// SessionStale is how long a 'completing' session with no node may sit
	// before the collector finishes its finalize.
	SessionStale time.Duration
	// OrphanAge is how old a multipart upload matching no live session must be
	// before it is aborted. It is long on purpose: a create that has opened its
	// multipart but not yet inserted its session row must never be collected.
	OrphanAge time.Duration
	// BlobGrace is how long an unreferenced blob survives. It must exceed the
	// presign TTL so an already-issued download URL cannot outlive its object.
	BlobGrace time.Duration
	// TrashAge is how long a trashed subtree is kept before it is purged.
	TrashAge time.Duration
	// TokenAge is how long a revoked or expired API token row is kept.
	TokenAge time.Duration
	// AccessLogAge is how long a share-access row is kept -- view, download
	// and denied alike. Audit rows outlive the share, not the retention
	// window: 90 days is what the owner's log screen reads, and it is the
	// whole answer to how long a visitor's IP address is retained.
	AccessLogAge time.Duration
	// ThrottleAge is how far past its start a throttle window is dropped. Every
	// budget's window is an hour or less, so a day is generous.
	ThrottleAge time.Duration
	// Batch bounds how many items one sweep handles, so a pass cannot run
	// unboundedly long.
	Batch int
}

// Defaults are the production numbers. The grace periods are not arbitrary:
// BlobGrace must exceed the presign TTL so a URL handed out just before a
// refcount hit zero cannot outlive its object, and OrphanAge is long enough
// that the create-then-insert race can never look like an orphan.
func Defaults() Config {
	return Config{
		Interval:     time.Hour,
		SessionStale: upload.StaleFinalize,
		OrphanAge:    24 * time.Hour,
		BlobGrace:    2 * time.Hour,
		TrashAge:     30 * 24 * time.Hour,
		TokenAge:     30 * 24 * time.Hour,
		AccessLogAge: 90 * 24 * time.Hour,
		ThrottleAge:  24 * time.Hour,
		Batch:        500,
	}
}

// GC is the collector.
type GC struct {
	Cfg Config

	db     *pgxpool.Pool
	s3     *s3.Client
	bucket string
	log    *slog.Logger
	fin    *upload.Finalizer
	nodes  *node.Store
}

// New builds a collector with the default deadlines. Adjust Cfg afterwards if
// you need different ones.
func New(db *pgxpool.Pool, s3c *s3.Client, bucket string, log *slog.Logger) *GC {
	if log == nil {
		log = slog.Default()
	}
	return &GC{
		Cfg:    Defaults(),
		db:     db,
		s3:     s3c,
		bucket: bucket,
		log:    log,
		fin:    upload.NewFinalizer(db, s3c, bucket, log),
		nodes:  node.NewStore(db),
	}
}

// Run is the background loop. It returns when ctx is cancelled.
func (g *GC) Run(ctx context.Context) {
	ticker := time.NewTicker(g.Cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := g.RunOnce(ctx); err != nil {
				g.log.Error("garbage collection pass failed", "error", err)
			}
		}
	}
}

// RunOnce performs one full pass. Sweeps are independent: one failing does not
// stop the rest, and every failure comes back joined.
//
// It is exported because the suites trigger passes synchronously -- the GC's
// schedule is the only deadline in the system that is not a stored timestamp,
// so it is the only one a test cannot move by backdating a row.
func (g *GC) RunOnce(ctx context.Context) error {
	started := time.Now()
	err := errors.Join(
		g.expireSessions(ctx),
		g.finishStaleFinalizes(ctx),
		g.abortOrphanMultiparts(ctx),
		g.deleteUnreferencedBlobs(ctx),
		g.purgeOldTrash(ctx),
		g.deleteExpiredRows(ctx),
	)
	g.log.Info("garbage collection pass", "took", time.Since(started).String(), "failed", err != nil)
	return err
}

// ------------------------------------------------------------ upload sweeps --

// expireSessions aborts uploads whose sliding expiry ran out. Only 'active'
// sessions qualify: a session mid-finalize has its own sweep, and its expiry
// was slid forward when the finalize began.
func (g *GC) expireSessions(ctx context.Context) error {
	const q = `SELECT id FROM upload_sessions
		 WHERE status = 'active' AND expires_at < now()
		 ORDER BY expires_at LIMIT $1`
	ids, err := g.selectIDs(ctx, q, g.Cfg.Batch)
	if err != nil {
		return fmt.Errorf("gc: listing expired upload sessions: %w", err)
	}

	var errs []error
	for _, id := range ids {
		aborted, err := g.fin.AbortSession(ctx, id, true)
		if err != nil {
			errs = append(errs, fmt.Errorf("gc: aborting expired session %s: %w", id, err))
			continue
		}
		if aborted {
			g.log.Info("gc: upload session aborted", "session_id", id, "reason", "sliding expiry lapsed")
		}
	}
	return errors.Join(errs...)
}

// finishStaleFinalizes takes over a finalize whose process died: 'completing',
// no node, untouched for longer than SessionStale.
//
// The recovery itself is NoSuchUpload-aware -- if Garage's complete already
// landed, the object is published and the session never returns to 'active'.
func (g *GC) finishStaleFinalizes(ctx context.Context) error {
	const q = `SELECT id FROM upload_sessions
		 WHERE status = 'completing' AND node_id IS NULL
		   AND updated_at < now() - make_interval(secs => $2)
		 ORDER BY updated_at LIMIT $1`
	ids, err := g.selectIDs(ctx, q, g.Cfg.Batch, g.Cfg.SessionStale.Seconds())
	if err != nil {
		return fmt.Errorf("gc: listing stale finalizes: %w", err)
	}

	var errs []error
	for _, id := range ids {
		res, err := g.fin.Recover(ctx, id)
		switch {
		case err == nil:
			g.log.Info("gc: stale finalize completed",
				"session_id", id, "node_id", res.NodeID, "name", res.Name,
				"reparented", res.Reparented, "reason", "finalizer died mid-publish")
		case errors.Is(err, upload.ErrInProgress):
			// Another finalizer claimed it between the select and the call.
		case errors.Is(err, upload.ErrExpired):
			g.log.Info("gc: stale finalize abandoned",
				"session_id", id, "reason", "multipart and object both gone")
		case errors.Is(err, upload.ErrVerify):
			g.log.Info("gc: stale finalize returned to active",
				"session_id", id, "reason", "ledger disagreed with the store")
		default:
			errs = append(errs, fmt.Errorf("gc: recovering finalize %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// abortOrphanMultiparts discards multipart uploads Garage still holds that no
// live session claims.
//
// The age guard is what makes this safe: create opens the multipart before it
// inserts the session row, so a multipart younger than OrphanAge might belong to
// a session that is milliseconds from existing. Nothing else in the system
// abandons a multipart, so waiting a day costs only storage.
//
// The abort uses the id the listing reported, never a stored one -- a genuine
// orphan has no session row and so no stored id, and R2's re-minted ids are
// interchangeable (proven: aborting by a listed id returns 204 and empties the
// listing).
func (g *GC) abortOrphanMultiparts(ctx context.Context) error {
	cutoff := time.Now().Add(-g.Cfg.OrphanAge)

	var (
		errs      []error
		keyMarker *string
		idMarker  *string
	)
	for {
		out, err := g.s3.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
			Bucket:         aws.String(g.bucket),
			KeyMarker:      keyMarker,
			UploadIdMarker: idMarker,
		})
		if err != nil {
			return errors.Join(append(errs, fmt.Errorf("gc: listing multipart uploads: %w", err))...)
		}
		for _, up := range out.Uploads {
			key, uploadID := aws.ToString(up.Key), aws.ToString(up.UploadId)
			if up.Initiated == nil || up.Initiated.After(cutoff) {
				continue
			}
			claimed, err := g.multipartClaimed(ctx, key)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if claimed {
				continue
			}
			if _, err := g.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(g.bucket),
				Key:      up.Key,
				UploadId: up.UploadId,
			}); err != nil {
				errs = append(errs, fmt.Errorf("gc: aborting orphan multipart %s: %w", uploadID, err))
				continue
			}
			g.log.Info("gc: orphan multipart aborted",
				"object_key", key, "upload_id", uploadID, "initiated", up.Initiated,
				"reason", "older than the orphan grace and matching no live session")
		}
		if !aws.ToBool(out.IsTruncated) {
			return errors.Join(errs...)
		}
		keyMarker, idMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}

// multipartClaimed reports whether a live session owns this multipart upload.
//
// The claim is on object_key alone, deliberately, and the upload id is not part
// of the predicate: R2 re-mints the UploadId on every response, so the id
// ListMultipartUploads reports for an upload is never the id
// CreateMultipartUpload returned for it (measured 2026-08-17 -- both address the
// same upload; aborting with either one empties the listing). Comparing the
// listed id to the stored one can therefore never match on R2, which would make
// every in-progress multipart past the orphan grace look unclaimed and get
// aborted -- the collector destroying exactly the long-running resumable
// uploads this product exists for.
//
// One key per session is what makes the narrower predicate sound: object keys
// come from upload.NewObjectKey (a fresh uuid), it has exactly one call site,
// and no path -- resume, expiry retire, the insert race -- ever reuses one.
func (g *GC) multipartClaimed(ctx context.Context, key string) (bool, error) {
	const q = `SELECT EXISTS (
		SELECT 1 FROM upload_sessions
		 WHERE object_key = $1
		   AND status IN ('active', 'completing'))`
	var claimed bool
	if err := g.db.QueryRow(ctx, q, key).Scan(&claimed); err != nil {
		return false, fmt.Errorf("gc: checking multipart at %s: %w", key, err)
	}
	return claimed, nil
}

// -------------------------------------------------------------------- blobs --

// deleteUnreferencedBlobs removes blobs nothing points at any more -- purge only
// ever decrements the refcount -- and then their objects.
//
// Row first, object second, on purpose: a crash in between leaves an object no
// row knows about, which is recoverable, instead of a row whose bytes are gone,
// which is not.
//
// The grace is measured from unreferenced_at -- when the last reference went --
// and never from created_at. From created_at, a blob older than the grace was
// collectible the instant its refcount hit zero, so a download URL issued a
// minute earlier could 404 mid-transfer and a file uploaded yesterday and
// trashed today got no grace at all. A NULL unreferenced_at at refcount 0 means
// the stamp has not landed yet; the row waits for the next pass rather than
// being collected on a timestamp that means something else.
func (g *GC) deleteUnreferencedBlobs(ctx context.Context) error {
	const q = `DELETE FROM blobs
		 WHERE id IN (SELECT id FROM blobs
		               WHERE refcount = 0
		                 AND unreferenced_at IS NOT NULL
		                 AND unreferenced_at < now() - make_interval(secs => $1)
		               ORDER BY unreferenced_at LIMIT $2)
		RETURNING id, object_key`
	rows, err := g.db.Query(ctx, q, g.Cfg.BlobGrace.Seconds(), g.Cfg.Batch)
	if err != nil {
		return fmt.Errorf("gc: deleting unreferenced blobs: %w", err)
	}
	type doomed struct {
		id  uuid.UUID
		key string
	}
	var list []doomed
	for rows.Next() {
		var d doomed
		if err := rows.Scan(&d.id, &d.key); err != nil {
			rows.Close()
			return fmt.Errorf("gc: deleting unreferenced blobs: %w", err)
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("gc: deleting unreferenced blobs: %w", err)
	}

	var errs []error
	for _, d := range list {
		if _, err := g.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(g.bucket),
			Key:    aws.String(d.key),
		}); err != nil {
			errs = append(errs, fmt.Errorf("gc: deleting object %s: %w", d.key, err))
			continue
		}
		g.log.Info("gc: blob deleted", "blob_id", d.id, "object_key", d.key,
			"reason", "refcount 0 past the grace period")
	}
	return errors.Join(errs...)
}

// -------------------------------------------------------------------- trash --

// purgeOldTrash permanently deletes trashed subtrees past the retention window,
// through the same purge the API uses -- so refcounts are decremented exactly
// once per node and shares are revoked with them.
func (g *GC) purgeOldTrash(ctx context.Context) error {
	const q = `SELECT id, owner_id FROM nodes
		 WHERE trashed_root AND deleted_at IS NOT NULL
		   AND deleted_at < now() - make_interval(secs => $2)
		 ORDER BY deleted_at LIMIT $1`
	rows, err := g.db.Query(ctx, q, g.Cfg.Batch, g.Cfg.TrashAge.Seconds())
	if err != nil {
		return fmt.Errorf("gc: listing old trash: %w", err)
	}
	type victim struct{ id, owner uuid.UUID }
	var list []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.owner); err != nil {
			rows.Close()
			return fmt.Errorf("gc: listing old trash: %w", err)
		}
		list = append(list, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("gc: listing old trash: %w", err)
	}

	var errs []error
	for _, v := range list {
		if err := g.nodes.Purge(ctx, v.owner, v.id); err != nil {
			if errors.Is(err, node.ErrNotFound) {
				continue
			}
			errs = append(errs, fmt.Errorf("gc: purging %s: %w", v.id, err))
			continue
		}
		g.log.Info("gc: trashed subtree purged", "node_id", v.id, "owner_id", v.owner,
			"reason", "in the trash past the retention window")
	}
	return errors.Join(errs...)
}

// --------------------------------------------------------------- expiry rows --

// deleteExpiredRows drops the short-lived rows nothing reads once their deadline
// has passed, plus the two audit-retention sweeps.
func (g *GC) deleteExpiredRows(ctx context.Context) error {
	sweeps := []struct {
		what string
		sql  string
		args []any
	}{
		{"share_otps", `DELETE FROM share_otps WHERE expires_at < now()`, nil},
		{"share_guest_sessions", `DELETE FROM share_guest_sessions WHERE expires_at < now()`, nil},
		{"email_tokens", `DELETE FROM email_tokens WHERE expires_at < now()`, nil},
		{"auth_sessions", `DELETE FROM auth_sessions WHERE expires_at < now()`, nil},
		{"throttle", `DELETE FROM throttle WHERE window_start < now() - make_interval(secs => $1)`,
			[]any{g.Cfg.ThrottleAge.Seconds()}},
		{"api_tokens", `DELETE FROM api_tokens
			 WHERE revoked_at < now() - make_interval(secs => $1)
			    OR expires_at < now() - make_interval(secs => $1)`,
			[]any{g.Cfg.TokenAge.Seconds()}},
		{"share_access_log", `DELETE FROM share_access_log
			 WHERE at < now() - make_interval(secs => $1)`,
			[]any{g.Cfg.AccessLogAge.Seconds()}},
	}

	var errs []error
	for _, s := range sweeps {
		tag, err := g.db.Exec(ctx, s.sql, s.args...)
		if err != nil {
			errs = append(errs, fmt.Errorf("gc: pruning %s: %w", s.what, err))
			continue
		}
		if n := tag.RowsAffected(); n > 0 {
			g.log.Info("gc: rows pruned", "table", s.what, "rows", n, "reason", "past their deadline")
		}
	}
	return errors.Join(errs...)
}

// ------------------------------------------------------------------ helpers --

func (g *GC) selectIDs(ctx context.Context, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := g.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
