package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// An account reached through an identity is always verified, because every path
// that creates or links one stamps the address. The code asserts it rather than
// assuming it, and this is the assertion.
//
// It is not a state a user can reach -- only a hand-edited row can -- and that
// is the point: if the invariant ever stops holding, the flow must refuse
// rather than sign somebody into an account whose address nobody proved.
func TestAnIdentityPointingAtAnUnverifiedAccountIsRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	userID := uuid.New()
	email := "identity-" + uuid.NewString() + "@drive.test"
	subject := "sub-invariant-" + uuid.NewString()

	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash, display_name, email_verified_at)
		 VALUES ($1, $2, NULL, 'Unverified', NULL)`, userID, email); err != nil {
		t.Fatalf("inserting the user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name)
		 VALUES ($1, $2, NULL, 'folder', $3)`, uuid.New(), userID, RootFolderName); err != nil {
		t.Fatalf("inserting the root folder: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_identities (id, user_id, provider, subject, email_at_link)
		 VALUES ($1, $2, $3, $4, $5)`, uuid.New(), userID, ProviderGoogle, subject, email); err != nil {
		t.Fatalf("inserting the identity: %v", err)
	}

	_, _, err := SignInWithIdentity(ctx, pool, ProviderGoogle, subject, email, "Unverified", true)
	if !errors.Is(err, ErrIdentityUnverified) {
		t.Fatalf("error = %v, want ErrIdentityUnverified", err)
	}
}

// email_at_link is written once and never rewritten: it records what the
// address was when the link was made, and the provider's copy can change.
func TestSignInWithIdentityIsIdempotentAndKeepsTheLinkAddress(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	email := "identity-" + uuid.NewString() + "@drive.test"
	subject := "sub-idem-" + uuid.NewString()

	first, created, err := SignInWithIdentity(ctx, pool, ProviderGoogle, subject, email, "First Name", true)
	if err != nil {
		t.Fatalf("the first sign-in: %v", err)
	}
	if !created {
		t.Error("the first sign-in did not report a new account")
	}

	changed := "changed-" + uuid.NewString() + "@drive.test"
	second, created, err := SignInWithIdentity(ctx, pool, ProviderGoogle, subject, changed, "Second Name", true)
	if err != nil {
		t.Fatalf("the second sign-in: %v", err)
	}
	if created {
		t.Error("the second sign-in reported a new account for a known subject")
	}
	if second.ID != first.ID {
		t.Errorf("the second sign-in landed on %s, want %s", second.ID, first.ID)
	}
	if second.DisplayName != "First Name" {
		t.Errorf("display name = %q -- a later claim rewrote the account's own name", second.DisplayName)
	}

	identities, err := ListIdentities(ctx, pool, first.ID)
	if err != nil {
		t.Fatalf("listing identities: %v", err)
	}
	if len(identities) != 1 {
		t.Fatalf("%d identities, want 1", len(identities))
	}
	if identities[0].EmailAtLink != email {
		t.Errorf("email_at_link = %q, want the address at link time %q", identities[0].EmailAtLink, email)
	}
	if identities[0].LastLoginAt == nil {
		t.Error("last_login_at is null after two sign-ins")
	}
}
