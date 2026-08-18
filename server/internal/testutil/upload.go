package testutil

// Upload-specific harness machinery: the upload protocol's wire shapes, a
// driver that walks the protocol over real HTTP, and the two kinds of side door
// the interruption battery needs.
//
// The side doors are the point of this file.
//
// The first is out-of-band S3: CreateMultipartUpload / UploadPart / Complete /
// PutObject issued directly against Garage, with no session row anywhere. That
// is how a crash window gets constructed rather than raced -- SIGKILL cannot
// deterministically land between CompleteMultipartUpload returning and the
// publish transaction committing, so the test puts the world in that exact
// state by hand and then runs one GC pass.
//
// The second is direct object reads. "The bytes that came back are
// byte-identical" is asserted by fetching the blob's object straight from
// Garage rather than through GET /files/{id}/download, so the assertion does
// not depend on the endpoint it would otherwise be checking.

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/uploadclient"
)

// ------------------------------------------------------------- wire shapes --
//
// Decoded into the harness's own structs rather than the api package's: a
// silent change to the frozen contract should break a test here, not be
// absorbed by a shared type.

// UploadStatus is the status shape every upload response is phrased in.
type UploadStatus struct {
	UploadID         uuid.UUID  `json:"upload_id"`
	Mode             string     `json:"mode"`
	FileName         string     `json:"file_name"`
	FileSize         int64      `json:"file_size"`
	PartSize         int64      `json:"part_size"`
	PartsTotal       int        `json:"parts_total"`
	Fingerprint      string     `json:"fingerprint"`
	ParentID         *uuid.UUID `json:"parent_id"`
	Status           string     `json:"status"`
	ConfirmedParts   []int      `json:"confirmed_parts"`
	NodeID           *uuid.UUID `json:"node_id"`
	SessionExpiresAt time.Time  `json:"session_expires_at"`
}

// PresignedPart is one part URL on the wire.
type PresignedPart struct {
	PartNumber int       `json:"part_number"`
	URL        string    `json:"url"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// CreateUpload is the POST /uploads body.
type CreateUpload struct {
	UploadStatus
	Presigned   []PresignedPart `json:"presigned"`
	VerifyParts []int           `json:"verify_parts"`
}

// ResumeUpload is the POST /uploads/{id}/resume body. Missing is absent, not
// empty, on a verification bounce.
type ResumeUpload struct {
	UploadStatus
	Missing     []PresignedPart `json:"missing"`
	VerifyParts []int           `json:"verify_parts"`
}

// CompletedUpload is the POST /uploads/{id}/complete body. ParentID is present
// only when the file was re-parented to the user's root.
type CompletedUpload struct {
	NodeID   uuid.UUID  `json:"node_id"`
	Name     string     `json:"name"`
	ParentID *uuid.UUID `json:"parent_id"`
}

// UploadList is the GET /uploads envelope.
type UploadList struct {
	Items      []UploadStatus `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

// ----------------------------------------------------------------- content --

// RandomBytes returns n deterministic pseudo-random bytes. Deterministic so a
// failure is reproducible; pseudo-random so Garage's zstd cannot collapse it
// and the transfer is a real one.
func RandomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // fixture entropy, not a secret
	_, _ = r.Read(b)
	return b
}

// SHA256Hex is the whole-file digest the complete endpoint takes.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// MD5Hex is a part's digest, which is also what Garage returns as its ETag.
func MD5Hex(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // S3 part integrity is MD5 by protocol
	return hex.EncodeToString(sum[:])
}

// NormalizeETag mirrors the server's normalization: no weak-validator prefix,
// no quotes, lower case.
func NormalizeETag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	s = strings.TrimPrefix(s, "w/")
	s = strings.Trim(s, `"`)
	return strings.ToLower(s)
}

// ------------------------------------------------------------------ servers --

// SpawnServerWithEnv starts an extra server carrying additional environment,
// e.g. SpawnServerWithEnv(t, "DRIVE_PRESIGN_TTL=2s") for the expired-URL case.
//
// A presign TTL is signed into the URL rather than stored in a row, so it is
// the one deadline in the system that backdating cannot move -- the only way to
// watch a URL expire is to make the window short and let it pass.
func (h *Harness) SpawnServerWithEnv(t testing.TB, extra ...string) *Child {
	t.Helper()

	port, err := freePort()
	if err != nil {
		t.Fatalf("%v", err)
	}
	child := newChild(h.binary, port)
	child.env = append(child.env, extra...)
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

// ------------------------------------------------------------------ objects --

// ObjectKeyOf returns the Garage key behind a published file node.
func (h *Harness) ObjectKeyOf(t testing.TB, nodeID uuid.UUID) string {
	t.Helper()
	const q = `SELECT b.object_key FROM blobs b JOIN nodes n ON n.blob_id = b.id WHERE n.id = $1`
	var key string
	if err := h.Pool.QueryRow(context.Background(), q, nodeID).Scan(&key); err != nil {
		t.Fatalf("testutil: object key of node %s: %v", nodeID, err)
	}
	return key
}

// GetObject reads an object out of Garage whole.
func (h *Harness) GetObject(t testing.TB, key string) []byte {
	t.Helper()
	out, err := h.S3.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(h.Cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("testutil: getting %s: %v", key, err)
	}
	defer out.Body.Close()
	raw, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("testutil: reading %s: %v", key, err)
	}
	return raw
}

// DownloadNode returns the bytes stored behind a file node, read from the
// store directly. That is deliberate: it is the independent oracle the
// download endpoint's own tests are checked against.
func (h *Harness) DownloadNode(t testing.TB, nodeID uuid.UUID) []byte {
	t.Helper()
	return h.GetObject(t, h.ObjectKeyOf(t, nodeID))
}

// DigestNode is DownloadNode's sha256, for the multi-GB cases where holding the
// bytes twice is not an option.
func (h *Harness) DigestNode(t testing.TB, nodeID uuid.UUID) string {
	t.Helper()
	key := h.ObjectKeyOf(t, nodeID)
	out, err := h.S3.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(h.Cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("testutil: getting %s: %v", key, err)
	}
	defer out.Body.Close()

	h256 := sha256.New()
	if _, err := io.Copy(h256, out.Body); err != nil {
		t.Fatalf("testutil: hashing %s: %v", key, err)
	}
	return hex.EncodeToString(h256.Sum(nil))
}

// ObjectExists reports whether Garage still holds an object. A purge that
// dropped the last reference must eventually make this false.
func (h *Harness) ObjectExists(t testing.TB, key string) bool {
	t.Helper()
	_, err := h.S3.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(h.Cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true
	}
	var missing *types.NotFound
	var noKey *types.NoSuchKey
	if errors.As(err, &missing) || errors.As(err, &noKey) {
		return false
	}
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) && status.HTTPStatusCode() == http.StatusNotFound {
		return false
	}
	t.Fatalf("testutil: heading %s: %v", key, err)
	return false
}

// ------------------------------------------------------- out-of-band S3 --

// OOBCreateMultipart opens a multipart upload with no session row behind it.
func (h *Harness) OOBCreateMultipart(t testing.TB, key string) string {
	t.Helper()
	out, err := h.S3.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String(h.Cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("testutil: creating multipart for %s: %v", key, err)
	}
	return aws.ToString(out.UploadId)
}

// OOBUploadPart PUTs one part and returns its normalized ETag.
func (h *Harness) OOBUploadPart(t testing.TB, key, uploadID string, n int, data []byte) string {
	t.Helper()
	num := int32(n)
	out, err := h.S3.UploadPart(context.Background(), &s3.UploadPartInput{
		Bucket:     aws.String(h.Cfg.S3Bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: &num,
		Body:       bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("testutil: uploading part %d of %s: %v", n, key, err)
	}
	return NormalizeETag(aws.ToString(out.ETag))
}

// OOBCompleteMultipart finishes a multipart upload behind the server's back.
// This is the crash window: after it returns, the object exists at its full
// size and the multipart is gone -- ListParts and a retried complete both
// answer NoSuchUpload -- while no nodes row references any of it.
func (h *Harness) OOBCompleteMultipart(t testing.TB, key, uploadID string, etags map[int]string) {
	t.Helper()

	numbers := make([]int, 0, len(etags))
	for n := range etags {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)

	parts := make([]types.CompletedPart, 0, len(numbers))
	for _, n := range numbers {
		num := int32(n)
		parts = append(parts, types.CompletedPart{
			PartNumber: &num,
			ETag:       aws.String(`"` + etags[n] + `"`),
		})
	}
	_, err := h.S3.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(h.Cfg.S3Bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		t.Fatalf("testutil: completing multipart %s: %v", uploadID, err)
	}
}

// OOBAbortMultipart discards a multipart upload behind the server's back, which
// is what an operator or a GC pass on another node looks like from the session's
// point of view: the row still says 'active', and every S3 call for it now
// answers NoSuchUpload.
func (h *Harness) OOBAbortMultipart(t testing.TB, key, uploadID string) {
	t.Helper()
	_, err := h.S3.AbortMultipartUpload(context.Background(), &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(h.Cfg.S3Bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		t.Fatalf("testutil: aborting multipart %s out of band: %v", uploadID, err)
	}
}

// CountMultiparts reports how many multipart uploads Garage currently holds.
func (h *Harness) CountMultiparts(t testing.TB) int {
	t.Helper()
	var (
		n         int
		keyMarker *string
		idMarker  *string
	)
	for {
		out, err := h.S3.ListMultipartUploads(context.Background(), &s3.ListMultipartUploadsInput{
			Bucket:         aws.String(h.Cfg.S3Bucket),
			KeyMarker:      keyMarker,
			UploadIdMarker: idMarker,
		})
		if err != nil {
			t.Fatalf("testutil: listing multipart uploads: %v", err)
		}
		n += len(out.Uploads)
		if !aws.ToBool(out.IsTruncated) {
			return n
		}
		keyMarker, idMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}

// ------------------------------------------------------------ session state --

// SessionRow is what a test needs to see of an upload_sessions row without
// going through the API -- the constructed crash windows assert on the state
// the API deliberately hides.
type SessionRow struct {
	Status     string
	ObjectKey  string
	S3UploadID *string
	NodeID     *uuid.UUID
	PartSize   int64
	PartsTotal int
	FileSize   int64
	ExpiresAt  time.Time
}

// Session reads one upload_sessions row.
func (h *Harness) Session(t testing.TB, id uuid.UUID) SessionRow {
	t.Helper()
	const q = `SELECT status, object_key, s3_upload_id, node_id, part_size, parts_total, file_size, expires_at
		         FROM upload_sessions WHERE id = $1`
	var r SessionRow
	err := h.Pool.QueryRow(context.Background(), q, id).Scan(
		&r.Status, &r.ObjectKey, &r.S3UploadID, &r.NodeID, &r.PartSize, &r.PartsTotal, &r.FileSize, &r.ExpiresAt)
	if err != nil {
		t.Fatalf("testutil: reading upload session %s: %v", id, err)
	}
	return r
}

// InjectCompleting puts a session into the state a finalizer that died would
// have left: claimed, no node published, and old enough for the collector to
// take it over.
//
// Constructed, not timed: SQL state injection plus out-of-band S3 calls, then
// one GC pass. The window it reproduces is a few milliseconds wide, so racing
// for it would produce a flake rather than a proof.
func (h *Harness) InjectCompleting(t testing.TB, id uuid.UUID, age time.Duration) {
	t.Helper()
	const q = `UPDATE upload_sessions
		          SET status = 'completing', node_id = NULL, updated_at = now() - make_interval(secs => $2)
		        WHERE id = $1`
	tag, err := h.Pool.Exec(context.Background(), q, id, age.Seconds())
	if err != nil {
		t.Fatalf("testutil: injecting completing state on %s: %v", id, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("testutil: injecting completing state on %s: %d rows, want 1", id, tag.RowsAffected())
	}
}

// ExpireUpload pushes a session past its sliding expiry without waiting seven
// days for it.
func (h *Harness) ExpireUpload(t testing.TB, id uuid.UUID) {
	t.Helper()
	if n := Backdate(t, h.Pool, "upload_sessions", "expires_at", 8*24*time.Hour, "id = $1", id); n != 1 {
		t.Fatalf("testutil: ExpireUpload: %d rows for %s, want 1", n, id)
	}
}

// ---------------------------------------------------------------- uploader --

// maxRehandshake is the client-side budget the contract sets on consecutive
// re-handshakes for one part: three, then the engine would pause. The driver
// honours it so the expired-presign case measures the real behaviour rather
// than an infinite retry loop that would pass no matter what.
const maxRehandshake = 3

// Uploader walks the frozen contract over real HTTP against a real server.
//
// It is deliberately granular -- Create, PutPart and Confirm are separate calls
// -- because half the battery is about what happens when one of them is
// interrupted, sent twice, or sent with a wrong size.
type Uploader struct {
	C        *Client
	ParentID uuid.UUID
	Name     string
	Mime     string
	Size     int64
	// Fingerprint defaults to uploadclient.Fingerprint(Src, Name, Size,
	// ModMillis); the chimera case sets it explicitly so two different files
	// share one.
	Fingerprint    string
	ConflictPolicy string
	// ModMillis is the fingerprint's lastModified field, in MILLISECONDS -- the
	// unit the pinned recipe uses and the golden vector's 1700000000000.
	ModMillis int64
	Src       io.ReaderAt

	// ID, PartSize and PartsTotal are filled by Create.
	ID         uuid.UUID
	PartSize   int64
	PartsTotal int

	urls map[int]string
	http *http.Client
}

// NewUpload prepares an uploader over in-memory bytes.
func (h *Harness) NewUpload(t testing.TB, c *Client, parentID uuid.UUID, name string, data []byte) *Uploader {
	t.Helper()
	return h.NewUploadFrom(t, c, parentID, name, bytes.NewReader(data), int64(len(data)))
}

// NewUploadFrom prepares an uploader over any random-access source, which is
// how the multi-GB cases avoid holding the file in memory.
func (h *Harness) NewUploadFrom(t testing.TB, c *Client, parentID uuid.UUID, name string, src io.ReaderAt, size int64) *Uploader {
	t.Helper()
	u := &Uploader{
		C:         c,
		ParentID:  parentID,
		Name:      name,
		Mime:      "application/octet-stream",
		Size:      size,
		ModMillis: 1_700_000_000_000,
		Src:       src,
		urls:      map[int]string{},
		// Long: a part PUT of a 100 MiB part against a local Garage is still a
		// real transfer, and a multi-GB run makes several hundred of them.
		http: &http.Client{Timeout: 10 * time.Minute},
	}
	// The production recipe, not a second implementation of it. The battery's
	// whole value is that it certifies what a real client sends; a testutil-local
	// fingerprint is self-consistent, passes every test including the chimera
	// cases, and certifies nothing.
	fp, err := uploadclient.Fingerprint(src, name, size, u.ModMillis)
	if err != nil {
		t.Fatalf("testutil: fingerprinting %s: %v", name, err)
	}
	u.Fingerprint = fp
	t.Cleanup(func() { h.discardUpload(t, u.ID) })
	return u
}

// discardUpload is every uploader's teardown: retire the session and delete the
// bytes it put in Garage.
//
// Without it each loop of the battery leaves its objects behind for good. The
// next run resets the schema, which drops the blobs rows -- and an object whose
// row is gone is invisible to the collector, so nothing will ever reclaim it.
//
// Best effort throughout: this runs after the assertions, and a failure to tidy
// up must not turn a passing test red.
func (h *Harness) discardUpload(t testing.TB, id uuid.UUID) {
	if id == uuid.Nil {
		return
	}
	ctx := context.Background()

	const q = `SELECT status, object_key, s3_upload_id FROM upload_sessions WHERE id = $1`
	var (
		status     string
		key        string
		s3UploadID *string
	)
	if err := h.Pool.QueryRow(ctx, q, id).Scan(&status, &key, &s3UploadID); err != nil {
		return // the create never landed a row
	}

	if status != StatusUploadDone && s3UploadID != nil {
		_, _ = h.S3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(h.Cfg.S3Bucket),
			Key:      aws.String(key),
			UploadId: aws.String(*s3UploadID),
		})
		if _, err := h.Pool.Exec(ctx,
			`UPDATE upload_sessions SET status = 'aborted' WHERE id = $1 AND status <> 'done'`, id,
		); err != nil {
			t.Logf("testutil: retiring upload session %s: %v", id, err)
		}
	}
	if _, err := h.S3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(h.Cfg.S3Bucket),
		Key:    aws.String(key),
	}); err != nil {
		t.Logf("testutil: deleting %s: %v", key, err)
	}
}

// StatusUploadDone is the one session status whose multipart must not be
// aborted: it no longer has one.
const StatusUploadDone = "done"

// At returns a copy of this uploader talking to another server, sharing the
// cookie jar and everything already uploaded. This is the restart step of the
// kill-9 case.
func (u *Uploader) At(baseURL string) *Uploader {
	clone := *u
	clone.C = u.C.At(baseURL)
	return &clone
}

// CreateBody is the request POST /uploads takes.
func (u *Uploader) CreateBody() map[string]any {
	body := map[string]any{
		"file_name":   u.Name,
		"file_size":   u.Size,
		"mime":        u.Mime,
		"parent_id":   u.ParentID,
		"fingerprint": u.Fingerprint,
	}
	if u.ConflictPolicy != "" {
		body["conflict_policy"] = u.ConflictPolicy
	}
	return body
}

// Create posts the handshake and returns the raw response, so a test can assert
// on 404/409/422 as easily as on success. A 200 or 201 fills in the session.
func (u *Uploader) Create(t testing.TB) *Resp {
	t.Helper()
	resp := u.C.Post(t, "/api/uploads", u.CreateBody())
	if resp.Status == http.StatusCreated || resp.Status == http.StatusOK {
		var out CreateUpload
		resp.JSON(&out)
		u.adopt(out.UploadStatus)
		u.take(out.Presigned)
	}
	return resp
}

// MustCreate creates and insists on the expected status.
func (u *Uploader) MustCreate(t testing.TB, status int) CreateUpload {
	t.Helper()
	resp := u.Create(t).Expect(status)
	var out CreateUpload
	resp.JSON(&out)
	return out
}

func (u *Uploader) adopt(s UploadStatus) {
	u.ID = s.UploadID
	u.PartSize = s.PartSize
	u.PartsTotal = s.PartsTotal
}

func (u *Uploader) take(parts []PresignedPart) {
	if u.urls == nil {
		u.urls = map[int]string{}
	}
	for _, p := range parts {
		u.urls[p.PartNumber] = p.URL
	}
}

// Path is this session's URL prefix.
func (u *Uploader) Path() string { return "/api/uploads/" + u.ID.String() }

// Status fetches GET /uploads/{id}.
func (u *Uploader) Status(t testing.TB) UploadStatus {
	t.Helper()
	var s UploadStatus
	u.C.Get(t, u.Path()).Expect(http.StatusOK).JSON(&s)
	return s
}

// Resume calls the handshake. md5s may be nil; the server ignores it unless
// verification is armed.
func (u *Uploader) Resume(t testing.TB, md5s map[string]string) ResumeUpload {
	t.Helper()
	body := map[string]any{}
	if md5s != nil {
		body["part_md5s"] = md5s
	}
	var out ResumeUpload
	u.C.Post(t, u.Path()+"/resume", body).Expect(http.StatusOK).JSON(&out)
	u.adopt(out.UploadStatus)
	u.take(out.Missing)
	return out
}

// TryResume calls the handshake without insisting on a status.
func (u *Uploader) TryResume(t testing.TB, md5s map[string]string) *Resp {
	t.Helper()
	body := map[string]any{}
	if md5s != nil {
		body["part_md5s"] = md5s
	}
	return u.C.Post(t, u.Path()+"/resume", body)
}

// ResumeVerified is the handshake as the engine performs it: call it, and if
// the response is a verification bounce, recompute the pinned parts' MD5s from
// the file in hand and call again.
//
// Reconciliation arms the guard whenever it finds drift, which is precisely the
// kill-9 window, so any resume after an interruption has to be able to answer
// it or the upload can never continue.
func (u *Uploader) ResumeVerified(t testing.TB) ResumeUpload {
	t.Helper()
	out := u.Resume(t, nil)
	if len(out.VerifyParts) == 0 {
		return out
	}
	md5s := make(map[string]string, len(out.VerifyParts))
	for _, n := range out.VerifyParts {
		md5s[strconv.Itoa(n)] = u.PartMD5(t, n)
	}
	return u.Resume(t, md5s)
}

// Part returns part n's bytes.
func (u *Uploader) Part(t testing.TB, n int) []byte {
	t.Helper()
	off := int64(n-1) * u.PartSize
	size := u.PartSize
	if rem := u.Size - off; rem < size {
		size = rem
	}
	if size < 0 {
		t.Fatalf("testutil: part %d is past the end of a %d-byte file", n, u.Size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(u.Src, off, size), buf); err != nil {
		t.Fatalf("testutil: reading part %d: %v", n, err)
	}
	return buf
}

// PartMD5 is what the client sends with a confirmation, and what the chimera
// handshake proves it can still compute from the file it is holding.
func (u *Uploader) PartMD5(t testing.TB, n int) string {
	t.Helper()
	return MD5Hex(u.Part(t, n))
}

// SHA256 is the whole-file digest complete takes.
func (u *Uploader) SHA256(t testing.TB) string {
	t.Helper()
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(u.Src, 0, u.Size)); err != nil {
		t.Fatalf("testutil: hashing the source: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PutResult is one part PUT against Garage.
type PutResult struct {
	Status int
	ETag   string
	MD5    string
	Size   int64
	Body   string
}

// Expired reports Garage's expired-presign signature, measured in the day-0
// spike: 400 carrying <Code>InvalidRequest</Code>, not the 403 S3 semantics
// would suggest. A plain 400 without that code stays a hard failure.
func (p PutResult) Expired() bool {
	return p.Status == http.StatusForbidden ||
		(p.Status == http.StatusBadRequest && strings.Contains(p.Body, "<Code>InvalidRequest</Code>"))
}

// PutPart PUTs part n at the given URL without judging the result.
func (u *Uploader) PutPart(t testing.TB, n int, url string) PutResult {
	t.Helper()
	return u.putBytes(t, n, url, u.Part(t, n))
}

// PutShort PUTs the first size bytes of part n at the given URL: a client that
// sliced the file at the wrong part size. Garage stores it happily, which is why
// reconciliation has to refuse it.
func (u *Uploader) PutShort(t testing.TB, n int, url string, size int64) PutResult {
	t.Helper()
	return u.putBytes(t, n, url, u.Part(t, n)[:size])
}

func (u *Uploader) putBytes(t testing.TB, n int, url string, data []byte) PutResult {
	t.Helper()

	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("testutil: building the PUT for part %d: %v", n, err)
	}
	req.ContentLength = int64(len(data))
	// Expect: 100-continue, for the same reason the production client sets it.
	// Garage rejects an expired presign before it has read the body and closes
	// the socket; without this Go is already streaming a 10 MiB part into a
	// half-closed connection and returns "broken pipe" instead of the 400 that
	// says the URL expired. Measured against Garage v2.3.0: it answers the
	// continue promptly on the happy path (no added latency) and answers the
	// expired case with the 400 in a few milliseconds, body never sent.
	req.Header.Set("Expect", "100-continue")

	resp, err := u.http.Do(req)
	if err != nil {
		t.Fatalf("testutil: PUT of part %d: %v", n, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	return PutResult{
		Status: resp.StatusCode,
		ETag:   NormalizeETag(resp.Header.Get("ETag")),
		MD5:    MD5Hex(data),
		Size:   int64(len(data)),
		Body:   string(raw),
	}
}

// URLFor returns a URL for part n, refilling from the handshake when the pool
// does not hold one. fresh forces a new handshake, which is the re-handshake
// the expired-URL path performs.
func (u *Uploader) URLFor(t testing.TB, n int, fresh bool) string {
	t.Helper()
	if !fresh {
		if url, ok := u.urls[n]; ok {
			return url
		}
	}
	u.ResumeVerified(t)
	url, ok := u.urls[n]
	if !ok {
		t.Fatalf("testutil: the handshake returned no URL for part %d", n)
	}
	return url
}

// Confirm posts the part confirmation and returns the raw response.
func (u *Uploader) Confirm(t testing.TB, n int, etag, md5hex string, size int64) *Resp {
	t.Helper()
	return u.C.Post(t, u.Path()+"/parts/"+strconv.Itoa(n), map[string]any{
		"etag": etag,
		"md5":  md5hex,
		"size": size,
	})
}

// TryConfirm posts a part confirmation and returns the transport error instead
// of failing the test.
//
// This is the kill-9 shape: the bytes go to Garage, which is still up, while
// the confirmation goes to a server that no longer exists. A client has to
// survive that, and a helper that called t.Fatal on a refused connection could
// never express it.
func (u *Uploader) TryConfirm(t testing.TB, n int, etag, md5hex string, size int64) (*Resp, error) {
	t.Helper()

	path := u.Path() + "/parts/" + strconv.Itoa(n)
	raw, err := json.Marshal(map[string]any{"etag": etag, "md5": md5hex, "size": size})
	if err != nil {
		t.Fatalf("testutil: encoding the confirmation of part %d: %v", n, err)
	}
	req, err := http.NewRequest(http.MethodPost, u.C.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("testutil: building the confirmation of part %d: %v", n, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Drive-Client", "web")

	resp, err := u.C.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &Resp{Status: resp.StatusCode, Body: body, Header: resp.Header, t: t, what: "POST " + path}, nil
}

// UploadPart transfers and confirms one part, re-handshaking for a fresh URL
// when Garage rejects the one it has as expired -- at most three times in a
// row, exactly like the engine's budget.
func (u *Uploader) UploadPart(t testing.TB, n int) {
	t.Helper()
	fresh := false
	for attempt := 0; ; attempt++ {
		res := u.PutPart(t, n, u.URLFor(t, n, fresh))
		if res.Status == http.StatusOK {
			if res.ETag != res.MD5 {
				t.Fatalf("part %d: Garage returned ETag %q, want the client MD5 %q", n, res.ETag, res.MD5)
			}
			u.Confirm(t, n, res.ETag, res.MD5, res.Size).Expect(http.StatusOK)
			return
		}
		if !res.Expired() || attempt >= maxRehandshake {
			t.Fatalf("part %d: PUT answered %d after %d re-handshake(s): %s",
				n, res.Status, attempt, res.Body)
		}
		// An expired URL costs a re-handshake, never the integrity budget.
		delete(u.urls, n)
		fresh = true
	}
}

// UploadParts transfers the given parts.
func (u *Uploader) UploadParts(t testing.TB, numbers ...int) {
	t.Helper()
	for _, n := range numbers {
		u.UploadPart(t, n)
	}
}

// UploadAll transfers every part the server still considers missing.
func (u *Uploader) UploadAll(t testing.TB) {
	t.Helper()
	for _, n := range u.Missing(t) {
		u.UploadPart(t, n)
	}
}

// Missing asks the server which parts it is still waiting for.
func (u *Uploader) Missing(t testing.TB) []int {
	t.Helper()
	have := map[int]bool{}
	for _, n := range u.Status(t).ConfirmedParts {
		have[n] = true
	}
	var missing []int
	for n := 1; n <= u.PartsTotal; n++ {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	return missing
}

// Complete posts the finalize request and returns the raw response.
func (u *Uploader) Complete(t testing.TB) *Resp {
	t.Helper()
	return u.C.Post(t, u.Path()+"/complete", map[string]any{"sha256": u.SHA256(t)})
}

// MustComplete finalizes and insists on a published node.
func (u *Uploader) MustComplete(t testing.TB) CompletedUpload {
	t.Helper()
	var out CompletedUpload
	u.Complete(t).Expect(http.StatusOK).JSON(&out)
	return out
}

// Run is the whole lifecycle: create, transfer every part, publish.
func (u *Uploader) Run(t testing.TB, createStatus int) CompletedUpload {
	t.Helper()
	u.MustCreate(t, createStatus)
	u.UploadAll(t)
	return u.MustComplete(t)
}

// Cancel discards the session, and is what a suite's teardown calls so a failed
// test does not leave a multipart upload behind for the next run to sweep.
func (u *Uploader) Cancel(t testing.TB) {
	t.Helper()
	if u.ID == uuid.Nil {
		return
	}
	u.C.Delete(t, u.Path())
}

// ------------------------------------------------------------- big fixtures --

// BigFixture returns a path to a file of exactly size bytes, creating it once
// and reusing it afterwards. sparse writes a hole instead of bytes, which is
// what makes an 11 GiB part-count fixture cost no disk.
//
// It fails the test unless the filesystem has twice the file's size free:
// running out mid-upload fails in ways that look like protocol bugs.
func BigFixture(t testing.TB, dir, name string, size int64, sparse bool) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("testutil: creating %s: %v", dir, err)
	}
	path := dir + "/" + name
	if st, err := os.Stat(path); err == nil && st.Size() == size {
		return path
	}
	if !sparse {
		RequireFreeSpace(t, dir, 2*size)
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("testutil: creating %s: %v", path, err)
	}
	defer f.Close()

	if sparse {
		// A hole plus a written tail: the object is still `size` bytes and still
		// splits into the same number of parts, but only the tail costs disk.
		if err := f.Truncate(size); err != nil {
			t.Fatalf("testutil: truncating %s to %d: %v", path, size, err)
		}
		if _, err := f.WriteAt(RandomBytes(1<<20, 7), size-(1<<20)); err != nil {
			t.Fatalf("testutil: writing the tail of %s: %v", path, err)
		}
		return path
	}

	const chunk = 8 << 20
	buf := RandomBytes(chunk, 11)
	for written := int64(0); written < size; {
		n := int64(chunk)
		if rem := size - written; rem < n {
			n = rem
		}
		// Vary the block so the file does not compress to nothing in Garage.
		buf[0] = byte(written / chunk)
		buf[1] = byte(written / chunk >> 8)
		if _, err := f.Write(buf[:n]); err != nil {
			t.Fatalf("testutil: writing %s: %v", path, err)
		}
		written += n
	}
	return path
}

// RequireFreeSpace skips the test unless the filesystem holding dir has at
// least want bytes free. Every big run does this check first: filling the
// Docker VM's disk mid-upload fails in ways that look like protocol bugs and
// are not.
func RequireFreeSpace(t testing.TB, dir string, want int64) {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatalf("testutil: statfs %s: %v", dir, err)
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	if free < want {
		t.Skipf("testutil: %s has %d bytes free, this run needs %d", dir, free, want)
	}
}

// OpenFixture opens a fixture file and closes it with the test.
func OpenFixture(t testing.TB, path string) (*os.File, int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("testutil: opening %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	st, err := f.Stat()
	if err != nil {
		t.Fatalf("testutil: stat %s: %v", path, err)
	}
	return f, st.Size()
}
