// Package share owns public share links: the shares table, the guest sessions
// a link holder is given, the download cap those sessions count against, and
// the access log that records every gate pass and every issued URL.
//
// Two rules shape everything here. First, the token is the whole credential
// and the database never sees it: a share row holds sha256(token), a guest
// session row holds sha256(cookie), and neither can be turned back into the
// string a browser presents. Second, a revoked share is kept, not deleted --
// share_access_log points at it and the owner's history has to stay
// attributable after the link is gone -- so "active" is revoked_at IS NULL
// everywhere, and the one-active-link-per-file rule is the partial unique
// index shares_node_active_idx rather than a check in Go.
//
// Nothing here knows about HTTP. Password hashing stays in the API layer,
// which hands the store a finished Argon2id string or nil.
package share

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Mode and permission are the only values this phase writes; the columns'
// check constraints allow more, and the API refuses anything else as
// unsupported rather than storing a value no route would honour.
const (
	ModePublic     = "public"
	PermissionView = "view"
)

// GuestSessionTTL is a guest session's sliding lifetime: every use extends
// expires_at this far, so it measures inactivity, not the visit.
const GuestSessionTTL = 30 * time.Minute

// MaxDownloadsCap bounds an owner's download limit. There is no use for a
// larger number and an int column has to be bounded by something.
const MaxDownloadsCap = 1_000_000

// MaxUserAgent bounds what Log stores of an attacker-controlled header, in
// runes -- the same cap the session list applies to the same string.
const MaxUserAgent = 200

// The access log's actions, matching share_access_log.action's check.
//
// ActionDownload means a presigned URL for the bytes was issued -- a download
// or an inline preview alike -- and never that the bytes were fetched, which
// the server cannot see.
const (
	ActionView     = "view"
	ActionDownload = "download"
	ActionDenied   = "denied"
)

var (
	// ErrNotFound covers every miss on a share or a guest session: no such
	// row, another owner's, a revoked one, an expired session, a session that
	// belongs to a different share. They are deliberately indistinguishable.
	ErrNotFound = errors.New("share not found")
	// ErrExists is a create against a file that already has an active link:
	// the one slot is taken, whether last week or by a concurrent request.
	ErrExists = errors.New("the file already has an active share link")
	// ErrUnsupported is a create against a node that is not a file.
	ErrUnsupported = errors.New("only files can be shared")
)

// State is what Resolve computed about a link, in the same query that found
// it. Only StateLive serves anything; every other state is one identical 404
// to the recipient and its own reason in the owner's log.
type State string

const (
	StateLive    State = "live"
	StateRevoked State = "revoked"
	StateExpired State = "expired"
	StateTrashed State = "trashed"
	StatePurged  State = "purged"
)

// Settings is what an owner sets on a link. ExpiresAt and MaxDownloads are
// the whole value each time -- nil clears the column -- because the settings
// body always sends both. Password is three-way (keep, clear, set), because
// an owner editing only an expiry cannot re-send a password that exists
// nowhere but as a hash.
type Settings struct {
	ExpiresAt    *time.Time
	MaxDownloads *int
	Password     PasswordChange
}

// PasswordChange says what happens to the password column. The zero value
// keeps it as it is; Set with a nil Hash clears it; Set with a Hash replaces
// it. Hash is the finished Argon2id string, never the password.
type PasswordChange struct {
	Set  bool
	Hash *string
}

// SetPassword is the change that stores hash.
func SetPassword(hash string) PasswordChange { return PasswordChange{Set: true, Hash: &hash} }

// ClearPassword is the change that removes the gate.
func ClearPassword() PasswordChange { return PasswordChange{Set: true} }

// Node is what the owner's share DTO says about the shared file: enough to
// name it and to build a link to its folder.
type Node struct {
	ID       uuid.UUID
	ParentID *uuid.UUID
	Name     string
	Size     *int64
	Mime     *string
}

// Share is one row of the shares table as the owner sees it. It carries no
// token and no hash: a database read never yields a working link.
type Share struct {
	ID            uuid.UUID
	CreatedBy     uuid.UUID
	HasPassword   bool
	ExpiresAt     *time.Time
	MaxDownloads  *int
	DownloadCount int
	CreatedAt     time.Time
	RevokedAt     *time.Time

	// Node is the shared file. NodeLive is false while it sits in the trash
	// (or has lost its blob), which is when the link is inert.
	Node     Node
	NodeLive bool
}

// Cursor is the keyset position in the owner's listing, which is ordered
// (created_at DESC, id DESC) -- the owner-list index's own order.
type Cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        uuid.UUID `json:"i"`
}

// Resolved is everything a public share route needs, answered in one query
// from the token's hash. State says whether the link serves; the share id is
// present in every state so a refusal can still be attributed in the log.
//
// Mime is the uploader's claim -- a key into the preview allowlist, never a
// content type to serve.
type Resolved struct {
	ShareID       uuid.UUID
	NodeID        uuid.UUID
	ParentID      *uuid.UUID
	Name          string
	Size          int64
	Mime          string
	ObjectKey     string
	PasswordHash  *string
	ExpiresAt     *time.Time
	MaxDownloads  *int
	DownloadCount int
	State         State
}

// Guest is one share_guest_sessions row: the session behind a recipient's
// cookie. DownloadedAt is the once-per-session mark the cap counts on.
type Guest struct {
	ID           uuid.UUID
	ShareID      uuid.UUID
	DownloadedAt *time.Time
	ExpiresAt    time.Time
}
