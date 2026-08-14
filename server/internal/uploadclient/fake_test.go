package uploadclient

// A fake Drive server that speaks PLAN's frozen Appendix, and nothing else.
//
// The point of the contract being frozen is that this package can be tested
// without a server, a database, or Garage: every behaviour the client has to
// get right -- the verification bounce, an expired presign, a finalizer already
// running -- is a response shape, and shapes are cheap to produce.
//
// The presigned "URLs" point back at this same test server. Each handshake
// bumps a generation counter that is embedded in the URLs it issues, which is
// what lets a test say "anything signed before the last handshake is expired"
// without any clocks.

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// expiredBody is what Garage actually answers for an expired presigned URL,
// verbatim from the day-0 spike report (400, not 403).
const expiredBody = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidRequest</Code>` +
	`<Message>Bad request: Date is too old</Message><Resource>/drive-blobs/blobs/x</Resource></Error>`

type fake struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex

	uploadID    string
	fileName    string
	fileSize    int64
	partSize    int64
	fingerprint string
	parentID    string
	nodeID      string
	finalName   string

	status    string
	confirmed map[int]bool
	ledgerMD5 map[int]string
	stored    map[int][]byte
	verify    []int
	urlGen    int

	// Knobs, set before the client runs.
	matched            bool // create answers 200 with the existing session
	nameConflict       bool // create answers 409 name_conflict
	sessionGone        bool // every session route answers 410
	presignBatch       int  // URLs a create hands out (the server's ~8)
	completeInProgress int  // completes answered 409 in_progress before the real one
	completeDropsPart  int  // >0: the first complete 422s and deletes this ledger row
	statusScript       []string
	putHook            func(w http.ResponseWriter, r *http.Request, n, attempt, gen int, body []byte) bool

	// Observations.
	calls          map[string]int
	putAttempts    map[int]int
	lastHeaders    http.Header
	lastResumeMD5s map[string]string
	completeSHA    string
	createReq      createReq
}

func newFake(t *testing.T, fileSize, partSize int64) *fake {
	t.Helper()
	f := &fake{
		t:            t,
		uploadID:     "11111111-2222-3333-4444-555555555555",
		fileName:     "report.pdf",
		fileSize:     fileSize,
		partSize:     partSize,
		fingerprint:  "fp-under-test",
		parentID:     "parent-1",
		nodeID:       "node-9",
		finalName:    "report.pdf",
		status:       StatusActive,
		confirmed:    map[int]bool{},
		ledgerMD5:    map[int]string{},
		stored:       map[int][]byte{},
		presignBatch: 8,
		calls:        map[string]int{},
		putAttempts:  map[int]int{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/uploads", f.create)
	mux.HandleFunc("GET /api/uploads", f.list)
	mux.HandleFunc("GET /api/uploads/{id}", f.getStatus)
	mux.HandleFunc("POST /api/uploads/{id}/resume", f.resume)
	mux.HandleFunc("POST /api/uploads/{id}/parts/{n}", f.confirmPart)
	mux.HandleFunc("POST /api/uploads/{id}/complete", f.complete)
	mux.HandleFunc("DELETE /api/uploads/{id}", f.cancel)
	mux.HandleFunc("PUT /blob", f.blob)

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// client returns a Client pointed at the fake with test-speed timings.
func (f *fake) client(opts ...Option) *Client {
	base := []Option{
		WithCookieAuth("session-token"),
		WithBackoff(time.Millisecond, 2*time.Millisecond),
		WithPollInterval(time.Millisecond),
	}
	return New(f.srv.URL, append(base, opts...)...)
}

// ------------------------------------------------------------------ helpers --

func (f *fake) partsTotal() int {
	if f.partSize <= 0 {
		return 0
	}
	return int((f.fileSize + f.partSize - 1) / f.partSize)
}

func (f *fake) confirmedList() []int {
	out := []int{}
	for n := 1; n <= f.partsTotal(); n++ {
		if f.confirmed[n] {
			out = append(out, n)
		}
	}
	return out
}

func (f *fake) missing() []int {
	out := []int{}
	for n := 1; n <= f.partsTotal(); n++ {
		if !f.confirmed[n] {
			out = append(out, n)
		}
	}
	return out
}

func (f *fake) urlsFor(ns []int) []map[string]any {
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		out = append(out, map[string]any{
			"part_number": n,
			"url":         fmt.Sprintf("%s/blob?part=%d&gen=%d", f.srv.URL, n, f.urlGen),
			"expires_at":  time.Now().Add(time.Hour),
		})
	}
	return out
}

func (f *fake) statusBody() map[string]any {
	return map[string]any{
		"upload_id":          f.uploadID,
		"mode":               "direct",
		"file_name":          f.fileName,
		"file_size":          f.fileSize,
		"part_size":          f.partSize,
		"parts_total":        f.partsTotal(),
		"fingerprint":        f.fingerprint,
		"parent_id":          f.parentID,
		"status":             f.status,
		"confirmed_parts":    f.confirmedList(),
		"session_expires_at": time.Now().Add(7 * 24 * time.Hour),
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code int, apiCode, msg string) {
	writeJSON(w, code, map[string]any{"code": apiCode, "message": msg})
}

// record notes the call and the headers it arrived with.
func (f *fake) record(name string, r *http.Request) {
	f.calls[name]++
	f.lastHeaders = r.Header.Clone()
}

func (f *fake) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

// ----------------------------------------------------------------- handlers --

func (f *fake) create(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create", r)

	_ = json.NewDecoder(r.Body).Decode(&f.createReq)
	if f.createReq.Fingerprint != "" {
		f.fingerprint = f.createReq.Fingerprint
	}
	if f.createReq.FileName != "" {
		f.fileName = f.createReq.FileName
	}

	if f.nameConflict && f.createReq.ConflictPolicy == "" {
		writeErr(w, http.StatusConflict, "name_conflict", "a file with that name already exists here")
		return
	}

	body := f.statusBody()
	if f.matched {
		if len(f.verify) > 0 {
			// The guard: no URL exists until the client proves which file it holds.
			body["presigned"] = []map[string]any{}
			body["verify_parts"] = f.verify
			writeJSON(w, http.StatusOK, body)
			return
		}
		body["presigned"] = f.urlsFor(f.batch(f.missing()))
		writeJSON(w, http.StatusOK, body)
		return
	}
	body["presigned"] = f.urlsFor(f.batch(f.missing()))
	writeJSON(w, http.StatusCreated, body)
}

func (f *fake) batch(ns []int) []int {
	if f.presignBatch > 0 && len(ns) > f.presignBatch {
		return ns[:f.presignBatch]
	}
	return ns
}

func (f *fake) getStatus(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("status", r)
	if f.sessionGone {
		writeErr(w, http.StatusGone, "session_expired", "this upload session has expired")
		return
	}
	// statusScript walks the session through completing -> done. The last entry
	// is sticky: a session that reached 'done' stays there.
	if len(f.statusScript) > 0 {
		f.status = f.statusScript[0]
		if len(f.statusScript) > 1 {
			f.statusScript = f.statusScript[1:]
		}
	}
	writeJSON(w, http.StatusOK, f.statusBody())
}

func (f *fake) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("list", r)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       []map[string]any{f.statusBody()},
		"next_cursor": nil,
	})
}

func (f *fake) resume(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("resume", r)
	if f.sessionGone {
		writeErr(w, http.StatusGone, "session_expired", "this upload session has expired")
		return
	}

	var req resumeReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.lastResumeMD5s = req.PartMD5s

	if f.status != StatusActive {
		// Nothing to reconcile: no `missing` field at all.
		writeJSON(w, http.StatusOK, f.statusBody())
		return
	}

	if len(f.verify) > 0 {
		covered := true
		for _, n := range f.verify {
			if _, ok := req.PartMD5s[strconv.Itoa(n)]; !ok {
				covered = false
				break
			}
		}
		if !covered {
			body := f.statusBody()
			body["verify_parts"] = f.verify
			writeJSON(w, http.StatusOK, body)
			return
		}
		for _, n := range f.verify {
			if req.PartMD5s[strconv.Itoa(n)] != f.ledgerMD5[n] {
				writeErr(w, http.StatusConflict, "invalid", "part verification failed")
				return
			}
		}
		f.verify = nil
	}

	f.urlGen++
	body := f.statusBody()
	body["missing"] = f.urlsFor(f.missing())
	writeJSON(w, http.StatusOK, body)
}

func (f *fake) confirmPart(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("confirm", r)
	if f.sessionGone {
		writeErr(w, http.StatusGone, "session_expired", "this upload session has expired")
		return
	}

	n, _ := strconv.Atoi(r.PathValue("n"))
	var req confirmReq
	_ = json.NewDecoder(r.Body).Decode(&req)

	// The same size rule the real server enforces: only the final part may be
	// short, and nothing may exceed what is left.
	want := f.partSize
	if int64(n)*f.partSize > f.fileSize {
		want = f.fileSize - int64(n-1)*f.partSize
	}
	if req.Size != want {
		writeErr(w, http.StatusUnprocessableEntity, "invalid",
			fmt.Sprintf("part %d has the wrong size for this session", n))
		return
	}
	if req.ETag != req.MD5 {
		writeErr(w, http.StatusUnprocessableEntity, "invalid", "etag does not match md5")
		return
	}

	f.confirmed[n] = true
	f.ledgerMD5[n] = req.MD5
	writeJSON(w, http.StatusOK, map[string]any{
		"confirmed":          true,
		"session_expires_at": time.Now().Add(7 * 24 * time.Hour),
	})
}

func (f *fake) complete(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("complete", r)
	if f.sessionGone {
		writeErr(w, http.StatusGone, "session_expired", "this upload session has expired")
		return
	}

	var req completeReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.completeSHA = req.SHA256

	// The finalize verify found the ledger and the store disagreeing. The real
	// server deletes the offending rows before rolling back, so the next
	// handshake re-requests exactly those parts.
	if f.completeDropsPart > 0 {
		delete(f.confirmed, f.completeDropsPart)
		delete(f.stored, f.completeDropsPart)
		f.completeDropsPart = 0
		writeErr(w, http.StatusUnprocessableEntity, "invalid",
			"the uploaded parts do not match the store; re-handshake and re-send the missing parts")
		return
	}

	if f.completeInProgress > 0 {
		f.completeInProgress--
		f.status = StatusCompleting
		writeErr(w, http.StatusConflict, "in_progress", "this upload is being finalized")
		return
	}
	f.status = StatusDone
	writeJSON(w, http.StatusOK, map[string]any{"node_id": f.nodeID, "name": f.finalName})
}

func (f *fake) cancel(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("cancel", r)
	f.status = StatusAborted
	w.WriteHeader(http.StatusNoContent)
}

// blob is the presigned part endpoint.
func (f *fake) blob(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("part"))
	gen, _ := strconv.Atoi(r.URL.Query().Get("gen"))
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.calls["put"]++
	f.putAttempts[n]++
	attempt := f.putAttempts[n]
	hook := f.putHook
	f.mu.Unlock()

	if hook != nil && hook(w, r, n, attempt, gen, body) {
		return
	}

	sum := md5.Sum(body)
	f.mu.Lock()
	f.stored[n] = body
	f.mu.Unlock()

	// S3 and Garage quote the ETag; the client must normalize before comparing.
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.WriteHeader(http.StatusOK)
}

// writeExpired answers with Garage's real expired-presign response.
func writeExpired(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, expiredBody)
}
