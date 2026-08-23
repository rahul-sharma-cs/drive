package share

// The store against the real drive-test Postgres. Nothing here is worth faking:
// the one-active-link rule is a partial unique index, the resolver's states are
// a CASE over three joined tables, and the cap is two statements in one
// transaction -- a fake would test the fake.
//
// The database is shared with the other suites, so every case works inside
// users it creates itself and never truncates anything.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/db"
	"github.com/rahul-sharma-cs/drive/server/internal/node"
)

// ---------------------------------------------------------------- harness ----

// testDSN is the drive-test stack's Postgres, verbatim from the committed
// .env.test. Tests never touch the dev stack on :55432.
const testDSN = "postgres://drive:drive@localhost:55433/drive?sslmode=disable"

// migrateLock serializes goose against the other packages' suites, which run
// as separate binaries against this same database.
const migrateLock = int64(0x64726976)

var (
	poolOnce sync.Once
	poolConn *pgxpool.Pool
	poolErr  error
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		dsn := os.Getenv("DRIVE_DB_DSN")
		if dsn == "" {
			dsn = testDSN
		}
		if strings.Contains(dsn, ":55432") {
			poolErr = fmt.Errorf("DRIVE_DB_DSN points at the dev stack (%s); tests run against drive-test on :55433", dsn)
			return
		}
		ctx := context.Background()
		if poolConn, poolErr = db.Connect(ctx, dsn); poolErr != nil {
			return
		}
		conn, err := poolConn.Acquire(ctx)
		if err != nil {
			poolErr = err
			return
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLock); err != nil {
			poolErr = err
			return
		}
		poolErr = db.Migrate(ctx, poolConn)
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrateLock)
	})
	if poolErr != nil {
		t.Fatalf("drive-test database: %v (is `docker compose -p drive-test --env-file .env.test up -d` running?)", poolErr)
	}
	return poolConn
}

// fixture is one throwaway user with a root folder. Isolation between cases
// is by owner: every owner query is owner-scoped, so a fresh user is a fresh
// universe, and every share row hangs off a node this user owns.
type fixture struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	store *Store
	owner uuid.UUID
	root  uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), pool: testPool(t), owner: uuid.New(), root: uuid.New()}
	f.store = NewStore(f.pool)

	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO users (id, email, password_hash, display_name, email_verified_at)
		 VALUES ($1, $2, 'x', 'Share Test', now())`, f.owner, "share-"+uuid.NewString()+"@drive.test"); err != nil {
		t.Fatalf("inserting user: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		 VALUES ($1, $2, NULL, 'folder', 'My Drive')`, f.root, f.owner); err != nil {
		t.Fatalf("inserting root: %v", err)
	}
	return f
}

// file inserts a file plus the blob it points at, the way a finished upload
// does. Eleven bytes, text/plain, for the resolver's assertions.
func (f *fixture) file(name string) (nodeID, blobID uuid.UUID) {
	f.t.Helper()
	nodeID, blobID = uuid.New(), uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO blobs (id, object_key, size, etag) VALUES ($1, $2, 11, 'etag')`,
		blobID, "blobs/"+blobID.String()); err != nil {
		f.t.Fatalf("inserting blob: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name, blob_id, size, mime)
		 VALUES ($1, $2, $3, 'file', $4, $5, 11, 'text/plain')`,
		nodeID, f.owner, f.root, name, blobID); err != nil {
		f.t.Fatalf("inserting file %q: %v", name, err)
	}
	return nodeID, blobID
}

// create makes a share and fails the test on anything but success.
func (f *fixture) create(nodeID uuid.UUID, set Settings) (Share, string) {
	f.t.Helper()
	sh, raw, err := f.store.Create(f.ctx, f.owner, nodeID, set)
	if err != nil {
		f.t.Fatalf("Create: %v", err)
	}
	return sh, raw
}

// resolve looks a raw token up the way a public route does.
func (f *fixture) resolve(raw string) (*Resolved, error) {
	f.t.Helper()
	return f.store.Resolve(f.ctx, auth.HashToken(raw))
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func (f *fixture) guests(shareID uuid.UUID) int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM share_guest_sessions WHERE share_id = $1`, shareID)
}

func (f *fixture) downloadCount(shareID uuid.UUID) int {
	f.t.Helper()
	return f.count(`SELECT download_count FROM shares WHERE id = $1`, shareID)
}

func (f *fixture) mint(shareID uuid.UUID) (string, Guest) {
	f.t.Helper()
	raw, g, err := f.store.MintGuest(f.ctx, shareID)
	if err != nil {
		f.t.Fatalf("MintGuest: %v", err)
	}
	return raw, g
}

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------- resolve ----

// Every state a share row can be in comes back as that state with the share
// id filled, and only a hash that matches no row at all is ErrNotFound. That
// split is what lets a public route answer one 404 for every dead link and
// still write the denied row for the four that have a share to attribute it
// to -- and write nothing for a token scan.
func TestResolveStates(t *testing.T) {
	f := newFixture(t)

	t.Run("live", func(t *testing.T) {
		fileID, blobID := f.file("live.txt")
		sh, raw := f.create(fileID, Settings{})

		r, err := f.resolve(raw)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if r.State != StateLive {
			t.Fatalf("State = %q, want live", r.State)
		}
		if r.ShareID != sh.ID || r.NodeID != fileID || r.ParentID == nil || *r.ParentID != f.root {
			t.Errorf("ids = share %s node %s parent %v, want %s %s %s", r.ShareID, r.NodeID, r.ParentID, sh.ID, fileID, f.root)
		}
		if r.Name != "live.txt" || r.Size != 11 || r.Mime != "text/plain" || r.ObjectKey != "blobs/"+blobID.String() {
			t.Errorf("file = %q %d %q %q, want live.txt 11 text/plain blobs/%s", r.Name, r.Size, r.Mime, r.ObjectKey, blobID)
		}
		if r.PasswordHash != nil || r.ExpiresAt != nil || r.MaxDownloads != nil || r.DownloadCount != 0 {
			t.Errorf("settings = %v %v %v %d, want all unset", r.PasswordHash, r.ExpiresAt, r.MaxDownloads, r.DownloadCount)
		}
	})

	dead := []struct {
		state State
		setup func(fileID, shareID uuid.UUID)
	}{
		{StateRevoked, func(_, shareID uuid.UUID) {
			if err := f.store.Revoke(f.ctx, f.owner, shareID); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
		}},
		{StateExpired, func(_, shareID uuid.UUID) {
			f.exec(`UPDATE shares SET expires_at = now() - interval '1 second' WHERE id = $1`, shareID)
		}},
		{StateTrashed, func(fileID, _ uuid.UUID) {
			f.exec(`UPDATE nodes SET deleted_at = now(), trashed_root = true WHERE id = $1`, fileID)
		}},
		{StatePurged, func(fileID, _ uuid.UUID) {
			// A file row whose blob is gone: what the download join refuses
			// and a real purge never leaves behind.
			f.exec(`UPDATE nodes SET blob_id = NULL WHERE id = $1`, fileID)
		}},
	}
	for _, c := range dead {
		t.Run(string(c.state), func(t *testing.T) {
			fileID, _ := f.file(string(c.state) + ".txt")
			sh, raw := f.create(fileID, Settings{})
			c.setup(fileID, sh.ID)

			r, err := f.resolve(raw)
			if err != nil {
				t.Fatalf("Resolve: %v -- a dead link still has a share row to attribute a denied row to", err)
			}
			if r.State != c.state {
				t.Errorf("State = %q, want %q", r.State, c.state)
			}
			if r.ShareID != sh.ID {
				t.Errorf("ShareID = %s, want %s", r.ShareID, sh.ID)
			}
		})
	}

	t.Run("unknown", func(t *testing.T) {
		_, err := f.resolve("no-such-token")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Resolve of an unknown token = %v, want ErrNotFound", err)
		}
	})
}

// ----------------------------------------------------------------- create ----

// The row holds sha256(token) and nothing that could be turned back into the
// URL: a dump of the shares table opens no file.
func TestCreateStoresOnlyTheHash(t *testing.T) {
	f := newFixture(t)
	fileID, _ := f.file("a.txt")
	sh, raw := f.create(fileID, Settings{})

	if len(raw) != 43 {
		t.Errorf("raw token is %d characters, want 43 (32 bytes, unpadded base64url)", len(raw))
	}
	var stored []byte
	if err := f.pool.QueryRow(f.ctx, `SELECT token_hash FROM shares WHERE id = $1`, sh.ID).Scan(&stored); err != nil {
		t.Fatalf("reading token_hash: %v", err)
	}
	if string(stored) != string(auth.HashToken(raw)) {
		t.Errorf("token_hash is not sha256(token)")
	}
	if n := f.count(`SELECT count(*) FROM shares WHERE token_hash = $1`, []byte(raw)); n != 0 {
		t.Errorf("the raw token is stored as the hash")
	}
	if sh.Node.ID != fileID || sh.Node.Name != "a.txt" || !sh.NodeLive || sh.HasPassword {
		t.Errorf("Share = %+v, want the live file and no password", sh)
	}
}

// Two creates for one file at once: one row, one id, and the loser gets
// ErrExists rather than a raw unique-violation it cannot map. The index is
// the rule and ON CONFLICT is how the rule is read; a pre-check would leave a
// window between checking and inserting that this test is shaped to hit.
func TestConcurrentCreatesYieldOneRow(t *testing.T) {
	f := newFixture(t)
	fileID, _ := f.file("raced.txt")

	type result struct {
		sh  Share
		err error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	for range 2 {
		go func() {
			start.Wait()
			sh, _, err := f.store.Create(f.ctx, f.owner, fileID, Settings{})
			results <- result{sh, err}
		}()
	}
	start.Done()

	var wins, exists int
	for range 2 {
		r := <-results
		switch {
		case r.err == nil:
			wins++
		case errors.Is(r.err, ErrExists):
			exists++
		default:
			t.Errorf("the loser got %v, want ErrExists", r.err)
		}
	}
	if wins != 1 || exists != 1 {
		t.Errorf("wins = %d, exists = %d, want 1 and 1", wins, exists)
	}
	if n := f.count(`SELECT count(*) FROM shares WHERE node_id = $1 AND revoked_at IS NULL`, fileID); n != 1 {
		t.Errorf("%d active rows for the file, want 1", n)
	}

	// And the slot is the active link only: once it is revoked, the file can
	// be shared again.
	if err := f.store.Revoke(f.ctx, f.owner, firstActive(t, f, fileID)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, _, err := f.store.Create(f.ctx, f.owner, fileID, Settings{}); err != nil {
		t.Errorf("a create after the revoke = %v, want success", err)
	}
}

func firstActive(t *testing.T, f *fixture, fileID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM shares WHERE node_id = $1 AND revoked_at IS NULL`, fileID).Scan(&id); err != nil {
		t.Fatalf("reading the active share: %v", err)
	}
	return id
}

// The create path resolves the node the way the public routes will: a
// folder is unsupported, and a file row without bytes, another owner's file
// and a trashed one are all the node's own miss.
func TestCreateResolvesTheNodeFirst(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)

	folderID := uuid.New()
	f.exec(`INSERT INTO nodes (id, owner_id, parent_id, kind, name) VALUES ($1, $2, $3, 'folder', 'Docs')`, folderID, f.owner, f.root)
	bloblessID := uuid.New()
	f.exec(`INSERT INTO nodes (id, owner_id, parent_id, kind, name, size) VALUES ($1, $2, $3, 'file', 'orphan.bin', 0)`, bloblessID, f.owner, f.root)
	trashedID, _ := f.file("gone.txt")
	f.exec(`UPDATE nodes SET deleted_at = now(), trashed_root = true WHERE id = $1`, trashedID)
	foreignID, _ := other.file("theirs.txt")

	cases := []struct {
		what string
		id   uuid.UUID
		want error
	}{
		{"a folder", folderID, ErrUnsupported},
		{"a file with no blob", bloblessID, node.ErrNotFound},
		{"a trashed file", trashedID, node.ErrNotFound},
		{"another user's file", foreignID, node.ErrNotFound},
		{"an unknown id", uuid.New(), node.ErrNotFound},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			_, _, err := f.store.Create(f.ctx, f.owner, c.id, Settings{})
			if !errors.Is(err, c.want) {
				t.Fatalf("Create = %v, want %v", err, c.want)
			}
			if n := f.count(`SELECT count(*) FROM shares WHERE node_id = $1`, c.id); n != 0 {
				t.Errorf("%d share rows were written for a refused node", n)
			}
		})
	}
}

// -------------------------------------------------------------------- cap ----

// The cap is one transaction: a session that is refused keeps its NULL stamp
// and the count stays at the cap; a session that already counted never counts
// again, however many times it asks.
func TestCountOnceStopsAtTheCapAndCountsASessionOnce(t *testing.T) {
	f := newFixture(t)

	t.Run("cap of two, three sessions", func(t *testing.T) {
		fileID, _ := f.file("capped.txt")
		sh, _ := f.create(fileID, Settings{MaxDownloads: ptr(2)})
		_, g1 := f.mint(sh.ID)
		_, g2 := f.mint(sh.ID)
		_, g3 := f.mint(sh.ID)

		for i, g := range []Guest{g1, g2} {
			exhausted, err := f.store.CountOnce(f.ctx, g.ID, sh.ID)
			if err != nil || exhausted {
				t.Fatalf("session %d: CountOnce = (%v, %v), want counted", i+1, exhausted, err)
			}
		}
		exhausted, err := f.store.CountOnce(f.ctx, g3.ID, sh.ID)
		if err != nil || !exhausted {
			t.Fatalf("third session: CountOnce = (%v, %v), want exhausted", exhausted, err)
		}
		if n := f.downloadCount(sh.ID); n != 2 {
			t.Errorf("download_count = %d, want 2", n)
		}
		// The refused session's stamp rolled back with the count: it can
		// download after the owner raises the cap.
		var stamped bool
		if err := f.pool.QueryRow(f.ctx, `SELECT downloaded_at IS NOT NULL FROM share_guest_sessions WHERE id = $1`, g3.ID).Scan(&stamped); err != nil {
			t.Fatalf("reading the third session: %v", err)
		}
		if stamped {
			t.Error("the refused session was stamped downloaded_at -- its download vanished into a cap it never spent")
		}

		// A counted session asking again is a re-issue, not a refusal, even
		// with the cap spent.
		exhausted, err = f.store.CountOnce(f.ctx, g1.ID, sh.ID)
		if err != nil || exhausted {
			t.Errorf("a counted session at the cap: CountOnce = (%v, %v), want re-issued", exhausted, err)
		}
	})

	t.Run("one session, three calls, no cap", func(t *testing.T) {
		fileID, _ := f.file("uncapped.txt")
		sh, _ := f.create(fileID, Settings{})
		_, g := f.mint(sh.ID)

		for i := range 3 {
			exhausted, err := f.store.CountOnce(f.ctx, g.ID, sh.ID)
			if err != nil || exhausted {
				t.Fatalf("call %d: CountOnce = (%v, %v)", i+1, exhausted, err)
			}
		}
		if n := f.downloadCount(sh.ID); n != 1 {
			t.Errorf("download_count = %d after three calls from one session, want 1", n)
		}
	})
}

// ----------------------------------------------------------------- guests ----

// A guest session is the row behind a cookie: it round-trips on its own
// share, answers nothing on any other, dies when it expires, and slides
// while it is used.
func TestGuestSessions(t *testing.T) {
	f := newFixture(t)
	fileA, _ := f.file("a.txt")
	fileB, _ := f.file("b.txt")
	shareA, _ := f.create(fileA, Settings{})
	shareB, _ := f.create(fileB, Settings{})

	raw, minted := f.mint(shareA.ID)
	if minted.ShareID != shareA.ID || minted.DownloadedAt != nil {
		t.Fatalf("minted = %+v, want share A and no download", minted)
	}
	if until := time.Until(minted.ExpiresAt); until < 29*time.Minute || until > 31*time.Minute {
		t.Errorf("a fresh session expires in %v, want ~30m", until)
	}

	g, err := f.store.GuestFor(f.ctx, shareA.ID, raw)
	if err != nil || g.ID != minted.ID {
		t.Fatalf("GuestFor on its own share = (%+v, %v), want the minted session", g, err)
	}
	if _, err := f.store.GuestFor(f.ctx, shareB.ID, raw); !errors.Is(err, ErrNotFound) {
		t.Errorf("a guest of A presented at B = %v, want ErrNotFound", err)
	}
	if _, err := f.store.GuestFor(f.ctx, shareA.ID, "not-a-cookie"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown cookie = %v, want ErrNotFound", err)
	}
	if _, err := f.store.GuestFor(f.ctx, shareA.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("an empty cookie = %v, want ErrNotFound", err)
	}

	// Sliding: a session one minute from dying is given the full TTL again.
	f.exec(`UPDATE share_guest_sessions SET expires_at = now() + interval '1 minute' WHERE id = $1`, minted.ID)
	slid, err := f.store.ReuseGuest(f.ctx, minted.ID)
	if err != nil {
		t.Fatalf("ReuseGuest: %v", err)
	}
	if until := time.Until(slid.ExpiresAt); until < 29*time.Minute {
		t.Errorf("a reused session expires in %v, want ~30m -- a 31-minute visit dies mid-page", until)
	}

	// Expired: gone for both lookups.
	f.exec(`UPDATE share_guest_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, minted.ID)
	if _, err := f.store.GuestFor(f.ctx, shareA.ID, raw); !errors.Is(err, ErrNotFound) {
		t.Errorf("an expired session = %v, want ErrNotFound", err)
	}
	if _, err := f.store.ReuseGuest(f.ctx, minted.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("reusing an expired session = %v, want ErrNotFound", err)
	}
}

// ------------------------------------------------- settings and regenerate ----

// Expiry and cap are replaced outright; the password is a three-way change.
// Keep touches neither the hash nor the sessions; set stores the hash and
// ends the sessions minted before the gate went up; clear removes the hash
// and ends the sessions only when there was a gate to remove. Regenerate is a
// new link: new hash, old one dead, count at zero, sessions gone.
func TestSettingsAndRegenerate(t *testing.T) {
	f := newFixture(t)
	fileID, _ := f.file("s.txt")
	sh, raw := f.create(fileID, Settings{})
	other := newFixture(t)

	until := time.Now().Add(48 * time.Hour).Truncate(time.Microsecond)
	hash := "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	t.Run("set all three", func(t *testing.T) {
		f.mint(sh.ID)
		got, err := f.store.Settings(f.ctx, f.owner, sh.ID, Settings{ExpiresAt: &until, MaxDownloads: ptr(5), Password: SetPassword(hash)})
		if err != nil {
			t.Fatalf("Settings: %v", err)
		}
		if !got.HasPassword || got.ExpiresAt == nil || !got.ExpiresAt.Equal(until) || got.MaxDownloads == nil || *got.MaxDownloads != 5 {
			t.Errorf("Share = %+v, want password on, expiry %v, cap 5", got, until)
		}
		if n := f.guests(sh.ID); n != 0 {
			t.Errorf("%d guest sessions survive a password being set -- the cookie minted before the gate still downloads", n)
		}
		r, _ := f.resolve(raw)
		if r.PasswordHash == nil || *r.PasswordHash != hash {
			t.Errorf("Resolve.PasswordHash = %v, want the stored hash", r.PasswordHash)
		}
	})

	t.Run("an absent password keeps the hash and the sessions", func(t *testing.T) {
		f.mint(sh.ID)
		got, err := f.store.Settings(f.ctx, f.owner, sh.ID, Settings{MaxDownloads: ptr(3)})
		if err != nil {
			t.Fatalf("Settings: %v", err)
		}
		if !got.HasPassword || got.ExpiresAt != nil || got.MaxDownloads == nil || *got.MaxDownloads != 3 {
			t.Errorf("Share = %+v, want the password kept and the other two replaced", got)
		}
		r, _ := f.resolve(raw)
		if r.PasswordHash == nil || *r.PasswordHash != hash {
			t.Errorf("Resolve.PasswordHash = %v, want the identical stored hash", r.PasswordHash)
		}
		if n := f.guests(sh.ID); n != 1 {
			t.Errorf("%d guest sessions after an expiry-and-cap change, want 1 untouched", n)
		}
	})

	t.Run("clearing an existing password ends the sessions", func(t *testing.T) {
		got, err := f.store.Settings(f.ctx, f.owner, sh.ID, Settings{Password: ClearPassword()})
		if err != nil {
			t.Fatalf("Settings: %v", err)
		}
		if got.HasPassword || got.ExpiresAt != nil || got.MaxDownloads != nil {
			t.Errorf("Share = %+v, want everything cleared", got)
		}
		r, _ := f.resolve(raw)
		if r.PasswordHash != nil {
			t.Error("a cleared password still resolves to a hash -- requires_password stays on")
		}
		if n := f.guests(sh.ID); n != 0 {
			t.Errorf("%d guest sessions survive the gate being removed, want 0", n)
		}
	})

	t.Run("clearing when none exists leaves sessions alone", func(t *testing.T) {
		f.mint(sh.ID)
		if _, err := f.store.Settings(f.ctx, f.owner, sh.ID, Settings{Password: ClearPassword()}); err != nil {
			t.Fatalf("Settings: %v", err)
		}
		if n := f.guests(sh.ID); n != 1 {
			t.Errorf("%d guest sessions after clearing a password that was not there, want 1", n)
		}
	})

	t.Run("regenerate", func(t *testing.T) {
		f.exec(`UPDATE shares SET download_count = 3 WHERE id = $1`, sh.ID)
		got, fresh, err := f.store.Regenerate(f.ctx, f.owner, sh.ID)
		if err != nil {
			t.Fatalf("Regenerate: %v", err)
		}
		if fresh == raw || len(fresh) != 43 {
			t.Errorf("new token %q is not a fresh 43-character token", fresh)
		}
		if got.DownloadCount != 0 {
			t.Errorf("download_count = %d after regenerate, want 0 -- the replacement link is born exhausted", got.DownloadCount)
		}
		if n := f.guests(sh.ID); n != 0 {
			t.Errorf("%d guest sessions survive a regenerate", n)
		}
		if _, err := f.resolve(raw); !errors.Is(err, ErrNotFound) {
			t.Errorf("the old token still resolves: %v", err)
		}
		if r, err := f.resolve(fresh); err != nil || r.State != StateLive || r.ShareID != sh.ID {
			t.Errorf("the new token resolves to (%+v, %v), want the same share, live", r, err)
		}
	})

	t.Run("another owner", func(t *testing.T) {
		if _, err := f.store.Settings(f.ctx, other.owner, sh.ID, Settings{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Settings by another owner = %v, want ErrNotFound", err)
		}
		if _, _, err := f.store.Regenerate(f.ctx, other.owner, sh.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("Regenerate by another owner = %v, want ErrNotFound", err)
		}
		if err := f.store.Revoke(f.ctx, other.owner, sh.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("Revoke by another owner = %v, want ErrNotFound", err)
		}
	})
}

// ----------------------------------------------------------------- revoke ----

// Revoke keeps the row and ends everything else: the sessions, the link, and
// the share's claim on the file's one slot.
func TestRevokeKeepsTheRow(t *testing.T) {
	f := newFixture(t)
	fileID, _ := f.file("r.txt")
	sh, raw := f.create(fileID, Settings{})
	f.mint(sh.ID)

	if err := f.store.Revoke(f.ctx, f.owner, sh.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	var revoked bool
	if err := f.pool.QueryRow(f.ctx, `SELECT revoked_at IS NOT NULL FROM shares WHERE id = $1`, sh.ID).Scan(&revoked); err != nil {
		t.Fatalf("the row is gone: %v -- the access log has nothing to point at", err)
	}
	if !revoked {
		t.Error("revoked_at is still NULL")
	}
	if n := f.guests(sh.ID); n != 0 {
		t.Errorf("%d guest sessions survive a revoke -- a minted session still downloads", n)
	}
	if r, err := f.resolve(raw); err != nil || r.State != StateRevoked {
		t.Errorf("Resolve after revoke = (%+v, %v), want revoked", r, err)
	}
	if err := f.store.Revoke(f.ctx, f.owner, sh.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a second Revoke = %v, want ErrNotFound", err)
	}
	if _, _, err := f.store.Create(f.ctx, f.owner, fileID, Settings{}); err != nil {
		t.Errorf("Create after revoke = %v, want the slot free again", err)
	}
}

// ------------------------------------------------------------------- list ----

// The listing is the owner's active links, newest first, paged by keyset and
// narrowed by node. Another owner's links never appear in it.
func TestListIsOwnerScopedAndPaged(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)

	var ids, files []uuid.UUID
	for i := range 5 {
		fileID, _ := f.file(fmt.Sprintf("f%d.txt", i))
		sh, _ := f.create(fileID, Settings{})
		ids, files = append(ids, sh.ID), append(files, fileID)
	}
	theirsFile, _ := other.file("theirs.txt")
	other.create(theirsFile, Settings{})
	if err := f.store.Revoke(f.ctx, f.owner, ids[2]); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	var seen []uuid.UUID
	var after *Cursor
	for page := 0; ; page++ {
		items, next, err := f.store.List(f.ctx, f.owner, nil, after, 2)
		if err != nil {
			t.Fatalf("List page %d: %v", page, err)
		}
		for i, sh := range items {
			if sh.CreatedBy != f.owner {
				t.Fatalf("page %d item %d belongs to %s -- another user's link appears", page, i, sh.CreatedBy)
			}
			if sh.RevokedAt != nil {
				t.Fatalf("page %d item %d is revoked", page, i)
			}
			if i > 0 {
				prev := items[i-1]
				if sh.CreatedAt.After(prev.CreatedAt) || (sh.CreatedAt.Equal(prev.CreatedAt) && sh.ID.String() > prev.ID.String()) {
					t.Errorf("page %d is not newest first at item %d", page, i)
				}
			}
			seen = append(seen, sh.ID)
		}
		if next == nil {
			break
		}
		after = next
		if page > 5 {
			t.Fatal("paging never ends")
		}
	}
	if len(seen) != 4 {
		t.Fatalf("listed %d shares, want the owner's 4 active ones", len(seen))
	}

	items, next, err := f.store.List(f.ctx, f.owner, &files[0], nil, 50)
	if err != nil || len(items) != 1 || items[0].ID != ids[0] || next != nil {
		t.Errorf("List by node = (%d items, %v, %v), want exactly that file's link", len(items), next, err)
	}
}

// -------------------------------------------------------------------- log ----

// The access-log writer takes whatever the request had: an unreadable peer is
// NULL rather than an inet the database refuses, and a User-Agent is cut to
// MaxUserAgent runes.
func TestLogNullIPAndTruncation(t *testing.T) {
	f := newFixture(t)
	fileID, _ := f.file("l.txt")
	sh, _ := f.create(fileID, Settings{})

	long := strings.Repeat("é", MaxUserAgent+100)
	if err := f.store.Log(f.ctx, &sh.ID, nil, ActionDenied, "", long); err != nil {
		t.Fatalf("Log with an empty ip: %v", err)
	}
	if err := f.store.Log(f.ctx, &sh.ID, nil, ActionView, "203.0.113.7", ""); err != nil {
		t.Fatalf("Log with an address: %v", err)
	}

	rows, err := f.pool.Query(f.ctx,
		`SELECT action, host(ip), char_length(user_agent), email FROM share_access_log WHERE share_id = $1 ORDER BY id`, sh.ID)
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	defer rows.Close()
	type entry struct {
		action string
		ip     *string
		length *int
		email  *string
	}
	var got []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.action, &e.ip, &e.length, &e.email); err != nil {
			t.Fatalf("scanning the log: %v", err)
		}
		got = append(got, e)
	}
	if len(got) != 2 {
		t.Fatalf("%d log rows, want 2", len(got))
	}
	if got[0].action != ActionDenied || got[0].ip != nil || got[0].length == nil || *got[0].length != MaxUserAgent || got[0].email != nil {
		t.Errorf("denied row = %+v, want NULL ip, %d-rune user agent, NULL email", got[0], MaxUserAgent)
	}
	if got[1].action != ActionView || got[1].ip == nil || *got[1].ip != "203.0.113.7" || got[1].length != nil {
		t.Errorf("view row = %+v, want the address and a NULL user agent", got[1])
	}
}
