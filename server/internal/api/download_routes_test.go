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
	"strconv"
	"strings"
	"testing"
	"time"

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
	if got, want := q.Get("X-Amz-Expires"), presignTTLSeconds(t); got != want {
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

// ------------------------------------------------------- preview and ?format --

// previewMkFile is nodeMkFile plus the one column these cases are about: the
// client-declared MIME, which nodeMkFile always sets to text/plain. A nil mime
// stores SQL NULL, which is what a file uploaded without a declared type has.
func previewMkFile(t *testing.T, pool *pgxpool.Pool, owner nodeUser, name string, mime *string) uuid.UUID {
	t.Helper()
	id, _ := nodeMkFile(t, pool, owner, owner.RootID, name)
	if _, err := pool.Exec(context.Background(), `UPDATE nodes SET mime = $1 WHERE id = $2`, mime, id); err != nil {
		t.Fatalf("setting mime %v on %q: %v", mime, name, err)
	}

	// Read it back. A nil that arrived as the empty string would leave the
	// "no mime at all" case silently testing the empty-string case instead --
	// both refuse, so nothing downstream would ever notice.
	var stored *string
	if err := pool.QueryRow(context.Background(), `SELECT mime FROM nodes WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("reading back the mime of %q: %v", name, err)
	}
	switch {
	case mime == nil && stored != nil:
		t.Fatalf("%q was asked for a null mime and stored %q", name, *stored)
	case mime != nil && (stored == nil || *stored != *mime):
		t.Fatalf("%q was asked for mime %q and stored %v", name, *mime, stored)
	}
	return id
}

func previewMime(s string) *string { return &s }

// presignTTLSeconds is what X-Amz-Expires must say, taken from the config the
// server was built with rather than written out: a TTL change in .env.test or in
// the config defaults must move this assertion with it, not break it.
func presignTTLSeconds(t *testing.T) string {
	t.Helper()
	return strconv.Itoa(int(uploadTestConfig(t).PresignTTL.Seconds()))
}

// The happy path: an allowlisted type gets an inline URL that carries the
// allowlist's constant, not the string the uploader declared.
//
// The two cases are the two halves of that claim. A PDF proves the type survives
// normalization (mixed case, a parameter) and comes back as the map's own
// spelling; a markdown file proves a type that is *not* its own constant is
// rewritten -- text/markdown must go out as text/plain, or the browser downloads
// it instead of showing it.
func TestPreviewSignsAnInlineURLCarryingTheAllowlistedType(t *testing.T) {
	h, pool := downloadTestServer(t)
	owner := nodeNewUser(t, pool)

	cases := []struct {
		what     string
		name     string
		declared string
		want     string
	}{
		{"a pdf, declared with mixed case and a parameter", `مرحبا "quarterly" 🚀.pdf`, "Application/PDF; charset=binary", "application/pdf"},
		{"markdown, which is served as plain text", "notes.md", "text/markdown", "text/plain"},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			id := previewMkFile(t, pool, owner, c.name, previewMime(c.declared))

			rec := authDo(t, h, http.MethodGet, "/api/files/"+id.String()+"/preview", nil, owner.Cookie)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
				t.Errorf("Cache-Control = %q, want %q", got, "private, no-store")
			}

			var body previewLink
			nodeDecode(t, rec, &body)
			if body.Mime != c.want {
				t.Errorf("mime = %q, want %q (the allowlist's constant, not the stored value)", body.Mime, c.want)
			}
			if d := time.Until(body.ExpiresAt); d <= 0 || d > uploadTestConfig(t).PresignTTL {
				t.Errorf("expires_at is %v away, want (0, %v]", d, uploadTestConfig(t).PresignTTL)
			}

			loc, err := url.Parse(body.URL)
			if err != nil {
				t.Fatalf("url is not a URL: %v", err)
			}
			q := loc.Query()
			// The whole control: whatever the client declared, the signature
			// commits the store to answering with the allowlist's constant.
			if got := q.Get("response-content-type"); got != c.want {
				t.Errorf("response-content-type = %q, want %q", got, c.want)
			}
			if got, want := q.Get("response-content-disposition"), upload.InlineDisposition(c.name); got != want {
				t.Errorf("response-content-disposition = %q, want %q", got, want)
			}
			if q.Get("X-Amz-Signature") == "" {
				t.Error("the preview URL is not signed")
			}
			if got, want := q.Get("X-Amz-Expires"), presignTTLSeconds(t); got != want {
				t.Errorf("X-Amz-Expires = %q, want %q (the config's PresignTTL)", got, want)
			}
		})
	}
}

// Everything the allowlist refuses gets 415 and, crucially, no URL: a preview
// that answered 200 for text/html or image/svg+xml would be script running on
// the store origin. A folder is a 404 like it is on download -- it is not "a
// type without a preview", it is not a file at all.
func TestPreviewRefusesEverythingOffTheAllowlist(t *testing.T) {
	h, pool := downloadTestServer(t)
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)

	htmlID := previewMkFile(t, pool, owner, "page.html", previewMime("text/html"))
	svgID := previewMkFile(t, pool, owner, "logo.svg", previewMime("image/svg+xml"))
	emptyID := previewMkFile(t, pool, owner, "mystery.bin", previewMime(""))
	nullID := previewMkFile(t, pool, owner, "untyped.bin", nil)
	pngID := previewMkFile(t, pool, owner, "photo.png", previewMime("image/png"))
	folderID := nodeMkFolder(t, pool, owner, owner.RootID, "Docs")

	trashedID := previewMkFile(t, pool, owner, "gone.png", previewMime("image/png"))
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
		{"html", htmlID.String(), owner.Cookie, http.StatusUnsupportedMediaType, CodeUnsupported},
		{"svg", svgID.String(), owner.Cookie, http.StatusUnsupportedMediaType, CodeUnsupported},
		{"an empty mime", emptyID.String(), owner.Cookie, http.StatusUnsupportedMediaType, CodeUnsupported},
		{"no mime at all", nullID.String(), owner.Cookie, http.StatusUnsupportedMediaType, CodeUnsupported},
		{"a folder", folderID.String(), owner.Cookie, http.StatusNotFound, CodeNotFound},
		{"a trashed file", trashedID.String(), owner.Cookie, http.StatusNotFound, CodeNotFound},
		{"someone else's file", pngID.String(), stranger.Cookie, http.StatusNotFound, CodeNotFound},
		{"an id that never existed", uuid.NewString(), owner.Cookie, http.StatusNotFound, CodeNotFound},
		{"an id that is not a UUID", "not-a-uuid", owner.Cookie, http.StatusNotFound, CodeNotFound},
		{"an anonymous caller", pngID.String(), nil, http.StatusUnauthorized, CodeUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			rec := authDo(t, h, http.MethodGet, "/api/files/"+c.id+"/preview", nil, c.cookie)
			nodeWant(t, rec, c.status, c.code)
			if body := rec.Body.String(); strings.Contains(body, "X-Amz-Signature") {
				t.Errorf("a refused preview still handed out a signed URL: %s", body)
			}
		})
	}
}

// ?format=json must be the redirect in a body, not a second way of signing.
//
// Every query parameter is compared except the two that are a function of the
// moment the URL was signed, so a JSON branch that dropped the attachment
// disposition -- an inline URL for arbitrary uploaded bytes, by another name --
// cannot pass.
func TestDownloadFormatJSONSignsTheSameURLAsTheRedirect(t *testing.T) {
	h, pool := downloadTestServer(t)
	owner := nodeNewUser(t, pool)

	const name = `مرحبا "quarterly" 🚀.html`
	id := previewMkFile(t, pool, owner, name, previewMime("text/html"))

	redirect := authDo(t, h, http.MethodGet, "/api/files/"+id.String()+"/download", nil, owner.Cookie)
	if redirect.Code != http.StatusFound {
		t.Fatalf("plain download status = %d, want 302 (body %s)", redirect.Code, redirect.Body.String())
	}
	asJSON := authDo(t, h, http.MethodGet, "/api/files/"+id.String()+"/download?format=json", nil, owner.Cookie)
	if asJSON.Code != http.StatusOK {
		t.Fatalf("?format=json status = %d, want 200 (body %s)", asJSON.Code, asJSON.Body.String())
	}
	if got := asJSON.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "private, no-store")
	}
	if loc := asJSON.Header().Get("Location"); loc != "" {
		t.Errorf("?format=json also redirected, to %q", loc)
	}

	var body downloadLink
	nodeDecode(t, asJSON, &body)
	if d := time.Until(body.ExpiresAt); d <= 0 || d > uploadTestConfig(t).PresignTTL {
		t.Errorf("expires_at is %v away, want (0, %v]", d, uploadTestConfig(t).PresignTTL)
	}

	signed, err := url.Parse(body.URL)
	if err != nil {
		t.Fatalf("url is not a URL: %v", err)
	}
	redirected, err := url.Parse(redirect.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location is not a URL: %v", err)
	}
	if signed.Path != redirected.Path {
		t.Errorf("the two forms point at different objects: %q vs %q", signed.Path, redirected.Path)
	}

	// X-Amz-Date, the signature over it, and the credential -- whose scope
	// carries the yyyymmdd of the signing date -- move with the clock; everything
	// else is the request the URL commits the store to. (Comparing the credential
	// would pass all day and fail on the two calls that straddle UTC midnight.)
	volatile := map[string]bool{"X-Amz-Date": true, "X-Amz-Signature": true, "X-Amz-Credential": true}
	sq, rq := signed.Query(), redirected.Query()
	if len(sq) != len(rq) {
		t.Fatalf("query parameters differ: json %v, redirect %v", sq, rq)
	}
	for k, want := range rq {
		got := sq[k]
		if volatile[k] {
			if len(got) != 1 || got[0] == "" {
				t.Errorf("%s is missing from the json form", k)
			}
			continue
		}
		if len(got) != len(want) || (len(want) == 1 && got[0] != want[0]) {
			t.Errorf("%s = %v in the json form, want %v (the redirect's)", k, got, want)
		}
	}
	// The credential moves only in its date scope, so it is compared without it
	// rather than not compared at all: the key id and the rest of the scope
	// still have to be the same two URLs' worth.
	credWithoutDate := func(what string, q url.Values) string {
		cred, stamp := q.Get("X-Amz-Credential"), q.Get("X-Amz-Date")
		if len(stamp) < 8 || !strings.Contains(cred, "/"+stamp[:8]+"/") {
			t.Errorf("the %s credential %q does not carry its own signing date %q", what, cred, stamp)
			return cred
		}
		return strings.Replace(cred, "/"+stamp[:8]+"/", "/", 1)
	}
	if got, want := credWithoutDate("json", sq), credWithoutDate("redirect", rq); got != want {
		t.Errorf("the two forms sign under different credentials: %q vs %q", got, want)
	}

	if got, want := sq.Get("response-content-disposition"), upload.AttachmentDisposition(name); got != want {
		t.Errorf("response-content-disposition = %q, want %q", got, want)
	}
	if got := sq.Get("response-content-type"); got != upload.DownloadContentType {
		t.Errorf("response-content-type = %q, want %q", got, upload.DownloadContentType)
	}
}

// An unknown format is refused before anything is looked up or signed. The
// empty value is the one exception: ?format= is read as "not asked for", which
// is what an absent parameter and an empty one mean to the same Get() call.
func TestDownloadRefusesAFormatItDoesNotKnow(t *testing.T) {
	h, pool := downloadTestServer(t)
	owner := nodeNewUser(t, pool)
	id := previewMkFile(t, pool, owner, "report.pdf", previewMime("application/pdf"))

	for _, format := range []string{"xml", "JSON", "json2", "302", "html"} {
		t.Run(format, func(t *testing.T) {
			rec := authDo(t, h, http.MethodGet,
				"/api/files/"+id.String()+"/download?format="+url.QueryEscape(format), nil, owner.Cookie)
			nodeWant(t, rec, http.StatusUnprocessableEntity, CodeInvalid)
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("a refused format still leaked a URL: %q", loc)
			}
			if body := rec.Body.String(); strings.Contains(body, "X-Amz-Signature") {
				t.Errorf("a refused format still handed out a signed URL: %s", body)
			}
		})
	}

	t.Run("an empty format is the same as no format", func(t *testing.T) {
		rec := authDo(t, h, http.MethodGet, "/api/files/"+id.String()+"/download?format=", nil, owner.Cookie)
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302 (body %s)", rec.Code, rec.Body.String())
		}
	})
}
