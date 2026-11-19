package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/core"
	"github.com/henry40408/domus/internal/store"
)

const apiToken = "secret-token"

var bg = context.Background()

type lightCall struct {
	id     string
	action core.LightAction
}

// fakeLights records light calls; ids ending in "bad" fail.
type fakeLights struct {
	core.BaseIntegration
	mu    sync.Mutex
	calls []lightCall
}

func (f *fakeLights) Owns(id string) bool {
	return strings.HasPrefix(id, "light.hue_") || strings.HasPrefix(id, "scene.hue_")
}

func (f *fakeLights) ActivateScene(_ context.Context, id string) error {
	if strings.HasSuffix(id, "bad") {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeLights) CallLight(_ context.Context, id string, action core.LightAction) error {
	if strings.HasSuffix(id, "bad") {
		return errors.New("boom")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, lightCall{id: id, action: action})
	return nil
}

func (f *fakeLights) recorded() []lightCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lightCall(nil), f.calls...)
}

type apiApp struct {
	*testApp
	fake *fakeLights
}

// newAPIApp is an app with an unrestricted token (apiToken), two off lights
// and the group Garmin holding both.
func newAPIApp(t *testing.T) *apiApp {
	t.Helper()
	app := newTestApp(t)
	fake := &fakeLights{}
	c := app.app.core
	app.addToken(t, apiToken, store.NewToken{UserID: 1, Name: "t"})
	c.SetIntegration("fake", fake)
	c.SetState("light.hue_a", "off", nil)
	c.SetState("light.hue_b", "off", nil)
	noError(t, c.Store().GroupSet(bg, "Garmin", []string{"light.hue_a", "light.hue_b"}))
	return &apiApp{testApp: app, fake: fake}
}

func noError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (a *testApp) addToken(t *testing.T, secret string, token store.NewToken) {
	t.Helper()
	token.TokenHash = auth.SHA256Hex(secret)
	_, err := a.store.CreateToken(bg, token)
	noError(t, err)
}

func (a *testApp) setGroup(t *testing.T, name string, members ...string) {
	t.Helper()
	noError(t, a.store.GroupSet(bg, name, members))
}

func (a *testApp) exposeGroup(t *testing.T, name string) {
	t.Helper()
	noError(t, a.store.GroupSetExposed(bg, name, true))
}

// call sends a JSON request and returns the status and the decoded body (nil
// when the body is not JSON).
func (a *testApp) call(t *testing.T, method, path, token, body string) (int, any) {
	t.Helper()
	headers := map[string]string{"Content-Type": "application/json"}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	rec := a.do(t, request{method: method, path: path, headers: headers, body: body})
	var decoded any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec.Code, decoded
}

func (a *testApp) apiGet(t *testing.T, path, token string) (int, any) {
	t.Helper()
	return a.call(t, http.MethodGet, path, token, "")
}

func (a *testApp) apiPost(t *testing.T, path, token, body string) (int, any) {
	t.Helper()
	return a.call(t, http.MethodPost, path, token, body)
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %#v", v)
	}
	return m
}

func asList(t *testing.T, v any) []any {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("not an array: %#v", v)
	}
	return list
}

func entityIDsOfList(t *testing.T, v any) []string {
	t.Helper()
	ids := []string{}
	for _, item := range asList(t, v) {
		ids = append(ids, asMap(t, item)["entity_id"].(string))
	}
	return ids
}

func equal(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestAuthRequired(t *testing.T) {
	app := newAPIApp(t)
	for token, want := range map[string]int{"": 401, "wrong": 401, apiToken: 200} {
		status, _ := app.apiGet(t, "/api/states/light.hue_a", token)
		equal(t, status, want)
	}
}

func TestStateShapeAnd404(t *testing.T) {
	app := newAPIApp(t)
	_, v := app.apiGet(t, "/api/states/light.hue_a", apiToken)
	state := asMap(t, v)
	equal(t, state["entity_id"], "light.hue_a")
	equal(t, state["state"], "off")
	if _, ok := asMap(t, state["context"])["id"].(string); !ok {
		t.Errorf("context id is not a string: %v", state["context"])
	}
	status, v := app.apiGet(t, "/api/states/light.nope", apiToken)
	equal(t, status, http.StatusNotFound)
	equal(t, asMap(t, v)["message"], "Entity not found.")
}

func TestStatesListIsSortedAndScoped(t *testing.T) {
	app := newAPIApp(t)
	app.app.core.SetState("light.hue_c", "off", nil)
	app.setGroup(t, "Other", "light.hue_c")
	status, v := app.apiGet(t, "/api/states", apiToken)
	equal(t, status, http.StatusOK)
	equal(t, entityIDsOfList(t, v), []string{
		"group.Garmin", "group.Other", "light.hue_a", "light.hue_b", "light.hue_c",
	})
	app.addToken(t, "scoped", store.NewToken{UserID: 1, Name: "scoped", Scope: store.Scope{"Garmin"}})
	_, v = app.apiGet(t, "/api/states", "scoped")
	equal(t, entityIDsOfList(t, v), []string{"group.Garmin", "light.hue_a", "light.hue_b"})
	status, _ = app.apiGet(t, "/api/states", "")
	equal(t, status, http.StatusUnauthorized)
}

func TestLightToggleFollowsTheCurrentState(t *testing.T) {
	app := newAPIApp(t)
	app.app.core.SetState("light.hue_a", "on", nil)
	body := `{"entity_id":["light.hue_a","light.hue_b"]}`
	status, _ := app.apiPost(t, "/api/services/light/toggle", apiToken, body)
	equal(t, status, http.StatusOK)
	equal(t, app.fake.recorded(), []lightCall{
		{id: "light.hue_a", action: core.LightAction{}},
		{id: "light.hue_b", action: core.LightAction{On: true}},
	})
}

func TestLightToggleFlipsOverlappingTargetsOnce(t *testing.T) {
	app := newAPIApp(t)
	app.app.core.SetState("light.hue_a", "on", nil)
	body := `{"entity_id":["group.Garmin","light.hue_a","light.hue_a"]}`
	status, v := app.apiPost(t, "/api/services/light/toggle", apiToken, body)
	equal(t, status, http.StatusOK)
	equal(t, len(app.fake.recorded()), 2)
	equal(t, len(asList(t, v)), 2)
}

func TestLightServiceErrorsMapToStatuses(t *testing.T) {
	app := newAPIApp(t)
	app.app.core.SetState("light.hue_bad", "off", nil)
	turnOn := "/api/services/light/turn_on"
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{turnOn, `{"entity_id":"light.hue_bad"}`, http.StatusBadGateway},
		{turnOn, "{not json", http.StatusBadRequest},
		{turnOn, `{"transition":-1}`, http.StatusBadRequest},
		{"/api/services/light/toggle", `{"transition":-1}`, http.StatusBadRequest},
	} {
		status, _ := app.apiPost(t, c.path, apiToken, c.body)
		if status != c.want {
			t.Errorf("%s %s: status %d, want %d", c.path, c.body, status, c.want)
		}
	}
}

func TestGroupListsMembers(t *testing.T) {
	app := newAPIApp(t)
	status, v := app.apiGet(t, "/api/states/group.Garmin", apiToken)
	equal(t, status, http.StatusOK)
	state := asMap(t, v)
	equal(t, state["entity_id"], "group.Garmin")
	equal(t, asMap(t, state["attributes"])["entity_id"], []any{"light.hue_a", "light.hue_b"})
	status, _ = app.apiGet(t, "/api/states/group.None", apiToken)
	equal(t, status, http.StatusNotFound)
}

func TestReadOnlyTokensReadButNeverControl(t *testing.T) {
	app := newAPIApp(t)
	app.addToken(t, "ro", store.NewToken{UserID: 1, Name: "ro", ReadOnly: true})
	status, _ := app.apiGet(t, "/api/states/light.hue_a", "ro")
	equal(t, status, http.StatusOK)
	body := `{"entity_id":"light.hue_a"}`
	for _, path := range []string{
		"/api/services/light/turn_on",
		"/api/services/light/turn_off",
		"/api/services/scene/turn_on",
	} {
		status, v := app.apiPost(t, path, "ro", body)
		if status != http.StatusForbidden {
			t.Errorf("%s: status %d", path, status)
		}
		equal(t, asMap(t, v)["message"], "This token is read-only.")
	}
	equal(t, len(app.fake.recorded()), 0)
}

func TestOwnerGroupLimitNarrowsExistingTokens(t *testing.T) {
	app := newAPIApp(t)
	owner, err := app.store.CreateUser(bg, "owner", "h", false)
	noError(t, err)
	app.addToken(t, "owned-token", store.NewToken{UserID: owner, Name: "o"})
	app.setGroup(t, "Other", "light.hue_c")
	app.app.core.SetState("light.hue_c", "off", nil)
	get := func(id string) int {
		status, _ := app.apiGet(t, "/api/states/"+id, "owned-token")
		return status
	}
	equal(t, get("light.hue_c"), http.StatusOK)
	// the token itself is unrestricted, but its owner is limited to Garmin
	noError(t, app.store.SetUserScope(bg, owner, store.Scope{"Garmin"}))
	equal(t, get("light.hue_a"), http.StatusOK)
	equal(t, get("light.hue_c"), http.StatusNotFound)
}

func TestTurnOnStringEntityReturnsStateArray(t *testing.T) {
	app := newAPIApp(t)
	status, v := app.apiPost(t, "/api/services/light/turn_on", apiToken, `{"entity_id":"light.hue_a"}`)
	equal(t, status, http.StatusOK)
	first := asMap(t, asList(t, v)[0])
	equal(t, first["entity_id"], "light.hue_a")
	equal(t, first["state"], "on")
	state, _ := app.app.core.GetState("light.hue_a")
	equal(t, state.State, "on")
	equal(t, app.fake.recorded()[0], lightCall{id: "light.hue_a", action: core.LightAction{On: true}})
}

func TestTargetArrayGroupAndBrightness(t *testing.T) {
	app := newAPIApp(t)
	body := `{"target":{"entity_id":["group.Garmin"]},"brightness_pct":50}`
	status, v := app.apiPost(t, "/api/services/light/turn_on", apiToken, body)
	equal(t, status, http.StatusOK)
	equal(t, len(asList(t, v)), 2)
	brightness := uint8(128)
	equal(t, app.fake.recorded()[1].action, core.LightAction{On: true, Brightness: &brightness})

	_, v = app.apiPost(t, "/api/services/light/turn_off", apiToken, `{"entity_id":"group.Garmin"}`)
	for _, item := range asList(t, v) {
		equal(t, asMap(t, item)["state"], "off")
	}
}

func TestColorAndTransitionFieldsFollowHomeAssistant(t *testing.T) {
	action := func(service, body string) (*core.LightAction, error) {
		parsed, err := parseBody([]byte(body))
		noError(t, err)
		return lightAction(service, parsed)
	}
	mustAction := func(service, body string) core.LightAction {
		t.Helper()
		got, err := action(service, body)
		if err != nil || got == nil {
			t.Fatalf("%s %s: %v %v", service, body, got, err)
		}
		return *got
	}
	ms, mirekOf := func(v uint32) *uint32 { return &v }, func(v uint16) *uint16 { return &v }

	equal(t, mustAction("turn_on", `{"color_temp": 300, "transition": 1.5}`),
		core.LightAction{On: true, ColorTemp: mirekOf(300), TransitionMS: ms(1500)})
	// kelvin converts to mireds and is clamped to what Hue can do
	equal(t, mustAction("turn_on", `{"color_temp_kelvin": 2700}`), core.LightAction{On: true, ColorTemp: mirekOf(370)})
	equal(t, mustAction("turn_on", `{"color_temp": 10}`), core.LightAction{On: true, ColorTemp: mirekOf(153)})
	equal(t, mustAction("turn_off", `{"transition": 3}`), core.LightAction{TransitionMS: ms(3000)})

	// red in sRGB lands near the red corner of the gamut
	red := mustAction("turn_on", `{"rgb_color": [255, 0, 0]}`).XY
	if red == nil || absDiff(red.X, 0.7) >= 0.03 || absDiff(red.Y, 0.3) >= 0.03 {
		t.Fatalf("rgb_color should give xy near red: %v", red)
	}
	equal(t, mustAction("turn_on", `{"hs_color": [0, 100]}`).XY, red) // hs red and rgb red are the same color

	for _, bad := range []string{
		`{"transition": -1}`, `{"transition": "x"}`, `{"color_temp": 0}`, `{"xy_color": [0.5]}`,
		`{"xy_color": [2, 0]}`, `{"rgb_color": [1, 2]}`, `{"hs_color": "red"}`,
	} {
		if _, err := action("turn_on", bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
	got, err := action("toggle", `{}`)
	equal(t, got == nil && err == nil, true)
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

func TestColorSpellingsAndEdges(t *testing.T) {
	xyOf := func(body string) core.XY {
		t.Helper()
		parsed, err := parseBody([]byte(body))
		noError(t, err)
		action, err := lightAction("turn_on", parsed)
		if err != nil || action == nil || action.XY == nil {
			t.Fatalf("expected xy for %s: %v %v", body, action, err)
		}
		return *action.XY
	}
	// every hue sector produces a color; black falls back to the white point
	for _, hue := range []string{"0", "60", "120", "180", "240", "300"} {
		xy := xyOf(`{"hs_color": [` + hue + `, 100]}`)
		if !inUnitRange(xy.X) || !inUnitRange(xy.Y) {
			t.Errorf("hue %s: %v", hue, xy)
		}
	}
	equal(t, xyOf(`{"rgb_color": [0, 0, 0]}`), core.XY{X: 0.3127, Y: 0.3290})
	equal(t, xyOf(`{"xy_color": [0.2, 0.4]}`), core.XY{X: 0.2, Y: 0.4})
	// dark and bright channels use both gamma branches
	xyOf(`{"rgb_color": [3, 3, 3]}`)
	// `kelvin` is an alias, and bad values are refused
	parsed, _ := parseBody([]byte(`{"kelvin": 4000}`))
	action, err := lightAction("turn_on", parsed)
	noError(t, err)
	equal(t, *action.ColorTemp, uint16(250))
	for _, bad := range []string{
		`{"color_temp_kelvin": 0}`, `{"kelvin": "warm"}`, `{"hs_color": [1]}`,
		`{"rgb_color": [1, 2, 300]}`, `{"xy_color": ["a", "b"]}`,
	} {
		parsed, _ := parseBody([]byte(bad))
		if _, err := lightAction("turn_on", parsed); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestServiceEdgeCases(t *testing.T) {
	app := newAPIApp(t)
	// unknown entity: empty array, still JSON
	status, v := app.apiPost(t, "/api/services/light/turn_on", apiToken, `{"entity_id":"light.ghost"}`)
	equal(t, status, http.StatusOK)
	equal(t, v, []any{})
	// empty body
	status, v = app.apiPost(t, "/api/services/light/turn_off", apiToken, "")
	equal(t, status, http.StatusOK)
	equal(t, v, []any{})
	// bad entity type
	status, _ = app.apiPost(t, "/api/services/light/turn_on", apiToken, `{"entity_id":5}`)
	equal(t, status, http.StatusBadRequest)
	// unknown service
	status, _ = app.apiPost(t, "/api/services/light/blink", apiToken, "{}")
	equal(t, status, http.StatusNotFound)
	// unauthenticated service call
	status, _ = app.apiPost(t, "/api/services/light/turn_on", "", "{}")
	equal(t, status, http.StatusUnauthorized)
}

func TestSceneTurnOnStampsStateAndLightCallsSkipScenes(t *testing.T) {
	app := newAPIApp(t)
	c := app.app.core
	c.SetState("scene.hue_a", "unknown", nil)
	c.SetState("scene.hue_bad", "unknown", nil)

	_, v := app.apiGet(t, "/api/states/scene.hue_a", apiToken)
	equal(t, asMap(t, v)["state"], "unknown")

	status, v := app.apiPost(t, "/api/services/scene/turn_on", apiToken, `{"entity_id":"scene.hue_a"}`)
	equal(t, status, http.StatusOK)
	first := asMap(t, asList(t, v)[0])
	equal(t, first["entity_id"], "scene.hue_a")
	if !strings.Contains(first["state"].(string), "T") {
		t.Errorf("scene state is not a timestamp: %v", first["state"])
	}
	_, v = app.apiGet(t, "/api/states/scene.hue_a", apiToken)
	if !strings.Contains(asMap(t, v)["state"].(string), "T") {
		t.Errorf("stored scene state is not a timestamp: %v", v)
	}

	// unknown scenes are ignored, failures are 502, other services 404
	status, v = app.apiPost(t, "/api/services/scene/turn_on", apiToken, `{"target":{"entity_id":["scene.ghost"]}}`)
	equal(t, status, http.StatusOK)
	equal(t, v, []any{})
	status, _ = app.apiPost(t, "/api/services/scene/turn_on", apiToken, `{"entity_id":"scene.hue_bad"}`)
	equal(t, status, http.StatusBadGateway)
	status, _ = app.apiPost(t, "/api/services/scene/create", apiToken, "{}")
	equal(t, status, http.StatusNotFound)

	// a group holding a scene lists it, and light calls on the group leave it alone
	app.setGroup(t, "Mixed", "light.hue_a", "scene.hue_a")
	_, v = app.apiGet(t, "/api/states/group.Mixed", apiToken)
	equal(t, asMap(t, asMap(t, v)["attributes"])["entity_id"], []any{"light.hue_a", "scene.hue_a"})
	_, v = app.apiPost(t, "/api/services/light/turn_on", apiToken, `{"entity_id":"group.Mixed"}`)
	equal(t, len(asList(t, v)), 1)
	equal(t, len(app.fake.recorded()), 1)
}

func TestGroupLightServiceCallReturnsItsState(t *testing.T) {
	app := newAPIApp(t)
	app.exposeGroup(t, "Garmin")
	status, v := app.apiPost(t, "/api/services/light/turn_on", apiToken, `{"entity_id":"light.domus_group_Garmin"}`)
	equal(t, status, http.StatusOK)
	first := asMap(t, asList(t, v)[0])
	equal(t, first["entity_id"], "light.domus_group_Garmin")
	equal(t, first["state"], "on")
	equal(t, len(app.fake.recorded()), 2)
	status, v = app.apiGet(t, "/api/states/light.domus_group_Garmin", apiToken)
	equal(t, status, http.StatusOK)
	equal(t, asMap(t, v)["state"], "on")
}

func TestScopedTokenOnlyReachesItsGroups(t *testing.T) {
	app := newAPIApp(t)
	c := app.app.core
	c.SetState("light.hue_c", "off", nil)
	c.SetState("scene.hue_a", "unknown", nil)
	app.setGroup(t, "Other", "light.hue_c", "scene.hue_a")
	app.exposeGroup(t, "Garmin")
	app.addToken(t, "scoped", store.NewToken{UserID: 1, Name: "scoped", Scope: store.Scope{"Garmin"}})

	for _, ok := range []string{"group.Garmin", "light.hue_a", "light.hue_b", "light.domus_group_Garmin"} {
		status, _ := app.apiGet(t, "/api/states/"+ok, "scoped")
		if status != http.StatusOK {
			t.Errorf("%s: status %d", ok, status)
		}
	}
	for _, denied := range []string{"group.Other", "light.hue_c", "scene.hue_a", "group.None"} {
		status, _ := app.apiGet(t, "/api/states/"+denied, "scoped")
		if status != http.StatusNotFound {
			t.Errorf("%s: status %d", denied, status)
		}
	}

	// out-of-scope entities are skipped, in-scope ones still run
	body := `{"entity_id":["light.hue_a","light.hue_c","group.Other"]}`
	status, v := app.apiPost(t, "/api/services/light/turn_on", "scoped", body)
	equal(t, status, http.StatusOK)
	equal(t, entityIDsOfList(t, v), []string{"light.hue_a"})
	equal(t, len(app.fake.recorded()), 1)

	status, v = app.apiPost(t, "/api/services/scene/turn_on", "scoped", `{"entity_id":"scene.hue_a"}`)
	equal(t, status, http.StatusOK)
	equal(t, v, []any{})
	_, v = app.apiGet(t, "/api/states/scene.hue_a", apiToken)
	equal(t, asMap(t, v)["state"], "unknown")

	// the group's own light switch fans out to its members
	status, v = app.apiPost(t, "/api/services/light/turn_off", "scoped", `{"entity_id":"light.domus_group_Garmin"}`)
	equal(t, status, http.StatusOK)
	equal(t, asMap(t, asList(t, v)[0])["entity_id"], "light.domus_group_Garmin")
	equal(t, len(app.fake.recorded()), 3)

	// an unrestricted token still reaches everything
	status, _ = app.apiGet(t, "/api/states/light.hue_c", apiToken)
	equal(t, status, http.StatusOK)
}

func TestANullEntityIDRequestsNothing(t *testing.T) {
	parsed, err := parseBody([]byte(`{"entity_id": null}`))
	noError(t, err)
	ids, err := requestedEntities(parsed)
	noError(t, err)
	equal(t, len(ids), 0)
}

func assertClose(t *testing.T, gotX, gotY, wantX, wantY float64) {
	t.Helper()
	if absDiff(gotX, wantX) >= 1e-4 || absDiff(gotY, wantY) >= 1e-4 {
		t.Errorf("(%v, %v) != (%v, %v)", gotX, gotY, wantX, wantY)
	}
}

func TestRgbToXyMatchesKnownColours(t *testing.T) {
	// Expected values computed by hand from the Hue wide-gamut matrix.
	cases := []struct{ r, g, b, x, y float64 }{
		{255, 0, 0, 0.7006, 0.2993},
		{0, 255, 0, 0.1724, 0.7468},
		{0, 0, 255, 0.1355, 0.0399},
		{255, 255, 255, 0.3227, 0.3290},
		{128, 64, 32, 0.5737, 0.3592},
		// 10/255 is below the sRGB knee, so these use the linear branch
		{10, 5, 0, 0.5313, 0.4428},
		// mixes both branches
		{50, 10, 0, 0.6571, 0.3362},
		{0, 10, 50, 0.1380, 0.0869},
	}
	for _, c := range cases {
		x, y := rgbToXY(c.r, c.g, c.b)
		assertClose(t, x, y, c.x, c.y)
	}
}

func TestRgbToXyClampsAndHandlesBlack(t *testing.T) {
	x, y := rgbToXY(0, 0, 0)
	equal(t, [2]float64{x, y}, [2]float64{0.3127, 0.3290})
	x, y = rgbToXY(300, -20, 0)
	assertClose(t, x, y, 0.7006, 0.2993)
}

func TestHsToRgbCoversEveryHueSector(t *testing.T) {
	cases := []struct{ h, s, r, g, b float64 }{
		{0, 100, 255, 0, 0},
		{30, 100, 255, 127.5, 0},
		{60, 100, 255, 255, 0},
		{90, 100, 127.5, 255, 0},
		{120, 100, 0, 255, 0},
		{150, 100, 0, 255, 127.5},
		{180, 100, 0, 255, 255},
		{210, 100, 0, 127.5, 255},
		{240, 100, 0, 0, 255},
		{270, 100, 127.5, 0, 255},
		{300, 100, 255, 0, 255},
		{330, 100, 255, 0, 127.5},
		// hue wraps around
		{360, 100, 255, 0, 0},
		{-30, 100, 255, 0, 127.5},
	}
	for _, c := range cases {
		r, g, b := hsToRGB(c.h, c.s)
		if absDiff(r, c.r) >= 1e-6 || absDiff(g, c.g) >= 1e-6 || absDiff(b, c.b) >= 1e-6 {
			t.Errorf("hs(%v, %v) = (%v, %v, %v)", c.h, c.s, r, g, b)
		}
	}
}

func TestHsToRgbDesaturatesTowardWhite(t *testing.T) {
	r, g, b := hsToRGB(200, 0)
	equal(t, [3]float64{r, g, b}, [3]float64{255, 255, 255})
	r, g, b = hsToRGB(30, 50)
	if absDiff(r, 255) >= 1e-6 || absDiff(g, 191.25) >= 1e-6 || absDiff(b, 127.5) >= 1e-6 {
		t.Errorf("(%v, %v, %v)", r, g, b)
	}
}

func TestToggleTurningOnKeepsTheTransition(t *testing.T) {
	app := newAPIApp(t)
	status, _ := app.apiPost(t, "/api/services/light/toggle", apiToken, `{"entity_id":"light.hue_a","transition":2}`)
	equal(t, status, http.StatusOK)
	transition := uint32(2000)
	equal(t, app.fake.recorded(), []lightCall{
		{id: "light.hue_a", action: core.LightAction{On: true, TransitionMS: &transition}},
	})
}

func TestVisibleStatesKeepsOnlyTheRequestedIDs(t *testing.T) {
	app := newAPIApp(t)
	states := app.app.visibleStates(bg, nil, map[string]bool{"light.hue_b": true})
	equal(t, len(states), 1)
	equal(t, states[0].EntityID, "light.hue_b")
}

// The tests below pin behaviour of the Rust server that no Rust unit test
// covers; the expectations come from the recorded goldens.

func TestRestRoutesAnswerLikeTheRustServer(t *testing.T) {
	app := newAPIApp(t)
	bearer := map[string]string{"Authorization": "Bearer " + apiToken}
	cases := []struct {
		name       string
		req        request
		status     int
		allow      string
		bodyText   string
		bodyIsJSON bool
	}{
		{"no token is plain text", request{path: "/api/states"}, 401, "", "401: Unauthorized", false},
		{"bad scheme", request{path: "/api/states", headers: map[string]string{"Authorization": "Basic x"}}, 401, "", "401: Unauthorized", false},
		{"lowercase scheme", request{path: "/api/states", headers: map[string]string{"Authorization": "bearer " + apiToken}}, 200, "", "", true},
		{"wrong method with token", request{method: "POST", path: "/api/states", headers: bearer}, 405, "GET,HEAD", "", false},
		{"wrong method without token", request{method: "POST", path: "/api/states"}, 401, "GET,HEAD", "401: Unauthorized", false},
		{"service with GET", request{path: "/api/services/light/turn_on", headers: bearer}, 405, "POST", "", false},
		{"unknown domain falls through", request{method: "POST", path: "/api/services/switch/turn_on", headers: bearer, body: "{}"}, 404, "", "", false},
		{"bad JSON", request{method: "POST", path: "/api/services/light/turn_on", headers: bearer, body: "{nope"}, 400, "", `{"message":"Invalid JSON."}`, true},
		{"no content type is fine", request{method: "POST", path: "/api/services/light/turn_on", headers: bearer, body: `{"entity_id":"light.hue_a"}`}, 200, "", "", true},
	}
	for _, c := range cases {
		rec := app.do(t, c.req)
		if rec.Code != c.status || rec.Header().Get("Allow") != c.allow {
			t.Errorf("%s: status %d allow %q", c.name, rec.Code, rec.Header().Get("Allow"))
		}
		if c.bodyText != "" && rec.Body.String() != c.bodyText {
			t.Errorf("%s: body %q", c.name, rec.Body)
		}
		if c.bodyText == "" && !c.bodyIsJSON && rec.Body.Len() != 0 {
			t.Errorf("%s: body %q, want empty", c.name, rec.Body)
		}
	}
	rec := app.do(t, request{path: "/api/states"})
	equal(t, rec.Header().Get("Content-Type"), "text/plain; charset=utf-8")
}

func TestBearerToken(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer abc": "abc", "BEARER abc": "abc", "bearer   abc  ": "abc", "Bearer ": "",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", header)
		got, ok := bearerToken(r)
		if !ok || got != strings.TrimSpace(want) {
			t.Errorf("%q: got %q %v", header, got, ok)
		}
	}
	for _, header := range []string{"", "Bearer", "Basic abc", "Bearerabc"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if _, ok := bearerToken(r); ok {
			t.Errorf("%q: accepted", header)
		}
	}
}
