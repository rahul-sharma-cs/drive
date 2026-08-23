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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/db"
	"github.com/rahul-sharma-cs/drive/server/internal/node"
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
	return newTrashClientBudget(t, 0)
}

// newTrashClientBudget is newTrashClient with the bulk routes' wall-clock
// budget replaced. A budget of one nanosecond is already spent by the time the
// first root is looked at, which is how the pending/remaining path is exercised
// without a 25 s test.
func newTrashClientBudget(t *testing.T, budget time.Duration) *trashClient {
	t.Helper()
	pool := trashTestPool(t)
	srv := New(&config.Config{}, pool, nil, nil, nil, nil)
	srv.BulkBudget = budget
	c := &trashClient{
		t:    t,
		ctx:  context.Background(),
		pool: pool,
		h:    srv.Routes(),
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

// ------------------------------------------------------------ bulk trash -----

// rebudget swaps the client's handler for one with a different bulk budget,
// keeping the same user and session: the same browser calling again, at a
// server that is not out of time.
func (c *trashClient) rebudget(budget time.Duration) {
	c.t.Helper()
	srv := New(&config.Config{}, c.pool, nil, nil, nil, nil)
	srv.BulkBudget = budget
	c.h = srv.Routes()
}

// send issues a request with a JSON body through the whole chain, as the
// signed-in user.
func (c *trashClient) send(method, path string, body any) *httptest.ResponseRecorder {
	c.t.Helper()
	return authDo(c.t, c.h, method, path, body, c.cookie)
}

func (c *trashClient) decodeBulk(rec *httptest.ResponseRecorder) BulkResponse {
	c.t.Helper()
	var b BulkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		c.t.Fatalf("response is not a bulk envelope: %v (body %q)", err, rec.Body.String())
	}
	return b
}

func (c *trashClient) decodeEmpty(rec *httptest.ResponseRecorder) emptyTrashResponse {
	c.t.Helper()
	var e emptyTrashResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		c.t.Fatalf("response is not an empty-trash envelope: %v (body %q)", err, rec.Body.String())
	}
	return e
}

// trash moves a node into the trash over the wire, which is how every fixture
// below gets a trashed root.
func (c *trashClient) trash(id uuid.UUID) {
	c.t.Helper()
	if rec := c.do(http.MethodDelete, "/api/nodes/"+id.String(), true); rec.Code != http.StatusNoContent {
		c.t.Fatalf("DELETE /api/nodes/%s = %d, want 204 (body %s)", id, rec.Code, rec.Body.String())
	}
}

// seedTrashedFiles inserts n already-trashed files in one statement -- the same
// row state Trash produces -- because 250 of them one wire call at a time is
// several seconds of nothing being tested. Each gets its own deleted_at so the
// oldest-first paging order is total.
func (c *trashClient) seedTrashedFiles(n int) {
	c.t.Helper()
	if _, err := c.pool.Exec(c.ctx, `
		INSERT INTO nodes (id, owner_id, parent_id, kind, name, size, mime, deleted_at, updated_at, trashed_root)
		SELECT gen_random_uuid(), $1, $2, 'file', 'bulk-' || i || '.txt', 10, 'text/plain',
		       now() - make_interval(secs => i), now(), true
		  FROM generate_series(1, $3) AS i`, c.owner, c.root, n); err != nil {
		c.t.Fatalf("seeding %d trashed files: %v", n, err)
	}
}

func (c *trashClient) trashedCount() int {
	c.t.Helper()
	var n int
	if err := c.pool.QueryRow(c.ctx,
		`SELECT count(*) FROM nodes WHERE owner_id = $1 AND trashed_root AND deleted_at IS NOT NULL`,
		c.owner).Scan(&n); err != nil {
		c.t.Fatalf("counting trashed roots: %v", err)
	}
	return n
}

func (c *trashClient) isLive(id uuid.UUID) bool {
	c.t.Helper()
	var live bool
	if err := c.pool.QueryRow(c.ctx,
		`SELECT deleted_at IS NULL FROM nodes WHERE id = $1`, id).Scan(&live); err != nil {
		c.t.Fatalf("reading node %s: %v", id, err)
	}
	return live
}

// Every id gets its own answer, and one id's failure leaves the others alone.
// That is the whole reason the route is a loop over single-id restores instead
// of one transaction: a not_found in the middle of a selection is normal (a
// second tab emptied the trash), and it is not a reason to leave the other
// nineteen files in the bin.
func TestBulkRestoreReportsPerIDOutcomes(t *testing.T) {
	c := newTrashClient(t)
	first := c.file(c.root, "first.txt", 10)
	last := c.file(c.root, "last.txt", 20)
	c.trash(first)
	c.trash(last)
	missing := uuid.New()

	// The miss sits between two restorable ids on purpose: an all-or-nothing
	// implementation loses the one before it as well as the one after.
	rec := c.send(http.MethodPost, "/api/trash/restore",
		map[string]any{"ids": []uuid.UUID{first, missing, last}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/trash/restore = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	got := c.decodeBulk(rec)
	want := []BulkResult{
		{ID: first, Status: "ok"},
		{ID: missing, Status: "not_found"},
		{ID: last, Status: "ok"},
	}
	if len(got.Results) != len(want) {
		t.Fatalf("results = %+v, want one per id: %+v", got.Results, want)
	}
	for i, w := range want {
		if got.Results[i] != w {
			t.Errorf("results[%d] = %+v, want %+v", i, got.Results[i], w)
		}
	}
	if got.Remaining {
		t.Error("remaining = true, but every id was answered")
	}

	if !c.isLive(first) {
		t.Error("the id before the miss was not restored")
	}
	if !c.isLive(last) {
		t.Error("the id after the miss was not restored")
	}
	if n := c.trashedCount(); n != 0 {
		t.Errorf("%d roots left in the trash, want 0", n)
	}
}

// A bulk purge is a loop over the single-id purge, so it inherits its
// contract with in-flight uploads: the session survives with its destination
// nulled (the FK is ON DELETE SET NULL) and finalize re-parents the finished
// file to the owner's root. Aborting it here would throw away a fully uploaded
// file.
func TestBulkPurgeRehomesInFlightUploads(t *testing.T) {
	c := newTrashClient(t)
	folder := c.folder(c.root, "Destination")
	c.trash(folder)

	sessionID := uuid.New()
	if _, err := c.pool.Exec(c.ctx, `
		INSERT INTO upload_sessions
		    (id, user_id, parent_id, file_name, file_size, fingerprint,
		     object_key, part_size, parts_total, status, expires_at)
		VALUES ($1, $2, $3, 'big.bin', 1048576, 'fp', $4, 10485760, 1, 'active',
		        now() + interval '7 days')`,
		sessionID, c.owner, folder, "blobs/"+uuid.NewString()); err != nil {
		t.Fatalf("seeding upload session: %v", err)
	}

	rec := c.send(http.MethodPost, "/api/trash/purge", map[string]any{"ids": []uuid.UUID{folder}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/trash/purge = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := c.decodeBulk(rec); len(got.Results) != 1 || got.Results[0].Status != "ok" {
		t.Fatalf("results = %+v, want one ok", got.Results)
	}

	var status string
	var parentID *uuid.UUID
	if err := c.pool.QueryRow(c.ctx,
		`SELECT status, parent_id FROM upload_sessions WHERE id = $1`, sessionID).
		Scan(&status, &parentID); err != nil {
		t.Fatalf("reading upload session: %v", err)
	}
	if status != "active" {
		t.Errorf("upload session status = %q, want %q", status, "active")
	}
	if parentID != nil {
		t.Errorf("upload session parent_id = %v, want NULL", *parentID)
	}
}

// Emptying the trash covers every root, not just the first page, and picks up
// where it left off when it stops early.
func TestEmptyTrashPurgesEveryRootAndResumes(t *testing.T) {
	const roots = 250 // more than one emptyTrashPage

	// Stopping early: a spent budget purges nothing, says so, and leaves the
	// trash exactly as it found it.
	c := newTrashClientBudget(t, time.Nanosecond)
	c.seedTrashedFiles(roots)

	rec := c.do(http.MethodDelete, "/api/trash", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/trash = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := c.decodeEmpty(rec); got.Purged != 0 || !got.Remaining {
		t.Errorf("out-of-budget response = %+v, want {purged:0, remaining:true}", got)
	}
	if n := c.trashedCount(); n != roots {
		t.Errorf("%d roots left in the trash, want all %d still there", n, roots)
	}

	// Resuming: the same trash and the same session, a server with its budget
	// back, one call, everything gone -- including the roots past the first
	// page of 200.
	c.rebudget(0)
	rec = c.do(http.MethodDelete, "/api/trash", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/trash = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := c.decodeEmpty(rec); got.Purged != roots || got.Remaining {
		t.Errorf("response = %+v, want {purged:%d, remaining:false} -- every page, not just the first", got, roots)
	}
	if n := c.trashedCount(); n != 0 {
		t.Errorf("%d roots left in the trash, want 0", n)
	}

	// Idempotent: emptying an empty trash is a 200 that did nothing.
	rec = c.do(http.MethodDelete, "/api/trash", true)
	if got := c.decodeEmpty(rec); got.Purged != 0 || got.Remaining {
		t.Errorf("emptying an empty trash = %+v, want {purged:0, remaining:false}", got)
	}
}

// The budget is what keeps a bulk call inside a platform's edge timeout: ids it
// never reached come back pending with remaining=true, and the client loops.
// Nothing is half-done -- a pending id was not touched at all.
func TestBulkBudgetAnswersPending(t *testing.T) {
	c := newTrashClientBudget(t, time.Nanosecond)
	ids := []uuid.UUID{
		c.file(c.root, "a.txt", 10),
		c.file(c.root, "b.txt", 10),
		c.file(c.root, "c.txt", 10),
	}
	for _, id := range ids {
		c.trash(id)
	}

	rec := c.send(http.MethodPost, "/api/trash/purge", map[string]any{"ids": ids})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/trash/purge = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got := c.decodeBulk(rec)
	if !got.Remaining {
		t.Error("remaining = false, but the budget ran out before the first id")
	}
	if len(got.Results) != len(ids) {
		t.Fatalf("results = %+v, want one per id", got.Results)
	}
	for i, res := range got.Results {
		if res.Status != "pending" {
			t.Errorf("results[%d] = %+v, want pending", i, res)
		}
	}
	if n := c.trashedCount(); n != len(ids) {
		t.Errorf("%d roots left in the trash, want all %d -- a pending id must not have been purged", n, len(ids))
	}

	// The same ids with a real budget: every one of them ok, and gone.
	c.rebudget(0)
	rec = c.send(http.MethodPost, "/api/trash/purge", map[string]any{"ids": ids})
	got = c.decodeBulk(rec)
	if got.Remaining {
		t.Error("remaining = true with the whole budget available")
	}
	for i, res := range got.Results {
		if res.Status != "ok" {
			t.Errorf("results[%d] = %+v, want ok", i, res)
		}
	}
	if n := c.trashedCount(); n != 0 {
		t.Errorf("%d roots left in the trash, want 0", n)
	}
}

// The bulk routes' refusals: no session, too many ids, none at all, a body that
// is not one of ours.
func TestBulkRouteRefusals(t *testing.T) {
	c := newTrashClient(t)
	id := c.file(c.root, "one.txt", 10)
	c.trash(id)

	tooMany := make([]uuid.UUID, BulkIDLimit+1)
	for i := range tooMany {
		tooMany[i] = uuid.New()
	}

	cases := []struct {
		name       string
		path       string
		body       any
		cookie     *http.Cookie
		wantStatus int
		wantCode   string
	}{
		{"restore without a session", "/api/trash/restore", map[string]any{"ids": []uuid.UUID{id}}, nil, http.StatusUnauthorized, CodeUnauthorized},
		{"purge without a session", "/api/trash/purge", map[string]any{"ids": []uuid.UUID{id}}, nil, http.StatusUnauthorized, CodeUnauthorized},
		{"more ids than the limit", "/api/trash/restore", map[string]any{"ids": tooMany}, c.cookie, http.StatusUnprocessableEntity, CodeInvalid},
		{"no ids at all", "/api/trash/purge", map[string]any{"ids": []uuid.UUID{}}, c.cookie, http.StatusUnprocessableEntity, CodeInvalid},
		{"an id that is not a uuid", "/api/trash/purge", map[string]any{"ids": []string{"nope"}}, c.cookie, http.StatusUnprocessableEntity, CodeInvalid},
		{"a field we do not accept", "/api/trash/purge", map[string]any{"ids": []uuid.UUID{id}, "force": true}, c.cookie, http.StatusUnprocessableEntity, CodeInvalid},
	}

	for _, tc := range cases {
		rec := authDo(t, c.h, http.MethodPost, tc.path, tc.body, tc.cookie)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d (body %s)", tc.name, rec.Code, tc.wantStatus, rec.Body.String())
			continue
		}
		if got := decodeErr(t, rec.Body.String()).Code; got != tc.wantCode {
			t.Errorf("%s: code = %q, want %q", tc.name, got, tc.wantCode)
		}
	}

	// Emptying the trash needs a session too.
	if rec := c.do(http.MethodDelete, "/api/trash", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("DELETE /api/trash anonymously = %d, want 401", rec.Code)
	}
	if n := c.trashedCount(); n != 1 {
		t.Errorf("%d roots in the trash, want the one every refused request left alone", n)
	}
}

// deleted_at is the trash listing's own column -- "deleted 3 days ago" is the
// question a trash screen answers. A live listing has no such moment, and its
// rows must not grow the field: NodeDTOFrom leaves it nil so every other
// response serializes exactly as it did before.
func TestTrashDTOCarriesDeletedAtAndChildrenDoNot(t *testing.T) {
	c := newTrashClient(t)
	file := c.file(c.root, "receipt.pdf", 10)
	c.folder(c.root, "Keep")
	before := time.Now()
	c.trash(file)

	rec := c.do(http.MethodGet, "/api/trash", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/trash = %d, want 200", rec.Code)
	}
	list := c.decodeList(rec)
	if len(list.Items) != 1 {
		t.Fatalf("trash listing = %d items, want 1", len(list.Items))
	}
	got := list.Items[0]
	if got.DeletedAt == nil {
		t.Fatal("the trashed item carries no deleted_at")
	}
	if got.DeletedAt.Before(before.Add(-time.Minute)) || got.DeletedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("deleted_at = %s, want the moment it was trashed (~%s)", got.DeletedAt, before)
	}
	if got.DeletedAt.Equal(got.UpdatedAt) && got.UpdatedAt.IsZero() {
		t.Error("deleted_at looks like a zero value, not a timestamp")
	}

	// The live listing's rows do not carry the field at all -- not null, absent.
	rec = c.do(http.MethodGet, "/api/nodes/"+c.root.String()+"/children", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET children = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding children: %v", err)
	}
	if len(raw.Items) == 0 {
		t.Fatal("the folder listed no children")
	}
	for _, item := range raw.Items {
		if v, ok := item["deleted_at"]; ok {
			t.Errorf("a live child carries deleted_at = %v; the field belongs to the trash listing only", v)
		}
	}

	// The two converters, handed the same trashed row. Only the trash's fills
	// the field in.
	//
	// The wire cannot make this assertion on its own: a live listing never
	// returns a trashed row, so a live converter that filled deleted_at in
	// would produce identical bytes for every response there is. The rule is
	// about which converter owns the field, so it is tested where the
	// converters are.
	when := time.Now().Add(-3 * time.Hour)
	row := node.Node{
		ID: uuid.New(), Kind: node.KindFile, Name: "receipt.pdf",
		CreatedAt: when, UpdatedAt: when, DeletedAt: &when, TrashedRoot: true,
	}
	if got := itemDTO(row).DeletedAt; got == nil || !got.Equal(when) {
		t.Errorf("itemDTO deleted_at = %v, want %s -- the trash listing's own column", got, when)
	}
	if got := NodeDTOFrom(row).DeletedAt; got != nil {
		t.Errorf("NodeDTOFrom deleted_at = %s, want nil -- live responses must stay byte-identical", got)
	}
}

// ------------------------------------------------ the sweep under contention --

// Roots do not only leave the trash by this handler's hand. The hourly GC
// purges, a second tab empties, a phone restores -- and each of those turns a
// root the sweep has already listed into something its purge cannot find. The
// two cases below arrange exactly that, using a row lock in place of the
// timing: the handler reads its page, blocks inside Purge, and the test changes
// the world while it waits there.

// lockNodes takes a row lock on ids so the next Purge of any of them blocks,
// and hands back the transaction holding it. The caller commits or rolls back.
func (c *trashClient) lockNodes(ids []uuid.UUID) pgx.Tx {
	c.t.Helper()
	tx, err := c.pool.Begin(c.ctx)
	if err != nil {
		c.t.Fatalf("opening the locking transaction: %v", err)
	}
	if _, err := tx.Exec(c.ctx, `SELECT id FROM nodes WHERE id = ANY($1) FOR UPDATE`, ids); err != nil {
		_ = tx.Rollback(c.ctx)
		c.t.Fatalf("locking %d nodes: %v", len(ids), err)
	}
	return tx
}

// waitForABlockedPurge waits until a backend is stuck on one of those locks.
//
// It is the synchronization these cases rest on, and it is also what stops them
// being vacuous: SELECT ... FOR UPDATE is reached only after the page of roots
// has been read, so a backend waiting on one is proof that the sweep read the
// page this test arranged. Without it, a test that changed the rows too early
// would simply be watching the handler read the rows it had left behind, and
// would pass for the wrong reason.
func (c *trashClient) waitForABlockedPurge() {
	c.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := c.pool.QueryRow(c.ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND pid <> pg_backend_pid()
			   AND state = 'active'
			   AND wait_event_type = 'Lock'
			   AND query LIKE '%FOR UPDATE%'`).Scan(&n); err != nil {
			c.t.Fatalf("reading pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatal("no backend ever blocked on a node row lock, so the sweep never reached the rows under test")
}

// sendAsync serves a request on its own goroutine and hands back a channel
// carrying the recorder. The request is built here rather than there so nothing
// on that goroutine ever calls t.Fatalf.
func (c *trashClient) sendAsync(ctx context.Context, method, path string) <-chan *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("")).WithContext(ctx)
	req.Header.Set(ClientHeader, "web")
	req.AddCookie(c.cookie)

	h, done := c.h, make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		done <- rec
	}()
	return done
}

// A page whose roots all vanished before the sweep reached them is not the end
// of the trash -- it is one page of a list that still has rows behind it.
//
// Counting only successful purges made the handler stop on such a page and
// answer {purged:0, remaining:false}, which leaves everything past the first
// 200 in the bin and tells the client there is nothing left to loop for. The
// hourly GC is a real actor here, not a hypothetical one.
func TestEmptyTrashCarriesOnPastAPageSomethingElseAlreadyPurged(t *testing.T) {
	const behind = 50

	c := newTrashClient(t)
	c.seedTrashedFiles(emptyTrashPage + behind)

	// The first page, in the order the sweep is about to read it.
	first, err := node.NewStore(c.pool).TrashRootIDs(c.ctx, c.owner, emptyTrashPage)
	if err != nil {
		t.Fatalf("reading the first page of trashed roots: %v", err)
	}
	if len(first) != emptyTrashPage {
		t.Fatalf("the first page is %d roots, want %d", len(first), emptyTrashPage)
	}

	tx := c.lockNodes(first)
	defer tx.Rollback(c.ctx) //nolint:errcheck // rollback after commit is a no-op

	done := c.sendAsync(c.ctx, http.MethodDelete, "/api/trash")
	c.waitForABlockedPurge()

	// The other actor's part: by the time the lock lifts, that whole page is
	// gone and every root on it is a miss.
	if _, err := tx.Exec(c.ctx, `DELETE FROM nodes WHERE id = ANY($1)`, first); err != nil {
		t.Fatalf("purging the first page out from under the sweep: %v", err)
	}
	if err := tx.Commit(c.ctx); err != nil {
		t.Fatalf("committing the concurrent purge: %v", err)
	}

	rec := <-done
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/trash = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := c.decodeEmpty(rec); got.Purged != behind || got.Remaining {
		t.Errorf("response = %+v, want {purged:%d, remaining:false} -- the roots behind the vanished page",
			got, behind)
	}
	if n := c.trashedCount(); n != 0 {
		t.Errorf("%d roots left in the trash, want 0", n)
	}
}

// Anything that is not a miss ends the sweep, not the request.
//
// A cancelled request (the user navigated away mid-empty) and a deadlock
// against the GC's own purge both land here, and both used to be a 500 that
// threw away a count the database had already committed -- the client saw an
// error, retried from scratch, and the response never said how far it got. The
// contract is one shape: 200 {purged, remaining}, and the client loops.
func TestEmptyTrashReportsWhatItPurgedWhenTheSweepIsCutShort(t *testing.T) {
	const (
		roots  = 5
		stopAt = 3 // the 1-based root the request dies on
	)

	c := newTrashClient(t)
	c.seedTrashedFiles(roots)

	page, err := node.NewStore(c.pool).TrashRootIDs(c.ctx, c.owner, roots)
	if err != nil {
		t.Fatalf("reading the trashed roots: %v", err)
	}
	if len(page) != roots {
		t.Fatalf("the page is %d roots, want %d", len(page), roots)
	}

	tx := c.lockNodes(page[stopAt-1 : stopAt])
	defer tx.Rollback(c.ctx) //nolint:errcheck // rollback after commit is a no-op

	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()

	done := c.sendAsync(ctx, http.MethodDelete, "/api/trash")
	c.waitForABlockedPurge() // the first two are purged and committed; the third is waiting
	cancel()

	rec := <-done
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/trash = %d, want 200 even though the sweep was cut short (body %s)",
			rec.Code, rec.Body.String())
	}
	got := c.decodeEmpty(rec)
	if got.Purged != stopAt-1 || !got.Remaining {
		t.Errorf("response = %+v, want {purged:%d, remaining:true}", got, stopAt-1)
	}
	if n := c.trashedCount(); n != roots-(stopAt-1) {
		t.Errorf("%d roots left in the trash, want %d -- what was purged before the interruption is committed",
			n, roots-(stopAt-1))
	}
}

// The same id twice is one selected row, not two operations. It is deduplicated
// before the id count is checked, so a selection that is inside the limit is
// never refused for having repeats in it.
func TestBulkPurgeDeduplicatesBeforeCountingIDs(t *testing.T) {
	c := newTrashClient(t)
	id := c.file(c.root, "twice.txt", 10)
	c.trash(id)

	rec := c.send(http.MethodPost, "/api/trash/purge", map[string]any{"ids": []uuid.UUID{id, id, id}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/trash/purge = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got := c.decodeBulk(rec)
	if len(got.Results) != 1 {
		t.Fatalf("results = %+v, want exactly one -- the id was sent three times and purged once", got.Results)
	}
	if got.Results[0].ID != id || got.Results[0].Status != bulkOK {
		t.Errorf("results[0] = %+v, want {%s ok}", got.Results[0], id)
	}

	// One over the limit, with one repeat in it: BulkIDLimit distinct ids, which
	// is the number the contract bounds.
	ids := make([]uuid.UUID, 0, BulkIDLimit+1)
	for i := 0; i < BulkIDLimit; i++ {
		ids = append(ids, uuid.New())
	}
	ids = append(ids, ids[0])

	rec = c.send(http.MethodPost, "/api/trash/purge", map[string]any{"ids": ids})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/trash/purge with %d ids and one repeat = %d, want 200 (body %s)",
			len(ids), rec.Code, rec.Body.String())
	}
	if got := c.decodeBulk(rec); len(got.Results) != BulkIDLimit {
		t.Errorf("results = %d, want %d (one per distinct id)", len(got.Results), BulkIDLimit)
	}
}
