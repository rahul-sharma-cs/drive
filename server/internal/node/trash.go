package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Trash listing, restore and purge.
//
// The whole model rests on one invariant, established by stampSubtreeTrashed:
// trashing stamps a single deleted_at across the subtree but only over rows
// that were live, so a subtree trashed earlier keeps its own, earlier
// timestamp. Restore then clears exactly the rows sharing the root's
// timestamp, which is what makes "restore the parent, the separately-trashed
// child stays in the trash as its own root" fall out of the data instead of
// out of bookkeeping.

// Trash moves a node and its live descendants into the trash, marking only the
// top node as the trashed root. A subtree that was already in the trash keeps
// its own, earlier timestamp -- which is what lets Restore put back exactly
// what one delete removed, and no more.
func (s *Store) Trash(ctx context.Context, ownerID, id uuid.UUID) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("trashing node: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	// lockLive refuses a node that is already trashed, so a second delete is a
	// 404 rather than a silent no-op that restamps the subtree.
	cur, err := lockLive(ctx, tx, ownerID, id)
	if err != nil {
		return err
	}
	if cur.IsRoot() {
		return ErrRootNode
	}
	if err := stampSubtreeTrashed(ctx, tx, id); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("trashing node: %w", err)
	}
	return nil
}

// TrashCursor is the keyset the trash listing paginates on. It travels to the
// client base64-encoded inside the opaque cursor.
type TrashCursor struct {
	DeletedAt time.Time `json:"d"`
	ID        uuid.UUID `json:"i"`
}

// ListTrash returns the trashed roots, most recently deleted first.
//
// The predicate is exactly trashed_root AND deleted_at IS NOT NULL:
// descendants of a trashed root are in the trash too, but the listing shows
// the thing the user actually deleted.
func (s *Store) ListTrash(ctx context.Context, ownerID uuid.UUID, cur *TrashCursor, limit int) ([]Node, *TrashCursor, error) {
	args := []any{ownerID}
	where := `owner_id = $1 AND trashed_root AND deleted_at IS NOT NULL`
	if cur != nil {
		args = append(args, cur.DeletedAt, cur.ID)
		where += fmt.Sprintf(` AND (deleted_at, id) < ($%d::timestamptz, $%d::uuid)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)

	sql := `SELECT ` + nodeCols + ` FROM nodes WHERE ` + where +
		fmt.Sprintf(` ORDER BY deleted_at DESC, id DESC LIMIT $%d`, len(args))

	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("listing the trash: %w", err)
	}
	defer rows.Close()

	var items []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("listing the trash: %w", err)
		}
		items = append(items, n)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("listing the trash: %w", err)
	}

	if len(items) <= limit {
		return items, nil, nil
	}
	items = items[:limit]
	last := items[len(items)-1]
	return items, &TrashCursor{DeletedAt: *last.DeletedAt, ID: last.ID}, nil
}

// Restore brings a trashed root, and the rows sharing its timestamp, back.
//
// Three things happen here that the trash model requires: rows stamped at a
// different (earlier) time stay in the trash as their own roots; a name
// collision at the destination auto-renames rather than failing, because
// restore must never block; and a destination folder that is itself trashed
// sends the node to the user's root instead.
func (s *Store) Restore(ctx context.Context, ownerID, id uuid.UUID) (Node, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Node{}, fmt.Errorf("restoring node: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	cur, err := lockTrashed(ctx, tx, ownerID, id)
	if err != nil {
		return Node{}, err
	}

	// Destination: the original parent if it is still a live folder of ours,
	// the user's root otherwise.
	dest := *cur.ParentID
	if _, err := folderForOwner(ctx, tx, ownerID, dest); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return Node{}, err
		}
		if err := tx.QueryRow(ctx,
			`SELECT id FROM nodes WHERE owner_id = $1 AND parent_id IS NULL`,
			ownerID).Scan(&dest); err != nil {
			return Node{}, fmt.Errorf("restoring node: locating the root folder: %w", err)
		}
	}

	taken, err := takenNames(ctx, tx, dest, cur.Name)
	if err != nil {
		return Node{}, err
	}
	name := NextFreeName(cur.Name, taken)

	// One statement clears the subtree and re-homes/renames the root, so the
	// partial unique index on (parent_id, lower(name)) never sees the old name
	// go live. The recursion descends only through rows carrying the root's own
	// deleted_at, read from the statement's pre-update snapshot.
	const sql = `
		WITH RECURSIVE sub AS (
		    SELECT id FROM nodes WHERE id = $1
		    UNION ALL
		    SELECT n.id FROM nodes n JOIN sub ON n.parent_id = sub.id
		     WHERE n.deleted_at = (SELECT deleted_at FROM nodes WHERE id = $1)
		)
		UPDATE nodes
		   SET deleted_at   = NULL,
		       trashed_root = false,
		       updated_at   = now(),
		       parent_id    = CASE WHEN id = $1 THEN $2::uuid ELSE parent_id END,
		       name         = CASE WHEN id = $1 THEN $3::text ELSE name END
		 WHERE id IN (SELECT id FROM sub)`
	if _, err := tx.Exec(ctx, sql, id, dest, name); err != nil {
		return Node{}, fmt.Errorf("restoring node: %w", writeErr(err))
	}

	restored, err := getLive(ctx, tx, ownerID, id)
	if err != nil {
		return Node{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, fmt.Errorf("restoring node: %w", err)
	}
	return restored, nil
}

// Purge permanently deletes a trashed root and everything under it.
//
// It takes no S3 client on purpose: purging only decrements blob refcounts.
// Deleting the objects is the GC sweep's job -- blobs at refcount 0 past a 2 h
// grace -- which is what makes a crash between the two steps survivable.
func (s *Store) Purge(ctx context.Context, ownerID, id uuid.UUID) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("purging node: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := lockTrashed(ctx, tx, ownerID, id); err != nil {
		return err
	}

	// The whole subtree, grouped by depth, deepest level first: nodes.parent_id
	// is ON DELETE NO ACTION, so children have to go before their parent.
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE sub AS (
		    SELECT id, 0 AS depth FROM nodes WHERE id = $1
		    UNION ALL
		    SELECT n.id, sub.depth + 1 FROM nodes n JOIN sub ON n.parent_id = sub.id
		)
		SELECT id, depth FROM sub ORDER BY depth DESC`, id)
	if err != nil {
		return fmt.Errorf("purging node: %w", err)
	}
	var (
		ids    []uuid.UUID
		levels [][]uuid.UUID
		prev   = -1
	)
	for rows.Next() {
		var nodeID uuid.UUID
		var depth int
		if err := rows.Scan(&nodeID, &depth); err != nil {
			rows.Close()
			return fmt.Errorf("purging node: %w", err)
		}
		if depth != prev {
			levels = append(levels, nil)
			prev = depth
		}
		levels[len(levels)-1] = append(levels[len(levels)-1], nodeID)
		ids = append(ids, nodeID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("purging node: %w", err)
	}

	// Upload sessions still aiming at a folder that is about to vanish are
	// aborted; their multiparts fall to the GC sweep. This has to run before
	// the delete, while parent_id still points at the purged rows.
	if _, err := tx.Exec(ctx,
		`UPDATE upload_sessions SET status = 'aborted', updated_at = now()
		  WHERE status = 'active' AND parent_id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("purging node: aborting upload sessions: %w", err)
	}

	// One decrement per referencing node, so two nodes sharing a blob take it
	// down by two. Never below zero, and never a DeleteObject.
	if _, err := tx.Exec(ctx, `
		UPDATE blobs b
		   SET refcount = GREATEST(b.refcount - c.n, 0)
		  FROM (
		        SELECT blob_id, count(*) AS n
		          FROM nodes
		         WHERE id = ANY($1) AND blob_id IS NOT NULL
		         GROUP BY blob_id
		       ) c
		 WHERE b.id = c.blob_id`, ids); err != nil {
		return fmt.Errorf("purging node: decrementing refcounts: %w", err)
	}

	// Purging a node revokes its shares implicitly; the allowlist, OTP and
	// guest-session rows cascade from shares.
	if _, err := tx.Exec(ctx, `DELETE FROM shares WHERE node_id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("purging node: deleting shares: %w", err)
	}

	for _, level := range levels {
		if _, err := tx.Exec(ctx, `DELETE FROM nodes WHERE id = ANY($1)`, level); err != nil {
			return fmt.Errorf("purging node: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("purging node: %w", err)
	}
	return nil
}

// lockTrashed reads and locks a node that restore and purge may act on: ours,
// in the trash, and the root of its own trash operation -- the only ids the
// trash listing ever hands out. A user's root folder is refused outright.
func lockTrashed(ctx context.Context, q querier, ownerID, id uuid.UUID) (Node, error) {
	const sql = `SELECT ` + nodeCols + ` FROM nodes
		 WHERE id = $1 AND owner_id = $2 FOR UPDATE`
	n, err := scanNode(q.QueryRow(ctx, sql, id, ownerID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, fmt.Errorf("reading node: %w", err)
	}
	if n.IsRoot() {
		return Node{}, ErrRootNode
	}
	if n.DeletedAt == nil || !n.TrashedRoot {
		return Node{}, ErrNotFound
	}
	return n, nil
}
