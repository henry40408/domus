package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// User is an account. Scope is the groups the user may use; nil is
// unrestricted and always nil for admins.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
	Created  int64  `json:"created"`
	Scope    Scope  `json:"scope"`
}

// DeleteResult is the outcome of DeleteUserKeepingAnAdmin.
type DeleteResult int

const (
	UserDeleted DeleteResult = iota
	UserNotFound
	// LastAdmin means the user is the only admin left.
	LastAdmin
	// DeleteFailed means the database refused; nothing was changed.
	DeleteFailed
)

// ErrAlreadySetUp is returned by CreateFirstAdmin when a user exists.
var ErrAlreadySetUp = errors.New("a user already exists")

// scanUser reads the columns id, username, is_admin, created, scope and then
// any extra destinations.
func scanUser(row rowScanner, extra ...any) (User, error) {
	var user User
	var scope sql.NullString
	dest := append([]any{&user.ID, &user.Username, &user.IsAdmin, &user.Created, &scope}, extra...)
	if err := row.Scan(dest...); err != nil {
		return User{}, err
	}
	user.Scope = parseScope(scope)
	return user, nil
}

// UserCount is the number of users.
func (s *Store) UserCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

// CreateUser adds a user and returns its id. It fails if the username is
// taken (names are case-insensitive for ASCII letters only).
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, isAdmin bool) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO users (username, password_hash, is_admin, created) VALUES (?1, ?2, ?3, ?4)",
		username, passwordHash, isAdmin, s.now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CreateFirstAdmin creates the first user as an admin, only while no user
// exists (ErrAlreadySetUp otherwise).
func (s *Store) CreateFirstAdmin(ctx context.Context, username, passwordHash string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, is_admin, created)
             SELECT ?1, ?2, 1, ?3 WHERE NOT EXISTS (SELECT 1 FROM users)`,
		username, passwordHash, s.now())
	if err != nil {
		return 0, err
	}
	if err := affected(res, nil); err != nil {
		return 0, ErrAlreadySetUp
	}
	return res.LastInsertId()
}

// UserLogin is the user and their password hash, for logging in.
func (s *Store) UserLogin(ctx context.Context, username string) (User, string, error) {
	var hash string
	user, err := scanUser(s.db.QueryRowContext(ctx,
		"SELECT id, username, is_admin, created, scope, password_hash FROM users WHERE username = ?1",
		username), &hash)
	return user, hash, notFound(err)
}

// UserHash is the user's password hash.
func (s *Store) UserHash(ctx context.Context, id int64) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id = ?1", id).Scan(&hash)
	return hash, notFound(err)
}

// ListUsers lists all users by id.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	return queryAll(ctx, s.db, func(row rowScanner) (User, error) { return scanUser(row) },
		"SELECT id, username, is_admin, created, scope FROM users ORDER BY id")
}

// SetPassword replaces the user's password hash (ErrNotFound for an unknown
// user).
func (s *Store) SetPassword(ctx context.Context, id int64, passwordHash string) error {
	return affected(s.db.ExecContext(ctx, "UPDATE users SET password_hash = ?2 WHERE id = ?1", id, passwordHash))
}

// SetUserScope sets the user's group limit; nil removes it.
func (s *Store) SetUserScope(ctx context.Context, id int64, scope Scope) error {
	encoded, err := encodeScope(scope)
	if err != nil {
		return err
	}
	return affected(s.db.ExecContext(ctx, "UPDATE users SET scope = ?2 WHERE id = ?1", id, encoded))
}

// SetAdmin sets the user's role. Demoting the last admin is refused
// (ErrNotFound) in the same statement that would do it, so two admins
// demoting each other at once cannot leave none.
func (s *Store) SetAdmin(ctx context.Context, id int64, isAdmin bool) error {
	return affected(s.db.ExecContext(ctx,
		`UPDATE users SET is_admin = ?2 WHERE id = ?1
               AND (?2 = 1 OR is_admin = 0 OR (SELECT COUNT(*) FROM users WHERE is_admin = 1) > 1)`,
		id, isAdmin))
}

// DeleteUserKeepingAnAdmin removes the user with their sessions, tokens and
// registered apps, unless they are the last admin. The check and the delete
// are one statement, so two admins deleting each other at once cannot leave
// none; the clean-up is in the same transaction, so a token never outlives
// its owner (it would lose the owner's group limit).
func (s *Store) DeleteUserKeepingAnAdmin(ctx context.Context, id int64) (DeleteResult, error) {
	const deleteUser = `DELETE FROM users WHERE id = ?1
               AND (is_admin = 0 OR (SELECT COUNT(*) FROM users WHERE is_admin = 1) > 1)`
	result := DeleteFailed
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, deleteUser, id)
		if err != nil {
			return err
		}
		gone, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if gone == 0 {
			result, err = missingUserResult(ctx, tx, id)
			return err
		}
		err = execAll(ctx, tx, []string{
			"DELETE FROM sessions WHERE user_id = ?1",
			"DELETE FROM tokens WHERE user_id = ?1",
			"DELETE FROM refresh_tokens WHERE user_id = ?1",
			"DELETE FROM registrations WHERE user_id = ?1",
		}, id)
		if err != nil {
			return fmt.Errorf("delete user data: %w", err)
		}
		result = UserDeleted
		return nil
	})
	if err != nil {
		return DeleteFailed, err
	}
	return result, nil
}

// missingUserResult tells why nothing was deleted: the user is gone already,
// or is the last admin.
func missingUserResult(ctx context.Context, tx *sql.Tx, id int64) (DeleteResult, error) {
	var one int64
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM users WHERE id = ?1", id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return UserNotFound, nil
	}
	if err != nil {
		return DeleteFailed, err
	}
	return LastAdmin, nil
}

// UserByID is the user with this id.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	user, err := scanUser(s.db.QueryRowContext(ctx,
		"SELECT id, username, is_admin, created, scope FROM users WHERE id = ?1", id))
	return user, notFound(err)
}
