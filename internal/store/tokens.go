package store

import (
	"context"
	"database/sql"
	"errors"
)

// TokenInfo describes a long-lived token to its owner. The secret is never
// kept, only its hash.
type TokenInfo struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Created int64  `json:"created"`
	// LastUsed is the Unix seconds of the last authenticated request (coarse:
	// refreshed at most once a minute).
	LastUsed *int64 `json:"last_used"`
	// Scope is the group names the token may use; nil means unrestricted.
	Scope    Scope `json:"scope"`
	ReadOnly bool  `json:"read_only"`
	// Expires is the Unix seconds after which the token stops working; nil
	// never expires.
	Expires  *int64  `json:"expires"`
	UserID   *int64  `json:"user_id"`
	Username *string `json:"username"`
}

// TokenAuth is what an authenticated token may do: its own scope narrowed by
// its owner's, and read-only or not.
type TokenAuth struct {
	Scope    Scope
	ReadOnly bool
	UserID   *int64
	// RefreshID is the app login (refresh token) this access token was minted
	// from, if any.
	RefreshID *int64
}

// NewToken is a long-lived token to store; TokenHash is the hash, the secret
// itself is never kept.
type NewToken struct {
	UserID    int64
	Name      string
	TokenHash string
	Scope     Scope
	ReadOnly  bool
	Expires   *int64
}

// NewAppToken is a short-lived access token tied to the refresh token it was
// minted from.
type NewAppToken struct {
	UserID    int64
	Name      string
	TokenHash string
	Expires   int64
	RefreshID int64
}

// CreateToken stores a token and returns its id.
func (s *Store) CreateToken(ctx context.Context, token NewToken) (int64, error) {
	scope, err := encodeScope(token.Scope)
	if err != nil {
		return 0, err
	}
	now := s.now()
	// Expired tokens never authenticate; this just keeps the table tidy.
	_, _ = s.db.ExecContext(ctx, "DELETE FROM tokens WHERE expires IS NOT NULL AND expires < ?1", now)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (name, token_hash, created, scope, user_id, read_only, expires)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)`,
		token.Name, token.TokenHash, now, scope, token.UserID, token.ReadOnly, token.Expires)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListTokens lists all tokens, or only those of userID when given.
func (s *Store) ListTokens(ctx context.Context, userID *int64) ([]TokenInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, t.name, t.created, t.last_used, t.scope, t.user_id, u.username, t.read_only, t.expires
             FROM tokens t LEFT JOIN users u ON u.id = t.user_id
             WHERE ?1 IS NULL OR t.user_id = ?1 ORDER BY t.id`,
		userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tokens []TokenInfo
	for rows.Next() {
		var info TokenInfo
		var scope sql.NullString
		if err := rows.Scan(&info.ID, &info.Name, &info.Created, &info.LastUsed, &scope,
			&info.UserID, &info.Username, &info.ReadOnly, &info.Expires); err != nil {
			return nil, err
		}
		info.Scope = parseScope(scope)
		tokens = append(tokens, info)
	}
	return tokens, rows.Err()
}

// RevokeToken revokes a token; with userID only if it belongs to that user.
// An app's access token takes its refresh token with it, so the app really is
// logged out. ErrNotFound when there was nothing to revoke.
func (s *Store) RevokeToken(ctx context.Context, id int64, userID *int64) error {
	var refreshID sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT refresh_id FROM tokens WHERE id = ?1 AND (?2 IS NULL OR user_id = ?2)",
		id, userID).Scan(&refreshID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if refreshID.Valid {
		return s.refreshDelete(ctx, refreshID.Int64)
	}
	return affected(s.db.ExecContext(ctx,
		"DELETE FROM tokens WHERE id = ?1 AND (?2 IS NULL OR user_id = ?2)", id, userID))
}

// TokenAuth authenticates a token by its hash (ErrNotFound if unknown or
// expired) and records the use. The scope is the token's own narrowed by its
// owner's, so shrinking a user's groups shrinks their tokens too.
func (s *Store) TokenAuth(ctx context.Context, tokenHash string) (TokenAuth, error) {
	auth, err := s.TokenView(ctx, tokenHash)
	if err != nil {
		return TokenAuth{}, err
	}
	// Throttled so a polling client does not write on every request. Best
	// effort: a missed update only leaves last_used stale.
	now := s.now()
	_, _ = s.db.ExecContext(ctx,
		`UPDATE tokens SET last_used = ?2
                 WHERE token_hash = ?1 AND (last_used IS NULL OR last_used < ?3)`,
		tokenHash, now, now-useTouchSecs)
	return auth, nil
}

// TokenView is like TokenAuth, but without counting as use: for long-lived
// connections that re-check their token.
func (s *Store) TokenView(ctx context.Context, tokenHash string) (TokenAuth, error) {
	var tokenScope, userScope sql.NullString
	var auth TokenAuth
	err := s.db.QueryRowContext(ctx,
		`SELECT t.scope, t.read_only, u.scope, t.user_id, t.refresh_id FROM tokens t LEFT JOIN users u ON u.id = t.user_id
             WHERE t.token_hash = ?1 AND (t.expires IS NULL OR t.expires > ?2)`,
		tokenHash, s.now()).Scan(&tokenScope, &auth.ReadOnly, &userScope, &auth.UserID, &auth.RefreshID)
	if err != nil {
		return TokenAuth{}, notFound(err)
	}
	auth.Scope = IntersectScope(parseScope(tokenScope), parseScope(userScope))
	return auth, nil
}
