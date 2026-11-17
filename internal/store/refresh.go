package store

import (
	"context"
	"database/sql"
	"fmt"
)

// RefreshExists reports whether an app login (refresh token) still exists.
func (s *Store) RefreshExists(ctx context.Context, refreshID int64) (bool, error) {
	return s.exists(ctx, "SELECT 1 FROM refresh_tokens WHERE id = ?1", refreshID)
}

// RefreshCreate stores a refresh token and returns its id. It first sweeps
// the ones idle for too long, with the access tokens minted from them.
func (s *Store) RefreshCreate(ctx context.Context, userID int64, clientID, tokenHash string) (int64, error) {
	now := s.now()
	// A failed sweep must not block the login.
	_, _ = s.db.ExecContext(ctx,
		`DELETE FROM tokens WHERE refresh_id IN
               (SELECT id FROM refresh_tokens WHERE COALESCE(last_used, created) < ?1)`,
		now-refreshIdleSecs)
	_, _ = s.db.ExecContext(ctx, "DELETE FROM refresh_tokens WHERE COALESCE(last_used, created) < ?1",
		now-refreshIdleSecs)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO refresh_tokens (token_hash, user_id, client_id, created)
             VALUES (?1, ?2, ?3, ?4)`,
		tokenHash, userID, clientID, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RefreshLookup is the (refresh id, user id) of a live refresh token issued
// to clientID for an existing user.
func (s *Store) RefreshLookup(ctx context.Context, tokenHash, clientID string) (refreshID, userID int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT r.id, r.user_id FROM refresh_tokens r JOIN users u ON u.id = r.user_id
             WHERE r.token_hash = ?1 AND r.client_id = ?2 AND COALESCE(r.last_used, r.created) >= ?3`,
		tokenHash, clientID, s.now()-refreshIdleSecs).Scan(&refreshID, &userID)
	return refreshID, userID, notFound(err)
}

// CreateAppToken mints a short-lived access token tied to a refresh token;
// any older one stops working, so at most one stays live (of two racing
// refreshes the later one wins).
func (s *Store) CreateAppToken(ctx context.Context, token NewAppToken) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO tokens (name, token_hash, created, user_id, read_only, expires, refresh_id)
             VALUES (?1, ?2, ?3, ?4, 0, ?5, ?6)`,
		token.Name, token.TokenHash, now, token.UserID, token.Expires, token.RefreshID)
	if err != nil {
		return err
	}
	inserted, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM tokens WHERE refresh_id = ?1 AND id < ?2",
		token.RefreshID, inserted); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE refresh_tokens SET last_used = ?2 WHERE id = ?1",
		token.RefreshID, now); err != nil {
		return err
	}
	return tx.Commit()
}

// refreshDelete ends a refresh token and every access token, session and
// registration minted from it.
func (s *Store) refreshDelete(ctx context.Context, refreshID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		"DELETE FROM sessions WHERE refresh_id = ?1",
		"DELETE FROM tokens WHERE refresh_id = ?1",
		"DELETE FROM registrations WHERE refresh_id = ?1",
		"DELETE FROM refresh_tokens WHERE id = ?1",
	} {
		if _, err := tx.ExecContext(ctx, statement, refreshID); err != nil {
			return fmt.Errorf("end app login: %w", err)
		}
	}
	return tx.Commit()
}

// RefreshRevoke revokes a refresh token (and its access tokens) by its hash.
// ErrNotFound when there is no such token.
func (s *Store) RefreshRevoke(ctx context.Context, tokenHash string) error {
	var id int64
	err := s.db.QueryRowContext(ctx, "SELECT id FROM refresh_tokens WHERE token_hash = ?1", tokenHash).Scan(&id)
	if err != nil {
		return notFound(err)
	}
	return s.refreshDelete(ctx, id)
}

// AccessRevoke revokes an access token; one minted from a refresh token takes
// that refresh token with it, so the app has to log in again. ErrNotFound
// when there is no such token.
func (s *Store) AccessRevoke(ctx context.Context, accessHash string) error {
	var id int64
	var refreshID sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT id, refresh_id FROM tokens WHERE token_hash = ?1", accessHash).
		Scan(&id, &refreshID)
	if err != nil {
		return notFound(err)
	}
	if refreshID.Valid {
		return s.refreshDelete(ctx, refreshID.Int64)
	}
	_, err = s.db.ExecContext(ctx, "DELETE FROM tokens WHERE id = ?1", id)
	return err
}

// DeleteUserRefreshTokens ends all of a user's app logins (after a password
// change).
func (s *Store) DeleteUserRefreshTokens(ctx context.Context, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		"DELETE FROM sessions WHERE refresh_id IN (SELECT id FROM refresh_tokens WHERE user_id = ?1)",
		"DELETE FROM registrations WHERE refresh_id IN (SELECT id FROM refresh_tokens WHERE user_id = ?1)",
		"DELETE FROM tokens WHERE refresh_id IN (SELECT id FROM refresh_tokens WHERE user_id = ?1)",
		"DELETE FROM refresh_tokens WHERE user_id = ?1",
	} {
		if _, err := tx.ExecContext(ctx, statement, userID); err != nil {
			return fmt.Errorf("end app logins: %w", err)
		}
	}
	return tx.Commit()
}
