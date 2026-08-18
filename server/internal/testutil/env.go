// Package testutil is Drive's integration harness: the machinery the closed-loop
// suites in server/integration are built on.
//
// Three things live here and nothing else does:
//
//   - the run-level setup -- reset the schema, sweep leftover Garage
//     multiparts, purge the Mailpit inbox, build the server binary once, and
//     own the server as a child process the tests can SIGKILL and restart;
//   - time control -- direct SQL backdating of expires_at / created_at /
//     confirmed_at / window_start plus the RunGCOnce hook, so no suite ever
//     sleeps a wall-clock window;
//   - fixtures -- an authenticated HTTP client two lines from a signed-in
//     request, and real objects in Garage for the phases whose upload protocol
//     does not exist yet.
//
// Everything targets the drive-test compose stack. The values come from the
// committed .env.test, and guardTestStack refuses to run if any of them still
// points at the dev stack: this package drops schemas, aborts multipart uploads
// and empties an inbox, none of which is reversible.
package testutil

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rahul-sharma-cs/drive/server/internal/config"
)

// envFile is the committed test-stack environment, read from the repo root.
const envFile = ".env.test"

// devPorts are the dev stack's host ports. Seeing one of these in a resolved
// config means the harness is pointed at the developer's real data.
var devPorts = []struct {
	what string
	port string
}{
	{"DRIVE_DB_DSN", ":55432"},
	{"DRIVE_S3_ENDPOINT", ":3900"},
	{"DRIVE_MAILPIT_API", ":8025"},
	{"DRIVE_SMTP_ADDR", ":1025"},
}

// RepoRoot returns the directory holding go.work, walking up from the working
// directory. Every `go` invocation and every path this package resolves is
// relative to it, so a suite works regardless of which directory `go test` was
// started from.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("testutil: working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("testutil: no go.work above the working directory")
		}
		dir = parent
	}
}

// LoadTestEnv exports the DRIVE_* variables from the repo's .env.test, so
// `go test ./server/integration/` works on its own -- without `make test`, and
// without the caller having sourced anything.
//
// A variable already present in the environment wins, which is what makes the
// documented `set -a; . ./.env.test` invocation and a per-suite override both
// behave the way a reader expects. Anything that points at the dev stack is
// caught later by guardTestStack, not silently accepted.
func LoadTestEnv(root string) error {
	f, err := os.Open(filepath.Join(root, envFile))
	if err != nil {
		return fmt.Errorf("testutil: %s: %w", envFile, err)
	}
	defer f.Close()

	vars, err := parseDotEnv(f)
	if err != nil {
		return fmt.Errorf("testutil: %s: %w", envFile, err)
	}
	for k, v := range vars {
		if !strings.HasPrefix(k, "DRIVE_") {
			continue // GARAGE_*/POSTGRES_* are compose's, not the server's
		}
		if _, set := os.LookupEnv(k); set {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("testutil: setting %s: %w", k, err)
		}
	}
	return nil
}

// parseDotEnv reads the KEY=VALUE subset compose and this harness both use:
// blank lines and # comments are skipped, and a value may be quoted.
func parseDotEnv(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		out[key] = value
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// guardTestStack refuses a configuration pointing at the dev stack.
//
// This is not defensive decoration. Setup drops schema public, aborts every
// multipart upload in the bucket and empties the Mailpit inbox; against the dev
// stack that destroys the developer's working data with no way back.
func guardTestStack(cfg *config.Config) error {
	values := map[string]string{
		"DRIVE_DB_DSN":      cfg.DBDSN,
		"DRIVE_S3_ENDPOINT": cfg.S3Endpoint,
		"DRIVE_MAILPIT_API": cfg.MailpitAPI,
		"DRIVE_SMTP_ADDR":   cfg.SMTPAddr,
	}
	for _, dev := range devPorts {
		if strings.Contains(values[dev.what], dev.port) {
			return fmt.Errorf(
				"testutil: %s is %q, which is the DEV stack (port %s). The harness resets schemas, aborts multiparts and empties the inbox: it only ever runs against the drive-test stack from %s",
				dev.what, values[dev.what], dev.port, envFile)
		}
	}
	return nil
}
