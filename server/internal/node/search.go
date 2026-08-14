package node

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Filename search: a trigram-accelerated ILIKE over the caller's own live
// nodes, plus AND-combined filters. Every bound is inclusive -- after/before on
// updated_at, min_size/max_size in bytes -- and a size filter restricts the
// result to files, because folder rows carry size NULL by design.

// SearchQuery is the parsed /api/search query string. A zero value matches
// every live node the caller owns.
type SearchQuery struct {
	// Q is the substring matched case-insensitively against name. Empty means
	// no name predicate at all, so the filters stand on their own.
	Q string
	// Kind is KindFile, KindFolder, or "" for both.
	Kind string
	// After and Before bound updated_at inclusively.
	After  *time.Time
	Before *time.Time
	// MinSize and MaxSize bound size inclusively, in bytes. Either one present
	// restricts the result to files.
	MinSize *int64
	MaxSize *int64
}

// SearchCursor is the keyset search paginates on.
type SearchCursor struct {
	UpdatedAt time.Time `json:"u"`
	ID        uuid.UUID `json:"i"`
}

// escapeLike neutralizes LIKE's wildcards in text that came from a user, so a
// query of "100%" searches for a literal per cent sign. Every query using it
// declares ESCAPE '\'. (Shared with name.go's likePattern.)
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Search returns the caller's live nodes matching q, most recently updated
// first. Nothing trashed and nothing belonging to another user can appear:
// both are predicates in the query, not filters applied afterwards.
func (s *Store) Search(ctx context.Context, ownerID uuid.UUID, q SearchQuery, cur *SearchCursor, limit int) ([]Node, *SearchCursor, error) {
	args := []any{ownerID}
	where := []string{`owner_id = $1`, `deleted_at IS NULL`}

	add := func(format string, arg any) {
		args = append(args, arg)
		where = append(where, fmt.Sprintf(format, len(args)))
	}

	if q.Q != "" {
		// The GIN trigram index on name serves this; escapeLike keeps a query
		// of "100%" a search for a literal per cent sign.
		add(`name ILIKE $%d ESCAPE '\'`, "%"+escapeLike(q.Q)+"%")
	}

	// A size filter implies files: a folder has no size to compare against.
	kind := q.Kind
	if q.MinSize != nil || q.MaxSize != nil {
		kind = KindFile
	}
	if kind != "" {
		add(`kind = $%d`, kind)
	}

	if q.After != nil {
		add(`updated_at >= $%d::timestamptz`, *q.After)
	}
	if q.Before != nil {
		add(`updated_at <= $%d::timestamptz`, *q.Before)
	}
	if q.MinSize != nil {
		add(`size >= $%d`, *q.MinSize)
	}
	if q.MaxSize != nil {
		add(`size <= $%d`, *q.MaxSize)
	}

	if cur != nil {
		args = append(args, cur.UpdatedAt, cur.ID)
		where = append(where, fmt.Sprintf(`(updated_at, id) < ($%d::timestamptz, $%d::uuid)`, len(args)-1, len(args)))
	}

	args = append(args, limit+1)
	sql := `SELECT ` + nodeCols + ` FROM nodes WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(` ORDER BY updated_at DESC, id DESC LIMIT $%d`, len(args))

	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("searching: %w", err)
	}
	defer rows.Close()

	var items []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("searching: %w", err)
		}
		items = append(items, n)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("searching: %w", err)
	}

	if len(items) <= limit {
		return items, nil, nil
	}
	items = items[:limit]
	last := items[len(items)-1]
	return items, &SearchCursor{UpdatedAt: last.UpdatedAt, ID: last.ID}, nil
}
