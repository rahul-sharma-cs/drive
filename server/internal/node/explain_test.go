package node

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The index evidence for the children listing's sort orders.
//
// Migration 0003 adds two indexes whose expressions have to match
// childrenQuery's character for character, and the ascending/descending
// asymmetry ("rank ASC, key DESC" is neither a btree's forward scan nor its
// backward one) is a claim that only a plan can settle. This seeds a folder
// with 5 000 children and prints the plan for the second page -- the one that
// actually exercises the keyset -- in every direction.
//
// It is skipped by default: 5 000 rows in the shared drive-test database is not
// something two gate runs an hour should keep doing. Run it deliberately:
//
//	set -a; . ./.env.test; set +a
//	DRIVE_EXPLAIN=1 go test -count=1 -v -run TestExplainChildrenSortPlans ./server/internal/node/
func TestExplainChildrenSortPlans(t *testing.T) {
	if os.Getenv("DRIVE_EXPLAIN") == "" {
		t.Skip("set DRIVE_EXPLAIN=1 to seed 5 000 children and print the sort plans")
	}

	f := newFixture(t)
	parent := f.folder(f.root, "Big")
	seedChildren(f, parent, 5000)

	// Without stats the planner is guessing at a table this suite has been
	// filling all along.
	if _, err := f.pool.Exec(f.ctx, `ANALYZE nodes`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	for _, key := range []string{SortName, SortUpdatedAt, SortSize} {
		for _, dir := range []string{DirAsc, DirDesc} {
			sort := ChildSort{Key: key, Dir: dir}

			// Page 1 gives a real keyset boundary to plan page 2 from.
			items, next, err := f.store.Children(f.ctx, f.owner, parent, sort, nil, 50)
			if err != nil {
				t.Fatalf("%s %s: first page: %v", key, dir, err)
			}
			if next == nil {
				t.Fatalf("%s %s: no cursor after 50 of 5 000 rows", key, dir)
			}
			if len(items) != 50 {
				t.Fatalf("%s %s: first page = %d rows, want 50", key, dir, len(items))
			}
			boundary, err := next.keyValue(key)
			if err != nil {
				t.Fatalf("%s %s: cursor: %v", key, dir, err)
			}

			rows, err := f.pool.Query(f.ctx, `EXPLAIN (ANALYZE, BUFFERS) `+childrenQuery(sort),
				parent, next.KindRank, next.Name, next.ID, boundary, 51)
			if err != nil {
				t.Fatalf("%s %s: EXPLAIN: %v", key, dir, err)
			}
			var plan []string
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					rows.Close()
					t.Fatalf("%s %s: reading the plan: %v", key, dir, err)
				}
				plan = append(plan, line)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatalf("%s %s: reading the plan: %v", key, dir, err)
			}
			t.Logf("sort=%s dir=%s, second page of 5 000:\n%s", key, dir, strings.Join(plan, "\n"))
		}
	}
}

// The index evidence for the trash.
//
// ListTrash and TrashRootIDs share one predicate and order by (deleted_at, id)
// in opposite directions; migration 0004 claims one btree serves both. Until it
// landed, only nodes_owner_id_idx helped, so emptying a trash re-read every
// node the owner had ever trashed and top-N-sorted it once per 200-root page.
//
// Both plans are printed twice: once as they run, and once inside a transaction
// that drops the index and rolls back, which is the "before" the migration
// changed. Seeding 2 000 roots in the shared drive-test database is not
// something a gate run should keep doing, so it is opt-in:
//
//	set -a; . ./.env.test; set +a
//	DRIVE_EXPLAIN=1 go test -count=1 -v -run TestExplainTrashPlans ./server/internal/node/
func TestExplainTrashPlans(t *testing.T) {
	if os.Getenv("DRIVE_EXPLAIN") == "" {
		t.Skip("set DRIVE_EXPLAIN=1 to seed a 2 000-root trash and print the plans")
	}

	f := newFixture(t)
	seedTrashRoots(f, 2000)

	if _, err := f.pool.Exec(f.ctx, `ANALYZE nodes`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	// A real cursor, so the listing plan is the second page's -- the one that
	// exercises the keyset rather than a bare first page.
	_, next, err := f.store.ListTrash(f.ctx, f.owner, nil, 200)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if next == nil {
		t.Fatal("no cursor after 200 of 2 000 trashed roots")
	}

	queries := []struct {
		what string
		sql  string
		args []any
	}{
		{"ListTrash, second page of 200", trashListQuery(true),
			[]any{f.owner, next.DeletedAt, next.ID, 201}},
		{"TrashRootIDs, one 200-root page of emptying", trashRootIDsQuery,
			[]any{f.owner, 200}},
	}

	for _, q := range queries {
		t.Logf("=== %s, with nodes_trash_roots_idx ===\n%s",
			q.what, strings.Join(explain(f, q.sql, q.args, false), "\n"))
		t.Logf("=== %s, without it (index dropped inside a rolled-back tx) ===\n%s",
			q.what, strings.Join(explain(f, q.sql, q.args, true), "\n"))
	}
}

// explain runs EXPLAIN (ANALYZE, BUFFERS) over one query, optionally with the
// trash index dropped for the duration of a transaction that is then rolled
// back -- so the "before" plan costs the shared database nothing permanent.
func explain(f *fixture, sql string, args []any, withoutIndex bool) []string {
	f.t.Helper()

	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatalf("EXPLAIN: begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck // the rollback is the point

	if withoutIndex {
		if _, err := tx.Exec(f.ctx, `DROP INDEX nodes_trash_roots_idx`); err != nil {
			f.t.Fatalf("EXPLAIN: dropping the trash index: %v", err)
		}
	}

	rows, err := tx.Query(f.ctx, `EXPLAIN (ANALYZE, BUFFERS) `+sql, args...)
	if err != nil {
		f.t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			f.t.Fatalf("EXPLAIN: reading the plan: %v", err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("EXPLAIN: reading the plan: %v", err)
	}
	return plan
}

// seedTrashRoots fills the owner's trash with n roots, each its own trashed
// root at its own timestamp, and removes them when the test ends.
func seedTrashRoots(f *fixture, n int) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO nodes (id, owner_id, parent_id, kind, name, deleted_at, trashed_root)
		SELECT gen_random_uuid(), $1, $2, 'file',
		       'trashed-' || lpad(i::text, 5, '0') || '.bin',
		       now() - make_interval(secs => i), true
		  FROM generate_series(1, $3) AS i`, f.owner, f.root, n); err != nil {
		f.t.Fatalf("seeding %d trashed roots: %v", n, err)
	}
	f.t.Cleanup(func() {
		if _, err := f.pool.Exec(f.ctx,
			`DELETE FROM nodes WHERE owner_id = $1 AND trashed_root`, f.owner); err != nil {
			f.t.Errorf("cleaning up the seeded trash: %v", err)
		}
	})
}

// The trash's partial index only applies while its predicate is written the way
// the two queries write it: the planner has to prove the query implies it.
// Nothing fails loudly when they drift -- the plan just starts sorting the
// owner's whole trash again -- so the texts are compared here. No database
// needed.
func TestTrashPredicateMatchesTheMigration(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/0004_upload_key_and_trash_index.sql")
	if err != nil {
		t.Fatalf("reading the migration: %v", err)
	}
	migration := string(raw)

	// The predicate without its owner parameter: the index is not per-owner in
	// its WHERE, it carries owner_id as the leading column instead.
	const partial = `WHERE trashed_root AND deleted_at IS NOT NULL`
	if !strings.Contains(migration, partial) {
		t.Errorf("migration 0004 does not restrict the trash index to %q", partial)
	}
	if !strings.Contains(migration, `(owner_id, deleted_at, id)`) {
		t.Error("migration 0004's trash index is not on (owner_id, deleted_at, id)")
	}

	// Both readers, against the one predicate.
	for _, q := range []string{trashListQuery(false), trashListQuery(true), trashRootIDsQuery} {
		if !strings.Contains(q, trashPredicate) {
			t.Errorf("a trash query no longer filters on %q:\n%s", trashPredicate, q)
		}
	}
	if want := strings.TrimPrefix(trashPredicate, `owner_id = $1 AND `); !strings.Contains(migration, want) {
		t.Errorf("the index predicate %q is not the queries' own", want)
	}

	// Ascending for emptying, descending for the listing -- one btree, read
	// both ways. An index that carried its own ASC/DESC would serve one of
	// them and silently sort for the other.
	if !strings.Contains(trashRootIDsQuery, `ORDER BY deleted_at, id`) {
		t.Error("emptying no longer pages oldest-deletion-first")
	}
	if !strings.Contains(trashListQuery(true), `ORDER BY deleted_at DESC, id DESC`) {
		t.Error("the listing no longer pages most-recent-first")
	}
	if strings.Contains(migration, `deleted_at DESC`) {
		t.Error("migration 0004's index pins a direction; a btree serves both without one")
	}
}

// An index on an expression only serves a query that writes the expression the
// same way. Nothing fails loudly when they drift -- the plan just quietly
// starts sorting 5 000 rows -- so the two texts are compared here, where a
// rewrite of either one is caught the moment it happens. No database needed.
//
// The comparison is against the whole rendered column list, not against the
// sort key alone. "updated_at" as a substring proves nothing: it is in nodeCols,
// in the query's SELECT, and in the index's own *name*, so an index rebuilt on
// created_at would pass a contains-check while serving nothing.
func TestSortExpressionsMatchTheMigration(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/0003_children_sort_indexes.sql")
	if err != nil {
		t.Fatalf("reading the migration: %v", err)
	}
	migration := string(raw)

	if !strings.Contains(migration, rankExpr) {
		t.Errorf("the migration does not contain the folders-first expression %q", rankExpr)
	}
	// The listing pages over live rows only, so an unpartial index would be a
	// second copy of the table rather than the one the keyset walks.
	if !strings.Contains(migration, `WHERE deleted_at IS NULL`) {
		t.Error("migration 0003's indexes are not restricted to live rows")
	}
	for _, key := range []string{SortUpdatedAt, SortSize} {
		spec := sortSpecs[key]

		// The keyset's own columns, in the keyset's own order.
		columns := `(parent_id, ` + rankExpr + `, ` + spec.key + `, lower(name), id)`
		if !strings.Contains(migration, columns) {
			t.Errorf("sort=%s pages over %s, which is not an index in migration 0003", key, columns)
		}
		for _, dir := range []string{DirAsc, DirDesc} {
			q := childrenQuery(ChildSort{Key: key, Dir: dir})
			// Folders first, then this key -- adjacent, so the index prefix is
			// the ordering rather than merely appearing somewhere in it.
			if want := `ORDER BY ` + rankExpr + ` ASC, ` + spec.key + ` `; !strings.Contains(q, want) {
				t.Errorf("sort=%s dir=%s does not order by %q", key, dir, want)
			}
		}
	}

	// The name key rides the index migration 0001 already created.
	if got := sortSpecs[SortName].key; got != `lower(name)` {
		t.Errorf("the name key is %q; nodes_parent_name_idx indexes lower(name)", got)
	}
}

// seedChildren fills a folder with n children -- one in ten a folder, the rest
// files with spread-out sizes and update times -- in one statement, and removes
// them when the test ends so the shared database does not carry them.
func seedChildren(f *fixture, parent uuid.UUID, n int) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO nodes (id, owner_id, parent_id, kind, name, size, mime, updated_at)
		SELECT gen_random_uuid(), $1, $2,
		       CASE WHEN i % 10 = 0 THEN 'folder' ELSE 'file' END,
		       'child-' || lpad(i::text, 5, '0') || CASE WHEN i % 10 = 0 THEN '' ELSE '.bin' END,
		       CASE WHEN i % 10 = 0 THEN NULL ELSE (i * 7919) % 1000000 END,
		       CASE WHEN i % 10 = 0 THEN NULL ELSE 'application/octet-stream' END,
		       now() - make_interval(secs => (i * 37) % 100000)
		  FROM generate_series(1, $3) AS i`, f.owner, parent, n); err != nil {
		f.t.Fatalf("seeding %d children: %v", n, err)
	}
	f.t.Cleanup(func() {
		if _, err := f.pool.Exec(f.ctx, `DELETE FROM nodes WHERE parent_id = $1`, parent); err != nil {
			f.t.Errorf("cleaning up the seeded children: %v", err)
		}
	})
}
