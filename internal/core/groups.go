package core

import (
	"context"
	"slices"
	"strings"

	"github.com/henry40408/domus/internal/store"
	"github.com/henry40408/domus/internal/timefmt"
)

// GroupLightPrefix is the entity id prefix of the light that stands for a whole
// group (see Store.GroupExposed).
const GroupLightPrefix = "light.domus_group_"

// GroupLightID is the entity id of the light of the group called name.
func GroupLightID(name string) string { return GroupLightPrefix + name }

// A store error reads as "nothing there": the caller sees a missing group or no
// members, never a half answer. The error is logged.

func (c *Core) groupExists(ctx context.Context, name string) bool {
	exists, err := c.store.GroupExists(ctx, name)
	if err != nil {
		c.logger.Warn("group lookup failed", "group", name, "error", err)
	}
	return exists
}

func (c *Core) groupMembers(ctx context.Context, name string) []string {
	members, err := c.store.GroupMembers(ctx, name)
	if err != nil {
		c.logger.Warn("group members lookup failed", "group", name, "error", err)
	}
	return members
}

func (c *Core) groupExposed(ctx context.Context, name string) bool {
	exposed, err := c.store.GroupExposed(ctx, name)
	if err != nil {
		c.logger.Warn("group exposure lookup failed", "group", name, "error", err)
	}
	return exposed
}

func (c *Core) exposedGroupNames(ctx context.Context) []string {
	names, err := c.store.ExposedGroupNames(ctx)
	if err != nil {
		c.logger.Warn("exposed groups lookup failed", "error", err)
	}
	return names
}

func stateWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// GroupState synthesises group.<name>: "on" if any member is on, listing the
// member ids. It reports false when the group does not exist.
func (c *Core) GroupState(ctx context.Context, name string) (State, bool) {
	if !c.groupExists(ctx, name) {
		return State{}, false
	}
	members := c.groupMembers(ctx, name)
	anyOn := false
	for _, member := range members {
		if state, ok := c.Lookup(ctx, member); ok && state.State == "on" {
			anyOn = true
			break
		}
	}
	// An exposed group lists its own all-lights switch first, so a client that
	// imports the group gets it without a second group.
	listed := []string{}
	if c.groupExposed(ctx, name) {
		listed = append(listed, GroupLightID(name))
	}
	listed = append(listed, members...)
	now := timefmt.RFC3339Micros(c.now())
	return State{
		EntityID:    "group." + name,
		State:       stateWord(anyOn),
		Attributes:  Attributes{"entity_id": listed, "friendly_name": name},
		LastChanged: now,
		LastUpdated: now,
		Context:     NewContext(),
	}, true
}

// exposedGroupOf is the name of the exposed group a light.domus_group_* id
// stands for.
func (c *Core) exposedGroupOf(ctx context.Context, entityID string) (string, bool) {
	name, ok := strings.CutPrefix(entityID, GroupLightPrefix)
	if !ok || !c.groupExposed(ctx, name) {
		return "", false
	}
	return name, true
}

// realMembers are the members that are real lights. Group lights are skipped,
// so expansion is one level deep and groups that include each other's lights
// cannot recurse.
func (c *Core) realMembers(ctx context.Context, name string) []string {
	return slices.DeleteFunc(c.groupMembers(ctx, name), func(member string) bool {
		return strings.HasPrefix(member, GroupLightPrefix)
	})
}

// groupLightState is the state of a group's light: "on" if any real member is
// on. The attributes are deliberately minimal.
func (c *Core) groupLightState(ctx context.Context, name string) State {
	anyOn := false
	for _, member := range c.realMembers(ctx, name) {
		if state, ok := c.GetState(member); ok && state.State == "on" {
			anyOn = true
			break
		}
	}
	now := timefmt.RFC3339Micros(c.now())
	return State{
		EntityID:    GroupLightID(name),
		State:       stateWord(anyOn),
		Attributes:  Attributes{"friendly_name": name},
		LastChanged: now,
		LastUpdated: now,
		Context:     NewContext(),
	}
}

// GroupLights are the states of every group exposed as a light.
func (c *Core) GroupLights(ctx context.Context) []State {
	var out []State
	for _, name := range c.exposedGroupNames(ctx) {
		out = append(out, c.groupLightState(ctx, name))
	}
	return out
}

// Lookup finds any state, including synthesised groups and group lights.
func (c *Core) Lookup(ctx context.Context, entityID string) (State, bool) {
	if name, ok := c.exposedGroupOf(ctx, entityID); ok {
		return c.groupLightState(ctx, name), true
	}
	if name, ok := strings.CutPrefix(entityID, "group."); ok {
		return c.GroupState(ctx, name)
	}
	return c.GetState(entityID)
}

// ExpandEntities replaces group.* ids by their members and passes other ids
// through. Duplicates are dropped; the first occurrence keeps its place.
func (c *Core) ExpandEntities(ctx context.Context, ids []string) []string {
	out := []string{}
	for _, id := range ids {
		expanded := []string{id}
		if name, ok := strings.CutPrefix(id, "group."); ok {
			expanded = c.groupMembers(ctx, name)
		}
		for _, entityID := range expanded {
			if !slices.Contains(out, entityID) {
				out = append(out, entityID)
			}
		}
	}
	return out
}

// Allowed reports whether a token with this scope may read or control the
// entity: the scoped groups (group.<name>), their all-lights switches and every
// member of those groups. A nil scope allows everything.
func (c *Core) Allowed(ctx context.Context, scope store.Scope, entityID string) bool {
	if scope == nil {
		return true
	}
	if name, ok := strings.CutPrefix(entityID, "group."); ok {
		return slices.Contains(scope, name)
	}
	if name, ok := strings.CutPrefix(entityID, GroupLightPrefix); ok && slices.Contains(scope, name) {
		return true
	}
	for _, group := range scope {
		if slices.Contains(c.groupMembers(ctx, group), entityID) {
			return true
		}
	}
	return false
}
