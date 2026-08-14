package seed

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/db"
)

// TestHashPassword_Shape is a pure check of the PHC-format Argon2id output —
// the parameters PLAN §Fixed choices pins, and the same string shape
// internal/auth's verifier will need to parse once it exists.
func TestHashPassword_Shape(t *testing.T) {
	hash, err := hashPassword(Password)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	const wantPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"
	if !strings.HasPrefix(hash, wantPrefix) {
		t.Errorf("hash = %q, want prefix %q", hash, wantPrefix)
	}
	parts := strings.Split(hash, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, tag
	if len(parts) != 6 {
		t.Fatalf("hash has %d $-separated parts, want 6: %q", len(parts), hash)
	}
	if parts[4] == "" || parts[5] == "" {
		t.Errorf("empty salt or tag segment in %q", hash)
	}

	hash2, err := hashPassword(Password)
	if err != nil {
		t.Fatalf("hashPassword (2nd call): %v", err)
	}
	if hash == hash2 {
		t.Error("two hashes of the same password are identical — salt is not being randomized")
	}
}

// TestFixtureBytes checks the deterministic-content helper directly: same
// inputs, same bytes, and the requested length exactly.
func TestFixtureBytes(t *testing.T) {
	a := fixtureBytes("photo1.jpg", 1000)
	b := fixtureBytes("photo1.jpg", 1000)
	if len(a) != 1000 {
		t.Fatalf("len = %d, want 1000", len(a))
	}
	if string(a) != string(b) {
		t.Error("fixtureBytes is not deterministic for identical inputs")
	}
	other := fixtureBytes("readme.md", 1000)
	if string(a) == string(other) {
		t.Error("different names produced identical content")
	}
}

// TestRun_IdempotentAndTreeShape is the real, end-to-end verification: it
// drops and recreates the test stack's schema, runs Run twice, and checks
// that (a) the second run is a genuine no-op (same row counts, no error) and
// (b) the seeded tree matches PLAN §Seed's shape — two verified users, one
// root each, and rahul's 3-level / ~10-file tree with real objects behind
// every file.
func TestRun_IdempotentAndTreeShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := testPool(t, ctx)
	s3c := testS3(t, ctx)
	bucket := envOr("DRIVE_S3_BUCKET", "drive-blobs")
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	resetSchema(t, ctx, pool)

	if err := Run(ctx, pool, s3c, bucket, log); err != nil {
		t.Fatalf("Run (1st): %v", err)
	}

	var userCount, nodeCount, blobCount int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&userCount))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM nodes`).Scan(&nodeCount))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM blobs`).Scan(&blobCount))

	if userCount != 2 {
		t.Errorf("users = %d, want 2", userCount)
	}
	// 2 roots + 4 folders (Documents, Reports, Photos, Trip) + 11 files = 17.
	if nodeCount != 17 {
		t.Errorf("nodes = %d, want 17", nodeCount)
	}
	if blobCount != 11 {
		t.Errorf("blobs = %d, want 11", blobCount)
	}

	var verifiedCount int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email_verified_at IS NOT NULL`).Scan(&verifiedCount))
	if verifiedCount != 2 {
		t.Errorf("verified users = %d, want 2 (both seeded accounts)", verifiedCount)
	}

	var rootCount int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM nodes WHERE parent_id IS NULL AND kind = 'folder'`).Scan(&rootCount))
	if rootCount != 2 {
		t.Errorf("root folders = %d, want 2", rootCount)
	}

	// Second run: idempotent, no-op.
	if err := Run(ctx, pool, s3c, bucket, log); err != nil {
		t.Fatalf("Run (2nd, should be a no-op): %v", err)
	}
	var userCount2, nodeCount2, blobCount2 int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&userCount2))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM nodes`).Scan(&nodeCount2))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM blobs`).Scan(&blobCount2))
	if userCount2 != userCount || nodeCount2 != nodeCount || blobCount2 != blobCount {
		t.Errorf("2nd Run changed row counts: users %d->%d, nodes %d->%d, blobs %d->%d",
			userCount, userCount2, nodeCount, nodeCount2, blobCount, blobCount2)
	}
}

func resetSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

func testPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	dsn := envOr("DRIVE_DB_DSN", "postgres://drive:drive@localhost:55433/drive?sslmode=disable")
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("db.Connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testS3(t *testing.T, ctx context.Context) *s3.Client {
	t.Helper()
	cfg := &config.Config{
		S3Endpoint:  envOr("DRIVE_S3_ENDPOINT", "http://localhost:3910"),
		S3Bucket:    envOr("DRIVE_S3_BUCKET", "drive-blobs"),
		S3AccessKey: envOr("DRIVE_S3_ACCESS_KEY", "drivetestkey0001"),
		S3SecretKey: envOr("DRIVE_S3_SECRET_KEY", "drivetestsecretkey0001"),
	}
	client, _, err := blob.New(ctx, cfg)
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	return client
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
}
