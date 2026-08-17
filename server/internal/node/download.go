package node

// The read behind GET /api/files/{id}/download: resolve a node id to the object
// its bytes live in, or refuse.
//
// Bytes never pass through the server -- the endpoint answers with a redirect to
// a presigned GET -- so this lookup is the entire authorization boundary for a
// download. Everything that is not "a live file of mine" is one indistinguishable
// ErrNotFound: someone else's file, a trashed one, a folder, an id that never
// existed. A 403 anywhere here would confirm that a stranger's id is real.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Download is everything the download endpoint needs: which object to sign, and
// the name to hand the browser.
type Download struct {
	NodeID    uuid.UUID
	Name      string
	Size      int64
	ObjectKey string
}

// Download resolves a file node the caller owns to its stored object.
//
// The join is inner on purpose: a file row whose blob is gone -- which the
// collector's row-before-object ordering can never produce, but which a
// half-applied restore or a future bug could -- answers ErrNotFound rather than
// signing a URL for bytes that are not there.
func (s *Store) Download(ctx context.Context, ownerID, id uuid.UUID) (Download, error) {
	const q = `
		SELECT n.id, n.name, b.size, b.object_key
		  FROM nodes n
		  JOIN blobs b ON b.id = n.blob_id
		 WHERE n.id = $1 AND n.owner_id = $2
		   AND n.kind = 'file' AND n.deleted_at IS NULL`

	var d Download
	if err := s.db.QueryRow(ctx, q, id, ownerID).Scan(&d.NodeID, &d.Name, &d.Size, &d.ObjectKey); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Download{}, ErrNotFound
		}
		return Download{}, fmt.Errorf("reading node for download: %w", err)
	}
	return d, nil
}
