package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the read/write surface these helpers need. Both *pgxpool.Pool and
// pgx.Tx satisfy it, so a caller can run a helper standalone or inside its own
// transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Email token purposes, matching the email_tokens.purpose check constraint.
const (
	PurposeVerify = "verify"
	PurposeReset  = "reset"
)

// EmailTokenTTL is how long a verification link stays usable. Nothing external
// fixes the number; two days is long enough to survive a weekend and short
// enough that a link found in an old mailbox is dead.
const EmailTokenTTL = 48 * time.Hour

// RootFolderName is the name of the folder created for every user at signup.
const RootFolderName = "My Drive"

// ErrInvalidToken covers every reason a token cannot be redeemed -- unknown,
// expired, already used, wrong purpose. Callers must not distinguish them: the
// client is told one thing, "this link is invalid or has expired".
var ErrInvalidToken = errors.New("auth: token is invalid, expired or already used")

// Account is a user plus the id of their root folder.
type Account struct {
	ID              uuid.UUID
	Email           string
	DisplayName     string
	PasswordHash    string
	RootID          uuid.UUID
	EmailVerifiedAt *time.Time
}

// Verified reports whether the account finished email verification.
func (a *Account) Verified() bool { return a != nil && a.EmailVerifiedAt != nil }

// CreateUser inserts a user and their root folder in one transaction: a user
// without a root folder has nowhere to put anything, so the two rows are never
// separately visible. A partial unique index on (owner_id) WHERE parent_id IS
// NULL enforces the one-root-per-user half of that.
//
// An address that already exists is not an error and returns (nil, false, nil).
// The caller answers exactly as it would for a fresh signup -- the API must not
// reveal which addresses have accounts -- and deliberately gets no handle on
// the existing account.
func CreateUser(ctx context.Context, pool *pgxpool.Pool, email, passwordHash, displayName string) (*Account, bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("auth: creating user: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insertUser = `
		INSERT INTO users (id, email, password_hash, display_name)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO NOTHING
		RETURNING id`

	var id uuid.UUID
	err = tx.QueryRow(ctx, insertUser, uuid.New(), email, passwordHash, displayName).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("auth: creating user: %w", err)
	}

	rootID := uuid.New()
	const insertRoot = `
		INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		VALUES ($1, $2, NULL, 'folder', $3)`
	if _, err := tx.Exec(ctx, insertRoot, rootID, id, RootFolderName); err != nil {
		return nil, false, fmt.Errorf("auth: creating root folder: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("auth: creating user: %w", err)
	}
	return &Account{ID: id, Email: email, DisplayName: displayName, PasswordHash: passwordHash, RootID: rootID}, true, nil
}

// FindUserByEmail returns the account for email, or (nil, nil) if there is
// none. The column is citext, so the match is case-insensitive.
func FindUserByEmail(ctx context.Context, q Querier, email string) (*Account, error) {
	const sql = `
		SELECT u.id, u.email, u.display_name, u.password_hash, u.email_verified_at, root.id
		  FROM users u
		  LEFT JOIN nodes root ON root.owner_id = u.id AND root.parent_id IS NULL
		 WHERE u.email = $1`

	var a Account
	var rootID *uuid.UUID
	err := q.QueryRow(ctx, sql, email).Scan(&a.ID, &a.Email, &a.DisplayName, &a.PasswordHash, &a.EmailVerifiedAt, &rootID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: looking up %s: %w", email, err)
	}
	if rootID != nil {
		a.RootID = *rootID
	}
	return &a, nil
}

// CreateEmailToken mints a verification or reset link token. Only its hash is
// stored; the raw value returned here is the one that goes in the email and is
// never recoverable afterwards.
func CreateEmailToken(ctx context.Context, q Querier, userID uuid.UUID, purpose string) (string, error) {
	raw, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	const sql = `
		INSERT INTO email_tokens (id, user_id, purpose, token_hash, expires_at)
		VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))`
	if _, err := q.Exec(ctx, sql, uuid.New(), userID, purpose, hash, EmailTokenTTL.Seconds()); err != nil {
		return "", fmt.Errorf("auth: creating %s token: %w", purpose, err)
	}
	return raw, nil
}

// ConsumeEmailToken redeems a token exactly once and returns whose it was.
// For a 'verify' token it also stamps users.email_verified_at, in the same
// transaction -- a burnt token that failed to verify the address would lock the
// account out permanently.
//
// The burn is a conditional UPDATE, so two simultaneous clicks on the same link
// cannot both succeed.
func ConsumeEmailToken(ctx context.Context, pool *pgxpool.Pool, raw, purpose string) (uuid.UUID, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: consuming token: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const burn = `
		UPDATE email_tokens
		   SET used_at = now()
		 WHERE token_hash = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > now()
		RETURNING user_id`

	var userID uuid.UUID
	err = tx.QueryRow(ctx, burn, HashToken(raw), purpose).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrInvalidToken
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: consuming token: %w", err)
	}

	if purpose == PurposeVerify {
		const stamp = `UPDATE users SET email_verified_at = COALESCE(email_verified_at, now()) WHERE id = $1`
		if _, err := tx.Exec(ctx, stamp, userID); err != nil {
			return uuid.Nil, fmt.Errorf("auth: marking %s verified: %w", userID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("auth: consuming token: %w", err)
	}
	return userID, nil
}
