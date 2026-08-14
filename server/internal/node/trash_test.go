package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/db"
)

// These are DB-backed on purpose. The trash model is a set of recursive CTEs
// and a partial unique index; a fake would test the fake.

// ---------------------------------------------------------------- harness ----

// trashTestDSN is the drive-test stack's Postgres, verbatim from the committed
// .env.test. Tests never touch the dev stack on :55432.
const trashTestDSN = "postgres://drive:drive@localhost:55433/drive?sslmode=disable"

// nodeMigrateLock serializes goose against the other packages' suites, which
// run as separate binaries against this same database.
const nodeMigrateLock = int64(0x64726976)

var (
	nodePoolOnce sync.Once
	nodePool     *pgxpool.Pool
	nodePoolErr  error
)

func trashTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	nodePoolOnce.Do(func() {
		dsn := os.Getenv("DRIVE_DB_DSN")
		if dsn == "" {
			dsn = trashTestDSN
		}
		if strings.Contains(dsn, ":55432") {
			nodePoolErr = fmt.Errorf("DRIVE_DB_DSN points at the dev stack (%s); tests run against drive-test on :55433", dsn)
			return
		}
		ctx := context.Background()
		if nodePool, nodePoolErr = db.Connect(ctx, dsn); nodePoolErr != nil {
			return
		}
		conn, err := nodePool.Acquire(ctx)
		if err != nil {
			nodePoolErr = err
			return
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, nodeMigrateLock); err != nil {
			nodePoolErr = err
			return
		}
		nodePoolErr = db.Migrate(ctx, nodePool)
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, nodeMigrateLock)
	})
	if nodePoolErr != nil {
		t.Fatalf("drive-test database: %v (is `docker compose -p drive-test --env-file .env.test up -d` running?)", nodePoolErr)
	}
	return nodePool
}

// fixture is one throwaway user with a root folder, plus the builders every
// case needs. Isolation between cases is by owner: every query in the node
// package is owner-scoped, so a fresh user is a fresh universe.
type fixture struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	store *Store
	owner uuid.UUID
	root  uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := trashTestPool(t)
	f := &fixture{t: t, ctx: context.Background(), pool: pool, store: NewStore(pool)}

	f.owner = uuid.New()
	f.root = uuid.New()
	email := "node-" + uuid.NewString() + "@drive.test"
	if _, err := pool.Exec(f.ctx,
		`INSERT INTO users (id, email, password_hash, display_name, email_verified_at)
		 VALUES ($1, $2, 'x', 'Test User', now())`, f.owner, email); err != nil {
		t.Fatalf("seeding user: %v", err)
	}
	if _, err := pool.Exec(f.ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		 VALUES ($1, $2, NULL, 'folder', 'My Drive')`, f.root, f.owner); err != nil {
		t.Fatalf("seeding root folder: %v", err)
	}
	return f
}

func (f *fixture) folder(parent uuid.UUID, name string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		 VALUES ($1, $2, $3, 'folder', $4)`, id, f.owner, parent, name); err != nil {
		f.t.Fatalf("seeding folder %q: %v", name, err)
	}
	return id
}

func (f *fixture) file(parent uuid.UUID, name string, size int64) uuid.UUID {
	return f.fileWithBlob(parent, name, size, nil)
}

func (f *fixture) fileWithBlob(parent uuid.UUID, name string, size int64, blob *uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name, blob_id, size, mime)
		 VALUES ($1, $2, $3, 'file', $4, $5, $6, 'application/octet-stream')`,
		id, f.owner, parent, name, blob, size); err != nil {
		f.t.Fatalf("seeding file %q: %v", name, err)
	}
	return id
}

// blob inserts one blobs row at the given refcount, standing in for an object
// already in Garage.
func (f *fixture) blob(refcount int) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO blobs (id, object_key, size, refcount) VALUES ($1, $2, 1024, $3)`,
		id, "blobs/"+id.String(), refcount); err != nil {
		f.t.Fatalf("seeding blob: %v", err)
	}
	return id
}

func (f *fixture) get(id uuid.UUID) Node {
	f.t.Helper()
	n, err := scanNode(f.pool.QueryRow(f.ctx,
		`SELECT `+nodeCols+` FROM nodes WHERE id = $1`, id))
	if err != nil {
		f.t.Fatalf("reading node %s: %v", id, err)
	}
	return n
}

func (f *fixture) exists(id uuid.UUID) bool {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM nodes WHERE id = $1`, id).Scan(&n); err != nil {
		f.t.Fatalf("counting node %s: %v", id, err)
	}
	return n == 1
}

// backdateTrash pushes every trashed row of this user an hour into the past, so
// a following trash operation lands on a provably later timestamp. This is the
// plan's time-control convention: state lives in Postgres, tests move it with
// SQL rather than sleeping.
func (f *fixture) backdateTrash() {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE nodes SET deleted_at = deleted_at - interval '1 hour'
		  WHERE owner_id = $1 AND deleted_at IS NOT NULL`, f.owner); err != nil {
		f.t.Fatalf("backdating the trash: %v", err)
	}
}

func (f *fixture) trashedRootCount() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM nodes WHERE owner_id = $1 AND trashed_root`, f.owner).Scan(&n); err != nil {
		f.t.Fatalf("counting trash roots: %v", err)
	}
	return n
}

func (f *fixture) refcount(blob uuid.UUID) *int {
	f.t.Helper()
	var n int
	err := f.pool.QueryRow(f.ctx, `SELECT refcount FROM blobs WHERE id = $1`, blob).Scan(&n)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil
		}
		f.t.Fatalf("reading refcount: %v", err)
	}
	return &n
}

// ------------------------------------------------------------------ trash ----

// Trashing a folder takes its whole subtree with it, in one stamp, and the
// listing must show exactly one entry: the thing the user deleted.
func TestTrashStampsTheSubtreeAndMarksOneRoot(t *testing.T) {
	f := newFixture(t)
	a := f.folder(f.root, "A")
	b := f.folder(a, "B")
	deep := f.file(b, "deep.txt", 10)

	if err := f.store.Trash(f.ctx, f.owner, a); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	stamp := f.get(a).DeletedAt
	if stamp == nil {
		t.Fatal("A: deleted_at is still NULL")
	}
	for name, id := range map[string]uuid.UUID{"B": b, "deep.txt": deep} {
		got := f.get(id).DeletedAt
		if got == nil {
			t.Errorf("%s: deleted_at is still NULL", name)
			continue
		}
		if !got.Equal(*stamp) {
			t.Errorf("%s: deleted_at = %s, want the root's %s", name, got, stamp)
		}
	}

	if n := f.trashedRootCount(); n != 1 {
		t.Errorf("trashed_root rows = %d, want exactly 1", n)
	}
	if !f.get(a).TrashedRoot {
		t.Error("A is not the trashed root")
	}

	items, next, err := f.store.ListTrash(f.ctx, f.owner, nil, 50)
	if err != nil {
		t.Fatalf("ListTrash: %v", err)
	}
	if len(items) != 1 || items[0].ID != a {
		t.Errorf("ListTrash returned %d items, want just A", len(items))
	}
	if next != nil {
		t.Errorf("next cursor = %+v, want nil", next)
	}
}

// The case the timestamp design exists for: an inner subtree trashed earlier
// keeps its own timestamp, so restoring its ancestor leaves it in the trash --
// as a trash root of its own.
func TestRestoringAnAncestorLeavesAnEarlierTrashedSubtreeBehind(t *testing.T) {
	f := newFixture(t)
	a := f.folder(f.root, "A")
	sibling := f.file(a, "sibling.txt", 10)
	b := f.folder(a, "B")
	inner := f.file(b, "inner.txt", 10)

	if err := f.store.Trash(f.ctx, f.owner, b); err != nil {
		t.Fatalf("trashing B: %v", err)
	}
	f.backdateTrash() // B's subtree is now provably older than A's stamp
	if err := f.store.Trash(f.ctx, f.owner, a); err != nil {
		t.Fatalf("trashing A: %v", err)
	}

	bStamp, aStamp := f.get(b).DeletedAt, f.get(a).DeletedAt
	if bStamp == nil || aStamp == nil {
		t.Fatal("A and B should both be trashed")
	}
	if !bStamp.Before(*aStamp) {
		t.Fatalf("B's stamp %s should predate A's %s -- trashing A must not restamp it", bStamp, aStamp)
	}
	if n := f.trashedRootCount(); n != 2 {
		t.Errorf("trashed_root rows = %d, want 2 (A and B)", n)
	}

	if _, err := f.store.Restore(f.ctx, f.owner, a); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Everything stamped with A's timestamp is back.
	for name, id := range map[string]uuid.UUID{"A": a, "sibling.txt": sibling} {
		n := f.get(id)
		if n.DeletedAt != nil {
			t.Errorf("%s: still trashed after restoring A", name)
		}
		if n.TrashedRoot {
			t.Errorf("%s: trashed_root is still set after restore", name)
		}
	}

	// B's subtree stayed behind, and B is now the trash root the user sees.
	for name, id := range map[string]uuid.UUID{"B": b, "inner.txt": inner} {
		if f.get(id).DeletedAt == nil {
			t.Errorf("%s: came back with A, but it was trashed separately and earlier", name)
		}
	}
	if !f.get(b).TrashedRoot {
		t.Error("B should be its own trashed root now")
	}

	items, _, err := f.store.ListTrash(f.ctx, f.owner, nil, 50)
	if err != nil {
		t.Fatalf("ListTrash: %v", err)
	}
	if len(items) != 1 || items[0].ID != b {
		t.Errorf("trash listing = %d items, want just B", len(items))
	}
}

// Restore never blocks on a name collision.
func TestRestoreAutoRenamesIntoAConflict(t *testing.T) {
	f := newFixture(t)
	original := f.file(f.root, "report.pdf", 10)

	if err := f.store.Trash(f.ctx, f.owner, original); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	replacement := f.file(f.root, "report.pdf", 20) // the name is free again

	restored, err := f.store.Restore(f.ctx, f.owner, original)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.Name != "report (1).pdf" {
		t.Errorf("restored name = %q, want %q", restored.Name, "report (1).pdf")
	}
	if f.get(replacement).Name != "report.pdf" {
		t.Error("the newer file was renamed; restore must not touch it")
	}
	if restored.ParentID == nil || *restored.ParentID != f.root {
		t.Errorf("restored parent = %v, want the original folder %s", restored.ParentID, f.root)
	}
}

// A node whose original parent is itself in the trash lands in the user's root.
func TestRestoreFallsBackToTheRootWhenTheParentIsTrashed(t *testing.T) {
	f := newFixture(t)
	a := f.folder(f.root, "A")
	b := f.folder(a, "B")

	if err := f.store.Trash(f.ctx, f.owner, b); err != nil {
		t.Fatalf("trashing B: %v", err)
	}
	f.backdateTrash()
	if err := f.store.Trash(f.ctx, f.owner, a); err != nil {
		t.Fatalf("trashing A: %v", err)
	}

	restored, err := f.store.Restore(f.ctx, f.owner, b)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.ParentID == nil || *restored.ParentID != f.root {
		t.Errorf("restored parent = %v, want the user's root %s", restored.ParentID, f.root)
	}
	if f.get(a).DeletedAt == nil {
		t.Error("A should still be in the trash")
	}
}

// ------------------------------------------------------------------ purge ----

// Purge decrements; it never deletes a blobs row and never touches Garage.
// A row at refcount 0 is what the GC sweep later collects.
func TestPurgeDecrementsRefcountsAndLeavesTheBlobRow(t *testing.T) {
	f := newFixture(t)
	blob := f.blob(2)
	one := f.folder(f.root, "one")
	two := f.folder(f.root, "two")
	copyA := f.fileWithBlob(one, "shared.bin", 1024, &blob)
	_ = f.fileWithBlob(two, "shared.bin", 1024, &blob)

	if err := f.store.Trash(f.ctx, f.owner, copyA); err != nil {
		t.Fatalf("trashing the first copy: %v", err)
	}
	if err := f.store.Purge(f.ctx, f.owner, copyA); err != nil {
		t.Fatalf("purging the first copy: %v", err)
	}
	if f.exists(copyA) {
		t.Error("the purged node is still there")
	}
	if got := f.refcount(blob); got == nil || *got != 1 {
		t.Fatalf("refcount after purging one of two references = %v, want 1", got)
	}

	if err := f.store.Trash(f.ctx, f.owner, two); err != nil {
		t.Fatalf("trashing the second copy's folder: %v", err)
	}
	if err := f.store.Purge(f.ctx, f.owner, two); err != nil {
		t.Fatalf("purging the second copy's folder: %v", err)
	}
	got := f.refcount(blob)
	if got == nil {
		t.Fatal("the blobs row was deleted; purge only decrements -- the GC sweep deletes")
	}
	if *got != 0 {
		t.Errorf("refcount after purging the last reference = %d, want 0", *got)
	}
}

// Purge takes the whole subtree, including an inner subtree that was trashed
// separately and earlier: those rows are descendants, so they go too, and
// their blobs are decremented along with the rest.
func TestPurgeTakesAnEarlierTrashedSubtreeWithIt(t *testing.T) {
	f := newFixture(t)
	blob := f.blob(1)
	a := f.folder(f.root, "A")
	b := f.folder(a, "B")
	inner := f.fileWithBlob(b, "inner.bin", 1024, &blob)

	if err := f.store.Trash(f.ctx, f.owner, b); err != nil {
		t.Fatalf("trashing B: %v", err)
	}
	f.backdateTrash()
	if err := f.store.Trash(f.ctx, f.owner, a); err != nil {
		t.Fatalf("trashing A: %v", err)
	}

	if err := f.store.Purge(f.ctx, f.owner, a); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	for name, id := range map[string]uuid.UUID{"A": a, "B": b, "inner.bin": inner} {
		if f.exists(id) {
			t.Errorf("%s survived the purge of its ancestor", name)
		}
	}
	if got := f.refcount(blob); got == nil || *got != 0 {
		t.Errorf("refcount after purging the inner subtree = %v, want 0", got)
	}
}

// Purging a node revokes its shares, inside the purge transaction.
func TestPurgeDeletesTheNodesShares(t *testing.T) {
	f := newFixture(t)
	folder := f.folder(f.root, "shared folder")
	file := f.file(folder, "shared.txt", 10)

	shareID := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO shares (id, node_id, created_by, mode, token_hash)
		 VALUES ($1, $2, $3, 'public', $4)`,
		shareID, file, f.owner, []byte(uuid.NewString())); err != nil {
		t.Fatalf("seeding share: %v", err)
	}

	if err := f.store.Trash(f.ctx, f.owner, folder); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if err := f.store.Purge(f.ctx, f.owner, folder); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	var shares int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM shares WHERE id = $1`, shareID).Scan(&shares); err != nil {
		t.Fatalf("counting shares: %v", err)
	}
	if shares != 0 {
		t.Error("the share outlived the node it pointed at")
	}
}

// An upload still aiming at a folder that just vanished is aborted, so the GC
// sweep can clean its multipart up.
func TestPurgeAbortsUploadSessionsAimedIntoTheSubtree(t *testing.T) {
	f := newFixture(t)
	a := f.folder(f.root, "A")
	b := f.folder(a, "B")

	sessionID := uuid.New()
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO upload_sessions
		    (id, user_id, parent_id, file_name, file_size, fingerprint,
		     object_key, part_size, parts_total, status, expires_at)
		VALUES ($1, $2, $3, 'big.bin', 1048576, 'fp', $4, 10485760, 1, 'active',
		        now() + interval '7 days')`,
		sessionID, f.owner, b, "blobs/"+uuid.NewString()); err != nil {
		t.Fatalf("seeding upload session: %v", err)
	}

	if err := f.store.Trash(f.ctx, f.owner, a); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if err := f.store.Purge(f.ctx, f.owner, a); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	var status string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT status FROM upload_sessions WHERE id = $1`, sessionID).Scan(&status); err != nil {
		t.Fatalf("reading upload session: %v", err)
	}
	if status != "aborted" {
		t.Errorf("upload session status = %q, want %q", status, "aborted")
	}
}

// ------------------------------------------------------------- refusals ------

// The per-user root folder is not a deletable node.
func TestTheRootFolderCannotBeTrashedRestoredOrPurged(t *testing.T) {
	f := newFixture(t)
	for name, op := range map[string]func() error{
		"Trash":   func() error { return f.store.Trash(f.ctx, f.owner, f.root) },
		"Restore": func() error { _, err := f.store.Restore(f.ctx, f.owner, f.root); return err },
		"Purge":   func() error { return f.store.Purge(f.ctx, f.owner, f.root) },
	} {
		if err := op(); !errors.Is(err, ErrRootNode) {
			t.Errorf("%s(root) = %v, want ErrRootNode", name, err)
		}
	}
}

// Another user's node is indistinguishable from one that does not exist, and
// restore/purge only accept a node that is actually a trash root.
func TestTrashOperationsRefuseWhatTheCallerCannotSee(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	theirs := other.file(other.root, "theirs.txt", 10)
	mine := f.file(f.root, "mine.txt", 10)

	if err := f.store.Trash(f.ctx, f.owner, theirs); !errors.Is(err, ErrNotFound) {
		t.Errorf("trashing another user's node = %v, want ErrNotFound", err)
	}
	if _, err := f.store.Restore(f.ctx, f.owner, mine); !errors.Is(err, ErrNotFound) {
		t.Errorf("restoring a live node = %v, want ErrNotFound", err)
	}
	if err := f.store.Purge(f.ctx, f.owner, mine); !errors.Is(err, ErrNotFound) {
		t.Errorf("purging a live node = %v, want ErrNotFound", err)
	}

	if err := f.store.Trash(f.ctx, f.owner, mine); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if err := f.store.Trash(f.ctx, f.owner, mine); !errors.Is(err, ErrNotFound) {
		t.Errorf("trashing an already trashed node = %v, want ErrNotFound", err)
	}
}

// A descendant of a trash root is in the trash but is not itself restorable or
// purgeable: the listing never offers it, and accepting it would half-restore
// one delete.
func TestOnlyTrashRootsCanBeRestoredOrPurged(t *testing.T) {
	f := newFixture(t)
	a := f.folder(f.root, "A")
	child := f.file(a, "child.txt", 10)

	if err := f.store.Trash(f.ctx, f.owner, a); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if _, err := f.store.Restore(f.ctx, f.owner, child); !errors.Is(err, ErrNotFound) {
		t.Errorf("restoring a trashed descendant = %v, want ErrNotFound", err)
	}
	if err := f.store.Purge(f.ctx, f.owner, child); !errors.Is(err, ErrNotFound) {
		t.Errorf("purging a trashed descendant = %v, want ErrNotFound", err)
	}
}

// The listing pages on (deleted_at, id) and never shows another user's trash.
func TestListTrashPaginates(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	theirs := other.file(other.root, "theirs.txt", 10)
	if err := other.store.Trash(other.ctx, other.owner, theirs); err != nil {
		t.Fatalf("trashing the other user's file: %v", err)
	}

	var want []uuid.UUID
	for i := range 3 {
		id := f.file(f.root, fmt.Sprintf("f%d.txt", i), 10)
		if err := f.store.Trash(f.ctx, f.owner, id); err != nil {
			t.Fatalf("Trash: %v", err)
		}
		f.backdateTrash() // distinct, decreasing timestamps
		want = append(want, id)
	}

	var seen []uuid.UUID
	var cur *TrashCursor
	for range 5 {
		items, next, err := f.store.ListTrash(f.ctx, f.owner, cur, 1)
		if err != nil {
			t.Fatalf("ListTrash: %v", err)
		}
		for _, n := range items {
			seen = append(seen, n.ID)
		}
		if next == nil {
			break
		}
		cur = next
	}

	if len(seen) != len(want) {
		t.Fatalf("paged through %d entries, want %d", len(seen), len(want))
	}
	// Most recently deleted first: the last one trashed comes back first.
	for i, id := range seen {
		if id != want[len(want)-1-i] {
			t.Errorf("page order at %d = %s, want %s", i, id, want[len(want)-1-i])
		}
	}
}

// A restored node is a live node again: it can be trashed a second time, and
// the new stamp is its own.
func TestRestoreThenTrashAgain(t *testing.T) {
	f := newFixture(t)
	a := f.folder(f.root, "A")
	if err := f.store.Trash(f.ctx, f.owner, a); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	f.backdateTrash()
	first := *f.get(a).DeletedAt

	if _, err := f.store.Restore(f.ctx, f.owner, a); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := f.store.Trash(f.ctx, f.owner, a); err != nil {
		t.Fatalf("second Trash: %v", err)
	}
	second := f.get(a).DeletedAt
	if second == nil || !second.After(first) {
		t.Errorf("second stamp = %v, want later than %v", second, first)
	}
	if got := f.get(a).UpdatedAt; got.Before(time.Now().Add(-time.Minute)) {
		t.Errorf("updated_at = %s, want it moved with the trash operation", got)
	}
}
