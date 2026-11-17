// Package store holds domus's SQLite persistence: users, sessions,
// long-lived tokens, app logins, registered devices, the Hue bridge and
// groups.
//
// Most Rust store functions swallowed errors and returned an Option, bool or
// default. These return the error instead; ErrNotFound stands for "no such
// row" (the Rust None/false). Callers that want the Rust behaviour treat any
// error as that degraded value.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// ErrNotFound means the row does not exist (or does not belong to the caller).
var ErrNotFound = errors.New("not found")

const fileMaxOpenConns = 10

// Lifetimes and limits. Times are in seconds.
const (
	SessionTTLSecs = 7 * 24 * 3600
	// SessionIdleSecs: a session unused for this long is dead, even before its
	// absolute lifetime ends.
	SessionIdleSecs = 24 * 3600
	// AppSessionTTLSecs is the lifetime of a session started from an app's
	// token; the page just starts another one on the next visit, so it can be
	// short.
	AppSessionTTLSecs = 24 * 3600
	// ReauthSecs is how long a password re-entry unlocks sensitive actions in
	// an app session.
	ReauthSecs = 10 * 60

	// last_seen is refreshed at most this often, to avoid a write on every
	// request.
	sessionTouchSecs = 60
	userAgentMax     = 200
	// Refresh tokens unused for this long stop working, like Home Assistant's.
	refreshIdleSecs = 90 * 24 * 3600
	// last_used and last_seen of tokens and registrations are refreshed at
	// most this often.
	useTouchSecs = 60
	// Registered devices one user may have; the apps re-register on every
	// fresh login, so stale rows would otherwise pile up.
	maxRegistrationsPerUser = 20
)

// Store is the domus database.
type Store struct {
	db  *sql.DB
	now func() int64
}

// Open opens (creating if needed) the database file and brings its schema up
// to date. The file's permissions are the caller's business.
func Open(ctx context.Context, path string) (*Store, error) {
	// IMMEDIATE transactions take the write lock up front, so two writers wait
	// for each other (busy_timeout) instead of failing on a lock upgrade.
	pragmas := url.Values{
		"_pragma": {
			"journal_mode(WAL)",
			"foreign_keys(1)",
			"busy_timeout(5000)",
			"synchronous(FULL)",
		},
		"_txlock": {"immediate"},
	}
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + pragmas.Encode()
	db, err := openPool(ctx, dsn, fileMaxOpenConns)
	if err != nil {
		return nil, err
	}
	return initStore(ctx, db)
}

// OpenMemory opens a private in-memory database. It is limited to one
// connection, because every connection to :memory: would be a separate empty
// database.
func OpenMemory(ctx context.Context) (*Store, error) {
	db, err := openMemoryPool(ctx)
	if err != nil {
		return nil, err
	}
	return initStore(ctx, db)
}

func openMemoryPool(ctx context.Context) (*sql.DB, error) {
	pragmas := url.Values{"_pragma": {"foreign_keys(1)"}}
	return openPool(ctx, "file::memory:?"+pragmas.Encode(), 1)
}

func openPool(ctx context.Context, dsn string, maxOpenConns int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	if maxOpenConns == 1 {
		// A closed connection would take the in-memory database with it.
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	return db, nil
}

func initStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := upgradeUnversioned(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	migrations, err := loadMigrations(migrationFiles, "migrations")
	if err == nil {
		err = migrate(ctx, db, migrations)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, now: func() int64 { return time.Now().Unix() }}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping reports whether the database answers a trivial query.
func (s *Store) Ping(ctx context.Context) error {
	var one int64
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// notFound turns sql.ErrNoRows into ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// affected is nil when the statement changed a row and ErrNotFound otherwise.
func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// exists runs a query that selects 1 when the row exists.
func (s *Store) exists(ctx context.Context, query string, args ...any) (bool, error) {
	return rowExists(ctx, s.db, query, args...)
}
