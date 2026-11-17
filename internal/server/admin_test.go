package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/store"
)

const (
	adminBody  = `{"username":"admin","password":"correct horse"}`
	bobsSecret = "bob's password"
)

// adminDo sends a JSON request to the admin API, with the session cookie when
// given.
func (a *testApp) adminDo(t *testing.T, method, path, cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{"Content-Type": "application/json"}
	if cookie != "" {
		headers["Cookie"] = cookie
	}
	return a.do(t, request{method: method, path: path, headers: headers, body: body})
}

func (a *testApp) adminStatus(t *testing.T, method, path, cookie, body string) int {
	t.Helper()
	return a.adminDo(t, method, path, cookie, body).Code
}

// adminJSON is adminDo with the status and the decoded body.
func (a *testApp) adminJSON(t *testing.T, method, path, cookie, body string) (int, any) {
	t.Helper()
	rec := a.adminDo(t, method, path, cookie, body)
	var decoded any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec.Code, decoded
}

// cookieOf is the name=value pair of the response's Set-Cookie.
func cookieOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	setCookie := rec.Header().Get("Set-Cookie")
	pair, _, _ := strings.Cut(setCookie, ";")
	if !strings.HasPrefix(pair, sessionCookieName+"=") {
		t.Fatalf("no session cookie in %q (status %d, body %s)", setCookie, rec.Code, rec.Body)
	}
	return pair
}

// setupAdmin creates the first admin through the setup route and returns the
// session cookie.
func (a *testApp) setupAdmin(t *testing.T) string {
	t.Helper()
	rec := a.adminDo(t, http.MethodPost, "/api/domus/setup", "", adminBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	return cookieOf(t, rec)
}

// addMember creates a regular user directly in the store.
func (a *testApp) addMember(t *testing.T, name, password string) int64 {
	t.Helper()
	id, err := a.store.CreateUser(bg, name, auth.HashPassword(password), false)
	noError(t, err)
	return id
}

// login logs in as name and returns the session cookie.
func (a *testApp) login(t *testing.T, name, password string) string {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, name, password)
	rec := a.adminDo(t, http.MethodPost, "/api/domus/login", "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", name, rec.Code, rec.Body)
	}
	return cookieOf(t, rec)
}

// adminAndMember sets up the admin, adds the member bob and logs both in.
func (a *testApp) adminAndMember(t *testing.T) (admin, bob string) {
	t.Helper()
	admin = a.setupAdmin(t)
	a.addMember(t, "bob", bobsSecret)
	return admin, a.login(t, "BOB", bobsSecret)
}

// alive reports whether the session cookie still works.
func (a *testApp) alive(t *testing.T, cookie string) bool {
	t.Helper()
	return a.adminStatus(t, http.MethodGet, "/api/domus/sessions", cookie, "") == http.StatusOK
}

func TestStatusBeforeSetupReportsNothingDone(t *testing.T) {
	app := newTestApp(t)
	code, body := app.adminJSON(t, http.MethodGet, "/api/domus/status", "", "")
	equal(t, code, 200)
	equal(t, body, map[string]any{
		"setup_done": false, "setup_code_required": false, "logged_in": false,
		"user": nil, "hue_paired": false, "version": goldenVersion,
	})
}

func TestSetupRefusesABadUsernameAndAShortPassword(t *testing.T) {
	app := newTestApp(t)
	for _, body := range []string{
		`{"username":"a\nb","password":"correct horse"}`,
		`{"username":"   ","password":"correct horse"}`,
		`{"username":"admin","password":"short"}`,
	} {
		equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/setup", "", body), 400)
	}
}

func TestSetupStartsASessionAndCannotRunTwice(t *testing.T) {
	app := newTestApp(t)
	rec := app.adminDo(t, http.MethodPost, "/api/domus/setup", "", adminBody)
	equal(t, rec.Code, 200)
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "HttpOnly") || !strings.Contains(setCookie, "SameSite=Strict") {
		t.Fatalf("cookie %q", setCookie)
	}
	if strings.Contains(setCookie, "Secure") {
		t.Fatalf("no Secure over plain http: %q", setCookie)
	}
	cookie := cookieOf(t, rec)

	second := `{"username":"second","password":"another one pw12"}`
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/setup", "", second), 409)

	_, body := app.adminJSON(t, http.MethodGet, "/api/domus/status", cookie, "")
	status := asMap(t, body)
	equal(t, status["logged_in"], true)
	user := asMap(t, status["user"])
	equal(t, user["username"], "admin")
	equal(t, user["is_admin"], true)
}

func TestSetupPasswordRules(t *testing.T) {
	setup := func(app *testApp, user, password string) int {
		body := fmt.Sprintf(`{"username":%q,"password":%q}`, user, password)
		return app.adminStatus(t, http.MethodPost, "/api/domus/setup", "", body)
	}
	app := newTestApp(t)
	equal(t, setup(app, "admin", "elevenchars"), 400)
	equal(t, setup(app, "administrator", "Administrator"), 400) // same as the username, any case
	equal(t, setup(app, "admin", strings.Repeat("x", 129)), 400)
	equal(t, setup(app, "admin", strings.Repeat("x", 128)), 200) // 128 is the limit

	// exactly 12 characters, counted as characters rather than bytes
	app = newTestApp(t)
	equal(t, setup(app, "admin", strings.Repeat("密碼", 6)), 200)
}

func TestLoginNeedsTheRightPassword(t *testing.T) {
	app := newTestApp(t)
	app.setupAdmin(t)
	wrong := `{"username":"admin","password":"nope nope"}`
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "", wrong), 401)
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "", adminBody), 200)
}

func TestLoginRejectsAMalformedBody(t *testing.T) {
	app := newTestApp(t)
	app.setupAdmin(t)
	rec := app.adminDo(t, http.MethodPost, "/api/domus/login", "", `{"username": "x"}`)
	equal(t, rec.Code, 422)
	equal(t, rec.Body.String(),
		"Failed to deserialize the JSON body into the target type: missing field `password` at line 1 column 17")
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "", `{"password":"x"}`), 422)
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "", `{nope`), 400)
}

func TestLogoutInvalidatesTheSession(t *testing.T) {
	app := newTestApp(t)
	cookie := app.setupAdmin(t)
	rec := app.adminDo(t, http.MethodPost, "/api/domus/logout", cookie, "")
	equal(t, rec.Code, 200)
	equal(t, rec.Header().Get("Set-Cookie"), "domus_session=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0")
	equal(t, app.alive(t, cookie), false)
}

func TestLoginIsThrottledPerUsername(t *testing.T) {
	app := newTestApp(t)
	app.adminAndMember(t)
	login := func(user, password string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"username":%q,"password":%q}`, user, password)
		return app.adminDo(t, http.MethodPost, "/api/domus/login", "", body)
	}
	// unknown names are throttled the same way, so a 429 reveals nothing
	for _, name := range []string{"admin", "ghost"} {
		for range 5 {
			equal(t, login(name, "wrong").Code, 401)
		}
		rec := login(name, "correct horse")
		equal(t, rec.Code, 429)
		var wait int
		_, err := fmt.Sscanf(rec.Header().Get("Retry-After"), "%d", &wait)
		noError(t, err)
		if wait < 1 || wait > 60 {
			t.Fatalf("%s: Retry-After %d", name, wait)
		}
		equal(t, bodyString(rec), fmt.Sprintf(`{"message":"Too many failed attempts. Try again in %d seconds."}`, wait))
	}
	// the lock is case-insensitive and does not spill onto other users
	equal(t, login("ADMIN", "correct horse").Code, 429)
	equal(t, login("bob", bobsSecret).Code, 200)
}

func TestSuccessfulLoginResetsTheFailureCount(t *testing.T) {
	app := newTestApp(t)
	app.adminAndMember(t)
	login := func(password string) int {
		body := fmt.Sprintf(`{"username":"bob","password":%q}`, password)
		return app.adminStatus(t, http.MethodPost, "/api/domus/login", "", body)
	}
	for range 4 {
		equal(t, login("wrong"), 401)
	}
	equal(t, login(bobsSecret), 200)
	for range 4 {
		equal(t, login("wrong"), 401)
	}
	equal(t, login(bobsSecret), 200)
}

func TestLockLiftsWhenTheClockPassesIt(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	app := newTestAppWith(t, func(cfg *Config) { cfg.Now = func() time.Time { return now } })
	app.setupAdmin(t)
	wrong := `{"username":"admin","password":"nope nope"}`
	for range 5 {
		equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "", wrong), 401)
	}
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "", adminBody), 429)
	now = now.Add(61 * time.Second)
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "", adminBody), 200)
}

func TestPasswordChangeIsThrottled(t *testing.T) {
	app := newTestApp(t)
	_, bob := app.adminAndMember(t)
	change := func(current string) int {
		body := fmt.Sprintf(`{"current":%q,"new":"brand new pw 12"}`, current)
		return app.adminStatus(t, http.MethodPost, "/api/domus/password", bob, body)
	}
	for range 5 {
		equal(t, change("wrong"), 401)
	}
	equal(t, change(bobsSecret), 429)
}

func TestChangingOwnPassword(t *testing.T) {
	app := newTestApp(t)
	_, bob := app.adminAndMember(t)
	change := func(body string) *httptest.ResponseRecorder {
		return app.adminDo(t, http.MethodPost, "/api/domus/password", bob, body)
	}
	equal(t, change(`{"current":"wrong","new":"brand new pw"}`).Code, 401)
	rec := change(`{"current":"bob's password","new":"short"}`)
	equal(t, rec.Code, 400)
	equal(t, bodyString(rec), `{"message":"Password must be 12 to 128 characters and must not be the username."}`)
	rec = change(`{"current":"bob's password","new":"brand new pw"}`)
	equal(t, rec.Code, 200)
	// the old session is gone, the fresh one works
	equal(t, app.alive(t, bob), false)
	equal(t, app.alive(t, cookieOf(t, rec)), true)
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "",
		`{"username":"bob","password":"brand new pw"}`), 200)
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "",
		`{"username":"bob","password":"bob's password"}`), 401)
}

func TestPasswordChangeEndsEverySessionAndAppLogin(t *testing.T) {
	app := newTestApp(t)
	_, bob := app.adminAndMember(t)
	other := app.login(t, "bob", bobsSecret)
	bobID := app.userID(t, "bob")
	refreshID, err := app.store.RefreshCreate(bg, bobID, "client", "refresh-hash")
	noError(t, err)

	body := `{"current":"bob's password","new":"brand new pw"}`
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/password", bob, body), 200)
	equal(t, app.alive(t, other), false)
	live, err := app.store.RefreshExists(bg, refreshID)
	noError(t, err)
	equal(t, live, false)
}

func (a *testApp) userID(t *testing.T, name string) int64 {
	t.Helper()
	user, _, err := a.store.UserLogin(bg, name)
	noError(t, err)
	return user.ID
}

func TestSecureCookieBehindAnHTTPSProxy(t *testing.T) {
	for _, proto := range []string{"https", "HTTPS"} {
		app := newTestApp(t)
		rec := app.do(t, request{method: http.MethodPost, path: "/api/domus/setup", body: adminBody,
			headers: map[string]string{"Content-Type": "application/json", "X-Forwarded-Proto": proto}})
		equal(t, rec.Code, 200)
		cookie := rec.Header().Get("Set-Cookie")
		if !strings.HasSuffix(cookie, "; Max-Age=604800; Secure") {
			t.Fatalf("%s: cookie %q", proto, cookie)
		}
		out := app.do(t, request{method: http.MethodPost, path: "/api/domus/logout",
			headers: map[string]string{"Cookie": cookieOfString(cookie), "X-Forwarded-Proto": proto}})
		equal(t, out.Header().Get("Set-Cookie"), "domus_session=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0; Secure")
	}
}

func cookieOfString(setCookie string) string {
	pair, _, _ := strings.Cut(setCookie, ";")
	return pair
}

func TestSessionCookieAttributesAndOrder(t *testing.T) {
	equal(t, sessionCookie("abc", 604800, false), "domus_session=abc; HttpOnly; SameSite=Strict; Path=/; Max-Age=604800")
	equal(t, sessionCookie("abc", 86400, true), "domus_session=abc; HttpOnly; SameSite=Strict; Path=/; Max-Age=86400; Secure")
	equal(t, sessionCookie("", 0, false), "domus_session=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0")
}

func TestSessionTokenIsReadFromTheCookieHeader(t *testing.T) {
	read := func(header string) (string, bool) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Cookie", header)
		}
		return sessionToken(r)
	}
	for header, want := range map[string]string{
		"domus_session=abc":                   "abc",
		"a=1; domus_session=abc; b=2":         "abc",
		"  domus_session=abc  ":               "abc",
		"domus_session=a=b":                   "a=b",
		"domus_session=":                      "",
		"x_domus_session=no; domus_session=y": "y",
	} {
		got, ok := read(header)
		if !ok || got != want {
			t.Errorf("%q: got %q %v, want %q", header, got, ok, want)
		}
	}
	for _, header := range []string{"", "other=1", "domus_session", "Domus_Session=abc"} {
		if got, ok := read(header); ok {
			t.Errorf("%q: found %q", header, got)
		}
	}
}

func TestTheUserAgentIsReadFromTheHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	equal(t, userAgent(r), "")
	r.Header.Set("User-Agent", "Companion/1.0")
	equal(t, userAgent(r), "Companion/1.0")
}

func TestProtectedRoutesNeedASession(t *testing.T) {
	app := newTestApp(t)
	for _, route := range [][2]string{
		{http.MethodPost, "/api/domus/logout"},
		{http.MethodPost, "/api/domus/password"},
		{http.MethodPost, "/api/domus/session/reauth"},
		{http.MethodGet, "/api/domus/sessions"},
		{http.MethodPost, "/api/domus/sessions/revoke-others"},
		{http.MethodDelete, "/api/domus/sessions/1"},
		{http.MethodGet, "/api/domus/events"},
		{http.MethodGet, "/api/domus/lights"},
		{http.MethodGet, "/api/domus/rooms"},
		{http.MethodPost, "/api/domus/devices/test"},
		{http.MethodGet, "/api/domus/groups"},
		{http.MethodPut, "/api/domus/groups/G"},
		{http.MethodDelete, "/api/domus/groups/G"},
		{http.MethodGet, "/api/domus/tokens"},
		{http.MethodPost, "/api/domus/tokens"},
		{http.MethodDelete, "/api/domus/tokens/1"},
		{http.MethodGet, "/api/domus/apps"},
		{http.MethodDelete, "/api/domus/apps/1"},
		{http.MethodGet, "/api/domus/hue"},
		{http.MethodPost, "/api/domus/hue/pair"},
		{http.MethodGet, "/api/domus/users"},
		{http.MethodPost, "/api/domus/users"},
		{http.MethodPut, "/api/domus/users/1"},
		{http.MethodDelete, "/api/domus/users/1"},
	} {
		rec := app.adminDo(t, route[0], route[1], "", "{}")
		equal(t, rec.Code, 401)
		equal(t, bodyString(rec), `{"message":"Login required."}`)
	}
	equal(t, app.adminStatus(t, http.MethodGet, "/api/domus/sessions", "domus_session=forged", ""), 401)
}

func TestAWrongMethodIsAnsweredAfterTheSessionCheck(t *testing.T) {
	app := newTestApp(t)
	rec := app.adminDo(t, http.MethodPost, "/api/domus/sessions", "", "{}")
	equal(t, rec.Code, 401)
	equal(t, rec.Header().Get("Allow"), "GET,HEAD")

	cookie := app.setupAdmin(t)
	rec = app.adminDo(t, http.MethodPost, "/api/domus/sessions", cookie, "{}")
	equal(t, rec.Code, 405)
	equal(t, rec.Header().Get("Allow"), "GET,HEAD")
	equal(t, rec.Body.Len(), 0)

	// a public path answers 405 to anyone
	rec = app.adminDo(t, http.MethodGet, "/api/domus/login", "", "")
	equal(t, rec.Code, 405)
	equal(t, rec.Header().Get("Allow"), "POST")
	equal(t, app.adminStatus(t, http.MethodHead, "/api/domus/status", "", ""), 200)
}

func TestSessionsAreListedAndRevokedPerUser(t *testing.T) {
	app := newTestApp(t)
	admin, bob := app.adminAndMember(t)
	bob2 := app.login(t, "bob", bobsSecret)

	code, body := app.adminJSON(t, http.MethodGet, "/api/domus/sessions", bob, "")
	equal(t, code, 200)
	list := asList(t, body)
	equal(t, len(list), 2)
	current := 0
	for _, item := range list {
		session := asMap(t, item)
		if session["current"] == true {
			current++
		}
		if _, leaked := session["token_hash"]; leaked {
			t.Fatal("the session list leaks the token hash")
		}
	}
	equal(t, current, 1)
	// the admin's own list holds only the admin's session
	_, body = app.adminJSON(t, http.MethodGet, "/api/domus/sessions", admin, "")
	theirs := asList(t, body)
	equal(t, len(theirs), 1)
	adminSessionID := asMap(t, theirs[0])["id"].(float64)

	// someone else's session cannot be ended, and a bogus id is just as unknown
	for _, id := range []float64{adminSessionID, 9999} {
		path := fmt.Sprintf("/api/domus/sessions/%d", int(id))
		equal(t, app.adminStatus(t, http.MethodDelete, path, bob, ""), 404)
	}
	equal(t, app.alive(t, admin), true)

	// ending the other bob session logs that browser out
	var other float64
	for _, item := range list {
		if session := asMap(t, item); session["current"] == false {
			other = session["id"].(float64)
		}
	}
	rec := app.adminDo(t, http.MethodDelete, fmt.Sprintf("/api/domus/sessions/%d", int(other)), bob, "")
	equal(t, rec.Code, 200)
	equal(t, bodyString(rec), `{"ok":true}`)
	if app.alive(t, bob) == app.alive(t, bob2) {
		t.Fatal("exactly one of the two sessions survives")
	}

	// revoke-others keeps the caller's session
	bob3 := app.login(t, "bob", bobsSecret)
	code, body = app.adminJSON(t, http.MethodPost, "/api/domus/sessions/revoke-others", bob3, "")
	equal(t, code, 200)
	equal(t, asMap(t, body)["ended"], float64(1))
	equal(t, app.alive(t, bob3), true)
	equal(t, app.alive(t, admin), true)
	_, body = app.adminJSON(t, http.MethodGet, "/api/domus/sessions", bob3, "")
	equal(t, len(asList(t, body)), 1)
}

func TestARevokedSessionIdMustBeANumber(t *testing.T) {
	app := newTestApp(t)
	cookie := app.setupAdmin(t)
	equal(t, app.adminStatus(t, http.MethodDelete, "/api/domus/sessions/abc", cookie, ""), 400)
}

func TestCrossSiteRequestsAreRefused(t *testing.T) {
	app := newTestApp(t)
	send := func(method string, headers map[string]string) int {
		headers["Host"] = "domus.example"
		return app.do(t, request{method: method, path: "/api/domus/logout", headers: headers}).Code
	}
	// no Origin (curl, hasscontrol) passes the check; the route then needs a session
	for name, headers := range map[string]map[string]string{
		"no origin":      {},
		"same origin":    {"Origin": "https://domus.example"},
		"host case":      {"Origin": "https://DOMUS.example"},
		"behind a proxy": {"Origin": "https://domus.example", "Host": "127.0.0.1:8123", "X-Forwarded-Host": "domus.example"},
		"fetch metadata": {"Sec-Fetch-Site": "same-origin"},
		"typed address":  {"Sec-Fetch-Site": "none"},
		"proxy chain":    {"Origin": "https://domus.example", "X-Forwarded-Host": " domus.example , other.example"},
	} {
		if name == "behind a proxy" {
			// send overwrites Host, so build this one by hand
			rec := app.do(t, request{method: http.MethodPost, path: "/api/domus/logout", headers: headers})
			equal(t, rec.Code, 401)
			continue
		}
		equal(t, send(http.MethodPost, headers), 401)
	}
	for name, test := range map[string]struct {
		method  string
		headers map[string]string
	}{
		"other origin":     {http.MethodPost, map[string]string{"Origin": "https://evil.example"}},
		"null origin":      {http.MethodPost, map[string]string{"Origin": "null"}},
		"other on delete":  {http.MethodDelete, map[string]string{"Origin": "https://evil.example"}},
		"cross-site":       {http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}},
		"same-site":        {http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site"}},
		"fetch beats host": {http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://domus.example"}},
		"origin port":      {http.MethodPost, map[string]string{"Origin": "https://domus.example:8443"}},
		"forwarded other":  {http.MethodPost, map[string]string{"Origin": "https://evil.example", "X-Forwarded-Host": "domus.example, evil.example"}},
	} {
		if got := send(test.method, test.headers); got != 403 {
			t.Errorf("%s: status %d, want 403", name, got)
		}
	}
	// reads are never blocked
	if got := send(http.MethodGet, map[string]string{"Origin": "https://evil.example"}); got == 403 {
		t.Errorf("a GET was refused")
	}
}

func TestCrossSiteRefusalCoversLoginAndSetup(t *testing.T) {
	app := newTestApp(t)
	for _, path := range []string{"/api/domus/login", "/api/domus/setup"} {
		rec := app.do(t, request{method: http.MethodPost, path: path, body: adminBody,
			headers: map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"}})
		equal(t, rec.Code, 403)
		equal(t, bodyString(rec), `{"message":"Cross-site request refused."}`)
	}
	equal(t, app.adminStatus(t, http.MethodGet, "/api/domus/status", "", ""), 200)
	// even a wrong method is judged by origin first
	rec := app.do(t, request{method: http.MethodDelete, path: "/api/domus/login",
		headers: map[string]string{"Origin": "http://evil.example"}})
	equal(t, rec.Code, 403)
	equal(t, rec.Header().Get("Allow"), "")
}

func newCodeApp(t *testing.T) *testApp {
	t.Helper()
	return newTestAppWith(t, func(cfg *Config) { cfg.SetupCode = "abc123" })
}

func TestSetupNeedsTheOneTimeCode(t *testing.T) {
	app := newCodeApp(t)
	setup := func(code string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"username":"admin","password":"correct horse","setup_code":%q}`, code)
		return app.adminDo(t, http.MethodPost, "/api/domus/setup", "", body)
	}
	_, body := app.adminJSON(t, http.MethodGet, "/api/domus/status", "", "")
	equal(t, asMap(t, body)["setup_code_required"], true)

	equal(t, setup("").Code, 403)
	rec := setup("nope")
	equal(t, rec.Code, 403)
	equal(t, bodyString(rec), `{"message":"Wrong setup code. Find it in the domus log."}`)
	missing := `{"username":"admin","password":"correct horse"}`
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/setup", "", missing), 403)
	rec = setup(" ABC-123 ")
	equal(t, rec.Code, 200)
	if rec.Header().Get("Set-Cookie") == "" {
		t.Fatal("no session cookie")
	}

	// the code is spent: it cannot be reused, and the page stops asking for it
	equal(t, setup("abc123").Code, 409)
	_, body = app.adminJSON(t, http.MethodGet, "/api/domus/status", "", "")
	equal(t, asMap(t, body)["setup_code_required"], false)
}

func TestWrongSetupCodesAreThrottled(t *testing.T) {
	app := newCodeApp(t)
	body := func(code string) string {
		return fmt.Sprintf(`{"username":"admin","password":"correct horse","setup_code":%q}`, code)
	}
	for range 5 {
		equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/setup", "", body("bad")), 403)
	}
	// even the right code waits out the lock
	rec := app.adminDo(t, http.MethodPost, "/api/domus/setup", "", body("abc123"))
	equal(t, rec.Code, 429)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
}

func TestSetupCodesCompareWithoutDashesAndCase(t *testing.T) {
	equal(t, plainSetupCode(" ab-C1-2 "), "ABC12")
	equal(t, plainSetupCode("ABC12"), plainSetupCode("abc-12"))
}

// logBuffer collects log output; the test handler calls it from one goroutine
// at a time.
type logBuffer struct{ bytes.Buffer }

func TestAuditLogEscapesUsernamesSoTheyCannotForgeFields(t *testing.T) {
	var log logBuffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	app := newTestAppWith(t, func(cfg *Config) { cfg.Logger = logger })
	app.setupAdmin(t)
	name := `x" by="root`
	app.addMember(t, name, "long enough pw12")
	body := fmt.Sprintf(`{"username":%q,"password":"long enough pw12"}`, name)
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "", body), 200)
	text := log.String()
	if !strings.Contains(text, `user="x\" by=\"root"`) {
		t.Fatalf("the audit line does not escape the name: %s", text)
	}
	if strings.Contains(text, `user=x" by="root`) {
		t.Fatalf("the audit line can be forged: %s", text)
	}
	if !strings.Contains(text, "target=audit") {
		t.Fatalf("no audit target: %s", text)
	}
}

func TestFailedAndRefusedLoginsAreAudited(t *testing.T) {
	var log logBuffer
	app := newTestAppWith(t, func(cfg *Config) { cfg.Logger = slog.New(slog.NewTextHandler(&log, nil)) })
	app.setupAdmin(t)
	wrong := `{"username":"admin","password":"nope nope"}`
	for range 5 {
		app.adminStatus(t, http.MethodPost, "/api/domus/login", "", wrong)
	}
	app.adminStatus(t, http.MethodPost, "/api/domus/login", "", adminBody)
	for _, want := range []string{"first admin created", "login failed", "login refused: locked"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("no %q in the audit log: %s", want, log.String())
		}
	}
}

// appLogin creates an app login (refresh token and access token) for the user
// and returns the access token.
func (a *testApp) appLogin(t *testing.T, userID int64, name string) string {
	t.Helper()
	refreshID, err := a.store.RefreshCreate(bg, userID, "client", "refresh-"+name)
	noError(t, err)
	access := "access-" + name
	noError(t, a.store.CreateAppToken(bg, store.NewAppToken{
		UserID: userID, Name: name, TokenHash: auth.SHA256Hex(access),
		Expires: time.Now().Unix() + 1800, RefreshID: refreshID,
	}))
	return access
}

func (a *testApp) fromToken(t *testing.T, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{"User-Agent": "Companion/1.0"}
	if authorization != "" {
		headers["Authorization"] = authorization
	}
	return a.do(t, request{method: http.MethodPost, path: "/api/domus/session/from-token", headers: headers})
}

func TestAnAppTokenStartsASessionThatEndsWithTheAppLogin(t *testing.T) {
	app := newTestApp(t)
	app.setupAdmin(t)
	access := app.appLogin(t, 1, "phone")
	rec := app.fromToken(t, "Bearer "+access)
	equal(t, rec.Code, 200)
	if !strings.HasSuffix(rec.Header().Get("Set-Cookie"), "; Max-Age=86400") {
		t.Fatalf("cookie %q", rec.Header().Get("Set-Cookie"))
	}
	cookie := cookieOf(t, rec)
	_, body := app.adminJSON(t, http.MethodGet, "/api/domus/status", cookie, "")
	equal(t, asMap(t, body)["logged_in"], true)
	_, body = app.adminJSON(t, http.MethodGet, "/api/domus/sessions", cookie, "")
	equal(t, asMap(t, asList(t, body)[0])["user_agent"], "HA app · Companion/1.0")

	// logging the app out (what iOS does) also ends the page session
	noError(t, app.store.AccessRevoke(bg, auth.SHA256Hex(access)))
	_, body = app.adminJSON(t, http.MethodGet, "/api/domus/status", cookie, "")
	equal(t, asMap(t, body)["logged_in"], false)
}

func TestOnlyAppLoginTokensMayStartASession(t *testing.T) {
	app := newTestApp(t)
	app.setupAdmin(t)
	for _, authorization := range []string{"", "Bearer nope", "Basic abc", "Bearer"} {
		rec := app.fromToken(t, authorization)
		equal(t, rec.Code, 401)
		equal(t, bodyString(rec), `{"message":"Invalid token."}`)
	}
	// an ordinary API token is not accepted either
	app.addToken(t, "domus_plain", store.NewToken{UserID: 1, Name: "plain"})
	rec := app.fromToken(t, "Bearer domus_plain")
	equal(t, rec.Code, 401)
	equal(t, rec.Header().Get("Set-Cookie"), "")
	// the scheme is case-insensitive
	equal(t, app.fromToken(t, "bearer "+app.appLogin(t, 1, "phone")).Code, 200)
}

func TestAppSessionsNeedThePasswordForSensitiveActions(t *testing.T) {
	app := newTestApp(t)
	app.setupAdmin(t)
	app.addMember(t, "bob", bobsSecret)
	cookie := cookieOf(t, app.fromToken(t, "Bearer "+app.appLogin(t, 1, "phone")))

	mux := http.NewServeMux()
	routes := newAdminRoutes(app.app, mux)
	reached := func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) }
	routes.route("POST /sensitive", needAdmin|needRecentAuth, reached)
	routes.route("GET /admin", needAdmin, reached)
	routes.route("POST /elevated", needRecentAuth, reached)
	routes.mount()
	send := func(method, path, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, adminPrefix+path, nil)
		r.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}

	rec := send(http.MethodPost, "/sensitive", cookie)
	equal(t, rec.Code, 403)
	equal(t, bodyString(rec), `{"message":"Enter your password to continue.","reauth":true}`)
	equal(t, send(http.MethodGet, "/admin", cookie).Code, 200)

	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/session/reauth", cookie, `{"password":"nope"}`), 401)
	rec = app.adminDo(t, http.MethodPost, "/api/domus/session/reauth", cookie, `{"password":"correct horse"}`)
	equal(t, rec.Code, 200)
	equal(t, bodyString(rec), `{"ok":true}`)
	equal(t, send(http.MethodPost, "/sensitive", cookie).Code, 200)

	// a password session was never app-started, so it is never asked again
	equal(t, send(http.MethodPost, "/sensitive", app.login(t, "admin", "correct horse")).Code, 200)
	// a member passes the session check but not the admin one, before any reauth
	member := app.login(t, "bob", bobsSecret)
	rec = send(http.MethodPost, "/sensitive", member)
	equal(t, rec.Code, 403)
	equal(t, bodyString(rec), `{"message":"Admin only."}`)
	equal(t, send(http.MethodPost, "/sensitive", "").Code, 401)
}

func TestReauthSharesThePasswordThrottle(t *testing.T) {
	app := newTestApp(t)
	cookie := app.setupAdmin(t)
	for range 5 {
		equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/session/reauth", cookie, `{"password":"nope"}`), 401)
	}
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/session/reauth", cookie, `{"password":"correct horse"}`), 429)
	// the password change draws on the same counter
	change := `{"current":"correct horse","new":"another one pw12"}`
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/password", cookie, change), 429)
}

func TestFromTokenIsNotThrottledOrCounted(t *testing.T) {
	app := newTestApp(t)
	app.setupAdmin(t)
	access := app.appLogin(t, 1, "phone")
	for range 3 {
		equal(t, app.fromToken(t, "Bearer "+access).Code, 200)
	}
}

var hex64 = regexp.MustCompile(`[0-9a-f]{64}`)

// replayGolden sends the golden's request, including its body, and compares
// the status, the headers it lists and a JSON body. A plain-text rejection
// compares by status only, because Go's parser words its errors differently.
// replacements are old, new pairs applied to the body first.
func (a *testApp) replayGolden(t *testing.T, name string, replacements ...string) *httptest.ResponseRecorder {
	t.Helper()
	g := loadGolden(t, name)
	body := ""
	switch {
	case g.Request.Body != nil:
		var compact bytes.Buffer
		noError(t, json.Compact(&compact, g.Request.Body))
		body = compact.String()
	case g.Request.BodyText != nil:
		body = *g.Request.BodyText
	}
	body = strings.NewReplacer(replacements...).Replace(body)
	rec := a.do(t, request{method: g.Request.Method, path: g.Request.Path, headers: g.Request.Headers, body: body})
	want := g.Response
	if rec.Code != want.Status {
		t.Errorf("%s: status %d, want %d", name, rec.Code, want.Status)
	}
	for header, value := range want.Headers {
		if strings.Contains(value, "<") {
			continue // a placeholder for a random or timing value
		}
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s: header %s = %q, want %q", name, header, got, value)
		}
	}
	if want.BodyEmpty && rec.Body.Len() != 0 {
		t.Errorf("%s: body %q, want empty", name, rec.Body)
	}
	if want.Body != nil && !strings.Contains(string(want.Body), "<") && !jsonEqual(t, rec.Body.Bytes(), want.Body) {
		t.Errorf("%s: body %s, want %s", name, rec.Body, want.Body)
	}
	if want.BodyText != nil && strings.HasPrefix(*want.BodyText, "Expected request") && rec.Body.String() != *want.BodyText {
		t.Errorf("%s: body %q, want %q", name, rec.Body, *want.BodyText)
	}
	return rec
}

func TestStatelessAdminGoldens(t *testing.T) {
	for _, name := range []string{
		"admin_status_before_setup",
		"admin_login_get_405",
		"admin_login_bad_json_400",
		"admin_login_wrong_content_type_415",
		"admin_login_cross_site_origin_403",
		"admin_login_origin_null_403",
		"admin_login_sec_fetch_cross_site_403",
		"admin_login_same_origin_header_ok_401",
		"admin_login_before_setup_401",
		"admin_login_unknown_user_401",
		"admin_unknown_route_404",
	} {
		t.Run(name, func(t *testing.T) { newTestApp(t).replayGolden(t, name) })
	}
}

// The setup goldens were captured with a setup code; <SETUP_CODE> stands for
// it. The golden admin_setup_bad_username_400 is left out: it carries an older
// username hint than the Rust source (docs: domus-go-deviations.md).
func TestSetupGoldens(t *testing.T) {
	app := newCodeApp(t)
	app.replayGolden(t, "admin_status_before_setup_code_required")
	for _, name := range []string{"admin_setup_short_password_400", "admin_setup_password_equals_username_400"} {
		app.replayGolden(t, name, "<SETUP_CODE>", "abc123")
	}
	for _, name := range []string{"admin_setup_no_code_403", "admin_setup_wrong_code_403"} {
		app.replayGolden(t, name)
	}
}

func TestLoginGoldensAfterSetup(t *testing.T) {
	app := newTestApp(t)
	body := `{"username":"admin","password":"Fixture-Admin-Pass-1"}`
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/setup", "", body), 200)
	for _, name := range []string{"admin_login_ok_https", "admin_login_ok_upper_case_username"} {
		rec := app.replayGolden(t, name)
		if cookie := rec.Header().Get("Set-Cookie"); !hex64.MatchString(cookie) {
			t.Errorf("%s: the cookie does not carry 64 hex digits: %q", name, cookie)
		}
	}
	app.replayGolden(t, "admin_login_wrong_password_401")
	app.replayGolden(t, "admin_setup_again_409")
}
