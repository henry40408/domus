package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/store"
)

const (
	iosClient     = "https://home-assistant.io/iOS"
	androidClient = "https://home-assistant.io/android"
	appRedirect   = "homeassistant://auth-callback"
	oauthPassword = "correct-horse-battery"
)

// newOAuthApp is a test app with the admin "admin" (id 1) set up directly in
// the store.
func newOAuthApp(t *testing.T) *testApp {
	t.Helper()
	app := newTestApp(t)
	_, err := app.store.CreateFirstAdmin(bg, "admin", auth.HashPassword(oauthPassword))
	noError(t, err)
	return app
}

func decodeObject(rec *httptest.ResponseRecorder) map[string]any {
	var object map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &object)
	return object
}

func (a *testApp) loginFlow(t *testing.T, username, password, clientID, redirectURI string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"username": username, "password": password, "client_id": clientID, "redirect_uri": redirectURI,
	})
	noError(t, err)
	return a.do(t, request{
		method: http.MethodPost, path: "/auth/login_flow", body: string(body),
		headers: map[string]string{"Content-Type": "application/json"},
	})
}

func (a *testApp) loginCode(t *testing.T, password string) (int, map[string]any) {
	t.Helper()
	rec := a.loginFlow(t, "admin", password, iosClient, appRedirect)
	return rec.Code, decodeObject(rec)
}

func (a *testApp) tokenForm(t *testing.T, form string) (int, map[string]any) {
	t.Helper()
	rec := a.do(t, request{
		method: http.MethodPost, path: "/auth/token", body: form,
		headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	})
	return rec.Code, decodeObject(rec)
}

// stateStatus is the status of a states request with token: 404 when the
// token works (the entity does not exist), 401 when it does not.
func (a *testApp) stateStatus(t *testing.T, token string) int {
	t.Helper()
	return a.do(t, request{path: "/api/states/light.none", headers: map[string]string{"Authorization": "Bearer " + token}}).Code
}

func str(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, ok := object[key].(string)
	if !ok {
		t.Fatalf("no string %q in %#v", key, object)
	}
	return value
}

// codeGrant trades a code for tokens.
func (a *testApp) codeGrant(t *testing.T, code, clientID string) (int, map[string]any) {
	t.Helper()
	return a.tokenForm(t, "grant_type=authorization_code&code="+code+"&client_id="+url.QueryEscape(clientID))
}

func (a *testApp) refreshGrant(t *testing.T, refresh string) (int, map[string]any) {
	t.Helper()
	return a.tokenForm(t, "grant_type=refresh_token&refresh_token="+refresh+"&client_id="+url.QueryEscape(iosClient))
}

// oauthLogin runs the whole login and returns (access token, refresh token).
func (a *testApp) oauthLogin(t *testing.T) (access, refresh string) {
	t.Helper()
	_, body := a.loginCode(t, oauthPassword)
	status, tokens := a.codeGrant(t, str(t, body, "code"), iosClient)
	equal(t, status, 200)
	return str(t, tokens, "access_token"), str(t, tokens, "refresh_token")
}

// fromBearer trades the bearer token for an admin-page session and returns the
// status and the cookie (empty when none was set).
func (a *testApp) fromBearer(t *testing.T, bearer string) (int, string) {
	t.Helper()
	headers := map[string]string{}
	if bearer != "" {
		headers["Authorization"] = "Bearer " + bearer
	}
	rec := a.do(t, request{method: http.MethodPost, path: "/api/domus/session/from-token", headers: headers})
	cookie, _, _ := strings.Cut(rec.Header().Get("Set-Cookie"), ";")
	return rec.Code, cookie
}

func (a *testApp) loggedIn(t *testing.T, cookie string) bool {
	t.Helper()
	_, body := a.adminJSON(t, http.MethodGet, "/api/domus/status", cookie, "")
	return asMap(t, body)["logged_in"] == true
}

func (a *testApp) appSession(t *testing.T, access string) string {
	t.Helper()
	status, cookie := a.fromBearer(t, access)
	equal(t, status, 200)
	return cookie
}

func (a *testApp) firstTokenID(t *testing.T) int64 {
	t.Helper()
	tokens, err := a.store.ListTokens(bg, nil)
	noError(t, err)
	return tokens[0].ID
}

// secondAdmin lets the test delete the first one.
func (a *testApp) secondAdmin(t *testing.T) {
	t.Helper()
	_, err := a.store.CreateUser(bg, "root", "x", true)
	noError(t, err)
}

func TestAuthorizePageChecksClientAndRedirect(t *testing.T) {
	app := newOAuthApp(t)
	ok := app.get(t, "/auth/authorize?response_type=code&client_id="+iosClient+"&redirect_uri="+appRedirect)
	equal(t, ok.Code, 200)
	equal(t, ok.Header().Get("Cache-Control"), "no-store")
	equal(t, ok.Header().Get("Content-Type"), "text/html; charset=utf-8")
	evil := app.get(t, "/auth/authorize?client_id="+iosClient+"&redirect_uri=https://evil.example/cb")
	equal(t, evil.Code, 400)
	equal(t, evil.Body.String(), "Unknown client or redirect URI.")
}

func TestFullFlowLoginTokenRefreshRevoke(t *testing.T) {
	app := newOAuthApp(t)
	status, body := app.loginCode(t, oauthPassword)
	equal(t, status, 200)
	code := str(t, body, "code")

	status, tokens := app.codeGrant(t, code, iosClient)
	equal(t, status, 200)
	equal(t, tokens["token_type"], "Bearer")
	equal(t, tokens["expires_in"], float64(accessTTLSecs))
	access, refresh := str(t, tokens, "access_token"), str(t, tokens, "refresh_token")
	equal(t, app.stateStatus(t, access), 404)

	// a code works once
	status, body = app.codeGrant(t, code, iosClient)
	equal(t, status, 400)
	equal(t, body["error"], "invalid_grant")

	// refreshing mints a new access token and retires the old one
	status, fresh := app.refreshGrant(t, refresh)
	equal(t, status, 200)
	if _, has := fresh["refresh_token"]; has {
		t.Fatal("a refresh grant must not rotate the refresh token")
	}
	access2 := str(t, fresh, "access_token")
	equal(t, app.stateStatus(t, access), 401)
	equal(t, app.stateStatus(t, access2), 404)

	// revoking the access token (what iOS does) also ends the refresh token
	status, _ = app.tokenForm(t, "action=revoke&token="+access2)
	equal(t, status, 200)
	equal(t, app.stateStatus(t, access2), 401)
	status, body = app.refreshGrant(t, refresh)
	equal(t, status, 400)
	equal(t, body["error"], "invalid_grant")
}

func TestRevokeEndpointEndsRefreshToken(t *testing.T) {
	app := newOAuthApp(t)
	access, refresh := app.oauthLogin(t)
	rec := app.do(t, request{
		method: http.MethodPost, path: "/auth/revoke", body: "token=" + refresh,
		headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	})
	equal(t, rec.Code, 200)
	equal(t, rec.Header().Get("Cache-Control"), "no-store")
	equal(t, app.stateStatus(t, access), 401)
}

func TestWrongPasswordAndWrongClientAreRefused(t *testing.T) {
	app := newOAuthApp(t)
	status, _ := app.loginCode(t, "nope")
	equal(t, status, 401)
	_, body := app.loginCode(t, oauthPassword)
	code := str(t, body, "code")
	status, body = app.codeGrant(t, code, androidClient)
	equal(t, status, 400)
	equal(t, body["error"], "invalid_grant")
	// the wrong guess burned the code, even for its own client
	status, body = app.codeGrant(t, code, iosClient)
	equal(t, status, 400)
	equal(t, body["error"], "invalid_grant")
}

func TestLoginFlowSharesTheLoginThrottle(t *testing.T) {
	app := newOAuthApp(t)
	for range 5 {
		status, _ := app.loginCode(t, "nope")
		equal(t, status, 401)
	}
	status, _ := app.loginCode(t, oauthPassword)
	equal(t, status, 429)
	// the admin login is locked by the same failures
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/login", "", adminBody), 429)
}

func TestRevokingTheAppTokenInTheAdminListLogsTheAppOut(t *testing.T) {
	app := newOAuthApp(t)
	access, refresh := app.oauthLogin(t)
	noError(t, app.store.RevokeToken(bg, app.firstTokenID(t), nil))
	equal(t, app.stateStatus(t, access), 401)
	status, _ := app.refreshGrant(t, refresh)
	equal(t, status, 400)
}

func TestPasswordChangeAndUserDeleteEndAppLogins(t *testing.T) {
	app := newOAuthApp(t)
	access, refresh := app.oauthLogin(t)
	noError(t, app.store.DeleteUserRefreshTokens(bg, 1))
	equal(t, app.stateStatus(t, access), 401)
	status, _ := app.refreshGrant(t, refresh)
	equal(t, status, 400)

	_, refresh = app.oauthLogin(t)
	app.secondAdmin(t)
	result, err := app.store.DeleteUserKeepingAnAdmin(bg, 1)
	noError(t, err)
	equal(t, result, store.UserDeleted)
	status, _ = app.refreshGrant(t, refresh)
	equal(t, status, 400)
}

func TestOnlyTheLatestAccessTokenStaysLive(t *testing.T) {
	app := newOAuthApp(t)
	first, refresh := app.oauthLogin(t)
	_, a := app.refreshGrant(t, refresh)
	_, b := app.refreshGrant(t, refresh)
	equal(t, app.stateStatus(t, first), 401)
	equal(t, app.stateStatus(t, str(t, a, "access_token")), 401)
	equal(t, app.stateStatus(t, str(t, b, "access_token")), 404)
}

func TestRevokingTheRefreshTokenLogsTheAppOut(t *testing.T) {
	app := newOAuthApp(t)
	access, refresh := app.oauthLogin(t)
	status, _ := app.tokenForm(t, "action=revoke&token="+refresh)
	equal(t, status, 200)
	equal(t, app.stateStatus(t, access), 401)
	status, _ = app.refreshGrant(t, refresh)
	equal(t, status, 400)
}

func TestLoginFlowRefusesUnknownOrMismatchedRedirects(t *testing.T) {
	app := newOAuthApp(t)
	for _, c := range []struct{ client, redirect string }{
		{iosClient, "https://evil.example/cb"},
		{iosClient, "homeassistant-dev://auth-callback"},
		{"https://evil.example", appRedirect},
		{"https://home-assistant.io/iOS/dev-auth", appRedirect},
		{iosClient + "/", appRedirect},
		{"", ""},
	} {
		rec := app.loginFlow(t, "admin", oauthPassword, c.client, c.redirect)
		equal(t, rec.Code, 400)
		equal(t, rec.Body.String(), `{"error":"invalid_request"}`)
	}
	// the dev-auth pair is a valid one
	rec := app.loginFlow(t, "admin", oauthPassword, "https://home-assistant.io/iOS/dev-auth", "homeassistant-dev://auth-callback")
	equal(t, rec.Code, 200)
}

func TestExpiredCodesAreRefused(t *testing.T) {
	app := newOAuthApp(t)
	_, body := app.loginCode(t, oauthPassword)
	code := str(t, body, "code")
	codes := app.app.codes
	codes.mu.Lock()
	for key, pending := range codes.codes {
		pending.expires = time.Now().Unix() - 1
		codes.codes[key] = pending
	}
	codes.mu.Unlock()
	status, body := app.codeGrant(t, code, iosClient)
	equal(t, status, 400)
	equal(t, body["error"], "invalid_grant")
}

func TestAnAppTokenStartsASessionThatEndsWithTheAppLoginOverOAuth(t *testing.T) {
	app := newOAuthApp(t)
	access, _ := app.oauthLogin(t)
	cookie := app.appSession(t, access)
	equal(t, app.loggedIn(t, cookie), true)
	_, sessions := app.adminJSON(t, http.MethodGet, "/api/domus/sessions", cookie, "")
	agent := asMap(t, asList(t, sessions)[0])["user_agent"].(string)
	if !strings.HasPrefix(agent, "HA app") {
		t.Fatalf("user agent %q", agent)
	}
	// logging the app out (what iOS does) also ends the page session
	app.tokenForm(t, "action=revoke&token="+access)
	equal(t, app.loggedIn(t, cookie), false)
}

func TestRemovingTheAppInTheAdminListEndsItsPageSession(t *testing.T) {
	app := newOAuthApp(t)
	access, _ := app.oauthLogin(t)
	cookie := app.appSession(t, access)
	noError(t, app.store.RevokeToken(bg, app.firstTokenID(t), nil))
	equal(t, app.loggedIn(t, cookie), false)
}

func TestOnlyAppLoginTokensMayStartASessionOverOAuth(t *testing.T) {
	app := newOAuthApp(t)
	status, _ := app.fromBearer(t, "")
	equal(t, status, 401)
	status, _ = app.fromBearer(t, "nope")
	equal(t, status, 401)
	// an ordinary API token (here an unrestricted one) is not accepted either
	app.addToken(t, "domus_plain", store.NewToken{UserID: 1, Name: "plain"})
	status, cookie := app.fromBearer(t, "domus_plain")
	equal(t, status, 401)
	equal(t, cookie, "")
}

func TestAppSessionsNeedThePasswordForSensitiveActionsOverOAuth(t *testing.T) {
	app := newOAuthApp(t)
	access, _ := app.oauthLogin(t)
	cookie := app.appSession(t, access)
	newToken := `{"name":"ci"}`
	status, body := app.adminJSON(t, http.MethodPost, "/api/domus/tokens", cookie, newToken)
	equal(t, status, 403)
	equal(t, asMap(t, body)["reauth"], true)
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/session/reauth", cookie, `{"password":"nope"}`), 401)
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/session/reauth", cookie, fmt.Sprintf(`{"password":%q}`, oauthPassword)), 200)
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/tokens", cookie, newToken), 200)
}

func TestEverySensitiveAdminActionNeedsThePassword(t *testing.T) {
	app := newOAuthApp(t)
	access, _ := app.oauthLogin(t)
	cookie := app.appSession(t, access)
	for _, attempt := range []struct{ method, path, body string }{
		{"POST", "/api/domus/tokens", `{"name":"ci"}`},
		{"POST", "/api/domus/users", `{"username":"bob","password":"long-enough-pass"}`},
		{"PUT", "/api/domus/users/1", `{"is_admin":true}`},
		{"DELETE", "/api/domus/users/1", ``},
		{"POST", "/api/domus/hue/pair", `{}`},
		{"PUT", "/api/domus/groups/den", `{"members":[]}`},
		{"DELETE", "/api/domus/groups/den", ``},
	} {
		status, body := app.adminJSON(t, attempt.method, attempt.path, cookie, attempt.body)
		equal(t, status, 403)
		equal(t, asMap(t, body)["reauth"], true)
	}
}

func TestPasswordChangeEndsTheAppSession(t *testing.T) {
	app := newOAuthApp(t)
	access, _ := app.oauthLogin(t)
	cookie := app.appSession(t, access)
	// the app session cannot even reach it without the old password; use a
	// password session
	own := app.login(t, "admin", oauthPassword)
	change := fmt.Sprintf(`{"current":%q,"new":"another-long-passphrase"}`, oauthPassword)
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/password", own, change), 200)
	equal(t, app.loggedIn(t, cookie), false)
	equal(t, app.stateStatus(t, access), 401)
}

func TestDeletingTheUserEndsTheAppSession(t *testing.T) {
	app := newOAuthApp(t)
	access, _ := app.oauthLogin(t)
	cookie := app.appSession(t, access)
	app.secondAdmin(t)
	result, err := app.store.DeleteUserKeepingAnAdmin(bg, 1)
	noError(t, err)
	equal(t, result, store.UserDeleted)
	equal(t, app.loggedIn(t, cookie), false)
}

func TestASecondExchangeReplacesTheFirstSession(t *testing.T) {
	app := newOAuthApp(t)
	access, _ := app.oauthLogin(t)
	first := app.appSession(t, access)
	second := app.appSession(t, access)
	if first == second {
		t.Fatal("both exchanges got the same session")
	}
	equal(t, app.loggedIn(t, first), false)
	equal(t, app.loggedIn(t, second), true)
}

func TestPasswordSessionsAreNotAskedAgain(t *testing.T) {
	app := newOAuthApp(t)
	cookie := app.login(t, "admin", oauthPassword)
	equal(t, app.adminStatus(t, http.MethodPost, "/api/domus/tokens", cookie, `{"name":"ci"}`), 200)
}

func TestUnknownGrantTypesAreRejected(t *testing.T) {
	app := newOAuthApp(t)
	status, body := app.tokenForm(t, "grant_type=password&client_id=x")
	equal(t, status, 400)
	equal(t, body["error"], "unsupported_grant_type")
	status, body = app.tokenForm(t, "client_id=x")
	equal(t, status, 400)
	equal(t, body["error"], "invalid_request")
	status, body = app.tokenForm(t, "grant_type=authorization_code&client_id=x")
	equal(t, status, 400)
	equal(t, body["error"], "invalid_request")
}

func fixedClock(now time.Time) func() time.Time { return func() time.Time { return now } }

func TestACodeLastsOneMinute(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	codes := newAuthCodes(fixedClock(now))
	code := codes.issue(1, iosClient)
	equal(t, codes.codes[auth.SHA256Hex(code)].expires, now.Unix()+60)
	equal(t, codeTTLSecs, 60)

	// still good at the last second, gone at the next
	late := codes.issue(1, iosClient)
	codes.now = fixedClock(now.Add(59 * time.Second))
	_, ok := codes.redeem(code, iosClient)
	equal(t, ok, true)
	codes.now = fixedClock(now.Add(60 * time.Second))
	_, ok = codes.redeem(late, iosClient)
	equal(t, ok, false)
}

func TestACodeWorksOnceAndOnlyForItsClient(t *testing.T) {
	codes := newAuthCodes(time.Now)
	code := codes.issue(7, iosClient)
	_, ok := codes.redeem(code, androidClient)
	equal(t, ok, false)
	_, ok = codes.redeem(code, iosClient)
	equal(t, ok, false) // a wrong guess burns the code

	code = codes.issue(7, iosClient)
	userID, ok := codes.redeem(code, iosClient)
	equal(t, ok, true)
	equal(t, userID, int64(7))
	_, ok = codes.redeem(code, iosClient)
	equal(t, ok, false)
}

func TestAFullCodeTableDropsExpiredCodesAndKeepsLiveOnes(t *testing.T) {
	codes := newAuthCodes(time.Now)
	now := time.Now().Unix()
	for i := range maxCodes - 1 {
		codes.codes[fmt.Sprintf("expired%d", i)] = pendingCode{userID: 1, clientID: iosClient, expires: now - 10}
	}
	codes.codes["live"] = pendingCode{userID: 1, clientID: iosClient, expires: now + 30}
	codes.issue(1, iosClient)
	equal(t, len(codes.codes), 2)
	if _, ok := codes.codes["live"]; !ok {
		t.Fatal("the live code was dropped")
	}
}

func TestAnAccessTokenLastsThirtyMinutes(t *testing.T) {
	app := newOAuthApp(t)
	before := time.Now().Unix()
	app.oauthLogin(t)
	after := time.Now().Unix()
	tokens, err := app.store.ListTokens(bg, nil)
	noError(t, err)
	equal(t, len(tokens), 1)
	equal(t, accessTTLSecs, 30*60)
	expires := *tokens[0].Expires
	if expires < before+1800 || expires > after+1800 {
		t.Fatalf("expires %d, want within [%d, %d]", expires, before+1800, after+1800)
	}
	equal(t, tokens[0].Name, "Home Assistant app (iOS)")
}

func TestOnlyTheAndroidClientGetsTheAndroidName(t *testing.T) {
	app := newOAuthApp(t)
	rec := app.loginFlow(t, "admin", oauthPassword, androidClient, appRedirect)
	status, _ := app.codeGrant(t, str(t, decodeObject(rec), "code"), androidClient)
	equal(t, status, 200)
	tokens, err := app.store.ListTokens(bg, nil)
	noError(t, err)
	equal(t, tokens[0].Name, "Home Assistant app (Android)")
}

func TestAuthCodesAreSafeForConcurrentUse(t *testing.T) {
	codes := newAuthCodes(time.Now)
	var wg sync.WaitGroup
	redeemed := make(chan int64, 16)
	code := codes.issue(3, iosClient)
	for range 16 {
		wg.Go(func() {
			if userID, ok := codes.redeem(code, iosClient); ok {
				redeemed <- userID
			}
		})
	}
	wg.Wait()
	close(redeemed)
	equal(t, len(redeemed), 1)
}

func TestOAuthEndpointsRejectWrongMethodsAndBodies(t *testing.T) {
	app := newOAuthApp(t)
	rec := app.do(t, request{method: http.MethodGet, path: "/auth/token"})
	equal(t, rec.Code, 405)
	equal(t, rec.Header().Get("Allow"), "POST")
	rec = app.do(t, request{method: http.MethodPost, path: "/auth/token", body: "grant_type=refresh_token"})
	equal(t, rec.Code, 415)
	rec = app.do(t, request{
		method: http.MethodPost, path: "/auth/login_flow", body: `{"username":"admin"}`,
		headers: map[string]string{"Content-Type": "application/json"},
	})
	equal(t, rec.Code, 422)
	// revoke answers 200 whether or not the token exists
	status, _ := app.tokenForm(t, "action=revoke&token=unknown")
	equal(t, status, 200)
	status, _ = app.tokenForm(t, "action=revoke")
	equal(t, status, 200)
}
