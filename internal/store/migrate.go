package store

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha512"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The Rust server ran these migrations with sqlx. Every existing database
// carries sqlx's bookkeeping table, so the runner below reproduces its table,
// description and checksum format exactly: both servers must accept the
// other's database.

//go:embed migrations/*.sql
var migrationFiles embed.FS

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS _sqlx_migrations (
    version BIGINT PRIMARY KEY,
    description TEXT NOT NULL,
    installed_on TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    success BOOLEAN NOT NULL,
    checksum BLOB NOT NULL,
    execution_time BIGINT NOT NULL
);
`

type migration struct {
	version     int64
	description string
	sql         string
	checksum    []byte
}

// loadMigrations reads files named <version>_<description>.sql, sorted by
// version. Other files are ignored, like sqlx does.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var migrations []migration
	for _, entry := range entries {
		m, ok, err := loadMigration(fsys, dir, entry)
		if err != nil {
			return nil, err
		}
		if ok {
			migrations = append(migrations, m)
		}
	}
	slices.SortFunc(migrations, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	return migrations, nil
}

func loadMigration(fsys fs.FS, dir string, entry fs.DirEntry) (migration, bool, error) {
	name := entry.Name()
	versionText, rest, found := strings.Cut(name, "_")
	if entry.IsDir() || !found || !strings.HasSuffix(rest, ".sql") {
		return migration{}, false, nil
	}
	version, err := strconv.ParseInt(versionText, 10, 64)
	if err != nil {
		return migration{}, false, fmt.Errorf("migration file %q: version is not an integer", name)
	}
	body, err := fs.ReadFile(fsys, dir+"/"+name)
	if err != nil {
		return migration{}, false, fmt.Errorf("read migration %q: %w", name, err)
	}
	sum := sha512.Sum384(body)
	return migration{
		version:     version,
		description: strings.ReplaceAll(strings.TrimSuffix(rest, ".sql"), "_", " "),
		sql:         string(body),
		checksum:    sum[:],
	}, true, nil
}

// migrate applies the pending migrations in version order. Like sqlx it
// refuses a database with a dirty row, with an applied migration that is not
// in the source, or whose checksum changed.
func migrate(ctx context.Context, db *sql.DB, migrations []migration) error {
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	if err := rejectDirty(ctx, db); err != nil {
		return err
	}
	applied, err := appliedChecksums(ctx, db)
	if err != nil {
		return err
	}
	if err := rejectMissing(applied, migrations); err != nil {
		return err
	}
	for _, m := range migrations {
		checksum, done := applied[m.version]
		if done && !bytes.Equal(checksum, m.checksum) {
			return fmt.Errorf("migration %d was previously applied but has been modified", m.version)
		}
		if done {
			continue
		}
		if err := apply(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func rejectDirty(ctx context.Context, db *sql.DB) error {
	var version int64
	err := db.QueryRowContext(ctx,
		"SELECT version FROM _sqlx_migrations WHERE success = false ORDER BY version LIMIT 1").Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check dirty migrations: %w", err)
	}
	return fmt.Errorf("migration %d is partially applied; fix and remove row from `_sqlx_migrations` table", version)
}

func appliedChecksums(ctx context.Context, db *sql.DB) (map[int64][]byte, error) {
	rows, err := db.QueryContext(ctx, "SELECT version, checksum FROM _sqlx_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("list applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	applied := map[int64][]byte{}
	for rows.Next() {
		var version int64
		var checksum []byte
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("list applied migrations: %w", err)
		}
		applied[version] = checksum
	}
	return applied, rows.Err()
}

func rejectMissing(applied map[int64][]byte, migrations []migration) error {
	known := map[int64]bool{}
	for _, m := range migrations {
		known[m.version] = true
	}
	for _, version := range slices.Sorted(maps.Keys(applied)) {
		if !known[version] {
			return fmt.Errorf("migration %d was previously applied but is missing in the resolved migrations", version)
		}
	}
	return nil
}

// apply runs one migration and its bookkeeping row in one transaction. As in
// sqlx the measured time can only be stored after the commit, so the row
// first says -1.
func apply(ctx context.Context, db *sql.DB, m migration) error {
	start := time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("while executing migration %d: %w", m.version, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO _sqlx_migrations ( version, description, success, checksum, execution_time )
		 VALUES ( ?1, ?2, TRUE, ?3, -1 )`,
		m.version, m.description, m.checksum); err != nil {
		return fmt.Errorf("record migration %d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", m.version, err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE _sqlx_migrations SET execution_time = ?1 WHERE version = ?2",
		time.Since(start).Nanoseconds(), m.version); err != nil {
		return fmt.Errorf("record migration %d time: %w", m.version, err)
	}
	return nil
}
