// Package core holds the entity state model, the integration interface and
// the synthesised group entities.
package core

import (
	"maps"

	"github.com/henry40408/domus/internal/auth"
)

// Attributes are the JSON attributes of a state. Values are treated as
// immutable once stored: replace them, never edit them in place.
type Attributes = map[string]any

// Context ties a state to the call that caused it.
type Context struct {
	ID       string  `json:"id"`
	ParentID *string `json:"parent_id"`
	UserID   *string `json:"user_id"`
}

// NewContext is a context with a fresh random id.
func NewContext() Context {
	return Context{ID: auth.RandomHex(13)}
}

// State is one entity's state in the Home Assistant shape.
type State struct {
	EntityID    string     `json:"entity_id"`
	State       string     `json:"state"`
	Attributes  Attributes `json:"attributes"`
	LastChanged string     `json:"last_changed"`
	LastUpdated string     `json:"last_updated"`
	Context     Context    `json:"context"`
}

// cloneAttributes copies the map so callers cannot change a stored state. A
// nil map becomes an empty one, which also marshals as {}.
func cloneAttributes(attributes Attributes) Attributes {
	if attributes == nil {
		return Attributes{}
	}
	return maps.Clone(attributes)
}
