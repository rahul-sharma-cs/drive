package api

// The owner's share routes through the whole router: real cookies, real
// RequireAuth, real SQL against the drive-test Postgres. The public
// /api/s/{token}/* routes have their own file when they land; what belongs
// here is the owner surface's contract -- the URL shown once, the one 409,
// the full-triple PATCH, the 404 for everything that is not the caller's
// live share, and the two writers sitting in the auth bucket.

import (
	"context"
	"net"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/auth"
	"github.com/rahul-sharma-cs/drive/server/internal/share"
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
