package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SessionTTL is the sliding lifetime of a login session. It must stay equal to
// api.SessionTTL, which the cookie's MaxAge uses; auth_routes_test.go asserts
// they match (auth cannot import api -- api imports auth).
const SessionTTL = 30 * 24 * time.Hour

// Session is a server-side login session. The raw token that identifies it
// lives only in the client's cookie.
type Session struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	ExpiresAt time.Time
}

// CreateSession mints a session and returns the raw cookie value alongside it.
// Only sha256(raw) is stored, so a dump of auth_sessions yields no usable
// cookies.
//
// ip and userAgent are recorded so a session list could show where a session
// came from; either may be empty, which stores NULL.
func CreateSession(ctx context.Context, q Querier, userID uuid.UUID, ip, userAgent string) (string, Session, error) {
	raw, hash, err := NewToken()
	if err != nil {
		return "", Session{}, err
	}

	const sql = `
		INSERT INTO auth_sessions (id, user_id, token_hash, expires_at, ip, user_agent)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4), $5, $6)
		RETURNING expires_at`

	s := Session{ID: uuid.New(), UserID: userID}
	if err := q.QueryRow(ctx, sql, s.ID, userID, hash, SessionTTL.Seconds(), nullable(ip), nullable(userAgent)).
		Scan(&s.ExpiresAt); err != nil {
		return "", Session{}, fmt.Errorf("auth: creating session: %w", err)
	}
	return raw, s, nil
}

// DeleteSession revokes the session a raw cookie value identifies. A cookie
// that matches nothing is not an error: logout answers the same either way.
func DeleteSession(ctx context.Context, q Querier, raw string) error {
	if raw == "" {
		return nil
	}
	if _, err := q.Exec(ctx, `DELETE FROM auth_sessions WHERE token_hash = $1`, HashToken(raw)); err != nil {
		return fmt.Errorf("auth: deleting session: %w", err)
	}
	return nil
}

// nullable turns an empty string into a SQL NULL, which is what the inet and
// text columns want for "not recorded".
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
