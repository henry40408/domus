package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"slices"
)

// Scope is what a token or user may touch. A nil Scope is everything; a
// non-nil one allows only the listed groups and their members, so an empty
// non-nil Scope allows nothing. JSON keeps the difference: null versus [].
type Scope []string

// parseScope reads the stored JSON column. An unreadable value must not widen
// access: it becomes an empty scope (nothing allowed).
func parseScope(raw sql.NullString) Scope {
	if !raw.Valid {
		return nil
	}
	var groups []string
	if err := json.Unmarshal([]byte(raw.String), &groups); err != nil || groups == nil {
		return Scope{}
	}
	return groups
}

// encodeScope is the stored JSON of a scope; NULL for an unrestricted one.
func encodeScope(scope Scope) (sql.NullString, error) {
	if scope == nil {
		return sql.NullString{}, nil
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode([]string(scope)); err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(bytes.TrimRight(buf.Bytes(), "\n")), Valid: true}, nil
}

// IntersectScope is the groups allowed by both scopes; an unrestricted scope
// yields to the other side.
func IntersectScope(a, b Scope) Scope {
	if a == nil {
		return slices.Clone(b)
	}
	if b == nil {
		return slices.Clone(a)
	}
	both := Scope{}
	for _, group := range a {
		if slices.Contains(b, group) {
			both = append(both, group)
		}
	}
	return both
}
