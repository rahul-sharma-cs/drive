package upload

// The ledger: upload_sessions and upload_parts in Postgres.
//
// Everything durable about an upload lives here, which is what makes a resume
// after a kill -9 possible at all -- the server keeps no in-memory state about
// a transfer, so restarting it loses nothing.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// activeIndex is the partial unique index that makes "one active session per
// (user, file, destination)" a database guarantee rather than a convention. A
// 23505 naming it is the two-tabs-at-once race, not a bug.
const activeIndex = "upload_sessions_active_fingerprint_idx"

const sessionCols = `id, user_id, parent_id, file_name, file_size, mime, fingerprint,
	conflict_policy, s3_upload_id, object_key, part_size, parts_total, status, mode,
	verify_parts, node_id, created_at, updated_at, expires_at`

// Store is the ledger's data access.
type Store struct{ db *pgxpool.Pool }

// NewStore wraps a pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// LockKey is the advisory-lock key for a session: the first eight bytes of its
// UUID, read big-endian.
//
// Every status transition anywhere in the codebase -- cancel here, complete and
// finalize-resume in the finalize path, the flip-back and expiry-abort in GC --
// must serialize on pg_advisory_xact_lock(LockKey(id)) with this exact
// function, or two of them will run concurrently on the same session.
//
// Lock order is always advisory lock first, row second.
func LockKey(id uuid.UUID) int64 {
	return int64(binary.BigEndian.Uint64(id[:8]))
}

// ---------------------------------------------------------------- sessions --

// Get returns one session belonging to userID. Another user's session is
// ErrNotFound, exactly like one that never existed: an upload_id is an
// identifier, not a credential.
func (s *Store) Get(ctx context.Context, userID, id uuid.UUID) (*Session, error) {
	const q = `SELECT ` + sessionCols + ` FROM upload_sessions WHERE id = $1 AND user_id = $2`
	sess, err := scanSession(s.db.QueryRow(ctx, q, id, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("selecting upload session: %w", err)
	}
	return sess, nil
}

// MatchActive finds the active session for (user, fingerprint, parent), which
// is what turns a second create for the same file into a resume. It returns
// nil, nil when there is none.
//
// Expiry is deliberately not filtered here: the partial unique index does not
// know about expiry either, so an expired-but-still-active row would block the
// insert. The caller retires it explicitly.
func (s *Store) MatchActive(ctx context.Context, userID, parentID uuid.UUID, fingerprint string) (*Session, error) {
	const q = `SELECT ` + sessionCols + ` FROM upload_sessions
		 WHERE user_id = $1 AND fingerprint = $2 AND parent_id = $3 AND status = 'active'`
	sess, err := scanSession(s.db.QueryRow(ctx, q, userID, fingerprint, parentID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("matching upload session: %w", err)
	}
	return sess, nil
}

// Insert writes a new session. A unique violation on the active index is
// ErrRace: another create won, and the caller must abort the multipart upload
// it just opened and return the winner instead of a duplicate.
func (s *Store) Insert(ctx context.Context, sess *Session) error {
	const q = `
		INSERT INTO upload_sessions
			(id, user_id, parent_id, file_name, file_size, mime, fingerprint, conflict_policy,
			 s3_upload_id, object_key, part_size, parts_total, status, mode, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'active', $13,
		        now() + make_interval(secs => $14))
		RETURNING created_at, updated_at, expires_at`
	err := s.db.QueryRow(ctx, q,
		sess.ID, sess.UserID, sess.ParentID, sess.FileName, sess.FileSize, sess.Mime,
		sess.Fingerprint, sess.ConflictPolicy, sess.S3UploadID, sess.ObjectKey,
		sess.PartSize, sess.PartsTotal, sess.Mode, TTL.Seconds(),
	).Scan(&sess.CreatedAt, &sess.UpdatedAt, &sess.ExpiresAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == activeIndex {
			return ErrRace
		}
		return fmt.Errorf("inserting upload session: %w", err)
	}
	sess.Status = StatusActive
	return nil
}

// Touch slides the sliding expiry. PLAN is specific that it moves on EVERY
// authenticated touch -- handshake, part confirm, status, complete entry -- not
// only on part confirmations, so a session whose final hash is still running
// cannot be collected out from under the client.
//
// A session that is no longer active keeps the expiry it had; the returned
// value is always the row's current one.
func (s *Store) Touch(ctx context.Context, id uuid.UUID) (time.Time, error) {
	const q = `
		UPDATE upload_sessions
		   SET expires_at = CASE WHEN status = 'active'
		                         THEN now() + make_interval(secs => $2) ELSE expires_at END,
		       updated_at = CASE WHEN status = 'active' THEN now() ELSE updated_at END
		 WHERE id = $1
		RETURNING expires_at`
	var expires time.Time
	if err := s.db.QueryRow(ctx, q, id, TTL.Seconds()).Scan(&expires); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, ErrNotFound
		}
		return time.Time{}, fmt.Errorf("touching upload session: %w", err)
	}
	return expires, nil
}

// SetConflictPolicy records the policy a matched create supplied, so the
// finalize path resolves the collision the way the user just answered.
func (s *Store) SetConflictPolicy(ctx context.Context, id uuid.UUID, policy string) error {
	const q = `UPDATE upload_sessions SET conflict_policy = $2, updated_at = now() WHERE id = $1`
	if _, err := s.db.Exec(ctx, q, id, policy); err != nil {
		return fmt.Errorf("setting conflict policy: %w", err)
	}
	return nil
}

// Arm arms chimera verification, and returns the pins in force.
//
// COALESCE is the whole point: an already-armed session keeps its pins. The
// pinned pair must not move between the bounce and the client's re-call, or
// the client would be answering a question the server has stopped asking.
// Passing nil pins is how a caller reads the current value without arming.
func (s *Store) Arm(ctx context.Context, id uuid.UUID, pins []int) ([]int, error) {
	const q = `
		UPDATE upload_sessions SET verify_parts = COALESCE(verify_parts, $2)
		 WHERE id = $1
		RETURNING verify_parts`
	var armed []int32
	if err := s.db.QueryRow(ctx, q, id, toInt32(pins)).Scan(&armed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("arming part verification: %w", err)
	}
	return toInt(armed), nil
}

// ClearVerify disarms verification after the client's MD5s matched.
func (s *Store) ClearVerify(ctx context.Context, id uuid.UUID) error {
	const q = `UPDATE upload_sessions SET verify_parts = NULL, updated_at = now() WHERE id = $1`
	if _, err := s.db.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("clearing part verification: %w", err)
	}
	return nil
}

// Abort is the cancel transition: under the session's advisory lock, an active
// session becomes 'aborted'. The session as it was BEFORE the transition is
// returned, so the caller can tell an actual cancel (abort the multipart) from
// a no-op (already done) from a refusal (a finalizer is running).
func (s *Store) Abort(ctx context.Context, userID, id uuid.UUID) (*Session, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("aborting upload session: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, LockKey(id)); err != nil {
		return nil, fmt.Errorf("locking upload session: %w", err)
	}

	const sel = `SELECT ` + sessionCols + ` FROM upload_sessions WHERE id = $1 AND user_id = $2`
	before, err := scanSession(tx.QueryRow(ctx, sel, id, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("aborting upload session: %w", err)
	}

	if before.Status == StatusActive {
		const upd = `UPDATE upload_sessions SET status = 'aborted', updated_at = now()
			 WHERE id = $1 AND status = 'active'`
		if _, err := tx.Exec(ctx, upd, id); err != nil {
			return nil, fmt.Errorf("aborting upload session: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("aborting upload session: %w", err)
	}
	return before, nil
}

// ListCursor is the opaque cursor for GET /uploads: newest first, id breaking
// ties.
type ListCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        uuid.UUID `json:"i"`
}

// List returns one page of a user's upload sessions, newest first.
func (s *Store) List(ctx context.Context, userID uuid.UUID, after *ListCursor, limit int) ([]Session, *ListCursor, error) {
	var (
		createdAt *time.Time
		id        *uuid.UUID
	)
	if after != nil {
		createdAt, id = &after.CreatedAt, &after.ID
	}

	const q = `SELECT ` + sessionCols + ` FROM upload_sessions
		 WHERE user_id = $1
		   AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::uuid))
		 ORDER BY created_at DESC, id DESC
		 LIMIT $4`
	rows, err := s.db.Query(ctx, q, userID, createdAt, id, limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("listing upload sessions: %w", err)
	}
	defer rows.Close()

	var items []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("listing upload sessions: %w", err)
		}
		items = append(items, *sess)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("listing upload sessions: %w", err)
	}

	if len(items) <= limit {
		return items, nil, nil
	}
	last := items[limit-1]
	return items[:limit], &ListCursor{CreatedAt: last.CreatedAt, ID: last.ID}, nil
}

// NameTaken reports whether a live sibling already holds this name. It is the
// create-path conflict check; the authoritative one runs again at complete,
// because a name can be taken while a 50 GB upload is in flight.
func (s *Store) NameTaken(ctx context.Context, parentID uuid.UUID, name string) (bool, error) {
	const q = `SELECT EXISTS (
		SELECT 1 FROM nodes
		 WHERE parent_id = $1 AND deleted_at IS NULL AND lower(name) = lower($2))`
	var taken bool
	if err := s.db.QueryRow(ctx, q, parentID, name).Scan(&taken); err != nil {
		return false, fmt.Errorf("checking for a name conflict: %w", err)
	}
	return taken, nil
}

// ------------------------------------------------------------------- parts --

// Parts returns a session's confirmed parts in ascending order.
func (s *Store) Parts(ctx context.Context, sessionID uuid.UUID) ([]Part, error) {
	const q = `SELECT part_number, size, etag, md5 FROM upload_parts
		 WHERE session_id = $1 ORDER BY part_number`
	rows, err := s.db.Query(ctx, q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing confirmed parts: %w", err)
	}
	defer rows.Close()

	var parts []Part
	for rows.Next() {
		var p Part
		if err := rows.Scan(&p.Number, &p.Size, &p.ETag, &p.MD5); err != nil {
			return nil, fmt.Errorf("listing confirmed parts: %w", err)
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

// ConfirmPart records a part, idempotently on (session_id, part_number).
//
// It overwrites rather than ignoring a repeat: the client's integrity retry
// re-PUTs a part and confirms it again, and the ledger has to end up holding
// the ETag of the bytes that are actually in Garage.
func (s *Store) ConfirmPart(ctx context.Context, sessionID uuid.UUID, p Part) error {
	const q = `
		INSERT INTO upload_parts (session_id, part_number, size, etag, md5)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (session_id, part_number) DO UPDATE
		   SET size = EXCLUDED.size, etag = EXCLUDED.etag, md5 = EXCLUDED.md5,
		       confirmed_at = now()`
	if _, err := s.db.Exec(ctx, q, sessionID, p.Number, p.Size, p.ETag, p.MD5); err != nil {
		return fmt.Errorf("confirming part %d: %w", p.Number, err)
	}
	return nil
}

// Reconcile merges what Garage holds into the ledger and reports the drift.
//
// ListParts wins, in both directions. A part in Garage that the ledger has
// never heard of is the kill-9 window -- the PUT landed, the confirmation did
// not -- so it is adopted with Garage's ETag and size and a NULL md5, and
// counts as confirmed. A part the ledger claims that Garage does not have is
// gone, so its row is deleted and the part is re-issued as missing.
//
// Rows already in the ledger keep their client MD5: it is the only thing the
// chimera guard can check against, and Garage cannot supply it.
func (s *Store) Reconcile(ctx context.Context, sessionID uuid.UUID, remote []Part) (adopted, dropped []int, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("reconciling parts: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is a no-op

	numbers := make([]int32, 0, len(remote))
	sizes := make([]int64, 0, len(remote))
	etags := make([]string, 0, len(remote))
	for _, p := range remote {
		numbers = append(numbers, int32(p.Number))
		sizes = append(sizes, p.Size)
		etags = append(etags, p.ETag)
	}

	const del = `DELETE FROM upload_parts
		 WHERE session_id = $1 AND NOT (part_number = ANY($2::int[]))
		RETURNING part_number`
	dropped, err = scanInts(tx.Query(ctx, del, sessionID, numbers))
	if err != nil {
		return nil, nil, fmt.Errorf("reconciling parts: %w", err)
	}

	if len(remote) > 0 {
		const ins = `
			INSERT INTO upload_parts (session_id, part_number, size, etag, md5)
			SELECT $1, n, sz, tag, NULL
			  FROM unnest($2::int[], $3::bigint[], $4::text[]) AS r(n, sz, tag)
			ON CONFLICT (session_id, part_number) DO NOTHING
			RETURNING part_number`
		adopted, err = scanInts(tx.Query(ctx, ins, sessionID, numbers, sizes, etags))
		if err != nil {
			return nil, nil, fmt.Errorf("reconciling parts: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("reconciling parts: %w", err)
	}
	return adopted, dropped, nil
}

// ----------------------------------------------------------------- scanning --

type row interface{ Scan(dest ...any) error }

func scanSession(r row) (*Session, error) {
	var (
		s      Session
		verify []int32
	)
	err := r.Scan(&s.ID, &s.UserID, &s.ParentID, &s.FileName, &s.FileSize, &s.Mime,
		&s.Fingerprint, &s.ConflictPolicy, &s.S3UploadID, &s.ObjectKey, &s.PartSize,
		&s.PartsTotal, &s.Status, &s.Mode, &verify, &s.NodeID,
		&s.CreatedAt, &s.UpdatedAt, &s.ExpiresAt)
	if err != nil {
		return nil, err
	}
	s.VerifyParts = toInt(verify)
	return &s, nil
}

func scanInts(rows pgx.Rows, err error) ([]int, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func toInt32(ns []int) []int32 {
	if ns == nil {
		return nil
	}
	out := make([]int32, len(ns))
	for i, n := range ns {
		out[i] = int32(n)
	}
	return out
}

func toInt(ns []int32) []int {
	if ns == nil {
		return nil
	}
	out := make([]int, len(ns))
	for i, n := range ns {
		out[i] = int(n)
	}
	return out
}
