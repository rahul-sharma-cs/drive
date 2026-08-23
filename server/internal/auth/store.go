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

// ResetTokenTTL is how long a password-reset link stays usable, and it is
// deliberately far shorter than EmailTokenTTL. A verification link only proves
// an address; a reset link *is* the account, so a copy sitting in a mailbox
// somebody else later reads has to be dead within the hour rather than two days
// later.
const ResetTokenTTL = time.Hour

// tokenTTL is how long a link of this purpose lives. Anything that is not a
// reset gets the verification lifetime.
func tokenTTL(purpose string) time.Duration {
	if purpose == PurposeReset {
		return ResetTokenTTL
	}
	return EmailTokenTTL
}

// RootFolderName is the name of the folder created for every user at signup.
const RootFolderName = "My Drive"

// ErrInvalidToken covers every reason a token cannot be redeemed -- unknown,
// expired, already used, wrong purpose. Callers must not distinguish them: the
// client is told one thing, "this link is invalid or has expired".
var ErrInvalidToken = errors.New("auth: token is invalid, expired or already used")

// Account is a user plus the id of their root folder.
type Account struct {
	ID          uuid.UUID
	Email       string
	DisplayName string
	// PasswordHash is nil for an account whose only sign-in method is an
	// external identity -- users.password_hash is nullable since migration
	// 0005. It is a pointer and not a string coalesced to "" on purpose:
	// VerifyPassword("") does not answer "wrong password", it answers
	// ErrBadHash, which the login handler turns into a 500. That would be both
	// a bug and an oracle -- a 500 for an identity-only account and a 401 for
	// every other address separates the two.
	PasswordHash    *string
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
	return &Account{ID: id, Email: email, DisplayName: displayName, PasswordHash: &passwordHash, RootID: rootID}, true, nil
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
	if _, err := q.Exec(ctx, sql, uuid.New(), userID, purpose, hash, tokenTTL(purpose).Seconds()); err != nil {
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

// FindUserByID returns the account with this id, or (nil, nil) if there is
// none. It is how an authenticated handler gets at the stored password hash,
// which the request-scoped user deliberately does not carry.
func FindUserByID(ctx context.Context, q Querier, id uuid.UUID) (*Account, error) {
	const sql = `
		SELECT u.id, u.email, u.display_name, u.password_hash, u.email_verified_at, root.id
		  FROM users u
		  LEFT JOIN nodes root ON root.owner_id = u.id AND root.parent_id IS NULL
		 WHERE u.id = $1`

	var a Account
	var rootID *uuid.UUID
	err := q.QueryRow(ctx, sql, id).Scan(&a.ID, &a.Email, &a.DisplayName, &a.PasswordHash, &a.EmailVerifiedAt, &rootID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: looking up %s: %w", id, err)
	}
	if rootID != nil {
		a.RootID = *rootID
	}
	return &a, nil
}

// SetDisplayName renames the account. The caller cleans the name first --
// display names ride into mail headers.
func SetDisplayName(ctx context.Context, q Querier, userID uuid.UUID, name string) error {
	if _, err := q.Exec(ctx, `UPDATE users SET display_name = $2 WHERE id = $1`, userID, name); err != nil {
		return fmt.Errorf("auth: renaming %s: %w", userID, err)
	}
	return nil
}

// ChangePassword replaces the password of a signed-in caller, in one
// transaction. keep is the session they are on, which is the only one that
// survives.
//
// All three writes commit together or none does, and the reason is that the two
// that are not the password are what make the change mean anything:
//
//   - every other session goes, because a person changing their password after
//     a scare is asking for exactly that -- while the browser they just typed it
//     into stays signed in, since signing somebody out of the form they
//     submitted is a bug and not a security control;
//   - every live reset link is spent, because a link already sitting in a
//     mailbox is a standing offer to undo this. Whoever asked for it -- the user
//     before they remembered the old password, or an attacker while the user was
//     reading their mail -- could otherwise set the password back afterwards.
//
// Doing these after the fact and merely logging a failure would fail open: the
// password would be new and the old sessions or the old links would still work,
// with a 204 telling the user they were safe.
func ChangePassword(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, hash string, keep uuid.UUID) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: changing the password for %s: %w", userID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash); err != nil {
		return fmt.Errorf("auth: setting the password for %s: %w", userID, err)
	}

	const revokeOthers = `DELETE FROM auth_sessions WHERE user_id = $1 AND id <> $2`
	if _, err := tx.Exec(ctx, revokeOthers, userID, keep); err != nil {
		return fmt.Errorf("auth: revoking the other sessions of %s: %w", userID, err)
	}

	const spendResetLinks = `
		UPDATE email_tokens
		   SET used_at = now()
		 WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL`
	if _, err := tx.Exec(ctx, spendResetLinks, userID, PurposeReset); err != nil {
		return fmt.Errorf("auth: spending the reset links of %s: %w", userID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth: changing the password for %s: %w", userID, err)
	}
	return nil
}

// ResetPassword redeems a reset link and replaces the password, in one
// transaction. It returns whose account it was.
//
// All five writes commit together or none does, and every one of them is
// load-bearing:
//
//   - the token is burnt by conditional UPDATE, so two clicks on one link cannot
//     both land;
//   - the address is marked verified, because whoever read that mailbox has just
//     proven the same thing the verification link proves -- and an unverified
//     account that reset its password could otherwise never sign in;
//   - every other live reset link for the account is spent, so an older mail
//     (or one an attacker asked for) cannot be replayed against the new password;
//   - the login lockout for the address is cleared, because forgetting a
//     password is exactly how somebody spends that budget: guess ten times, ask
//     for a link, and then be refused for a quarter of an hour by a counter that
//     is protecting a password which no longer exists;
//   - every session goes, the caller's included: a reset is what somebody does
//     when they think another person is signed in as them.
//
// The hash is computed by the caller before this is called. That order matters:
// hashing here would let a busy Argon2 limiter burn the token and leave the
// account with its old password and a dead link.
func ResetPassword(ctx context.Context, pool *pgxpool.Pool, raw, hash string) (uuid.UUID, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: resetting password: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const burn = `
		UPDATE email_tokens
		   SET used_at = now()
		 WHERE token_hash = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > now()
		RETURNING id, user_id`

	var tokenID, userID uuid.UUID
	err = tx.QueryRow(ctx, burn, HashToken(raw), PurposeReset).Scan(&tokenID, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrInvalidToken
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: resetting password: %w", err)
	}

	const setPassword = `
		UPDATE users
		   SET password_hash = $2, email_verified_at = COALESCE(email_verified_at, now())
		 WHERE id = $1`
	if _, err := tx.Exec(ctx, setPassword, userID, hash); err != nil {
		return uuid.Nil, fmt.Errorf("auth: resetting password for %s: %w", userID, err)
	}

	const spendSiblings = `
		UPDATE email_tokens
		   SET used_at = now()
		 WHERE user_id = $1 AND purpose = $2 AND id <> $3
		   AND used_at IS NULL AND expires_at > now()`
	if _, err := tx.Exec(ctx, spendSiblings, userID, PurposeReset, tokenID); err != nil {
		return uuid.Nil, fmt.Errorf("auth: spending the other reset tokens for %s: %w", userID, err)
	}

	// The address is read back out of users rather than carried in, because the
	// caller only ever had a token. lower() matches how the key is written: the
	// API canonicalises an address before charging the budget, and throttle.key
	// is plain text compared exactly.
	const clearLoginLockout = `
		DELETE FROM throttle
		 WHERE scope = $1
		   AND key = (SELECT lower(u.email::text) FROM users u WHERE u.id = $2)`
	if _, err := tx.Exec(ctx, clearLoginLockout, ScopeLogin, userID); err != nil {
		return uuid.Nil, fmt.Errorf("auth: clearing the login lockout for %s: %w", userID, err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM auth_sessions WHERE user_id = $1`, userID); err != nil {
		return uuid.Nil, fmt.Errorf("auth: revoking the sessions of %s: %w", userID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("auth: resetting password: %w", err)
	}
	return userID, nil
}
