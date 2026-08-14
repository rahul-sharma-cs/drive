package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/db"
)

// These drive the real router -- middleware chain, session cookie and all --
// against the drive-test stack. The node package's suites cover the trash
// semantics; these cover the wire: status codes, envelopes and authorization.

// ---------------------------------------------------------------- harness ----

// trashTestDSN is the drive-test stack's Postgres, verbatim from .env.test.
const trashTestDSN = "postgres://drive:drive@localhost:55433/drive?sslmode=disable"

// trashMigrateLock serializes goose against the other packages' suites, which
// run as separate binaries against this same database.
const trashMigrateLock = int64(0x64726976)

var (
	trashPoolOnce sync.Once
	trashPool     *pgxpool.Pool
	trashPoolErr  error
)

func trashTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	trashPoolOnce.Do(func() {
		dsn := os.Getenv("DRIVE_DB_DSN")
		if dsn == "" {
			dsn = trashTestDSN
		}
		if strings.Contains(dsn, ":55432") {
			trashPoolErr = fmt.Errorf("DRIVE_DB_DSN points at the dev stack (%s); tests run against drive-test on :55433", dsn)
			return
		}
		ctx := context.Background()
		if trashPool, trashPoolErr = db.Connect(ctx, dsn); trashPoolErr != nil {
			return
		}
		conn, err := trashPool.Acquire(ctx)
		if err != nil {
			trashPoolErr = err
			return
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, trashMigrateLock); err != nil {
			trashPoolErr = err
			return
		}
		trashPoolErr = db.Migrate(ctx, trashPool)
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, trashMigrateLock)
	})
	if trashPoolErr != nil {
		t.Fatalf("drive-test database: %v (is `docker compose -p drive-test --env-file .env.test up -d` running?)", trashPoolErr)
	}
	return trashPool
}

// trashClient is one signed-in user talking to the real router.
type trashClient struct {
	t      *testing.T
	ctx    context.Context
	h      http.Handler
	pool   *pgxpool.Pool
	owner  uuid.UUID
	root   uuid.UUID
	cookie *http.Cookie
}

// newTrashClient seeds a verified user with a root folder and an auth session,
// then hands back a client holding that session's cookie. The session row is
// written directly: these tests are about the trash routes, not about signup.
func newTrashClient(t *testing.T) *trashClient {
	t.Helper()
	pool := trashTestPool(t)
	c := &trashClient{
		t:    t,
		ctx:  context.Background(),
		pool: pool,
		h:    New(&config.Config{}, pool, nil, nil, nil, nil).Routes(),
	}

	c.owner, c.root = uuid.New(), uuid.New()
	if _, err := pool.Exec(c.ctx,
		`INSERT INTO users (id, email, password_hash, display_name, email_verified_at)
		 VALUES ($1, $2, 'x', 'Test User', now())`,
		c.owner, "trash-"+uuid.NewString()+"@drive.test"); err != nil {
		t.Fatalf("seeding user: %v", err)
	}
	if _, err := pool.Exec(c.ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		 VALUES ($1, $2, NULL, 'folder', 'My Drive')`, c.root, c.owner); err != nil {
		t.Fatalf("seeding root folder: %v", err)
	}

	token := uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	if _, err := pool.Exec(c.ctx,
		`INSERT INTO auth_sessions (id, user_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, now() + interval '30 days')`,
		uuid.New(), c.owner, sum[:]); err != nil {
		t.Fatalf("seeding auth session: %v", err)
	}
	c.cookie = &http.Cookie{Name: SessionCookie, Value: token}
	return c
}

// do issues a request through the whole chain, with the CSRF header every
// mutation needs. Pass authed=false to send it anonymously.
func (c *trashClient) do(method, path string, authed bool) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	req.Header.Set(ClientHeader, "web")
	if authed {
		req.AddCookie(c.cookie)
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec
}

func (c *trashClient) folder(parent uuid.UUID, name string) uuid.UUID {
	c.t.Helper()
	id := uuid.New()
	if _, err := c.pool.Exec(c.ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		 VALUES ($1, $2, $3, 'folder', $4)`, id, c.owner, parent, name); err != nil {
		c.t.Fatalf("seeding folder: %v", err)
	}
	return id
}

func (c *trashClient) file(parent uuid.UUID, name string, size int64) uuid.UUID {
	c.t.Helper()
	id := uuid.New()
	if _, err := c.pool.Exec(c.ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name, size, mime)
		 VALUES ($1, $2, $3, 'file', $4, $5, 'text/plain')`,
		id, c.owner, parent, name, size); err != nil {
		c.t.Fatalf("seeding file: %v", err)
	}
	return id
}

func (c *trashClient) decodeNode(rec *httptest.ResponseRecorder) NodeDTO {
	c.t.Helper()
	var n NodeDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
		c.t.Fatalf("response is not a node: %v (body %q)", err, rec.Body.String())
	}
	return n
}

func (c *trashClient) decodeList(rec *httptest.ResponseRecorder) List[NodeDTO] {
	c.t.Helper()
	var l List[NodeDTO]
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		c.t.Fatalf("response is not a list envelope: %v (body %q)", err, rec.Body.String())
	}
	return l
}

// ------------------------------------------------------------------ cases ----

// The whole round trip over the wire: delete, see it in the trash, restore it.
func TestTrashRestoreRoundTrip(t *testing.T) {
	c := newTrashClient(t)
	folder := c.folder(c.root, "Q1 reports")

	if rec := c.do(http.MethodDelete, "/api/nodes/"+folder.String(), true); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/nodes/{id} = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}

	rec := c.do(http.MethodGet, "/api/trash", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/trash = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	list := c.decodeList(rec)
	if len(list.Items) != 1 || list.Items[0].ID != folder {
		t.Fatalf("trash listing = %+v, want the deleted folder", list.Items)
	}
	if !list.Items[0].TrashedRoot {
		t.Error("the listed entry is not marked trashed_root")
	}
	if list.NextCursor != nil {
		t.Errorf("next_cursor = %v, want null on a single page", *list.NextCursor)
	}

	rec = c.do(http.MethodPost, "/api/nodes/"+folder.String()+"/restore", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST restore = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	restored := c.decodeNode(rec)
	if restored.ID != folder || restored.Name != "Q1 reports" {
		t.Errorf("restored = %+v, want the folder under its own name", restored)
	}
	if restored.Size != nil {
		t.Errorf("folder size = %v, want null", *restored.Size)
	}

	if rec := c.do(http.MethodGet, "/api/trash", true); len(c.decodeList(rec).Items) != 0 {
		t.Error("the restored folder is still in the trash listing")
	}
}

// Restore into a taken name answers 200 with the new name, never a conflict.
func TestRestoreOverTheWireAutoRenames(t *testing.T) {
	c := newTrashClient(t)
	original := c.file(c.root, "report.pdf", 10)

	if rec := c.do(http.MethodDelete, "/api/nodes/"+original.String(), true); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", rec.Code)
	}
	c.file(c.root, "report.pdf", 20)

	rec := c.do(http.MethodPost, "/api/nodes/"+original.String()+"/restore", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := c.decodeNode(rec).Name; got != "report (1).pdf" {
		t.Errorf("restored name = %q, want %q", got, "report (1).pdf")
	}
}

// Purge answers 204 and the node is gone for good.
func TestPurgeOverTheWire(t *testing.T) {
	c := newTrashClient(t)
	file := c.file(c.root, "gone.txt", 10)

	if rec := c.do(http.MethodDelete, "/api/nodes/"+file.String(), true); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", rec.Code)
	}
	if rec := c.do(http.MethodDelete, "/api/nodes/"+file.String()+"/purge", true); rec.Code != http.StatusNoContent {
		t.Fatalf("purge = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}

	var n int
	if err := c.pool.QueryRow(c.ctx, `SELECT count(*) FROM nodes WHERE id = $1`, file).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 0 {
		t.Error("the purged node is still in the database")
	}
	if rec := c.do(http.MethodDelete, "/api/nodes/"+file.String()+"/purge", true); rec.Code != http.StatusNotFound {
		t.Errorf("purging it twice = %d, want 404", rec.Code)
	}
}

// The route-level refusals: anonymous callers, the root folder, ids that name
// nothing the caller can see.
func TestTrashRouteRefusals(t *testing.T) {
	c := newTrashClient(t)
	other := newTrashClient(t)
	theirs := other.file(other.root, "theirs.txt", 10)
	live := c.file(c.root, "live.txt", 10)

	cases := []struct {
		name       string
		method     string
		path       string
		authed     bool
		wantStatus int
		wantCode   string
	}{
		{"trash list without a session", http.MethodGet, "/api/trash", false, http.StatusUnauthorized, CodeUnauthorized},
		{"delete without a session", http.MethodDelete, "/api/nodes/" + live.String(), false, http.StatusUnauthorized, CodeUnauthorized},
		{"restore without a session", http.MethodPost, "/api/nodes/" + live.String() + "/restore", false, http.StatusUnauthorized, CodeUnauthorized},
		{"purge without a session", http.MethodDelete, "/api/nodes/" + live.String() + "/purge", false, http.StatusUnauthorized, CodeUnauthorized},
		{"deleting the root folder", http.MethodDelete, "/api/nodes/" + c.root.String(), true, http.StatusUnprocessableEntity, CodeInvalid},
		{"restoring the root folder", http.MethodPost, "/api/nodes/" + c.root.String() + "/restore", true, http.StatusUnprocessableEntity, CodeInvalid},
		{"purging the root folder", http.MethodDelete, "/api/nodes/" + c.root.String() + "/purge", true, http.StatusUnprocessableEntity, CodeInvalid},
		{"deleting another user's node", http.MethodDelete, "/api/nodes/" + theirs.String(), true, http.StatusNotFound, CodeNotFound},
		{"deleting an id that is not a uuid", http.MethodDelete, "/api/nodes/not-a-uuid", true, http.StatusNotFound, CodeNotFound},
		{"restoring a node that is not in the trash", http.MethodPost, "/api/nodes/" + live.String() + "/restore", true, http.StatusNotFound, CodeNotFound},
	}

	for _, tc := range cases {
		rec := c.do(tc.method, tc.path, tc.authed)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d (body %s)", tc.name, rec.Code, tc.wantStatus, rec.Body.String())
			continue
		}
		if got := decodeErr(t, rec.Body.String()).Code; got != tc.wantCode {
			t.Errorf("%s: code = %q, want %q", tc.name, got, tc.wantCode)
		}
	}

	// The refused delete must not have happened.
	var deleted *string
	if err := c.pool.QueryRow(c.ctx, `SELECT deleted_at::text FROM nodes WHERE id = $1`, theirs).Scan(&deleted); err != nil {
		t.Fatalf("re-reading the other user's node: %v", err)
	}
	if deleted != nil {
		t.Error("the other user's node was trashed by a request that was answered 404")
	}
}

// The trash listing pages with the opaque cursor, and rejects one it did not
// issue.
func TestTrashListPagination(t *testing.T) {
	c := newTrashClient(t)
	for i := range 3 {
		id := c.file(c.root, fmt.Sprintf("f%d.txt", i), 10)
		if rec := c.do(http.MethodDelete, "/api/nodes/"+id.String(), true); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE = %d, want 204", rec.Code)
		}
	}

	rec := c.do(http.MethodGet, "/api/trash?limit=2", true)
	first := c.decodeList(rec)
	if len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("first page = %d items, cursor %v; want 2 and a cursor", len(first.Items), first.NextCursor)
	}

	rec = c.do(http.MethodGet, "/api/trash?limit=2&cursor="+*first.NextCursor, true)
	second := c.decodeList(rec)
	if len(second.Items) != 1 || second.NextCursor != nil {
		t.Fatalf("second page = %d items, cursor %v; want 1 and null", len(second.Items), second.NextCursor)
	}

	rec = c.do(http.MethodGet, "/api/trash?cursor=not-a-cursor", true)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a forged cursor = %d, want 422", rec.Code)
	}
	if got := decodeErr(t, rec.Body.String()).Code; got != CodeInvalid {
		t.Errorf("a forged cursor code = %q, want %q", got, CodeInvalid)
	}
}
