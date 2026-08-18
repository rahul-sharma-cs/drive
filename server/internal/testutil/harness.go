package testutil

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/mail"
)

// setupTimeout bounds the whole global setup, migrations included.
const setupTimeout = 3 * time.Minute

// Harness is one run's shared state: the test-stack config, a pool, an S3
// client, the Mailpit inbox, and the server the tests talk to.
//
// Exactly one exists per `go test` invocation, built by Start from TestMain.
type Harness struct {
	Cfg     *config.Config
	Pool    *pgxpool.Pool
	S3      *s3.Client
	Presign *s3.PresignClient
	Mailpit *mail.Client

	// Server is the shared server every test uses unless it needs to kill one,
	// in which case it calls SpawnServer for a private child.
	Server *Child

	RepoRoot string

	binary string
	tmpDir string
	users  atomic.Int64
}

// Start performs the run-level global setup and returns the harness. Call it
// from TestMain and pair it with Stop.
//
// The order is deliberate:
//
//  1. resolve the test-stack config and refuse anything pointing at dev;
//  2. reset the schema over a short-lived connection, which is then closed --
//     a pool that predates DROP SCHEMA carries cached type OIDs and plans that
//     stop matching the moment the server re-migrates;
//  3. sweep leftover Garage multiparts and empty the Mailpit inbox, because a
//     prior failed run -- a kill -9 test above all -- never cleaned up;
//  4. build the binary and start the shared server, which runs goose.Up;
//  5. only now open the pool the tests share.
func Start() *Harness {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	root, err := RepoRoot()
	if err != nil {
		log.Fatal(err)
	}
	if err := LoadTestEnv(root); err != nil {
		log.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("testutil: loading the config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("testutil: %v", err)
	}
	if err := guardTestStack(cfg); err != nil {
		log.Fatal(err)
	}

	h := &Harness{Cfg: cfg, RepoRoot: root, Mailpit: mail.NewClient(cfg.MailpitAPI)}

	if err := resetSchema(ctx, cfg.DBDSN); err != nil {
		log.Fatal(err)
	}

	h.S3, h.Presign, err = blob.New(ctx, cfg)
	if err != nil {
		log.Fatalf("testutil: s3 client: %v", err)
	}
	if n, err := SweepMultiparts(ctx, h.S3, cfg.S3Bucket); err != nil {
		log.Fatal(err)
	} else if n > 0 {
		log.Printf("testutil: aborted %d multipart upload(s) left by an earlier run", n)
	}
	if err := h.Mailpit.DeleteAll(ctx); err != nil {
		log.Fatalf("testutil: purging the Mailpit inbox: %v", err)
	}

	h.tmpDir, h.binary, err = buildServer(root)
	if err != nil {
		log.Fatal(err)
	}

	port, err := freePort()
	if err != nil {
		log.Fatal(err)
	}
	h.Server = newChild(h.binary, port)
	if err := h.Server.Start(ctx); err != nil {
		log.Fatal(err)
	}

	h.Pool, err = pgxpool.New(ctx, cfg.DBDSN)
	if err != nil {
		log.Fatalf("testutil: opening the pool: %v", err)
	}
	if err := h.Pool.Ping(ctx); err != nil {
		log.Fatalf("testutil: pinging the pool: %v", err)
	}
	return h
}

// Stop tears the run down. TestMain must call it explicitly before os.Exit --
// deferred functions do not run through os.Exit.
func (h *Harness) Stop() {
	if h == nil {
		return
	}
	if h.Server != nil {
		if err := h.Server.Kill(); err != nil {
			log.Printf("testutil: %v", err)
		}
	}
	if h.Pool != nil {
		h.Pool.Close()
	}
	if h.tmpDir != "" {
		_ = os.RemoveAll(h.tmpDir)
	}
}

// BaseURL is the shared server's origin.
func (h *Harness) BaseURL() string { return h.Server.URL }

// resetSchema drops and recreates schema public so the server rebuilds it from
// the migrations at boot. It runs over its own connection, which is closed
// before anything else opens one.
//
// Backends left over from an earlier run are terminated first: a stale drive
// binary still holding connections would block the DROP indefinitely. The dev
// guard above is what makes that safe to do unconditionally.
func resetSchema(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("testutil: connecting to reset the schema: %w", err)
	}
	defer conn.Close(ctx)

	const kick = `
		SELECT pg_terminate_backend(pid)
		  FROM pg_stat_activity
		 WHERE datname = current_database() AND pid <> pg_backend_pid()`
	if _, err := conn.Exec(ctx, kick); err != nil {
		return fmt.Errorf("testutil: clearing stale connections: %w", err)
	}
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		return fmt.Errorf("testutil: resetting schema public: %w", err)
	}
	return nil
}

// SweepMultiparts aborts every in-progress multipart upload in the bucket and
// reports how many it aborted.
//
// Prior failed runs never clean up after themselves: a kill -9 in the middle of
// the interruption battery leaves an initiated multipart in Garage that no
// session row references, and nothing else ever collects it in a test stack.
func SweepMultiparts(ctx context.Context, c *s3.Client, bucket string) (int, error) {
	var (
		aborted   int
		keyMarker *string
		idMarker  *string
	)
	for {
		out, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
			Bucket:         aws.String(bucket),
			KeyMarker:      keyMarker,
			UploadIdMarker: idMarker,
		})
		if err != nil {
			return aborted, fmt.Errorf("testutil: listing multipart uploads: %w", err)
		}
		for _, up := range out.Uploads {
			_, err := c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(bucket),
				Key:      up.Key,
				UploadId: up.UploadId,
			})
			if err != nil {
				return aborted, fmt.Errorf("testutil: aborting multipart %s: %w", aws.ToString(up.UploadId), err)
			}
			aborted++
		}
		if !aws.ToBool(out.IsTruncated) {
			return aborted, nil
		}
		keyMarker, idMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}
