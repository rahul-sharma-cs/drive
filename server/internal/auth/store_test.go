package auth

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rahul-sharma-cs/drive/server/internal/db"
)

// ---------------------------------------------------------------- harness ----

// testDSN is the drive-test stack's Postgres, verbatim from the committed
// .env.test. Tests never touch the dev stack on :55432, and refuse to run if
// something points them at it.
const testDSN = "postgres://drive:drive@localhost:55433/drive?sslmode=disable"

var (
	poolOnce sync.Once
	poolConn *pgxpool.Pool
	poolErr  error
)

// testPool returns the shared connection to the drive-test database, migrating
// it once if it is not already at the latest version.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		dsn := os.Getenv("DRIVE_DB_DSN")
		if dsn == "" {
			dsn = testDSN
		}
		if strings.Contains(dsn, ":55432") {
			poolErr = fmt.Errorf("DRIVE_DB_DSN points at the dev stack (%s); tests run against the drive-test stack on :55433", dsn)
			return
		}
		ctx := context.Background()
		if poolConn, poolErr = db.Connect(ctx, dsn); poolErr != nil {
			return
		}
		poolErr = db.Migrate(ctx, poolConn)
	})
	if poolErr != nil {
		t.Fatalf("drive-test database: %v (is `docker compose -p drive-test --env-file .env.test up -d` running?)", poolErr)
	}
	return poolConn
}

// testEmail returns an address no other test or run has used. Signup is
// idempotent on the address, so unique-per-case emails keep cases independent.
func testEmail(t *testing.T) string {
	t.Helper()
	return "auth-" + uuid.NewString() + "@drive.test"
}

// newTestUser signs a verified user up and returns the account.
func newTestUser(t *testing.T, pool *pgxpool.Pool, password string) *Account {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	acct, created, err := CreateUser(t.Context(), pool, testEmail(t), hash, "Test User")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if !created {
		t.Fatal("CreateUser reported an existing account for a fresh address")
	}
	return acct
}

// ------------------------------------------------------------------ tests ----

func TestCreateUserCreatesExactlyOneRootFolder(t *testing.T) {
	pool := testPool(t)
	acct := newTestUser(t, pool, "correct horse battery staple")

	var kind, name string
	var parentID *uuid.UUID
	var size *int64
	err := pool.QueryRow(t.Context(),
		`SELECT kind, name, parent_id, size FROM nodes WHERE id = $1 AND owner_id = $2`,
		acct.RootID, acct.ID,
	).Scan(&kind, &name, &parentID, &size)
	if err != nil {
		t.Fatalf("reading the root node: %v", err)
	}
	if kind != "folder" || name != "My Drive" || parentID != nil || size != nil {
		t.Errorf("root node = {kind:%q name:%q parent_id:%v size:%v}, want {folder, My Drive, nil, nil}", kind, name, parentID, size)
	}

	var roots int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM nodes WHERE owner_id = $1 AND parent_id IS NULL`, acct.ID,
	).Scan(&roots); err != nil {
		t.Fatalf("counting roots: %v", err)
	}
	if roots != 1 {
		t.Errorf("user has %d root folders, want exactly 1", roots)
	}
}

// The partial unique index is the guarantee, not the application code: a second
// root must be impossible even by direct SQL.
func TestSecondRootFolderIsRejectedByTheDatabase(t *testing.T) {
	pool := testPool(t)
	acct := newTestUser(t, pool, "correct horse battery staple")

	_, err := pool.Exec(t.Context(),
		`INSERT INTO nodes (id, owner_id, parent_id, kind, name) VALUES ($1, $2, NULL, 'folder', 'Second Drive')`,
		uuid.New(), acct.ID)
	if err == nil {
		t.Fatal("a second root folder was inserted; nodes_one_root_per_owner_idx is not enforcing")
	}
	if !strings.Contains(err.Error(), "23505") && !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("second root failed with %v, want a unique-violation", err)
	}
}

// Signing up twice on one address must not create a second user, a second root,
// or leak that the address was taken.
func TestCreateUserOnAnExistingAddressCreatesNothing(t *testing.T) {
	pool := testPool(t)
	email := testEmail(t)
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	first, created, err := CreateUser(t.Context(), pool, email, hash, "First")
	if err != nil || !created {
		t.Fatalf("first CreateUser = %v, %v, %v; want an account, true, nil", first, created, err)
	}

	second, created, err := CreateUser(t.Context(), pool, email, hash, "Second")
	if err != nil {
		t.Fatalf("second CreateUser: %v", err)
	}
	if created {
		t.Error("second CreateUser reported created = true")
	}
	if second != nil {
		t.Error("second CreateUser returned an account; the caller must not learn anything about the existing user")
	}

	var users, roots int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE email = $1`, email).Scan(&users); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM nodes WHERE owner_id = $1 AND parent_id IS NULL`, first.ID).Scan(&roots); err != nil {
		t.Fatalf("counting roots: %v", err)
	}
	if users != 1 || roots != 1 {
		t.Errorf("after a duplicate signup: %d users, %d roots; want 1 and 1", users, roots)
	}

	var displayName string
	if err := pool.QueryRow(t.Context(), `SELECT display_name FROM users WHERE email = $1`, email).Scan(&displayName); err != nil {
		t.Fatalf("reading display_name: %v", err)
	}
	if displayName != "First" {
		t.Errorf("display_name = %q; the duplicate signup overwrote the original account", displayName)
	}
}

func TestFindUserByEmailIsCaseInsensitive(t *testing.T) {
	pool := testPool(t)
	acct := newTestUser(t, pool, "correct horse battery staple")

	found, err := FindUserByEmail(t.Context(), pool, strings.ToUpper(acct.Email))
	if err != nil {
		t.Fatalf("FindUserByEmail: %v", err)
	}
	if found == nil || found.ID != acct.ID {
		t.Fatalf("FindUserByEmail(upper-case) = %v, want the account %v", found, acct.ID)
	}
	if found.RootID != acct.RootID {
		t.Errorf("root_id = %v, want %v", found.RootID, acct.RootID)
	}
	if found.EmailVerifiedAt != nil {
		t.Error("a fresh account is already verified")
	}

	missing, err := FindUserByEmail(t.Context(), pool, testEmail(t))
	if err != nil {
		t.Fatalf("FindUserByEmail(unknown): %v", err)
	}
	if missing != nil {
		t.Error("FindUserByEmail returned an account for an address that does not exist")
	}
}

func TestVerifyEmailTokenStampsTheUserAndBurnsTheToken(t *testing.T) {
	pool := testPool(t)
	acct := newTestUser(t, pool, "correct horse battery staple")

	raw, err := CreateEmailToken(t.Context(), pool, acct.ID, PurposeVerify)
	if err != nil {
		t.Fatalf("CreateEmailToken: %v", err)
	}

	// The raw token is a credential: only its hash may reach the table.
	var stored int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM email_tokens WHERE user_id = $1 AND token_hash = $2`, acct.ID, HashToken(raw),
	).Scan(&stored); err != nil {
		t.Fatalf("reading email_tokens: %v", err)
	}
	if stored != 1 {
		t.Fatalf("found %d rows for the token hash, want 1", stored)
	}

	userID, err := ConsumeEmailToken(t.Context(), pool, raw, PurposeVerify)
	if err != nil {
		t.Fatalf("ConsumeEmailToken: %v", err)
	}
	if userID != acct.ID {
		t.Errorf("token consumed for user %v, want %v", userID, acct.ID)
	}

	after, err := FindUserByEmail(t.Context(), pool, acct.Email)
	if err != nil {
		t.Fatalf("FindUserByEmail: %v", err)
	}
	if after.EmailVerifiedAt == nil {
		t.Error("email_verified_at is still NULL after consuming the token")
	}

	// A verification link is single use.
	if _, err := ConsumeEmailToken(t.Context(), pool, raw, PurposeVerify); err == nil {
		t.Error("the same verification token was accepted twice")
	}
}

func TestConsumeEmailTokenRejectsUnknownExpiredAndWrongPurposeTokens(t *testing.T) {
	pool := testPool(t)
	acct := newTestUser(t, pool, "correct horse battery staple")

	unknown, _, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if _, err := ConsumeEmailToken(t.Context(), pool, unknown, PurposeVerify); err == nil {
		t.Error("an unknown token was accepted")
	}

	expired, err := CreateEmailToken(t.Context(), pool, acct.ID, PurposeVerify)
	if err != nil {
		t.Fatalf("CreateEmailToken: %v", err)
	}
	// Time control: backdate the row rather than sleeping the test -- expiry is
	// a stored timestamp compared against now(), so a test can age it directly.
	if _, err := pool.Exec(t.Context(),
		`UPDATE email_tokens SET expires_at = now() - interval '1 minute' WHERE token_hash = $1`, HashToken(expired),
	); err != nil {
		t.Fatalf("backdating expires_at: %v", err)
	}
	if _, err := ConsumeEmailToken(t.Context(), pool, expired, PurposeVerify); err == nil {
		t.Error("an expired token was accepted")
	}

	wrongPurpose, err := CreateEmailToken(t.Context(), pool, acct.ID, PurposeVerify)
	if err != nil {
		t.Fatalf("CreateEmailToken: %v", err)
	}
	if _, err := ConsumeEmailToken(t.Context(), pool, wrongPurpose, PurposeReset); err == nil {
		t.Error("a 'verify' token was accepted as a 'reset' token")
	}
}
