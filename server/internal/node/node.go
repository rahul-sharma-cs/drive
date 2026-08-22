// Package node owns Drive's file tree: the nodes table, the ownership rule
// that guards every path into it, and filename hygiene.
//
// The one invariant worth stating up front is the authorization rule. Every
// operation re-checks ownership of the node named in the path, and every
// operation that takes a destination folder in the body authorizes that
// destination independently -- exists, is a folder, is not trashed, belongs to
// the caller -- before any row is written or any refcount moves. A failure of
// either check is ErrNotFound, never "forbidden": node ids are opaque UUIDs and
// the API must not confirm that one exists.
package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The two kinds of node. A folder has children and no size; a file has a blob
// and no children.
const (
	KindFile   = "file"
	KindFolder = "folder"
)

// The complete conflict-policy vocabulary. Anything else on the wire is a 422.
//
//   - replace: trash the colliding node, take its name
//   - rename:  take the next free "name (n)" variant
//   - reuse:   return the existing folder instead of creating one (folders
//     only; it is what makes folder-tree creation idempotent)
const (
	PolicyReplace = "replace"
	PolicyRename  = "rename"
	PolicyReuse   = "reuse"
)

var (
	// ErrNotFound covers every miss: no such node, someone else's node, a
	// trashed node, a destination that is not a folder. They are deliberately
	// indistinguishable to the caller.
	ErrNotFound = errors.New("node not found")
	// ErrNameConflict is a live sibling with the same name and no policy that
	// resolves it.
	ErrNameConflict = errors.New("a node with that name already exists here")
	// ErrCycle is a move of a folder into itself or one of its descendants.
	ErrCycle = errors.New("a folder cannot be moved into itself or its own descendant")
	// ErrUnsupported is an operation this kind of node does not support --
	// copying a folder, for one.
	ErrUnsupported = errors.New("unsupported for this node")
	// ErrInvalidName is a name that filename hygiene rejected.
	ErrInvalidName = errors.New("invalid name")
	// ErrInvalidPolicy is a conflict_policy outside the vocabulary, or one the
	// endpoint does not accept.
	ErrInvalidPolicy = errors.New("invalid conflict_policy")
	// ErrRootNode guards the per-user root folder, which cannot be renamed,
	// moved, trashed, purged or shared.
	ErrRootNode = errors.New("the root folder cannot be renamed, moved or trashed")
	// ErrInvalidSort is a sort key or direction outside the children listing's
	// fixed vocabulary.
	ErrInvalidSort = errors.New("invalid sort")
	// ErrCursorSort is a children cursor replayed under a different ordering
	// than the one that minted it, or one missing its own key's value.
	ErrCursorSort = errors.New("cursor: minted under a different sort order")
)

// Node is a row of the nodes table.
type Node struct {
	ID          uuid.UUID
	OwnerID     uuid.UUID
	ParentID    *uuid.UUID // NULL only for a user's root folder
	Kind        string
	Name        string
	BlobID      *uuid.UUID
	Size        *int64 // NULL for folders
	Mime        *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
	TrashedRoot bool
}

// IsRoot reports whether n is a user's root folder.
func (n Node) IsRoot() bool { return n.ParentID == nil }

// nodeCols is the column list every node query selects, in the order scanNode
// reads them.
const nodeCols = `id, owner_id, parent_id, kind, name, blob_id, size, mime,
	created_at, updated_at, deleted_at, trashed_root`

// scanNode reads one row selected with nodeCols.
func scanNode(row pgx.Row) (Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.OwnerID, &n.ParentID, &n.Kind, &n.Name, &n.BlobID,
		&n.Size, &n.Mime, &n.CreatedAt, &n.UpdatedAt, &n.DeletedAt, &n.TrashedRoot)
	return n, err
}

// querier is the subset of pgx both the pool and a transaction implement, so
// the same helper can run inside or outside a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is the node package's data access. One per process is plenty; it holds
// no state beyond the pool.
type Store struct {
	db *pgxpool.Pool
}

// NewStore wraps a pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// ChildCursor is the keyset position in a children listing. Its fields are
// exactly the ORDER BY expressions -- folders first, then the sort key, then
// case-folded name, then id as the tiebreaker -- which is what makes the
// ordering stable across pages even while the folder is being written to.
//
// Sort and Dir travel with the position because a position only means
// something under the ordering that produced it: replaying a size cursor under
// sort=name would compare a byte count against a name and skip or repeat rows.
// Empty means the default -- name, ascending -- so a cursor minted before
// sorting shipped still pages correctly.
//
// UpdatedAt and Size carry the sort key's value at the page boundary; only the
// one belonging to the cursor's own key is set. The name key needs no field of
// its own, because Name already is it.
type ChildCursor struct {
	KindRank  int        `json:"k"`
	Name      string     `json:"n"`
	ID        uuid.UUID  `json:"i"`
	Sort      string     `json:"s,omitempty"`
	Dir       string     `json:"d,omitempty"`
	UpdatedAt *time.Time `json:"u,omitempty"`
	Size      *int64     `json:"z,omitempty"`
}

// The cursor's Name is Postgres' own lower(name), read back from the query
// rather than lowercased in Go: the keyset comparison happens in SQL, so the
// value has to be the one the database produced or a page boundary could skip
// or repeat a row.

// sortKey and dir read the cursor's own ordering, an empty one meaning the
// order that was the only one before sorting shipped.
func (c ChildCursor) sortKey() string {
	if c.Sort == "" {
		return SortName
	}
	return c.Sort
}

func (c ChildCursor) dir() string {
	if c.Dir == "" {
		return DirAsc
	}
	return c.Dir
}

// keyValue is the boundary value the keyset comparison binds for one sort key.
// A cursor naming a key but carrying no value for it is not one we minted, so
// it is refused rather than paged from a zero value.
func (c ChildCursor) keyValue(key string) (any, error) {
	switch key {
	case SortName:
		return c.Name, nil
	case SortUpdatedAt:
		if c.UpdatedAt == nil {
			return nil, fmt.Errorf("%w: it carries no updated_at", ErrCursorSort)
		}
		return *c.UpdatedAt, nil
	case SortSize:
		if c.Size == nil {
			return nil, fmt.Errorf("%w: it carries no size", ErrCursorSort)
		}
		return *c.Size, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidSort, key)
	}
}
