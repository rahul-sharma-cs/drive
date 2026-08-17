package node

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Get returns one live node belonging to ownerID. A trashed node, another
// user's node and a node that never existed are all ErrNotFound.
func (s *Store) Get(ctx context.Context, ownerID, id uuid.UUID) (Node, error) {
	return getLive(ctx, s.db, ownerID, id)
}

// Children lists one page of a folder's live children: folders before files,
// then by case-folded name, then by id. The parent itself is authorized first
// -- listing is a read, but it still must not confirm that another user's
// folder exists.
//
// limit is the page size; the returned cursor is non-nil only when a further
// page exists.
func (s *Store) Children(ctx context.Context, ownerID, parentID uuid.UUID, after *ChildCursor, limit int) ([]Node, *ChildCursor, error) {
	if _, err := folderForOwner(ctx, s.db, ownerID, parentID); err != nil {
		return nil, nil, err
	}

	var (
		rank *int
		name *string
		id   *uuid.UUID
	)
	if after != nil {
		rank, name, id = &after.KindRank, &after.Name, &after.ID
	}

	const q = `
		SELECT ` + nodeCols + `, lower(name)
		  FROM nodes
		 WHERE parent_id = $1 AND deleted_at IS NULL
		   AND ($2::int IS NULL
		        OR (CASE kind WHEN 'folder' THEN 0 ELSE 1 END, lower(name), id)
		            > ($2::int, $3::text, $4::uuid))
		 ORDER BY (CASE kind WHEN 'folder' THEN 0 ELSE 1 END), lower(name), id
		 LIMIT $5`

	rows, err := s.db.Query(ctx, q, parentID, rank, name, id, limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("listing children: %w", err)
	}
	defer rows.Close()

	var (
		items  []Node
		sorted []string
	)
	for rows.Next() {
		n, sortName, err := scanNodeSorted(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("listing children: %w", err)
		}
		items = append(items, n)
		sorted = append(sorted, sortName)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("listing children: %w", err)
	}

	if len(items) <= limit {
		return items, nil, nil
	}
	last := items[limit-1]
	next := &ChildCursor{KindRank: rankOf(last.Kind), Name: sorted[limit-1], ID: last.ID}
	return items[:limit], next, nil
}

// CreateFolder creates a folder under parentID. The destination is authorized
// independently of anything else in the request.
//
// With PolicyReuse a collision with an existing live folder is not an error:
// the existing folder is returned with existing=true, which is what makes
// dropping the same tree twice idempotent.
func (s *Store) CreateFolder(ctx context.Context, ownerID, parentID uuid.UUID, rawName, policy string) (Node, bool, error) {
	name, err := Clean(rawName)
	if err != nil {
		return Node{}, false, err
	}
	if err := allowPolicy(policy, PolicyReplace, PolicyRename, PolicyReuse); err != nil {
		return Node{}, false, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Node{}, false, fmt.Errorf("creating folder: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := folderForOwner(ctx, tx, ownerID, parentID); err != nil {
		return Node{}, false, err
	}

	final, existing, err := resolveName(ctx, tx, parentID, name, KindFolder, policy, nil)
	if err != nil {
		return Node{}, false, err
	}
	if existing != nil {
		return *existing, true, nil
	}

	const q = `
		INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		VALUES ($1, $2, $3, 'folder', $4)
		RETURNING ` + nodeCols
	created, err := scanNode(tx.QueryRow(ctx, q, uuid.New(), ownerID, parentID, final))
	if err != nil {
		return Node{}, false, fmt.Errorf("creating folder: %w", writeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, false, fmt.Errorf("creating folder: %w", writeErr(err))
	}
	return created, false, nil
}

// Update renames and/or moves a node. Both are one operation because the
// browser's "move into a folder that already has a file of this name" is both
// at once, and the conflict has to be resolved against the destination.
//
// A move authorizes the destination folder independently and then walks the
// destination's ancestry inside the same transaction: moving a folder into
// itself or into its own descendant would detach the subtree from the tree
// entirely, so it is ErrCycle.
func (s *Store) Update(ctx context.Context, ownerID, id uuid.UUID, newName *string, newParentID *uuid.UUID, policy string) (Node, error) {
	if err := allowPolicy(policy, PolicyReplace, PolicyRename); err != nil {
		return Node{}, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Node{}, fmt.Errorf("updating node: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	cur, err := lockLive(ctx, tx, ownerID, id)
	if err != nil {
		return Node{}, err
	}
	if cur.IsRoot() {
		return Node{}, ErrRootNode
	}

	parentID := *cur.ParentID
	if newParentID != nil && *newParentID != parentID {
		if _, err := folderForOwner(ctx, tx, ownerID, *newParentID); err != nil {
			return Node{}, err
		}
		if err := rejectCycle(ctx, tx, *newParentID, id); err != nil {
			return Node{}, err
		}
		parentID = *newParentID
	}

	name := cur.Name
	if newName != nil {
		if name, err = Clean(*newName); err != nil {
			return Node{}, err
		}
	}

	// selfID excludes the node from its own conflict check: renaming "a.txt"
	// to "A.txt", or moving nothing at all, must not collide with itself.
	final, _, err := resolveName(ctx, tx, parentID, name, cur.Kind, policy, &id)
	if err != nil {
		return Node{}, err
	}

	const q = `
		UPDATE nodes SET name = $1, parent_id = $2, updated_at = now()
		 WHERE id = $3
		RETURNING ` + nodeCols
	updated, err := scanNode(tx.QueryRow(ctx, q, final, parentID, id))
	if err != nil {
		return Node{}, fmt.Errorf("updating node: %w", writeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, fmt.Errorf("updating node: %w", writeErr(err))
	}
	return updated, nil
}

// Copy copies a file into a destination folder. Files only: a folder copy
// would be a recursive tree walk plus a refcount per file, which the MVP does
// not do (ErrUnsupported).
//
// No bytes move. The new node points at the same blob, and the refcount
// increment is a conditional UPDATE that re-verifies the source node is still
// live and its blob row still exists -- 0 rows updated means the source was
// purged out from under us, so nothing is inserted.
func (s *Store) Copy(ctx context.Context, ownerID, srcID, destParentID uuid.UUID, rawName *string, policy string) (Node, error) {
	if err := allowPolicy(policy, PolicyReplace, PolicyRename); err != nil {
		return Node{}, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Node{}, fmt.Errorf("copying node: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	src, err := getLive(ctx, tx, ownerID, srcID)
	if err != nil {
		return Node{}, err
	}
	if src.Kind != KindFile {
		return Node{}, ErrUnsupported
	}

	name := src.Name
	if rawName != nil {
		if name, err = Clean(*rawName); err != nil {
			return Node{}, err
		}
	}

	// The destination is authorized before any write, including before the
	// refcount moves.
	if _, err := folderForOwner(ctx, tx, ownerID, destParentID); err != nil {
		return Node{}, err
	}

	final, _, err := resolveName(ctx, tx, destParentID, name, KindFile, policy, nil)
	if err != nil {
		return Node{}, err
	}

	// unreferenced_at is cleared with the increment: a blob that has a reference
	// again is not waiting out any grace, and leaving a stale stamp behind would
	// let the collector delete bytes this copy points at.
	const bump = `
		UPDATE blobs SET refcount = refcount + 1, unreferenced_at = NULL
		 WHERE id = (SELECT blob_id FROM nodes
		              WHERE id = $1 AND owner_id = $2
		                AND kind = 'file' AND deleted_at IS NULL)
		RETURNING id`
	var blobID uuid.UUID
	if err := tx.QueryRow(ctx, bump, srcID, ownerID).Scan(&blobID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, fmt.Errorf("copying node: %w", err)
	}

	const q = `
		INSERT INTO nodes (id, owner_id, parent_id, kind, name, blob_id, size, mime)
		VALUES ($1, $2, $3, 'file', $4, $5, $6, $7)
		RETURNING ` + nodeCols
	created, err := scanNode(tx.QueryRow(ctx, q, uuid.New(), ownerID, destParentID, final, blobID, src.Size, src.Mime))
	if err != nil {
		return Node{}, fmt.Errorf("copying node: %w", writeErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return Node{}, fmt.Errorf("copying node: %w", writeErr(err))
	}
	return created, nil
}

// Trashing a node outright is node.Trash (trash.go); the helper below exists
// because conflict_policy=replace has to trash the colliding node inside the
// same transaction as the write that takes its name.

// ------------------------------------------------------------------ helpers --

// getLive reads one live node owned by ownerID, or ErrNotFound.
func getLive(ctx context.Context, q querier, ownerID, id uuid.UUID) (Node, error) {
	const sql = `SELECT ` + nodeCols + ` FROM nodes
		 WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL`
	n, err := scanNode(q.QueryRow(ctx, sql, id, ownerID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, fmt.Errorf("selecting node: %w", err)
	}
	return n, nil
}

// lockLive is getLive plus a row lock, for the read-check-write paths.
func lockLive(ctx context.Context, q querier, ownerID, id uuid.UUID) (Node, error) {
	const sql = `SELECT ` + nodeCols + ` FROM nodes
		 WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL
		 FOR UPDATE`
	n, err := scanNode(q.QueryRow(ctx, sql, id, ownerID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, fmt.Errorf("selecting node: %w", err)
	}
	return n, nil
}

// folderForOwner is the destination check every body-supplied parent goes
// through: exists, is a folder, is not trashed, belongs to the caller. Every
// failure is ErrNotFound.
func folderForOwner(ctx context.Context, q querier, ownerID, id uuid.UUID) (Node, error) {
	n, err := getLive(ctx, q, ownerID, id)
	if err != nil {
		return Node{}, err
	}
	if n.Kind != KindFolder {
		return Node{}, ErrNotFound
	}
	return n, nil
}

// rejectCycle refuses a move whose destination is the moved node itself or one
// of its descendants, by walking the destination's ancestry.
func rejectCycle(ctx context.Context, q querier, destID, movingID uuid.UUID) error {
	const sql = `
		WITH RECURSIVE up AS (
		    SELECT id, parent_id FROM nodes WHERE id = $1
		    UNION ALL
		    SELECT n.id, n.parent_id FROM nodes n JOIN up ON n.id = up.parent_id
		)
		SELECT EXISTS (SELECT 1 FROM up WHERE id = $2)`
	var cycle bool
	if err := q.QueryRow(ctx, sql, destID, movingID).Scan(&cycle); err != nil {
		return fmt.Errorf("checking ancestry: %w", err)
	}
	if cycle {
		return ErrCycle
	}
	return nil
}

// resolveName applies the conflict policy to a name in a destination folder.
// It returns the name to use, or the existing folder when the policy is reuse
// and one is there. selfID, when set, is excluded from the collision check.
func resolveName(ctx context.Context, q querier, parentID uuid.UUID, name, kind, policy string, selfID *uuid.UUID) (string, *Node, error) {
	const sql = `SELECT ` + nodeCols + ` FROM nodes
		 WHERE parent_id = $1 AND deleted_at IS NULL AND lower(name) = lower($2)
		   AND ($3::uuid IS NULL OR id <> $3::uuid)`
	collision, err := scanNode(q.QueryRow(ctx, sql, parentID, name, selfID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return name, nil, nil
		}
		return "", nil, fmt.Errorf("checking for a name conflict: %w", err)
	}

	switch policy {
	case PolicyReuse:
		// Only a folder can be reused; a file of the same name is still a
		// conflict the client has to answer for.
		if kind == KindFolder && collision.Kind == KindFolder {
			return "", &collision, nil
		}
		return "", nil, ErrNameConflict
	case PolicyRename:
		taken, err := takenNames(ctx, q, parentID, name)
		if err != nil {
			return "", nil, err
		}
		return NextFreeName(name, taken), nil, nil
	case PolicyReplace:
		if err := stampSubtreeTrashed(ctx, q, collision.ID); err != nil {
			return "", nil, err
		}
		return name, nil, nil
	default:
		return "", nil, ErrNameConflict
	}
}

// takenNames returns the sibling names that could block name or one of its
// numbered variants.
func takenNames(ctx context.Context, q querier, parentID uuid.UUID, name string) ([]string, error) {
	stem, ext := splitExt(name)
	const sql = `SELECT name FROM nodes
		 WHERE parent_id = $1 AND deleted_at IS NULL
		   AND (lower(name) = lower($2) OR lower(name) LIKE $3 ESCAPE '\')`
	rows, err := q.Query(ctx, sql, parentID, name, likePattern(stem, ext))
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

// stampSubtreeTrashed marks a node and its live descendants deleted in one
// recursive-CTE update, restricted to rows that are not already deleted so an
// inner subtree trashed earlier keeps its own timestamp and stays its own
// trash root.
func stampSubtreeTrashed(ctx context.Context, q querier, rootID uuid.UUID) error {
	const sql = `
		WITH RECURSIVE sub AS (
		    SELECT id FROM nodes WHERE id = $1 AND deleted_at IS NULL
		    UNION ALL
		    SELECT n.id FROM nodes n JOIN sub ON n.parent_id = sub.id
		     WHERE n.deleted_at IS NULL
		)
		UPDATE nodes
		   SET deleted_at = now(), updated_at = now(), trashed_root = (id = $1)
		 WHERE id IN (SELECT id FROM sub)`
	if _, err := q.Exec(ctx, sql, rootID); err != nil {
		return fmt.Errorf("trashing subtree: %w", err)
	}
	return nil
}

// allowPolicy rejects a conflict_policy this endpoint does not accept. An
// empty policy always means "tell me about the conflict instead".
func allowPolicy(policy string, allowed ...string) error {
	if policy == "" || slices.Contains(allowed, policy) {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrInvalidPolicy, policy)
}

// writeErr turns the sibling-uniqueness violation -- the race where two
// requests insert the same name at once -- into ErrNameConflict, and leaves
// every other database error alone.
func writeErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "nodes_parent_name_idx" {
		return ErrNameConflict
	}
	return err
}

// scanNodeSorted reads a row selected with nodeCols plus lower(name).
func scanNodeSorted(row pgx.Row) (Node, string, error) {
	var (
		n        Node
		sortName string
	)
	err := row.Scan(&n.ID, &n.OwnerID, &n.ParentID, &n.Kind, &n.Name, &n.BlobID,
		&n.Size, &n.Mime, &n.CreatedAt, &n.UpdatedAt, &n.DeletedAt, &n.TrashedRoot, &sortName)
	return n, sortName, err
}

// rankOf is the children ordering's folders-first key.
func rankOf(kind string) int {
	if kind == KindFolder {
		return 0
	}
	return 1
}
