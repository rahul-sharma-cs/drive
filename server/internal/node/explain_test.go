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
