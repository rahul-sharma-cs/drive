package api

// GET /api/files/{id}/download through the whole router.
//
// The redirect is built here and the bytes are not: what these cases prove is
// that the endpoint authorizes before it signs, that the signature carries the
// two response-content-* overrides the safety posture depends on, and that the
// URL -- a one-hour bearer credential for one object -- is never handed to a
// cache. Whether the store honours the overrides is the upload package's
// download test, which runs against Garage and R2 alike.

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/upload"
)

// downloadTestServer builds the whole router -- Routes(), not a hand-assembled
// chain -- so a download route that is written but never mounted fails here.
func downloadTestServer(t *testing.T) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := authTestPool(t)
	cfg := uploadTestConfig(t)

	uploadS3Once.Do(func() {
		uploadS3Client, uploadS3Presign, uploadS3Err = blob.New(context.Background(), cfg)
	})
	if uploadS3Err != nil {
		t.Fatalf("drive-test Garage: %v (is `docker compose -p drive-test --env-file .env.test up -d` running?)", uploadS3Err)
	}
	return New(cfg, pool, nil, nil, uploadS3Client, uploadS3Presign).Routes(), pool
}

// The happy path, and every property of the redirect that matters.
func TestDownloadRedirectsToASignedURLCarryingTheOverrides(t *testing.T) {
	h, pool := downloadTestServer(t)
	owner := nodeNewUser(t, pool)

	const name = `مرحبا "quarterly" 🚀.html`
	fileID, blobID := nodeMkFile(t, pool, owner, owner.RootID, name)

	rec := authDo(t, h, http.MethodGet, "/api/files/"+fileID.String()+"/download", nil, owner.Cookie)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body %s)", rec.Code, rec.Body.String())
	}

	// The URL is a credential: a shared cache holding it would hand the object
	// to whoever asks next.
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "private, no-store")
	}

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location is not a URL: %v", err)
	}
	if loc.Path != "/"+uploadTestConfig(t).S3Bucket+"/blobs/"+blobID.String() {
		t.Errorf("the redirect points at %q, which is not this file's object", loc.Path)
	}

	q := loc.Query()
	if got, want := q.Get("response-content-disposition"), upload.AttachmentDisposition(name); got != want {
		t.Errorf("response-content-disposition = %q, want %q", got, want)
	}
	if got := q.Get("response-content-type"); got != upload.DownloadContentType {
		t.Errorf("response-content-type = %q, want %q", got, upload.DownloadContentType)
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Error("the redirect target is not signed")
	}
	if got, want := q.Get("X-Amz-Expires"), "900"; got != want {
		t.Errorf("X-Amz-Expires = %q, want %q (the config's PresignTTL)", got, want)
	}
}

// Everything that is not "a live file of mine" is the same 404: a folder,
// somebody else's file, a trashed one, an id that never existed, and an id that
// is not a UUID at all. Anonymous callers never get that far.
func TestDownloadIsOwnerOnlyLiveFilesOnly(t *testing.T) {
	h, pool := downloadTestServer(t)
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)

	fileID, _ := nodeMkFile(t, pool, owner, owner.RootID, "mine.bin")
	folderID := nodeMkFolder(t, pool, owner, owner.RootID, "Docs")

	trashedID, _ := nodeMkFile(t, pool, owner, owner.RootID, "gone.bin")
	if _, err := pool.Exec(context.Background(),
		`UPDATE nodes SET deleted_at = now(), trashed_root = true WHERE id = $1`, trashedID); err != nil {
		t.Fatalf("trashing the file: %v", err)
	}

	cases := []struct {
		what   string
		id     string
		cookie *http.Cookie
		status int
		code   string
	}{
		{"a folder", folderID.String(), owner.Cookie, http.StatusNotFound, CodeNotFound},
		{"a trashed file", trashedID.String(), owner.Cookie, http.StatusNotFound, CodeNotFound},
		{"someone else's file", fileID.String(), stranger.Cookie, http.StatusNotFound, CodeNotFound},
		{"an id that never existed", uuid.NewString(), owner.Cookie, http.StatusNotFound, CodeNotFound},
		{"an id that is not a UUID", "not-a-uuid", owner.Cookie, http.StatusNotFound, CodeNotFound},
		{"an anonymous caller", fileID.String(), nil, http.StatusUnauthorized, CodeUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			rec := authDo(t, h, http.MethodGet, "/api/files/"+c.id+"/download", nil, c.cookie)
			nodeWant(t, rec, c.status, c.code)
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("a refused download still leaked a URL: %q", loc)
			}
		})
	}
}

// A file whose blob row is gone must 404 rather than sign a URL for bytes that
// are not there -- the inner join is what makes that true, and dropping it
// would turn this into a 302 to a 404 the client cannot interpret.
func TestDownloadOfAFileWithNoBlobIsAMiss(t *testing.T) {
	h, pool := downloadTestServer(t)
	owner := nodeNewUser(t, pool)

	fileID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name, size)
		 VALUES ($1, $2, $3, 'file', 'orphan.bin', 0)`,
		fileID, owner.ID, owner.RootID); err != nil {
		t.Fatalf("inserting the blobless file: %v", err)
	}

	rec := authDo(t, h, http.MethodGet, "/api/files/"+fileID.String()+"/download", nil, owner.Cookie)
	nodeWant(t, rec, http.StatusNotFound, CodeNotFound)
}
