package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/henry40408/domus/internal/store"
)

const groupsPath = "/api/domus/groups"

// adminWithGarminGroup is an admin session plus a Garmin group holding
// light.hue_a.
func adminWithGarminGroup(t *testing.T) (*testApp, string) {
	t.Helper()
	app := newTestApp(t)
	cookie := app.setupAdmin(t)
	app.app.core.SetState("light.hue_a", "on", nil)
	members := `{"members":["light.hue_a"]}`
	equal(t, app.adminStatus(t, http.MethodPut, groupsPath+"/Garmin", cookie, members), 200)
	return app, cookie
}

func TestGroupsValidateTheirNameAndMembers(t *testing.T) {
	app, cookie := adminWithGarminGroup(t)
	equal(t, app.adminStatus(t, http.MethodPut, groupsPath+"/bad%20name", cookie, `{"members":[]}`), 400)
	equal(t, app.adminStatus(t, http.MethodPut, groupsPath+"/G", cookie, `{"members":["switch.x"]}`), 400)
	equal(t, app.adminStatus(t, http.MethodPut, groupsPath+"/G", cookie, `{}`), 422)
	_, groups := app.adminJSON(t, http.MethodGet, groupsPath, cookie, "")
	equal(t, groups, []any{map[string]any{
		"name": "Garmin", "members": []any{"light.hue_a"},
		"expose_light": false, "light_entity_id": "light.domus_group_Garmin",
	}})
}

func TestExposingAGroupShowsTheFlagAndListsItsLight(t *testing.T) {
	app, cookie := adminWithGarminGroup(t)
	body := `{"members":["light.hue_a"],"expose_light":true}`
	equal(t, app.adminStatus(t, http.MethodPut, groupsPath+"/Garmin", cookie, body), 200)
	_, groups := app.adminJSON(t, http.MethodGet, groupsPath, cookie, "")
	equal(t, asMap(t, asList(t, groups)[0])["expose_light"], true)
	_, lights := app.adminJSON(t, http.MethodGet, "/api/domus/lights", cookie, "")
	found := false
	for _, item := range asList(t, lights) {
		light := asMap(t, item)
		if light["entity_id"] == "light.domus_group_Garmin" && light["state"] == "on" {
			found = true
		}
	}
	equal(t, found, true)
}

func TestGroupsTakeScenesButNotOtherDomainsOrTheirOwnLight(t *testing.T) {
	app, cookie := adminWithGarminGroup(t)
	put := func(body string) int {
		return app.adminStatus(t, http.MethodPut, groupsPath+"/Garmin", cookie, body)
	}
	put(`{"members":["light.hue_a"],"expose_light":true}`)

	// scenes are accepted as members and offered in the picker
	app.app.core.SetState("scene.hue_s", "unknown", nil)
	equal(t, put(`{"members":["light.hue_a","scene.hue_s"]}`), 200)
	_, lights := app.adminJSON(t, http.MethodGet, "/api/domus/lights", cookie, "")
	equal(t, strings.Contains(strings.Join(entityIDsOfList(t, lights), " "), "scene.hue_s"), true)

	equal(t, put(`{"members":["switch.x"]}`), 400)
	equal(t, put(`{"members":["light.domus_group_Garmin"]}`), 400)
}

func TestAnOmittedExposeFlagKeepsItAndGroupsCanBeDeleted(t *testing.T) {
	app, cookie := adminWithGarminGroup(t)
	path := groupsPath + "/Garmin"
	app.adminStatus(t, http.MethodPut, path, cookie, `{"members":["light.hue_a"],"expose_light":true}`)
	app.adminStatus(t, http.MethodPut, path, cookie, `{"members":["light.hue_a"]}`)
	exposed, err := app.store.GroupExposed(bg, "Garmin")
	noError(t, err)
	equal(t, exposed, true)

	equal(t, app.adminStatus(t, http.MethodDelete, path, cookie, ""), 200)
	equal(t, app.adminStatus(t, http.MethodDelete, path, cookie, ""), 404)
}

func TestGroupNamesUseLettersDigitsUnderscoreAndDash(t *testing.T) {
	for _, name := range []string{"a", "A_b-9", strings.Repeat("x", 64)} {
		equal(t, validGroupName(name), true)
	}
	for _, name := range []string{"", "a b", "a/b", "ä", strings.Repeat("x", 65)} {
		equal(t, validGroupName(name), false)
	}
}

func TestPairingFailureReportsBadGateway(t *testing.T) {
	app := newTestApp(t)
	cookie := app.setupAdmin(t)
	pair := "/api/domus/hue/pair"
	equal(t, app.adminStatus(t, http.MethodPost, pair, cookie, `{"ip":"http://127.0.0.1:1"}`), 502)
	_, err := app.store.HueGet(bg)
	equal(t, err, store.ErrNotFound)
	equal(t, app.adminStatus(t, http.MethodPost, pair, cookie, `{"ip":""}`), 400)
	equal(t, app.adminStatus(t, http.MethodPost, pair, cookie, `{}`), 422)

	_, status := app.adminJSON(t, http.MethodGet, "/api/domus/hue", cookie, "")
	equal(t, status, map[string]any{"paired": false, "ip": nil, "running": false})
}
