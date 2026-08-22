package api

// Node CRUD against the real drive-test Postgres, through the whole middleware
// chain -- real session cookies, real RequireAuth, real SQL. The authorization
// rows are the point of this file: an IDOR that only unit tests catch is an
// IDOR that ships.
//
// The database is shared with the other suites in this package, so every test
// works inside users it creates itself and never truncates anything.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/node"
)

// ---------------------------------------------------------------- harness ----

// nodeTestServer builds the real router over the drive-test database. It
// reuses authTestPool so the package connects and migrates once.
func nodeTestServer(t *testing.T) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := authTestPool(t)
	return New(&config.Config{BaseURL: authTestBaseURL}, pool, nil, nil, nil, nil).Routes(), pool
}

// nodeUser is a signed-in account: its id, its root folder, and a cookie that
// resolves to a live session row.
type nodeUser struct {
	ID     uuid.UUID
	RootID uuid.UUID
	Cookie *http.Cookie
}

// nodeNewUser inserts a verified user, their root folder and one auth session
// straight into the database. Signup is the auth agent's endpoint; these tests
// only need an authenticated caller.
func nodeNewUser(t *testing.T, pool *pgxpool.Pool) nodeUser {
	t.Helper()
	ctx := context.Background()

	u := nodeUser{ID: uuid.New(), RootID: uuid.New()}
	email := "node-" + uuid.NewString() + "@drive.test"

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx,
		`INSERT INTO users (id, email, password_hash, display_name, email_verified_at)
		 VALUES ($1, $2, 'x', 'Node Test', now())`, u.ID, email); err != nil {
		t.Fatalf("inserting user: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		 VALUES ($1, $2, NULL, 'folder', 'My Drive')`, u.RootID, u.ID); err != nil {
		t.Fatalf("inserting root folder: %v", err)
	}

	token := uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	if _, err := tx.Exec(ctx,
		`INSERT INTO auth_sessions (id, user_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, now() + interval '1 day')`, uuid.New(), u.ID, sum[:]); err != nil {
		t.Fatalf("inserting session: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	u.Cookie = &http.Cookie{Name: SessionCookie, Value: token}
	return u
}

// nodeMkFolder inserts a folder directly, for the fixtures a test needs before
// the endpoint under test runs.
func nodeMkFolder(t *testing.T, pool *pgxpool.Pool, owner nodeUser, parent uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name) VALUES ($1, $2, $3, 'folder', $4)`,
		id, owner.ID, parent, name); err != nil {
		t.Fatalf("inserting folder %q: %v", name, err)
	}
	return id
}

// nodeMkFile inserts a file plus the blob it points at, the way the seed
// helper and a finished upload do.
func nodeMkFile(t *testing.T, pool *pgxpool.Pool, owner nodeUser, parent uuid.UUID, name string) (nodeID, blobID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	nodeID, blobID = uuid.New(), uuid.New()

	if _, err := pool.Exec(ctx,
		`INSERT INTO blobs (id, object_key, size, etag) VALUES ($1, $2, 11, 'etag')`,
		blobID, "blobs/"+blobID.String()); err != nil {
		t.Fatalf("inserting blob: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name, blob_id, size, mime)
		 VALUES ($1, $2, $3, 'file', $4, $5, 11, 'text/plain')`,
		nodeID, owner.ID, parent, name, blobID); err != nil {
		t.Fatalf("inserting file %q: %v", name, err)
	}
	return nodeID, blobID
}

func nodeRefcount(t *testing.T, pool *pgxpool.Pool, blobID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT refcount FROM blobs WHERE id = $1`, blobID).Scan(&n); err != nil {
		t.Fatalf("reading refcount: %v", err)
	}
	return n
}

type nodeRow struct {
	ParentID    *uuid.UUID
	Name        string
	DeletedAt   *time.Time
	TrashedRoot bool
}

func nodeReload(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) nodeRow {
	t.Helper()
	var row nodeRow
	if err := pool.QueryRow(context.Background(),
		`SELECT parent_id, name, deleted_at, trashed_root FROM nodes WHERE id = $1`, id).
		Scan(&row.ParentID, &row.Name, &row.DeletedAt, &row.TrashedRoot); err != nil {
		t.Fatalf("reloading node: %v", err)
	}
	return row
}

func nodeChildCount(t *testing.T, pool *pgxpool.Pool, parent uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM nodes WHERE parent_id = $1 AND deleted_at IS NULL`, parent).Scan(&n); err != nil {
		t.Fatalf("counting children: %v", err)
	}
	return n
}

func nodeDecode(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decoding %d response %q: %v", rec.Code, rec.Body.String(), err)
	}
}

func nodeWant(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	if code == "" {
		return
	}
	var e ErrorBody
	nodeDecode(t, rec, &e)
	if e.Code != code {
		t.Fatalf("error code = %q, want %q (body %s)", e.Code, code, rec.Body.String())
	}
}

// ---------------------------------------------------------------- reading ----

func TestGetNodeIsOwnerOnly(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)
	folderID := nodeMkFolder(t, pool, owner, owner.RootID, "Docs")

	rec := authDo(t, h, http.MethodGet, "/api/nodes/"+folderID.String(), nil, owner.Cookie)
	nodeWant(t, rec, http.StatusOK, "")
	var dto NodeDTO
	nodeDecode(t, rec, &dto)
	if dto.ID != folderID || dto.Name != "Docs" || dto.Kind != node.KindFolder {
		t.Errorf("node DTO = %+v", dto)
	}
	if dto.Size != nil {
		t.Errorf("folder size = %v, want null", *dto.Size)
	}

	// Someone else's node, a node that never existed and a malformed id are
	// indistinguishable from outside.
	for _, path := range []string{
		"/api/nodes/" + folderID.String(),
		"/api/nodes/" + uuid.NewString(),
		"/api/nodes/not-a-uuid",
	} {
		rec := authDo(t, h, http.MethodGet, path, nil, stranger.Cookie)
		nodeWant(t, rec, http.StatusNotFound, CodeNotFound)
	}

	// Anonymous callers are rejected before any of that.
	rec = authDo(t, h, http.MethodGet, "/api/nodes/"+folderID.String(), nil, nil)
	nodeWant(t, rec, http.StatusUnauthorized, CodeUnauthorized)
}

func TestChildrenOrderingAndPaging(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	parent := nodeMkFolder(t, pool, owner, owner.RootID, "Parent")

	// Inserted deliberately out of order: folders first then name is the
	// server's job, not the insertion order's.
	nodeMkFile(t, pool, owner, parent, "zebra.txt")
	nodeMkFile(t, pool, owner, parent, "apple.txt")
	nodeMkFolder(t, pool, owner, parent, "Beta")
	nodeMkFolder(t, pool, owner, parent, "alpha")

	want := []string{"alpha", "Beta", "apple.txt", "zebra.txt"}

	var got []string
	cursor := ""
	for page := 0; page < 5; page++ {
		path := "/api/nodes/" + parent.String() + "/children?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := authDo(t, h, http.MethodGet, path, nil, owner.Cookie)
		nodeWant(t, rec, http.StatusOK, "")

		var list List[NodeDTO]
		nodeDecode(t, rec, &list)
		for _, item := range list.Items {
			got = append(got, item.Name)
		}
		if list.NextCursor == nil {
			break
		}
		cursor = *list.NextCursor
	}

	if len(got) != len(want) {
		t.Fatalf("children = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("children = %v, want %v", got, want)
		}
	}

	// A cursor we did not mint is a validation error, not a 500.
	rec := authDo(t, h, http.MethodGet, "/api/nodes/"+parent.String()+"/children?cursor=zzzz", nil, owner.Cookie)
	nodeWant(t, rec, http.StatusUnprocessableEntity, CodeInvalid)

	// Listing someone else's folder is a miss.
	stranger := nodeNewUser(t, pool)
	rec = authDo(t, h, http.MethodGet, "/api/nodes/"+parent.String()+"/children", nil, stranger.Cookie)
	nodeWant(t, rec, http.StatusNotFound, CodeNotFound)
}

// ------------------------------------------------------------------ sorting --

// nodeMkSortable inserts one child with the exact size and updated_at a sort
// case needs. A folder keeps size NULL, which is the case coalesce(size, 0) is
// there for.
func nodeMkSortable(t *testing.T, pool *pgxpool.Pool, owner nodeUser, parent uuid.UUID, kind, name string, size *int64, age time.Duration) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name, size, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, now() - make_interval(secs => $7))`,
		id, owner.ID, parent, kind, name, size, age.Seconds()); err != nil {
		t.Fatalf("inserting %s %q: %v", kind, name, err)
	}
	return id
}

func nodeSize(n int64) *int64 { return &n }

// nodePageAll walks the children endpoint to exhaustion with the given query
// and returns the names in order. A row seen twice fails on the spot: a keyset
// that repeats rows is the exact failure a broken cursor produces, and a test
// that only checked the final set would pass through it.
func nodePageAll(t *testing.T, h http.Handler, owner nodeUser, parent uuid.UUID, query string, limit int) []string {
	t.Helper()
	var (
		names  []string
		seen   = map[string]bool{}
		cursor string
	)
	for page := 0; ; page++ {
		if page > 20 {
			t.Fatalf("still paging after %d pages of %d: %v", page, limit, names)
		}
		path := fmt.Sprintf("/api/nodes/%s/children?limit=%d&%s", parent, limit, query)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := authDo(t, h, http.MethodGet, path, nil, owner.Cookie)
		nodeWant(t, rec, http.StatusOK, "")

		var list List[NodeDTO]
		nodeDecode(t, rec, &list)
		for _, item := range list.Items {
			if seen[item.Name] {
				t.Fatalf("%q came back on two different pages of %s", item.Name, query)
			}
			seen[item.Name] = true
			names = append(names, item.Name)
		}
		if list.NextCursor == nil {
			return names
		}
		cursor = *list.NextCursor
	}
}

func nodeSameOrder(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

// Sorting by date, in both directions, across pages. Folders lead in both:
// dir orders within a kind, it does not interleave the kinds -- which is why
// the rank term of the keyset and of the ORDER BY is always ascending.
func TestChildrenSortByUpdatedAt(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	parent := nodeMkFolder(t, pool, owner, owner.RootID, "Parent")

	nodeMkSortable(t, pool, owner, parent, node.KindFolder, "Older folder", nil, 300*time.Second)
	nodeMkSortable(t, pool, owner, parent, node.KindFolder, "Newer folder", nil, 10*time.Second)
	nodeMkSortable(t, pool, owner, parent, node.KindFile, "oldest.txt", nodeSize(5), 200*time.Second)
	nodeMkSortable(t, pool, owner, parent, node.KindFile, "middle.txt", nodeSize(500), 100*time.Second)
	nodeMkSortable(t, pool, owner, parent, node.KindFile, "newest.txt", nodeSize(50), 20*time.Second)

	// limit=2 over five rows: every page boundary is crossed by the keyset,
	// including the folder-to-file one.
	got := nodePageAll(t, h, owner, parent, "sort=updated_at&dir=desc", 2)
	nodeSameOrder(t, "updated_at desc", got, []string{
		"Newer folder", "Older folder", "newest.txt", "middle.txt", "oldest.txt",
	})

	got = nodePageAll(t, h, owner, parent, "sort=updated_at&dir=asc", 2)
	nodeSameOrder(t, "updated_at asc", got, []string{
		"Older folder", "Newer folder", "oldest.txt", "middle.txt", "newest.txt",
	})
}

// Sorting by size. Folders have none -- the column is NULL by design -- and the
// listing still leads with them, largest-first included, because rank is
// settled before coalesce(size, 0) is ever compared.
func TestChildrenSortBySizeKeepsFoldersFirst(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	parent := nodeMkFolder(t, pool, owner, owner.RootID, "Parent")

	nodeMkSortable(t, pool, owner, parent, node.KindFolder, "Alpha folder", nil, time.Second)
	nodeMkSortable(t, pool, owner, parent, node.KindFolder, "Zulu folder", nil, time.Second)
	nodeMkSortable(t, pool, owner, parent, node.KindFile, "big.bin", nodeSize(900), time.Second)
	nodeMkSortable(t, pool, owner, parent, node.KindFile, "mid.bin", nodeSize(50), time.Second)
	nodeMkSortable(t, pool, owner, parent, node.KindFile, "small.bin", nodeSize(1), time.Second)

	got := nodePageAll(t, h, owner, parent, "sort=size&dir=desc", 2)
	nodeSameOrder(t, "size desc", got, []string{
		// The two folders tie at 0 and fall through to the name tiebreaker,
		// which runs the same way as the key.
		"Zulu folder", "Alpha folder", "big.bin", "mid.bin", "small.bin",
	})

	got = nodePageAll(t, h, owner, parent, "sort=size&dir=asc", 2)
	nodeSameOrder(t, "size asc", got, []string{
		"Alpha folder", "Zulu folder", "small.bin", "mid.bin", "big.bin",
	})
}

// A cursor is a position in one ordering. Replaying a size cursor under
// sort=name would compare a byte count against a name: the second page would
// repeat rows the client already has, silently. It is a 422 instead.
func TestChildrenCursorFromAnotherSortIsRejected(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	parent := nodeMkFolder(t, pool, owner, owner.RootID, "Parent")

	for i, name := range []string{"a.bin", "b.bin", "c.bin"} {
		nodeMkSortable(t, pool, owner, parent, node.KindFile, name, nodeSize(int64(10*(i+1))), time.Second)
	}

	rec := authDo(t, h, http.MethodGet,
		"/api/nodes/"+parent.String()+"/children?limit=1&sort=size&dir=desc", nil, owner.Cookie)
	nodeWant(t, rec, http.StatusOK, "")
	var first List[NodeDTO]
	nodeDecode(t, rec, &first)
	if first.NextCursor == nil {
		t.Fatal("the first page of three rows handed back no cursor")
	}
	cursor := url.QueryEscape(*first.NextCursor)

	for _, tc := range []struct{ name, query string }{
		{"a size cursor under sort=name", "sort=name&dir=desc"},
		{"a size cursor under the other direction", "sort=size&dir=asc"},
		{"a size cursor with no sort at all (which means name asc)", ""},
	} {
		path := fmt.Sprintf("/api/nodes/%s/children?limit=1&%s&cursor=%s", parent, tc.query, cursor)
		rec := authDo(t, h, http.MethodGet, path, nil, owner.Cookie)
		nodeWant(t, rec, http.StatusUnprocessableEntity, CodeInvalid)
	}

	// The same cursor under the ordering that minted it still pages.
	path := fmt.Sprintf("/api/nodes/%s/children?limit=1&sort=size&dir=desc&cursor=%s", parent, cursor)
	rec = authDo(t, h, http.MethodGet, path, nil, owner.Cookie)
	nodeWant(t, rec, http.StatusOK, "")
	var second List[NodeDTO]
	nodeDecode(t, rec, &second)
	if len(second.Items) != 1 || second.Items[0].Name != "b.bin" {
		t.Fatalf("second page = %+v, want just b.bin", second.Items)
	}
}

// A sort key or direction outside the vocabulary is a 422, never a fallback to
// the default and never SQL.
func TestChildrenSortParamRefusals(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	parent := nodeMkFolder(t, pool, owner, owner.RootID, "Parent")

	for _, query := range []string{
		"sort=created_at",
		"sort=" + url.QueryEscape("name; DROP TABLE nodes"),
		"dir=sideways",
		"dir=DESC",
		"sort=size&dir=descending",
	} {
		rec := authDo(t, h, http.MethodGet,
			"/api/nodes/"+parent.String()+"/children?"+query, nil, owner.Cookie)
		nodeWant(t, rec, http.StatusUnprocessableEntity, CodeInvalid)
	}

	// No sort at all is name ascending, as it always was.
	rec := authDo(t, h, http.MethodGet, "/api/nodes/"+parent.String()+"/children", nil, owner.Cookie)
	nodeWant(t, rec, http.StatusOK, "")
}

// ------------------------------------------------------------ item counts ----

// A folder row in a children page says how much is inside it. The count is of
// live children only -- what the user would see on opening it -- and files
// never carry the field at all.
func TestChildrenItemCountsLiveChildrenOnly(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)
	parent := nodeMkFolder(t, pool, owner, owner.RootID, "Parent")

	docs := nodeMkFolder(t, pool, owner, parent, "Docs")
	nodeMkFile(t, pool, owner, docs, "kept-1.txt")
	nodeMkFile(t, pool, owner, docs, "kept-2.txt")
	binned, _ := nodeMkFile(t, pool, owner, docs, "binned.txt")
	nodeMkFolder(t, pool, owner, parent, "Empty")
	nodeMkFile(t, pool, owner, parent, "loose.txt")

	if rec := authDo(t, h, http.MethodDelete, "/api/nodes/"+binned.String(), nil, owner.Cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("trashing a child = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}

	rec := authDo(t, h, http.MethodGet, "/api/nodes/"+parent.String()+"/children", nil, owner.Cookie)
	nodeWant(t, rec, http.StatusOK, "")

	var raw struct {
		Items []map[string]any `json:"items"`
	}
	nodeDecode(t, rec, &raw)
	// Keyed by the decoded row itself, not by a value pulled out of it: whether
	// the key is there at all is half of what this test asserts.
	rows := map[string]map[string]any{}
	for _, item := range raw.Items {
		name, _ := item["name"].(string)
		rows[name] = item
	}
	if len(rows) != 3 {
		t.Fatalf("the folder listed %v, want Docs, Empty and loose.txt", rows)
	}

	if got := rows["Docs"]["item_count"]; got != float64(2) {
		t.Errorf("Docs item_count = %v, want 2 -- the trashed child is not in the folder", got)
	}
	if got := rows["Empty"]["item_count"]; got != float64(0) {
		t.Errorf("Empty item_count = %v, want 0", got)
	}
	if got, ok := rows["loose.txt"]["item_count"]; ok {
		t.Errorf("a file carries item_count = %v; it has nothing to count", got)
	}

	// The field belongs to the children listing. A single node read is
	// unchanged.
	rec = authDo(t, h, http.MethodGet, "/api/nodes/"+docs.String(), nil, owner.Cookie)
	nodeWant(t, rec, http.StatusOK, "")
	var one map[string]any
	nodeDecode(t, rec, &one)
	if got, ok := one["item_count"]; ok {
		t.Errorf("GET /api/nodes/{id} carries item_count = %v, want the field absent", got)
	}

	// And the count is owner-scoped in the SQL: another user's children can
	// never be counted into it.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name, size, mime)
		 VALUES ($1, $2, $3, 'file', 'theirs.txt', 1, 'text/plain')`,
		uuid.New(), stranger.ID, docs); err != nil {
		t.Fatalf("planting another user's child: %v", err)
	}
	rec = authDo(t, h, http.MethodGet, "/api/nodes/"+parent.String()+"/children", nil, owner.Cookie)
	nodeWant(t, rec, http.StatusOK, "")
	nodeDecode(t, rec, &raw)
	for _, item := range raw.Items {
		if item["name"] == "Docs" && item["item_count"] != float64(2) {
			t.Errorf("Docs item_count = %v after a stranger's row landed under it, want 2", item["item_count"])
		}
	}
}

// ---------------------------------------------------- the authorization matrix

func TestCreateFolderUnderAnotherUsersFolderIsAMiss(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)
	victim := nodeMkFolder(t, pool, owner, owner.RootID, "Private")

	before := nodeChildCount(t, pool, victim)
	rec := authDo(t, h, http.MethodPost, "/api/folders",
		map[string]any{"parent_id": victim, "name": "intruder"}, stranger.Cookie)
	nodeWant(t, rec, http.StatusNotFound, CodeNotFound)

	if after := nodeChildCount(t, pool, victim); after != before {
		t.Errorf("the folder gained %d children through a rejected request", after-before)
	}
}

func TestMoveIntoAnotherUsersFolderIsAMissAndWritesNothing(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)

	mine := nodeMkFolder(t, pool, owner, owner.RootID, "Mine")
	theirs := nodeMkFolder(t, pool, stranger, stranger.RootID, "Theirs")

	rec := authDo(t, h, http.MethodPatch, "/api/nodes/"+mine.String(),
		map[string]any{"parent_id": theirs}, owner.Cookie)
	nodeWant(t, rec, http.StatusNotFound, CodeNotFound)

	row := nodeReload(t, pool, mine)
	if row.ParentID == nil || *row.ParentID != owner.RootID {
		t.Errorf("parent_id = %v, want the owner's root -- the rejected move wrote anyway", row.ParentID)
	}
	if n := nodeChildCount(t, pool, theirs); n != 0 {
		t.Errorf("the other user's folder has %d children", n)
	}

	// And the mirror image: the caller does not own the node in the path.
	rec = authDo(t, h, http.MethodPatch, "/api/nodes/"+theirs.String(),
		map[string]any{"name": "renamed"}, owner.Cookie)
	nodeWant(t, rec, http.StatusNotFound, CodeNotFound)
	if nodeReload(t, pool, theirs).Name != "Theirs" {
		t.Error("a rejected rename renamed the node")
	}
}

func TestCopyIntoAnotherUsersFolderLeavesTheRefcountAlone(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)

	fileID, blobID := nodeMkFile(t, pool, owner, owner.RootID, "secret.txt")
	theirs := nodeMkFolder(t, pool, stranger, stranger.RootID, "Theirs")

	rec := authDo(t, h, http.MethodPost, "/api/nodes/"+fileID.String()+"/copy",
		map[string]any{"parent_id": theirs}, owner.Cookie)
	nodeWant(t, rec, http.StatusNotFound, CodeNotFound)

	if got := nodeRefcount(t, pool, blobID); got != 1 {
		t.Errorf("refcount = %d, want 1 -- the rejected copy still bumped it", got)
	}
	if n := nodeChildCount(t, pool, theirs); n != 0 {
		t.Errorf("the other user's folder has %d children", n)
	}

	// Copying someone else's file is equally a miss, with no bump.
	rec = authDo(t, h, http.MethodPost, "/api/nodes/"+fileID.String()+"/copy",
		map[string]any{"parent_id": stranger.RootID}, stranger.Cookie)
	nodeWant(t, rec, http.StatusNotFound, CodeNotFound)
	if got := nodeRefcount(t, pool, blobID); got != 1 {
		t.Errorf("refcount = %d after a foreign copy attempt, want 1", got)
	}
}

func TestMoveIntoOwnDescendantIsACycle(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)

	top := nodeMkFolder(t, pool, owner, owner.RootID, "Top")
	mid := nodeMkFolder(t, pool, owner, top, "Mid")
	leaf := nodeMkFolder(t, pool, owner, mid, "Leaf")

	for _, dest := range []uuid.UUID{leaf, mid, top} {
		rec := authDo(t, h, http.MethodPatch, "/api/nodes/"+top.String(),
			map[string]any{"parent_id": dest}, owner.Cookie)
		nodeWant(t, rec, http.StatusUnprocessableEntity, CodeCycle)
	}

	row := nodeReload(t, pool, top)
	if row.ParentID == nil || *row.ParentID != owner.RootID {
		t.Errorf("parent_id = %v after the refused moves", row.ParentID)
	}
}

func TestRootCannotBeRenamedOrMoved(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	folder := nodeMkFolder(t, pool, owner, owner.RootID, "Docs")

	rec := authDo(t, h, http.MethodPatch, "/api/nodes/"+owner.RootID.String(),
		map[string]any{"name": "Not My Drive"}, owner.Cookie)
	nodeWant(t, rec, http.StatusUnprocessableEntity, CodeInvalid)

	rec = authDo(t, h, http.MethodPatch, "/api/nodes/"+owner.RootID.String(),
		map[string]any{"parent_id": folder}, owner.Cookie)
	nodeWant(t, rec, http.StatusUnprocessableEntity, CodeInvalid)

	if nodeReload(t, pool, owner.RootID).Name != "My Drive" {
		t.Error("the root folder was renamed")
	}
}

// ------------------------------------------------------------- conflicts -----

func TestFolderCollisionWithoutAPolicyIsAConflict(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	parent := nodeMkFolder(t, pool, owner, owner.RootID, "Parent")

	body := map[string]any{"parent_id": parent, "name": "Reports"}
	rec := authDo(t, h, http.MethodPost, "/api/folders", body, owner.Cookie)
	nodeWant(t, rec, http.StatusCreated, "")

	rec = authDo(t, h, http.MethodPost, "/api/folders", body, owner.Cookie)
	nodeWant(t, rec, http.StatusConflict, CodeNameConflict)

	// Case-insensitively, too: the sibling index is on lower(name).
	rec = authDo(t, h, http.MethodPost, "/api/folders",
		map[string]any{"parent_id": parent, "name": "REPORTS"}, owner.Cookie)
	nodeWant(t, rec, http.StatusConflict, CodeNameConflict)
}

func TestFolderReuseIsIdempotent(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	parent := nodeMkFolder(t, pool, owner, owner.RootID, "Parent")

	body := map[string]any{"parent_id": parent, "name": "Trip", "conflict_policy": node.PolicyReuse}

	rec := authDo(t, h, http.MethodPost, "/api/folders", body, owner.Cookie)
	nodeWant(t, rec, http.StatusCreated, "")
	var first struct {
		NodeDTO
		Existing bool `json:"existing"`
	}
	nodeDecode(t, rec, &first)
	if first.Existing {
		t.Error("the first create reported existing:true")
	}

	rec = authDo(t, h, http.MethodPost, "/api/folders", body, owner.Cookie)
	nodeWant(t, rec, http.StatusOK, "")
	var second struct {
		NodeDTO
		Existing bool `json:"existing"`
	}
	nodeDecode(t, rec, &second)
	if !second.Existing {
		t.Error("the second create did not report existing:true")
	}
	if second.ID != first.ID {
		t.Errorf("reuse returned %s, want the existing folder %s", second.ID, first.ID)
	}
	if n := nodeChildCount(t, pool, parent); n != 1 {
		t.Errorf("the folder was created %d times", n)
	}

	// reuse only reuses folders: a file of that name is still a conflict.
	nodeMkFile(t, pool, owner, parent, "notes.txt")
	rec = authDo(t, h, http.MethodPost, "/api/folders",
		map[string]any{"parent_id": parent, "name": "notes.txt", "conflict_policy": node.PolicyReuse}, owner.Cookie)
	nodeWant(t, rec, http.StatusConflict, CodeNameConflict)
}

func TestRenamePolicyPicksTheNextFreeName(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	parent := nodeMkFolder(t, pool, owner, owner.RootID, "Parent")

	nodeMkFile(t, pool, owner, parent, "a.txt")
	nodeMkFile(t, pool, owner, parent, "a (1).txt")
	nodeMkFile(t, pool, owner, parent, "a (2).txt")
	mover, _ := nodeMkFile(t, pool, owner, parent, "b.txt")

	// No policy: the client is told about the collision.
	rec := authDo(t, h, http.MethodPatch, "/api/nodes/"+mover.String(),
		map[string]any{"name": "a.txt"}, owner.Cookie)
	nodeWant(t, rec, http.StatusConflict, CodeNameConflict)

	rec = authDo(t, h, http.MethodPatch, "/api/nodes/"+mover.String(),
		map[string]any{"name": "a.txt", "conflict_policy": node.PolicyRename}, owner.Cookie)
	nodeWant(t, rec, http.StatusOK, "")
	var dto NodeDTO
	nodeDecode(t, rec, &dto)
	if dto.Name != "a (3).txt" {
		t.Errorf("name = %q, want %q", dto.Name, "a (3).txt")
	}
	if got := nodeReload(t, pool, mover).Name; got != "a (3).txt" {
		t.Errorf("stored name = %q", got)
	}
}

func TestReplacePolicyTrashesTheCollidingNode(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	parent := nodeMkFolder(t, pool, owner, owner.RootID, "Parent")

	victim, _ := nodeMkFile(t, pool, owner, parent, "a.txt")
	mover, _ := nodeMkFile(t, pool, owner, parent, "b.txt")

	rec := authDo(t, h, http.MethodPatch, "/api/nodes/"+mover.String(),
		map[string]any{"name": "a.txt", "conflict_policy": node.PolicyReplace}, owner.Cookie)
	nodeWant(t, rec, http.StatusOK, "")

	moved := nodeReload(t, pool, mover)
	if moved.Name != "a.txt" || moved.DeletedAt != nil {
		t.Errorf("the replacing node = %+v", moved)
	}
	replaced := nodeReload(t, pool, victim)
	if replaced.DeletedAt == nil {
		t.Error("the replaced node was not trashed")
	}
	if !replaced.TrashedRoot {
		t.Error("the replaced node is not its own trash root")
	}
}

func TestReuseIsRejectedOutsideFolderCreation(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	fileID, _ := nodeMkFile(t, pool, owner, owner.RootID, "a.txt")

	rec := authDo(t, h, http.MethodPatch, "/api/nodes/"+fileID.String(),
		map[string]any{"name": "b.txt", "conflict_policy": node.PolicyReuse}, owner.Cookie)
	nodeWant(t, rec, http.StatusUnprocessableEntity, CodeInvalid)

	rec = authDo(t, h, http.MethodPost, "/api/nodes/"+fileID.String()+"/copy",
		map[string]any{"parent_id": owner.RootID, "conflict_policy": "sideways"}, owner.Cookie)
	nodeWant(t, rec, http.StatusUnprocessableEntity, CodeInvalid)
}

// ------------------------------------------------------------------ copy -----

func TestCopyFileSharesTheBlob(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	dest := nodeMkFolder(t, pool, owner, owner.RootID, "Dest")
	fileID, blobID := nodeMkFile(t, pool, owner, owner.RootID, "a.txt")

	rec := authDo(t, h, http.MethodPost, "/api/nodes/"+fileID.String()+"/copy",
		map[string]any{"parent_id": dest}, owner.Cookie)
	nodeWant(t, rec, http.StatusCreated, "")

	var dto NodeDTO
	nodeDecode(t, rec, &dto)
	if dto.ID == fileID {
		t.Error("copy returned the source node")
	}
	if dto.Name != "a.txt" || dto.Size == nil || *dto.Size != 11 {
		t.Errorf("copy DTO = %+v", dto)
	}
	if got := nodeRefcount(t, pool, blobID); got != 2 {
		t.Errorf("refcount = %d, want 2", got)
	}

	// Into the same folder as the source, the name has to give way.
	rec = authDo(t, h, http.MethodPost, "/api/nodes/"+fileID.String()+"/copy",
		map[string]any{"parent_id": owner.RootID}, owner.Cookie)
	nodeWant(t, rec, http.StatusConflict, CodeNameConflict)
	if got := nodeRefcount(t, pool, blobID); got != 2 {
		t.Errorf("refcount = %d after a refused copy, want 2", got)
	}

	rec = authDo(t, h, http.MethodPost, "/api/nodes/"+fileID.String()+"/copy",
		map[string]any{"parent_id": owner.RootID, "conflict_policy": node.PolicyRename}, owner.Cookie)
	nodeWant(t, rec, http.StatusCreated, "")
	nodeDecode(t, rec, &dto)
	if dto.Name != "a (1).txt" {
		t.Errorf("copy name = %q, want %q", dto.Name, "a (1).txt")
	}
	if got := nodeRefcount(t, pool, blobID); got != 3 {
		t.Errorf("refcount = %d, want 3", got)
	}
}

func TestCopyAFolderIsUnsupported(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	src := nodeMkFolder(t, pool, owner, owner.RootID, "Src")
	dest := nodeMkFolder(t, pool, owner, owner.RootID, "Dest")

	rec := authDo(t, h, http.MethodPost, "/api/nodes/"+src.String()+"/copy",
		map[string]any{"parent_id": dest}, owner.Cookie)
	nodeWant(t, rec, http.StatusUnprocessableEntity, CodeUnsupported)
	if n := nodeChildCount(t, pool, dest); n != 0 {
		t.Errorf("the destination gained %d children", n)
	}
}

// ------------------------------------------------------- filename hygiene ----

func TestNameHygieneAtTheAPIBoundary(t *testing.T) {
	h, pool := nodeTestServer(t)
	owner := nodeNewUser(t, pool)
	parent := nodeMkFolder(t, pool, owner, owner.RootID, "Parent")

	for _, name := range []string{"", "  ", "a/b", `a\b`, ".", "..", "CON", "report.", "nul.txt", "\x01\x02"} {
		rec := authDo(t, h, http.MethodPost, "/api/folders",
			map[string]any{"parent_id": parent, "name": name}, owner.Cookie)
		nodeWant(t, rec, http.StatusUnprocessableEntity, CodeInvalid)
	}

	// Controls are stripped and the rest is kept, emoji included.
	rec := authDo(t, h, http.MethodPost, "/api/folders",
		map[string]any{"parent_id": parent, "name": "  trip\r\n 2026 🏖️  "}, owner.Cookie)
	nodeWant(t, rec, http.StatusCreated, "")
	var dto NodeDTO
	nodeDecode(t, rec, &dto)
	if dto.Name != "trip 2026 🏖️" {
		t.Errorf("stored name = %q", dto.Name)
	}

	// A move needs a body that says what to do.
	rec = authDo(t, h, http.MethodPatch, "/api/nodes/"+parent.String(), map[string]any{}, owner.Cookie)
	nodeWant(t, rec, http.StatusUnprocessableEntity, CodeInvalid)
}
