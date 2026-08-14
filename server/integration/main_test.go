// Package integration is Drive's closed-loop suite: real Postgres, real Garage,
// real Mailpit, and the real server binary running as a child process.
//
// Nothing here is mocked and nothing sleeps. The harness in
// server/internal/testutil owns the server, resets the schema, sweeps the
// leftovers of earlier failed runs, and moves time by backdating rows.
//
// Run it against the drive-test stack:
//
//	set -a; . ./.env.test; set +a
//	go test ./server/integration/ -count=1
package integration

import (
	"os"
	"testing"

	"github.com/rahul-sharma-cs/drive/server/internal/testutil"
)

// H is the run's harness. Tests read H.Server, H.Pool and H.S3 from it and
// build their identities with H.NewUser.
var H *testutil.Harness

func TestMain(m *testing.M) {
	// Explicit teardown: os.Exit skips deferred functions, and leaving a child
	// server or a temp binary behind would poison the next run.
	var code int
	func() {
		H = testutil.Start()
		defer H.Stop()

		// Phase 2 wires the GC here:
		//   testutil.RunGCOnce = func(ctx context.Context) error { return gc.RunOnce(ctx, ...) }
		// Until then testutil.GC fails with that instruction rather than
		// silently doing nothing.

		code = m.Run()
	}()
	os.Exit(code)
}
