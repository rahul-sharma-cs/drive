package share

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/node"
)

// querier is the subset of pgx both the pool and a transaction implement.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is the share package's data access. It holds no state beyond the pool.
type Store struct {
	db    *pgxpool.Pool
	nodes *node.Store
}

// NewStore wraps a pool.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db, nodes: node.NewStore(db)}
}

// ------------------------------------------------------------------ owner --

// shareCols is the owner-facing select every listing and read-back shares,
// in the order scanShare reads it. The blob join is what makes NodeLive
// honest: a file row whose bytes are gone is not live, exactly as
// node.Store.Download would refuse it.
const shareCols = `
	s.id, s.created_by, s.password_hash IS NOT NULL, s.expires_at, s.max_downloads,
	s.download_count, s.created_at, s.revoked_at,
	n.id, n.parent_id, n.name, n.size, n.mime,
	(n.deleted_at IS NULL AND n.kind = 'file' AND b.id IS NOT NULL)
	  FROM shares s
	  JOIN nodes n ON n.id = s.node_id
	  LEFT JOIN blobs b ON b.id = n.blob_id`

func scanShare(row pgx.Row) (Share, error) {
	var sh Share
	err := row.Scan(&sh.ID, &sh.CreatedBy, &sh.HasPassword, &sh.ExpiresAt, &sh.MaxDownloads,
		&sh.DownloadCount, &sh.CreatedAt, &sh.RevokedAt,
		&sh.Node.ID, &sh.Node.ParentID, &sh.Node.Name, &sh.Node.Size, &sh.Node.Mime,
		&sh.NodeLive)
	return sh, err
}

// Create makes the one active link for a file the caller owns and returns the
// raw token, which is shown once and never recoverable afterwards.
//
// The node is resolved in two owner-scoped reads before anything is written:
// node.Store.Get, so a trashed node, another user's and an unknown one are all
// node.ErrNotFound and a folder is ErrUnsupported; then node.Store.Download,
// whose inner join on blobs refuses a file row with no bytes. A share on a
// blobless node would be permanently dead while occupying the file's one slot,
// so the create path uses the same liveness predicate the public routes do.
//
// The write is one statement. ON CONFLICT names the partial unique index's
// predicate word for word, and zero rows back is ErrExists whether the slot
// was taken last week or by a concurrent request a millisecond earlier; there
// is no pre-check and no window between checking and inserting.
func (s *Store) Create(ctx context.Context, ownerID, nodeID uuid.UUID, set Settings) (Share, string, error) {
	n, err := s.nodes.Get(ctx, ownerID, nodeID)
	if err != nil {
		return Share{}, "", err
	}
	if n.Kind != node.KindFile {
		return Share{}, "", ErrUnsupported
	}
	if _, err := s.nodes.Download(ctx, ownerID, nodeID); err != nil {
		return Share{}, "", err
	}

	raw, hash, err := auth.NewToken()
	if err != nil {
		return Share{}, "", err
	}

	const q = `
		INSERT INTO shares (id, node_id, created_by, mode, token_hash, permission,
		                    password_hash, expires_at, max_downloads)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (node_id) WHERE revoked_at IS NULL DO NOTHING
		RETURNING id`
	var id uuid.UUID
	err = s.db.QueryRow(ctx, q, uuid.New(), nodeID, ownerID, ModePublic, hash, PermissionView,
		set.Password.Hash, set.ExpiresAt, set.MaxDownloads).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Share{}, "", ErrExists
	}
	if err != nil {
		return Share{}, "", fmt.Errorf("share: creating: %w", err)
	}

	sh, err := get(ctx, s.db, ownerID, id)
	if err != nil {
		return Share{}, "", err
	}
	return sh, raw, nil
}

// get reads back one active share the owner holds.
func get(ctx context.Context, q querier, ownerID, id uuid.UUID) (Share, error) {
	const sql = `SELECT ` + shareCols + `
		 WHERE s.id = $1 AND s.created_by = $2 AND s.revoked_at IS NULL`
	sh, err := scanShare(q.QueryRow(ctx, sql, id, ownerID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Share{}, ErrNotFound
		}
		return Share{}, fmt.Errorf("share: reading: %w", err)
	}
	return sh, nil
}

// List pages the caller's active shares, newest first. nodeID narrows the
// listing to one file; after is the previous page's cursor. The returned
// cursor is non-nil only when a further page exists.
//
// The WHERE repeats shares_owner_active_idx's predicate as written, because
// the planner uses a partial index only when it can prove the query implies
// it; a rewrite here has to move the migration too.
func (s *Store) List(ctx context.Context, ownerID uuid.UUID, nodeID *uuid.UUID, after *Cursor, limit int) ([]Share, *Cursor, error) {
	var (
		afterCreated *time.Time
		afterID      *uuid.UUID
	)
	if after != nil {
		afterCreated, afterID = &after.CreatedAt, &after.ID
	}

	const q = `SELECT ` + shareCols + `
		 WHERE s.created_by = $1 AND s.revoked_at IS NULL
		   AND ($2::uuid IS NULL OR s.node_id = $2::uuid)
		   AND ($3::timestamptz IS NULL OR (s.created_at, s.id) < ($3::timestamptz, $4::uuid))
		 ORDER BY s.created_at DESC, s.id DESC
		 LIMIT $5`
	rows, err := s.db.Query(ctx, q, ownerID, nodeID, afterCreated, afterID, limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("share: listing: %w", err)
	}
	defer rows.Close()

	var items []Share
	for rows.Next() {
		sh, err := scanShare(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("share: listing: %w", err)
		}
		items = append(items, sh)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("share: listing: %w", err)
	}

	if len(items) <= limit {
		return items, nil, nil
	}
	last := items[limit-1]
	return items[:limit], &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}, nil
}

// Regenerate replaces the token of an active share the owner holds and
// returns the new raw token. The old link stops working at once, every guest
// session goes with it, and download_count starts again at zero: a new link is
// a new budget, and a replacement born exhausted would be no replacement.
func (s *Store) Regenerate(ctx context.Context, ownerID, id uuid.UUID) (Share, string, error) {
	raw, hash, err := auth.NewToken()
	if err != nil {
		return Share{}, "", err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Share{}, "", fmt.Errorf("share: regenerating: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	const q = `
		UPDATE shares SET token_hash = $3, download_count = 0
		 WHERE id = $1 AND created_by = $2 AND revoked_at IS NULL
		RETURNING id`
	if err := tx.QueryRow(ctx, q, id, ownerID, hash).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Share{}, "", ErrNotFound
		}
		return Share{}, "", fmt.Errorf("share: regenerating: %w", err)
	}
	if err := deleteGuests(ctx, tx, id); err != nil {
		return Share{}, "", err
	}
	sh, err := get(ctx, tx, ownerID, id)
	if err != nil {
		return Share{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return Share{}, "", fmt.Errorf("share: regenerating: %w", err)
	}
	return sh, raw, nil
}

// Settings replaces an active share's expiry and cap outright and applies the
// password change: keep leaves the column and the guest sessions alone; set
// stores the new hash and deletes the sessions, because a gate that leaves
// the people already inside is not a gate; clear removes the column and
// deletes the sessions only when there was a password to clear -- the
// sessions were minted through a gate that no longer describes the link.
func (s *Store) Settings(ctx context.Context, ownerID, id uuid.UUID, set Settings) (Share, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Share{}, fmt.Errorf("share: updating settings: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	// The lock also answers "was there a password", which the session rule
	// below turns on and the UPDATE would have already overwritten.
	var hadPassword bool
	const lock = `
		SELECT password_hash IS NOT NULL FROM shares
		 WHERE id = $1 AND created_by = $2 AND revoked_at IS NULL
		 FOR UPDATE`
	if err := tx.QueryRow(ctx, lock, id, ownerID).Scan(&hadPassword); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Share{}, ErrNotFound
		}
		return Share{}, fmt.Errorf("share: updating settings: %w", err)
	}

	const q = `
		UPDATE shares SET expires_at = $2, max_downloads = $3,
		       password_hash = CASE WHEN $4::bool THEN $5 ELSE password_hash END
		 WHERE id = $1`
	if _, err := tx.Exec(ctx, q, id, set.ExpiresAt, set.MaxDownloads, set.Password.Set, set.Password.Hash); err != nil {
		return Share{}, fmt.Errorf("share: updating settings: %w", err)
	}
	if set.Password.Set && (set.Password.Hash != nil || hadPassword) {
		if err := deleteGuests(ctx, tx, id); err != nil {
			return Share{}, err
		}
	}
	sh, err := get(ctx, tx, ownerID, id)
	if err != nil {
		return Share{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Share{}, fmt.Errorf("share: updating settings: %w", err)
	}
	return sh, nil
}

// Revoke stops an active share the owner holds: revoked_at is set, the guest
// sessions go, and the row stays so the access log remains attributable. A
// share already revoked is ErrNotFound, like any id the caller has no live
// share for.
func (s *Store) Revoke(ctx context.Context, ownerID, id uuid.UUID) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("share: revoking: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	const q = `
		UPDATE shares SET revoked_at = now()
		 WHERE id = $1 AND created_by = $2 AND revoked_at IS NULL
		RETURNING id`
	if err := tx.QueryRow(ctx, q, id, ownerID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("share: revoking: %w", err)
	}
	if err := deleteGuests(ctx, tx, id); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("share: revoking: %w", err)
	}
	return nil
}

// deleteGuests ends every guest session of a share. Regenerate, revoke and a
// raised password gate all call it: each puts a recipient mid-page back at
// the front door on their next click.
func deleteGuests(ctx context.Context, q querier, shareID uuid.UUID) error {
	if _, err := q.Exec(ctx, `DELETE FROM share_guest_sessions WHERE share_id = $1`, shareID); err != nil {
		return fmt.Errorf("share: deleting guest sessions: %w", err)
	}
	return nil
}

// -------------------------------------------------------------- recipient --

// Resolve answers a public share route from the token's hash, in one query.
//
// ErrNotFound means no share row matches, and nothing else: every dead state
// comes back as a Resolved with its State and the share id filled, so the
// handler can answer the one identical 404 and still write the denied row the
// owner is owed -- without a second query and without a window between
// "found" and "allowed". An unknown token yields no id to attribute anything
// to, which is what keeps a scan from filling the log.
//
// State is computed in SQL from the same row set that resolves the token.
// Revoked and expired are the share's own columns; trashed is the node's
// deleted_at; purged is a node that is not a live file with bytes -- which a
// real purge never leaves behind (it deletes the share), but a half-applied
// restore or a future bug could.
func (s *Store) Resolve(ctx context.Context, tokenHash []byte) (*Resolved, error) {
	const q = `
		SELECT s.id, s.node_id, n.parent_id,
		       COALESCE(n.name, ''), COALESCE(b.size, 0), COALESCE(n.mime, ''), COALESCE(b.object_key, ''),
		       s.password_hash, s.expires_at, s.max_downloads, s.download_count,
		       CASE
		         WHEN s.revoked_at IS NOT NULL THEN 'revoked'
		         WHEN s.expires_at IS NOT NULL AND s.expires_at <= now() THEN 'expired'
		         WHEN n.deleted_at IS NOT NULL THEN 'trashed'
		         WHEN n.id IS NULL OR n.kind <> 'file' OR b.id IS NULL THEN 'purged'
		         ELSE 'live'
		       END
		  FROM shares s
		  LEFT JOIN nodes n ON n.id = s.node_id
		  LEFT JOIN blobs b ON b.id = n.blob_id
		 WHERE s.token_hash = $1`

	var r Resolved
	if err := s.db.QueryRow(ctx, q, tokenHash).Scan(
		&r.ShareID, &r.NodeID, &r.ParentID, &r.Name, &r.Size, &r.Mime, &r.ObjectKey,
		&r.PasswordHash, &r.ExpiresAt, &r.MaxDownloads, &r.DownloadCount, &r.State,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("share: resolving: %w", err)
	}
	return &r, nil
}

// MintGuest opens a guest session on a share and returns the raw cookie value
// alongside it. Only sha256(raw) is stored.
func (s *Store) MintGuest(ctx context.Context, shareID uuid.UUID) (string, Guest, error) {
	raw, hash, err := auth.NewToken()
	if err != nil {
		return "", Guest{}, err
	}
	const q = `
		INSERT INTO share_guest_sessions (id, share_id, token_hash, expires_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4))
		RETURNING expires_at`
	g := Guest{ID: uuid.New(), ShareID: shareID}
	if err := s.db.QueryRow(ctx, q, g.ID, shareID, hash, GuestSessionTTL.Seconds()).Scan(&g.ExpiresAt); err != nil {
		return "", Guest{}, fmt.Errorf("share: minting guest session: %w", err)
	}
	return raw, g, nil
}

// GuestFor resolves a raw cookie value to the live guest session it names on
// this share, or ErrNotFound.
//
// The share predicate is the point. A guest of share A presents its cookie at
// share B's routes too -- the cookie path is shared -- and the row behind it
// still says share_id = A, so it answers nothing for B however the cookie was
// named.
func (s *Store) GuestFor(ctx context.Context, shareID uuid.UUID, raw string) (Guest, error) {
	if raw == "" {
		return Guest{}, ErrNotFound
	}
	const q = `
		SELECT id, share_id, downloaded_at, expires_at
		  FROM share_guest_sessions
		 WHERE token_hash = $1 AND share_id = $2 AND expires_at > now()`
	var g Guest
	if err := s.db.QueryRow(ctx, q, auth.HashToken(raw), shareID).Scan(&g.ID, &g.ShareID, &g.DownloadedAt, &g.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Guest{}, ErrNotFound
		}
		return Guest{}, fmt.Errorf("share: reading guest session: %w", err)
	}
	return g, nil
}

// ReuseGuest slides a live guest session's expiry to now()+GuestSessionTTL and
// returns it. A session already gone is ErrNotFound. Thirty minutes means
// thirty minutes of inactivity: a page open for an hour keeps its session as
// long as it keeps using it.
func (s *Store) ReuseGuest(ctx context.Context, guestID uuid.UUID) (Guest, error) {
	const q = `
		UPDATE share_guest_sessions
		   SET expires_at = now() + make_interval(secs => $2)
		 WHERE id = $1 AND expires_at > now()
		RETURNING id, share_id, downloaded_at, expires_at`
	var g Guest
	if err := s.db.QueryRow(ctx, q, guestID, GuestSessionTTL.Seconds()).Scan(&g.ID, &g.ShareID, &g.DownloadedAt, &g.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Guest{}, ErrNotFound
		}
		return Guest{}, fmt.Errorf("share: sliding guest session: %w", err)
	}
	return g, nil
}

// CountOnce spends one download on a guest session and reports whether the
// cap refused it. It is the cap transaction -- the two statements, behind a
// lock that makes their answers unambiguous:
//
//  0. lock the session row FOR UPDATE, live and belonging to this share. No
//     row is ErrNotFound: it lapsed or was deleted between the handler's
//     read and this call, and a download issued against it would never be
//     counted;
//  1. a row already stamped is a re-issue -- reloads, Range re-requests and
//     double clicks never count twice -- and nothing is written;
//  2. otherwise stamp the session's downloaded_at and add one to the share's
//     download_count, only while it is under max_downloads -- and zero rows
//     there rolls both back, so a refused session keeps its NULL stamp and
//     can try again after the owner raises the cap.
//
// Two sessions racing for the last slot serialise on the shares row, and the
// loser re-evaluates the predicate against the winner's count. The lock is
// what tells "already stamped" from "gone": on the stamp alone both are zero
// rows, and a session the owner's revoke had just deleted would read as
// counted.
func (s *Store) CountOnce(ctx context.Context, sessionID, shareID uuid.UUID) (exhausted bool, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("share: counting download: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	const lock = `
		SELECT downloaded_at FROM share_guest_sessions
		 WHERE id = $1 AND share_id = $2 AND expires_at > now()
		 FOR UPDATE`
	var downloadedAt *time.Time
	if err := tx.QueryRow(ctx, lock, sessionID, shareID).Scan(&downloadedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("share: counting download: %w", err)
	}
	if downloadedAt != nil {
		// Already counted: nothing to write.
		return false, nil
	}

	const stamp = `
		UPDATE share_guest_sessions SET downloaded_at = now()
		 WHERE id = $1 AND share_id = $2 AND downloaded_at IS NULL`
	if _, err := tx.Exec(ctx, stamp, sessionID, shareID); err != nil {
		return false, fmt.Errorf("share: counting download: %w", err)
	}

	const count = `
		UPDATE shares SET download_count = download_count + 1
		 WHERE id = $1 AND (max_downloads IS NULL OR download_count < max_downloads)`
	tag, err := tx.Exec(ctx, count, shareID)
	if err != nil {
		return false, fmt.Errorf("share: counting download: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The deferred rollback undoes the stamp.
		return true, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("share: counting download: %w", err)
	}
	return false, nil
}

// Log writes one share_access_log row. Call it outside any transaction that
// can roll back: a denied row for a spent cap has to survive CountOnce's
// rollback.
//
// shareID is nil only when there is no share to attribute the event to, which
// callers avoid -- an unknown token writes nothing. email is Mode 2's and is
// always nil here. An empty ip stores NULL, because an unreadable peer yields
// "" and "" is not an inet; userAgent is cut to MaxUserAgent runes, the header
// being attacker-controlled text of unbounded length.
func (s *Store) Log(ctx context.Context, shareID *uuid.UUID, email *string, action, ip, userAgent string) error {
	const q = `
		INSERT INTO share_access_log (share_id, email, ip, user_agent, action)
		VALUES ($1, $2, $3, $4, $5)`
	if _, err := s.db.Exec(ctx, q, shareID, email, nullable(ip), nullable(truncateRunes(userAgent, MaxUserAgent)), action); err != nil {
		return fmt.Errorf("share: logging %s: %w", action, err)
	}
	return nil
}

// nullable turns an empty string into a SQL NULL.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// truncateRunes caps s at n runes, cutting on a rune boundary so the result is
// still valid UTF-8.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
