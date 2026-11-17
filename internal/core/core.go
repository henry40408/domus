package core

import (
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/henry40408/domus/internal/store"
	"github.com/henry40408/domus/internal/timefmt"
)

// Core is the state machine: the entity states, the registered integrations
// and the change feed.
type Core struct {
	store  *store.Store
	logger *slog.Logger
	hub    *Hub
	now    func() time.Time

	statesMu sync.RWMutex
	states   map[string]State

	integrationsMu sync.RWMutex
	integrations   map[string]Integration
}

// New is an empty core. A nil logger means slog.Default().
func New(st *store.Store, logger *slog.Logger) *Core {
	if logger == nil {
		logger = slog.Default()
	}
	return &Core{
		store:        st,
		logger:       logger,
		hub:          NewHub(),
		now:          time.Now,
		states:       map[string]State{},
		integrations: map[string]Integration{},
	}
}

// Store is the store the core reads groups from.
func (c *Core) Store() *store.Store { return c.store }

// Subscribe receives the ids of entities as they change. A slow subscriber may
// lag and miss some; it should then treat everything as changed.
func (c *Core) Subscribe() *Subscription { return c.hub.Subscribe() }

// SetIntegration registers (or replaces) an integration under name.
func (c *Core) SetIntegration(name string, integration Integration) {
	c.integrationsMu.Lock()
	defer c.integrationsMu.Unlock()
	c.integrations[name] = integration
}

// RemoveIntegration unregisters the integration called name.
func (c *Core) RemoveIntegration(name string) {
	c.integrationsMu.Lock()
	defer c.integrationsMu.Unlock()
	delete(c.integrations, name)
}

// registeredIntegrations is a snapshot ordered by name, so callers can use the
// integrations without holding the lock (they do network I/O).
func (c *Core) registeredIntegrations() []Integration {
	c.integrationsMu.RLock()
	defer c.integrationsMu.RUnlock()
	names := slices.Sorted(maps.Keys(c.integrations))
	out := make([]Integration, 0, len(names))
	for _, name := range names {
		out = append(out, c.integrations[name])
	}
	return out
}

// SetState sets a state. LastChanged only moves when the state string changes.
// Only a new entity or a changed state or attributes is announced.
func (c *Core) SetState(entityID, state string, attributes Attributes) {
	now := timefmt.RFC3339Micros(c.now())
	attributes = cloneAttributes(attributes)
	c.statesMu.Lock()
	old, existed := c.states[entityID]
	lastChanged := now
	if existed && old.State == state {
		lastChanged = old.LastChanged
	}
	changed := !existed || old.State != state || !reflect.DeepEqual(old.Attributes, attributes)
	c.states[entityID] = State{
		EntityID:    entityID,
		State:       state,
		Attributes:  attributes,
		LastChanged: lastChanged,
		LastUpdated: now,
		Context:     NewContext(),
	}
	c.statesMu.Unlock()
	if changed {
		c.hub.Publish(entityID)
	}
}

// RemoveState forgets an entity and announces it when it existed.
func (c *Core) RemoveState(entityID string) {
	c.statesMu.Lock()
	_, existed := c.states[entityID]
	delete(c.states, entityID)
	c.statesMu.Unlock()
	if existed {
		c.hub.Publish(entityID)
	}
}

// GetState is a copy of the stored state of an entity.
func (c *Core) GetState(entityID string) (State, bool) {
	c.statesMu.RLock()
	defer c.statesMu.RUnlock()
	state, ok := c.states[entityID]
	state.Attributes = cloneAttributes(state.Attributes)
	return state, ok
}

// AllStates are the stored states ordered by entity id.
func (c *Core) AllStates() []State {
	c.statesMu.RLock()
	out := make([]State, 0, len(c.states))
	for _, state := range c.states {
		state.Attributes = cloneAttributes(state.Attributes)
		out = append(out, state)
	}
	c.statesMu.RUnlock()
	slices.SortFunc(out, func(a, b State) int { return strings.Compare(a.EntityID, b.EntityID) })
	return out
}

// EntityIDsWithPrefix lists the stored entity ids that start with prefix, in no
// particular order. Integrations use it to clean up what they own.
func (c *Core) EntityIDsWithPrefix(prefix string) []string {
	c.statesMu.RLock()
	defer c.statesMu.RUnlock()
	var out []string
	for id := range c.states {
		if strings.HasPrefix(id, prefix) {
			out = append(out, id)
		}
	}
	return out
}

// LightSets are the light sets of every integration.
func (c *Core) LightSets() []LightSet {
	var out []LightSet
	for _, integration := range c.registeredIntegrations() {
		out = append(out, integration.LightSets()...)
	}
	return out
}
