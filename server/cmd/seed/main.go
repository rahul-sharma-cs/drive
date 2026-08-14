// Command seed populates Drive's database with the two demo users and
// rahul@drive.local's sample folder tree (PLAN.md §Seed). Idempotent: a no-op
// once any user exists.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/db"
	"github.com/rahul-sharma-cs/drive/server/internal/seed"
)

func main() {
	envFile := flag.String("env-file", ".env", "env file to load before seeding (same pattern as cmd/infra-init)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(context.Background(), log, *envFile); err != nil {
		log.Error("seed failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, envFile string) error {
	if err := loadEnvFile(envFile); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Seed is also called mid-e2e-run against a freshly recreated schema, ahead
	// of the server's own boot (which would otherwise apply migrations first) —
	// running Migrate here too makes seed self-sufficient regardless of
	// invocation order, and it is a no-op against an already-migrated database.
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	s3Client, _, err := blob.New(ctx, cfg)
	if err != nil {
		return err
	}

	return seed.Run(ctx, pool, s3Client, cfg.S3Bucket, log)
}

// loadEnvFile reads KEY=VALUE lines into the process environment, overriding
// whatever the caller's shell already exported — the same "env file is
// authoritative" rule cmd/infra-init uses, so `-env-file .env.test` reliably
// wins over a dev shell with .env sourced.
func loadEnvFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s not found — run `make infra-init` first", path)
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("set %s: %w", k, err)
		}
	}
	return nil
}
