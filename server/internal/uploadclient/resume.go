package uploadclient

// Resume, the handshake, and the finalize path.
//
// The handshake is one endpoint doing three jobs, and the client has to keep
// them straight:
//
//   - it is the URL source. A create hands out a short batch; every URL after
//     that comes from here, including the proactive refill that fires with no
//     error in sight.
//   - it is the reconciliation point. The response's confirmed_parts is the
//     truth about what Garage holds, which is why every response is adopted
//     wholesale rather than merged optimistically.
//   - it is the chimera gate. A session that was re-selected, or whose ledger
//     drifted from Garage, answers with pinned part numbers and no URLs until
//     the client shows MD5s for exactly those parts. The bounce is a normal
//     200, not an error.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// maxVerifyRounds bounds the bounce/recompute/re-call cycle. One round is the
// normal case; reconciliation can re-arm the flag, and a loop that never gives
// up would spin against a server that keeps re-arming it.
const maxVerifyRounds = 3

// maxCompleteRounds bounds complete/poll alternation.
const maxCompleteRounds = 8

// Resume continues an upload that already has a session.
//
// src must be the same file the session was created for; the server verifies
// that itself when the chimera guard is armed, and this rejects an outright
// size mismatch up front. A session that is already done or mid-finalize is not
// an error -- Resume settles it and returns where the file landed.
func (c *Client) Resume(ctx context.Context, uploadID string, source io.ReaderAt) (*Result, error) {
	src, err := asSource(source)
	if err != nil {
		return nil, err
	}
	st, err := c.Status(ctx, uploadID)
	if err != nil {
		return nil, err
	}
	if st.Status == StatusAborted {
		return nil, abortedErr(uploadID)
	}
	if src.Size() != st.FileSize {
		return nil, fmt.Errorf(
			"uploadclient: upload %s is for a %d-byte file but the source holds %d bytes",
			uploadID, st.FileSize, src.Size())
	}

	t := c.newTransfer(*st, src)
	t.adopt(*st, nil, false)
	t.resumed = true

	hashed := startHashing(src, st.FileSize)

	if st.Status == StatusActive {
		if _, err := t.handshake(ctx, nil); err != nil {
			return nil, err
		}
	}
	c.log.Info("upload resumed", "upload_id", uploadID, "status", st.Status,
		"confirmed", len(st.ConfirmedParts), "parts_total", st.PartsTotal)

	return t.drive(ctx, hashed)
}

func abortedErr(uploadID string) error {
	return &APIError{
		Method:  http.MethodGet,
		Path:    "/uploads/" + uploadID,
		Status:  http.StatusGone,
		Code:    "session_expired",
		Message: "the upload session was aborted",
	}
}

// ------------------------------------------------------------------ geometry --

func (t *transfer) geometry() (partSize, fileSize int64, partsTotal int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.partSize, t.fileSize, t.partsTotal
}

func (t *transfer) snapshot() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (t *transfer) bumpRefillGen() {
	t.mu.Lock()
	t.refillGen++
	t.mu.Unlock()
}

// ----------------------------------------------------------------- handshake --

// handshake calls POST /uploads/{id}/resume and folds the answer in.
//
// md5s is nil for a plain refill. When the response is a verification bounce --
// pinned parts and no `missing` field at all -- the MD5s for exactly those parts
// are computed from the source and the call is repeated.
func (t *transfer) handshake(ctx context.Context, md5s map[string]string) (*resumeResp, error) {
	for round := 0; round < maxVerifyRounds; round++ {
		var out resumeResp
		_, err := t.c.do(ctx, http.MethodPost, "/uploads/"+t.id+"/resume", resumeReq{PartMD5s: md5s}, &out)
		if err != nil {
			return nil, err
		}

		// The bounce. `missing` absent is what distinguishes it from a normal
		// answer: an empty list would mean "nothing left to upload".
		if len(out.VerifyParts) > 0 && out.Missing == nil && out.Status.Status == StatusActive {
			t.adopt(out.Status, nil, false)
			t.c.log.Info("upload verification bounce",
				"upload_id", t.id, "verify_parts", out.VerifyParts, "round", round+1)
			next, err := t.md5sFor(out.VerifyParts)
			if err != nil {
				return nil, err
			}
			md5s = next
			continue
		}

		var urls []PresignedPart
		if out.Missing != nil {
			urls = *out.Missing
		}
		t.adopt(out.Status, urls, out.Missing != nil)
		t.bumpRefillGen()
		t.c.log.Debug("upload handshake",
			"upload_id", t.id, "status", out.Status.Status,
			"confirmed", len(out.Status.ConfirmedParts), "urls", len(urls))
		return &out, nil
	}
	return nil, fmt.Errorf("uploadclient: upload %s: part verification kept re-arming after %d rounds",
		t.id, maxVerifyRounds)
}

// md5sFor computes the MD5 of each named part straight from the source. This is
// the client's half of the chimera check.
func (t *transfer) md5sFor(parts []int) (map[string]string, error) {
	partSize, fileSize, partsTotal := t.geometry()
	out := make(map[string]string, len(parts))
	for _, n := range parts {
		if n < 1 || n > partsTotal {
			return nil, fmt.Errorf("uploadclient: upload %s: asked to verify part %d of %d",
				t.id, n, partsTotal)
		}
		off, length := partRange(n, partSize, fileSize)
		sum, err := partMD5(t.src, off, length)
		if err != nil {
			return nil, fmt.Errorf("uploadclient: reading part %d of %s for verification: %w", n, t.id, err)
		}
		out[strconv.Itoa(n)] = sum
	}
	return out, nil
}

// refill fetches fresh URLs, collapsing a burst of callers into one round trip:
// whoever gets the lock does the work, and anyone who was waiting on it returns
// as soon as they see the generation moved.
func (t *transfer) refill(ctx context.Context) error {
	t.mu.Lock()
	gen := t.refillGen
	t.mu.Unlock()

	t.refillMu.Lock()
	defer t.refillMu.Unlock()

	t.mu.Lock()
	stale := t.refillGen != gen
	t.mu.Unlock()
	if stale {
		return nil
	}

	resp, err := t.handshake(ctx, nil)
	if err != nil {
		return err
	}
	// The session left 'active' underneath us -- another actor is finalizing.
	// Stop pumping parts; the complete path polls it out.
	if resp.Status.Status != StatusActive {
		return errFinalizing
	}
	return nil
}

// ------------------------------------------------------------------- finalize --

// drive is the shared tail of Upload and Resume: pump the parts, wait for the
// whole-file digest, complete, and recover once from a verify mismatch.
func (t *transfer) drive(ctx context.Context, hashed <-chan hashResult) (*Result, error) {
	err := t.run(ctx)
	if err != nil && !errors.Is(err, errFinalizing) {
		return nil, err
	}

	var hr hashResult
	select {
	case hr = <-hashed:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if hr.err != nil {
		return nil, fmt.Errorf("uploadclient: hashing %s: %w", t.id, hr.err)
	}

	out, cerr := t.complete(ctx, hr.sum)
	if cerr != nil {
		// 422 invalid on complete means the ledger disagreed with the store.
		// The server has already deleted the offending rows, so one handshake
		// re-requests exactly those parts. Exactly one recovery cycle: if the
		// second complete fails too, the caller needs to hear about it.
		if !errors.Is(cerr, ErrInvalid) {
			return nil, cerr
		}
		t.c.log.Warn("complete rejected the ledger; re-sending the parts it dropped",
			"upload_id", t.id, "error", cerr.Error())
		if _, herr := t.handshake(ctx, nil); herr != nil {
			return nil, cerr
		}
		if rerr := t.run(ctx); rerr != nil && !errors.Is(rerr, errFinalizing) {
			return nil, cerr
		}
		if out, cerr = t.complete(ctx, hr.sum); cerr != nil {
			return nil, cerr
		}
	}

	st := t.snapshot()
	res := &Result{
		UploadID:      t.id,
		NodeID:        out.NodeID,
		Name:          out.Name,
		Fingerprint:   st.Fingerprint,
		SHA256:        hr.sum,
		PartSize:      st.PartSize,
		PartsTotal:    st.PartsTotal,
		PartsUploaded: int(t.uploaded.Load()),
		Resumed:       t.resumed,
	}
	if out.ParentID != nil {
		res.ParentID = *out.ParentID
	}
	t.c.log.Info("upload complete",
		"upload_id", t.id, "node_id", res.NodeID, "name", res.Name,
		"parts_uploaded", res.PartsUploaded, "sha256", res.SHA256)
	return res, nil
}

// complete publishes the file, polling instead of re-sending whenever a
// finalizer already holds the session.
func (t *transfer) complete(ctx context.Context, sha string) (*completeResp, error) {
	for round := 0; round < maxCompleteRounds; round++ {
		var out completeResp
		_, err := t.c.do(ctx, http.MethodPost, "/uploads/"+t.id+"/complete", completeReq{SHA256: sha}, &out)
		if err == nil {
			return &out, nil
		}
		if !errors.Is(err, ErrInProgress) {
			return nil, err
		}
		// 409 in_progress: someone else is finalizing. Poll until they are
		// done, then re-send -- complete on a published session is idempotent
		// and returns the node it published, including its final name.
		t.c.log.Debug("complete is in progress elsewhere; polling", "upload_id", t.id)
		if perr := t.awaitFinalizer(ctx); perr != nil {
			return nil, perr
		}
	}
	return nil, fmt.Errorf("uploadclient: upload %s: complete kept answering in_progress", t.id)
}

// awaitFinalizer polls GET /uploads/{id} until the session leaves 'completing'.
func (t *transfer) awaitFinalizer(ctx context.Context) error {
	deadline := time.Now().Add(t.c.maxPoll)
	for {
		st, err := t.c.Status(ctx, t.id)
		if err != nil {
			return err
		}
		t.adopt(*st, nil, false)
		switch st.Status {
		case StatusDone, StatusActive:
			// done: re-send complete and take the published result.
			// active: the finalizer rolled back, so complete is ours to retry.
			return nil
		case StatusAborted:
			return abortedErr(t.id)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("uploadclient: upload %s is still finalizing after %s", t.id, t.c.maxPoll)
		}
		if err := t.c.sleep(ctx, t.c.poll); err != nil {
			return err
		}
	}
}
