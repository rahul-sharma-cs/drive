package auth

// External identities: the rule that turns a verified claim from an identity
// provider into a Drive account, and the three ways an account screen touches
// one afterwards.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProviderGoogle is the only value user_identities.provider accepts today; the
// column's check constraint is the seam a second provider would widen.
const ProviderGoogle = "google"

// Identity is one external account linked to a Drive account.
type Identity struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Provider    string
	Subject     string
	EmailAtLink string
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

var (
	// ErrSignupsClosed means the claim belongs to nobody here and this
	// deployment is not accepting new accounts.
	ErrSignupsClosed = errors.New("auth: this deployment is not accepting new accounts")
	// ErrIdentityRace means two attempts both lost: the inserts found a
	// conflict and the re-run found nothing to sign in as. A race resolves on
	// the second attempt, so this is not one -- it is a state the code does not
	// model, and the caller refuses rather than guessing.
	ErrIdentityRace = errors.New("auth: could not resolve the identity after a retry")
	// ErrIdentityAlreadyLinked means the account already holds an identity from
	// this provider under a different subject. One Drive account links one
	// account per provider, so this is a permanent refusal and not a race: no
	// number of retries makes the unique index on (user_id, provider) admit a
	// second row.
	ErrIdentityAlreadyLinked = errors.New("auth: that account is already linked to a different account at this provider")
	// ErrIdentityUnverified means an account reached through an identity has no
	// email_verified_at. Every path that creates or links one sets it, so this
	// is an invariant violation and not a user-visible state.
	ErrIdentityUnverified = errors.New("auth: an account reached through an identity is not verified")
	// ErrLastSignInMethod means unlinking would leave the account with no way
	// to sign in at all.
	ErrLastSignInMethod = errors.New("auth: that is the account's only sign-in method")
)

// SignInWithIdentity resolves a verified provider claim to an account, linking
// or creating one if it has to, and reports whether the account is new.
//
// The order is what makes a provider-side email change a non-event and an
// email-based takeover impossible after the first link:
//
//  1. by (provider, subject) -- the provider's stable id. Whatever the claim
//     says the address is now, this is the account. Nothing about the user row
//     is rewritten from a claim.
//  2. by the address, when the subject is unknown. The provider has just proven
//     the same thing Drive's own verification mail proves, so the identity is
//     linked to the existing account and a never-clicked verification is
//     stamped. A verified account keeps its password and its display name and
//     still signs in with that password; an account this link is *activating*
//     loses both, since neither was ever the address owner's, for the reasons
//     the UPDATE documents.
//  3. otherwise a new account, its root folder and the identity, in one
//     transaction -- unless signups are closed, which is a refusal and not a
//     back door.
//
// The race is closed by re-running the lookup rather than by copying
// CreateUser: that one answers (nil, false, nil) for a taken address and
// deliberately never re-selects, because signup must learn nothing about the
// account already there. Here the caller has to end up signed in. So both
// inserts are ON CONFLICT DO NOTHING, zero rows means somebody else won, and
// the loser rolls back and runs the whole thing once more against what the
// winner committed.
func SignInWithIdentity(ctx context.Context, pool *pgxpool.Pool, provider, subject, email, displayName string, signupsOpen bool) (*Account, bool, error) {
	const attempts = 2
	for attempt := range attempts {
		acct, created, retry, err := signInAttempt(ctx, pool, provider, subject, email, displayName, signupsOpen)
		if err != nil {
			return nil, false, err
		}
		if !retry {
			return acct, created, nil
		}
		if attempt == attempts-1 {
			return nil, false, ErrIdentityRace
		}
	}
	return nil, false, ErrIdentityRace
}

// signInAttempt is one transaction's worth of the rule above. retry is true
// when an insert found a conflict, which means another caller committed the
// row we wanted and the whole lookup should be run again against it.
func signInAttempt(ctx context.Context, pool *pgxpool.Pool, provider, subject, email, displayName string, signupsOpen bool) (acct *Account, created, retry bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, false, false, fmt.Errorf("auth: signing in with %s: %w", provider, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Known subject.
	var userID uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT user_id FROM user_identities WHERE provider = $1 AND subject = $2`,
		provider, subject).Scan(&userID)
	switch {
	case err == nil:
		if _, err := tx.Exec(ctx,
			`UPDATE user_identities SET last_login_at = now() WHERE provider = $1 AND subject = $2`,
			provider, subject); err != nil {
			return nil, false, false, fmt.Errorf("auth: stamping the identity's last login: %w", err)
		}
		found, err := FindUserByID(ctx, tx, userID)
		if err != nil {
			return nil, false, false, err
		}
		if found == nil {
			// The identity's foreign key makes this impossible; if it ever
			// happens the honest answer is a refusal, not a new account.
			return nil, false, false, fmt.Errorf("auth: identity %s/%s points at no user", provider, subject)
		}
		// Every path that creates or links an identity stamps the address as
		// verified, so this holds by construction. It is asserted rather than
		// assumed because the whole flow's trust rests on it: an identity is
		// only ever made from a claim the provider vouched for.
		if !found.Verified() {
			return nil, false, false, ErrIdentityUnverified
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, false, fmt.Errorf("auth: signing in with %s: %w", provider, err)
		}
		return found, false, false, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, false, false, fmt.Errorf("auth: looking up the %s identity: %w", provider, err)
	}

	// 2. Unknown subject, known address.
	existing, err := FindUserByEmail(ctx, tx, email)
	if err != nil {
		return nil, false, false, err
	}
	if existing != nil {
		linked, err := insertIdentity(ctx, tx, existing.ID, provider, subject, email)
		if err != nil {
			return nil, false, false, err
		}
		if !linked {
			// Two different conflicts wear the same zero-rows answer, and only
			// one of them is a race. Another caller linking this subject first
			// is resolved by the re-run; this account already holding a
			// different subject from this provider is not resolved by anything,
			// and reporting it as a race would spend a second transaction to
			// arrive at the same refusal under a log reason that names the
			// wrong cause.
			var held string
			lookupErr := tx.QueryRow(ctx,
				`SELECT subject FROM user_identities WHERE user_id = $1 AND provider = $2`,
				existing.ID, provider).Scan(&held)
			if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
				return nil, false, false, fmt.Errorf("auth: looking up the account's %s identity: %w", provider, lookupErr)
			}
			if lookupErr == nil && held != subject {
				return nil, false, false, ErrIdentityAlreadyLinked
			}
			return nil, false, true, nil
		}
		// The link both activates the account and discards a password nobody
		// has ever proven.
		//
		// On an open-signup deployment anyone can sign up an address they do not
		// own. That row sits unverified, login refuses it, and the verification
		// mail goes to the real owner -- who may well press Continue with Google
		// instead. This UPDATE is the moment that squatted row becomes a live
		// account, so keeping the hash would hand the squatter a working
		// password on somebody else's Drive: one they could rotate, and use to
		// unlink the owner's identity. Password reset has no equivalent hole
		// because a reset *replaces* the hash rather than activating one.
		//
		// A verified account keeps its password: it proved the address itself
		// before any of this, so the credential is its own. Postgres evaluates
		// every SET expression against the pre-update row, which is what lets
		// one statement read email_verified_at and write all three columns.
		//
		// The display name goes the same way as the hash, and for the same
		// reason. Whatever the squatter typed at signup is attacker-chosen text
		// sitting on somebody else's account, and this is the moment the account
		// stops being theirs -- so the name the provider vouches for replaces
		// it. A verified account's name is its own and is not touched: that
		// keeps the rule in this package's doc comment intact, since the only
		// row a claim is ever written over is one nobody had proven.
		if _, err := tx.Exec(ctx, `
			UPDATE users
			   SET password_hash = CASE WHEN email_verified_at IS NULL THEN NULL ELSE password_hash END,
			       display_name  = CASE WHEN email_verified_at IS NULL THEN $2 ELSE display_name END,
			       email_verified_at = COALESCE(email_verified_at, now())
			 WHERE id = $1`, existing.ID, displayName); err != nil {
			return nil, false, false, fmt.Errorf("auth: marking %s verified: %w", existing.ID, err)
		}
		// Re-read, so the caller gets the stamped row rather than the one from
		// before the update.
		found, err := FindUserByID(ctx, tx, existing.ID)
		if err != nil {
			return nil, false, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, false, fmt.Errorf("auth: linking the %s identity: %w", provider, err)
		}
		return found, false, false, nil
	}

	// 3. Nobody here.
	if !signupsOpen {
		return nil, false, false, ErrSignupsClosed
	}

	newID := uuid.New()
	var insertedID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO users (id, email, password_hash, display_name, email_verified_at)
		VALUES ($1, $2, NULL, $3, now())
		ON CONFLICT (email) DO NOTHING
		RETURNING id`, newID, email, displayName).Scan(&insertedID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Somebody created this address between the lookup and here.
		return nil, false, true, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("auth: creating the user: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		VALUES ($1, $2, NULL, 'folder', $3)`, uuid.New(), insertedID, RootFolderName); err != nil {
		return nil, false, false, fmt.Errorf("auth: creating the root folder: %w", err)
	}

	linked, err := insertIdentity(ctx, tx, insertedID, provider, subject, email)
	if err != nil {
		return nil, false, false, err
	}
	if !linked {
		return nil, false, true, nil
	}

	found, err := FindUserByID(ctx, tx, insertedID)
	if err != nil {
		return nil, false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, false, fmt.Errorf("auth: creating the account: %w", err)
	}
	return found, true, false, nil
}

// insertIdentity writes the link and reports whether a row went in. Zero rows
// is a unique-index conflict on either index, and which index it was decides
// what the caller does -- so the caller asks, rather than assuming a race.
//
// last_login_at is stamped at insert: this sign-in is a use of the link, and an
// account screen that showed "never used" straight after signing in with it
// would be wrong on its first render. email_at_link is written once and never
// updated -- it is the audit fact of what the address was when the link was
// made, and the provider's copy of it can change afterwards.
func insertIdentity(ctx context.Context, q Querier, userID uuid.UUID, provider, subject, email string) (bool, error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO user_identities (id, user_id, provider, subject, email_at_link, last_login_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT DO NOTHING`, uuid.New(), userID, provider, subject, email)
	if err != nil {
		return false, fmt.Errorf("auth: linking the %s identity: %w", provider, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListIdentities returns a user's linked accounts, oldest first.
func ListIdentities(ctx context.Context, q Querier, userID uuid.UUID) ([]Identity, error) {
	rows, err := q.Query(ctx, `
		SELECT id, user_id, provider, subject, email_at_link::text, created_at, last_login_at
		  FROM user_identities
		 WHERE user_id = $1
		 ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("auth: listing identities: %w", err)
	}
	defer rows.Close()

	var out []Identity
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.ID, &i.UserID, &i.Provider, &i.Subject, &i.EmailAtLink, &i.CreatedAt, &i.LastLoginAt); err != nil {
			return nil, fmt.Errorf("auth: listing identities: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: listing identities: %w", err)
	}
	return out, nil
}

// DeleteIdentity unlinks one of a user's identities, and refuses when it is the
// only way into the account.
//
// The check and the delete are one transaction because the alternative is a
// user who unlinks their last identity in the window between them and can never
// sign in again -- there is no support desk here to undo that.
//
// The owner predicate is the authorization: without it the endpoint would
// unlink anybody's identity for anybody who could guess an id. A row that is
// not the caller's is reported exactly as one that does not exist.
func DeleteIdentity(ctx context.Context, pool *pgxpool.Pool, userID, id uuid.UUID) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("auth: unlinking the identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The user row is locked first, so a password being set or another identity
	// being unlinked cannot slip between the count and the delete.
	var hasPassword bool
	err = tx.QueryRow(ctx,
		`SELECT password_hash IS NOT NULL FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&hasPassword)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: unlinking the identity: %w", err)
	}

	// Whether the row is theirs is settled before the last-method rule, so an
	// id that is not theirs is a 404 rather than a 409 that would tell them
	// something about somebody else's account.
	var mine bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM user_identities WHERE id = $1 AND user_id = $2)`,
		id, userID).Scan(&mine); err != nil {
		return false, fmt.Errorf("auth: unlinking the identity: %w", err)
	}
	if !mine {
		return false, nil
	}

	var identities int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM user_identities WHERE user_id = $1`, userID).Scan(&identities); err != nil {
		return false, fmt.Errorf("auth: unlinking the identity: %w", err)
	}
	if !hasPassword && identities <= 1 {
		return false, ErrLastSignInMethod
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM user_identities WHERE id = $1 AND user_id = $2`, id, userID); err != nil {
		return false, fmt.Errorf("auth: unlinking the identity: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("auth: unlinking the identity: %w", err)
	}
	return true, nil
}
