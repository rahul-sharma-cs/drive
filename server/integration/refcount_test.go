package integration

// The refcount battery: what happens to the bytes when the rows pointing at
// them go away.
//
// The rule the whole thing turns on is that purge never calls DeleteObject. It
// decrements, and only the collector -- after a grace longer than any presigned
// URL can live -- deletes the row and then the object. So a copy outliving its
// original is not a lucky accident; it is the design, and these cases hold it
// to that.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/testutil"
)

// blobGrace is how far past the collector's two-hour window a blob has to be
// pushed before it is collectible. Moved by a row update, not by waiting.
const blobGrace = 3 * time.Hour

// TestRefcountCopySurvivesPurgedOriginal: a copy shares its original's object,
// so purging the original must leave the copy downloadable byte for byte.
func TestRefcountCopySurvivesPurgedOriginal(t *testing.T) {
	ctx := context.Background()
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "copies")
	elsewhere := owner.CreateFolder(t, owner.RootID, "elsewhere")

	data := testutil.RandomBytes(smallFileSize, 30)
	original := H.NewUpload(t, owner, folder.ID, "original.bin", data).Run(t, http.StatusCreated)

	copied := owner.Post(t, "/api/nodes/"+original.NodeID.String()+"/copy",
		map[string]any{"parent_id": elsewhere.ID}).Expect(http.StatusCreated).Node()

	if got := H.Refcount(t, original.NodeID); got != 2 {
		t.Fatalf("refcount %d after a copy, want 2", got)
	}
	key := H.ObjectKeyOf(t, original.NodeID)
	if H.ObjectKeyOf(t, copied.ID) != key {
		t.Fatal("the copy points at a different object; copies share bytes by design")
	}

	purge(t, owner, original.NodeID)

	if got := H.Refcount(t, copied.ID); got != 1 {
		t.Fatalf("refcount %d after purging one of two references, want 1", got)
	}
	// A pass now must not touch the object: something still points at it.
	testutil.GC(t, ctx)
	if !H.ObjectExists(t, key) {
		t.Fatal("collection deleted an object a live node still references")
	}
	if got := testutil.SHA256Hex(H.DownloadNode(t, copied.ID)); got != testutil.SHA256Hex(data) {
		t.Fatal("the surviving copy is not byte-identical")
	}
}

// TestRefcountPurgeBothDeletesTheObject: with the last reference gone the row
// drops and the object with it -- but only after the grace, which exists so an
// already-issued download URL can never outlive its bytes.
func TestRefcountPurgeBothDeletesTheObject(t *testing.T) {
	ctx := context.Background()
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "doomed-bytes")
	elsewhere := owner.CreateFolder(t, owner.RootID, "doomed-copies")

	data := testutil.RandomBytes(smallFileSize, 31)
	original := H.NewUpload(t, owner, folder.ID, "twice.bin", data).Run(t, http.StatusCreated)
	key := H.ObjectKeyOf(t, original.NodeID)

	copied := owner.Post(t, "/api/nodes/"+original.NodeID.String()+"/copy",
		map[string]any{"parent_id": elsewhere.ID}).Expect(http.StatusCreated).Node()

	purge(t, owner, original.NodeID)
	purge(t, owner, copied.ID)

	if n := H.CountRows(t, "blobs", "object_key = $1 AND refcount = 0", key); n != 1 {
		t.Fatal("purging every reference did not leave the blob at refcount 0")
	}
	// Inside the grace the object stays: a presigned GET issued a minute ago is
	// still valid and must still resolve.
	testutil.GC(t, ctx)
	if !H.ObjectExists(t, key) {
		t.Fatal("an unreferenced object was deleted inside its grace window")
	}

	testutil.Backdate(t, H.Pool, "blobs", "created_at", blobGrace, "object_key = $1", key)
	testutil.GC(t, ctx)

	if n := H.CountRows(t, "blobs", "object_key = $1", key); n != 0 {
		t.Fatal("the unreferenced blob row survived collection")
	}
	if H.ObjectExists(t, key) {
		t.Fatalf("%s is still in Garage after its last reference was purged", key)
	}
}

// TestRefcountTrashPurgeSubtreeWithCopy purges a whole folder tree that holds
// one side of a copy pair. The subtree's own file dies; the shared bytes do
// not, because the copy outside the subtree still points at them.
func TestRefcountTrashPurgeSubtreeWithCopy(t *testing.T) {
	ctx := context.Background()
	owner := H.NewUser(t)

	tree := owner.CreateFolder(t, owner.RootID, "tree")
	inner := owner.CreateFolder(t, tree.ID, "inner")
	keep := owner.CreateFolder(t, owner.RootID, "keep")

	shared := testutil.RandomBytes(smallFileSize, 32)
	lonely := testutil.RandomBytes(smallFileSize, 33)

	sharedNode := H.NewUpload(t, owner, inner.ID, "shared.bin", shared).Run(t, http.StatusCreated)
	lonelyNode := H.NewUpload(t, owner, inner.ID, "lonely.bin", lonely).Run(t, http.StatusCreated)

	survivor := owner.Post(t, "/api/nodes/"+sharedNode.NodeID.String()+"/copy",
		map[string]any{"parent_id": keep.ID}).Expect(http.StatusCreated).Node()

	sharedKey := H.ObjectKeyOf(t, sharedNode.NodeID)
	lonelyKey := H.ObjectKeyOf(t, lonelyNode.NodeID)

	// Trash and purge the whole tree, two levels above the files.
	purge(t, owner, tree.ID)

	if n := H.CountRows(t, "nodes", "id = ANY($1)",
		[]uuid.UUID{tree.ID, inner.ID, sharedNode.NodeID, lonelyNode.NodeID}); n != 0 {
		t.Fatalf("%d rows of the purged subtree survived", n)
	}
	if got := H.Refcount(t, survivor.ID); got != 1 {
		t.Fatalf("the surviving copy's refcount is %d, want 1", got)
	}

	testutil.Backdate(t, H.Pool, "blobs", "created_at", blobGrace, "object_key = ANY($1)",
		[]string{sharedKey, lonelyKey})
	testutil.GC(t, ctx)

	if H.ObjectExists(t, lonelyKey) {
		t.Fatal("the purged subtree's own object survived collection")
	}
	if !H.ObjectExists(t, sharedKey) {
		t.Fatal("collection deleted bytes the surviving copy still points at")
	}
	if got := testutil.SHA256Hex(H.DownloadNode(t, survivor.ID)); got != testutil.SHA256Hex(shared) {
		t.Fatal("the copy outside the purged subtree is not byte-identical")
	}
}

// purge trashes a node and then purges it, which is the only order the API
// accepts -- purge operates on trash.
func purge(t *testing.T, c *testutil.Client, id uuid.UUID) {
	t.Helper()
	c.Delete(t, "/api/nodes/"+id.String()).Expect(http.StatusNoContent)
	c.Delete(t, "/api/nodes/"+id.String()+"/purge").Expect(http.StatusNoContent)
}
