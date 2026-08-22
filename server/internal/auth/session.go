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

// SessionInfo is one row of a user's session list. It is the audit view of
// auth_sessions -- everything the row records about where a login came from,
// and nothing that could impersonate it: the token hash is never read back out.
type SessionInfo struct {
	ID         uuid.UUID
	CreatedAt  time.Time
	LastSeenAt *time.Time
	IP         *string
	UserAgent  *string
}

// ListSessions returns the user's live sessions, newest first. Expired rows are
// left out: they cannot be used, so listing them would only ask somebody to
// revoke what is already dead.
func ListSessions(ctx context.Context, q Querier, userID uuid.UUID) ([]SessionInfo, error) {
	// host() renders inet as a bare address, without the /32 a text cast adds.
	const sql = `
		SELECT id, created_at, last_seen_at, host(ip), user_agent
		  FROM auth_sessions
		 WHERE user_id = $1 AND expires_at > now()
		 ORDER BY created_at DESC, id`

	rows, err := q.Query(ctx, sql, userID)
	if err != nil {
		return nil, fmt.Errorf("auth: listing sessions: %w", err)
	}
	defer rows.Close()

	var out []SessionInfo
	for rows.Next() {
		var s SessionInfo
		if err := rows.Scan(&s.ID, &s.CreatedAt, &s.LastSeenAt, &s.IP, &s.UserAgent); err != nil {
			return nil, fmt.Errorf("auth: listing sessions: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: listing sessions: %w", err)
	}
	return out, nil
}

// DeleteSessionByID revokes one session by id, scoped to its owner, and reports
// whether a row went. The owner predicate is the authorization: without it the
// endpoint would revoke anybody's session for anybody who could guess an id.
func DeleteSessionByID(ctx context.Context, q Querier, userID, id uuid.UUID) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM auth_sessions WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return false, fmt.Errorf("auth: deleting session: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteUserSessions revokes every session a user has, the caller's included.
// It is "sign out everywhere" and nothing else: the one caller that keeps a
// session -- a password change -- does its own revoke inside the transaction
// that sets the new hash, because a revoke that can fail after the password is
// already stored fails open.
func DeleteUserSessions(ctx context.Context, q Querier, userID uuid.UUID) error {
	if _, err := q.Exec(ctx, `DELETE FROM auth_sessions WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("auth: deleting sessions: %w", err)
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
