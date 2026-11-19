package hue

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/henry40408/domus/internal/core"
)

// IntegrationName is the name the integration registers under in the core.
const IntegrationName = "hue"

// roomLights is a room or zone with its lights resolved to uuids.
type roomLights struct {
	room     Room
	lightIDs []string
}

// known is what the integration knows about the bridge. It is only touched
// with Integration.mu held, and never across network I/O.
type known struct {
	// connectivity maps a zigbee_connectivity id to its device and reachability.
	connectivity  map[string]link
	rooms         []roomLights
	lights        map[string]*Light
	byEntity      map[string]string
	scenes        map[string]*Scene
	sceneByEntity map[string]string
}

// Integration mirrors a Hue bridge into the core and runs commands against it.
type Integration struct {
	client *Client
	core   *core.Core
	logger *slog.Logger
	timing Timing

	mu sync.Mutex
	in known
}

// IntegrationConfig is what NewIntegration needs.
type IntegrationConfig struct {
	Client *Client
	Core   *core.Core
	Timing Timing
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// NewIntegration is an integration that has not synced yet.
func NewIntegration(config IntegrationConfig) *Integration {
	return &Integration{
		client: config.Client,
		core:   config.Core,
		logger: cmp.Or(config.Logger, slog.Default()),
		timing: config.Timing.withDefaults(),
		in:     emptyKnown(),
	}
}

func emptyKnown() known {
	return known{
		connectivity:  map[string]link{},
		lights:        map[string]*Light{},
		byEntity:      map[string]string{},
		scenes:        map[string]*Scene{},
		sceneByEntity: map[string]string{},
	}
}

// Close releases the connections of the bridge client.
func (h *Integration) Close() { h.client.Close() }

// bridgeSnapshot is everything one sync fetched.
type bridgeSnapshot struct {
	lights []Light
	// scenes is only meaningful when hasScenes is set.
	scenes    []Scene
	hasScenes bool
	links     map[string]link
	rooms     []Room
}

// fetch reads the bridge. Only the lights are required: scenes, reachability
// and rooms are optional extras, so a failure there must not take the lights
// down with it.
func (h *Integration) fetch(ctx context.Context) (bridgeSnapshot, error) {
	lights, err := h.client.ListLights(ctx)
	if err != nil {
		return bridgeSnapshot{}, err
	}
	snapshot := bridgeSnapshot{lights: lights, links: map[string]link{}}
	if snapshot.scenes, err = h.client.ListScenes(ctx); err != nil {
		h.logger.Warn("hue: scene sync failed", "error", err)
	} else {
		snapshot.hasScenes = true
	}
	if snapshot.links, err = h.client.ListConnectivity(ctx); err != nil {
		h.logger.Warn("hue: connectivity sync failed", "error", err)
		snapshot.links = map[string]link{}
	}
	if snapshot.rooms, err = h.client.ListRooms(ctx); err != nil {
		h.logger.Warn("hue: room sync failed", "error", err)
		snapshot.rooms = nil
	}
	return snapshot, ctx.Err()
}

// Sync fetches all lights and replaces the known set: it adds, updates and
// removes states. It returns the number of lights.
func (h *Integration) Sync(ctx context.Context) (int, error) {
	snapshot, err := h.fetch(ctx)
	if err != nil {
		return 0, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.in.rooms = roomsWithLights(snapshot.rooms, snapshot.lights)
	h.in.connectivity = snapshot.links
	h.replaceLights(snapshot.lights)
	if snapshot.hasScenes {
		h.replaceScenes(snapshot.scenes)
	}
	return len(snapshot.lights), nil
}

func roomsWithLights(rooms []Room, lights []Light) []roomLights {
	owners := map[string][]string{}
	for _, light := range lights {
		if light.Owner != "" {
			owners[light.Owner] = append(owners[light.Owner], light.ID)
		}
	}
	out := make([]roomLights, 0, len(rooms))
	for _, room := range rooms {
		out = append(out, roomLights{room: room, lightIDs: room.lightIDs(owners)})
	}
	return out
}

func (h *Integration) replaceLights(lights []Light) {
	old := h.in.byEntity
	h.in.lights = map[string]*Light{}
	h.in.byEntity = map[string]string{}
	for _, light := range lights {
		light.Reachable = h.deviceReachable(light.Owner)
		h.core.SetState(light.entityID(), light.haState(), light.attributes())
		h.in.byEntity[light.entityID()] = light.ID
		h.in.lights[light.ID] = &light
	}
	for entityID := range old {
		if _, still := h.in.byEntity[entityID]; !still {
			h.core.RemoveState(entityID)
		}
	}
}

// deviceReachable is true unless the bridge says the device's link is down.
func (h *Integration) deviceReachable(device string) bool {
	if device == "" {
		return true
	}
	for _, l := range h.in.connectivity {
		if l.Device == device {
			return l.Reachable
		}
	}
	return true
}

// replaceScenes replaces the known scenes. A scene's state is its last
// activation time, so an existing state survives a resync; new scenes start as
// "unknown".
func (h *Integration) replaceScenes(scenes []Scene) {
	old := h.in.sceneByEntity
	h.in.scenes = map[string]*Scene{}
	h.in.sceneByEntity = map[string]string{}
	for _, scene := range scenes {
		entityID := scene.entityID()
		state := "unknown"
		if existing, ok := h.core.GetState(entityID); ok {
			state = existing.State
		}
		h.core.SetState(entityID, state, scene.attributes())
		h.in.sceneByEntity[entityID] = scene.ID
		h.in.scenes[scene.ID] = &scene
	}
	for entityID := range old {
		if _, still := h.in.sceneByEntity[entityID]; !still {
			h.core.RemoveState(entityID)
		}
	}
}

// ApplyEventPayload applies one SSE payload. It reports true when a full
// resync is needed (a resource was added or removed).
func (h *Integration) ApplyEventPayload(payload string) bool {
	var events []any
	if json.Unmarshal([]byte(payload), &events) != nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	resync := false
	for _, event := range events {
		kind, _ := stringAt(event, "type")
		for _, item := range arrayAt(event, "data") {
			if h.applyItemEvent(kind, item) {
				resync = true
			}
		}
	}
	return resync
}

// applyItemEvent applies one resource of an event. It reports true when the
// resource was added or removed.
func (h *Integration) applyItemEvent(kind string, item any) bool {
	itemType := resourceType(item)
	switch itemType {
	case "scene", smartSceneType, "zigbee_connectivity", "room", "zone", "light":
	default:
		return false
	}
	switch kind {
	case "add", "delete":
		return true
	case "update":
		h.applyUpdate(itemType, item)
	}
	return false
}

func (h *Integration) applyUpdate(itemType string, item any) {
	switch itemType {
	case "scene", smartSceneType:
		h.applySceneUpdate(item)
	case "zigbee_connectivity":
		h.applyConnectivityUpdate(item)
	case "light":
		h.applyLightUpdate(item)
	}
}

func (h *Integration) applyLightUpdate(item any) {
	id, ok := stringAt(item, "id")
	if !ok {
		return
	}
	light, ok := h.in.lights[id]
	if !ok {
		return
	}
	light.applyUpdate(item)
	h.core.SetState(light.entityID(), light.haState(), light.attributes())
}

// applyConnectivityUpdate flips every light of the device the link belongs to.
func (h *Integration) applyConnectivityUpdate(item any) {
	id, okID := stringAt(item, "id")
	status, okStatus := stringAt(item, "status")
	if !okID || !okStatus {
		return
	}
	l, ok := h.in.connectivity[id]
	if !ok {
		return
	}
	l.Reachable = reachableStatus(status)
	h.in.connectivity[id] = l
	for _, light := range h.in.lights {
		if light.Owner != l.Device {
			continue
		}
		light.Reachable = l.Reachable
		h.core.SetState(light.entityID(), light.haState(), light.attributes())
	}
}

// applySceneUpdate treats a scene turning active (for example from the Hue app)
// as an activation.
func (h *Integration) applySceneUpdate(item any) {
	id, ok := stringAt(item, "id")
	if !ok {
		return
	}
	scene, ok := h.in.scenes[id]
	if !ok {
		return
	}
	entityID := scene.entityID()
	if name, ok := stringAt(item, "metadata", "name"); ok {
		scene.Name = name
		if existing, ok := h.core.GetState(entityID); ok {
			h.core.SetState(entityID, existing.State, scene.attributes())
		}
	}
	active, found := sceneActive(item)
	if !found {
		return
	}
	if active && !scene.Active {
		h.core.MarkSceneActivated(entityID)
	}
	scene.Active = active
}

// Owns reports whether the entity is a light or scene of this bridge.
func (h *Integration) Owns(entityID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, isLight := h.in.byEntity[entityID]
	_, isScene := h.in.sceneByEntity[entityID]
	return isLight || isScene
}

// ActivateScene recalls a scene on the bridge.
func (h *Integration) ActivateScene(ctx context.Context, entityID string) error {
	h.mu.Lock()
	uuid, ok := h.in.sceneByEntity[entityID]
	scene := h.in.scenes[uuid]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown scene %s", entityID)
	}
	return h.client.RecallScene(ctx, uuid, scene != nil && scene.Smart)
}

// CallLight sends an action to one light.
func (h *Integration) CallLight(ctx context.Context, entityID string, action core.LightAction) error {
	h.mu.Lock()
	uuid, ok := h.in.byEntity[entityID]
	body := lightBody(action, false, false)
	if light := h.in.lights[uuid]; ok && light != nil {
		body = light.requestBody(action)
	}
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown light %s", entityID)
	}
	return h.client.SetLight(ctx, uuid, body)
}

// LightSets are the rooms and zones with the entity ids of their known lights.
func (h *Integration) LightSets() []core.LightSet {
	h.mu.Lock()
	defer h.mu.Unlock()
	sets := make([]core.LightSet, 0, len(h.in.rooms))
	for _, entry := range h.in.rooms {
		entityIDs := []string{}
		for _, id := range entry.lightIDs {
			if light, ok := h.in.lights[id]; ok {
				entityIDs = append(entityIDs, light.entityID())
			}
		}
		sets = append(sets, core.LightSet{Name: entry.room.Name, Kind: entry.room.Kind, EntityIDs: entityIDs})
	}
	return sets
}

// CallLightSet drives exactly one room or zone through its grouped_light when
// entityIDs are all of its lights; otherwise it is not handled.
func (h *Integration) CallLightSet(ctx context.Context, entityIDs []string, action core.LightAction) (bool, error) {
	target, ok := h.groupedLightFor(entityIDs)
	if !ok {
		return false, nil
	}
	return true, h.client.SetGroupedLight(ctx, target, lightBody(action, true, true))
}

// groupedLightFor is the grouped_light of the room whose lights are exactly
// these entities.
func (h *Integration) groupedLightFor(entityIDs []string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var wanted []string
	for _, entityID := range entityIDs {
		uuid, ok := h.in.byEntity[entityID]
		if !ok {
			return "", false
		}
		wanted = append(wanted, uuid)
	}
	slices.Sort(wanted)
	wanted = slices.Compact(wanted)
	if len(wanted) == 0 {
		return "", false
	}
	for _, entry := range h.in.rooms {
		if slices.Equal(entry.lightIDs, wanted) {
			return entry.room.GroupedLight, true
		}
	}
	return "", false
}
