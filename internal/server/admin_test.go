package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	newTestApp(t).expect(t, request{path: statusPath}, adminReply(200, anonymousStatus(false, false)))
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

const (
	loginPath  = "/api/domus/login"
	setupPath  = "/api/domus/setup"
	statusPath = "/api/domus/status"

	wrongLogin       = `{"message":"Wrong username or password."}`
	loginNeeded      = `{"message":"Login required."}`
	crossSiteRefused = `{"message":"Cross-site request refused."}`
	badPassword      = `{"message":"Password must be 12 to 128 characters and must not be the username."}`
	wrongSetupCode   = `{"message":"Wrong setup code. Find it in the domus log."}`

	// newSession is the cookie of a password login; the value is random.
	newSession = "domus_session=<HEX64>; HttpOnly; SameSite=Strict; Path=/; Max-Age=604800"
)

// adminReply is a JSON answer of the admin API; none of them may be stored.
func adminReply(status int, body string) reply {
	return reply{status: status, contentType: jsonType, cacheControl: noStore, json: body}
}

func adminPost(path, body string) request {
	return request{
		method: http.MethodPost, path: path, body: body,
		headers: map[string]string{"Content-Type": "application/json"},
	}
}

// withHeaders is req with more request headers.
func withHeaders(req request, more map[string]string) request {
	headers := map[string]string{}
	for name, value := range req.headers {
		headers[name] = value
	}
	for name, value := range more {
		headers[name] = value
	}
	req.headers = headers
	return req
}

func TestTheAdminAPIRefusesBadRequestsBeforeAnyLogin(t *testing.T) {
	login := adminPost(loginPath, `{"username":"x","password":"y"}`)
	expectEach(t, []exchange{
		{
			name:    "a login before the first admin exists is a wrong login",
			request: login,
			reply:   adminReply(401, wrongLogin),
		},
		{
			name:    "a login of an unknown user is a wrong login",
			request: adminPost(loginPath, `{"username":"ghost","password":"wrong password!"}`),
			reply:   adminReply(401, wrongLogin),
		},
		{
			name:    "a JSON body needs the JSON content type",
			request: withHeaders(login, map[string]string{"Content-Type": "text/plain"}),
			reply: reply{
				status: 415, contentType: textType, cacheControl: noStore,
				text: "Expected request with `Content-Type: application/json`",
			},
		},
		{
			name:    "a GET on the login route is refused with the allowed method",
			request: request{path: loginPath},
			reply:   reply{status: 405, cacheControl: noStore, headers: map[string]string{"Allow": "POST"}},
		},
		{
			name:    "a request from another origin is refused",
			request: withHeaders(login, map[string]string{"Origin": "http://evil.example"}),
			reply:   adminReply(403, crossSiteRefused),
		},
		{
			name:    "a request with the origin null is refused",
			request: withHeaders(login, map[string]string{"Origin": "null"}),
			reply:   adminReply(403, crossSiteRefused),
		},
		{
			name:    "a request that fetch metadata calls cross-site is refused",
			request: withHeaders(login, map[string]string{"Sec-Fetch-Site": "cross-site"}),
			reply:   adminReply(403, crossSiteRefused),
		},
		{
			name: "a request from the same origin gets as far as the login",
			request: withHeaders(login, map[string]string{
				"Host": "127.0.0.1:8123", "Origin": "http://127.0.0.1:8123", "Sec-Fetch-Site": "same-origin",
			}),
			reply: adminReply(401, wrongLogin),
		},
		{
			name:    "an unknown admin route is an empty 404",
			request: request{path: "/api/domus/nope"},
			reply:   reply{status: 404, cacheControl: noStore},
		},
	})
}

// The wording of the parser's message is not compared: Go words it differently
// from the Rust server (a known, accepted difference), the status matches.
func TestALoginBodyThatIsNotJSONIsAPlainTextBadRequest(t *testing.T) {
	rec := newTestApp(t).do(t, adminPost(loginPath, "{nope"))
	equal(t, rec.Code, 400)
	equal(t, rec.Header().Get("Content-Type"), textType)
	equal(t, rec.Header().Get("Cache-Control"), noStore)
}

func TestProtectedRoutesAskAnonymousVisitorsToLogInBeforeAndAfterSetup(t *testing.T) {
	cases := []exchange{
		{name: "the lights", request: request{path: "/api/domus/lights"}, reply: adminReply(401, loginNeeded)},
		{
			name: "a group update",
			request: request{
				method: http.MethodPut, path: "/api/domus/groups/x", body: `{"members":[]}`,
				headers: map[string]string{"Content-Type": "application/json"},
			},
			reply: adminReply(401, loginNeeded),
		},
		{
			name:    "a token creation",
			request: adminPost("/api/domus/tokens", `{"name":"x"}`),
			reply:   adminReply(401, loginNeeded),
		},
		{
			name:    "a password check for sensitive actions",
			request: adminPost("/api/domus/session/reauth", `{"password":"correct horse"}`),
			reply:   adminReply(401, loginNeeded),
		},
		{
			name:    "a session from an app token without the token",
			request: request{method: http.MethodPost, path: "/api/domus/session/from-token"},
			reply:   adminReply(401, `{"message":"Invalid token."}`),
		},
	}
	t.Run("before setup", func(t *testing.T) { newTestApp(t).expectInOrder(t, cases) })
	t.Run("after setup", func(t *testing.T) {
		app := newTestApp(t)
		app.setupAdmin(t)
		app.expectInOrder(t, cases)
	})
}

// anonymousStatus is the status a visitor without a session gets.
func anonymousStatus(setupDone, codeRequired bool) string {
	return fmt.Sprintf(
		`{"hue_paired":false,"logged_in":false,"setup_code_required":%t,"setup_done":%t,"user":null,"version":%q}`,
		codeRequired, setupDone, testVersion)
}

// The steps run in order: the setup changes what the later ones see. The code
// is sent in lower case and without the dash.
func TestTheFirstAdminIsSetUpWithTheCodeAndThenLogsIn(t *testing.T) {
	app := newTestAppWith(t, func(cfg *Config) { cfg.SetupCode = "CD42Y-6YC3Y" })
	setup := func(body string) request { return adminPost(setupPath, body) }
	app.expectInOrder(t, []exchange{
		{
			name:    "the status asks for the setup code",
			request: request{path: statusPath},
			reply:   adminReply(200, anonymousStatus(false, true)),
		},
		{
			name:    "a setup without the code is refused",
			request: setup(adminBody),
			reply:   adminReply(403, wrongSetupCode),
		},
		{
			name:    "a setup with a wrong code is refused",
			request: setup(`{"username":"admin","password":"correct horse","setup_code":"nope"}`),
			reply:   adminReply(403, wrongSetupCode),
		},
		{
			name:    "a short password is refused once the code is right",
			request: setup(`{"username":"admin","password":"short","setup_code":"cd42y6yc3y"}`),
			reply:   adminReply(400, badPassword),
		},
		{
			name:    "a password that equals the username in any case is refused",
			request: setup(`{"username":"Hunter2-Hunter2","password":"hunter2-hunter2","setup_code":"cd42y6yc3y"}`),
			reply:   adminReply(400, badPassword),
		},
		{
			name:    "the right code creates the admin and starts a session",
			request: setup(`{"username":"admin","password":"correct horse","setup_code":"cd42y6yc3y"}`),
			reply: reply{
				status: 200, contentType: jsonType, cacheControl: noStore, json: `{"ok":true}`,
				headers: map[string]string{"Set-Cookie": newSession},
			},
		},
		{
			name:    "the status stops asking for the code",
			request: request{path: statusPath},
			reply:   adminReply(200, anonymousStatus(true, false)),
		},
		{
			name:    "a second setup is refused",
			request: setup(`{"username":"admin","password":"correct horse","setup_code":"cd42y6yc3y"}`),
			reply:   adminReply(409, `{"message":"Already set up."}`),
		},
		{
			name:    "a wrong password is refused",
			request: adminPost(loginPath, `{"username":"admin","password":"wrong password!"}`),
			reply:   adminReply(401, wrongLogin),
		},
		{
			name:    "an unknown user is refused the same way",
			request: adminPost(loginPath, `{"username":"mallory ","password":"wrong password!"}`),
			reply:   adminReply(401, wrongLogin),
		},
		{
			name:    "behind an https proxy the cookie is Secure and the answer carries HSTS",
			request: withHeaders(adminPost(loginPath, adminBody), map[string]string{"X-Forwarded-Proto": "https"}),
			reply: reply{
				status: 200, contentType: jsonType, cacheControl: noStore, json: `{"ok":true}`,
				headers: map[string]string{
					"Set-Cookie":                newSession + "; Secure",
					"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
				},
			},
		},
		{
			name:    "the username is trimmed and not case sensitive",
			request: adminPost(loginPath, `{"username":" ADMIN ","password":"correct horse"}`),
			reply: reply{
				status: 200, contentType: jsonType, cacheControl: noStore, json: `{"ok":true}`,
				headers: map[string]string{"Set-Cookie": newSession},
			},
		},
	})
}
