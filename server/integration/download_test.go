package integration

// The download half of the round trip, end to end: real upload through the real
// protocol, then GET /api/files/{id}/download against the real server, following
// the redirect into the store exactly as a browser would.
//
// The upload battery proves bytes go in by reading the object directly. This is
// the only place that proves they come back out through the product's own
// endpoint -- and that what comes back is an attachment, not a page.

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/rahul-sharma-cs/drive/server/internal/testutil"
	"github.com/rahul-sharma-cs/drive/server/internal/upload"
)

// evilName is markup with a name to match: if any layer of this path let the
// store choose the Content-Type, this file would execute on the store's origin.
const evilName = `تقرير "evil" 🚀.html`

func TestDownloadEndpointReturnsTheBytesAsAnAttachment(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "downloads")

	data := testutil.RandomBytes(smallFileSize, 71)
	done := H.NewUpload(t, owner, folder.ID, evilName, data).Run(t, http.StatusCreated)

	// The client follows the 302, so this response is the store's.
	resp := owner.Get(t, "/api/files/"+done.NodeID.String()+"/download").Expect(http.StatusOK)

	if !bytes.Equal(resp.Body, data) {
		t.Fatalf("the download returned %d bytes, want the %d that were uploaded", len(resp.Body), len(data))
	}
	if got, want := resp.Header.Get("Content-Disposition"), upload.AttachmentDisposition(done.Name); got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
	if got := resp.Header.Get("Content-Type"); got != upload.DownloadContentType {
		t.Errorf("Content-Type = %q, want %q: uploaded markup must never come back renderable",
			got, upload.DownloadContentType)
	}
}

// A download of a file that has since been trashed is a miss, not a redirect --
// the authorization is re-run at download time, never carried in the URL.
func TestDownloadOfATrashedFileIsRefused(t *testing.T) {
	owner := H.NewUser(t)
	folder := owner.CreateFolder(t, owner.RootID, "vanishing")

	data := testutil.RandomBytes(1<<16, 72)
	done := H.NewUpload(t, owner, folder.ID, "temporary.bin", data).Run(t, http.StatusCreated)

	owner.Get(t, "/api/files/"+done.NodeID.String()+"/download").Expect(http.StatusOK)

	owner.Delete(t, "/api/nodes/"+done.NodeID.String()).Expect(http.StatusNoContent)
	owner.Get(t, "/api/files/"+done.NodeID.String()+"/download").Expect(http.StatusNotFound)
}
