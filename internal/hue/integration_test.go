package hue

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/henry40408/domus/internal/core"
)

func TestSyncControlAndEventsAgainstMockBridge(t *testing.T) {
	mock := newMock()
	mock.set("light", lightJSON("aaaaaaaa-0000", "Desk", true, 40), lightJSON("bbbbbbbb-0000", "Shelf", false, 100))
	mock.setEvents(eventJSON("update", J{"id": "bbbbbbbb-0000", "type": "light", "on": J{"on": true}}))
	env := newEnv(t, mock, Timing{})

	if n := env.sync(t); n != 2 {
		t.Fatalf("got %d lights", n)
	}
	if env.state(t, "light.hue_aaaaaaaa").State != "on" || env.state(t, "light.hue_bbbbbbbb").State != "off" {
		t.Fatal("unexpected states after sync")
	}

	// control
	action := core.LightAction{On: true, Brightness: ptr(uint8(255))}
	noError(t, env.core.CallLight(ctx, "light.hue_bbbbbbbb", action))
	puts := mock.recordedPuts()
	if len(puts) != 1 || puts[0].ID != "bbbbbbbb-0000" {
		t.Fatalf("got %+v", puts)
	}
	sameJSON(t, puts[0].Body, J{"on": J{"on": true}, "dimming": J{"brightness": 100.0}})

	// events: reset to off, then the stream applies the update (-> on) and ends
	env.core.SetState("light.hue_bbbbbbbb", "off", core.Attributes{})
	if err := env.hue.streamOnce(ctx); !errors.Is(err, errEventStreamClosed) {
		t.Fatalf("got %v", err)
	}
	if env.state(t, "light.hue_bbbbbbbb").State != "on" {
		t.Fatal("the event must turn the light on")
	}

	// a light disappearing from the bridge is removed on resync
	mock.set("light", lightJSON("aaaaaaaa-0000", "Desk", true, 40))
	env.sync(t)
	if _, ok := env.core.GetState("light.hue_bbbbbbbb"); ok || env.hue.Owns("light.hue_bbbbbbbb") {
		t.Fatal("the removed light must be forgotten")
	}
}

func TestUnreachableLightsAreUnavailableUntilTheLinkReturns(t *testing.T) {
	mock := newMock()
	mock.set("light", richLightJSON("aaaa1111-0", "dev-a"))
	mock.set("zigbee_connectivity", J{"id": "z1", "type": "zigbee_connectivity", "status": "disconnected",
		"owner": J{"rid": "dev-a", "rtype": "device"}})
	mock.setEvents(eventJSON("update", J{"id": "z1", "type": "zigbee_connectivity", "status": "connected"}))
	env := newEnv(t, mock, Timing{})

	env.sync(t)
	state := env.state(t, "light.hue_aaaa1111")
	if state.State != "unavailable" || len(state.Attributes) != 1 {
		t.Fatalf("no stale brightness or color expected: %+v", state)
	}
	_ = env.hue.streamOnce(ctx)
	if env.state(t, "light.hue_aaaa1111").State != "on" {
		t.Fatal("the light must return with the link")
	}
}

func TestARoomIsDrivenThroughItsGroupedLight(t *testing.T) {
	mock := newMock()
	mock.set("light", richLightJSON("aaaa1111-0", "dev-a"), richLightJSON("bbbb2222-0", "dev-b"),
		richLightJSON("cccc3333-0", "dev-c"))
	mock.set("room", J{"id": "room1", "type": "room", "metadata": J{"name": "Study"},
		"children": []any{J{"rid": "dev-a", "rtype": "device"}, J{"rid": "dev-b", "rtype": "device"}},
		"services": []any{J{"rid": "gl-study", "rtype": "grouped_light"}}})
	env := newEnv(t, mock, Timing{})
	env.sync(t)

	sets := env.core.LightSets()
	wantIDs := []string{"light.hue_aaaa1111", "light.hue_bbbb2222"}
	if len(sets) != 1 || sets[0].Name != "Study" || !slices.Equal(sets[0].EntityIDs, wantIDs) {
		t.Fatalf("got %+v", sets)
	}

	// exactly the room's lights: one request to the grouped_light
	noError(t, env.core.CallLights(ctx, sets[0].EntityIDs, core.ActionOff()))
	puts := mock.recordedPuts()
	if len(puts) != 1 || puts[0].ID != "gl-study" {
		t.Fatalf("got %+v", puts)
	}
	sameJSON(t, puts[0].Body, J{"on": J{"on": false}})
	if env.state(t, "light.hue_bbbb2222").State != "off" {
		t.Fatal("the optimistic state must follow")
	}

	// any other combination goes light by light
	mock.clearPuts()
	two := []string{"light.hue_aaaa1111", "light.hue_cccc3333"}
	noError(t, env.core.CallLights(ctx, two, core.ActionOn()))
	if got := len(mock.recordedPuts()); got != 2 {
		t.Fatalf("got %d puts", got)
	}
}

func TestOddEventsAreIgnoredAndStructureChangesResync(t *testing.T) {
	mock := newMock()
	mock.set("light", richLightJSON("aaaa1111-0", "dev-a"))
	mock.set("zigbee_connectivity", J{"id": "z1", "type": "zigbee_connectivity", "status": "connected",
		"owner": J{"rid": "dev-a", "rtype": "device"}})
	env := newEnv(t, mock, Timing{})
	env.sync(t)

	expect := func(want bool, kind string, item J) {
		t.Helper()
		if got := env.hue.ApplyEventPayload(eventJSON(kind, item)); got != want {
			t.Fatalf("%s %v: got resync %v", kind, item, got)
		}
	}
	// updates without an id or status, or for an unknown link, change nothing
	expect(false, "update", J{"type": "zigbee_connectivity", "status": "disconnected"})
	expect(false, "update", J{"id": "z1", "type": "zigbee_connectivity"})
	expect(false, "update", J{"id": "unknown", "type": "zigbee_connectivity", "status": "disconnected"})
	if env.state(t, "light.hue_aaaa1111").State != "on" {
		t.Fatal("nothing should have changed")
	}
	// light updates without an id or for an unknown light change nothing either
	expect(false, "update", J{"type": "light", "on": J{"on": false}})
	expect(false, "update", J{"id": "unknown", "type": "light", "on": J{"on": false}})
	if env.state(t, "light.hue_aaaa1111").State != "on" {
		t.Fatal("nothing should have changed")
	}
	// unknown event kinds and resource types are skipped, even when added
	expect(false, "other", J{"type": "light"})
	expect(false, "add", J{"type": "bridge"})
	// a link going down and a link or room appearing
	expect(false, "update", J{"id": "z1", "type": "zigbee_connectivity", "status": "connectivity_issue"})
	if env.state(t, "light.hue_aaaa1111").State != "unavailable" {
		t.Fatal("the light must become unavailable")
	}
	expect(true, "add", J{"type": "zigbee_connectivity"})
	expect(true, "delete", J{"type": "zigbee_connectivity"})
	expect(true, "add", J{"type": "room"})
	expect(true, "delete", J{"type": "zone"})
	expect(false, "update", J{"type": "room"})
	if env.hue.ApplyEventPayload("not json") {
		t.Fatal("garbage must be ignored")
	}

	// no known light matches: no room request
	for _, ids := range [][]string{{"light.hue_zzzzzzzz"}, nil} {
		if handled, err := env.hue.CallLightSet(ctx, ids, core.ActionOn()); handled || err != nil {
			t.Fatalf("got %v %v", handled, err)
		}
	}
}

func TestSmartScenesSyncRecallAndEvents(t *testing.T) {
	mock := newMock()
	mock.set("room", J{"id": "room-1", "type": "room", "metadata": J{"name": "Living Room"}})
	mock.set("scene", sceneJSON("cccccccc-0000", "Relax", "room-1", "inactive"))
	mock.set("smart_scene", smartSceneJSON("dddddddd-0000", "Natural light", "room-1", "inactive"))
	env := newEnv(t, mock, Timing{})
	env.sync(t)

	id := "scene.hue_dddddddd"
	state := env.state(t, id)
	sameJSON(t, state.State, "unknown")
	sameJSON(t, state.Attributes["friendly_name"], "Living Room: Natural light")
	if !env.hue.Owns(id) {
		t.Fatal("the smart scene must be owned")
	}
	env.state(t, "scene.hue_cccccccc")

	// recall uses the smart scene action
	noError(t, env.core.ActivateScene(ctx, id))
	puts := mock.recordedPuts()
	if len(puts) != 1 || puts[0].ID != "dddddddd-0000" {
		t.Fatalf("got %+v", puts)
	}
	sameJSON(t, puts[0].Body, J{"recall": J{"action": "activate"}})
	if !strings.Contains(env.state(t, id).State, "T") {
		t.Fatal("a timestamp is expected")
	}

	// activation from the Hue app (inactive -> active) moves the stamp
	env.core.SetState(id, "unknown", core.Attributes{})
	event := eventJSON("update", J{"id": "dddddddd-0000", "type": "smart_scene", "state": "active"})
	if env.hue.ApplyEventPayload(event) {
		t.Fatal("an update must not resync")
	}
	if !strings.Contains(env.state(t, id).State, "T") {
		t.Fatal("the activation must be stamped")
	}

	// add/delete asks for a resync; removal drops the entity
	if !env.hue.ApplyEventPayload(eventJSON("delete", J{"id": "x", "type": "smart_scene"})) {
		t.Fatal("a delete must resync")
	}
	mock.set("smart_scene")
	env.sync(t)
	if _, ok := env.core.GetState(id); ok {
		t.Fatal("the removed scene must be gone")
	}
	env.state(t, "scene.hue_cccccccc")
}

func TestScenesSyncRecallAndEvents(t *testing.T) {
	mock := newMock()
	mock.set("room", J{"id": "room-1", "type": "room", "metadata": J{"name": "Living Room"}})
	mock.set("scene", sceneJSON("cccccccc-0000", "Relax", "room-1", "inactive"))
	env := newEnv(t, mock, Timing{})
	env.sync(t)

	id := "scene.hue_cccccccc"
	state := env.state(t, id)
	sameJSON(t, state.State, "unknown")
	sameJSON(t, state.Attributes["friendly_name"], "Living Room: Relax")
	if !env.hue.Owns(id) {
		t.Fatal("the scene must be owned")
	}

	// recall: sends the recall body and stamps the activation time
	noError(t, env.core.ActivateScene(ctx, id))
	puts := mock.recordedPuts()
	if len(puts) != 1 || puts[0].ID != "cccccccc-0000" {
		t.Fatalf("got %+v", puts)
	}
	sameJSON(t, puts[0].Body, J{"recall": J{"action": "active"}})
	stamp := env.state(t, id).State
	if !strings.Contains(stamp, "T") {
		t.Fatalf("timestamp expected, got %s", stamp)
	}

	// the stamp survives a resync
	env.sync(t)
	if got := env.state(t, id).State; got != stamp {
		t.Fatalf("got %s, want %s", got, stamp)
	}

	// activation from the Hue app (inactive -> static) moves the stamp; staying
	// active does not
	env.core.SetState(id, "unknown", core.Attributes{})
	event := func(active string) string {
		return eventJSON("update", J{"id": "cccccccc-0000", "type": "scene", "status": J{"active": active}})
	}
	if env.hue.ApplyEventPayload(event("static")) {
		t.Fatal("an update must not resync")
	}
	if !strings.Contains(env.state(t, id).State, "T") {
		t.Fatal("the activation must be stamped")
	}
	env.core.SetState(id, "unknown", core.Attributes{})
	env.hue.ApplyEventPayload(event("dynamic_palette"))
	if env.state(t, id).State != "unknown" {
		t.Fatal("staying active must not move the stamp")
	}
	env.hue.ApplyEventPayload(event("inactive"))
	env.hue.ApplyEventPayload(event("static"))
	if !strings.Contains(env.state(t, id).State, "T") {
		t.Fatal("a new activation must be stamped")
	}

	// add/delete of scenes asks for a resync; removal drops the entity
	if !env.hue.ApplyEventPayload(eventJSON("delete", J{"id": "x", "type": "scene"})) {
		t.Fatal("a delete must resync")
	}
	mock.set("scene")
	env.sync(t)
	if _, ok := env.core.GetState(id); ok || env.hue.Owns(id) {
		t.Fatal("the removed scene must be gone")
	}
}
