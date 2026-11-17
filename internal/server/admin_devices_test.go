package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/henry40408/domus/internal/core"
)

const devicesTestPath = "/api/domus/devices/test"

// bridgeIntegration owns the hue_ lights and scenes, offers the room "Study",
// and fails every call for ids ending in "bad".
type bridgeIntegration struct{ core.BaseIntegration }

func (bridgeIntegration) Owns(id string) bool {
	return strings.HasPrefix(id, "light.hue_") || strings.HasPrefix(id, "scene.hue_")
}

func (bridgeIntegration) LightSets() []core.LightSet {
	return []core.LightSet{{Name: "Study", Kind: "room", EntityIDs: []string{"light.hue_a", "light.hue_b"}}}
}

func (bridgeIntegration) CallLight(_ context.Context, id string, _ core.LightAction) error {
	if strings.HasSuffix(id, "bad") {
		return errors.New("boom")
	}
	return nil
}

func (bridgeIntegration) ActivateScene(context.Context, string) error { return nil }

func (a *testApp) useBridge(ids ...string) {
	a.app.core.SetIntegration("bridge", bridgeIntegration{})
	for _, id := range ids {
		a.app.core.SetState(id, "off", nil)
	}
}

// limitedFixture is the admin, the member bob and two groups, G1 and G2, each
// holding one light.
type limitedFixture struct {
	app    *testApp
	admin  string
	bob    string
	bobURI string
}

func newLimitedFixture(t *testing.T) limitedFixture {
	t.Helper()
	app := newTestApp(t)
	admin, bob := app.adminAndMember(t)
	app.useBridge("light.hue_a", "light.hue_b")
	app.setGroup(t, "G1", "light.hue_a")
	app.setGroup(t, "G2", "light.hue_b")
	return limitedFixture{app: app, admin: admin, bob: bob, bobURI: app.bobURI(t)}
}

func (f limitedFixture) limitBobToG1(t *testing.T) {
	t.Helper()
	equal(t, f.app.adminStatus(t, http.MethodPut, f.bobURI, f.admin, `{"scope":["G1"]}`), 200)
}

func (f limitedFixture) testDevice(t *testing.T, cookie, id string) int {
	t.Helper()
	body := `{"entity_id":"` + id + `"}`
	return f.app.adminStatus(t, http.MethodPost, devicesTestPath, cookie, body)
}

func TestLightsListingShowsHueStates(t *testing.T) {
	app := newTestApp(t)
	cookie := app.setupAdmin(t)
	app.app.core.SetState("light.hue_a", "on", map[string]any{"friendly_name": "Desk"})
	code, lights := app.adminJSON(t, http.MethodGet, "/api/domus/lights", cookie, "")
	equal(t, code, 200)
	equal(t, lights, []any{map[string]any{
		"entity_id": "light.hue_a", "name": "Desk", "state": "on", "brightness": nil,
	}})
}

func TestALimitedUserSeesAndTestsOnlyTheirGroups(t *testing.T) {
	f := newLimitedFixture(t)
	f.limitBobToG1(t)
	_, groups := f.app.adminJSON(t, http.MethodGet, "/api/domus/groups", f.bob, "")
	equal(t, len(asList(t, groups)), 1)
	equal(t, asMap(t, asList(t, groups)[0])["name"], "G1")
	_, lights := f.app.adminJSON(t, http.MethodGet, "/api/domus/lights", f.bob, "")
	equal(t, entityIDsOfList(t, lights), []string{"light.hue_a"})
	equal(t, f.testDevice(t, f.bob, "light.hue_a"), 200)
	equal(t, f.testDevice(t, f.bob, "light.hue_b"), 404)
	equal(t, f.testDevice(t, f.admin, "light.hue_b"), 200)
}

func TestALimitedUserSeesARoomOnlyWhenEveryLightIsTheirs(t *testing.T) {
	f := newLimitedFixture(t)
	f.limitBobToG1(t)
	_, rooms := f.app.adminJSON(t, http.MethodGet, "/api/domus/rooms", f.bob, "")
	equal(t, rooms, []any{})
	_, rooms = f.app.adminJSON(t, http.MethodGet, "/api/domus/rooms", f.admin, "")
	equal(t, len(asList(t, rooms)), 1)

	both := `{"scope":["G1","G2"]}`
	equal(t, f.app.adminStatus(t, http.MethodPut, f.bobURI, f.admin, both), 200)
	_, rooms = f.app.adminJSON(t, http.MethodGet, "/api/domus/rooms", f.bob, "")
	equal(t, len(asList(t, rooms)), 1)
}

func TestDeviceTestDrivesEveryLightItIsGiven(t *testing.T) {
	app := newTestApp(t)
	cookie := app.setupAdmin(t)
	app.useBridge("light.hue_a", "light.hue_b")
	body := `{"entity_ids":["light.hue_a","light.hue_b"],"on":true}`
	equal(t, app.adminStatus(t, http.MethodPost, devicesTestPath, cookie, body), 200)
	for _, id := range []string{"light.hue_a", "light.hue_b"} {
		state, ok := app.app.core.GetState(id)
		equal(t, ok, true)
		equal(t, state.State, "on")
	}
}

func TestDeviceTestEndpointControlsLightsAndScenes(t *testing.T) {
	app := newTestApp(t)
	cookie := app.setupAdmin(t)
	app.useBridge("light.hue_a", "light.hue_bad")
	app.app.core.SetState("scene.hue_s", "unknown", nil)
	post := func(body string) (int, map[string]any) {
		code, decoded := app.adminJSON(t, http.MethodPost, devicesTestPath, cookie, body)
		reply, _ := decoded.(map[string]any)
		return code, reply
	}

	code, reply := post(`{"entity_id":"light.hue_a","on":true}`)
	equal(t, code, 200)
	equal(t, reply["state"], "on")
	_, reply = post(`{"entity_id":"light.hue_a","on":false}`)
	equal(t, reply["state"], "off")
	code, reply = post(`{"entity_id":"scene.hue_s"}`)
	equal(t, code, 200)
	equal(t, strings.Contains(reply["state"].(string), "T"), true)
	code, _ = post(`{"entity_id":"light.hue_bad"}`)
	equal(t, code, 502)
	code, _ = post(`{"entity_id":"light.ghost"}`)
	equal(t, code, 404)
	code, _ = post(`{"entity_id":"switch.x"}`)
	equal(t, code, 400)

	// color and transition use the Home Assistant field names and are validated
	code, reply = post(`{"entity_id":"light.hue_a","color_temp":300,"transition":2}`)
	equal(t, code, 200)
	equal(t, reply["state"], "on")
	code, _ = post(`{"entity_id":"light.hue_a","xy_color":[2,0]}`)
	equal(t, code, 400)
	// several lights at once, and the sets that can be driven together
	code, reply = post(`{"entity_ids":["light.hue_a","light.hue_bad"],"on":true}`)
	equal(t, code, 200)
	equal(t, reply["state"], "on")
	code, _ = post(`{}`)
	equal(t, code, 400)
	_, rooms := app.adminJSON(t, http.MethodGet, "/api/domus/rooms", cookie, "")
	study := asMap(t, asList(t, rooms)[0])
	equal(t, study["name"], "Study")
	equal(t, study["entity_ids"], []any{"light.hue_a", "light.hue_b"})

	// /lights carries color details only for lights that report them
	app.app.core.SetState("light.hue_a", "on", map[string]any{
		"color_mode": "color_temp", "color_temp": 300.0,
		"supported_color_modes": []any{"brightness", "color_temp"},
	})
	_, lights := app.adminJSON(t, http.MethodGet, "/api/domus/lights", cookie, "")
	for _, item := range asList(t, lights) {
		light := asMap(t, item)
		if light["entity_id"] != "light.hue_a" {
			continue
		}
		equal(t, light["color_temp"], 300.0)
		equal(t, light["supported_color_modes"], []any{"brightness", "color_temp"})
		_, hasXY := light["xy_color"]
		equal(t, hasXY, false)
	}

	equal(t, app.adminStatus(t, http.MethodPost, devicesTestPath, "", `{"entity_id":"light.hue_a"}`), 401)
}
