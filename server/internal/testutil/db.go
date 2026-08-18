package testutil

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
)

// DB is the slice of pgx a helper here needs. *pgxpool.Pool, *pgx.Conn and a
// transaction all satisfy it.
type DB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// queryExecer is DB under its old name, kept so the time-control helpers read
// the way they did when they were written.
type queryExecer = DB

// digestSQL fingerprints everything a write could disturb for a set of owners:
// every node row in full, plus the blob rows those nodes reference, refcount
// included. Ordering is inside the aggregate, so the digest is stable.
//
// The refcount column is the reason blobs are in here at all: a copy that is
// supposed to be rejected must not have bumped one, and that leaves no trace in
// the nodes table.
const digestSQL = `
	SELECT coalesce(md5(string_agg(row_text, '|' ORDER BY row_text)), 'empty')
	  FROM (
	        SELECT concat_ws('/', 'node', n.id, coalesce(n.parent_id::text, '-'), n.kind, n.name,
	                        coalesce(n.size::text, '-'), coalesce(n.mime, '-'),
	                        coalesce(n.blob_id::text, '-'),
	                        coalesce(n.deleted_at::text, '-'), n.trashed_root::text,
	                        n.updated_at::text) AS row_text
	          FROM nodes n
	         WHERE n.owner_id = ANY($1)
	        UNION ALL
	        SELECT concat_ws('/', 'blob', b.id, b.object_key, b.size::text, b.refcount::text)
	          FROM blobs b
	         WHERE b.id IN (SELECT blob_id FROM nodes WHERE owner_id = ANY($1) AND blob_id IS NOT NULL)
	       ) rows`

// Digest returns a fingerprint of every node and blob row belonging to the
// given owners. Take it before and after a request that must be rejected: an
// unchanged digest is the proof that nothing was written, which a status code
// alone never gives.
func Digest(t testing.TB, db DB, owners ...uuid.UUID) string {
	t.Helper()
	var sum string
	if err := db.QueryRow(context.Background(), digestSQL, owners).Scan(&sum); err != nil {
		t.Fatalf("testutil: digesting owners %v: %v", owners, err)
	}
	return sum
}

// CreateFile puts real bytes in Garage and publishes them as a file node, in
// the shape the upload path will produce: one blobs row at refcount 1 and one
// nodes row pointing at it.
//
// Fixtures deliberately write their bytes with a direct PutObject rather than
// driving the upload protocol: node and share tests need files to exist without
// depending on the protocol under test, and the copy/refcount rows of the authz
// matrix need a file node to aim at.
func (h *Harness) CreateFile(t testing.TB, ownerID, parentID uuid.UUID, name string, content []byte) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	fixture, err := blob.PutFixture(ctx, h.S3, h.Cfg.S3Bucket, content)
	if err != nil {
		t.Fatalf("testutil: CreateFile %q: %v", name, err)
	}

	blobID, nodeID := uuid.New(), uuid.New()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("testutil: CreateFile %q: begin: %v", name, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit succeeded

	const insertBlob = `
		INSERT INTO blobs (id, object_key, size, sha256, etag, refcount)
		VALUES ($1, $2, $3, $4, $5, 1)`
	if _, err := tx.Exec(ctx, insertBlob, blobID, fixture.ObjectKey, fixture.Size, fixture.SHA256, fixture.ETag); err != nil {
		t.Fatalf("testutil: CreateFile %q: insert blob: %v", name, err)
	}

	const insertNode = `
		INSERT INTO nodes (id, owner_id, parent_id, kind, name, blob_id, size, mime)
		VALUES ($1, $2, $3, 'file', $4, $5, $6, 'application/octet-stream')`
	if _, err := tx.Exec(ctx, insertNode, nodeID, ownerID, parentID, name, blobID, fixture.Size); err != nil {
		t.Fatalf("testutil: CreateFile %q: insert node: %v", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("testutil: CreateFile %q: commit: %v", name, err)
	}
	return nodeID
}

// Refcount reads a file node's blob refcount, which is what a copy increments
// and a purge decrements.
func (h *Harness) Refcount(t testing.TB, nodeID uuid.UUID) int {
	t.Helper()
	const q = `SELECT b.refcount FROM blobs b JOIN nodes n ON n.blob_id = b.id WHERE n.id = $1`
	var n int
	if err := h.Pool.QueryRow(context.Background(), q, nodeID).Scan(&n); err != nil {
		t.Fatalf("testutil: refcount of node %s: %v", nodeID, err)
	}
	return n
}

// countRows is a small helper for assertions phrased as "nothing was created".
func (h *Harness) countRows(ctx context.Context, table, where string, args ...any) (int, error) {
	if !identifier.MatchString(table) {
		return 0, fmt.Errorf("testutil: countRows: %q is not a plain identifier", table)
	}
	var n int
	sql := fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s`, table, where)
	if err := h.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CountRows counts rows matching a predicate, failing the test on a SQL error.
func (h *Harness) CountRows(t testing.TB, table, where string, args ...any) int {
	t.Helper()
	n, err := h.countRows(context.Background(), table, where, args...)
	if err != nil {
		t.Fatalf("testutil: counting %s: %v", table, err)
	}
	return n
}
