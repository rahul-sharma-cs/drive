// Package upload owns Drive's resumable-upload protocol: the session record,
// the part ledger, and the presigner that hands the browser its part URLs.
//
// Three rules shape everything here.
//
// The ledger is the truth about what the client believes, and Garage is the
// truth about what actually landed. Where they disagree -- the kill-9 window,
// where a part PUT succeeded but its confirmation never reached us -- the
// resume handshake reconciles them and Garage wins.
//
// A session is matched, never duplicated. One active session exists per
// (user, fingerprint, parent), enforced by a partial unique index, so a second
// create for the same file into the same folder resumes the first.
//
// An upload_id is an identifier, never a credential. Every entry point
// re-checks ownership; a session belonging to someone else is indistinguishable
// from one that never existed.
package upload

import (
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Session lifecycle. Only 'active' accepts parts; the finalize path moves a
// session through 'completing' to 'done', and cancel/GC to 'aborted'.
const (
	StatusActive     = "active"
	StatusCompleting = "completing"
	StatusDone       = "done"
	StatusAborted    = "aborted"
)

// ModeDirect is the only transfer mode built: the browser PUTs parts straight
// to Garage with presigned URLs. The relay fallback the plan specced as a
// contingency was not needed -- the day-0 spike proved direct works -- so
// nothing ever writes 'relay'.
const ModeDirect = "direct"

const (
	// TTL is the sliding session lifetime, refreshed on every authenticated
	// touch. A multi-day gap in a 50 GB upload has to survive it.
	TTL = 7 * 24 * time.Hour

	// MaxParts is S3's ceiling on parts per multipart upload.
	MaxParts = 10000

	// PartSizeGrain is Garage's block size. A grown part size stays a multiple
	// of it, exactly like the configured one.
	PartSizeGrain = 10 << 20

	// MaxPartSize is S3's ceiling on a single part.
	MaxPartSize = 5 << 30

	// PresignBatch is how many missing parts a create response carries URLs
	// for. The resume handshake is the general URL source; this is just enough
	// to get the transfer moving.
	PresignBatch = 8
)

var (
	// ErrNotFound is every miss: no such session, someone else's session, an
	// unparseable id.
	ErrNotFound = errors.New("upload session not found")
	// ErrExpired is a session past its sliding expiry, or one already aborted.
	// The client's answer to it is to discard its record and create afresh.
	ErrExpired = errors.New("upload session expired")
	// ErrRace is the 23505 on the active-session unique index: another tab won
	// the create. The caller aborts the multipart it just opened and returns
	// the winner.
	ErrRace = errors.New("another session for this file already exists")
	// ErrInvalid is a value the protocol rejects -- a bad part number, a
	// wrong-sized part, a fingerprint we will not store.
	ErrInvalid = errors.New("invalid")
	// ErrTooLarge is a file that cannot be split into 10,000 parts even at the
	// maximum part size.
	ErrTooLarge = errors.New("file is too large to upload")
)

// Session is one upload_sessions row.
type Session struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	ParentID       *uuid.UUID
	FileName       string
	FileSize       int64
	Mime           *string
	Fingerprint    string
	ConflictPolicy *string
	// S3UploadID is NULL for the 0-byte special case, which never opens a
	// multipart upload at all.
	S3UploadID *string
	ObjectKey  string
	PartSize   int64
	PartsTotal int
	Status     string
	Mode       string
	// VerifyParts non-empty means chimera verification is armed: the pinned
	// part numbers a resume must show client MD5s for before it gets any URL.
	VerifyParts []int
	NodeID      *uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time
}

// Expired reports whether the sliding expiry has run out. Status alone is not
// enough: GC flips an expired session to 'aborted' eventually, but every
// endpoint has to treat it as gone the moment the deadline passes.
func (s *Session) Expired() bool {
	return s.Status == StatusAborted || (s.Status == StatusActive && !s.ExpiresAt.After(time.Now()))
}

// Live reports whether the session still accepts parts.
func (s *Session) Live() bool { return s.Status == StatusActive && !s.Expired() }

// Remaining is how many bytes part n is allowed to carry: exactly PartSize for
// every part but the last, and whatever is left for the last.
func (s *Session) Remaining(n int) int64 {
	return s.FileSize - int64(n-1)*s.PartSize
}

// CheckPart validates a part confirmation's number and size. A wrong-sized
// part must fail here, inside the client's retry budget, rather than at
// complete when the whole transfer is already paid for.
func (s *Session) CheckPart(n int, size int64) error {
	if n < 1 || n > s.PartsTotal {
		return ErrInvalid
	}
	if n < s.PartsTotal {
		if size != s.PartSize {
			return ErrInvalid
		}
		return nil
	}
	if size <= 0 || size > s.Remaining(n) {
		return ErrInvalid
	}
	return nil
}

// AdoptablePart reports whether a part ListParts returned may be taken into the
// ledger as confirmed, which is a stricter question than CheckPart's.
//
// CheckPart is the confirm endpoint's rule and follows PLAN's wording: the final
// part passes at anything up to the remainder. Adoption cannot afford that
// leniency. A part adopted from Garage is reported confirmed and never appears
// in `missing` again, so if its size does not make the declared file size add
// up, complete refuses forever and no response ever tells the client which part
// to re-send. The only size that can be right for an unconfirmed part is the
// exact one this session's geometry demands.
func (s *Session) AdoptablePart(n int, size int64) bool {
	if n < 1 || n > s.PartsTotal {
		return false
	}
	if n < s.PartsTotal {
		return size == s.PartSize
	}
	return size == s.Remaining(n)
}

// Part is one upload_parts row, or one entry ListParts returned. MD5 is nil
// for rows adopted from Garage during reconciliation: those parts landed
// before their confirmation did, so no client MD5 was ever recorded.
type Part struct {
	Number int
	Size   int64
	ETag   string
	MD5    *string
}

// PartNumbers returns the sorted part numbers of a ledger.
func PartNumbers(parts []Part) []int {
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Number)
	}
	sort.Ints(out)
	return out
}

// MissingParts returns the part numbers of partsTotal that confirmed does not
// cover, in ascending order.
func MissingParts(partsTotal int, confirmed []Part) []int {
	have := make(map[int]bool, len(confirmed))
	for _, p := range confirmed {
		have[p.Number] = true
	}
	var missing []int
	for n := 1; n <= partsTotal; n++ {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	return missing
}

// ResolvePartSize picks the part size for a file, starting from the configured
// one and growing it when the file would need more than 10,000 parts. The
// grown size is the smallest multiple of Garage's block size that fits, so
// parts stay block-aligned; a file too big even for the maximum part size is
// ErrTooLarge.
//
// A 0-byte file gets no parts at all.
func ResolvePartSize(fileSize, configured int64) (partSize int64, partsTotal int, err error) {
	if fileSize < 0 || configured <= 0 {
		return 0, 0, ErrInvalid
	}
	if fileSize == 0 {
		return configured, 0, nil
	}

	partSize = configured
	total := ceilDiv(fileSize, partSize)
	if total > MaxParts {
		partSize = ceilDiv(ceilDiv(fileSize, MaxParts), PartSizeGrain) * PartSizeGrain
		if partSize > MaxPartSize {
			return 0, 0, ErrTooLarge
		}
		total = ceilDiv(fileSize, partSize)
	}
	return partSize, int(total), nil
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }
