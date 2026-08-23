package testutil

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// bootTimeout bounds the wait on /healthz. The first child of a run applies the
// whole migration before it listens, so this is generous on purpose.
const bootTimeout = 60 * time.Second

// healthPoll is how often the boot wait re-probes /healthz.
const healthPoll = 50 * time.Millisecond

// Child is one drive server running as a real process, which is the only way to
// test the crash cases honestly: SIGKILL it mid-flight and bring it back on the
// same port with the same state, because all of the state is in Postgres. An
// in-process server can be stopped but never killed.
type Child struct {
	// URL is the child's origin, e.g. http://127.0.0.1:53312.
	URL  string
	Port int

	bin  string
	env  []string
	logs *syncBuffer

	mu  sync.Mutex
	cmd *exec.Cmd
}

// buildServer compiles ./server/cmd/drive once per run. Every Child in the run
// then starts from this one binary: a per-test `go build` would dominate the
// suite's wall clock and prove nothing.
func buildServer(root string) (string, string, error) {
	tmp, err := os.MkdirTemp("", "drive-harness-")
	if err != nil {
		return "", "", fmt.Errorf("testutil: temp dir: %w", err)
	}
	bin := filepath.Join(tmp, "drive")

	cmd := exec.Command("go", "build", "-o", bin, "./server/cmd/drive")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(tmp)
		return "", "", fmt.Errorf("testutil: go build ./server/cmd/drive: %w\n%s", err, out)
	}
	return tmp, bin, nil
}

// freePort asks the kernel for an unused port and gives it straight back. There
// is a race in principle; in practice nothing else on this machine is grabbing
// ephemeral ports fast enough to matter, and a spawn that loses it fails loudly
// on the health wait rather than silently sharing a port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("testutil: reserving a port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, fmt.Errorf("testutil: releasing the reserved port: %w", err)
	}
	return port, nil
}

// newChild prepares a server on its own port. The child inherits the process
// environment -- which LoadTestEnv has already pointed at the drive-test stack
// -- with the address and base URL overridden so several servers can run at
// once against the one database.
//
// The origin names 127.0.0.1 and not localhost, which resolves to both ::1 and
// 127.0.0.1 here. The server listens on every family, so which one a dial picks
// is up to the resolver and to Happy Eyeballs -- and the per-IP bucket keys on
// the peer address, so a redialled connection that landed on the other family
// would be spending a second bucket's tokens. A test that counts requests into
// one bucket has to be sure they all arrive as one caller.
func newChild(bin string, port int) *Child {
	return &Child{
		URL:  fmt.Sprintf("http://127.0.0.1:%d", port),
		Port: port,
		bin:  bin,
		env: append(os.Environ(),
			fmt.Sprintf("DRIVE_ADDR=:%d", port),
			fmt.Sprintf("DRIVE_BASE_URL=http://127.0.0.1:%d", port),
		),
		logs: &syncBuffer{},
	}
}

// Start launches the process and waits until /healthz answers 200. Calling it
// on an already-running child is a no-op, which is what makes EnsureAlive cheap.
func (c *Child) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.cmd != nil {
		c.mu.Unlock()
		return nil
	}

	cmd := exec.Command(c.bin)
	cmd.Env = c.env
	cmd.Stdout = c.logs
	cmd.Stderr = c.logs
	if err := cmd.Start(); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("testutil: starting the server on port %d: %w", c.Port, err)
	}
	c.cmd = cmd
	c.mu.Unlock()

	if err := c.waitHealthy(ctx); err != nil {
		_ = c.Kill()
		return err
	}
	return nil
}

// waitHealthy polls /healthz, and reports the child's own log on failure --
// a boot that dies on a config or migration error says exactly why there.
func (c *Child) waitHealthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/healthz", nil)
		if err != nil {
			return fmt.Errorf("testutil: health request: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("testutil: server on port %d never became healthy: %w\n--- server log ---\n%s",
				c.Port, ctx.Err(), c.Log())
		case <-time.After(healthPoll):
		}
	}
}

// Kill SIGKILLs the process and reaps it. This is the interruption primitive:
// no shutdown hook runs, no in-flight request is finished, and anything the
// server had not written to Postgres is gone -- exactly the failure the resume
// protocol has to survive.
func (c *Child) Kill() error {
	c.mu.Lock()
	cmd := c.cmd
	c.cmd = nil
	c.mu.Unlock()

	if cmd == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !strings.Contains(err.Error(), "process already finished") {
		return fmt.Errorf("testutil: killing the server on port %d: %w", c.Port, err)
	}
	_, _ = cmd.Process.Wait()
	return nil
}

// Running reports whether this Child believes it has a live process.
func (c *Child) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cmd != nil
}

// Healthy reports whether /healthz answers 200 right now. Tests use it to
// assert a killed server is really gone, and the harness uses it to decide
// whether the shared server needs restarting.
func (c *Child) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Log returns everything the child has written to stdout and stderr so far --
// slog's JSON lines. Failure messages attach it.
func (c *Child) Log() string { return c.logs.String() }

// ---------------------------------------------------------------- harness API --

// SpawnServer starts an extra server on its own port for a test that wants to
// interrupt one. The test can Kill and Start it freely; cleanup stops it and
// guarantees the shared server is alive for whatever runs next.
func (h *Harness) SpawnServer(t testing.TB) *Child {
	t.Helper()

	port, err := freePort()
	if err != nil {
		t.Fatalf("%v", err)
	}
	child := newChild(h.binary, port)
	if err := child.Start(context.Background()); err != nil {
		t.Fatalf("%v", err)
	}

	t.Cleanup(func() {
		if err := child.Kill(); err != nil {
			t.Errorf("%v", err)
		}
		h.EnsureAlive(t)
	})
	return child
}

// EnsureAlive restarts the shared server if a test killed it. Register it (or
// let SpawnServer's cleanup do it) so the next test never inherits a dead
// server -- that is the one piece of harness state a test can break for
// everybody else.
func (h *Harness) EnsureAlive(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	if h.Server.Healthy(ctx) {
		return
	}
	if err := h.Server.Kill(); err != nil {
		t.Fatalf("%v", err)
	}
	if err := h.Server.Start(ctx); err != nil {
		t.Fatalf("restarting the shared server: %v", err)
	}
}

// syncBuffer is a Writer safe for the two pipes a child writes concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
