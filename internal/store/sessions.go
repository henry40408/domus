package store

import (
	"context"
	"database/sql"
)

// SessionInfo describes a live session to its owner.
type SessionInfo struct {
	// ID is the public id (the row id); the token hash never leaves the store.
	ID        int64  `json:"id"`
	Created   int64  `json:"created"`
	LastSeen  int64  `json:"last_seen"`
	UserAgent string `json:"user_agent"`
	Current   bool   `json:"current"`
}

func truncateUserAgent(userAgent string) string {
	runes := []rune(userAgent)
	if len(runes) <= userAgentMax {
		return userAgent
	}
	return string(runes[:userAgentMax])
}

// CreateSession stores a password-login session. It first sweeps expired and
// idle ones; a failed sweep must not block the login.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, userAgent string) error {
	now := s.now()
	_, _ = s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires < ?1 OR last_seen < ?2",
		now, now-SessionIdleSecs)
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO sessions (token_hash, expires, user_id, created, last_seen, user_agent)
             VALUES (?1, ?2, ?3, ?4, ?4, ?5)`,
		tokenHash, now+SessionTTLSecs, userID, now, truncateUserAgent(userAgent))
	return err
}

// SessionUser is the session's user, unless it expired or sat idle for too
// long. It counts as activity, refreshing last_seen at most once a minute.
func (s *Store) SessionUser(ctx context.Context, tokenHash string) (User, error) {
	now := s.now()
	user, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.is_admin, u.created, u.scope FROM sessions s
             JOIN users u ON u.id = s.user_id
             WHERE s.token_hash = ?1 AND s.expires > ?2 AND s.last_seen > ?3
               AND (s.refresh_id IS NULL OR EXISTS (SELECT 1 FROM refresh_tokens r WHERE r.id = s.refresh_id))`,
		tokenHash, now, now-SessionIdleSecs))
	if err != nil {
		return User{}, notFound(err)
	}
	// Best effort: a missed touch only makes the idle clock a minute stale.
	_, _ = s.db.ExecContext(ctx,
		"UPDATE sessions SET last_seen = ?2 WHERE token_hash = ?1 AND last_seen < ?3",
		tokenHash, now, now-sessionTouchSecs)
	return user, nil
}

// SessionValid reports whether the session is still alive, without counting
// as activity (long-lived streams use this so an open tab does not defeat the
// idle timeout).
func (s *Store) SessionValid(ctx context.Context, tokenHash string) (bool, error) {
	now := s.now()
	return s.exists(ctx, `SELECT 1 FROM sessions s WHERE s.token_hash = ?1 AND s.expires > ?2 AND s.last_seen > ?3
               AND (s.refresh_id IS NULL OR EXISTS (SELECT 1 FROM refresh_tokens r WHERE r.id = s.refresh_id))`,
		tokenHash, now, now-SessionIdleSecs)
}

// ListSessions lists the user's live sessions, newest first; currentHash
// marks the caller's own.
func (s *Store) ListSessions(ctx context.Context, userID int64, currentHash string) ([]SessionInfo, error) {
	now := s.now()
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.rowid, s.created, s.last_seen, s.user_agent, s.token_hash = ?2 FROM sessions s
             WHERE s.user_id = ?1 AND s.expires > ?3 AND s.last_seen > ?4
               AND (s.refresh_id IS NULL OR EXISTS (SELECT 1 FROM refresh_tokens r WHERE r.id = s.refresh_id))
             ORDER BY s.created DESC, s.rowid DESC`,
		userID, currentHash, now, now-SessionIdleSecs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var sessions []SessionInfo
	for rows.Next() {
		var info SessionInfo
		var userAgent sql.NullString
		if err := rows.Scan(&info.ID, &info.Created, &info.LastSeen, &userAgent, &info.Current); err != nil {
			return nil, err
		}
		info.UserAgent = userAgent.String
		sessions = append(sessions, info)
	}
	return sessions, rows.Err()
}

// DeleteSessionByID ends one of the user's own sessions (ErrNotFound when it
// is not theirs or does not exist).
func (s *Store) DeleteSessionByID(ctx context.Context, userID, id int64) error {
	return affected(s.db.ExecContext(ctx, "DELETE FROM sessions WHERE rowid = ?1 AND user_id = ?2", id, userID))
}

// DeleteOtherSessions ends every session of the user except the one with
// keepHash and returns how many ended.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID int64, keepHash string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?1 AND token_hash <> ?2", userID, keepHash)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// NewAppLoginSession is the session an app login starts.
type NewAppLoginSession struct {
	TokenHash string
	UserID    int64
	RefreshID int64
	UserAgent string
}

// CreateAppSession starts a session for the app login refreshID, replacing
// the one it already had. It ends with that login and is short-lived;
// sensitive actions in it need a password re-entry.
func (s *Store) CreateAppSession(ctx context.Context, session NewAppLoginSession) error {
	now := s.now()
	_, _ = s.db.ExecContext(ctx, "DELETE FROM sessions WHERE refresh_id = ?1", session.RefreshID)
	// One statement, so a failure can never leave a plain password-style
	// session behind.
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO sessions (token_hash, expires, user_id, created, last_seen, user_agent, refresh_id)
             VALUES (?1, ?2, ?3, ?4, ?4, ?5, ?6)`,
		session.TokenHash, now+AppSessionTTLSecs, session.UserID, now, truncateUserAgent(session.UserAgent), session.RefreshID)
	return err
}

// SessionNeedsReauth reports whether the session came from an app token and
// has not had its password re-entered lately.
func (s *Store) SessionNeedsReauth(ctx context.Context, tokenHash string) (bool, error) {
	return s.exists(ctx,
		`SELECT 1 FROM sessions WHERE token_hash = ?1 AND refresh_id IS NOT NULL
               AND (elevated_until IS NULL OR elevated_until <= ?2)`,
		tokenHash, s.now())
}

// SessionElevate unlocks sensitive actions in the session for ReauthSecs.
func (s *Store) SessionElevate(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET elevated_until = ?2 WHERE token_hash = ?1",
		tokenHash, s.now()+ReauthSecs)
	return err
}

// DeleteSession ends the session.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?1", tokenHash)
	return err
}

// DeleteUserSessions ends every session of the user (after a password
// change).
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?1", userID)
	return err
}
