package uploadclient

// Protocol tests. Every one of them runs against the fake Appendix server in
// fake_test.go -- no database, no Garage, no real drive binary.

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func makeData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

func md5Hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func req(data []byte) UploadRequest {
	return UploadRequest{
		Source:   bytes.NewReader(data),
		Name:     goldenName,
		ParentID: "parent-1",
		Mime:     "application/pdf",
		ModTime:  time.UnixMilli(goldenModMs),
	}
}

// assembled concatenates the parts the fake received, in order.
func (f *fake) assembled() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []byte
	for n := 1; n <= f.partsTotal(); n++ {
		out = append(out, f.stored[n]...)
	}
	return out
}

// ----------------------------------------------------------------- happy path --

func TestUploadHappyPath(t *testing.T) {
	data := makeData(250)
	f := newFake(t, 250, 100)
	c := f.client(WithPartConcurrency(2))

	res, err := c.Upload(context.Background(), req(data))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if res.NodeID != "node-9" || res.Name != goldenName {
		t.Fatalf("Result = %+v", res)
	}
	if res.PartsUploaded != 3 || res.PartsTotal != 3 {
		t.Fatalf("parts: uploaded %d of %d, want 3 of 3", res.PartsUploaded, res.PartsTotal)
	}
	if res.Resumed {
		t.Errorf("a 201 create must not report Resumed")
	}
	if got := f.assembled(); !bytes.Equal(got, data) {
		t.Fatalf("stored %d bytes, want the original %d", len(got), len(data))
	}
	if f.completeSHA != sha256Hex(data) {
		t.Fatalf("complete sha256 = %s, want %s", f.completeSHA, sha256Hex(data))
	}
	if res.SHA256 != sha256Hex(data) {
		t.Fatalf("Result.SHA256 = %s", res.SHA256)
	}

	// The create carried the fingerprint this package computes, and the result
	// echoes it.
	want, err := Fingerprint(bytes.NewReader(data), goldenName, int64(len(data)), goldenModMs)
	if err != nil {
		t.Fatal(err)
	}
	if f.createReq.Fingerprint != want {
		t.Fatalf("create fingerprint = %s, want %s", f.createReq.Fingerprint, want)
	}
	if f.createReq.FileSize != 250 || f.createReq.Mime != "application/pdf" {
		t.Fatalf("create request = %+v", f.createReq)
	}
	if f.count("resume") != 0 {
		t.Errorf("a create that hands out every URL needs no handshake, got %d", f.count("resume"))
	}
}

// The battery's source is an *os.File it must never buffer, so the PUT has to
// carry an explicit Content-Length and no Content-Type (objects are stored
// without one on purpose) and no credentials (the URL is the credential).
func TestUploadPartPutShape(t *testing.T) {
	data := makeData(250)
	f := newFake(t, 250, 100)

	var bad []string
	f.putHook = func(w http.ResponseWriter, r *http.Request, n, attempt, gen int, body []byte) bool {
		wantLen := int64(100)
		if n == 3 {
			wantLen = 50
		}
		if r.ContentLength != wantLen {
			bad = append(bad, "part "+strconv.Itoa(n)+" Content-Length "+strconv.FormatInt(r.ContentLength, 10))
		}
		if r.Header.Get("Content-Type") != "" {
			bad = append(bad, "part "+strconv.Itoa(n)+" carried a Content-Type")
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get(ClientHeader) != "" {
			bad = append(bad, "part "+strconv.Itoa(n)+" carried credentials")
		}
		if r.TransferEncoding != nil {
			bad = append(bad, "part "+strconv.Itoa(n)+" was chunked")
		}
		// The transport must hold the body back until the status line arrives.
		// Garage rejects an expired presign before reading the body and closes
		// the socket behind the rejection; a part already streaming into that
		// connection surfaces as "broken pipe" rather than the 400 that means
		// "expired", and the part is charged the integrity budget where PLAN
		// promises a free re-handshake.
		if r.Header.Get("Expect") != "100-continue" {
			bad = append(bad, "part "+strconv.Itoa(n)+" did not send Expect: 100-continue")
		}
		return false
	}

	if _, err := f.client(WithPartConcurrency(1)).Upload(context.Background(), req(data)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(bad) > 0 {
		t.Fatalf("part PUT shape: %s", strings.Join(bad, "; "))
	}
}

// ------------------------------------------------------------------- 0 bytes --

func TestUploadZeroByte(t *testing.T) {
	f := newFake(t, 0, 100)
	c := f.client()

	res, err := c.Upload(context.Background(), UploadRequest{
		Source:   bytes.NewReader(nil),
		Name:     "empty.txt",
		ParentID: "parent-1",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if f.count("put") != 0 {
		t.Fatalf("a 0-byte upload must PUT nothing, got %d PUTs", f.count("put"))
	}
	if res.PartsTotal != 0 || res.PartsUploaded != 0 {
		t.Fatalf("parts = %d/%d, want 0/0", res.PartsUploaded, res.PartsTotal)
	}
	// e3b0c442... is sha256 of the empty string; complete requires a real digest.
	if want := sha256Hex(nil); f.completeSHA != want {
		t.Fatalf("complete sha256 = %q, want the empty digest %s", f.completeSHA, want)
	}
	if res.NodeID != "node-9" {
		t.Fatalf("Result = %+v", res)
	}
}

// --------------------------------------------------------------- expired URLs --

// Garage answers an expired presign with 400 + <Code>InvalidRequest</Code>,
// measured in the day-0 spike. Treating that as a hard failure would burn the
// integrity budget instead of re-handshaking.
func TestUploadExpiredURL400InvalidRequest(t *testing.T) {
	data := makeData(250)
	f := newFake(t, 250, 100)
	f.putHook = func(w http.ResponseWriter, r *http.Request, n, attempt, gen int, body []byte) bool {
		if n == 1 && attempt == 1 {
			writeExpired(w)
			return true
		}
		return false
	}

	if _, err := f.client(WithPartConcurrency(1)).Upload(context.Background(), req(data)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := f.count("resume"); got != 1 {
		t.Fatalf("expired URL should trigger exactly one re-handshake, got %d", got)
	}
	if f.putAttempts[1] != 2 {
		t.Fatalf("part 1 PUT attempts = %d, want 2", f.putAttempts[1])
	}
	if !bytes.Equal(f.assembled(), data) {
		t.Fatal("bytes did not survive the re-handshake")
	}
}

func TestUploadExpiredURL403(t *testing.T) {
	data := makeData(250)
	f := newFake(t, 250, 100)
	f.putHook = func(w http.ResponseWriter, r *http.Request, n, attempt, gen int, body []byte) bool {
		if n == 2 && attempt == 1 {
			w.WriteHeader(http.StatusForbidden)
			return true
		}
		return false
	}

	if _, err := f.client(WithPartConcurrency(1)).Upload(context.Background(), req(data)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := f.count("resume"); got != 1 {
		t.Fatalf("403 should trigger exactly one re-handshake, got %d", got)
	}
	if !bytes.Equal(f.assembled(), data) {
		t.Fatal("bytes did not survive the re-handshake")
	}
}

// A plain 400 with no InvalidRequest code is a hard failure, so it charges the
// integrity budget rather than looping on fresh URLs.
func TestUploadPlainBadRequestIsHardFailure(t *testing.T) {
	data := makeData(100)
	f := newFake(t, 100, 100)
	f.putHook = func(w http.ResponseWriter, r *http.Request, n, attempt, gen int, body []byte) bool {
		http.Error(w, "<Error><Code>SomethingElse</Code></Error>", http.StatusBadRequest)
		return true
	}

	_, err := f.client(WithPartConcurrency(1)).Upload(context.Background(), req(data))
	if err == nil {
		t.Fatal("expected the upload to fail")
	}
	if f.putAttempts[1] != IntegrityBudget {
		t.Fatalf("part 1 PUT attempts = %d, want the integrity budget %d", f.putAttempts[1], IntegrityBudget)
	}
	if strings.Contains(err.Error(), "clock skew") {
		t.Fatalf("a bare 400 must not be reported as URL expiry: %v", err)
	}
}

// The cap that guards against clock skew: three consecutive re-handshakes, then
// stop and say why.
func TestUploadRehandshakeCap(t *testing.T) {
	data := makeData(100)
	f := newFake(t, 100, 100)
	f.putHook = func(w http.ResponseWriter, r *http.Request, n, attempt, gen int, body []byte) bool {
		writeExpired(w)
		return true
	}

	_, err := f.client(WithPartConcurrency(1)).Upload(context.Background(), req(data))
	if err == nil {
		t.Fatal("expected failure once the re-handshake budget ran out")
	}
	if !strings.Contains(err.Error(), "clock skew") {
		t.Fatalf("error should name the cause it exists to catch, got: %v", err)
	}
	if got := f.count("resume"); got != MaxRehandshakes {
		t.Fatalf("re-handshakes = %d, want %d", got, MaxRehandshakes)
	}
	if f.putAttempts[1] != MaxRehandshakes+1 {
		t.Fatalf("part 1 PUT attempts = %d, want %d", f.putAttempts[1], MaxRehandshakes+1)
	}
	if f.count("complete") != 0 {
		t.Fatal("a failed transfer must not complete")
	}
}

// ---------------------------------------------------------------- integrity --

func TestUploadIntegrityMismatchThenSuccess(t *testing.T) {
	data := makeData(250)
	f := newFake(t, 250, 100)
	f.putHook = func(w http.ResponseWriter, r *http.Request, n, attempt, gen int, body []byte) bool {
		if n == 1 && attempt == 1 {
			// Right status, wrong bytes: the ETag is not the MD5 we sent.
			w.Header().Set("ETag", `"00000000000000000000000000000000"`)
			w.WriteHeader(http.StatusOK)
			return true
		}
		return false
	}

	res, err := f.client(WithPartConcurrency(1)).Upload(context.Background(), req(data))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if f.putAttempts[1] != 2 {
		t.Fatalf("part 1 PUT attempts = %d, want 2", f.putAttempts[1])
	}
	if !bytes.Equal(f.assembled(), data) {
		t.Fatal("the retried part did not land")
	}
	if res.PartsUploaded != 3 {
		t.Fatalf("PartsUploaded = %d, want 3", res.PartsUploaded)
	}
}

func TestUploadIntegrityBudgetExhausted(t *testing.T) {
	data := makeData(100)
	f := newFake(t, 100, 100)
	f.putHook = func(w http.ResponseWriter, r *http.Request, n, attempt, gen int, body []byte) bool {
		w.Header().Set("ETag", `"ffffffffffffffffffffffffffffffff"`)
		w.WriteHeader(http.StatusOK)
		return true
	}

	_, err := f.client(WithPartConcurrency(1)).Upload(context.Background(), req(data))
	if err == nil {
		t.Fatal("expected failure once the integrity budget ran out")
	}
	if f.putAttempts[1] != IntegrityBudget {
		t.Fatalf("part 1 PUT attempts = %d, want %d", f.putAttempts[1], IntegrityBudget)
	}
	if !strings.Contains(err.Error(), "does not match MD5") {
		t.Fatalf("error should name the integrity failure, got: %v", err)
	}
}

// A quoted, upper-case ETag is what S3 and Garage actually return; comparing it
// unnormalized would fail every part.
func TestUploadNormalizesWeakQuotedETag(t *testing.T) {
	data := makeData(100)
	f := newFake(t, 100, 100)
	f.putHook = func(w http.ResponseWriter, r *http.Request, n, attempt, gen int, body []byte) bool {
		w.Header().Set("ETag", `W/"`+strings.ToUpper(md5Hex(body))+`"`)
		f.mu.Lock()
		f.stored[n] = body
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return true
	}

	if _, err := f.client().Upload(context.Background(), req(data)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if f.putAttempts[1] != 1 {
		t.Fatalf("a normalizable ETag must not be retried, attempts = %d", f.putAttempts[1])
	}
}

// ------------------------------------------------------------ chimera bounce --

func TestUploadVerifyPartsBounce(t *testing.T) {
	data := makeData(250)
	f := newFake(t, 250, 100)

	// A matched session holding parts 1 and 2, with the guard armed on them.
	f.matched = true
	f.confirmed[1], f.confirmed[2] = true, true
	f.ledgerMD5[1] = md5Hex(data[0:100])
	f.ledgerMD5[2] = md5Hex(data[100:200])
	f.verify = []int{1, 2}

	res, err := f.client(WithPartConcurrency(1)).Upload(context.Background(), req(data))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if got := f.lastResumeMD5s; len(got) != 2 ||
		got["1"] != md5Hex(data[0:100]) || got["2"] != md5Hex(data[100:200]) {
		t.Fatalf("resume part_md5s = %v, want the two pinned parts' real MD5s", got)
	}
	if f.count("resume") != 1 {
		t.Fatalf("resume calls = %d, want 1 (the create already told us the pins)", f.count("resume"))
	}
	if res.PartsUploaded != 1 {
		t.Fatalf("PartsUploaded = %d, want 1 -- parts 1 and 2 were already stored", res.PartsUploaded)
	}
	if !res.Resumed {
		t.Error("a matched create must report Resumed")
	}
	if !bytes.Equal(f.stored[3], data[200:250]) {
		t.Fatal("the one missing part did not land")
	}
}

// A resume handshake that finds the guard armed bounces first, with no URLs,
// and the client answers with MD5s computed from the source.
func TestResumeVerifyPartsBounce(t *testing.T) {
	data := makeData(250)
	f := newFake(t, 250, 100)
	f.confirmed[1] = true
	f.ledgerMD5[1] = md5Hex(data[0:100])
	f.verify = []int{1, 1}

	res, err := f.client(WithPartConcurrency(1)).Resume(context.Background(), f.uploadID, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	// Two resume calls: the bounce, then the verified one that carries URLs.
	if f.count("resume") != 2 {
		t.Fatalf("resume calls = %d, want 2 (bounce + verified)", f.count("resume"))
	}
	if f.lastResumeMD5s["1"] != md5Hex(data[0:100]) {
		t.Fatalf("verification MD5 = %q", f.lastResumeMD5s["1"])
	}
	if res.PartsUploaded != 2 {
		t.Fatalf("PartsUploaded = %d, want 2", res.PartsUploaded)
	}
	// Part 1 was already stored server-side and must not be re-sent; 2 and 3 are
	// the ones this call owed.
	if f.putAttempts[1] != 0 {
		t.Error("the already-confirmed part was re-uploaded")
	}
	if !bytes.Equal(f.stored[2], data[100:200]) || !bytes.Equal(f.stored[3], data[200:250]) {
		t.Fatal("the missing parts did not land")
	}
}

// The chimera refusal: same name/size/mtime, different middle bytes.
func TestUploadVerifyPartsRefused(t *testing.T) {
	data := makeData(250)
	f := newFake(t, 250, 100)
	f.matched = true
	f.confirmed[1], f.confirmed[2] = true, true
	f.ledgerMD5[1] = md5Hex([]byte("a different file entirely"))
	f.ledgerMD5[2] = md5Hex([]byte("also different"))
	f.verify = []int{1, 2}

	_, err := f.client().Upload(context.Background(), req(data))
	if err == nil {
		t.Fatal("expected the resume to be refused")
	}
	if !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("error = %v, want ErrVerifyFailed", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Code != "invalid" {
		t.Fatalf("underlying APIError = %+v, want 409/invalid", apiErr)
	}
	if f.count("put") != 0 {
		t.Fatal("a refused verification must not upload anything")
	}
}

// --------------------------------------------------------------- in_progress --

func TestCompleteInProgressPolls(t *testing.T) {
	data := makeData(100)
	f := newFake(t, 100, 100)
	f.completeInProgress = 2
	f.statusScript = []string{StatusCompleting, StatusDone}
	f.finalName = "report (1).pdf" // the finalizer auto-renamed it

	res, err := f.client().Upload(context.Background(), req(data))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if f.count("complete") != 3 {
		t.Fatalf("complete calls = %d, want 3 (two in_progress, then the real one)", f.count("complete"))
	}
	if f.count("status") < 2 {
		t.Fatalf("status polls = %d, want at least 2", f.count("status"))
	}
	if f.count("put") != 1 {
		t.Fatalf("PUTs = %d: an in_progress complete must poll, never re-send parts", f.count("put"))
	}
	if res.Name != "report (1).pdf" {
		t.Fatalf("Result.Name = %q, want the server's final name", res.Name)
	}
}

// ------------------------------------------------------------- pool refill ---

func TestURLPoolRefillMidUpload(t *testing.T) {
	data := makeData(1200) // 12 parts against a create that hands out 8 URLs
	f := newFake(t, 1200, 100)
	f.presignBatch = 8

	res, err := f.client(WithPartConcurrency(1)).Upload(context.Background(), req(data))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.PartsUploaded != 12 {
		t.Fatalf("PartsUploaded = %d, want 12", res.PartsUploaded)
	}
	if !bytes.Equal(f.assembled(), data) {
		t.Fatal("not every part landed")
	}
	// The refill is proactive -- nothing failed -- and one handshake covers
	// every remaining part, so exactly one is needed.
	if got := f.count("resume"); got != 1 {
		t.Fatalf("handshakes = %d, want exactly 1 proactive refill", got)
	}
	if f.count("put") != 12 {
		t.Fatalf("PUTs = %d, want 12 (no part retried)", f.count("put"))
	}
}

// ---------------------------------------------------------------- resume ----

func TestResumeSkipsConfirmedParts(t *testing.T) {
	data := makeData(500)
	f := newFake(t, 500, 100)
	f.confirmed[1], f.confirmed[2], f.confirmed[4] = true, true, true

	res, err := f.client(WithPartConcurrency(2)).Resume(context.Background(), f.uploadID, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.PartsUploaded != 2 {
		t.Fatalf("PartsUploaded = %d, want 2 (parts 3 and 5)", res.PartsUploaded)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range []int{1, 2, 4} {
		if f.putAttempts[n] != 0 {
			t.Errorf("part %d was re-uploaded despite being confirmed", n)
		}
	}
	for _, n := range []int{3, 5} {
		if f.putAttempts[n] != 1 {
			t.Errorf("part %d PUT attempts = %d, want 1", n, f.putAttempts[n])
		}
	}
	if !res.Resumed {
		t.Error("Resume must report Resumed")
	}
}

func TestResumeRejectsASourceOfTheWrongSize(t *testing.T) {
	f := newFake(t, 500, 100)
	_, err := f.client().Resume(context.Background(), f.uploadID, bytes.NewReader(makeData(499)))
	if err == nil || !strings.Contains(err.Error(), "500-byte file") {
		t.Fatalf("error = %v, want a size mismatch", err)
	}
	if f.count("resume") != 0 {
		t.Fatal("a size mismatch must be caught before the handshake")
	}
}

// A session already published: Resume settles it rather than failing.
func TestResumeOnDoneSession(t *testing.T) {
	data := makeData(100)
	f := newFake(t, 100, 100)
	f.status = StatusDone
	f.confirmed[1] = true
	f.finalName = "report (2).pdf"

	res, err := f.client().Resume(context.Background(), f.uploadID, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if f.count("put") != 0 {
		t.Fatal("a done session must not re-upload")
	}
	if res.Name != "report (2).pdf" || res.NodeID != "node-9" {
		t.Fatalf("Result = %+v, want the published node", res)
	}
}

// --------------------------------------------------- complete verify retry ---

// Complete answering 422 means the finalizer's ledger verify failed and the
// server has already deleted the offending rows. One handshake re-requests
// exactly those parts; a second complete then publishes.
func TestCompleteVerifyMismatchRecoversOnce(t *testing.T) {
	data := makeData(250)
	f := newFake(t, 250, 100)
	f.completeDropsPart = 2

	res, err := f.client(WithPartConcurrency(1)).Upload(context.Background(), req(data))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if f.count("complete") != 2 {
		t.Fatalf("complete calls = %d, want 2", f.count("complete"))
	}
	if f.putAttempts[2] != 2 {
		t.Fatalf("part 2 PUT attempts = %d, want 2 (the dropped part is re-sent)", f.putAttempts[2])
	}
	if f.putAttempts[1] != 1 || f.putAttempts[3] != 1 {
		t.Fatalf("only the dropped part should be re-sent, attempts = %v", f.putAttempts)
	}
	if !bytes.Equal(f.assembled(), data) {
		t.Fatal("the re-sent part did not land")
	}
	if res.NodeID != "node-9" {
		t.Fatalf("Result = %+v", res)
	}
}

// ------------------------------------------------------------ typed errors ---

func TestUploadNameConflict(t *testing.T) {
	data := makeData(100)
	f := newFake(t, 100, 100)
	f.nameConflict = true

	_, err := f.client().Upload(context.Background(), req(data))
	if !errors.Is(err, ErrNameConflict) {
		t.Fatalf("error = %v, want ErrNameConflict", err)
	}

	// The same create with a policy goes through.
	r := req(data)
	r.ConflictPolicy = "rename"
	if _, err := f.client().Upload(context.Background(), r); err != nil {
		t.Fatalf("Upload with conflict_policy: %v", err)
	}
	if f.createReq.ConflictPolicy != "rename" {
		t.Fatalf("conflict_policy = %q", f.createReq.ConflictPolicy)
	}
}

func TestSessionExpiredIsTyped(t *testing.T) {
	f := newFake(t, 100, 100)
	f.sessionGone = true

	if _, err := f.client().Status(context.Background(), f.uploadID); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("Status error = %v, want ErrSessionExpired", err)
	}
	if _, err := f.client().Resume(context.Background(), f.uploadID, bytes.NewReader(makeData(100))); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("Resume error = %v, want ErrSessionExpired", err)
	}
}

func TestAPIErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    *APIError
		is     error
		isNot  []error
		status int
	}{
		{name: "410", err: &APIError{Status: 410, Code: "session_expired"}, is: ErrSessionExpired,
			isNot: []error{ErrInProgress, ErrVerifyFailed, ErrInvalid}},
		{name: "409 name_conflict", err: &APIError{Status: 409, Code: "name_conflict"}, is: ErrNameConflict,
			isNot: []error{ErrVerifyFailed, ErrInProgress}},
		{name: "409 in_progress", err: &APIError{Status: 409, Code: "in_progress"}, is: ErrInProgress,
			isNot: []error{ErrVerifyFailed, ErrInvalid}},
		{name: "409 invalid", err: &APIError{Status: 409, Code: "invalid"}, is: ErrVerifyFailed,
			isNot: []error{ErrInProgress, ErrInvalid}},
		{name: "422 invalid", err: &APIError{Status: 422, Code: "invalid"}, is: ErrInvalid,
			isNot: []error{ErrVerifyFailed, ErrInProgress}},
		{name: "404", err: &APIError{Status: 404, Code: "not_found"}, is: ErrNotFound,
			isNot: []error{ErrInvalid, ErrSessionExpired}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(error(tc.err), tc.is) {
				t.Fatalf("%+v is not %v", tc.err, tc.is)
			}
			for _, n := range tc.isNot {
				if errors.Is(error(tc.err), n) {
					t.Errorf("%+v must not match %v", tc.err, n)
				}
			}
		})
	}
}

// ------------------------------------------------------------------- auth ----

func TestCookieAuthSendsClientHeader(t *testing.T) {
	f := newFake(t, 100, 100)
	if _, err := f.client().Status(context.Background(), f.uploadID); err != nil {
		t.Fatalf("Status: %v", err)
	}
	h := f.lastHeaders
	if got := h.Get(ClientHeader); got != "web" {
		t.Fatalf("%s = %q, want web", ClientHeader, got)
	}
	if got := h.Get("Cookie"); !strings.Contains(got, CookieName+"=session-token") {
		t.Fatalf("Cookie = %q", got)
	}
	if got := h.Get("Authorization"); got != "" {
		t.Fatalf("cookie auth must send no Authorization header, got %q", got)
	}
}

// Bearer requests are CSRF-exempt and must NOT carry X-Drive-Client -- Phase 6
// asserts its absence.
func TestBearerAuthOmitsClientHeader(t *testing.T) {
	f := newFake(t, 100, 100)
	c := New(f.srv.URL, WithBearerToken("drv_abc123"))
	if _, err := c.Status(context.Background(), f.uploadID); err != nil {
		t.Fatalf("Status: %v", err)
	}
	h := f.lastHeaders
	if got := h.Get("Authorization"); got != "Bearer drv_abc123" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := h.Get(ClientHeader); got != "" {
		t.Fatalf("bearer auth must omit %s, got %q", ClientHeader, got)
	}
	if got := h.Get("Cookie"); got != "" {
		t.Fatalf("bearer auth must send no cookie, got %q", got)
	}
}

// A whole upload under bearer auth: this is the drive-mcp path, and it must
// never trip the CSRF middleware by adding the header back mid-flight.
func TestUploadUnderBearerAuth(t *testing.T) {
	data := makeData(1200)
	f := newFake(t, 1200, 100)
	f.presignBatch = 8

	seen := map[string]bool{}
	f.putHook = func(w http.ResponseWriter, r *http.Request, n, attempt, gen int, body []byte) bool {
		return false
	}
	c := New(f.srv.URL, WithBearerToken("drv_write"), WithPartConcurrency(2),
		WithBackoff(time.Millisecond, 2*time.Millisecond), WithPollInterval(time.Millisecond))
	if _, err := c.Upload(context.Background(), req(data)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	seen[f.lastHeaders.Get(ClientHeader)] = true
	if seen[""] != true || len(seen) != 1 {
		t.Fatalf("a bearer upload leaked %s", ClientHeader)
	}
	if !bytes.Equal(f.assembled(), data) {
		t.Fatal("bearer upload lost bytes")
	}
}

// ------------------------------------------------------------- os.File src ---

// The battery hands this package a multi-GB sparse *os.File and expects it to
// upload without buffering. A raw *os.File must therefore work as a Source with
// no wrapper, and its mtime must reach the fingerprint in milliseconds.
func TestUploadFromRawOSFile(t *testing.T) {
	data := makeData(250)
	path := filepath.Join(t.TempDir(), goldenName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	mod := time.UnixMilli(goldenModMs)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()

	f := newFake(t, 250, 100)
	res, err := f.client(WithPartConcurrency(2)).Upload(context.Background(), UploadRequest{
		Source:   fh, // no wrapper, no size argument
		Name:     goldenName,
		ParentID: "parent-1",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !bytes.Equal(f.assembled(), data) {
		t.Fatal("the file's bytes did not arrive intact")
	}

	// ModTime came from the file itself, truncated to milliseconds.
	want, err := Fingerprint(bytes.NewReader(data), goldenName, 250, goldenModMs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fingerprint != want || f.createReq.Fingerprint != want {
		t.Fatalf("fingerprint = %s / %s, want %s", res.Fingerprint, f.createReq.Fingerprint, want)
	}
}

// sizelessReader has ReadAt but no Size: exactly the shape that cannot be
// uploaded, because the create has to declare a byte count.
type sizelessReader struct{}

func (sizelessReader) ReadAt([]byte, int64) (int, error) { return 0, io.EOF }

func TestSourceRejectsAReaderWithNoSize(t *testing.T) {
	_, err := asSource(sizelessReader{})
	if err == nil || !strings.Contains(err.Error(), "does not report its size") {
		t.Fatalf("error = %v, want a size-reporting complaint", err)
	}
	if _, err := asSource(nil); err == nil {
		t.Fatal("a nil source must be rejected")
	}
}

// ----------------------------------------------------------------- plumbing --

func TestNormalizeBase(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://localhost:8080", "http://localhost:8080/api"},
		{"http://localhost:8080/", "http://localhost:8080/api"},
		{"http://localhost:8080/api", "http://localhost:8080/api"},
		{"http://localhost:8080/api/", "http://localhost:8080/api"},
	} {
		if got := normalizeBase(tc.in); got != tc.want {
			t.Errorf("normalizeBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBackoffIsCappedAndJittered(t *testing.T) {
	c := New("http://x", WithBackoff(time.Second, time.Minute))
	c.jitter = func() float64 { return 1 }
	if got := c.backoffFor(1); got != time.Second {
		t.Fatalf("attempt 1 = %v, want 1s", got)
	}
	if got := c.backoffFor(3); got != 4*time.Second {
		t.Fatalf("attempt 3 = %v, want 4s", got)
	}
	for _, n := range []int{7, 20, 200} {
		if got := c.backoffFor(n); got > time.Minute {
			t.Fatalf("attempt %d = %v, over the 60s cap", n, got)
		}
	}
	c.jitter = func() float64 { return 0 }
	if got := c.backoffFor(3); got != 2*time.Second {
		t.Fatalf("jitter floor at attempt 3 = %v, want exp/2 = 2s", got)
	}
}

func TestStatusCancelAndList(t *testing.T) {
	f := newFake(t, 250, 100)
	c := f.client()
	ctx := context.Background()

	st, err := c.Status(ctx, f.uploadID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.PartsTotal != 3 || st.PartSize != 100 || st.Status != StatusActive {
		t.Fatalf("Status = %+v", st)
	}
	if st.ConfirmedParts == nil {
		t.Error("confirmed_parts should decode as a slice, not nil")
	}

	page, err := c.List(ctx, "", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].UploadID != f.uploadID {
		t.Fatalf("List = %+v", page)
	}

	if err := c.Cancel(ctx, f.uploadID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if f.count("cancel") != 1 {
		t.Fatal("Cancel did not reach the server")
	}
}

// A context cancelled mid-upload stops the pump instead of burning budgets.
func TestUploadRespectsContextCancellation(t *testing.T) {
	data := makeData(1000)
	f := newFake(t, 1000, 100)
	ctx, cancel := context.WithCancel(context.Background())
	f.putHook = func(w http.ResponseWriter, r *http.Request, n, attempt, gen int, body []byte) bool {
		cancel()
		return false
	}

	_, err := f.client(WithPartConcurrency(1)).Upload(ctx, req(data))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if f.count("complete") != 0 {
		t.Fatal("a cancelled upload must not complete")
	}
}
