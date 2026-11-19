package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// upgradeUnversioned adds the two columns that databases from before
// versioned migrations lack. The baseline migration only declares them for
// fresh databases (CREATE TABLE IF NOT EXISTS skips existing tables).
func upgradeUnversioned(ctx context.Context, db *sql.DB) error {
	upgrades := []struct{ table, column, alter string }{
		{"tokens", "last_used", "ALTER TABLE tokens ADD COLUMN last_used INTEGER"},
		{"groups", "expose_light", "ALTER TABLE groups ADD COLUMN expose_light INTEGER NOT NULL DEFAULT 0"},
	}
	for _, u := range upgrades {
		lacks, err := tableLacksColumn(ctx, db, u.table, u.column)
		if err != nil {
			return fmt.Errorf("check schema of %s: %w", u.table, err)
		}
		if !lacks {
			continue
		}
		if _, err := db.ExecContext(ctx, u.alter); err != nil {
			return fmt.Errorf("upgrade %s: %w", u.table, err)
		}
	}
	return nil
}

// tableLacksColumn is true only when the table exists but has no such column
// (a fresh database has no table yet, and the migration creates it with the
// column).
func tableLacksColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	exists, err := rowExists(ctx, db, "SELECT 1 FROM pragma_table_info(?1) LIMIT 1", table)
	if err != nil || !exists {
		return false, err
	}
	hasColumn, err := rowExists(ctx, db, "SELECT 1 FROM pragma_table_info(?1) WHERE name = ?2", table, column)
	return !hasColumn, err
}

func rowExists(ctx context.Context, db *sql.DB, query string, args ...any) (bool, error) {
	var one int64
	err := db.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
