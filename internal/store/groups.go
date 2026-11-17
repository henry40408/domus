package store

import "context"

// GroupExists reports whether the group exists.
func (s *Store) GroupExists(ctx context.Context, name string) (bool, error) {
	return s.exists(ctx, "SELECT 1 FROM groups WHERE name = ?1", name)
}

// GroupMembers lists the group's entity ids in order.
func (s *Store) GroupMembers(ctx context.Context, name string) ([]string, error) {
	return s.queryStrings(ctx, "SELECT entity_id FROM group_members WHERE group_name = ?1 ORDER BY pos", name)
}

// GroupNames lists all group names.
func (s *Store) GroupNames(ctx context.Context) ([]string, error) {
	return s.queryStrings(ctx, "SELECT name FROM groups ORDER BY name")
}

// ExposedGroupNames lists the groups also exposed as light entities.
func (s *Store) ExposedGroupNames(ctx context.Context) ([]string, error) {
	return s.queryStrings(ctx, "SELECT name FROM groups WHERE expose_light != 0 ORDER BY name")
}

func (s *Store) queryStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// GroupSet creates the group if needed and replaces its members (order
// preserved, duplicates dropped).
func (s *Store) GroupSet(ctx context.Context, name string, members []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO groups (name) VALUES (?1)", name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM group_members WHERE group_name = ?1", name); err != nil {
		return err
	}
	var pos int64
	for _, member := range members {
		res, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO group_members (group_name, entity_id, pos) VALUES (?1, ?2, ?3)",
			name, member, pos)
		if err != nil {
			return err
		}
		// A duplicate is ignored and must not leave a gap in the positions.
		inserted, err := res.RowsAffected()
		if err != nil {
			return err
		}
		pos += inserted
	}
	return tx.Commit()
}

// GroupExposed reports whether the group is also exposed as a
// light.domus_group_<name> entity.
func (s *Store) GroupExposed(ctx context.Context, name string) (bool, error) {
	var exposed int64
	err := s.db.QueryRowContext(ctx, "SELECT expose_light FROM groups WHERE name = ?1", name).Scan(&exposed)
	if err != nil {
		return false, notFound(err)
	}
	return exposed != 0, nil
}

// GroupSetExposed sets the flag (ErrNotFound if the group does not exist).
func (s *Store) GroupSetExposed(ctx context.Context, name string, exposed bool) error {
	return affected(s.db.ExecContext(ctx, "UPDATE groups SET expose_light = ?2 WHERE name = ?1", name, exposed))
}

// GroupDelete removes the group (ErrNotFound if it does not exist).
func (s *Store) GroupDelete(ctx context.Context, name string) error {
	return affected(s.db.ExecContext(ctx, "DELETE FROM groups WHERE name = ?1", name))
}
