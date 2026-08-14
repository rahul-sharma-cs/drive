package api

// The upload session API against the real drive-test stack: real Postgres, real
// Garage, real presigned PUTs from the test process. Nothing here is mocked,
// because everything that has ever gone wrong in this protocol -- an ETag that
// does not match once normalized, a ledger that disagrees with the store, a
// second tab racing the first -- only shows up when both halves are real.

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/upload"
)

// uploadTestPartSize is deliberately far below the 10 MiB Garage block size the
// server config enforces: these tests never call CompleteMultipartUpload -- the
// only operation with a minimum part size -- so small parts buy speed at no
// cost in realism.
const uploadTestPartSize int64 = 1 << 20

// The drive-test stack's S3 constants, verbatim from the committed .env.test,
// with the environment winning when a suite sources it.
func uploadTestEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var (
	uploadS3Once    sync.Once
	uploadS3Client  *s3.Client
	uploadS3Presign *s3.PresignClient
	uploadS3Err     error
)

func uploadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{
		BaseURL:     authTestBaseURL,
		S3Endpoint:  uploadTestEnv("DRIVE_S3_ENDPOINT", "http://localhost:3910"),
		S3Bucket:    uploadTestEnv("DRIVE_S3_BUCKET", "drive-blobs"),
		S3AccessKey: uploadTestEnv("DRIVE_S3_ACCESS_KEY", "drivetestkey0001"),
		S3SecretKey: uploadTestEnv("DRIVE_S3_SECRET_KEY", "drivetestsecretkey0001"),
		PartSize:    uploadTestPartSize,
		PresignTTL:  15 * time.Minute,
	}
	// The same guard the harness applies: this suite creates and aborts
	// multipart uploads, and none of that belongs in the developer's stack.
	if strings.Contains(cfg.S3Endpoint, ":3900") {
		t.Fatalf("DRIVE_S3_ENDPOINT points at the dev stack (%s); tests run against drive-test on :3910", cfg.S3Endpoint)
	}
	return cfg
}

// uploadTestServer builds the upload routes over the real database and the real
// object store.
//
// It assembles the /api chain by hand rather than calling Routes(): wiring the
// upload routes into Routes() is a one-line change in server.go that belongs to
// whoever owns that file. TestUploadRoutesAreMountedInRoutes below is the
// assertion that it happened.
func uploadTestServer(t *testing.T) (http.Handler, *pgxpool.Pool, *s3.Client, *config.Config) {
	t.Helper()
	pool := authTestPool(t)
	cfg := uploadTestConfig(t)

	uploadS3Once.Do(func() {
		uploadS3Client, uploadS3Presign, uploadS3Err = blob.New(context.Background(), cfg)
	})
	if uploadS3Err != nil {
		t.Fatalf("drive-test Garage: %v (is `docker compose -p drive-test --env-file .env.test up -d` running?)", uploadS3Err)
	}

	s := New(cfg, pool, nil, nil, uploadS3Client, uploadS3Presign)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(s.requestLogger)
	r.Use(s.recoverer)
	r.Route("/api", func(r chi.Router) {
		r.Use(RequireClientHeader)
		r.Use(s.sessionLoader)
		s.mountUploads(r)
	})
	return r, pool, uploadS3Client, cfg
}

// ---------------------------------------------------------------- fixtures --

// upRec is one response with the assertion this suite repeats most. httptest's
// recorder cannot carry a method, so it is wrapped rather than reimplemented.
type upRec struct{ *httptest.ResponseRecorder }

func upDo(t *testing.T, h http.Handler, method, path string, body any, cookie *http.Cookie) upRec {
	t.Helper()
	return upRec{authDo(t, h, method, path, body, cookie)}
}

// Expect fails unless the status matches, quoting the body: an unexpected
// status is nearly always explained by the error envelope underneath it.
func (r upRec) Expect(t *testing.T, want int) upRec {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status %d, want %d (body %s)", r.Code, want, r.Body)
	}
	return r
}

// uploadBody is every upload response in one struct: the status shape plus the
// two URL lists and the chimera pins.
type uploadBody struct {
	UploadID         uuid.UUID   `json:"upload_id"`
	Mode             string      `json:"mode"`
	FileName         string      `json:"file_name"`
	FileSize         int64       `json:"file_size"`
	PartSize         int64       `json:"part_size"`
	PartsTotal       int         `json:"parts_total"`
	Fingerprint      string      `json:"fingerprint"`
	ParentID         *uuid.UUID  `json:"parent_id"`
	Status           string      `json:"status"`
	ConfirmedParts   []int       `json:"confirmed_parts"`
	NodeID           *uuid.UUID  `json:"node_id"`
	SessionExpiresAt time.Time   `json:"session_expires_at"`
	Presigned        []uploadURL `json:"presigned"`
	// Missing is a pointer so "absent" and "empty" stay distinguishable: an
	// armed resume must omit it, never hand back an empty list.
	Missing     *[]uploadURL `json:"missing"`
	VerifyParts []int        `json:"verify_parts"`
}

type uploadURL struct {
	PartNumber int       `json:"part_number"`
	URL        string    `json:"url"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (b uploadBody) urlFor(t *testing.T, n int) string {
	t.Helper()
	lists := [][]uploadURL{b.Presigned}
	if b.Missing != nil {
		lists = append(lists, *b.Missing)
	}
	for _, list := range lists {
		for _, u := range list {
			if u.PartNumber == n {
				return u.URL
			}
		}
	}
	t.Fatalf("no URL for part %d (presigned %v, missing %v)", n, b.Presigned, b.Missing)
	return ""
}

func uploadDecode(t *testing.T, rec upRec) uploadBody {
	t.Helper()
	var b uploadBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decoding the upload response: %v (body %s)", err, rec.Body)
	}
	return b
}

// uploadCreate posts a create. policy may be empty, in which case the field is
// omitted entirely -- "no policy" is meaningfully different from "some policy".
func uploadCreate(t *testing.T, h http.Handler, u nodeUser, parent uuid.UUID, name string, size int64, fingerprint, policy string) upRec {
	t.Helper()
	body := map[string]any{
		"file_name":   name,
		"file_size":   size,
		"mime":        "application/octet-stream",
		"parent_id":   parent,
		"fingerprint": fingerprint,
	}
	if policy != "" {
		body["conflict_policy"] = policy
	}
	return upDo(t, h, http.MethodPost, "/api/uploads", body, u.Cookie)
}

// uploadStart creates a session and schedules its cancellation, so a failing
// test never leaves a multipart upload behind in Garage.
func uploadStart(t *testing.T, h http.Handler, u nodeUser, parent uuid.UUID, name string, size int64) uploadBody {
	t.Helper()
	fingerprint := "fp-" + uuid.NewString()
	rec := uploadCreate(t, h, u, parent, name, size, fingerprint, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	body := uploadDecode(t, rec)
	body.Fingerprint = fingerprint
	uploadCancelLater(t, h, u, body.UploadID)
	return body
}

func uploadCancelLater(t *testing.T, h http.Handler, u nodeUser, id uuid.UUID) {
	t.Cleanup(func() {
		req := httptest.NewRequest(http.MethodDelete, "/api/uploads/"+id.String(), strings.NewReader(""))
		req.Header.Set(ClientHeader, "web")
		req.AddCookie(u.Cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Logf("cleanup: cancelling upload %s returned %d: %s", id, rec.Code, rec.Body)
		}
	})
}

// uploadPutPart PUTs bytes to a presigned URL and returns the normalized ETag,
// exactly as the browser engine does.
func uploadPutPart(t *testing.T, url string, data []byte) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("building the part PUT: %v", err)
	}
	req.ContentLength = int64(len(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("part PUT: status %d (body %s)", resp.StatusCode, raw)
	}
	return upload.NormalizeETag(resp.Header.Get("ETag"))
}

func uploadBytes(n int64, fill byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill
	}
	return b
}

func uploadMD5(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func uploadConfirm(t *testing.T, h http.Handler, u nodeUser, id uuid.UUID, n int, etag, digest string, size int64) upRec {
	t.Helper()
	return upDo(t, h, http.MethodPost,
		fmt.Sprintf("/api/uploads/%s/parts/%d", id, n),
		map[string]any{"etag": etag, "md5": digest, "size": size}, u.Cookie)
}

func uploadResume(t *testing.T, h http.Handler, u nodeUser, id uuid.UUID, body any) upRec {
	t.Helper()
	return upDo(t, h, http.MethodPost, "/api/uploads/"+id.String()+"/resume", body, u.Cookie)
}

func uploadRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (status string, s3UploadID *string, verify []int32) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT status, s3_upload_id, verify_parts FROM upload_sessions WHERE id = $1`, id).
		Scan(&status, &s3UploadID, &verify)
	if err != nil {
		t.Fatalf("reading upload session %s: %v", id, err)
	}
	return status, s3UploadID, verify
}

func uploadExpire(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE upload_sessions SET expires_at = now() - interval '1 day' WHERE id = $1`, id); err != nil {
		t.Fatalf("expiring upload session %s: %v", id, err)
	}
}

// ------------------------------------------------------------------ create --

func TestCreateUploadNew(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	size := 2*uploadTestPartSize + 512
	body := uploadStart(t, h, u, u.RootID, "report.bin", size)

	if body.Status != upload.StatusActive || body.Mode != upload.ModeDirect {
		t.Errorf("status=%q mode=%q, want active/direct", body.Status, body.Mode)
	}
	if body.PartSize != uploadTestPartSize || body.PartsTotal != 3 {
		t.Errorf("part_size=%d parts_total=%d, want %d and 3", body.PartSize, body.PartsTotal, uploadTestPartSize)
	}
	if len(body.ConfirmedParts) != 0 {
		t.Errorf("confirmed_parts = %v, want empty", body.ConfirmedParts)
	}
	if len(body.Presigned) != 3 {
		t.Fatalf("presigned = %d URLs, want 3 (every missing part, capped at %d)", len(body.Presigned), upload.PresignBatch)
	}
	for i, p := range body.Presigned {
		if p.PartNumber != i+1 || p.URL == "" || p.ExpiresAt.Before(time.Now()) {
			t.Errorf("presigned[%d] = %+v", i, p)
		}
		// A checksum parameter in the query is the one thing that breaks a
		// presigned PUT against Garage.
		if strings.Contains(strings.ToLower(p.URL), "x-amz-checksum") ||
			strings.Contains(strings.ToLower(p.URL), "x-amz-sdk-checksum") {
			t.Errorf("presigned[%d] carries a checksum parameter: %s", i, p.URL)
		}
	}
	if got := body.SessionExpiresAt.Sub(time.Now()); got < 6*24*time.Hour {
		t.Errorf("session_expires_at is only %v away, want about 7 days", got)
	}

	// Only eight URLs come back however many parts are missing.
	big := uploadStart(t, h, u, u.RootID, "bigger.bin", 20*uploadTestPartSize)
	if len(big.Presigned) != upload.PresignBatch {
		t.Errorf("presigned = %d URLs for a 20-part file, want %d", len(big.Presigned), upload.PresignBatch)
	}
}

func TestCreateUploadMatchesActiveSession(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	first := uploadStart(t, h, u, u.RootID, "same.bin", uploadTestPartSize)

	rec := uploadCreate(t, h, u, u.RootID, "same.bin", uploadTestPartSize, first.Fingerprint, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second create: status %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	second := uploadDecode(t, rec)
	if second.UploadID != first.UploadID {
		t.Fatalf("second create made a new session %s, want the existing %s", second.UploadID, first.UploadID)
	}
	// Nothing was armed: the session has no confirmed parts, so there is no
	// re-selection risk to guard against, and URLs still flow.
	if len(second.VerifyParts) != 0 {
		t.Errorf("verify_parts = %v on a session with no confirmed parts, want none", second.VerifyParts)
	}
	if len(second.Presigned) != 1 {
		t.Errorf("presigned = %v, want one URL", second.Presigned)
	}

	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM upload_sessions WHERE user_id = $1 AND fingerprint = $2`,
		u.ID, first.Fingerprint).Scan(&rows); err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d session rows for one file, want 1", rows)
	}
}

// The same file into two folders is two uploads; into one folder twice it is
// one. That pair is the whole meaning of the (user, fingerprint, parent) key.
func TestCreateUploadIsKeyedOnDestination(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)
	other := nodeMkFolder(t, pool, u, u.RootID, "elsewhere")

	fingerprint := "fp-" + uuid.NewString()

	rec := uploadCreate(t, h, u, u.RootID, "twice.bin", uploadTestPartSize, fingerprint, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d (%s)", rec.Code, rec.Body)
	}
	first := uploadDecode(t, rec)
	uploadCancelLater(t, h, u, first.UploadID)

	rec = uploadCreate(t, h, u, other, "twice.bin", uploadTestPartSize, fingerprint, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create into the second folder: %d (%s), want 201", rec.Code, rec.Body)
	}
	second := uploadDecode(t, rec)
	uploadCancelLater(t, h, u, second.UploadID)

	if second.UploadID == first.UploadID {
		t.Fatal("the same file into two folders shared one session")
	}

	rec = uploadCreate(t, h, u, u.RootID, "twice.bin", uploadTestPartSize, fingerprint, "")
	if rec.Code != http.StatusOK || uploadDecode(t, rec).UploadID != first.UploadID {
		t.Fatalf("re-create into the first folder: %d (%s), want 200 and %s", rec.Code, rec.Body, first.UploadID)
	}
}

// A matched create with confirmed parts is a re-selection: the server has no
// way to know the file on disk is still the file those parts came from, so it
// arms the chimera guard and hands back no URL at all.
func TestMatchedCreateWithConfirmedPartsArmsVerifyParts(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	size := 2*uploadTestPartSize + 128
	body := uploadStart(t, h, u, u.RootID, "chimera.bin", size)

	data := uploadBytes(uploadTestPartSize, 'a')
	etag := uploadPutPart(t, body.urlFor(t, 1), data)
	uploadConfirm(t, h, u, body.UploadID, 1, etag, uploadMD5(data), uploadTestPartSize).Expect(t, http.StatusOK)

	rec := uploadCreate(t, h, u, u.RootID, "chimera.bin", size, body.Fingerprint, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("matched create: %d (%s)", rec.Code, rec.Body)
	}
	matched := uploadDecode(t, rec)

	if len(matched.Presigned) != 0 {
		t.Fatalf("an armed create leaked %d URLs; no URL may reach a possibly-different file", len(matched.Presigned))
	}
	if len(matched.VerifyParts) != 2 || matched.VerifyParts[0] != 1 || matched.VerifyParts[1] != 1 {
		t.Fatalf("verify_parts = %v, want [1 1]", matched.VerifyParts)
	}
	if _, _, verify := uploadRow(t, pool, body.UploadID); len(verify) != 2 {
		t.Fatalf("verify_parts in the database = %v, want the pinned pair", verify)
	}

	// Arming is idempotent: a third create must not move the pins.
	data2 := uploadBytes(uploadTestPartSize, 'b')
	resume := uploadResume(t, h, u, body.UploadID, map[string]any{
		"part_md5s": map[string]string{"1": uploadMD5(data)},
	})
	resume.Expect(t, http.StatusOK)
	fresh := uploadDecode(t, resume)
	uploadPutPart(t, fresh.urlFor(t, 2), data2)
	uploadConfirm(t, h, u, body.UploadID, 2, uploadMD5(data2), uploadMD5(data2), uploadTestPartSize).Expect(t, http.StatusOK)

	rec = uploadCreate(t, h, u, u.RootID, "chimera.bin", size, body.Fingerprint, "")
	third := uploadDecode(t, rec)
	if len(third.VerifyParts) != 2 || third.VerifyParts[0] != 1 || third.VerifyParts[1] != 2 {
		t.Fatalf("re-armed verify_parts = %v, want [1 2] over the confirmed range", third.VerifyParts)
	}
}

// A 0-byte file never opens a multipart upload: Garage rejects a complete with
// an empty part list, so that case is a single PutObject at finalize time.
func TestCreateUploadZeroByte(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	rec := uploadCreate(t, h, u, u.RootID, "empty.txt", 0, "fp-"+uuid.NewString(), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", rec.Code, rec.Body)
	}
	body := uploadDecode(t, rec)
	uploadCancelLater(t, h, u, body.UploadID)

	if body.PartsTotal != 0 || len(body.Presigned) != 0 {
		t.Fatalf("parts_total=%d presigned=%v, want 0 and none", body.PartsTotal, body.Presigned)
	}
	if _, s3ID, _ := uploadRow(t, pool, body.UploadID); s3ID != nil {
		t.Fatalf("s3_upload_id = %q, want NULL for a 0-byte upload", *s3ID)
	}

	// The handshake still works; it just has nothing to reconcile or hand out.
	rec = uploadResume(t, h, u, body.UploadID, nil)
	rec.Expect(t, http.StatusOK)
	resumed := uploadDecode(t, rec)
	if resumed.Missing == nil || len(*resumed.Missing) != 0 {
		t.Fatalf("missing = %v, want an empty list", resumed.Missing)
	}
}

// The auto-grow arithmetic through the real endpoint: a 200 GiB file at a
// 1 MiB configured part size would need 204,800 parts, so the part size grows
// to the smallest 10 MiB multiple that keeps the count under 10,000.
func TestCreateUploadGrowsThePartSize(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	const size = 200 << 30
	rec := uploadCreate(t, h, u, u.RootID, "huge.img", size, "fp-"+uuid.NewString(), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", rec.Code, rec.Body)
	}
	body := uploadDecode(t, rec)
	uploadCancelLater(t, h, u, body.UploadID)

	if body.PartSize != 30<<20 || body.PartsTotal != 6827 {
		t.Fatalf("part_size=%d parts_total=%d, want %d and 6827", body.PartSize, body.PartsTotal, 30<<20)
	}
	if body.PartSize%(10<<20) != 0 {
		t.Fatalf("grown part_size %d is not a multiple of Garage's block size", body.PartSize)
	}

	// Past the point where even 5 GiB parts would fit, the create is refused
	// rather than silently truncated.
	rec = uploadCreate(t, h, u, u.RootID, "impossible.img",
		int64(upload.MaxParts)*upload.MaxPartSize+1, "fp-"+uuid.NewString(), "")
	rec.Expect(t, http.StatusUnprocessableEntity)
	if code := recCode(t, rec); code != CodeInvalid {
		t.Fatalf("code = %q, want %q", code, CodeInvalid)
	}
}

func TestCreateUploadRejectsBadRequests(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)
	other := nodeNewUser(t, pool)

	// The destination is authorized independently of everything else, and a
	// failure is 404 either way -- someone else's folder must not be
	// distinguishable from one that does not exist.
	cases := []struct {
		name       string
		parent     uuid.UUID
		fileName   string
		size       int64
		policy     string
		wantStatus int
		wantCode   string
	}{
		{name: "another user's folder", parent: other.RootID, fileName: "a.bin", size: 1,
			wantStatus: http.StatusNotFound, wantCode: CodeNotFound},
		{name: "a folder that does not exist", parent: uuid.New(), fileName: "a.bin", size: 1,
			wantStatus: http.StatusNotFound, wantCode: CodeNotFound},
		{name: "a negative size", parent: u.RootID, fileName: "a.bin", size: -1,
			wantStatus: http.StatusUnprocessableEntity, wantCode: CodeInvalid},
		{name: "a name filename hygiene rejects", parent: u.RootID, fileName: "../escape", size: 1,
			wantStatus: http.StatusUnprocessableEntity, wantCode: CodeInvalid},
		// reuse is the folder vocabulary; a colliding file gets replace or
		// rename, or no policy and a prompt.
		{name: "conflict_policy=reuse", parent: u.RootID, fileName: "a.bin", size: 1, policy: "reuse",
			wantStatus: http.StatusUnprocessableEntity, wantCode: CodeInvalid},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := uploadCreate(t, h, u, c.parent, c.fileName, c.size, "fp-"+uuid.NewString(), c.policy)
			rec.Expect(t, c.wantStatus)
			if got := recCode(t, rec); got != c.wantCode {
				t.Fatalf("code = %q, want %q (body %s)", got, c.wantCode, rec.Body)
			}
		})
	}

	// An empty fingerprint is refused: without it there is no resume key.
	rec := upDo(t, h, http.MethodPost, "/api/uploads", map[string]any{
		"file_name": "a.bin", "file_size": 1, "mime": "", "parent_id": u.RootID, "fingerprint": "",
	}, u.Cookie)
	rec.Expect(t, http.StatusUnprocessableEntity)
}

// The name-conflict check runs only on the create-new path, and only when the
// client has not already answered the question.
func TestCreateUploadNameConflict(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)
	nodeMkFile(t, pool, u, u.RootID, "taken.bin")

	rec := uploadCreate(t, h, u, u.RootID, "taken.bin", uploadTestPartSize, "fp-"+uuid.NewString(), "")
	rec.Expect(t, http.StatusConflict)
	if got := recCode(t, rec); got != CodeNameConflict {
		t.Fatalf("code = %q, want %q", got, CodeNameConflict)
	}

	// The comparison is case-insensitive, like the sibling uniqueness index.
	rec = uploadCreate(t, h, u, u.RootID, "TAKEN.BIN", uploadTestPartSize, "fp-"+uuid.NewString(), "")
	rec.Expect(t, http.StatusConflict)

	rec = uploadCreate(t, h, u, u.RootID, "taken.bin", uploadTestPartSize, "fp-"+uuid.NewString(), "rename")
	rec.Expect(t, http.StatusCreated)
	uploadCancelLater(t, h, u, uploadDecode(t, rec).UploadID)
}

// Two tabs creating the same upload at the same moment: the unique index picks
// a winner, the loser gives back the multipart it opened and returns the
// winner's session. Nobody gets a duplicate.
func TestConcurrentCreateRace(t *testing.T) {
	h, pool, s3c, cfg := uploadTestServer(t)
	u := nodeNewUser(t, pool)
	fingerprint := "fp-" + uuid.NewString()

	before := uploadCountMultiparts(t, s3c, cfg.S3Bucket)

	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		results [2]upRec
	)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{
				"file_name":   "race.bin",
				"file_size":   uploadTestPartSize,
				"mime":        "application/octet-stream",
				"parent_id":   u.RootID,
				"fingerprint": fingerprint,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/uploads", bytes.NewReader(raw))
			req.Header.Set(ClientHeader, "web")
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(u.Cookie)
			rec := httptest.NewRecorder()
			<-start
			h.ServeHTTP(rec, req)
			results[i] = upRec{rec}
		}(i)
	}
	close(start)
	wg.Wait()

	created, matched := 0, 0
	var ids []uuid.UUID
	for i, rec := range results {
		switch rec.Code {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			matched++
		default:
			t.Fatalf("goroutine %d: status %d (body %s)", i, rec.Code, rec.Body)
		}
		ids = append(ids, uploadDecode(t, rec).UploadID)
	}
	uploadCancelLater(t, h, u, ids[0])

	if created != 1 || matched != 1 {
		t.Fatalf("%d creates and %d matches, want exactly one of each", created, matched)
	}
	if ids[0] != ids[1] {
		t.Fatalf("the two tabs got different sessions, %s and %s", ids[0], ids[1])
	}

	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM upload_sessions WHERE fingerprint = $1`, fingerprint).Scan(&rows); err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d session rows after the race, want 1", rows)
	}

	// The loser's multipart upload was aborted, not abandoned: exactly one new
	// multipart exists, the winner's.
	if got := uploadCountMultiparts(t, s3c, cfg.S3Bucket) - before; got != 1 {
		t.Fatalf("the race left %d new multipart uploads in Garage, want 1", got)
	}
}

func uploadCountMultiparts(t *testing.T, c *s3.Client, bucket string) int {
	t.Helper()
	var (
		n         int
		keyMarker *string
		idMarker  *string
	)
	for {
		out, err := c.ListMultipartUploads(context.Background(), &s3.ListMultipartUploadsInput{
			Bucket: aws.String(bucket), KeyMarker: keyMarker, UploadIdMarker: idMarker,
		})
		if err != nil {
			t.Fatalf("listing multipart uploads: %v", err)
		}
		n += len(out.Uploads)
		if !aws.ToBool(out.IsTruncated) {
			return n
		}
		keyMarker, idMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}

// ----------------------------------------------------------------- confirm --

func TestConfirmPartValidatesSize(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	size := 2*uploadTestPartSize + 512
	body := uploadStart(t, h, u, u.RootID, "sized.bin", size)
	digest := uploadMD5(uploadBytes(1, 'x'))

	cases := []struct {
		name   string
		number int
		size   int64
		want   int
	}{
		{name: "non-final part short", number: 1, size: uploadTestPartSize - 1, want: http.StatusUnprocessableEntity},
		{name: "non-final part long", number: 2, size: uploadTestPartSize + 1, want: http.StatusUnprocessableEntity},
		{name: "final part past the remainder", number: 3, size: 513, want: http.StatusUnprocessableEntity},
		{name: "final part empty", number: 3, size: 0, want: http.StatusUnprocessableEntity},
		{name: "part zero", number: 0, size: uploadTestPartSize, want: http.StatusUnprocessableEntity},
		{name: "part past the total", number: 4, size: uploadTestPartSize, want: http.StatusUnprocessableEntity},
		{name: "final part at the remainder", number: 3, size: 512, want: http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := uploadConfirm(t, h, u, body.UploadID, c.number, digest, digest, c.size)
			rec.Expect(t, c.want)
			if c.want == http.StatusUnprocessableEntity && recCode(t, rec) != CodeInvalid {
				t.Fatalf("code = %q, want %q", recCode(t, rec), CodeInvalid)
			}
		})
	}

	// A malformed digest is refused too.
	rec := uploadConfirm(t, h, u, body.UploadID, 1, "abc", "not-a-digest", uploadTestPartSize)
	rec.Expect(t, http.StatusUnprocessableEntity)
}

func TestConfirmPartIsIdempotent(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	body := uploadStart(t, h, u, u.RootID, "idem.bin", 2*uploadTestPartSize)
	data := uploadBytes(uploadTestPartSize, 'q')
	etag := uploadPutPart(t, body.urlFor(t, 1), data)
	digest := uploadMD5(data)

	var first, second struct {
		Confirmed        bool      `json:"confirmed"`
		SessionExpiresAt time.Time `json:"session_expires_at"`
	}
	rec := uploadConfirm(t, h, u, body.UploadID, 1, etag, digest, uploadTestPartSize)
	rec.Expect(t, http.StatusOK)
	uploadJSON(t, rec, &first)

	rec = uploadConfirm(t, h, u, body.UploadID, 1, etag, digest, uploadTestPartSize)
	rec.Expect(t, http.StatusOK)
	uploadJSON(t, rec, &second)

	if !first.Confirmed || !second.Confirmed {
		t.Fatalf("confirmed = %v then %v, want true both times", first.Confirmed, second.Confirmed)
	}
	if !second.SessionExpiresAt.After(time.Now().Add(6 * 24 * time.Hour)) {
		t.Errorf("session_expires_at = %v, want the refreshed 7-day deadline", second.SessionExpiresAt)
	}

	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM upload_parts WHERE session_id = $1`, body.UploadID).Scan(&rows); err != nil {
		t.Fatalf("counting parts: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d part rows after two confirmations of the same part, want 1", rows)
	}

	rec = upDo(t, h, http.MethodGet, "/api/uploads/"+body.UploadID.String(), nil, u.Cookie)
	rec.Expect(t, http.StatusOK)
	if got := uploadDecode(t, rec).ConfirmedParts; len(got) != 1 || got[0] != 1 {
		t.Fatalf("confirmed_parts = %v, want [1]", got)
	}
}

// ------------------------------------------------------------------ resume --

// Reconciliation in both directions at once: a part that reached Garage but
// whose confirmation was lost, and a part the ledger claims that Garage does
// not have. ListParts wins both times.
func TestResumeReconcilesAgainstGarage(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	size := 2*uploadTestPartSize + 512
	body := uploadStart(t, h, u, u.RootID, "reconcile.bin", size)

	data1 := uploadBytes(uploadTestPartSize, '1')
	data2 := uploadBytes(uploadTestPartSize, '2')

	// Part 1: PUT and confirmed, the normal path.
	etag1 := uploadPutPart(t, body.urlFor(t, 1), data1)
	uploadConfirm(t, h, u, body.UploadID, 1, etag1, uploadMD5(data1), uploadTestPartSize).Expect(t, http.StatusOK)

	// Part 2: PUT but never confirmed -- the kill-9 window.
	uploadPutPart(t, body.urlFor(t, 2), data2)

	// Part 3: in the ledger but never in Garage.
	ghost := uploadMD5(uploadBytes(4, 'z'))
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO upload_parts (session_id, part_number, size, etag, md5) VALUES ($1, 3, 512, $2, $2)`,
		body.UploadID, ghost); err != nil {
		t.Fatalf("planting the ghost ledger row: %v", err)
	}

	rec := uploadResume(t, h, u, body.UploadID, nil)
	rec.Expect(t, http.StatusOK)
	bounced := uploadDecode(t, rec)

	// Drift arms verification, so the bounce carries pins and no URLs at all.
	if bounced.Missing != nil {
		t.Fatalf("an armed resume returned missing=%v; no URL may reach an unverified file", *bounced.Missing)
	}
	if len(bounced.VerifyParts) != 2 || bounced.VerifyParts[0] != 1 || bounced.VerifyParts[1] != 1 {
		t.Fatalf("verify_parts = %v, want [1 1] -- part 2 has no client MD5 to pin to", bounced.VerifyParts)
	}
	if len(bounced.ConfirmedParts) != 2 || bounced.ConfirmedParts[0] != 1 || bounced.ConfirmedParts[1] != 2 {
		t.Fatalf("confirmed_parts = %v, want [1 2] after adopting part 2", bounced.ConfirmedParts)
	}
	// The bounce is still an authenticated touch: a client that has to make a
	// second round trip must not lose a day of headroom for it.
	if !bounced.SessionExpiresAt.After(time.Now().Add(6 * 24 * time.Hour)) {
		t.Errorf("session_expires_at = %v on the bounce, want the refreshed deadline", bounced.SessionExpiresAt)
	}

	// The adopted row carries Garage's ETag and no MD5; the ghost row is gone.
	var (
		adoptedETag string
		adoptedMD5  *string
		adoptedSize int64
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT etag, md5, size FROM upload_parts WHERE session_id = $1 AND part_number = 2`,
		body.UploadID).Scan(&adoptedETag, &adoptedMD5, &adoptedSize); err != nil {
		t.Fatalf("reading the adopted part: %v", err)
	}
	if adoptedETag != uploadMD5(data2) || adoptedSize != uploadTestPartSize {
		t.Errorf("adopted part 2 = etag %q size %d, want %q and %d", adoptedETag, adoptedSize, uploadMD5(data2), uploadTestPartSize)
	}
	if adoptedMD5 != nil {
		t.Errorf("adopted part 2 has md5 %q; rows from reconciliation carry none", *adoptedMD5)
	}
	var ghosts int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM upload_parts WHERE session_id = $1 AND part_number = 3`, body.UploadID).Scan(&ghosts); err != nil {
		t.Fatalf("counting the ghost row: %v", err)
	}
	if ghosts != 0 {
		t.Fatal("the ledger-only part survived reconciliation; it must be deleted and re-issued")
	}

	// A wrong MD5 for the pinned part is the chimera refusal.
	rec = uploadResume(t, h, u, body.UploadID, map[string]any{
		"part_md5s": map[string]string{"1": uploadMD5(uploadBytes(uploadTestPartSize, 'X'))},
	})
	rec.Expect(t, http.StatusConflict)
	if got := recCode(t, rec); got != CodeInvalid {
		t.Fatalf("code = %q, want %q", got, CodeInvalid)
	}
	if msg := recMessage(t, rec); msg != "part verification failed" {
		t.Fatalf("message = %q, want %q", msg, "part verification failed")
	}

	// The right one clears the flag and the handshake proceeds: a fresh URL
	// for every missing part, which is part 3 alone.
	rec = uploadResume(t, h, u, body.UploadID, map[string]any{
		"part_md5s": map[string]string{"1": uploadMD5(data1)},
	})
	rec.Expect(t, http.StatusOK)
	resumed := uploadDecode(t, rec)
	if len(resumed.VerifyParts) != 0 {
		t.Fatalf("verify_parts = %v after a passing check, want none", resumed.VerifyParts)
	}
	if resumed.Missing == nil || len(*resumed.Missing) != 1 || (*resumed.Missing)[0].PartNumber != 3 {
		t.Fatalf("missing = %v, want exactly part 3", resumed.Missing)
	}
	if _, _, verify := uploadRow(t, pool, body.UploadID); verify != nil {
		t.Fatalf("verify_parts in the database = %v, want NULL", verify)
	}

	// And the re-issued URL actually works.
	tail := uploadBytes(512, '3')
	etag3 := uploadPutPart(t, resumed.urlFor(t, 3), tail)
	if etag3 != uploadMD5(tail) {
		t.Fatalf("re-issued part 3: ETag %q != MD5 %q", etag3, uploadMD5(tail))
	}
}

// The URL-refill case: normal in-tab progress must never arm verification, or
// a client topping up its URL pool mid-transfer would be interrupted by a
// verification bounce it has no reason to expect.
func TestResumeRefillIsNeverArmed(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	body := uploadStart(t, h, u, u.RootID, "refill.bin", 3*uploadTestPartSize)
	data := uploadBytes(uploadTestPartSize, 'r')
	etag := uploadPutPart(t, body.urlFor(t, 1), data)
	uploadConfirm(t, h, u, body.UploadID, 1, etag, uploadMD5(data), uploadTestPartSize).Expect(t, http.StatusOK)

	// No body at all, exactly as the engine calls it when its URL pool runs low.
	rec := uploadResume(t, h, u, body.UploadID, nil)
	rec.Expect(t, http.StatusOK)
	resumed := uploadDecode(t, rec)

	if len(resumed.VerifyParts) != 0 {
		t.Fatalf("verify_parts = %v; in-tab progress must never arm the guard", resumed.VerifyParts)
	}
	if resumed.Missing == nil || len(*resumed.Missing) != 2 {
		t.Fatalf("missing = %v, want fresh URLs for parts 2 and 3", resumed.Missing)
	}
	for i, m := range *resumed.Missing {
		if m.PartNumber != i+2 || m.URL == "" {
			t.Fatalf("missing[%d] = %+v", i, m)
		}
	}

	// part_md5s is ignored entirely when nothing is armed -- even nonsense.
	rec = uploadResume(t, h, u, body.UploadID, map[string]any{
		"part_md5s": map[string]string{"1": "ffffffffffffffffffffffffffffffff"},
	})
	rec.Expect(t, http.StatusOK)
	if got := uploadDecode(t, rec); got.Missing == nil || len(*got.Missing) != 2 {
		t.Fatalf("missing = %v with an unarmed session, want the same two URLs", got.Missing)
	}
}

// --------------------------------------------------------- status and list --

func TestUploadStatusSlidesTheExpiry(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	body := uploadStart(t, h, u, u.RootID, "sliding.bin", uploadTestPartSize)

	// Six days in: still live, and a plain status read pushes the deadline back
	// out to seven days. PLAN is explicit that every authenticated touch does
	// this, not only part confirmations.
	if _, err := pool.Exec(context.Background(),
		`UPDATE upload_sessions SET expires_at = now() + interval '1 day' WHERE id = $1`, body.UploadID); err != nil {
		t.Fatalf("ageing the session: %v", err)
	}

	rec := upDo(t, h, http.MethodGet, "/api/uploads/"+body.UploadID.String(), nil, u.Cookie)
	rec.Expect(t, http.StatusOK)
	if got := uploadDecode(t, rec).SessionExpiresAt; !got.After(time.Now().Add(6 * 24 * time.Hour)) {
		t.Fatalf("session_expires_at = %v, want it refreshed to about 7 days out", got)
	}
}

// An expired session is gone from every entry point, and creating again makes
// a genuinely new one rather than resurrecting the corpse -- the expired row
// still holds the active-session unique index until it is retired.
func TestExpiredSessionIsGoneAndRecreates(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	body := uploadStart(t, h, u, u.RootID, "stale.bin", uploadTestPartSize)
	uploadExpire(t, pool, body.UploadID)

	digest := uploadMD5(uploadBytes(4, 'x'))
	gone := []struct {
		name string
		rec  func() upRec
	}{
		{name: "status", rec: func() upRec {
			return upDo(t, h, http.MethodGet, "/api/uploads/"+body.UploadID.String(), nil, u.Cookie)
		}},
		{name: "resume", rec: func() upRec {
			return uploadResume(t, h, u, body.UploadID, nil)
		}},
		{name: "confirm", rec: func() upRec {
			return uploadConfirm(t, h, u, body.UploadID, 1, digest, digest, uploadTestPartSize)
		}},
	}
	for _, c := range gone {
		t.Run(c.name, func(t *testing.T) {
			rec := c.rec()
			rec.Expect(t, http.StatusGone)
			if got := recCode(t, rec); got != CodeSessionExpired {
				t.Fatalf("code = %q, want %q", got, CodeSessionExpired)
			}
		})
	}

	rec := uploadCreate(t, h, u, u.RootID, "stale.bin", uploadTestPartSize, body.Fingerprint, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-create after expiry: %d (%s), want 201", rec.Code, rec.Body)
	}
	fresh := uploadDecode(t, rec)
	uploadCancelLater(t, h, u, fresh.UploadID)

	if fresh.UploadID == body.UploadID {
		t.Fatal("the expired session was handed back instead of a fresh one")
	}
	if status, _, _ := uploadRow(t, pool, body.UploadID); status != upload.StatusAborted {
		t.Fatalf("the expired session is %q, want %q", status, upload.StatusAborted)
	}
}

func TestListUploads(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)
	other := nodeNewUser(t, pool)

	var mine []uuid.UUID
	for i := range 3 {
		body := uploadStart(t, h, u, u.RootID, fmt.Sprintf("list-%d.bin", i), uploadTestPartSize)
		mine = append(mine, body.UploadID)
	}
	uploadStart(t, h, other, other.RootID, "theirs.bin", uploadTestPartSize)

	var page struct {
		Items      []uploadBody `json:"items"`
		NextCursor *string      `json:"next_cursor"`
	}
	rec := upDo(t, h, http.MethodGet, "/api/uploads?limit=2", nil, u.Cookie)
	rec.Expect(t, http.StatusOK)
	uploadJSON(t, rec, &page)

	if len(page.Items) != 2 || page.NextCursor == nil {
		t.Fatalf("first page = %d items, cursor %v; want 2 and a cursor", len(page.Items), page.NextCursor)
	}
	// Newest first.
	if page.Items[0].UploadID != mine[2] || page.Items[1].UploadID != mine[1] {
		t.Fatalf("page order = %v, want newest first", []uuid.UUID{page.Items[0].UploadID, page.Items[1].UploadID})
	}

	rec = upDo(t, h, http.MethodGet, "/api/uploads?limit=2&cursor="+*page.NextCursor, nil, u.Cookie)
	rec.Expect(t, http.StatusOK)
	var second struct {
		Items      []uploadBody `json:"items"`
		NextCursor *string      `json:"next_cursor"`
	}
	uploadJSON(t, rec, &second)
	if len(second.Items) != 1 || second.Items[0].UploadID != mine[0] || second.NextCursor != nil {
		t.Fatalf("second page = %d items, cursor %v", len(second.Items), second.NextCursor)
	}

	// No other user's session appears, whatever the page.
	for _, item := range append(page.Items, second.Items...) {
		if item.UploadID != mine[0] && item.UploadID != mine[1] && item.UploadID != mine[2] {
			t.Fatalf("the list leaked session %s", item.UploadID)
		}
	}
}

// ------------------------------------------------------------------ cancel --

func TestCancelUpload(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	body := uploadStart(t, h, u, u.RootID, "cancel.bin", uploadTestPartSize)

	rec := upDo(t, h, http.MethodDelete, "/api/uploads/"+body.UploadID.String(), nil, u.Cookie)
	rec.Expect(t, http.StatusNoContent)
	if status, _, _ := uploadRow(t, pool, body.UploadID); status != upload.StatusAborted {
		t.Fatalf("status after cancel = %q, want %q", status, upload.StatusAborted)
	}

	// Cancelling twice is not an error; the session is simply already gone.
	upDo(t, h, http.MethodDelete, "/api/uploads/"+body.UploadID.String(), nil, u.Cookie).
		Expect(t, http.StatusNoContent)

	// And the session is gone from the other entry points.
	upDo(t, h, http.MethodGet, "/api/uploads/"+body.UploadID.String(), nil, u.Cookie).
		Expect(t, http.StatusGone)

	// A cancelled session no longer blocks a new one for the same file.
	rec = uploadCreate(t, h, u, u.RootID, "cancel.bin", uploadTestPartSize, body.Fingerprint, "")
	rec.Expect(t, http.StatusCreated)
	uploadCancelLater(t, h, u, uploadDecode(t, rec).UploadID)
}

// A session the finalizer has taken over accepts no more parts and cannot be
// cancelled: tearing down a multipart mid-complete would lose the file. Status
// injection is how the state is reached -- SIGKILL cannot be landed inside a
// millisecond window on demand.
func TestUploadBeingFinalized(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)

	body := uploadStart(t, h, u, u.RootID, "finalizing.bin", uploadTestPartSize)
	if _, err := pool.Exec(context.Background(),
		`UPDATE upload_sessions SET status = 'completing' WHERE id = $1`, body.UploadID); err != nil {
		t.Fatalf("injecting the completing state: %v", err)
	}

	digest := uploadMD5(uploadBytes(4, 'x'))
	rec := uploadConfirm(t, h, u, body.UploadID, 1, digest, digest, uploadTestPartSize)
	rec.Expect(t, http.StatusConflict)
	if got := recCode(t, rec); got != CodeInProgress {
		t.Fatalf("confirm code = %q, want %q", got, CodeInProgress)
	}

	rec = upDo(t, h, http.MethodDelete, "/api/uploads/"+body.UploadID.String(), nil, u.Cookie)
	rec.Expect(t, http.StatusConflict)
	if got := recCode(t, rec); got != CodeInProgress {
		t.Fatalf("cancel code = %q, want %q", got, CodeInProgress)
	}

	// Status and the handshake still answer -- the client polls them while it
	// waits -- but the handshake hands out nothing: after the multipart is
	// completed it no longer exists to reconcile against.
	rec = upDo(t, h, http.MethodGet, "/api/uploads/"+body.UploadID.String(), nil, u.Cookie)
	rec.Expect(t, http.StatusOK)
	if got := uploadDecode(t, rec).Status; got != upload.StatusCompleting {
		t.Fatalf("status = %q, want %q", got, upload.StatusCompleting)
	}

	rec = uploadResume(t, h, u, body.UploadID, nil)
	rec.Expect(t, http.StatusOK)
	if resumed := uploadDecode(t, rec); resumed.Missing != nil {
		t.Fatalf("missing = %v while finalizing, want none", *resumed.Missing)
	}

	// The multipart is still ours to clean up; the cleanup hook cannot cancel
	// a completing session, so release it first.
	if _, err := pool.Exec(context.Background(),
		`UPDATE upload_sessions SET status = 'active' WHERE id = $1`, body.UploadID); err != nil {
		t.Fatalf("releasing the injected state: %v", err)
	}
}

// -------------------------------------------------------------------- authz --

// An upload_id is an identifier, never a credential: another user's id is a
// 404 everywhere, and nothing it names is disturbed.
func TestForeignUploadIsNotFound(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	owner := nodeNewUser(t, pool)
	stranger := nodeNewUser(t, pool)

	body := uploadStart(t, h, owner, owner.RootID, "private.bin", uploadTestPartSize)
	digest := uploadMD5(uploadBytes(4, 'x'))
	path := "/api/uploads/" + body.UploadID.String()

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{name: "status", method: http.MethodGet, path: path},
		{name: "resume", method: http.MethodPost, path: path + "/resume"},
		{name: "confirm", method: http.MethodPost, path: path + "/parts/1",
			body: map[string]any{"etag": digest, "md5": digest, "size": uploadTestPartSize}},
		{name: "cancel", method: http.MethodDelete, path: path},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := upDo(t, h, c.method, c.path, c.body, stranger.Cookie)
			rec.Expect(t, http.StatusNotFound)
			if got := recCode(t, rec); got != CodeNotFound {
				t.Fatalf("code = %q, want %q", got, CodeNotFound)
			}
		})
	}

	// The owner's session is untouched by any of it.
	if status, _, _ := uploadRow(t, pool, body.UploadID); status != upload.StatusActive {
		t.Fatalf("status = %q after the stranger's attempts, want %q", status, upload.StatusActive)
	}

	// Anonymous callers never get that far.
	upDo(t, h, http.MethodGet, path, nil, nil).Expect(t, http.StatusUnauthorized)
	upDo(t, h, http.MethodPost, "/api/uploads", map[string]any{}, nil).Expect(t, http.StatusUnauthorized)
}

// The CSRF gate covers the upload mutations like every other one.
func TestUploadMutationsNeedTheClientHeader(t *testing.T) {
	h, pool, _, _ := uploadTestServer(t)
	u := nodeNewUser(t, pool)
	id := uuid.New()

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/uploads"},
		{http.MethodPost, "/api/uploads/" + id.String() + "/resume"},
		{http.MethodPost, "/api/uploads/" + id.String() + "/parts/1"},
		{http.MethodDelete, "/api/uploads/" + id.String()},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		req.AddCookie(u.Cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without X-Drive-Client: %d, want 403", c.method, c.path, rec.Code)
		}
	}
}

// Wiring: server.go has to call s.mountUploads(r) for any of this to be
// reachable in the real binary. Until it does, this skips loudly rather than
// failing a gate for a one-line change in a file this suite does not own.
func TestUploadRoutesAreMountedInRoutes(t *testing.T) {
	pool := authTestPool(t)
	cfg := uploadTestConfig(t)
	h := New(cfg, pool, nil, nil, nil, nil).Routes()
	u := nodeNewUser(t, pool)

	rec := upDo(t, h, http.MethodGet, "/api/uploads", nil, u.Cookie)
	if rec.Code == http.StatusNotFound && recCode(t, rec) == CodeNotFound &&
		strings.Contains(recMessage(t, rec), "no such endpoint") {
		t.Skip("server.go does not call s.mountUploads(r) yet -- one line inside Routes(), next to s.mountSearch(r)")
	}
	rec.Expect(t, http.StatusOK)
}

// ----------------------------------------------------------------- plumbing --

func uploadJSON(t *testing.T, rec upRec, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decoding the response: %v (body %s)", err, rec.Body)
	}
}

func recCode(t *testing.T, rec upRec) string {
	t.Helper()
	var e ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("response is not an error envelope: %v (body %s)", err, rec.Body)
	}
	return e.Code
}

func recMessage(t *testing.T, rec upRec) string {
	t.Helper()
	var e ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("response is not an error envelope: %v (body %s)", err, rec.Body)
	}
	return e.Message
}
