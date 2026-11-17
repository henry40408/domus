package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const usersPath = "/api/domus/users"

func (a *testApp) bobURI(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s/%d", usersPath, a.userID(t, "bob"))
}

func TestUserCreateRefusesABadUsername(t *testing.T) {
	app := newTestApp(t)
	admin := app.setupAdmin(t)
	for _, name := range []string{"a\nb", "   ", strings.Repeat("a", 65)} {
		body := fmt.Sprintf(`{"username":%q,"password":"long enough pw12"}`, name)
		equal(t, app.adminStatus(t, http.MethodPost, usersPath, admin, body), 400)
	}
}

func TestUserCreateRefusesDuplicatesAndWeakPasswords(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.adminAndMember(t)
	dup := `{"username":"Bob","password":"long enough pw12"}`
	equal(t, app.adminStatus(t, http.MethodPost, usersPath, admin, dup), 409)
	weak := `{"username":"eve","password":"short"}`
	equal(t, app.adminStatus(t, http.MethodPost, usersPath, admin, weak), 400)
}

func TestUserCreateNeedsEveryField(t *testing.T) {
	app := newTestApp(t)
	admin := app.setupAdmin(t)
	equal(t, app.adminStatus(t, http.MethodPost, usersPath, admin, `{"username":"eve"}`), 422)
	equal(t, app.adminStatus(t, http.MethodPost, usersPath, admin, `not json`), 400)
}

func TestANonASCIIUserCanBeCreatedAndLogIn(t *testing.T) {
	app := newTestApp(t)
	admin := app.setupAdmin(t)
	body := `{"username":"王小明","password":"long enough pw12"}`
	code, created := app.adminJSON(t, http.MethodPost, usersPath, admin, body)
	equal(t, code, 200)
	equal(t, asMap(t, created)["username"], "王小明")
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "", body), 200)
}

func TestMembersCannotUseAdminRoutes(t *testing.T) {
	app := newTestApp(t)
	admin, bob := app.adminAndMember(t)
	for _, route := range [][2]string{
		{http.MethodGet, "/api/domus/hue"},
		{http.MethodPost, "/api/domus/hue/pair"},
		{http.MethodPut, "/api/domus/groups/G"},
		{http.MethodDelete, "/api/domus/groups/G"},
		{http.MethodGet, usersPath},
		{http.MethodPost, usersPath},
		{http.MethodPut, usersPath + "/1"},
		{http.MethodDelete, usersPath + "/1"},
	} {
		equal(t, app.adminStatus(t, route[0], route[1], bob, "{}"), 403)
	}
	for _, path := range []string{"/api/domus/lights", "/api/domus/groups", "/api/domus/tokens"} {
		equal(t, app.adminStatus(t, http.MethodGet, path, bob, ""), 200)
	}
	equal(t, app.adminStatus(t, http.MethodGet, usersPath, admin, ""), 200)

	_, status := app.adminJSON(t, http.MethodGet, "/api/domus/status", bob, "")
	user := asMap(t, asMap(t, status)["user"])
	equal(t, user["username"], "bob")
	equal(t, user["is_admin"], false)
}

func TestTheLastAdminCannotBeDemotedOrDeleted(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.adminAndMember(t)
	own := usersPath + "/1"
	equal(t, app.adminStatus(t, http.MethodPut, own, admin, `{"is_admin":false}`), 400)
	equal(t, app.adminStatus(t, http.MethodDelete, own, admin, ""), 400)
	// a refused demotion applies nothing else from the same request
	changed := `{"is_admin":false,"password":"changed changed 12"}`
	equal(t, app.adminStatus(t, http.MethodPut, own, admin, changed), 400)
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "", adminBody), 200)
	equal(t, app.adminStatus(t, http.MethodGet, "/api/domus/tokens", admin, ""), 200)
}

func TestAdminPasswordResetSignsTheUserOut(t *testing.T) {
	app := newTestApp(t)
	admin, bob := app.adminAndMember(t)
	reset := `{"password":"reset reset reset"}`
	equal(t, app.adminStatus(t, http.MethodPut, app.bobURI(t), admin, reset), 200)
	equal(t, app.adminStatus(t, http.MethodGet, "/api/domus/tokens", bob, ""), 401)
	login := `{"username":"bob","password":"reset reset reset"}`
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "", login), 200)
}

func TestDeletingADemotedAdminEndsTheirSession(t *testing.T) {
	app := newTestApp(t)
	admin, bob := app.adminAndMember(t)
	// promote bob, then the original admin may be demoted
	equal(t, app.adminStatus(t, http.MethodPut, app.bobURI(t), admin, `{"is_admin":true}`), 200)
	equal(t, app.adminStatus(t, http.MethodPut, usersPath+"/1", admin, `{"is_admin":false}`), 200)
	equal(t, app.adminStatus(t, http.MethodPut, usersPath+"/99", admin, "{}"), 403)
	equal(t, app.adminStatus(t, http.MethodDelete, usersPath+"/1", bob, ""), 200)
	equal(t, app.adminStatus(t, http.MethodGet, "/api/domus/tokens", admin, ""), 401)
}

func TestAnAdminCannotDeleteThemselvesOrAMissingUser(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.adminAndMember(t)
	equal(t, app.adminStatus(t, http.MethodDelete, usersPath+"/1", admin, ""), 400)
	equal(t, app.adminStatus(t, http.MethodDelete, usersPath+"/99", admin, ""), 404)
	equal(t, app.adminStatus(t, http.MethodPut, usersPath+"/99", admin, "{}"), 404)
	equal(t, app.adminStatus(t, http.MethodDelete, app.bobURI(t), admin, ""), 200)
}

func TestLimitingAUserValidatesTheGroups(t *testing.T) {
	f := newLimitedFixture(t)
	for _, scope := range []string{`{"scope":["Nope"]}`, `{"scope":[]}`} {
		equal(t, f.app.adminStatus(t, http.MethodPut, f.bobURI, f.admin, scope), 400)
	}
	// an admin cannot be limited
	equal(t, f.app.adminStatus(t, http.MethodPut, usersPath+"/1", f.admin, `{"scope":["G1"]}`), 400)
	f.limitBobToG1(t)
	_, users := f.app.adminJSON(t, http.MethodGet, usersPath, f.admin, "")
	list := asList(t, users)
	equal(t, asMap(t, list[1])["scope"], []any{"G1"})
	equal(t, asMap(t, list[0])["scope"], nil)
}

func TestLiftingTheLimitOrMakingAnAdminClearsIt(t *testing.T) {
	f := newLimitedFixture(t)
	f.limitBobToG1(t)
	// null lifts the limit
	equal(t, f.app.adminStatus(t, http.MethodPut, f.bobURI, f.admin, `{"scope":null}`), 200)
	users, err := f.app.store.ListUsers(bg)
	noError(t, err)
	equal(t, users[1].Scope == nil, true)
	equal(t, f.testDevice(t, f.bob, "light.hue_b"), 200)

	// an absent scope leaves the limit alone
	f.limitBobToG1(t)
	equal(t, f.app.adminStatus(t, http.MethodPut, f.bobURI, f.admin, `{}`), 200)
	users, err = f.app.store.ListUsers(bg)
	noError(t, err)
	equal(t, []string(users[1].Scope), []string{"G1"})

	// making someone admin clears a limit too
	equal(t, f.app.adminStatus(t, http.MethodPut, f.bobURI, f.admin, `{"is_admin":true}`), 200)
	users, err = f.app.store.ListUsers(bg)
	noError(t, err)
	equal(t, users[1].Scope == nil, true)
}

func TestAMalformedScopeIsRefused(t *testing.T) {
	f := newLimitedFixture(t)
	equal(t, f.app.adminStatus(t, http.MethodPut, f.bobURI, f.admin, `{"scope":"G1"}`), 422)
	equal(t, f.app.adminStatus(t, http.MethodPut, usersPath+"/x", f.admin, `{}`), 400)
}
