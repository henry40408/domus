package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/store"
)

const tokensPath = "/api/domus/tokens"

// mint creates a token and returns the status and the decoded answer.
func (a *testApp) mint(t *testing.T, cookie, body string) (int, map[string]any) {
	t.Helper()
	code, decoded := a.adminJSON(t, http.MethodPost, tokensPath, cookie, body)
	reply, _ := decoded.(map[string]any)
	return code, reply
}

func (a *testApp) tokenAuth(t *testing.T, secret any) (store.TokenAuth, error) {
	t.Helper()
	return a.store.TokenAuth(bg, auth.SHA256Hex(fmt.Sprint(secret)))
}

func TestTokensBelongToTheirOwner(t *testing.T) {
	app := newTestApp(t)
	admin, bob := app.adminAndMember(t)
	_, adminToken := app.mint(t, admin, `{"name":"admin-watch"}`)
	_, bobToken := app.mint(t, bob, `{"name":"bob-watch"}`)

	names := func(cookie string) []string {
		_, list := app.adminJSON(t, http.MethodGet, tokensPath, cookie, "")
		names := []string{}
		for _, item := range asList(t, list) {
			names = append(names, asMap(t, item)["name"].(string))
		}
		return names
	}
	equal(t, names(bob), []string{"bob-watch"})
	equal(t, names(admin), []string{"admin-watch", "bob-watch"})
	_, all := app.adminJSON(t, http.MethodGet, tokensPath, admin, "")
	equal(t, asMap(t, asList(t, all)[1])["username"], "bob")

	// bob cannot revoke the admin's token; the admin can revoke bob's
	revoke := func(cookie string, token map[string]any) int {
		return app.adminStatus(t, http.MethodDelete, fmt.Sprintf("%s/%v", tokensPath, token["id"]), cookie, "")
	}
	equal(t, revoke(bob, adminToken), 404)
	equal(t, revoke(admin, bobToken), 200)
	_, err := app.tokenAuth(t, adminToken["token"])
	noError(t, err)
}

func TestTokenScopeIsValidatedAndListed(t *testing.T) {
	app := newTestApp(t)
	cookie := app.setupAdmin(t)
	app.setGroup(t, "Garmin", "light.hue_a")

	code, _ := app.mint(t, cookie, `{"name":"a","scope":[]}`)
	equal(t, code, 400)
	code, _ = app.mint(t, cookie, `{"name":"a","scope":["Nope"]}`)
	equal(t, code, 400)
	code, reply := app.mint(t, cookie, `{"name":"a","scope":["Garmin","Garmin"]}`)
	equal(t, code, 200)
	granted, err := app.tokenAuth(t, reply["token"])
	noError(t, err)
	equal(t, []string(granted.Scope), []string{"Garmin"})
	code, _ = app.mint(t, cookie, `{"name":"open"}`)
	equal(t, code, 200)

	_, list := app.adminJSON(t, http.MethodGet, tokensPath, cookie, "")
	items := asList(t, list)
	equal(t, asMap(t, items[0])["scope"], []any{"Garmin"})
	equal(t, asMap(t, items[1])["scope"], nil)
}

func TestTokensAreShownOnceStoredHashedAndRevocable(t *testing.T) {
	app := newTestApp(t)
	cookie := app.setupAdmin(t)

	code, reply := app.mint(t, cookie, `{"name":"watch"}`)
	equal(t, code, 200)
	token := reply["token"].(string)
	if !strings.HasPrefix(token, "domus_") || len(token) != 6+32 {
		t.Fatalf("token %q", token)
	}
	_, err := app.tokenAuth(t, token)
	noError(t, err)
	_, list := app.adminJSON(t, http.MethodGet, tokensPath, cookie, "")
	first := asMap(t, asList(t, list)[0])
	equal(t, first["name"], "watch")
	_, leaked := first["token"]
	equal(t, leaked, false)
	code, _ = app.mint(t, cookie, `{"name":" "}`)
	equal(t, code, 400)

	path := fmt.Sprintf("%s/%v", tokensPath, reply["id"])
	equal(t, app.adminStatus(t, http.MethodDelete, path, cookie, ""), 200)
	_, err = app.tokenAuth(t, token)
	equal(t, err != nil, true)
	equal(t, app.adminStatus(t, http.MethodDelete, path, cookie, ""), 404)
}

func TestTokenNamesMayBe64CharactersButNot65(t *testing.T) {
	app := newTestApp(t)
	cookie := app.setupAdmin(t)
	for length, want := range map[int]int{64: 200, 65: 400} {
		body := fmt.Sprintf(`{"name":%q}`, strings.Repeat("n", length))
		code, _ := app.mint(t, cookie, body)
		equal(t, code, want)
	}
}

func TestALimitedUserHandsOutTokensWithinTheirGroups(t *testing.T) {
	f := newLimitedFixture(t)
	f.limitBobToG1(t)
	// tokens default to bob's own groups, never beyond them
	code, _ := f.app.mint(t, f.bob, `{"name":"x","scope":["G2"]}`)
	equal(t, code, 403)
	code, _ = f.app.mint(t, f.bob, `{"name":"x","scope":["G1","G2"]}`)
	equal(t, code, 403)
	code, reply := f.app.mint(t, f.bob, `{"name":"open"}`)
	equal(t, code, 200)
	granted, err := f.app.tokenAuth(t, reply["token"])
	noError(t, err)
	equal(t, []string(granted.Scope), []string{"G1"})
	code, _ = f.app.mint(t, f.bob, `{"name":"ok","scope":["G1"]}`)
	equal(t, code, 200)
	// the admin is not limited
	_, reply = f.app.mint(t, f.admin, `{"name":"a"}`)
	granted, err = f.app.tokenAuth(t, reply["token"])
	noError(t, err)
	equal(t, granted.Scope == nil, true)
}

func TestTokenReadOnlyAndExpiryAreValidated(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.adminAndMember(t)
	for _, days := range []string{"0", "-5", "3651"} {
		code, _ := app.mint(t, admin, `{"name":"x","expires_days":`+days+`}`)
		equal(t, code, 400)
	}
	code, reply := app.mint(t, admin, `{"name":"x","read_only":true,"expires_days":30}`)
	equal(t, code, 200)
	granted, err := app.tokenAuth(t, reply["token"])
	noError(t, err)
	equal(t, granted.ReadOnly, true)

	_, list := app.adminJSON(t, http.MethodGet, tokensPath, admin, "")
	first := asMap(t, asList(t, list)[0])
	equal(t, first["read_only"], true)
	left := int64(first["expires"].(float64)) - time.Now().Unix()
	if left < 30*86400-5 || left > 30*86400 {
		t.Fatalf("expires in %d seconds", left)
	}
}

func TestAppDevicesAreListedAndRemovedPerOwner(t *testing.T) {
	app := newTestApp(t)
	admin, bob := app.adminAndMember(t)
	fields := func(name string) store.RegistrationFields {
		return store.RegistrationFields{
			AppID: "io.homeassistant.companion.ios", AppName: "Home Assistant",
			AppVersion: "2025.12", DeviceName: name, Manufacturer: "Apple",
			Model: "iPhone", OSName: "iOS",
		}
	}
	// admin is user 1, bob user 2
	_, err := app.store.RegistrationCreate(bg, store.NewRegistration{UserID: 1, WebhookHash: "h-admin", Fields: fields("admin phone")})
	noError(t, err)
	bobRegistration, err := app.store.RegistrationCreate(bg, store.NewRegistration{UserID: 2, WebhookHash: "h-bob", Fields: fields("bob phone")})
	noError(t, err)

	_, all := app.adminJSON(t, http.MethodGet, "/api/domus/apps", admin, "")
	equal(t, len(asList(t, all)), 2)
	_, mine := app.adminJSON(t, http.MethodGet, "/api/domus/apps", bob, "")
	equal(t, len(asList(t, mine)), 1)
	equal(t, asMap(t, asList(t, mine)[0])["device_name"], "bob phone")

	// bob cannot remove the admin's device, and missing ids are 404
	var adminRegistration int64
	for _, item := range asList(t, all) {
		if asMap(t, item)["device_name"] == "admin phone" {
			adminRegistration = int64(asMap(t, item)["id"].(float64))
		}
	}
	remove := func(id int64) int {
		return app.adminStatus(t, http.MethodDelete, fmt.Sprintf("/api/domus/apps/%d", id), bob, "")
	}
	equal(t, remove(adminRegistration), 404)
	equal(t, remove(9999), 404)
	equal(t, remove(bobRegistration), 200)
	left, err := app.store.ListRegistrations(bg, nil)
	noError(t, err)
	equal(t, len(left), 1)
}
