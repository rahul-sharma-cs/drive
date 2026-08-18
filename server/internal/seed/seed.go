// Package seed populates Drive with the two demo users and rahul@drive.local's
// sample folder tree. Run is idempotent: it is a
// no-op the moment any user row exists, so `make seed` and every e2e run that
// calls it are safe to repeat.
package seed

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/blob"
)

// Password is the shared password for both seeded accounts. It is a throwaway
// local-development constant, not a credential -- the seed only ever runs
// against a disposable stack.
const Password = "drive-demo-1"

func Run(ctx context.Context, pool *pgxpool.Pool, s3c *s3.Client, bucket string, log *slog.Logger) error {
	exists, err := usersExist(ctx, pool)
	if err != nil {
		return err
	}
	if exists {
		log.Info("seed: users already exist, nothing to do")
		return nil
	}

	rahul, err := createUser(ctx, pool, "rahul@drive.local", "Rahul")
	if err != nil {
		return err
	}
	log.Info("seed: created user", "email", rahul.email, "id", rahul.id)

	demo, err := createUser(ctx, pool, "demo@drive.local", "Demo")
	if err != nil {
		return err
	}
	log.Info("seed: created user", "email", demo.email, "id", demo.id)

	n, err := seedRahulTree(ctx, pool, s3c, bucket, rahul.id, rahul.rootID)
	if err != nil {
		return err
	}
	log.Info("seed: seeded rahul's tree", "files", n)

	return nil
}

func usersExist(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return false, fmt.Errorf("seed: count users: %w", err)
	}
	return n > 0, nil
}

type user struct {
	id     uuid.UUID
	rootID uuid.UUID
	email  string
}

// createUser inserts one verified user plus its root folder ("My Drive", the
// only allowed name for a parent_id-NULL node) in a single transaction -- the
// same atomicity real signup uses, because a user without a root folder has
// nowhere to put anything.
func createUser(ctx context.Context, pool *pgxpool.Pool, email, displayName string) (user, error) {
	hash, err := auth.HashPassword(Password)
	if err != nil {
		return user{}, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return user{}, fmt.Errorf("seed: begin tx for %s: %w", email, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit succeeds

	u := user{id: uuid.New(), rootID: uuid.New(), email: email}

	const insertUser = `
		INSERT INTO users (id, email, password_hash, display_name, email_verified_at)
		VALUES ($1, $2, $3, $4, now())`
	if _, err := tx.Exec(ctx, insertUser, u.id, email, hash, displayName); err != nil {
		return user{}, fmt.Errorf("seed: insert user %s: %w", email, err)
	}

	const insertRoot = `
		INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		VALUES ($1, $2, NULL, 'folder', 'My Drive')`
	if _, err := tx.Exec(ctx, insertRoot, u.rootID, u.id); err != nil {
		return user{}, fmt.Errorf("seed: insert root folder for %s: %w", email, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return user{}, fmt.Errorf("seed: commit user %s: %w", email, err)
	}
	return u, nil
}

func createFolder(ctx context.Context, pool *pgxpool.Pool, ownerID, parentID uuid.UUID, name string) (uuid.UUID, error) {
	id := uuid.New()
	const q = `INSERT INTO nodes (id, owner_id, parent_id, kind, name) VALUES ($1, $2, $3, 'folder', $4)`
	if _, err := pool.Exec(ctx, q, id, ownerID, parentID, name); err != nil {
		return uuid.Nil, fmt.Errorf("seed: create folder %q: %w", name, err)
	}
	return id, nil
}

// fileSpec describes one seeded file: content is generated to match size, and
// updatedAgo backdates created_at/updated_at from "now" so the search
// boundary tests (?after=/?before=/?min_size=/?max_size=) have real spread to
// test against.
type fileSpec struct {
	name       string
	mime       string
	size       int
	updatedAgo time.Duration
}

func createFile(ctx context.Context, pool *pgxpool.Pool, s3c *s3.Client, bucket string, ownerID, parentID uuid.UUID, spec fileSpec) error {
	data := fixtureBytes(spec.name, spec.size)

	fx, err := blob.PutFixture(ctx, s3c, bucket, data)
	if err != nil {
		return fmt.Errorf("seed: put fixture for %q: %w", spec.name, err)
	}

	blobID := uuid.New()
	const insertBlob = `INSERT INTO blobs (id, object_key, size, sha256, etag) VALUES ($1, $2, $3, $4, $5)`
	if _, err := pool.Exec(ctx, insertBlob, blobID, fx.ObjectKey, fx.Size, fx.SHA256, fx.ETag); err != nil {
		return fmt.Errorf("seed: insert blob for %q: %w", spec.name, err)
	}

	ts := time.Now().Add(-spec.updatedAgo)
	nodeID := uuid.New()
	const insertNode = `
		INSERT INTO nodes (id, owner_id, parent_id, kind, name, blob_id, size, mime, created_at, updated_at)
		VALUES ($1, $2, $3, 'file', $4, $5, $6, $7, $8, $8)`
	if _, err := pool.Exec(ctx, insertNode, nodeID, ownerID, parentID, spec.name, blobID, fx.Size, spec.mime, ts); err != nil {
		return fmt.Errorf("seed: insert node %q: %w", spec.name, err)
	}
	return nil
}

// fixtureBytes returns size real bytes, deterministic but not all-zero (a
// zero-filled buffer compresses away and proves nothing about a real object).
func fixtureBytes(name string, size int) []byte {
	data := make([]byte, size)
	if len(name) == 0 {
		name = "x"
	}
	for i := range data {
		data[i] = name[i%len(name)]
	}
	return data
}

const day = 24 * time.Hour

// seedRahulTree builds a 3-level folder tree with ~10 small files under rahul's
// root -- enough depth and breadth for the browser, search-filter and trash
// tests to have something real to work against:
//
//	My Drive/
//	  readme.md, notes.txt, budget.xlsx, archive.zip
//	  Documents/
//	    report1.pdf, report2.pdf, presentation.pptx
//	    Reports/
//	      summary.pdf
//	  Photos/
//	    photo1.jpg
//	    Trip/
//	      photo2.jpg, photo3.jpg
//
// Sizes and updated_at all vary so both the min/max-size and after/before
// search filters have real boundaries to test against.
func seedRahulTree(ctx context.Context, pool *pgxpool.Pool, s3c *s3.Client, bucket string, owner, rootID uuid.UUID) (int, error) {
	docs, err := createFolder(ctx, pool, owner, rootID, "Documents")
	if err != nil {
		return 0, err
	}
	reports, err := createFolder(ctx, pool, owner, docs, "Reports")
	if err != nil {
		return 0, err
	}
	photos, err := createFolder(ctx, pool, owner, rootID, "Photos")
	if err != nil {
		return 0, err
	}
	trip, err := createFolder(ctx, pool, owner, photos, "Trip")
	if err != nil {
		return 0, err
	}

	const (
		xlsxMime = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
		pptxMime = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	)

	files := []struct {
		parent uuid.UUID
		spec   fileSpec
	}{
		{rootID, fileSpec{"readme.md", "text/markdown", 1200, 30 * day}},
		{rootID, fileSpec{"notes.txt", "text/plain", 3500, 1 * day}},
		{rootID, fileSpec{"budget.xlsx", xlsxMime, 45_000, 7 * day}},
		{rootID, fileSpec{"archive.zip", "application/zip", 120_000, 3 * day}},
		{docs, fileSpec{"report1.pdf", "application/pdf", 52_000, 10 * day}},
		{docs, fileSpec{"report2.pdf", "application/pdf", 98_000, 5 * day}},
		{docs, fileSpec{"presentation.pptx", pptxMime, 210_000, 20 * day}},
		{reports, fileSpec{"summary.pdf", "application/pdf", 15_000, 12 * day}},
		{photos, fileSpec{"photo1.jpg", "image/jpeg", 180_000, 2 * day}},
		{trip, fileSpec{"photo2.jpg", "image/jpeg", 220_000, 2 * day}},
		{trip, fileSpec{"photo3.jpg", "image/jpeg", 95_000, 15 * day}},
	}

	for _, f := range files {
		if err := createFile(ctx, pool, s3c, bucket, owner, f.parent, f.spec); err != nil {
			return 0, err
		}
	}
	return len(files), nil
}
