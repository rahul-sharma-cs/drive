package api

// The share routes through the whole router: real cookies, real
// RequireAuth, real SQL against the drive-test Postgres, and -- for the
// public half -- the real Garage presigner. The owner surface's contract:
// the URL shown once, the one 409, the full-triple PATCH, the 404 for
// everything that is not the caller's live share, the two writers in the
// auth bucket. The recipient surface's: the one identical 404, the
// idempotent session, the budget in front of Argon2, the redirect-shaped
// download refusals, the preview that never counts, and a log stream the
// token never reaches.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/share"
	"github.com/rahul-sharma-cs/drive/server/internal/upload"
)

// shareTestServer is nodeTestServer under the name this file reads naturally.
func shareTestServer(t *testing.T) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	return nodeTestServer(t)
}

// sharePost creates a link and fails the test on anything but 201.
func sharePost(t *testing.T, h http.Handler, owner nodeUser, body any) shareResponse {
	t.Helper()
	rec := authDo(t, h, http.MethodPost, "/api/shares", body, owner.Cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /shares: status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp shareResponse
	nodeDecode(t, rec, &resp)
	return resp
}

// shareToken pulls the token out of a create/regenerate response's URL.
func shareToken(t *testing.T, url string) string {
	t.Helper()
	token, ok := strings.CutPrefix(url, authTestBaseURL+"/s/")
	if !ok {
		t.Fatalf("url = %q, want prefix %s/s/", url, authTestBaseURL)
	}
	return token
}

func shareGuests(t *testing.T, pool *pgxpool.Pool, shareID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM share_guest_sessions WHERE share_id = $1`, shareID).Scan(&n); err != nil {
		t.Fatalf("counting guest sessions: %v", err)
	}
	return n
}

func shareCount(t *testing.T, pool *pgxpool.Pool, owner nodeUser) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM shares WHERE created_by = $1`, owner.ID).Scan(&n); err != nil {
		t.Fatalf("counting shares: %v", err)
	}
	return n
}

// ----------------------------------------------------------------- create ----

// The one moment the server holds the token: 201 with the URL built from the
// deployment's base URL, a 43-character base64url token, and a row that holds
// only its sha256.
func TestCreateShareShowsTheURLOnce(t *testing.T) {
	h, pool := shareTestServer(t)
	owner := nodeNewUser(t, pool)
	fileID, _ := nodeMkFile(t, pool, owner, owner.RootID, "shared.txt")

	until := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	resp := sharePost(t, h, owner, map[string]any{
		"node_id": fileID, "mode": "public", "permission": "view",
		"expires_at": until, "password": authTestPassword, "max_downloads": 2,
	})

	token := shareToken(t, resp.URL)
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(token) {
		t.Errorf("token %q is not 43 base64url characters", token)
	}

	sh := resp.Share
	if sh.Node.ID != fileID || sh.Node.Name != "shared.txt" || sh.Node.ParentID == nil || *sh.Node.ParentID != owner.RootID {
		t.Errorf("share.node = %+v, want the shared file under the root", sh.Node)
	}
	if sh.Node.Size == nil || *sh.Node.Size != 11 || sh.Node.Mime == nil || *sh.Node.Mime != "text/plain" {
		t.Errorf("share.node size/mime = %v/%v, want 11/text/plain", sh.Node.Size, sh.Node.Mime)
	}
	if !sh.NodeLive || !sh.HasPassword || sh.DownloadCount != 0 {
		t.Errorf("share = %+v, want live, password on, count 0", sh)
	}
	if sh.ExpiresAt == nil || !sh.ExpiresAt.Equal(until) || sh.MaxDownloads == nil || *sh.MaxDownloads != 2 {
		t.Errorf("share expiry/cap = %v/%v, want %v/2", sh.ExpiresAt, sh.MaxDownloads, until)
	}

	// The row: sha256 of the URL's tail, an Argon2id hash, and no plaintext of
	// either credential.
	var tokenHash []byte
	var passwordHash string
	if err := pool.QueryRow(context.Background(),
		`SELECT token_hash, password_hash FROM shares WHERE id = $1`, sh.ID).Scan(&tokenHash, &passwordHash); err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if string(tokenHash) != string(auth.HashToken(token)) {
		t.Errorf("token_hash is not sha256 of the URL's token")
	}
	if !strings.HasPrefix(passwordHash, "$argon2id$") || strings.Contains(passwordHash, authTestPassword) {
		t.Errorf("password_hash = %q, want an Argon2id PHC string", passwordHash)
	}
}

// Everything the create refuses, and that a refusal writes nothing. The
// blobless row is the one worth a comment: resolving through the same
// blob-joined read the public routes use is what keeps a share from being
// created 404-at-birth while occupying the file's one slot.
func TestCreateShareRefusals(t *testing.T) {
	h, pool := shareTestServer(t)
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)

	fileID, _ := nodeMkFile(t, pool, owner, owner.RootID, "mine.txt")
	folderID := nodeMkFolder(t, pool, owner, owner.RootID, "Docs")
	trashedID, _ := nodeMkFile(t, pool, owner, owner.RootID, "gone.txt")
	if _, err := pool.Exec(context.Background(),
		`UPDATE nodes SET deleted_at = now(), trashed_root = true WHERE id = $1`, trashedID); err != nil {
		t.Fatalf("trashing: %v", err)
	}
	bloblessID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name, size)
		 VALUES ($1, $2, $3, 'file', 'orphan.bin', 0)`, bloblessID, owner.ID, owner.RootID); err != nil {
		t.Fatalf("inserting the blobless file: %v", err)
	}

	past := time.Now().Add(-time.Hour).UTC()
	cases := []struct {
		what   string
		body   map[string]any
		cookie *http.Cookie
		status int
		code   string
	}{
		{"a folder", map[string]any{"node_id": folderID}, owner.Cookie, http.StatusUnprocessableEntity, CodeUnsupported},
		{"a file with no blob", map[string]any{"node_id": bloblessID}, owner.Cookie, http.StatusNotFound, CodeNotFound},
		{"a trashed file", map[string]any{"node_id": trashedID}, owner.Cookie, http.StatusNotFound, CodeNotFound},
		{"someone else's file", map[string]any{"node_id": fileID}, stranger.Cookie, http.StatusNotFound, CodeNotFound},
		{"an unknown id", map[string]any{"node_id": uuid.New()}, owner.Cookie, http.StatusNotFound, CodeNotFound},
		{"no node_id", map[string]any{}, owner.Cookie, http.StatusUnprocessableEntity, CodeInvalid},
		{"mode restricted", map[string]any{"node_id": fileID, "mode": "restricted"}, owner.Cookie, http.StatusUnprocessableEntity, CodeUnsupported},
		{"permission edit", map[string]any{"node_id": fileID, "permission": "edit"}, owner.Cookie, http.StatusUnprocessableEntity, CodeUnsupported},
		{"a past expiry", map[string]any{"node_id": fileID, "expires_at": past}, owner.Cookie, http.StatusUnprocessableEntity, CodeInvalid},
		{"a cap of zero", map[string]any{"node_id": fileID, "max_downloads": 0}, owner.Cookie, http.StatusUnprocessableEntity, CodeInvalid},
		{"a cap over the ceiling", map[string]any{"node_id": fileID, "max_downloads": share.MaxDownloadsCap + 1}, owner.Cookie, http.StatusUnprocessableEntity, CodeInvalid},
		{"a 7-character password", map[string]any{"node_id": fileID, "password": "seven77"}, owner.Cookie, http.StatusUnprocessableEntity, CodeInvalid},
		{"a 129-character password", map[string]any{"node_id": fileID, "password": strings.Repeat("p", 129)}, owner.Cookie, http.StatusUnprocessableEntity, CodeInvalid},
		{"an anonymous caller", map[string]any{"node_id": fileID}, nil, http.StatusUnauthorized, CodeUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			rec := authDo(t, h, http.MethodPost, "/api/shares", c.body, c.cookie)
			nodeWant(t, rec, c.status, c.code)
		})
	}
	if n := shareCount(t, pool, owner); n != 0 {
		t.Fatalf("%d share rows exist after nothing but refusals", n)
	}

	// The SPA sends every optional key explicitly, so a null is the same as
	// leaving one out.
	nulled := sharePost(t, h, owner, map[string]any{
		"node_id": fileID, "expires_at": nil, "password": nil, "max_downloads": nil,
	})
	if nulled.Share.HasPassword || nulled.Share.ExpiresAt != nil || nulled.Share.MaxDownloads != nil {
		t.Errorf("share from explicit nulls = %+v, want everything unset", nulled.Share)
	}
	if rec := authDo(t, h, http.MethodDelete, "/api/shares/"+nulled.Share.ID.String(), nil, owner.Cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status %d, body %s", rec.Code, rec.Body.String())
	}

	// One active link per file: the second create is the one 409, and a
	// revoke frees the slot.
	first := sharePost(t, h, owner, map[string]any{"node_id": fileID})
	rec := authDo(t, h, http.MethodPost, "/api/shares", map[string]any{"node_id": fileID}, owner.Cookie)
	nodeWant(t, rec, http.StatusConflict, CodeExists)
	if rec := authDo(t, h, http.MethodDelete, "/api/shares/"+first.Share.ID.String(), nil, owner.Cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status %d, body %s", rec.Code, rec.Body.String())
	}
	sharePost(t, h, owner, map[string]any{"node_id": fileID})
}

// ------------------------------------------------------------------ patch ----

// The settings body: expires_at and max_downloads are required keys where
// null clears, and password is three-way -- absent keeps the current one,
// null clears it, a string replaces it -- because an owner editing an expiry
// cannot re-type a password that exists nowhere but as a hash. Absent and
// null have to stay different things for that key, which is what the raw
// decoding is for.
func TestPatchShareSettingsAndRegenerate(t *testing.T) {
	h, pool := shareTestServer(t)
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)
	fileID, _ := nodeMkFile(t, pool, owner, owner.RootID, "patched.txt")
	created := sharePost(t, h, owner, map[string]any{"node_id": fileID, "password": authTestPassword})
	id := created.Share.ID
	path := "/api/shares/" + id.String()
	store := share.NewStore(pool)

	until := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	t.Run("set all three", func(t *testing.T) {
		rec := authDo(t, h, http.MethodPatch, path, map[string]any{
			"expires_at": until, "password": "another password", "max_downloads": 5,
		}, owner.Cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}
		var resp shareResponse
		nodeDecode(t, rec, &resp)
		if resp.URL != "" {
			t.Errorf("a settings PATCH returned a URL -- only create and regenerate may")
		}
		sh := resp.Share
		if !sh.HasPassword || sh.ExpiresAt == nil || !sh.ExpiresAt.Equal(until) || sh.MaxDownloads == nil || *sh.MaxDownloads != 5 {
			t.Errorf("share = %+v, want the triple applied", sh)
		}
	})

	t.Run("null clears all three", func(t *testing.T) {
		rec := authDo(t, h, http.MethodPatch, path, map[string]any{
			"expires_at": nil, "password": nil, "max_downloads": nil,
		}, owner.Cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}
		var resp shareResponse
		nodeDecode(t, rec, &resp)
		if resp.Share.HasPassword || resp.Share.ExpiresAt != nil || resp.Share.MaxDownloads != nil {
			t.Errorf("share = %+v, want everything cleared", resp.Share)
		}
		var passwordHash *string
		if err := pool.QueryRow(context.Background(), `SELECT password_hash FROM shares WHERE id = $1`, id).Scan(&passwordHash); err != nil {
			t.Fatalf("reading the row: %v", err)
		}
		if passwordHash != nil {
			t.Error("password_hash survived an explicit null")
		}
	})

	refusals := []struct {
		what string
		body map[string]any
	}{
		{"a missing expires_at", map[string]any{"password": nil, "max_downloads": nil}},
		{"a missing max_downloads", map[string]any{"expires_at": nil, "password": nil}},
		{"action plus a setting", map[string]any{"action": "regenerate", "max_downloads": 3}},
		{"an action that is not regenerate", map[string]any{"action": "rotate"}},
		{"an unknown field", map[string]any{"expires_at": nil, "password": nil, "max_downloads": nil, "nope": 1}},
		{"a short password", map[string]any{"expires_at": nil, "password": "short", "max_downloads": nil}},
		{"a past expiry", map[string]any{"expires_at": time.Now().Add(-time.Hour).UTC(), "password": nil, "max_downloads": nil}},
		{"a cap of zero", map[string]any{"expires_at": nil, "password": nil, "max_downloads": 0}},
	}
	for _, c := range refusals {
		t.Run(c.what, func(t *testing.T) {
			rec := authDo(t, h, http.MethodPatch, path, c.body, owner.Cookie)
			nodeWant(t, rec, http.StatusUnprocessableEntity, CodeInvalid)
		})
	}

	t.Run("an absent password key keeps the hash", func(t *testing.T) {
		// Arm a password, then edit only the expiry and cap: the stored hash
		// must come through byte-identical -- the owner cannot re-send a
		// password that exists nowhere but as a hash.
		if rec := authDo(t, h, http.MethodPatch, path, map[string]any{
			"expires_at": nil, "password": authTestPassword, "max_downloads": nil,
		}, owner.Cookie); rec.Code != http.StatusOK {
			t.Fatalf("arming the password: %d %s", rec.Code, rec.Body.String())
		}
		var before string
		if err := pool.QueryRow(context.Background(), `SELECT password_hash FROM shares WHERE id = $1`, id).Scan(&before); err != nil {
			t.Fatalf("reading the hash: %v", err)
		}
		if _, _, err := store.MintGuest(context.Background(), id); err != nil {
			t.Fatalf("minting a guest: %v", err)
		}

		rec := authDo(t, h, http.MethodPatch, path, map[string]any{"expires_at": until, "max_downloads": 7}, owner.Cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}
		var resp shareResponse
		nodeDecode(t, rec, &resp)
		if !resp.Share.HasPassword || resp.Share.ExpiresAt == nil || !resp.Share.ExpiresAt.Equal(until) || resp.Share.MaxDownloads == nil || *resp.Share.MaxDownloads != 7 {
			t.Errorf("share = %+v, want the password kept and the other two applied", resp.Share)
		}
		var after string
		if err := pool.QueryRow(context.Background(), `SELECT password_hash FROM shares WHERE id = $1`, id).Scan(&after); err != nil {
			t.Fatalf("reading the hash: %v", err)
		}
		if after != before {
			t.Error("an expiry-only PATCH replaced the stored password hash")
		}
		if n := shareGuests(t, pool, id); n != 1 {
			t.Errorf("%d guest sessions after an expiry-only PATCH, want 1 untouched", n)
		}

		// And an explicit null still clears it, ending the sessions the gate
		// minted.
		rec = authDo(t, h, http.MethodPatch, path, map[string]any{"expires_at": nil, "password": nil, "max_downloads": nil}, owner.Cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("clearing: %d %s", rec.Code, rec.Body.String())
		}
		if n := shareGuests(t, pool, id); n != 0 {
			t.Errorf("%d guest sessions survive the password being cleared, want 0", n)
		}
	})

	t.Run("regenerate", func(t *testing.T) {
		if _, err := pool.Exec(context.Background(), `UPDATE shares SET download_count = 3 WHERE id = $1`, id); err != nil {
			t.Fatalf("seeding the count: %v", err)
		}
		if _, _, err := store.MintGuest(context.Background(), id); err != nil {
			t.Fatalf("minting a guest: %v", err)
		}

		rec := authDo(t, h, http.MethodPatch, path, map[string]any{"action": "regenerate"}, owner.Cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}
		var resp shareResponse
		nodeDecode(t, rec, &resp)
		fresh := shareToken(t, resp.URL)
		if fresh == shareToken(t, created.URL) {
			t.Error("regenerate returned the same URL")
		}
		if resp.Share.DownloadCount != 0 {
			t.Errorf("download_count = %d after regenerate, want 0", resp.Share.DownloadCount)
		}
		if n := shareGuests(t, pool, id); n != 0 {
			t.Errorf("%d guest sessions survive a regenerate", n)
		}
		if _, err := store.Resolve(context.Background(), auth.HashToken(shareToken(t, created.URL))); err == nil {
			t.Error("the old token still resolves after regenerate")
		}
		if r, err := store.Resolve(context.Background(), auth.HashToken(fresh)); err != nil || r.State != share.StateLive {
			t.Errorf("the new token resolves to (%v, %v), want live", r, err)
		}
	})

	t.Run("misses", func(t *testing.T) {
		body := map[string]any{"expires_at": nil, "password": nil, "max_downloads": nil}
		for what, c := range map[string]struct {
			path   string
			cookie *http.Cookie
			status int
			code   string
		}{
			"a foreign id":       {path, stranger.Cookie, http.StatusNotFound, CodeNotFound},
			"an unknown id":      {"/api/shares/" + uuid.NewString(), owner.Cookie, http.StatusNotFound, CodeNotFound},
			"a non-UUID id":      {"/api/shares/not-a-uuid", owner.Cookie, http.StatusNotFound, CodeNotFound},
			"an anonymous PATCH": {path, nil, http.StatusUnauthorized, CodeUnauthorized},
		} {
			rec := authDo(t, h, http.MethodPatch, c.path, body, c.cookie)
			if rec.Code != c.status {
				t.Errorf("%s: status %d, want %d (body %s)", what, rec.Code, c.status, rec.Body.String())
			}
		}
	})

	t.Run("a revoked share is a miss", func(t *testing.T) {
		if rec := authDo(t, h, http.MethodDelete, path, nil, owner.Cookie); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE: %d %s", rec.Code, rec.Body.String())
		}
		rec := authDo(t, h, http.MethodPatch, path, map[string]any{"expires_at": nil, "password": nil, "max_downloads": nil}, owner.Cookie)
		nodeWant(t, rec, http.StatusNotFound, CodeNotFound)
		rec = authDo(t, h, http.MethodPatch, path, map[string]any{"action": "regenerate"}, owner.Cookie)
		nodeWant(t, rec, http.StatusNotFound, CodeNotFound)
	})
}

// ----------------------------------------------------------------- bucket ----

// The two writers reach Argon2 with a caller-chosen password, so they sit in
// the per-IP auth bucket although they are authenticated -- an unbucketed
// hashing route would be one account holding every Argon2 slot against
// everybody else's login. The readers stay outside it.
//
// The config is explicit, as abuseServer's is: .env.test raises the allowance
// to 100000, which would make this pass unconditionally.
func TestShareWritersSitInTheAuthBucket(t *testing.T) {
	s, h, _, pool := abuseServer(t)
	owner := nodeNewUser(t, pool)
	fileID, _ := nodeMkFile(t, pool, owner, owner.RootID, "bucketed.txt")

	// Spend this caller's whole allowance. authDo stamps the test's own peer
	// address on every request, so the drained bucket is exactly the one the
	// requests below land in.
	host, _, err := net.SplitHostPort(testClientAddr(t))
	if err != nil {
		t.Fatalf("splitting the test address: %v", err)
	}
	for s.AuthRate.allow(host) { //nolint:revive // draining the bucket
	}

	rec := authDo(t, h, http.MethodPost, "/api/shares",
		map[string]any{"node_id": fileID, "password": authTestPassword}, owner.Cookie)
	nodeWant(t, rec, http.StatusTooManyRequests, CodeRateLimited)
	if n := shareCount(t, pool, owner); n != 0 {
		t.Fatalf("a refused create still wrote %d rows", n)
	}
	rec = authDo(t, h, http.MethodPatch, "/api/shares/"+uuid.NewString(),
		map[string]any{"action": "regenerate"}, owner.Cookie)
	nodeWant(t, rec, http.StatusTooManyRequests, CodeRateLimited)

	// The readers and the revoke answer from an empty bucket.
	if rec := authDo(t, h, http.MethodGet, "/api/shares", nil, owner.Cookie); rec.Code != http.StatusOK {
		t.Errorf("GET /shares from an empty bucket: status %d, want 200", rec.Code)
	}
	rec = authDo(t, h, http.MethodDelete, "/api/shares/"+uuid.NewString(), nil, owner.Cookie)
	nodeWant(t, rec, http.StatusNotFound, CodeNotFound)
}

// ------------------------------------------------------------------- list ----

// The listing is the owner's active links and nobody else's, newest first,
// keyset-paginated at the default 50, and narrowed by ?node_id= for the
// dialog. A trashed file's link stays listed and says node_live false.
func TestListShares(t *testing.T) {
	h, pool := shareTestServer(t)
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)
	store := share.NewStore(pool)
	ctx := context.Background()

	var fileIDs []uuid.UUID
	for i := range 51 {
		fileID, _ := nodeMkFile(t, pool, owner, owner.RootID, "list-"+uuid.NewString()[:8]+".txt")
		if _, _, err := store.Create(ctx, owner.ID, fileID, share.Settings{}); err != nil {
			t.Fatalf("creating share %d: %v", i, err)
		}
		fileIDs = append(fileIDs, fileID)
	}
	strangerFile, _ := nodeMkFile(t, pool, stranger, stranger.RootID, "theirs.txt")
	strangerShare, _, err := store.Create(ctx, stranger.ID, strangerFile, share.Settings{})
	if err != nil {
		t.Fatalf("creating the stranger's share: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE nodes SET deleted_at = now(), trashed_root = true WHERE id = $1`, fileIDs[0]); err != nil {
		t.Fatalf("trashing: %v", err)
	}

	rec := authDo(t, h, http.MethodGet, "/api/shares", nil, owner.Cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /shares: %d %s", rec.Code, rec.Body.String())
	}
	var page1 List[ShareDTO]
	nodeDecode(t, rec, &page1)
	if len(page1.Items) != DefaultLimit || page1.NextCursor == nil {
		t.Fatalf("page 1 = %d items, cursor %v; want %d and a cursor", len(page1.Items), page1.NextCursor, DefaultLimit)
	}
	rec = authDo(t, h, http.MethodGet, "/api/shares?cursor="+*page1.NextCursor, nil, owner.Cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("page 2: %d %s", rec.Code, rec.Body.String())
	}
	var page2 List[ShareDTO]
	nodeDecode(t, rec, &page2)
	if len(page2.Items) != 1 || page2.NextCursor != nil {
		t.Fatalf("page 2 = %d items, cursor %v; want the last 1 and no cursor", len(page2.Items), page2.NextCursor)
	}

	seen := map[uuid.UUID]bool{}
	var prev *ShareDTO
	for _, sh := range append(page1.Items, page2.Items...) {
		if sh.ID == strangerShare.ID {
			t.Fatal("another user's share appears in the listing")
		}
		if seen[sh.ID] {
			t.Fatalf("share %s appears twice across the pages", sh.ID)
		}
		seen[sh.ID] = true
		if prev != nil && sh.CreatedAt.After(prev.CreatedAt) {
			t.Fatal("the listing is not newest first")
		}
		sh := sh
		prev = &sh
	}

	// The trashed file's link is listed, inert, and says so.
	rec = authDo(t, h, http.MethodGet, "/api/shares?node_id="+fileIDs[0].String(), nil, owner.Cookie)
	var filtered List[ShareDTO]
	nodeDecode(t, rec, &filtered)
	if len(filtered.Items) != 1 || filtered.Items[0].Node.ID != fileIDs[0] {
		t.Fatalf("?node_id= returned %d items, want the one link", len(filtered.Items))
	}
	if filtered.Items[0].NodeLive {
		t.Error("node_live = true for a trashed file")
	}

	for what, path := range map[string]string{
		"a garbage cursor":  "/api/shares?cursor=%25%25",
		"a garbage node_id": "/api/shares?node_id=not-a-uuid",
		"a garbage limit":   "/api/shares?limit=none",
	} {
		rec := authDo(t, h, http.MethodGet, path, nil, owner.Cookie)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", what, rec.Code)
		}
	}
	rec = authDo(t, h, http.MethodGet, "/api/shares", nil, nil)
	nodeWant(t, rec, http.StatusUnauthorized, CodeUnauthorized)
}

// ----------------------------------------------------------------- revoke ----

// DELETE is the revoke: 204, the row kept for the access log, the guest
// sessions gone, the token dead. Every id that is not the caller's live
// share -- foreign, unknown, unparseable, already revoked -- is the same 404.
func TestRevokeShare(t *testing.T) {
	h, pool := shareTestServer(t)
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)
	fileID, _ := nodeMkFile(t, pool, owner, owner.RootID, "revoked.txt")
	created := sharePost(t, h, owner, map[string]any{"node_id": fileID})
	id := created.Share.ID
	path := "/api/shares/" + id.String()
	store := share.NewStore(pool)
	if _, _, err := store.MintGuest(context.Background(), id); err != nil {
		t.Fatalf("minting a guest: %v", err)
	}

	rec := authDo(t, h, http.MethodDelete, path, nil, stranger.Cookie)
	nodeWant(t, rec, http.StatusNotFound, CodeNotFound)
	rec = authDo(t, h, http.MethodDelete, path, nil, nil)
	nodeWant(t, rec, http.StatusUnauthorized, CodeUnauthorized)

	rec = authDo(t, h, http.MethodDelete, path, nil, owner.Cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status %d, body %s", rec.Code, rec.Body.String())
	}

	var revoked bool
	if err := pool.QueryRow(context.Background(),
		`SELECT revoked_at IS NOT NULL FROM shares WHERE id = $1`, id).Scan(&revoked); err != nil {
		t.Fatalf("the row is gone: %v -- the access log has nothing to point at", err)
	}
	if !revoked {
		t.Error("revoked_at is still NULL")
	}
	if n := shareGuests(t, pool, id); n != 0 {
		t.Errorf("%d guest sessions survive the revoke -- a minted session still downloads", n)
	}
	if r, err := store.Resolve(context.Background(), auth.HashToken(shareToken(t, created.URL))); err != nil || r.State != share.StateRevoked {
		t.Errorf("Resolve after revoke = (%v, %v), want the revoked state", r, err)
	}

	rec = authDo(t, h, http.MethodDelete, path, nil, owner.Cookie)
	nodeWant(t, rec, http.StatusNotFound, CodeNotFound)
	rec = authDo(t, h, http.MethodDelete, "/api/shares/not-a-uuid", nil, owner.Cookie)
	nodeWant(t, rec, http.StatusNotFound, CodeNotFound)

	// And the listing agrees the link is gone.
	rec = authDo(t, h, http.MethodGet, "/api/shares?node_id="+fileID.String(), nil, owner.Cookie)
	var listed List[ShareDTO]
	nodeDecode(t, rec, &listed)
	if len(listed.Items) != 0 {
		t.Errorf("a revoked share is still listed")
	}
}

// ------------------------------------------------------------- recipients ----

// shareGuestServer is downloadTestServer under the name this half reads
// naturally: the whole router over the real database and the real Garage
// presigner.
func shareGuestServer(t *testing.T) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	return downloadTestServer(t)
}

// shareDo issues a recipient's request: no drive_session, the CSRF header,
// this test's own peer address, and whatever cookies the case holds.
func shareDo(t *testing.T, h http.Handler, method, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return shareDoBare(t, h, method, path, body, true, cookies...)
}

// shareDoBare is shareDo with the X-Drive-Client header optional, for the
// cases asserting the gate.
func shareDoBare(t *testing.T, h http.Handler, method, path string, body any, clientHeader bool, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	reader := strings.NewReader("")
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshalling request body: %v", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, path, reader)
	if clientHeader {
		req.Header.Set(ClientHeader, "web")
	}
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = testClientAddr(t)
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// shareMkTypedFile is nodeMkFile with a chosen mime: the preview cases need
// image/png, application/pdf and image/svg+xml where nodeMkFile pins
// text/plain.
func shareMkTypedFile(t *testing.T, pool *pgxpool.Pool, owner nodeUser, name, mime string) (nodeID, blobID uuid.UUID) {
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
		 VALUES ($1, $2, $3, 'file', $4, $5, 11, $6)`,
		nodeID, owner.ID, owner.RootID, name, blobID, mime); err != nil {
		t.Fatalf("inserting file %q: %v", name, err)
	}
	return nodeID, blobID
}

// shareGuestCookie pulls this share's guest cookie out of a response.
func shareGuestCookie(t *testing.T, rec *httptest.ResponseRecorder, shareID uuid.UUID) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "gs_"+shareID.String() {
			return c
		}
	}
	t.Fatalf("no gs_%s cookie in the response (status %d, body %s)", shareID, rec.Code, rec.Body.String())
	return nil
}

// shareMint opens a guest session and returns its cookie.
func shareMint(t *testing.T, h http.Handler, token string, shareID uuid.UUID) *http.Cookie {
	t.Helper()
	rec := shareDo(t, h, http.MethodPost, "/api/s/"+token+"/session", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /session: status %d, body %s", rec.Code, rec.Body.String())
	}
	return shareGuestCookie(t, rec, shareID)
}

func shareAccessCount(t *testing.T, pool *pgxpool.Pool, shareID uuid.UUID, action string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM share_access_log WHERE share_id = $1 AND action = $2`, shareID, action).Scan(&n); err != nil {
		t.Fatalf("counting %s rows: %v", action, err)
	}
	return n
}

func shareCounter(t *testing.T, pool *pgxpool.Pool, shareID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT download_count FROM shares WHERE id = $1`, shareID).Scan(&n); err != nil {
		t.Fatalf("reading download_count: %v", err)
	}
	return n
}

// shareMetaBody mirrors GET /meta's wire shape.
type shareMetaBody struct {
	Name             string     `json:"name"`
	Size             int64      `json:"size"`
	Mime             string     `json:"mime"`
	RequiresPassword bool       `json:"requires_password"`
	ExpiresAt        *time.Time `json:"expires_at"`
	Exhausted        bool       `json:"exhausted"`
	Preview          bool       `json:"preview"`
}

// ------------------------------------------------------------------- /meta ----

// GET /meta is the page's first question and must cost the visitor nothing:
// no cookie, no view row -- and one identical 404 for everything that is not
// a live link, with the denied row written only when a share row exists to
// attribute it to.
func TestShareMeta(t *testing.T) {
	h, pool := shareGuestServer(t)
	owner := nodeNewUser(t, pool)
	ctx := context.Background()

	fileID, _ := nodeMkFile(t, pool, owner, owner.RootID, "meta.txt")
	until := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	created := sharePost(t, h, owner, map[string]any{"node_id": fileID, "expires_at": until})
	token := shareToken(t, created.URL)

	rec := shareDo(t, h, http.MethodGet, "/api/s/"+token+"/meta", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /meta: status %d, body %s", rec.Code, rec.Body.String())
	}
	var meta shareMetaBody
	nodeDecode(t, rec, &meta)
	if meta.Name != "meta.txt" || meta.Size != 11 || meta.Mime != "text/plain" {
		t.Errorf("meta = %+v, want the file's name, size and mime", meta)
	}
	if meta.RequiresPassword || meta.Exhausted || !meta.Preview {
		t.Errorf("meta = %+v, want passwordless, not exhausted, previewable", meta)
	}
	if meta.ExpiresAt == nil || !meta.ExpiresAt.Equal(until) {
		t.Errorf("meta.expires_at = %v, want %v", meta.ExpiresAt, until)
	}
	if got := len(rec.Result().Cookies()); got != 0 {
		t.Errorf("/meta set %d cookies, want none", got)
	}
	if n := shareAccessCount(t, pool, created.Share.ID, share.ActionView); n != 0 {
		t.Errorf("%d view rows after /meta, want 0", n)
	}

	// A password share says so and hands out no more.
	pwFile, _ := nodeMkFile(t, pool, owner, owner.RootID, "meta-pw.txt")
	pwResp := sharePost(t, h, owner, map[string]any{"node_id": pwFile, "password": authTestPassword})
	rec = shareDo(t, h, http.MethodGet, "/api/s/"+shareToken(t, pwResp.URL)+"/meta", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /meta on the password share: %d %s", rec.Code, rec.Body.String())
	}
	nodeDecode(t, rec, &meta)
	if !meta.RequiresPassword {
		t.Error("requires_password = false on a password share")
	}

	// The five identical 404s: four dead states with a share row, each owed
	// one denied row, and an unknown token that writes nothing at all.
	shape := func(name string) shareResponse {
		id, _ := nodeMkFile(t, pool, owner, owner.RootID, name)
		return sharePost(t, h, owner, map[string]any{"node_id": id})
	}
	revoked := shape("meta-revoked.txt")
	if rec := authDo(t, h, http.MethodDelete, "/api/shares/"+revoked.Share.ID.String(), nil, owner.Cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("revoking: %d %s", rec.Code, rec.Body.String())
	}
	expired := shape("meta-expired.txt")
	if _, err := pool.Exec(ctx, `UPDATE shares SET expires_at = now() - interval '1 hour' WHERE id = $1`, expired.Share.ID); err != nil {
		t.Fatalf("expiring: %v", err)
	}
	trashed := shape("meta-trashed.txt")
	if _, err := pool.Exec(ctx, `UPDATE nodes SET deleted_at = now(), trashed_root = true WHERE id = $1`, trashed.Share.Node.ID); err != nil {
		t.Fatalf("trashing: %v", err)
	}
	purged := shape("meta-purged.txt")
	if _, err := pool.Exec(ctx, `UPDATE nodes SET blob_id = NULL WHERE id = $1`, purged.Share.Node.ID); err != nil {
		t.Fatalf("unblobbing: %v", err)
	}

	get := func(tok string) *httptest.ResponseRecorder {
		return shareDo(t, h, http.MethodGet, "/api/s/"+tok+"/meta", nil)
	}
	bodies := map[string]string{}
	dead := map[string]shareResponse{
		"revoked": revoked, "expired": expired, "trashed": trashed, "purged": purged,
	}
	for name, resp := range dead {
		rec := get(shareToken(t, resp.URL))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404 (body %s)", name, rec.Code, rec.Body.String())
		}
		bodies[name] = rec.Body.String()
		if n := shareAccessCount(t, pool, resp.Share.ID, share.ActionDenied); n != 1 {
			t.Errorf("%s: %d denied rows, want 1", name, n)
		}
	}
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM share_access_log`).Scan(&before); err != nil {
		t.Fatalf("counting the log: %v", err)
	}
	rec = get(strings.Repeat("A", 43))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown: status %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	bodies["unknown"] = rec.Body.String()
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM share_access_log`).Scan(&after); err != nil {
		t.Fatalf("counting the log: %v", err)
	}
	if after != before {
		t.Errorf("an unknown token wrote %d access-log rows -- a scan fills the table", after-before)
	}
	for name, body := range bodies {
		if body != bodies["unknown"] {
			t.Errorf("the %s 404 (%s) differs from the unknown one (%s)", name, body, bodies["unknown"])
		}
	}

	// exhausted is a per-browser answer: the session that counted keeps its
	// page on a reload, a fresh browser is told the truth.
	capFile, _ := nodeMkFile(t, pool, owner, owner.RootID, "meta-cap.txt")
	capResp := sharePost(t, h, owner, map[string]any{"node_id": capFile, "max_downloads": 1})
	capToken := shareToken(t, capResp.URL)
	cookie := shareMint(t, h, capToken, capResp.Share.ID)
	if rec := shareDo(t, h, http.MethodGet, "/api/s/"+capToken+"/download", nil, cookie); rec.Code != http.StatusFound {
		t.Fatalf("spending the download: %d %s", rec.Code, rec.Body.String())
	}
	rec = shareDo(t, h, http.MethodGet, "/api/s/"+capToken+"/meta", nil, cookie)
	nodeDecode(t, rec, &meta)
	if meta.Exhausted {
		t.Error("exhausted = true for the browser whose own session counted")
	}
	rec = shareDo(t, h, http.MethodGet, "/api/s/"+capToken+"/meta", nil)
	nodeDecode(t, rec, &meta)
	if !meta.Exhausted {
		t.Error("exhausted = false for a fresh browser on a spent link")
	}
}

// ---------------------------------------------------------------- /session ----

// POST /session is idempotent per browser -- a reload is not a second
// visitor -- and refused outright on a password share, so the gate cannot be
// walked around.
func TestShareSession(t *testing.T) {
	h, pool := shareGuestServer(t)
	owner := nodeNewUser(t, pool)
	ctx := context.Background()

	fileID, _ := nodeMkFile(t, pool, owner, owner.RootID, "session.txt")
	created := sharePost(t, h, owner, map[string]any{"node_id": fileID})
	token := shareToken(t, created.URL)
	id := created.Share.ID

	rec := shareDo(t, h, http.MethodPost, "/api/s/"+token+"/session", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /session: status %d, body %s", rec.Code, rec.Body.String())
	}
	cookie := shareGuestCookie(t, rec, id)
	if !regexp.MustCompile("^[A-Za-z0-9_-]{43}$").MatchString(cookie.Value) {
		t.Errorf("cookie value %q is not 43 base64url characters", cookie.Value)
	}
	if cookie.Path != "/api/s/" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 1800 {
		t.Errorf("cookie = %+v, want Path=/api/s/, HttpOnly, Lax, Max-Age=1800", cookie)
	}
	if n := shareGuests(t, pool, id); n != 1 {
		t.Fatalf("%d guest rows, want 1", n)
	}
	var stored int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM share_guest_sessions WHERE share_id = $1 AND token_hash = $2`,
		id, auth.HashToken(cookie.Value)).Scan(&stored); err != nil || stored != 1 {
		t.Errorf("no guest row holds sha256 of the cookie (count %d, err %v)", stored, err)
	}
	if n := shareAccessCount(t, pool, id, share.ActionView); n != 1 {
		t.Errorf("%d view rows, want 1", n)
	}

	// Presented back, the session slides: no new row, no second view, the
	// same value re-set. The expiry is pulled close first so the slide shows.
	if _, err := pool.Exec(ctx,
		`UPDATE share_guest_sessions SET expires_at = now() + interval '1 minute' WHERE share_id = $1`, id); err != nil {
		t.Fatalf("backdating: %v", err)
	}
	rec = shareDo(t, h, http.MethodPost, "/api/s/"+token+"/session", nil, cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("second POST /session: status %d, body %s", rec.Code, rec.Body.String())
	}
	if again := shareGuestCookie(t, rec, id); again.Value != cookie.Value {
		t.Error("the reuse minted a different cookie")
	}
	if n := shareGuests(t, pool, id); n != 1 {
		t.Errorf("%d guest rows after the reuse, want still 1", n)
	}
	if n := shareAccessCount(t, pool, id, share.ActionView); n != 1 {
		t.Errorf("%d view rows after the reuse, want still 1", n)
	}
	var slid bool
	if err := pool.QueryRow(ctx,
		`SELECT expires_at > now() + interval '25 minutes' FROM share_guest_sessions WHERE share_id = $1`, id).Scan(&slid); err != nil || !slid {
		t.Errorf("the reuse did not slide expires_at (slid %v, err %v)", slid, err)
	}

	// A password share refuses the passwordless mint.
	pwFile, _ := nodeMkFile(t, pool, owner, owner.RootID, "session-pw.txt")
	pwResp := sharePost(t, h, owner, map[string]any{"node_id": pwFile, "password": authTestPassword})
	rec = shareDo(t, h, http.MethodPost, "/api/s/"+shareToken(t, pwResp.URL)+"/session", nil)
	nodeWant(t, rec, http.StatusUnauthorized, CodeUnauthorized)
	if n := shareGuests(t, pool, pwResp.Share.ID); n != 0 {
		t.Errorf("%d guest rows on the refused password share", n)
	}

	// The CSRF gate covers this POST -- asserted against a LIVE token,
	// because the old Mode 2 path assertion would stay green with the whole
	// group mounted outside the /api chain.
	rec = shareDoBare(t, h, http.MethodPost, "/api/s/"+token+"/session", nil, false)
	nodeWant(t, rec, http.StatusForbidden, CodeInvalid)

	// A dead link answers the one 404 and writes the denied row.
	if rec := authDo(t, h, http.MethodDelete, "/api/shares/"+id.String(), nil, owner.Cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("revoking: %d %s", rec.Code, rec.Body.String())
	}
	rec = shareDo(t, h, http.MethodPost, "/api/s/"+token+"/session", nil, cookie)
	nodeWant(t, rec, http.StatusNotFound, CodeNotFound)
	if n := shareAccessCount(t, pool, id, share.ActionDenied); n != 1 {
		t.Errorf("%d denied rows after the dead-link mint, want 1", n)
	}
}

// --------------------------------------------------------------- /password ----

// POST /password: the durable budget runs before Argon2 and is keyed
// share_id:ip -- the share alone would let anyone holding the link lock its
// real recipient out.
func TestSharePassword(t *testing.T) {
	h, pool := shareGuestServer(t)
	owner := nodeNewUser(t, pool)
	ctx := context.Background()

	fileID, _ := nodeMkFile(t, pool, owner, owner.RootID, "gate.txt")
	created := sharePost(t, h, owner, map[string]any{"node_id": fileID, "password": authTestPassword})
	token := shareToken(t, created.URL)
	id := created.Share.ID
	path := "/api/s/" + token + "/password"

	host, _, err := net.SplitHostPort(testClientAddr(t))
	if err != nil {
		t.Fatalf("splitting the test address: %v", err)
	}
	key := id.String() + ":" + host

	// Wrong: one generic 401, one denied row, one charge on share_id:ip.
	rec := shareDo(t, h, http.MethodPost, path, map[string]any{"password": "not the password"})
	nodeWant(t, rec, http.StatusUnauthorized, CodeUnauthorized)
	if n := shareAccessCount(t, pool, id, share.ActionDenied); n != 1 {
		t.Errorf("%d denied rows after the wrong password, want 1", n)
	}
	spent, err := auth.Count(ctx, pool, auth.ScopeSharePassword, key, auth.SharePasswordFailWindow)
	if err != nil || spent != 1 {
		t.Errorf("budget for the key = (%d, %v), want 1 spent", spent, err)
	}

	// Right, inside the window but under the limit: a session and one view.
	rec = shareDo(t, h, http.MethodPost, path, map[string]any{"password": authTestPassword})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("the right password: status %d, body %s", rec.Code, rec.Body.String())
	}
	cookie := shareGuestCookie(t, rec, id)
	if n := shareGuests(t, pool, id); n != 1 {
		t.Fatalf("%d guest rows, want 1", n)
	}
	if n := shareAccessCount(t, pool, id, share.ActionView); n != 1 {
		t.Errorf("%d view rows, want 1", n)
	}

	// Presented back, the gate reuses rather than minting a second session.
	rec = shareDo(t, h, http.MethodPost, path, map[string]any{"password": authTestPassword}, cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("the gate with a live cookie: status %d, body %s", rec.Code, rec.Body.String())
	}
	if n := shareGuests(t, pool, id); n != 1 {
		t.Errorf("%d guest rows after the reuse, want still 1", n)
	}
	if n := shareAccessCount(t, pool, id, share.ActionView); n != 1 {
		t.Errorf("%d view rows after the reuse, want still 1", n)
	}

	// The 11th failure from one address is refused -- even with the right
	// password -- and a second address is untouched, which is why the key
	// carries the ip.
	for range 9 {
		if _, err := auth.Bump(ctx, pool, auth.ScopeSharePassword, key, auth.SharePasswordFailWindow); err != nil {
			t.Fatalf("seeding the budget: %v", err)
		}
	}
	rec = shareDo(t, h, http.MethodPost, path, map[string]any{"password": authTestPassword})
	nodeWant(t, rec, http.StatusTooManyRequests, CodeRateLimited)
	if n := shareAccessCount(t, pool, id, share.ActionDenied); n != 2 {
		t.Errorf("%d denied rows after the lockout, want 2 (the wrong guess and the locked call)", n)
	}
	rec = abuseDo(t, h, http.MethodPost, path, map[string]any{"password": authTestPassword}, "198.51.100.9")
	if rec.Code != http.StatusNoContent {
		t.Errorf("a second address is caught in the first one's lockout: status %d, body %s", rec.Code, rec.Body.String())
	}

	// The budget is auto-clearing: a lapsed window unlocks.
	if _, err := pool.Exec(ctx,
		`UPDATE throttle SET window_start = window_start - interval '20 minutes' WHERE scope = 'share_password' AND key = $1`, key); err != nil {
		t.Fatalf("lapsing the window: %v", err)
	}
	rec = shareDo(t, h, http.MethodPost, path, map[string]any{"password": authTestPassword}, cookie)
	if rec.Code != http.StatusNoContent {
		t.Errorf("a success outside the window: status %d, body %s", rec.Code, rec.Body.String())
	}

	// 429 before Argon2, proven without a test seam: this share's stored
	// hash is malformed, so the moment Verify runs the answer is a 500 --
	// the locked answer staying 429 says the budget really is checked first.
	phcFile, _ := nodeMkFile(t, pool, owner, owner.RootID, "gate-phc.txt")
	phcResp := sharePost(t, h, owner, map[string]any{"node_id": phcFile, "password": authTestPassword})
	if _, err := pool.Exec(ctx, `UPDATE shares SET password_hash = 'not-a-phc-hash' WHERE id = $1`, phcResp.Share.ID); err != nil {
		t.Fatalf("breaking the hash: %v", err)
	}
	phcKey := phcResp.Share.ID.String() + ":" + host
	for range 10 {
		if _, err := auth.Bump(ctx, pool, auth.ScopeSharePassword, phcKey, auth.SharePasswordFailWindow); err != nil {
			t.Fatalf("seeding the budget: %v", err)
		}
	}
	rec = shareDo(t, h, http.MethodPost, "/api/s/"+shareToken(t, phcResp.URL)+"/password", map[string]any{"password": authTestPassword})
	nodeWant(t, rec, http.StatusTooManyRequests, CodeRateLimited)

	// A passwordless link given a password is a 422, not a hash run.
	openFile, _ := nodeMkFile(t, pool, owner, owner.RootID, "gate-open.txt")
	openResp := sharePost(t, h, owner, map[string]any{"node_id": openFile})
	rec = shareDo(t, h, http.MethodPost, "/api/s/"+shareToken(t, openResp.URL)+"/password", map[string]any{"password": authTestPassword})
	nodeWant(t, rec, http.StatusUnprocessableEntity, CodeInvalid)
}

// --------------------------------------------------------------- /download ----

// GET /download is a browser navigation: every refusal is a redirect back to
// the page, the cap counts once per guest session, and the loser of the cap
// keeps its NULL stamp so a raised cap lets it through later.
func TestShareDownload(t *testing.T) {
	h, pool := shareGuestServer(t)
	owner := nodeNewUser(t, pool)
	ctx := context.Background()
	bucket := uploadTestConfig(t).S3Bucket

	fileID, blobID := nodeMkFile(t, pool, owner, owner.RootID, "down.txt")
	created := sharePost(t, h, owner, map[string]any{"node_id": fileID})
	token := shareToken(t, created.URL)
	id := created.Share.ID
	path := "/api/s/" + token + "/download"

	// No cookie: back to the page, never JSON -- a person following a link
	// cannot act on the error envelope.
	rec := shareDo(t, h, http.MethodGet, path, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("no cookie: status %d, want 302 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/s/"+token+"?reason=session" {
		t.Errorf("no cookie: Location = %q, want the page with reason=session", got)
	}
	// The owner's own signed-in session grants a share page nothing a
	// stranger would not get.
	rec = authDo(t, h, http.MethodGet, path, nil, owner.Cookie)
	if got := rec.Header().Get("Location"); rec.Code != http.StatusFound || got != "/s/"+token+"?reason=session" {
		t.Errorf("the owner's cookie: status %d Location %q, want the same reason=session redirect", rec.Code, got)
	}

	cookie := shareMint(t, h, token, id)
	rec = shareDo(t, h, http.MethodGet, path, nil, cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("download: status %d, body %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location is not a URL: %v", err)
	}
	if loc.Path != "/"+bucket+"/blobs/"+blobID.String() {
		t.Errorf("the redirect points at %q, not this file's object", loc.Path)
	}
	q := loc.Query()
	if got, want := q.Get("response-content-disposition"), upload.AttachmentDisposition("down.txt"); got != want {
		t.Errorf("response-content-disposition = %q, want %q", got, want)
	}
	if got := q.Get("response-content-type"); got != upload.DownloadContentType {
		t.Errorf("response-content-type = %q, want %q", got, upload.DownloadContentType)
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Error("the redirect target is not signed")
	}
	if n := shareAccessCount(t, pool, id, share.ActionDownload); n != 1 {
		t.Errorf("%d download rows, want 1", n)
	}
	if n := shareCounter(t, pool, id); n != 1 {
		t.Errorf("download_count = %d, want 1", n)
	}
	if c := shareGuestCookie(t, rec, id); c.Value != cookie.Value {
		t.Error("the slide re-set a different cookie")
	}

	// A guest of share A presented at share B -- even under B's own cookie
	// name -- is nobody there: the row behind it still says share_id = A.
	otherFile, _ := nodeMkFile(t, pool, owner, owner.RootID, "down-other.txt")
	other := sharePost(t, h, owner, map[string]any{"node_id": otherFile})
	otherToken := shareToken(t, other.URL)
	forged := &http.Cookie{Name: "gs_" + other.Share.ID.String(), Value: cookie.Value}
	rec = shareDo(t, h, http.MethodGet, "/api/s/"+otherToken+"/download", nil, forged)
	if got := rec.Header().Get("Location"); rec.Code != http.StatusFound || got != "/s/"+otherToken+"?reason=session" {
		t.Errorf("a forged cookie name: status %d Location %q, want reason=session", rec.Code, got)
	}

	// A dead link redirects to reason=gone and writes the denied row -- the
	// session, live though it is, never gets a say.
	deadFile, _ := nodeMkFile(t, pool, owner, owner.RootID, "down-dead.txt")
	dead := sharePost(t, h, owner, map[string]any{"node_id": deadFile})
	deadToken := shareToken(t, dead.URL)
	deadCookie := shareMint(t, h, deadToken, dead.Share.ID)
	if _, err := pool.Exec(ctx, `UPDATE shares SET expires_at = now() - interval '1 hour' WHERE id = $1`, dead.Share.ID); err != nil {
		t.Fatalf("expiring: %v", err)
	}
	rec = shareDo(t, h, http.MethodGet, "/api/s/"+deadToken+"/download", nil, deadCookie)
	if got := rec.Header().Get("Location"); rec.Code != http.StatusFound || got != "/s/"+deadToken+"?reason=gone" {
		t.Errorf("an expired link: status %d Location %q, want reason=gone", rec.Code, got)
	}
	if n := shareAccessCount(t, pool, dead.Share.ID, share.ActionDenied); n != 1 {
		t.Errorf("%d denied rows for the expired link, want 1", n)
	}

	// The cap: three sessions against max_downloads 2.
	capFile, _ := nodeMkFile(t, pool, owner, owner.RootID, "down-cap.txt")
	capResp := sharePost(t, h, owner, map[string]any{"node_id": capFile, "max_downloads": 2})
	capToken := shareToken(t, capResp.URL)
	capPath := "/api/s/" + capToken + "/download"
	capID := capResp.Share.ID
	first := shareMint(t, h, capToken, capID)
	second := shareMint(t, h, capToken, capID)
	third := shareMint(t, h, capToken, capID)
	for i, c := range []*http.Cookie{first, second} {
		rec := shareDo(t, h, http.MethodGet, capPath, nil, c)
		if rec.Code != http.StatusFound || strings.HasPrefix(rec.Header().Get("Location"), "/s/") {
			t.Fatalf("download %d: status %d Location %q, want the store", i+1, rec.Code, rec.Header().Get("Location"))
		}
	}
	rec = shareDo(t, h, http.MethodGet, capPath, nil, third)
	if got := rec.Header().Get("Location"); rec.Code != http.StatusFound || got != "/s/"+capToken+"?reason=exhausted" {
		t.Errorf("the third session: status %d Location %q, want reason=exhausted", rec.Code, got)
	}
	if n := shareAccessCount(t, pool, capID, share.ActionDenied); n != 1 {
		t.Errorf("%d denied rows for the spent cap, want 1", n)
	}
	if n := shareCounter(t, pool, capID); n != 2 {
		t.Errorf("download_count = %d, want 2", n)
	}
	var stamped bool
	if err := pool.QueryRow(ctx,
		`SELECT downloaded_at IS NOT NULL FROM share_guest_sessions WHERE share_id = $1 AND token_hash = $2`,
		capID, auth.HashToken(third.Value)).Scan(&stamped); err != nil {
		t.Fatalf("reading the refused session's stamp: %v", err)
	}
	if stamped {
		t.Error("the refused session kept its stamp -- the rollback did not happen")
	}
	// Once per session: the first re-issues without counting again.
	rec = shareDo(t, h, http.MethodGet, capPath, nil, first)
	if rec.Code != http.StatusFound || strings.HasPrefix(rec.Header().Get("Location"), "/s/") {
		t.Fatalf("re-issue: status %d Location %q, want the store", rec.Code, rec.Header().Get("Location"))
	}
	if n := shareCounter(t, pool, capID); n != 2 {
		t.Errorf("download_count = %d after the re-issue, want still 2", n)
	}
	if n := shareAccessCount(t, pool, capID, share.ActionDownload); n != 3 {
		t.Errorf("%d download rows, want 3 (two firsts and a re-issue)", n)
	}

	// Every issue slides the session: pull the expiry close, download, and
	// the row is half an hour out again.
	if _, err := pool.Exec(ctx,
		`UPDATE share_guest_sessions SET expires_at = now() + interval '1 minute' WHERE share_id = $1 AND token_hash = $2`,
		capID, auth.HashToken(first.Value)); err != nil {
		t.Fatalf("backdating: %v", err)
	}
	if rec := shareDo(t, h, http.MethodGet, capPath, nil, first); rec.Code != http.StatusFound {
		t.Fatalf("the sliding download: %d", rec.Code)
	}
	var slid bool
	if err := pool.QueryRow(ctx,
		`SELECT expires_at > now() + interval '25 minutes' FROM share_guest_sessions WHERE share_id = $1 AND token_hash = $2`,
		capID, auth.HashToken(first.Value)).Scan(&slid); err != nil || !slid {
		t.Errorf("the download did not slide the session (slid %v, err %v)", slid, err)
	}
}

// ---------------------------------------------------------------- /preview ----

// GET /preview is an XHR: JSON refusals, an explicit X-Drive-Client
// requirement (the chain exempts GETs), the share allowlist (no PDF for a
// stranger), and a counter it never moves -- though a spent one refuses any
// browser that has not itself counted.
func TestSharePreview(t *testing.T) {
	h, pool := shareGuestServer(t)
	owner := nodeNewUser(t, pool)
	bucket := uploadTestConfig(t).S3Bucket

	pngID, pngBlob := shareMkTypedFile(t, pool, owner, "pic.png", "image/png")
	created := sharePost(t, h, owner, map[string]any{"node_id": pngID})
	token := shareToken(t, created.URL)
	id := created.Share.ID
	path := "/api/s/" + token + "/preview"

	// No session is a 401 -- and the owner's own cookie is nobody here too.
	rec := shareDo(t, h, http.MethodGet, path, nil)
	nodeWant(t, rec, http.StatusUnauthorized, CodeUnauthorized)
	rec = authDo(t, h, http.MethodGet, path, nil, owner.Cookie)
	nodeWant(t, rec, http.StatusUnauthorized, CodeUnauthorized)

	cookie := shareMint(t, h, token, id)

	// A state-touching GET requires the client header explicitly, because
	// RequireClientHeader exempts GETs.
	rec = shareDoBare(t, h, http.MethodGet, path, nil, false, cookie)
	nodeWant(t, rec, http.StatusForbidden, CodeInvalid)

	rec = shareDo(t, h, http.MethodGet, path, nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: status %d, body %s", rec.Code, rec.Body.String())
	}
	var link struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
		Mime      string    `json:"mime"`
	}
	nodeDecode(t, rec, &link)
	if link.Mime != "image/png" {
		t.Errorf("mime = %q, want the allowlist's constant image/png", link.Mime)
	}
	loc, err := url.Parse(link.URL)
	if err != nil {
		t.Fatalf("url is not a URL: %v", err)
	}
	if loc.Path != "/"+bucket+"/blobs/"+pngBlob.String() {
		t.Errorf("the URL points at %q, not this file's object", loc.Path)
	}
	q := loc.Query()
	if got, want := q.Get("response-content-disposition"), upload.InlineDisposition("pic.png"); got != want {
		t.Errorf("response-content-disposition = %q, want %q", got, want)
	}
	if got := q.Get("response-content-type"); got != "image/png" {
		t.Errorf("response-content-type = %q, want image/png", got)
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Error("the URL is not signed")
	}
	// The bytes were issued -- one download row -- but the counter never
	// moves for a preview.
	if n := shareAccessCount(t, pool, id, share.ActionDownload); n != 1 {
		t.Errorf("%d download rows after the preview, want 1 (the URL was issued)", n)
	}
	if n := shareCounter(t, pool, id); n != 0 {
		t.Errorf("download_count = %d after a preview, want 0", n)
	}
	if c := shareGuestCookie(t, rec, id); c.Value != cookie.Value {
		t.Error("the slide re-set a different cookie")
	}

	// No PDF and no SVG for a stranger, whatever the owner's own preview
	// dialog accepts.
	for _, c := range []struct{ name, mime string }{
		{"doc.pdf", "application/pdf"},
		{"vec.svg", "image/svg+xml"},
	} {
		fid, _ := shareMkTypedFile(t, pool, owner, c.name, c.mime)
		resp := sharePost(t, h, owner, map[string]any{"node_id": fid})
		tok := shareToken(t, resp.URL)
		ck := shareMint(t, h, tok, resp.Share.ID)
		rec := shareDo(t, h, http.MethodGet, "/api/s/"+tok+"/preview", nil, ck)
		nodeWant(t, rec, http.StatusUnsupportedMediaType, CodeUnsupported)
	}

	// A spent cap refuses a session that has not itself counted -- 403
	// exhausted, a denied row -- and still serves the one that did.
	spentID, _ := shareMkTypedFile(t, pool, owner, "spent.png", "image/png")
	spentResp := sharePost(t, h, owner, map[string]any{"node_id": spentID, "max_downloads": 1})
	spentToken := shareToken(t, spentResp.URL)
	spender := shareMint(t, h, spentToken, spentResp.Share.ID)
	fresh := shareMint(t, h, spentToken, spentResp.Share.ID)
	if rec := shareDo(t, h, http.MethodGet, "/api/s/"+spentToken+"/download", nil, spender); rec.Code != http.StatusFound {
		t.Fatalf("spending the cap: %d %s", rec.Code, rec.Body.String())
	}
	rec = shareDo(t, h, http.MethodGet, "/api/s/"+spentToken+"/preview", nil, fresh)
	nodeWant(t, rec, http.StatusForbidden, CodeExhausted)
	if n := shareAccessCount(t, pool, spentResp.Share.ID, share.ActionDenied); n != 1 {
		t.Errorf("%d denied rows for the refused preview, want 1", n)
	}
	rec = shareDo(t, h, http.MethodGet, "/api/s/"+spentToken+"/preview", nil, spender)
	if rec.Code != http.StatusOK {
		t.Errorf("the counting session's own preview: status %d, body %s", rec.Code, rec.Body.String())
	}
	if n := shareCounter(t, pool, spentResp.Share.ID); n != 1 {
		t.Errorf("download_count = %d, want still 1", n)
	}
}

// ---------------------------------------------------------------- headers ----

// Every answer from the five routes carries the three share headers: the
// group middleware writes them before any handler runs, so WriteErr's
// refusals and the redirects carry them too.
func TestShareRouteAnswersCarryTheHeaders(t *testing.T) {
	h, pool := shareGuestServer(t)
	owner := nodeNewUser(t, pool)
	fileID, _ := nodeMkFile(t, pool, owner, owner.RootID, "headers.txt")
	created := sharePost(t, h, owner, map[string]any{"node_id": fileID})
	token := shareToken(t, created.URL)

	cases := []struct {
		what string
		rec  *httptest.ResponseRecorder
		want int
	}{
		{"a live /meta", shareDo(t, h, http.MethodGet, "/api/s/"+token+"/meta", nil), http.StatusOK},
		{"an unknown /meta", shareDo(t, h, http.MethodGet, "/api/s/"+strings.Repeat("A", 43)+"/meta", nil), http.StatusNotFound},
		{"a /session mint", shareDo(t, h, http.MethodPost, "/api/s/"+token+"/session", nil), http.StatusNoContent},
		{"a /download refusal", shareDo(t, h, http.MethodGet, "/api/s/"+token+"/download", nil), http.StatusFound},
		{"a /preview without the header", shareDoBare(t, h, http.MethodGet, "/api/s/"+token+"/preview", nil, false), http.StatusForbidden},
		{"a /password 422", shareDo(t, h, http.MethodPost, "/api/s/"+token+"/password", map[string]any{"password": "irrelevant guess"}), http.StatusUnprocessableEntity},
		{"an unmatched subpath", shareDo(t, h, http.MethodGet, "/api/s/"+token+"/nope", nil), http.StatusNotFound},
	}
	for _, c := range cases {
		if c.rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (body %s)", c.what, c.rec.Code, c.want, c.rec.Body.String())
		}
		assertShareHeaders(t, c.what, c.rec.Header())
	}
}

// ---------------------------------------------------------------- logging ----

// One whole round trip through a Debug logger: the raw token and the raw
// cookie appear in no line at any level, refusals carry their constant
// reasons, and the request logger's path is redacted.
func TestShareRoundTripLogsNoToken(t *testing.T) {
	_, pool := shareGuestServer(t) // primes the shared S3 client
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(uploadTestConfig(t), pool, logger, nil, uploadS3Client, uploadS3Presign)
	h := s.Routes()

	owner := nodeNewUser(t, pool)
	pngID, _ := shareMkTypedFile(t, pool, owner, "trip.png", "image/png")
	store := share.NewStore(pool)
	sh, token, err := store.Create(context.Background(), owner.ID, pngID, share.Settings{})
	if err != nil {
		t.Fatalf("creating the share: %v", err)
	}

	if rec := shareDo(t, h, http.MethodGet, "/api/s/"+token+"/meta", nil); rec.Code != http.StatusOK {
		t.Fatalf("/meta: %d %s", rec.Code, rec.Body.String())
	}
	rec := shareDo(t, h, http.MethodPost, "/api/s/"+token+"/session", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("/session: %d %s", rec.Code, rec.Body.String())
	}
	cookie := shareGuestCookie(t, rec, sh.ID)
	if rec := shareDo(t, h, http.MethodGet, "/api/s/"+token+"/preview", nil, cookie); rec.Code != http.StatusOK {
		t.Fatalf("/preview: %d %s", rec.Code, rec.Body.String())
	}
	if rec := shareDo(t, h, http.MethodGet, "/api/s/"+token+"/download", nil, cookie); rec.Code != http.StatusFound {
		t.Fatalf("/download: %d %s", rec.Code, rec.Body.String())
	}
	// A handler refusal, a bucket refusal, and a dead-link 404, so every
	// kind of line is in the buffer.
	if rec := shareDo(t, h, http.MethodGet, "/api/s/"+token+"/download", nil); rec.Code != http.StatusFound {
		t.Fatalf("the no-session refusal: %d", rec.Code)
	}
	host, _, err := net.SplitHostPort(testClientAddr(t))
	if err != nil {
		t.Fatalf("splitting the test address: %v", err)
	}
	for s.ShareRate.allow(host) { //nolint:revive // draining the bucket
	}
	if rec := shareDo(t, h, http.MethodGet, "/api/s/"+token+"/meta", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the bucket refusal: %d", rec.Code)
	}
	s.ShareRate = newIPLimiter(60, 120) // room for the rest
	if err := store.Revoke(context.Background(), owner.ID, sh.ID); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if rec := shareDo(t, h, http.MethodGet, "/api/s/"+token+"/meta", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("the revoked 404: %d", rec.Code)
	}

	out := logs.String()
	if strings.Contains(out, token) {
		t.Fatalf("the raw token reached the log: %s", out)
	}
	if strings.Contains(out, cookie.Value) {
		t.Fatalf("the raw guest cookie reached the log: %s", out)
	}
	for _, want := range []string{
		"reason=no_session",
		"reason=revoked",
		"share request refused by the per-IP bucket",
		"path=/api/s/{redacted}/meta",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the log is missing %q: %s", want, out)
		}
	}
}
