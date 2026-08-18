package integration

// The upload protocol against the real thing: a real server binary, real
// Postgres, real Garage, real presigned PUTs from outside the process.
//
// Nothing here sleeps a
// wall-clock window except the presign-expiry case, which cannot be faked --
// a presign deadline is signed into the URL, not stored in a row.

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/testutil"
)

// loopFileSize is what the per-loop battery transfers: 100 MB at the test
// stack's 10 MiB parts is ten parts and a remainder, which exercises exactly
// the code paths the multi-GB run does -- multiple parts, a short final part,
// pagination-free reconciliation -- in seconds instead of minutes.
const loopFileSize = 100 << 20

// smallFileSize is for the cases whose subject is a decision, not a transfer.
const smallFileSize = 3 << 20

// goldenFingerprint is the pinned vector for the create fingerprint: 2048 zero
// bytes, named "report.pdf", lastModified 1700000000000 ms. The browser engine
// and server/internal/uploadclient both assert this same digest -- a mismatch
// between the two raises no error anywhere, it just silently restarts an
// upload as a fresh one instead of resuming.
const goldenFingerprint = "8d64d4f47dc60e17724c5541a57afb5014f972cec889a11d5d00f4f9548d7ca5"

// TestUploadFingerprintIsTheGoldenVector pins the battery's own fingerprints to
// the recipe production uses.
//
// The battery is what certifies the server, so a harness carrying its own
// encoding certifies nothing: every case here passes against it, the chimera and
// rematch cases included, because their whole premise is only that two
// fingerprints are equal to each other. A divergence raises no error anywhere --
// the server's (user, fingerprint, parent) match simply misses and a 50 GB
// resume silently restarts from zero.
func TestUploadFingerprintIsTheGoldenVector(t *testing.T) {
	owner := H.NewUser(t)
	up := H.NewUpload(t, owner, owner.RootID, "report.pdf", make([]byte, 2048))

	if up.ModMillis != 1_700_000_000_000 {
		t.Fatalf("the harness stamps lastModified %d, want the vector's 1700000000000 ms", up.ModMillis)
	}
	if up.Fingerprint != goldenFingerprint {
		t.Fatalf("the harness computed\n  %s\nwant the golden vector\n  %s",
			up.Fingerprint, goldenFingerprint)
	}
}

// ------------------------------------------------------------- lifecycle --

func TestUploadLifecycle(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "lifecycle")
	data := testutil.RandomBytes(loopFileSize, 1)

	up := H.NewUpload(t, owner, folder.ID, "movie.bin", data)
	created := up.MustCreate(t, http.StatusCreated)

	if created.PartSize != 10<<20 {
		t.Fatalf("part_size %d, want the configured 10 MiB", created.PartSize)
	}
	want := (loopFileSize + created.PartSize - 1) / created.PartSize
	if int64(created.PartsTotal) != want {
		t.Fatalf("parts_total %d, want ceil(%d/%d) = %d", created.PartsTotal, loopFileSize, created.PartSize, want)
	}
	if created.Status != "active" || created.Mode != "direct" {
		t.Fatalf("created status %q mode %q, want active/direct", created.Status, created.Mode)
	}
	// The create response carries only the first batch; the handshake is the
	// general URL source.
	if len(created.Presigned) != 8 {
		t.Fatalf("create returned %d presigned parts, want the first 8", len(created.Presigned))
	}
	if len(created.ConfirmedParts) != 0 {
		t.Fatalf("a fresh session already has confirmed parts: %v", created.ConfirmedParts)
	}

	up.UploadAll(t)

	status := up.Status(t)
	if len(status.ConfirmedParts) != created.PartsTotal {
		t.Fatalf("confirmed %d parts, want all %d", len(status.ConfirmedParts), created.PartsTotal)
	}

	done := up.MustComplete(t)
	if done.Name != "movie.bin" {
		t.Fatalf("published as %q, want movie.bin", done.Name)
	}
	if done.ParentID != nil {
		t.Fatalf("parent_id %v echoed on an upload that landed where it was aimed", *done.ParentID)
	}

	// The bytes, not the client's claim about the bytes.
	if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatalf("downloaded sha256 %s, want %s", got, testutil.SHA256Hex(data))
	}

	if s := up.Status(t); s.Status != "done" || s.NodeID == nil || *s.NodeID != done.NodeID {
		t.Fatalf("after complete the session is %+v, want done pointing at %s", s, done.NodeID)
	}
	// A finished session leaves nothing behind in Garage to sweep.
	if n := H.CountMultiparts(t); n != 0 {
		t.Fatalf("%d multipart upload(s) still open after a clean lifecycle", n)
	}
}

func TestUploadZeroByteFile(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "empty")

	up := H.NewUpload(t, owner, folder.ID, "nothing.txt", nil)
	created := up.MustCreate(t, http.StatusCreated)

	if created.PartsTotal != 0 || len(created.Presigned) != 0 {
		t.Fatalf("0-byte create returned parts_total=%d presigned=%d, want 0/0",
			created.PartsTotal, len(created.Presigned))
	}
	// No multipart upload is opened at all: Garage rejects a complete with an
	// empty part list, so this branch is a PutObject, not a shortcut.
	if row := H.Session(t, up.ID); row.S3UploadID != nil {
		t.Fatalf("0-byte session opened multipart %q", *row.S3UploadID)
	}

	done := up.MustComplete(t)
	if got := H.DownloadNode(t, done.NodeID); len(got) != 0 {
		t.Fatalf("downloaded %d bytes for a 0-byte file", len(got))
	}
}

func TestUploadEmojiAndRTLFilename(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "names")

	// Real RTL letters, not a bidi override: overrides are stripped by filename
	// hygiene, which is a different test in a different phase.
	for _, name := range []string{"🚀 launch 🎉.bin", "تقرير-٢٠٢٦.bin", "שלום עולם.bin"} {
		t.Run(name, func(t *testing.T) {
			data := testutil.RandomBytes(smallFileSize, 2)
			up := H.NewUpload(t, owner, folder.ID, name, data)
			done := up.Run(t, http.StatusCreated)
			if done.Name != name {
				t.Fatalf("published as %q, want %q", done.Name, name)
			}
			if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
				t.Fatalf("%q came back with a different digest", name)
			}
		})
	}
}

// --------------------------------------------------------------- sessions --

func TestUploadSameFileTwoFoldersAreTwoSessions(t *testing.T) {
	owner := H.NewUser(t)
	a := owner.CreateFolder(t, owner.RootID, "folder-a")
	b := owner.CreateFolder(t, owner.RootID, "folder-b")
	data := testutil.RandomBytes(smallFileSize, 3)

	first := H.NewUpload(t, owner, a.ID, "same.bin", data)
	first.MustCreate(t, http.StatusCreated)

	second := H.NewUpload(t, owner, b.ID, "same.bin", data)
	created := second.MustCreate(t, http.StatusCreated)

	if second.ID == first.ID {
		t.Fatalf("the same file into two folders reused session %s", first.ID)
	}
	if created.ParentID == nil || *created.ParentID != b.ID {
		t.Fatalf("second session's parent is %v, want %s", created.ParentID, b.ID)
	}

	first.UploadAll(t)
	second.UploadAll(t)
	firstDone := first.MustComplete(t)
	secondDone := second.MustComplete(t)
	if firstDone.NodeID == secondDone.NodeID {
		t.Fatal("both folders got the same node")
	}
}

func TestUploadSameFolderDuplicateIsOneSession(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "dedupe")
	data := testutil.RandomBytes(smallFileSize, 4)

	first := H.NewUpload(t, owner, folder.ID, "same.bin", data)
	first.MustCreate(t, http.StatusCreated)

	// A second create for the same file into the same folder is a resume, and
	// the match is decided before any name-conflict check.
	second := H.NewUpload(t, owner, folder.ID, "same.bin", data)
	matched := second.MustCreate(t, http.StatusOK)
	if matched.UploadID != first.ID {
		t.Fatalf("the duplicate create opened session %s, want the existing %s", matched.UploadID, first.ID)
	}
	if n := H.CountRows(t, "upload_sessions", "user_id = $1 AND parent_id = $2", owner.ID, folder.ID); n != 1 {
		t.Fatalf("%d session rows for one file in one folder, want 1", n)
	}
	// Nothing was confirmed yet, so there is nothing to verify against and URLs
	// flow immediately.
	if len(matched.VerifyParts) != 0 {
		t.Fatalf("a match with no confirmed parts armed verification: %v", matched.VerifyParts)
	}
	if len(matched.Presigned) == 0 {
		t.Fatal("a match with no confirmed parts returned no URLs")
	}
}

func TestUploadRematchAfterProgressArmsVerification(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "rematch")
	data := testutil.RandomBytes(4*(10<<20), 5)

	up := H.NewUpload(t, owner, folder.ID, "resumed.bin", data)
	up.MustCreate(t, http.StatusCreated)
	up.UploadParts(t, 1, 2)

	// Re-selecting the file is the only signal the server has that the bytes on
	// disk may no longer be the bytes already in Garage.
	again := H.NewUpload(t, owner, folder.ID, "resumed.bin", data)
	matched := again.MustCreate(t, http.StatusOK)

	if len(matched.VerifyParts) != 2 || matched.VerifyParts[0] != 1 || matched.VerifyParts[1] != 2 {
		t.Fatalf("verify_parts %v, want the pinned [1 2]", matched.VerifyParts)
	}
	if len(matched.Presigned) != 0 {
		t.Fatalf("an armed match leaked %d URLs", len(matched.Presigned))
	}

	// The handshake bounces until the client shows the pinned MD5s.
	bounced := again.Resume(t, nil)
	if len(bounced.VerifyParts) != 2 || bounced.Missing != nil {
		t.Fatalf("armed resume returned verify=%v missing=%v, want the pins and no URLs",
			bounced.VerifyParts, bounced.Missing)
	}

	proved := again.Resume(t, map[string]string{
		"1": again.PartMD5(t, 1),
		"2": again.PartMD5(t, 2),
	})
	if len(proved.VerifyParts) != 0 {
		t.Fatalf("a passed verification left the guard armed: %v", proved.VerifyParts)
	}
	if len(proved.Missing) != proved.PartsTotal-2 {
		t.Fatalf("resume offered %d URLs, want the %d missing parts", len(proved.Missing), proved.PartsTotal-2)
	}

	again.UploadAll(t)
	done := again.MustComplete(t)
	if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the resumed file does not match the source")
	}
}

// TestUploadChimeraGuard is the case the guard exists for: a second file with
// the same name, size, mtime and both edge MiBs -- so an identical fingerprint
// -- but different bytes in the middle. Resuming onto it would splice two files
// into one object that hashes to neither.
func TestUploadChimeraGuard(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "chimera")

	const size = 4 * (10 << 20)
	original := testutil.RandomBytes(size, 6)
	edited := append([]byte(nil), original...)
	// Rewrite one MiB inside part 2, which is neither edge and therefore
	// invisible to the fingerprint -- and which is one of the parts already in
	// Garage, so the guard is the only thing that can catch it.
	copy(edited[15<<20:16<<20], testutil.RandomBytes(1<<20, 7))

	up := H.NewUpload(t, owner, folder.ID, "vm.img", original)
	up.MustCreate(t, http.StatusCreated)
	up.UploadParts(t, 1, 2)

	chimera := H.NewUpload(t, owner, folder.ID, "vm.img", edited)
	if chimera.Fingerprint != up.Fingerprint {
		t.Fatalf("the fixture is not a chimera: fingerprints differ (%s vs %s)",
			chimera.Fingerprint, up.Fingerprint)
	}

	matched := chimera.MustCreate(t, http.StatusOK)
	if matched.UploadID != up.ID {
		t.Fatalf("the chimera opened a new session %s", matched.UploadID)
	}
	if len(matched.VerifyParts) == 0 {
		t.Fatal("the chimera create did not arm verification")
	}

	// Part 2 straddles the rewritten middle, so its MD5 no longer matches the
	// ledger and the resume is refused.
	refused := chimera.TryResume(t, map[string]string{
		"1": chimera.PartMD5(t, 1),
		"2": chimera.PartMD5(t, 2),
	}).Expect(http.StatusConflict)
	if refused.Code() != "invalid" || refused.Message() != "part verification failed" {
		t.Fatalf("chimera refusal was %s/%q, want invalid/\"part verification failed\"",
			refused.Code(), refused.Message())
	}

	// And it stays refused: a refusal that armed nothing would let the next call
	// through.
	chimera.TryResume(t, map[string]string{
		"1": chimera.PartMD5(t, 1),
		"2": chimera.PartMD5(t, 2),
	}).Expect(http.StatusConflict)

	// The original file still resumes cleanly -- the guard refuses the wrong
	// file, not the session.
	up.Resume(t, map[string]string{"1": up.PartMD5(t, 1), "2": up.PartMD5(t, 2)})
	up.UploadAll(t)
	done := up.MustComplete(t)
	if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(original) {
		t.Fatal("the original resumed into something that is not the original")
	}
}

// ------------------------------------------------------------ part checks --

func TestUploadDuplicatePartConfirmation(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "dup-part")
	data := testutil.RandomBytes(2*(10<<20), 8)

	up := H.NewUpload(t, owner, folder.ID, "twice.bin", data)
	up.MustCreate(t, http.StatusCreated)

	res := up.PutPart(t, 1, up.URLFor(t, 1, false))
	if res.Status != http.StatusOK {
		t.Fatalf("part PUT answered %d: %s", res.Status, res.Body)
	}
	up.Confirm(t, 1, res.ETag, res.MD5, res.Size).Expect(http.StatusOK)
	// Idempotent, keyed on (session_id, part_number): a retried confirmation is
	// the normal case when a response was lost, not an error.
	up.Confirm(t, 1, res.ETag, res.MD5, res.Size).Expect(http.StatusOK)

	if n := H.CountRows(t, "upload_parts", "session_id = $1", up.ID); n != 1 {
		t.Fatalf("%d ledger rows after a duplicate confirmation, want 1", n)
	}
	if parts := up.Status(t).ConfirmedParts; len(parts) != 1 {
		t.Fatalf("confirmed_parts %v, want exactly one", parts)
	}

	up.UploadAll(t)
	up.MustComplete(t)
}

func TestUploadWrongSizedPartRejectedAtConfirm(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "sizes")
	data := testutil.RandomBytes(3*(10<<20), 9)

	up := H.NewUpload(t, owner, folder.ID, "sized.bin", data)
	up.MustCreate(t, http.StatusCreated)

	res := up.PutPart(t, 1, up.URLFor(t, 1, false))

	// A non-final part must be exactly part_size. Claiming anything else fails
	// here, inside the client's retry budget, not at complete.
	short := up.Confirm(t, 1, res.ETag, res.MD5, res.Size-1).Expect(http.StatusUnprocessableEntity)
	if short.Code() != "invalid" {
		t.Fatalf("short part rejected with %q, want invalid", short.Code())
	}
	up.Confirm(t, 1, res.ETag, res.MD5, res.Size+1).Expect(http.StatusUnprocessableEntity)
	if n := H.CountRows(t, "upload_parts", "session_id = $1", up.ID); n != 0 {
		t.Fatal("a rejected confirmation still wrote a ledger row")
	}

	up.Confirm(t, 1, res.ETag, res.MD5, res.Size).Expect(http.StatusOK)
}

func TestUploadOversizePastDeclaredRejected(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "oversize")
	// 25 MiB at 10 MiB parts: three parts, the last one 5 MiB.
	data := testutil.RandomBytes(25<<20, 10)

	up := H.NewUpload(t, owner, folder.ID, "over.bin", data)
	created := up.MustCreate(t, http.StatusCreated)
	if created.PartsTotal != 3 {
		t.Fatalf("parts_total %d, want 3", created.PartsTotal)
	}

	res := up.PutPart(t, 3, up.URLFor(t, 3, false))

	// The final part may not carry more than what is left of the declared size.
	up.Confirm(t, 3, res.ETag, res.MD5, res.Size+1).Expect(http.StatusUnprocessableEntity)
	// Nor may a part exist past the declared total at all.
	up.Confirm(t, 4, res.ETag, res.MD5, res.Size).Expect(http.StatusUnprocessableEntity)
	if n := H.CountRows(t, "upload_parts", "session_id = $1", up.ID); n != 0 {
		t.Fatal("an oversize confirmation still wrote a ledger row")
	}

	up.Confirm(t, 3, res.ETag, res.MD5, res.Size).Expect(http.StatusOK)
}

// TestUploadDroppedConnectionMidPart cuts a part PUT off in the middle. Garage
// stores nothing for it, so the handshake still reports it missing and the
// client re-sends exactly that part -- no ledger row, no partial object.
func TestUploadDroppedConnectionMidPart(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "dropped")
	data := testutil.RandomBytes(2*(10<<20), 11)

	up := H.NewUpload(t, owner, folder.ID, "dropped.bin", data)
	up.MustCreate(t, http.StatusCreated)
	up.UploadPart(t, 1)

	url := up.URLFor(t, 2, false)
	if err := truncatedPut(url, up.Part(t, 2)); err == nil {
		t.Log("the truncated PUT was not refused; the assertions below are what matter")
	}

	missing := up.Resume(t, nil)
	if len(missing.ConfirmedParts) != 1 || missing.ConfirmedParts[0] != 1 {
		t.Fatalf("confirmed_parts %v after a dropped PUT, want [1]", missing.ConfirmedParts)
	}
	if len(missing.Missing) != 1 || missing.Missing[0].PartNumber != 2 {
		t.Fatalf("resume offered %v, want a URL for part 2 only", missing.Missing)
	}

	up.UploadAll(t)
	done := up.MustComplete(t)
	if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the file that survived a dropped connection is not byte-identical")
	}
}

// truncatedPut announces a Content-Length it never delivers, then hangs up. The
// server sees a connection that died mid-body, which is what a wifi drop looks
// like on the wire.
func truncatedPut(url string, body []byte) error {
	half := len(body) / 2
	req, err := http.NewRequest(http.MethodPut, url, &haltingReader{data: body[:half]})
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := (&http.Client{}).Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return fmt.Errorf("the truncated PUT answered %d", resp.StatusCode)
}

// haltingReader hands over its data and then reports a broken connection.
type haltingReader struct {
	data []byte
	off  int
}

func (r *haltingReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, fmt.Errorf("connection lost")
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// ------------------------------------------------------ reconciliation --

// TestUploadWrongSizedRemotePartConverges is the case a wrong-sized part PUT
// creates: the bytes reach Garage, the confirmation never happens, and the next
// handshake finds a part the ledger has never heard of.
//
// Adopting it would be permanent data loss dressed as success -- the part is
// reported confirmed, so it never appears in `missing` again and the client is
// never told to re-send it, while complete's total check fails on every attempt.
// What this pins is convergence: the session must still be finishable.
func TestUploadWrongSizedRemotePartConverges(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "short-part")
	data := testutil.RandomBytes(25<<20, 41)

	up := H.NewUpload(t, owner, folder.ID, "sliced.bin", data)
	created := up.MustCreate(t, http.StatusCreated)
	if up.PartsTotal != 3 {
		t.Fatalf("parts_total is %d, want 3 -- this case needs a non-final part to abuse", up.PartsTotal)
	}

	// The classic client mistake: slicing at the environment default rather
	// than the session's returned part_size. Garage stores it happily.
	if res := up.PutShort(t, 1, created.Presigned[0].URL, 5<<20); res.Status != http.StatusOK {
		t.Fatalf("the short PUT answered %d: %s", res.Status, res.Body)
	}
	up.UploadParts(t, 2, 3)

	// Three rounds, because the bug this pins is a loop that never terminates:
	// complete refuses, the handshake re-adopts the bad part, and nothing ever
	// tells the client which part to re-send.
	for round := 1; round <= 3; round++ {
		up.Complete(t).Expect(http.StatusUnprocessableEntity)

		out := up.ResumeVerified(t)
		if hasPart(out.ConfirmedParts, 1) {
			t.Fatalf("round %d: part 1 is reported confirmed at %d bytes, want it refused",
				round, 5<<20)
		}
		if !hasPresigned(out.Missing, 1) {
			t.Fatalf("round %d: the handshake named %v as missing, want part 1 among them",
				round, out.Missing)
		}
	}

	// The client does what it was told and re-sends the part at the right size.
	up.UploadPart(t, 1)
	done := up.MustComplete(t)
	if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the file finished after a wrong-sized part is not byte-identical")
	}
}

// TestUploadShortFinalRemotePartConverges is the same shape at the one part the
// confirm endpoint is lenient about: the final one, which may be anything up to
// the remainder.
//
// That leniency must not extend to adoption. A truncated final part sitting in
// Garage looks legal to CheckPart, so adopting it would re-confirm it after
// every rollback -- complete deletes the row, the next handshake takes it
// straight back, and the loop never terminates.
func TestUploadShortFinalRemotePartConverges(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "short-final")
	data := testutil.RandomBytes(25<<20, 42)

	up := H.NewUpload(t, owner, folder.ID, "truncated.bin", data)
	created := up.MustCreate(t, http.StatusCreated)
	up.UploadParts(t, 1, 2)

	// Part 3 is 5 MiB; 2 MiB of it lands and is never confirmed.
	if res := up.PutShort(t, 3, created.Presigned[2].URL, 2<<20); res.Status != http.StatusOK {
		t.Fatalf("the short final PUT answered %d: %s", res.Status, res.Body)
	}

	for round := 1; round <= 3; round++ {
		up.Complete(t).Expect(http.StatusUnprocessableEntity)

		out := up.ResumeVerified(t)
		if hasPart(out.ConfirmedParts, 3) {
			t.Fatalf("round %d: the truncated final part is reported confirmed: %v",
				round, out.ConfirmedParts)
		}
		if !hasPresigned(out.Missing, 3) {
			t.Fatalf("round %d: the handshake named %v as missing, want part 3", round, out.Missing)
		}
		for _, n := range []int{1, 2} {
			if !hasPart(out.ConfirmedParts, n) {
				t.Fatalf("round %d: part %d stopped being confirmed; only part 3 is in question",
					round, n)
			}
		}
	}

	up.UploadPart(t, 3)
	done := up.MustComplete(t)
	if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the file finished after a truncated final part is not byte-identical")
	}
}

// TestUploadShortConfirmedFinalPartConverges drives the other half: a final part
// that was actually CONFIRMED short. The confirm rule allows it (size <=
// remaining), so nothing rejects it until complete adds the sizes up.
//
// This is the case verifyLedger's total-mismatch fallback exists for, and the
// assertion is that it names the part on evidence: exactly part 3's row is
// deleted, parts 1 and 2 survive, and the handshake re-issues part 3.
func TestUploadShortConfirmedFinalPartConverges(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "short-confirmed")
	data := testutil.RandomBytes(25<<20, 43)

	up := H.NewUpload(t, owner, folder.ID, "shortconf.bin", data)
	created := up.MustCreate(t, http.StatusCreated)
	up.UploadParts(t, 1, 2)

	res := up.PutShort(t, 3, created.Presigned[2].URL, 2<<20)
	if res.Status != http.StatusOK {
		t.Fatalf("the short final PUT answered %d: %s", res.Status, res.Body)
	}
	// Confirm accepts it -- a final part is allowed to be short.
	up.Confirm(t, 3, res.ETag, res.MD5, res.Size).Expect(http.StatusOK)

	up.Complete(t).Expect(http.StatusUnprocessableEntity)

	out := up.ResumeVerified(t)
	if hasPart(out.ConfirmedParts, 3) {
		t.Fatalf("part 3 survived the failed complete as confirmed: %v", out.ConfirmedParts)
	}
	for _, n := range []int{1, 2} {
		if !hasPart(out.ConfirmedParts, n) {
			t.Fatalf("the rollback deleted part %d; only the short final part was at fault", n)
		}
	}
	if !hasPresigned(out.Missing, 3) {
		t.Fatalf("the handshake named %v as missing, want part 3", out.Missing)
	}

	up.UploadPart(t, 3)
	done := up.MustComplete(t)
	if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the file finished after a short confirmation is not byte-identical")
	}
}

// TestUploadResumeAfterMultipartVanishedIsGone pins the handshake's answer when
// the multipart it would reconcile against no longer exists.
//
// The contract's answer is 410 session_expired -- the one code both clients know
// how to act on: discard the record, create a fresh upload. A 500 instead maps
// to "backend trouble" in both clients, which means an indefinite retry loop
// against a session that can never come back.
func TestUploadResumeAfterMultipartVanishedIsGone(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "vanished")
	data := testutil.RandomBytes(2*(10<<20), 44)

	up := H.NewUpload(t, owner, folder.ID, "vanished.bin", data)
	up.MustCreate(t, http.StatusCreated)
	up.UploadPart(t, 1)

	row := H.Session(t, up.ID)
	if row.S3UploadID == nil {
		t.Fatal("the session has no multipart upload to abort")
	}
	H.OOBAbortMultipart(t, row.ObjectKey, *row.S3UploadID)

	resp := up.TryResume(t, nil).Expect(http.StatusGone)
	if resp.Code() != "session_expired" {
		t.Fatalf("resume answered code %q, want session_expired", resp.Code())
	}
}

// ------------------------------------------------------------- expiry --

func TestUploadExpiredSessionResumeIsGone(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "expired")
	data := testutil.RandomBytes(2*(10<<20), 12)

	up := H.NewUpload(t, owner, folder.ID, "stale.bin", data)
	up.MustCreate(t, http.StatusCreated)
	up.UploadPart(t, 1)

	// Seven days of sliding expiry, moved by a row update rather than a wait.
	H.ExpireUpload(t, up.ID)

	for _, call := range []struct {
		what string
		resp *testutil.Resp
	}{
		{"status", owner.Get(t, up.Path())},
		{"resume", up.TryResume(t, nil)},
		{"confirm", up.Confirm(t, 2, "deadbeef", "00000000000000000000000000000000", up.PartSize)},
		{"complete", up.Complete(t)},
	} {
		call.resp.Expect(http.StatusGone)
		if call.resp.Code() != "session_expired" {
			t.Fatalf("%s answered code %q, want session_expired", call.what, call.resp.Code())
		}
	}

	// The contract's answer to 410 is to discard the record and create afresh.
	fresh := H.NewUpload(t, owner, folder.ID, "stale.bin", data)
	created := fresh.MustCreate(t, http.StatusCreated)
	if created.UploadID == up.ID {
		t.Fatal("the fresh create resurrected the expired session")
	}
	if len(created.ConfirmedParts) != 0 {
		t.Fatalf("the fresh session inherited %v", created.ConfirmedParts)
	}

	fresh.UploadAll(t)
	done := fresh.MustComplete(t)
	if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the file uploaded after an expiry is not byte-identical")
	}
	if H.Session(t, up.ID).Status != "aborted" {
		t.Fatal("the expired session was not retired")
	}
}

// TestUploadExpiredPresignReHandshakes runs against a server whose presign TTL
// is two seconds, so a URL genuinely dies between being issued and being used.
//
// Garage answers an expired presign with 400 and <Code>InvalidRequest</Code>,
// measured in the day-0 spike -- not the 403 S3 semantics would suggest. A client
// that treated it as a hard failure would burn its integrity budget and stall,
// so this case asserts the re-handshake happens and the upload finishes.
func TestUploadExpiredPresignReHandshakes(t *testing.T) {
	child := H.SpawnServerWithEnv(t, "DRIVE_PRESIGN_TTL=2s")
	owner := H.NewUser(t).At(child.URL)

	folder := owner.CreateFolder(t, owner.RootID, "short-ttl")
	data := testutil.RandomBytes(2*(10<<20), 13)

	up := H.NewUpload(t, owner, folder.ID, "ttl.bin", data)
	created := up.MustCreate(t, http.StatusCreated)

	stale := created.Presigned[0].URL
	// The one deadline in the system that is not a Postgres row: it is signed
	// into the URL, so it can only be waited out.
	time.Sleep(3 * time.Second)

	res := up.PutPart(t, 1, stale)
	if res.Status == http.StatusOK {
		t.Fatal("a URL used three seconds after a two-second TTL was still accepted")
	}
	if !res.Expired() {
		t.Fatalf("expired PUT answered %d with body %q, want 403 or 400 InvalidRequest", res.Status, res.Body)
	}

	// The re-handshake path, which is what the engine does with that signal.
	up.UploadAll(t)
	done := up.MustComplete(t)
	if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the file finished through re-handshakes is not byte-identical")
	}
}

// ------------------------------------------------------------ publishing --

func TestUploadCompleteTimeConflictAutoRenames(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "rename")
	data := testutil.RandomBytes(smallFileSize, 14)

	up := H.NewUpload(t, owner, folder.ID, "report.pdf", data)
	up.MustCreate(t, http.StatusCreated)
	up.UploadAll(t)

	// The collision appears after the create answered: a long upload outlives
	// whatever the user was told at the start, so complete re-checks.
	existing := H.CreateFile(t, owner.ID, folder.ID, "report.pdf", []byte("the other one"))

	done := up.MustComplete(t)
	if done.Name != "report (1).pdf" {
		t.Fatalf("published as %q, want \"report (1).pdf\"", done.Name)
	}
	if done.NodeID == existing {
		t.Fatal("the upload published over the existing node")
	}
	if got := string(H.DownloadNode(t, existing)); got != "the other one" {
		t.Fatalf("the pre-existing file now holds %q", got)
	}
	if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the renamed upload is not byte-identical")
	}
}

func TestUploadCompleteTimeConflictReplace(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "replace")
	data := testutil.RandomBytes(smallFileSize, 15)

	up := H.NewUpload(t, owner, folder.ID, "report.pdf", data)
	up.ConflictPolicy = "replace"
	up.MustCreate(t, http.StatusCreated)
	up.UploadAll(t)

	existing := H.CreateFile(t, owner.ID, folder.ID, "report.pdf", []byte("the old one"))

	done := up.MustComplete(t)
	if done.Name != "report.pdf" {
		t.Fatalf("replace published as %q, want the plain name", done.Name)
	}
	// Replace trashes the collision in the publish transaction; it does not
	// destroy it.
	if n := H.CountRows(t, "nodes", "id = $1 AND deleted_at IS NOT NULL", existing); n != 1 {
		t.Fatal("replace did not trash the node it replaced")
	}
	if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the replacing upload is not byte-identical")
	}
}

// TestUploadDestinationGoneMidSession trashes AND purges the destination folder
// mid-session: the parent is gone by the time the bytes
// land, and the file must still be published -- into the user's root, with the
// new location echoed back. A complete often fires unattended; it may not fail
// because a folder moved underneath it.
//
// The two halves are separate because they exercise different disappearances:
// a trashed parent still exists as a row, a purged one does not.
func TestUploadDestinationGoneMidSession(t *testing.T) {
	run := func(t *testing.T, purge bool) {
		owner := H.NewUser(t)
		folder := owner.CreateFolder(t, owner.RootID, "doomed")
		data := testutil.RandomBytes(smallFileSize, 16)

		up := H.NewUpload(t, owner, folder.ID, "orphan.bin", data)
		up.MustCreate(t, http.StatusCreated)
		up.UploadAll(t)

		owner.Delete(t, "/api/nodes/"+folder.ID.String()).Expect(http.StatusNoContent)
		if purge {
			owner.Delete(t, "/api/nodes/"+folder.ID.String()+"/purge").Expect(http.StatusNoContent)
		}

		done := up.MustComplete(t)
		if done.ParentID == nil {
			t.Fatal("a re-parented complete did not echo parent_id")
		}
		if *done.ParentID != owner.RootID {
			t.Fatalf("re-parented to %s, want the root %s", *done.ParentID, owner.RootID)
		}
		if got := testutil.SHA256Hex(H.DownloadNode(t, done.NodeID)); got != testutil.SHA256Hex(data) {
			t.Fatal("the re-parented file is not byte-identical")
		}
		names := owner.Get(t, "/api/nodes/"+owner.RootID.String()+"/children").Expect(http.StatusOK).List().Names()
		if !contains(names, done.Name) {
			t.Fatalf("root holds %v, want %q among them", names, done.Name)
		}
	}

	t.Run("trashed", func(t *testing.T) { run(t, false) })
	t.Run("purged", func(t *testing.T) { run(t, true) })
}

// TestUploadTwoConcurrentSameNameBothPublish is the unattended-completion case:
// two different files aimed at one name, finished at the same moment. Neither
// may fail, and neither may overwrite the other.
func TestUploadTwoConcurrentSameNameBothPublish(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "collide")

	first := testutil.RandomBytes(smallFileSize, 17)
	second := testutil.RandomBytes(smallFileSize, 18)

	a := H.NewUpload(t, owner, folder.ID, "shared.bin", first)
	a.ConflictPolicy = "rename"
	a.MustCreate(t, http.StatusCreated)
	a.UploadAll(t)

	b := H.NewUpload(t, owner, folder.ID, "shared.bin", second)
	b.ConflictPolicy = "rename"
	b.MustCreate(t, http.StatusCreated)
	b.UploadAll(t)

	// The responses are collected in the goroutines and asserted on the test's
	// own goroutine: a t.Fatal off it would leave the other complete in flight.
	var (
		wg        sync.WaitGroup
		responses [2]*testutil.Resp
	)
	for i, up := range []*testutil.Uploader{a, b} {
		wg.Add(1)
		go func(i int, up *testutil.Uploader) {
			defer wg.Done()
			responses[i] = up.Complete(t)
		}(i, up)
	}
	wg.Wait()

	var results [2]testutil.CompletedUpload
	for i, resp := range responses {
		resp.Expect(http.StatusOK).JSON(&results[i])
	}

	if results[0].NodeID == results[1].NodeID {
		t.Fatal("both completes published the same node")
	}
	if results[0].Name == results[1].Name {
		t.Fatalf("both published as %q; one had to be renamed", results[0].Name)
	}
	if testutil.SHA256Hex(H.DownloadNode(t, results[0].NodeID)) == testutil.SHA256Hex(H.DownloadNode(t, results[1].NodeID)) {
		t.Fatal("the two publishes point at the same bytes")
	}
	children := owner.Get(t, "/api/nodes/"+folder.ID.String()+"/children").Expect(http.StatusOK).List()
	if len(children.Items) != 2 {
		t.Fatalf("the folder holds %v, want both files", children.Names())
	}
}

// TestUploadConcurrentCompleteIsRefused: the second finalizer to arrive finds
// the session already claimed and is told to poll rather than run a second
// CompleteMultipartUpload against the same object.
func TestUploadConcurrentCompleteIsRefused(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "in-progress")
	data := testutil.RandomBytes(smallFileSize, 19)

	up := H.NewUpload(t, owner, folder.ID, "once.bin", data)
	up.MustCreate(t, http.StatusCreated)
	up.UploadAll(t)

	// Constructed, not raced: the claim is a CAS inside an advisory lock, so a
	// session parked in 'completing' is exactly what a live finalizer looks like
	// to the request that arrives second. The true race below is a bonus.
	H.InjectCompleting(t, up.ID, time.Minute)

	refused := up.Complete(t).Expect(http.StatusConflict)
	if refused.Code() != "in_progress" {
		t.Fatalf("the second complete answered %q, want in_progress", refused.Code())
	}
	// Cancel is refused for the same reason: tearing down a multipart a
	// finalizer holds would lose the file.
	owner.Delete(t, up.Path()).Expect(http.StatusConflict)

	// Hand the session back and let the real thing finish it.
	if _, err := H.Pool.Exec(context.Background(),
		`UPDATE upload_sessions SET status = 'active' WHERE id = $1`, up.ID); err != nil {
		t.Fatalf("restoring the session: %v", err)
	}

	// And now the genuine race: several completes at once, at most one publish.
	const racers = 4
	var (
		wg        sync.WaitGroup
		responses [racers]*testutil.Resp
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			responses[i] = up.Complete(t)
		}(i)
	}
	wg.Wait()

	var (
		codes  []int
		nodeID uuid.UUID
	)
	for _, resp := range responses {
		codes = append(codes, resp.Status)
		if resp.Status != http.StatusOK && resp.Status != http.StatusConflict {
			t.Fatalf("a racing complete answered %d, want 200 or 409: %s", resp.Status, resp.Body)
		}
		if resp.Status != http.StatusOK {
			continue
		}
		var out testutil.CompletedUpload
		resp.JSON(&out)
		if nodeID != uuid.Nil && nodeID != out.NodeID {
			t.Fatalf("two completes published different nodes: %s and %s", nodeID, out.NodeID)
		}
		nodeID = out.NodeID
	}
	if nodeID == uuid.Nil {
		t.Fatalf("no complete won the race: statuses %v", codes)
	}
	if n := H.CountRows(t, "nodes", "parent_id = $1 AND deleted_at IS NULL", folder.ID); n != 1 {
		t.Fatalf("%d nodes published by %d concurrent completes, want 1", n, racers)
	}
	if got := testutil.SHA256Hex(H.DownloadNode(t, nodeID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the published file is not byte-identical")
	}
}

// --------------------------------------------------------------- listing --

func TestUploadListAndCancel(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "listing")

	up := H.NewUpload(t, owner, folder.ID, "listed.bin", testutil.RandomBytes(smallFileSize, 20))
	up.MustCreate(t, http.StatusCreated)
	up.UploadPart(t, 1)

	var list testutil.UploadList
	owner.Get(t, "/api/uploads").Expect(http.StatusOK).JSON(&list)
	if !containsUpload(list.Items, up.ID) {
		t.Fatalf("GET /uploads did not list %s", up.ID)
	}

	before := H.CountMultiparts(t)
	owner.Delete(t, up.Path()).Expect(http.StatusNoContent)
	// Idempotent: a cancel the client retries must not become an error.
	owner.Delete(t, up.Path()).Expect(http.StatusNoContent)

	if H.Session(t, up.ID).Status != "aborted" {
		t.Fatal("cancel did not abort the session")
	}
	if after := H.CountMultiparts(t); after >= before {
		t.Fatalf("%d multipart uploads before the cancel, %d after: nothing was discarded", before, after)
	}
	// A cancelled session is gone, not resumable.
	up.TryResume(t, nil).Expect(http.StatusGone)
}

// --------------------------------------------------------------- helpers --

func hasPart(ns []int, want int) bool {
	for _, n := range ns {
		if n == want {
			return true
		}
	}
	return false
}

func hasPresigned(parts []testutil.PresignedPart, want int) bool {
	for _, p := range parts {
		if p.PartNumber == want {
			return true
		}
	}
	return false
}

func containsUpload(items []testutil.UploadStatus, id uuid.UUID) bool {
	for _, s := range items {
		if s.UploadID == id {
			return true
		}
	}
	return false
}
