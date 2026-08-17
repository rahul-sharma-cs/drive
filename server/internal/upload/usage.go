package upload

// How many bytes the service is holding, and how many belong to one user.
//
// This is what the volume caps are enforced against, and it is the only spend
// control that exists: the object store bills for what it holds and offers no
// limit of its own, so if this query is wrong the bill is wrong.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Usage is one reading of stored volume.
type Usage struct {
	// Stored is every published blob's size, service-wide.
	Stored int64
	// InFlight is the declared size of every upload still running. These bytes
	// are not published, but the parts already PUT are billed as storage from
	// the moment they land -- counting only Stored would let a hundred
	// simultaneous uploads walk straight past the cap.
	InFlight int64
	// User is the bytes one owner accounts for: their files, trashed ones
	// included -- trash keeps its bytes until it is purged and collected -- plus
	// their own uploads still running, for the same reason InFlight exists. A
	// quota that counted only published files would let one account start a
	// hundred uploads that each pass the check on their own.
	User int64
}

// Total is the figure the service-wide cap is compared against.
func (u Usage) Total() int64 { return u.Stored + u.InFlight }

// Usage reads current volume for the service and for one owner in a single
// round trip.
func (s *Store) Usage(ctx context.Context, ownerID uuid.UUID) (Usage, error) {
	const q = `
		SELECT
			(SELECT COALESCE(sum(size), 0) FROM blobs),
			(SELECT COALESCE(sum(file_size), 0) FROM upload_sessions
			  WHERE status IN ('active', 'completing') AND expires_at > now()),
			(SELECT COALESCE(sum(size), 0) FROM nodes
			  WHERE owner_id = $1 AND kind = 'file')
			+ (SELECT COALESCE(sum(file_size), 0) FROM upload_sessions
			    WHERE user_id = $1 AND status IN ('active', 'completing')
			      AND expires_at > now())`

	var u Usage
	if err := s.db.QueryRow(ctx, q, ownerID).Scan(&u.Stored, &u.InFlight, &u.User); err != nil {
		return Usage{}, fmt.Errorf("reading storage usage: %w", err)
	}
	return u, nil
}
