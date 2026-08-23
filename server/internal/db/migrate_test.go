package db

// Migration 0006's indexes, exercised rather than inspected: the partial
// unique one IS the "one active link per file" rule, and a create that leans
// on ON CONFLICT against it is exactly as right as the index is.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDSN is the drive-test stack's Postgres, verbatim from the committed
// .env.test. Tests never touch the dev stack on :55432, and refuse to run if
// something points them at it.
const testDSN = "postgres://drive:drive@localhost:55433/drive?sslmode=disable"

var (
	poolOnce sync.Once
	poolConn *pgxpool.Pool
	poolErr  error
)

// testPool returns the shared connection to the drive-test database, migrated
// to the latest version once.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		dsn := os.Getenv("DRIVE_DB_DSN")
		if dsn == "" {
			dsn = testDSN
		}
		if strings.Contains(dsn, ":55432") {
			poolErr = fmt.Errorf("DRIVE_DB_DSN points at the dev stack (%s); tests run against the drive-test stack on :55433", dsn)
			return
		}
		ctx := context.Background()
		if poolConn, poolErr = Connect(ctx, dsn); poolErr != nil {
			return
		}
		poolErr = Migrate(ctx, poolConn)
	})
	if poolErr != nil {
		t.Fatalf("drive-test database: %v (is `docker compose -p drive-test --env-file .env.test up -d` running?)", poolErr)
	}
	return poolConn
}

// A node has at most one share whose revoked_at is NULL, and exactly that: a
// second active share is refused by shares_node_active_idx, and a revoked one
// leaves the slot free for its replacement. A plain unique index on node_id
// would pass the first half and fail the second -- revoked rows are kept so the
// access log stays attributable.
func TestOneActiveSharePerNode(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	userID, rootID, nodeID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash, display_name, email_verified_at)
		 VALUES ($1, $2, 'not-a-hash', 'Share Owner', now())`,
		userID, "share-index-"+uuid.NewString()+"@drive.test"); err != nil {
		t.Fatalf("inserting the user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		 VALUES ($1, $2, NULL, 'folder', 'My Drive')`, rootID, userID); err != nil {
		t.Fatalf("inserting the root folder: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name, size, mime)
		 VALUES ($1, $2, $3, 'file', 'shared.png', 3, 'image/png')`, nodeID, userID, rootID); err != nil {
		t.Fatalf("inserting the file: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM shares WHERE created_by = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM nodes WHERE owner_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	// A raw insert, not a store call: the index is what is under test, and
	// the store that will write through it does not exist yet.
	insert := func() error {
		sum := sha256.Sum256([]byte(uuid.NewString()))
		_, err := pool.Exec(ctx,
			`INSERT INTO shares (id, node_id, created_by, mode, token_hash)
			 VALUES ($1, $2, $3, 'public', $4)`, uuid.New(), nodeID, userID, sum[:])
		return err
	}

	if err := insert(); err != nil {
		t.Fatalf("the first share: %v", err)
	}

	err := insert()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("a second active share for the node = %v, want a unique violation", err)
	}
	if pgErr.ConstraintName != "shares_node_active_idx" {
		t.Errorf("the violation names %q, want shares_node_active_idx", pgErr.ConstraintName)
	}

	if _, err := pool.Exec(ctx, `UPDATE shares SET revoked_at = now() WHERE node_id = $1`, nodeID); err != nil {
		t.Fatalf("revoking the first share: %v", err)
	}
	if err := insert(); err != nil {
		t.Fatalf("a share after the first was revoked: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM shares WHERE node_id = $1`, nodeID).Scan(&n); err != nil {
		t.Fatalf("counting the node's shares: %v", err)
	}
	if n != 2 {
		t.Errorf("the node has %d shares, want 2 -- the revoked one must still be there", n)
	}
}

// The owner's listing index carries the listing's own order and predicate.
// The planner matches a partial index on its text, so the predicate is the
// thing to pin: an index without it would exist and never be used.
func TestOwnerActiveSharesIndexHasTheListingPredicate(t *testing.T) {
	pool := testPool(t)

	var def string
	err := pool.QueryRow(context.Background(),
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'shares_owner_active_idx'`).Scan(&def)
	if err != nil {
		t.Fatalf("reading shares_owner_active_idx: %v", err)
	}
	for _, want := range []string{"(created_by, created_at DESC, id DESC)", "WHERE (revoked_at IS NULL)"} {
		if !strings.Contains(def, want) {
			t.Errorf("index definition %q lacks %q", def, want)
		}
	}
}
