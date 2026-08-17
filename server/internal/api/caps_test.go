package api

// The three volume caps, at the one place new bytes are admitted.
//
// The service-wide one is the only spend control that exists: the object store
// bills for what it holds and offers no limit of its own. All three default to
// off, which is what lets the battery upload multi-GB files -- so every case
// here sets its own numbers.

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/upload"
)

func capsServer(t *testing.T, tune func(*config.Config)) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := authTestPool(t)
	cfg := uploadTestConfig(t)
	tune(cfg)

	uploadS3Once.Do(func() {
		uploadS3Client, uploadS3Presign, uploadS3Err = blob.New(context.Background(), cfg)
	})
	if uploadS3Err != nil {
		t.Fatalf("drive-test Garage: %v", uploadS3Err)
	}
	return New(cfg, pool, nil, nil, uploadS3Client, uploadS3Presign).Routes(), pool
}

// capsUsage reads what the service is already holding, so a case can set a cap
// relative to it rather than assuming an empty database it does not own.
func capsUsage(t *testing.T, pool *pgxpool.Pool, ownerID uuid.UUID) upload.Usage {
	t.Helper()
	used, err := upload.NewStore(pool).Usage(context.Background(), ownerID)
	if err != nil {
		t.Fatalf("reading usage: %v", err)
	}
	return used
}

func TestUploadRefusedPastTheMaxFileSize(t *testing.T) {
	h, pool := capsServer(t, func(c *config.Config) { c.MaxFileSize = 1 << 20 })
	owner := nodeNewUser(t, pool)
	folder := nodeMkFolder(t, pool, owner, owner.RootID, "capped")

	rec := uploadCreate(t, h, owner, folder, "too-big.bin", 2<<20, "fp-"+uuid.NewString(), "")
	nodeWant(t, rec.ResponseRecorder, http.StatusUnprocessableEntity, CodeInvalid)

	// Exactly at the limit is inside it.
	rec = uploadCreate(t, h, owner, folder, "just-right.bin", 1<<20, "fp-"+uuid.NewString(), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("a file exactly at the limit: status %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	uploadCancelLater(t, h, owner, uploadDecode(t, rec).UploadID)
}

// The cap counts uploads still running as well as published blobs. Only
// counting what is published would let a hundred simultaneous uploads walk
// straight past it -- and the parts they have already PUT are billed as storage
// from the moment they land.
func TestUploadRefusedPastTheServiceStorageCap(t *testing.T) {
	pool := authTestPool(t)
	owner := nodeNewUser(t, pool)
	headroom := int64(4096)
	used := capsUsage(t, pool, owner.ID)

	h, _ := capsServer(t, func(c *config.Config) { c.StorageCap = used.Total() + headroom })
	folder := nodeMkFolder(t, pool, owner, owner.RootID, "service-cap")

	rec := uploadCreate(t, h, owner, folder, "over.bin", headroom*2, "fp-"+uuid.NewString(), "")
	nodeWant(t, rec.ResponseRecorder, http.StatusUnprocessableEntity, CodeInvalid)

	// Under the headroom it goes through, so the refusal above is the cap and
	// not a broken endpoint.
	rec = uploadCreate(t, h, owner, folder, "under.bin", headroom/4, "fp-"+uuid.NewString(), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("an upload inside the cap: status %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	created := uploadDecode(t, rec)
	uploadCancelLater(t, h, owner, created.UploadID)

	// And that session now counts against the cap while it runs.
	if after := capsUsage(t, pool, owner.ID); after.InFlight <= used.InFlight {
		t.Errorf("in-flight bytes did not move: %d then %d", used.InFlight, after.InFlight)
	}
}

func TestUploadRefusedPastTheUserQuota(t *testing.T) {
	h, pool := capsServer(t, func(c *config.Config) { c.UserQuota = 4096 })
	owner := nodeNewUser(t, pool)
	folder := nodeMkFolder(t, pool, owner, owner.RootID, "quota")

	// A fresh user holds nothing, so the quota is the only thing in play.
	nodeMkFile(t, pool, owner, folder, "already-here.bin") // 11 bytes

	rec := uploadCreate(t, h, owner, folder, "over.bin", 8192, "fp-"+uuid.NewString(), "")
	nodeWant(t, rec.ResponseRecorder, http.StatusUnprocessableEntity, CodeInvalid)

	rec = uploadCreate(t, h, owner, folder, "under.bin", 3000, "fp-"+uuid.NewString(), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("an upload inside the quota: status %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	uploadCancelLater(t, h, owner, uploadDecode(t, rec).UploadID)

	// That upload is running, not published, and it still counts: otherwise one
	// account starts a hundred uploads that each pass the check on their own.
	rec = uploadCreate(t, h, owner, folder, "one-too-many.bin", 3000, "fp-"+uuid.NewString(), "")
	nodeWant(t, rec.ResponseRecorder, http.StatusUnprocessableEntity, CodeInvalid)
}

// A resume must never be refused by a cap.
//
// Its bytes are already stored and already counted, so turning it away strands
// them: the upload can neither finish nor free anything, and the user is told to
// delete something to make room for bytes that are already taking up the room.
// This is why the capacity check sits after the active-session match and not
// with the other validation.
func TestResumeIsNotRefusedByACapTheUploadAlreadyPassed(t *testing.T) {
	pool := authTestPool(t)
	owner := nodeNewUser(t, pool)
	folder := nodeMkFolder(t, pool, owner, owner.RootID, "resumable")
	fingerprint := "fp-" + uuid.NewString()

	open, _ := capsServer(t, func(c *config.Config) {})
	rec := uploadCreate(t, open, owner, folder, "long-running.bin", 4096, fingerprint, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	created := uploadDecode(t, rec)
	uploadCancelLater(t, open, owner, created.UploadID)

	// The service fills up while the upload is running.
	full, _ := capsServer(t, func(c *config.Config) { c.StorageCap = 1; c.UserQuota = 1 })

	rec = uploadCreate(t, full, owner, folder, "long-running.bin", 4096, fingerprint, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("re-creating a running upload under a full service: status %d, want 200 (body %s)",
			rec.Code, rec.Body)
	}
	if got := uploadDecode(t, rec); got.UploadID != created.UploadID {
		t.Errorf("resumed session %s, want the original %s", got.UploadID, created.UploadID)
	}
}
