package uploadclient

// Upload: the full protocol from a local file to a published node, and the part
// pump that does the work.
//
// The retry rules are the browser engine's, deliberately: the two clients hit
// the same server and must not disagree about what a failure means.
//
//   - An expired presigned URL -- 403, or the 400 carrying
//     <Code>InvalidRequest</Code> that Garage actually returns (spike-measured
//     2026-08-14) -- is not the part's fault. It buys a fresh handshake and no
//     integrity charge, but at most three in a row: past that the clock is
//     wrong, not the URL, and looping forever would hide that.
//   - Anything about the bytes -- an ETag that is not the MD5, a rejected
//     confirmation, a hard HTTP error, a dead socket -- charges the 8-attempt
//     integrity budget with exponential backoff and jitter, capped at 60 s.
//   - A part's re-handshake count resets whenever that part fails for some
//     other reason, which is what makes the cap "consecutive".

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// UploadRequest describes one file to upload.
type UploadRequest struct {
	// Source is the bytes, and must stay readable for the whole call. An
	// *os.File, a *bytes.Reader or an *io.SectionReader all work as they are;
	// anything else must report its own length (see Source).
	Source io.ReaderAt

	// Name is the file name as it should land in Drive. It also feeds the
	// fingerprint, so it must be the same string the browser would send.
	Name string

	// ParentID is the destination folder's id. Required.
	ParentID string

	// Mime is the client-declared content type. Optional; never trusted for
	// serving.
	Mime string

	// ModTime is the file's last-modified time, truncated to milliseconds for
	// the fingerprint. When zero, a Source that knows its own mtime (FileSource)
	// supplies it; otherwise the fingerprint uses 0.
	ModTime time.Time

	// ConflictPolicy is "replace" or "rename", or empty to be told about a
	// collision with ErrNameConflict. drive-mcp always sends "rename".
	ConflictPolicy string

	// Fingerprint overrides the computed one. Leave empty in normal use; it
	// exists so a caller that already hashed the file (the browser handing off,
	// a test pinning a value) need not read it twice.
	Fingerprint string
}

// Result is where an upload ended up.
type Result struct {
	UploadID string

	// NodeID and Name are the published file. Name is the server's final
	// choice, which may be an auto-rename such as "report (1).pdf".
	NodeID string
	Name   string

	// ParentID is set only when the file did not land where it was aimed: the
	// destination was trashed mid-upload and it was re-parented to the root.
	ParentID string

	Fingerprint string
	SHA256      string
	PartSize    int64
	PartsTotal  int

	// PartsUploaded counts the parts this call actually transferred; on a
	// resume that finished someone else's work it can be less than PartsTotal.
	PartsUploaded int

	// Resumed reports that the create matched an existing session instead of
	// opening a new one.
	Resumed bool
}

// errFinalizing is internal: a part confirmation came back 409 in_progress, so
// something else is already finalizing this session. The pump stops and the
// complete path takes over by polling.
var errFinalizing = errors.New("uploadclient: session is being finalized")

// invalidRequestBody matches Garage's expired-presign error. A plain 400 without
// this code is a hard failure, not an expiry.
var invalidRequestBody = regexp.MustCompile(`(?i)<Code>\s*InvalidRequest\s*</Code>`)

// Upload runs the whole protocol: fingerprint, create, transfer, complete.
//
// It is safe to call concurrently for different files. Calling it twice for the
// same (file, parent) is not an error -- the second call matches the first's
// session and resumes it -- but the two will fight over the same parts.
func (c *Client) Upload(ctx context.Context, req UploadRequest) (*Result, error) {
	src, err := asSource(req.Source)
	if err != nil {
		return nil, err
	}
	if req.Name == "" {
		return nil, fmt.Errorf("uploadclient: Upload needs a Name")
	}
	if req.ParentID == "" {
		return nil, fmt.Errorf("uploadclient: Upload needs a ParentID")
	}
	size := src.Size()

	fingerprint := req.Fingerprint
	if fingerprint == "" {
		fingerprint, err = Fingerprint(src, req.Name, size, modMillis(req, src))
		if err != nil {
			return nil, err
		}
	}

	// The whole-file digest runs alongside the transfer rather than after it:
	// on a multi-GB file a second sequential pass would double the wall clock.
	hashed := startHashing(src, size)

	parent := req.ParentID
	var created createResp
	status, err2 := c.do(ctx, http.MethodPost, "/uploads", createReq{
		FileName:       req.Name,
		FileSize:       size,
		Mime:           req.Mime,
		ParentID:       &parent,
		Fingerprint:    fingerprint,
		ConflictPolicy: req.ConflictPolicy,
	}, &created)
	if err2 != nil {
		return nil, err2
	}

	t := c.newTransfer(created.Status, src)
	t.adopt(created.Status, created.Presigned, false)
	t.resumed = status == http.StatusOK

	c.log.Info("upload session created",
		"upload_id", t.id, "matched", t.resumed, "file_size", size,
		"part_size", t.partSize, "parts_total", t.partsTotal,
		"presigned", len(created.Presigned), "verify_parts", created.VerifyParts)

	// A matched session with confirmed parts arms the chimera guard and hands
	// back no URLs at all. Clear it by proving, with MD5s of exactly the pinned
	// parts, that this is the same file.
	if len(created.VerifyParts) > 0 {
		md5s, err := t.md5sFor(created.VerifyParts)
		if err != nil {
			return nil, err
		}
		if _, err := t.handshake(ctx, md5s); err != nil {
			return nil, err
		}
	}

	return t.drive(ctx, hashed)
}

// modMillis is the fingerprint's lastModified field: explicit request value
// first, then a source that knows its own mtime, then zero.
func modMillis(req UploadRequest, src Source) int64 {
	if !req.ModTime.IsZero() {
		return req.ModTime.UnixMilli()
	}
	if m, ok := src.(modTimer); ok {
		if t := m.ModTime(); !t.IsZero() {
			return t.UnixMilli()
		}
	}
	return 0
}

// startHashing computes the whole-file SHA-256 on its own goroutine. The source
// is read through ReadAt, so this never contends with the part uploads for a
// file offset.
func startHashing(src Source, size int64) <-chan hashResult {
	ch := make(chan hashResult, 1)
	go func() {
		sum, err := wholeSHA256(src, size)
		ch <- hashResult{sum: sum, err: err}
	}()
	return ch
}

type hashResult struct {
	sum string
	err error
}

// ------------------------------------------------------------------ transfer --

// transfer is one session's in-flight state: which parts are still owed, which
// URLs are in hand, and what the server last said.
type transfer struct {
	c   *Client
	src Source

	id         string
	partSize   int64
	fileSize   int64
	partsTotal int
	resumed    bool

	mu        sync.Mutex
	pool      map[int]string // part number -> presigned URL
	pending   map[int]bool   // parts not yet confirmed
	status    Status
	refillGen uint64

	// refillMu serializes handshakes so a burst of workers running out of URLs
	// at once produces one round trip, not one each.
	refillMu sync.Mutex

	uploaded atomic.Int64
}

func (c *Client) newTransfer(st Status, src Source) *transfer {
	return &transfer{
		c:          c,
		src:        src,
		id:         st.UploadID,
		partSize:   st.PartSize,
		fileSize:   st.FileSize,
		partsTotal: st.PartsTotal,
		pool:       map[int]string{},
		pending:    map[int]bool{},
		status:     st,
	}
}

// adopt folds a server response into the local view: which parts are confirmed
// (so pending is always "everything not confirmed"), and which URLs are usable.
// replacePool is true for a resume handshake, whose `missing` list is complete
// and supersedes whatever was held; a create's short batch merges instead.
func (t *transfer) adopt(st Status, urls []PresignedPart, replacePool bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.status = st
	if st.PartSize > 0 {
		t.partSize = st.PartSize
	}
	t.partsTotal = st.PartsTotal

	confirmed := make(map[int]bool, len(st.ConfirmedParts))
	for _, n := range st.ConfirmedParts {
		confirmed[n] = true
	}
	t.pending = make(map[int]bool, t.partsTotal)
	for n := 1; n <= t.partsTotal; n++ {
		if !confirmed[n] {
			t.pending[n] = true
		}
	}

	if replacePool {
		t.pool = make(map[int]string, len(urls))
	}
	for _, p := range urls {
		t.pool[p.PartNumber] = p.URL
	}
	for n := range t.pool {
		if !t.pending[n] {
			delete(t.pool, n)
		}
	}
}

func (t *transfer) missingParts() []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]int, 0, len(t.pending))
	for n := 1; n <= t.partsTotal; n++ {
		if t.pending[n] {
			out = append(out, n)
		}
	}
	return out
}

func (t *transfer) isPending(n int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pending[n]
}

func (t *transfer) urlFor(n int) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	u, ok := t.pool[n]
	return u, ok
}

func (t *transfer) dropURL(n int) {
	t.mu.Lock()
	delete(t.pool, n)
	t.mu.Unlock()
}

func (t *transfer) markConfirmed(n int) {
	t.mu.Lock()
	delete(t.pending, n)
	delete(t.pool, n)
	t.mu.Unlock()
	t.uploaded.Add(1)
}

// poolLow reports that a proactive refill is worth a round trip: the pool is
// nearly empty AND some part still owed has no URL. Without the second half,
// the last two parts of every upload would each trigger a handshake.
func (t *transfer) poolLow() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pool) > LowPool {
		return false
	}
	for n := range t.pending {
		if _, ok := t.pool[n]; !ok {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------- the pump ----

// run uploads every part still owed, at most concurrency at a time. The first
// worker error cancels the rest.
func (t *transfer) run(ctx context.Context) error {
	missing := t.missingParts()
	if len(missing) == 0 {
		return nil
	}

	queue := make(chan int, len(missing))
	for _, n := range missing {
		queue <- n
	}
	close(queue)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := t.c.concurrency
	if workers > len(missing) {
		workers = len(missing)
	}
	if workers < 1 {
		workers = 1
	}

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range queue {
				if ctx.Err() != nil {
					return
				}
				if err := t.uploadPart(ctx, n); err != nil {
					once.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
			}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	return nil
}

// uploadPart drives one part to confirmation, or to the end of its budgets.
func (t *transfer) uploadPart(ctx context.Context, n int) error {
	var (
		integrity    int
		rehandshakes int
		lastReason   error
	)
	partSize, fileSize, _ := t.geometry()
	off, length := partRange(n, partSize, fileSize)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Another actor (a refill's status, a second tab) may have confirmed
		// this part while we were backing off.
		if !t.isPending(n) {
			return nil
		}

		url, ok := t.urlFor(n)
		if !ok {
			if err := t.refill(ctx); err != nil {
				return err
			}
			if !t.isPending(n) {
				return nil
			}
			if url, ok = t.urlFor(n); !ok {
				return fmt.Errorf("uploadclient: upload %s: no presigned URL for part %d after a handshake", t.id, n)
			}
		} else if t.poolLow() {
			// Proactive refill: no error required, just a pool running dry
			// (PLAN §Upload protocol, "URL pool refill").
			if err := t.refill(ctx); err != nil {
				return err
			}
			if !t.isPending(n) {
				return nil
			}
			if fresh, ok := t.urlFor(n); ok {
				url = fresh
			}
		}

		md5hex, err := partMD5(t.src, off, length)
		if err != nil {
			return fmt.Errorf("uploadclient: reading part %d of %s: %w", n, t.id, err)
		}

		out := t.put(ctx, url, off, length)

		switch {
		case out.err != nil:
			// No response reached us: a dropped socket, a refused connection, a
			// cancelled context. Bounded by the integrity budget so the call
			// always terminates.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			rehandshakes = 0
			lastReason = out.err
			if err := t.chargeIntegrity(ctx, n, &integrity, lastReason); err != nil {
				return err
			}

		case out.status >= 200 && out.status < 300:
			rehandshakes = 0
			etag := NormalizeETag(out.etag)
			if etag == "" || etag != md5hex {
				lastReason = fmt.Errorf("part %d: ETag %q does not match MD5 %s", n, out.etag, md5hex)
				if err := t.chargeIntegrity(ctx, n, &integrity, lastReason); err != nil {
					return err
				}
				continue
			}
			done, err := t.confirm(ctx, n, etag, md5hex, length)
			if err != nil {
				if errors.Is(err, ErrInvalid) {
					lastReason = err
					if err := t.chargeIntegrity(ctx, n, &integrity, lastReason); err != nil {
						return err
					}
					continue
				}
				return err
			}
			if done {
				return nil
			}

		case isExpiredURL(out.status, out.body):
			// Not the part's fault: fresh URLs, no integrity charge, at most
			// three in a row.
			if err := t.chargeRehandshake(ctx, n, &rehandshakes); err != nil {
				return err
			}

		default:
			rehandshakes = 0
			lastReason = fmt.Errorf("part %d: PUT returned %d: %s", n, out.status, truncate(out.body, 300))
			if err := t.chargeIntegrity(ctx, n, &integrity, lastReason); err != nil {
				return err
			}
		}
	}
}

// isExpiredURL is the expired-presign signal. Garage answers an expired URL
// with 400 and <Code>InvalidRequest</Code>, not the 403 the original plan
// assumed; both count, and a bare 400 does not.
func isExpiredURL(status int, body string) bool {
	if status == http.StatusForbidden {
		return true
	}
	return status == http.StatusBadRequest && invalidRequestBody.MatchString(body)
}

type putOutcome struct {
	status int
	etag   string
	body   string
	err    error
}

// put streams one part to its presigned URL.
//
// The body is a SectionReader over the source, and ContentLength is set by
// hand: an io.Reader that Go cannot size is sent chunked, which a presigned PUT
// rejects. No Content-Type is sent -- objects must be stored without one, or
// Range responses would serve user HTML as renderable content -- and no auth
// header either, because the signed URL is the credential.
func (t *transfer) put(ctx context.Context, url string, off, length int64) putOutcome {
	body := io.NewSectionReader(t.src, off, length)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return putOutcome{err: err}
	}
	req.ContentLength = length
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(io.NewSectionReader(t.src, off, length)), nil
	}
	// Wait for the status line before writing the part. An expired presign is
	// rejected before Garage reads the body, and it closes the socket behind the
	// rejection: without this the transport is already streaming 100 MiB into a
	// dead connection and surfaces "broken pipe" instead of the 400 that means
	// "expired". That misreading costs the part its integrity budget where PLAN
	// promises a free re-handshake. Measured against Garage v2.3.0: the continue
	// comes back immediately, so the happy path pays nothing for it.
	req.Header.Set("Expect", "100-continue")

	res, err := t.c.http.Do(req)
	if err != nil {
		return putOutcome{err: err}
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxErrBody))
	return putOutcome{status: res.StatusCode, etag: res.Header.Get("ETag"), body: string(raw)}
}

// confirm records a part in the ledger. It returns false when the caller should
// retry the part, and an error only when the whole upload is over.
func (t *transfer) confirm(ctx context.Context, n int, etag, md5hex string, size int64) (bool, error) {
	var out confirmResp
	_, err := t.c.do(ctx, http.MethodPost,
		"/uploads/"+t.id+"/parts/"+strconv.Itoa(n),
		confirmReq{ETag: etag, MD5: md5hex, Size: size}, &out)
	if err != nil {
		switch {
		case errors.Is(err, ErrInProgress):
			// A finalizer already holds the session. Stop pumping and let the
			// complete path poll; re-driving parts now would fight it.
			return false, errFinalizing
		case errors.Is(err, ErrInvalid):
			return false, err
		default:
			return false, err
		}
	}
	t.markConfirmed(n)
	t.c.log.Debug("upload part confirmed", "upload_id", t.id, "part_number", n, "size", size)
	return true, nil
}

// chargeIntegrity spends one of the part's eight attempts, then backs off and
// re-handshakes before the next try.
func (t *transfer) chargeIntegrity(ctx context.Context, n int, used *int, reason error) error {
	*used++
	if *used >= IntegrityBudget {
		return fmt.Errorf("uploadclient: upload %s: part %d failed %d times: %w", t.id, n, *used, reason)
	}
	t.c.log.Warn("upload part retry", "upload_id", t.id, "part_number", n,
		"attempt", *used, "budget", IntegrityBudget, "reason", reason.Error())
	t.dropURL(n)
	if err := t.c.sleep(ctx, t.c.backoffFor(*used)); err != nil {
		return err
	}
	return t.refill(ctx)
}

// chargeRehandshake spends one of the part's three consecutive expired-URL
// retries. Running out means the URLs are being signed against a clock this
// host disagrees with -- looping instead of saying so is how that bug hides.
func (t *transfer) chargeRehandshake(ctx context.Context, n int, used *int) error {
	*used++
	if *used > MaxRehandshakes {
		return fmt.Errorf(
			"uploadclient: upload %s: part %d still expired after %d re-handshakes; "+
				"check clock skew between this host and the storage node", t.id, n, MaxRehandshakes)
	}
	t.c.log.Warn("upload part URL expired", "upload_id", t.id, "part_number", n,
		"rehandshake", *used, "max", MaxRehandshakes)
	t.dropURL(n)
	if err := t.c.sleep(ctx, t.c.backoffFor(*used)); err != nil {
		return err
	}
	return t.refill(ctx)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
