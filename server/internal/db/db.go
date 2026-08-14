// Package db owns the Postgres connection pool and the migration run.
package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/rahul-sharma-cs/drive/server/migrations"
)

// ErrForeignDatabase is returned when the configured database holds tables but
// none of them is goose's version table.
var ErrForeignDatabase = errors.New("DRIVE_DB_DSN: refusing to migrate: the database is not empty and has no goose_db_version table (not drive's database)")

// Connect opens the pgx pool and verifies it with a ping.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("DRIVE_DB_DSN: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("DRIVE_DB_DSN: %w", err)
	}
	return pool, nil
}

// Migrate runs the embedded goose migrations, but only against a database that
// is provably ours.
//
// The marker is goose's own goose_db_version table: if it exists, or the
// database is completely empty (a fresh compose volume), migrating is safe. A
// non-empty database without the marker is somebody else's Postgres and we
// refuse to touch it.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	hasGoose, tableCount, err := inspect(ctx, pool)
	if err != nil {
		return err
	}
	if err := decideMigrate(hasGoose, tableCount); err != nil {
		return err
	}

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	return nil
}

// decideMigrate is the ownership marker check, isolated from the database so it
// can be tested directly.
func decideMigrate(hasGooseTable bool, tableCount int) error {
	if hasGooseTable || tableCount == 0 {
		return nil
	}
	return ErrForeignDatabase
}

// inspect reports whether goose's version table exists and how many base tables
// live in schema public.
func inspect(ctx context.Context, pool *pgxpool.Pool) (hasGoose bool, tableCount int, err error) {
	const q = `
		SELECT to_regclass('public.goose_db_version') IS NOT NULL,
		       (SELECT count(*) FROM information_schema.tables
		         WHERE table_schema = 'public' AND table_type = 'BASE TABLE')`
	if err := pool.QueryRow(ctx, q).Scan(&hasGoose, &tableCount); err != nil {
		return false, 0, fmt.Errorf("DRIVE_DB_DSN: inspecting the database: %w", err)
	}
	return hasGoose, tableCount, nil
}
