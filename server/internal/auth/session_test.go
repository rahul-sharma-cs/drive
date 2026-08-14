package auth

import (
	"testing"
	"time"
)

func TestCreateSessionStoresOnlyTheHash(t *testing.T) {
	pool := testPool(t)
	acct := newTestUser(t, pool, "correct horse battery staple")

	raw, sess, err := CreateSession(t.Context(), pool, acct.ID, "203.0.113.7", "Go test")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if raw == "" {
		t.Fatal("CreateSession returned an empty token")
	}

	// The raw token must appear nowhere in the row: a stolen database must not
	// be a stolen set of live cookies.
	var stored []byte
	var userID any
	if err := pool.QueryRow(t.Context(),
		`SELECT token_hash, user_id FROM auth_sessions WHERE id = $1`, sess.ID,
	).Scan(&stored, &userID); err != nil {
		t.Fatalf("reading the session row: %v", err)
	}
	if string(stored) == raw {
		t.Fatal("auth_sessions.token_hash holds the raw token")
	}

	var matches int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM auth_sessions WHERE token_hash = $1`, HashToken(raw),
	).Scan(&matches); err != nil {
		t.Fatalf("looking the session up by hash: %v", err)
	}
	if matches != 1 {
		t.Errorf("lookup by sha256(token) found %d rows, want 1", matches)
	}

	if d := time.Until(sess.ExpiresAt); d < SessionTTL-time.Minute || d > SessionTTL+time.Minute {
		t.Errorf("session expires in %v, want about %v", d, SessionTTL)
	}
}

func TestDeleteSessionRemovesTheRow(t *testing.T) {
	pool := testPool(t)
	acct := newTestUser(t, pool, "correct horse battery staple")

	raw, sess, err := CreateSession(t.Context(), pool, acct.ID, "", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := DeleteSession(t.Context(), pool, raw); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	var left int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM auth_sessions WHERE id = $1`, sess.ID).Scan(&left); err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	if left != 0 {
		t.Error("the session row survived DeleteSession")
	}

	// Logging out twice, or with a cookie that was never valid, is not an
	// error -- the handler answers 204 either way.
	if err := DeleteSession(t.Context(), pool, raw); err != nil {
		t.Errorf("second DeleteSession: %v", err)
	}
}
