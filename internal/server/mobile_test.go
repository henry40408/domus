package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/store"
	"github.com/henry40408/domus/internal/version"
)

const (
	mobileToken       = "app-token"
	registrationsPath = "/api/mobile_app/registrations"
)

// newMobileApp is a test app with the admin (id 1), an unrestricted token and
// a read-only one.
func newMobileApp(t *testing.T) *testApp {
	return newMobileAppWith(t, func(*Config) {})
}

func newMobileAppWith(t *testing.T, tweak func(cfg *Config)) *testApp {
	t.Helper()
	app := newTestAppWith(t, tweak)
	_, err := app.store.CreateFirstAdmin(bg, "admin", "x")
	noError(t, err)
	app.addToken(t, mobileToken, store.NewToken{UserID: 1, Name: "t"})
	app.addToken(t, "ro-token", store.NewToken{UserID: 1, Name: "ro", ReadOnly: true})
	return app
}

// postJSON posts body (a Go value, or a string sent as is) with the bearer
// token, when given, and decodes the answer.
func (a *testApp) postJSON(t *testing.T, path, token string, body any) (int, any) {
	t.Helper()
	text, isText := body.(string)
	if !isText {
		encoded, err := json.Marshal(body)
		noError(t, err)
		text = string(encoded)
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	rec := a.do(t, request{method: http.MethodPost, path: path, headers: headers, body: text})
	var decoded any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec.Code, decoded
}

func registration() map[string]any {
	return map[string]any{
		"app_id":              "io.homeassistant.companion.android",
		"app_name":            "Home Assistant",
		"app_version":         "2025.10.0",
		"device_name":         "Pixel",
		"manufacturer":        "Google",
		"model":               "Pixel 8",
		"os_name":             "Android",
		"os_version":          "15",
		"supports_encryption": true,
		"app_data":            map[string]any{"push_token": "abc"},
	}
}

func (a *testApp) registered(t *testing.T) string {
	t.Helper()
	status, body := a.postJSON(t, registrationsPath, mobileToken, registration())
	equal(t, status, 201)
	return asMap(t, body)["webhook_id"].(string)
}

func (a *testApp) registrations(t *testing.T) []store.Registration {
	t.Helper()
	list, err := a.store.ListRegistrations(bg, nil)
	noError(t, err)
	return list
}

func (a *testApp) hook(t *testing.T, id string, body any) (int, any) {
	t.Helper()
	return a.postJSON(t, "/api/webhook/"+id, "", body)
}

func TestAuditLogEscapesDeviceFieldsSoTheyCannotForgeFields(t *testing.T) {
	var log logBuffer
	app := newMobileAppWith(t, func(cfg *Config) { cfg.Logger = slog.New(slog.NewTextHandler(&log, nil)) })
	body := registration()
	body["device_name"] = "x\" by=\"root\nforged line"
	body["app_id"] = "a\" user_id=\"0"
	status, _ := app.postJSON(t, registrationsPath, mobileToken, body)
	equal(t, status, 201)
	text := log.String()
	if !strings.Contains(text, `device="x\" by=\"root\nforged line"`) {
		t.Fatalf("device not escaped: %s", text)
	}
	if !strings.Contains(text, `app="a\" user_id=\"0"`) {
		t.Fatalf("app not escaped: %s", text)
	}
	equal(t, len(strings.Split(strings.TrimSpace(text), "\n")), 1)
}

func TestRegistrationNeedsAWritableTokenAndRequiredFields(t *testing.T) {
	app := newMobileApp(t)
	status, _ := app.postJSON(t, registrationsPath, "", registration())
	equal(t, status, 401)
	status, _ = app.postJSON(t, registrationsPath, "ro-token", registration())
	equal(t, status, 403)
	missing := registration()
	delete(missing, "model")
	status, body := app.postJSON(t, registrationsPath, mobileToken, missing)
	equal(t, status, 400)
	equal(t, asMap(t, body)["message"], "Missing required field: model.")
	equal(t, len(app.registrations(t)), 0)

	status, body = app.postJSON(t, registrationsPath, mobileToken, registration())
	equal(t, status, 201)
	object := asMap(t, body)
	if len(object["webhook_id"].(string)) < 32 {
		t.Fatalf("short webhook id %q", object["webhook_id"])
	}
	for _, key := range []string{"secret", "cloudhook_url", "remote_ui_url"} {
		value, present := object[key]
		equal(t, present, true) // key present but null: webhooks stay plain
		equal(t, value, nil)
	}
	list := app.registrations(t)
	equal(t, len(list), 1)
	equal(t, list[0].DeviceName, "Pixel")
	equal(t, *list[0].Username, "admin")
}

func TestWebhookServesWhatTheAppsAskFor(t *testing.T) {
	app := newMobileApp(t)
	id := app.registered(t)

	status, config := app.hook(t, id, map[string]any{"type": "get_config"})
	equal(t, status, 200)
	cfg := asMap(t, config)
	equal(t, cfg["version"], version.HAVersion)
	asMap(t, cfg["unit_system"])
	equal(t, cfg["components"], []any{"api", "mobile_app", "light", "scene"})

	status, body := app.hook(t, id, map[string]any{
		"type": "register_sensor", "data": map[string]any{"unique_id": "battery", "state": 80},
	})
	equal(t, status, 201)
	equal(t, asMap(t, body)["success"], true)

	status, body = app.hook(t, id, map[string]any{
		"type": "update_sensor_states", "data": []any{map[string]any{"unique_id": "a"}, map[string]any{"unique_id": "b"}},
	})
	equal(t, status, 200)
	equal(t, body, map[string]any{"a": map[string]any{"success": true}, "b": map[string]any{"success": true}})

	status, _ = app.hook(t, id, map[string]any{"type": "update_location", "data": map[string]any{}})
	equal(t, status, 204)
	_, body = app.hook(t, id, map[string]any{"type": "get_zones"})
	equal(t, body, []any{})
	// unknown types are accepted so a newer app does not break
	status, body = app.hook(t, id, map[string]any{"type": "something_new"})
	equal(t, status, 200)
	equal(t, body, map[string]any{})
	status, _ = app.hook(t, id, map[string]any{"data": 1})
	equal(t, status, 400)
}

func TestConfigPrintsTheLocationAsFloats(t *testing.T) {
	app := newMobileApp(t)
	rec := app.do(t, request{
		method: http.MethodPost, path: "/api/webhook/" + app.registered(t), body: `{"type":"get_config"}`,
	})
	equal(t, rec.Code, 200)
	text := rec.Body.String()
	for _, want := range []string{`"latitude":0.0,`, `"longitude":0.0,`, `"elevation":0,`, `"version":"2025.12.0"`, `"temperature":"°C"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %s in %s", want, text)
		}
	}
	equal(t, rec.Header().Get("Content-Type"), "application/json")
}

func TestUpdateRegistrationChangesTheDeviceAndRejectsEmptyData(t *testing.T) {
	app := newMobileApp(t)
	id := app.registered(t)
	status, _ := app.hook(t, id, map[string]any{"type": "update_registration", "data": map[string]any{}})
	equal(t, status, 400)
	status, body := app.hook(t, id, map[string]any{
		"type": "update_registration",
		"data": map[string]any{"device_name": "Pixel Pro", "app_version": "2025.11.0"},
	})
	equal(t, status, 200)
	equal(t, asMap(t, body)["device_name"], "Pixel Pro")
	equal(t, asMap(t, body)["supports_encryption"], false)
	reg := app.registrations(t)[0]
	equal(t, reg.DeviceName, "Pixel Pro")
	equal(t, reg.AppVersion, "2025.11.0")
	equal(t, reg.Model, "Pixel 8")
}

func TestUnknownAndRemovedWebhooksAreGone(t *testing.T) {
	app := newMobileApp(t)
	rec := app.do(t, request{method: http.MethodPost, path: "/api/webhook/nope", body: `{"type":"get_config"}`})
	equal(t, rec.Code, 410)
	equal(t, rec.Body.String(), "")
	id := app.registered(t)
	noError(t, app.store.DeleteRegistration(bg, app.registrations(t)[0].ID, nil))
	status, _ := app.hook(t, id, map[string]any{"type": "get_config"})
	equal(t, status, 410)
}

func TestAnUnknownWebhookIsGoneBeforeTheBodyIsRead(t *testing.T) {
	app := newMobileApp(t)
	rec := app.do(t, request{method: http.MethodPost, path: "/api/webhook/nope", body: "not json"})
	equal(t, rec.Code, 410)
}

func TestOnlyTheOwnerMayRemoveARegistration(t *testing.T) {
	app := newMobileApp(t)
	app.registered(t)
	reg := app.registrations(t)[0].ID
	other := app.addMember(t, "bob", "x")
	theirs, err := app.store.ListRegistrations(bg, &other)
	noError(t, err)
	equal(t, len(theirs), 0)
	if err := app.store.DeleteRegistration(bg, reg, &other); err == nil {
		t.Fatal("bob removed another user's registration")
	}
	owner := int64(1)
	noError(t, app.store.DeleteRegistration(bg, reg, &owner))
}

// mobileAppLogin creates an app login (a refresh token and one access token
// minted from it).
func (a *testApp) mobileAppLogin(t *testing.T, access string) {
	t.Helper()
	refresh, err := a.store.RefreshCreate(bg, 1, iosClient, auth.SHA256Hex("refresh"))
	noError(t, err)
	noError(t, a.store.CreateAppToken(bg, store.NewAppToken{
		UserID: 1, Name: "app", TokenHash: auth.SHA256Hex(access), Expires: 1 << 62, RefreshID: refresh,
	}))
}

func TestMalformedRequestsAreRejected(t *testing.T) {
	app := newMobileApp(t)
	status, _ := app.postJSON(t, registrationsPath, mobileToken, []any{1})
	equal(t, status, 400)
	nested := registration()
	nested["model"] = map[string]any{"a": 1}
	status, _ = app.postJSON(t, registrationsPath, mobileToken, nested)
	equal(t, status, 400)
	long := registration()
	long["device_name"] = strings.Repeat("x", fieldMax+1)
	status, _ = app.postJSON(t, registrationsPath, mobileToken, long)
	equal(t, status, 400)
	equal(t, len(app.registrations(t)), 0)

	id := app.registered(t)
	status, _ = app.hook(t, id, []any{1})
	equal(t, status, 400)
	rec := app.do(t, request{method: http.MethodPost, path: "/api/webhook/" + id, body: "not json"})
	equal(t, rec.Code, 400)
	status, body := app.hook(t, id, map[string]any{"type": "update_registration", "data": map[string]any{"os_version": "16"}})
	equal(t, status, 200)
	equal(t, asMap(t, body)["os_version"], "16")
}

func TestScopedTokensMayNotRegister(t *testing.T) {
	app := newMobileApp(t)
	app.addToken(t, "scoped", store.NewToken{UserID: 1, Name: "scoped", Scope: store.Scope{"Garmin"}})
	status, body := app.postJSON(t, registrationsPath, "scoped", registration())
	equal(t, status, 403)
	equal(t, asMap(t, body)["message"], "This token may not register devices.")
}

func TestRequiredFieldsAcceptEmptyAndNumbers(t *testing.T) {
	app := newMobileApp(t)
	body := registration()
	body["os_version"] = 18
	body["manufacturer"] = ""
	status, _ := app.postJSON(t, registrationsPath, mobileToken, body)
	equal(t, status, 201)
	reg := app.registrations(t)[0]
	equal(t, *reg.OSVersion, "18")
	equal(t, reg.Manufacturer, "")
}

func TestRegisteringTheSameDeviceAgainReplacesIt(t *testing.T) {
	app := newMobileApp(t)
	body := registration()
	body["device_id"] = "ABC"
	_, first := app.postJSON(t, registrationsPath, mobileToken, body)
	status, second := app.postJSON(t, registrationsPath, mobileToken, body)
	equal(t, status, 201)
	if asMap(t, first)["webhook_id"] == asMap(t, second)["webhook_id"] {
		t.Fatal("the replacement kept the old webhook id")
	}
	equal(t, len(app.registrations(t)), 1)
	status, _ = app.hook(t, asMap(t, first)["webhook_id"].(string), map[string]any{"type": "get_config"})
	equal(t, status, 410)
}

func TestRegistrationsPerUserAreCapped(t *testing.T) {
	app := newMobileApp(t)
	for i := range 20 {
		body := registration()
		body["device_id"] = "dev-" + strconv.Itoa(i)
		status, _ := app.postJSON(t, registrationsPath, mobileToken, body)
		equal(t, status, 201)
	}
	body := registration()
	body["device_id"] = "one-too-many"
	status, _ := app.postJSON(t, registrationsPath, mobileToken, body)
	equal(t, status, 429)
	// re-registering a known device is not "one more"
	body["device_id"] = "dev-3"
	status, _ = app.postJSON(t, registrationsPath, mobileToken, body)
	equal(t, status, 201)
}

func TestRemovingARegistrationLogsTheAppOut(t *testing.T) {
	app := newMobileApp(t)
	app.mobileAppLogin(t, "app-access")
	status, _ := app.postJSON(t, registrationsPath, "app-access", registration())
	equal(t, status, 201)
	owner := int64(1)
	noError(t, app.store.DeleteRegistration(bg, app.registrations(t)[0].ID, &owner))
	equal(t, len(app.registrations(t)), 0)
	_, _, err := app.store.RefreshLookup(bg, auth.SHA256Hex("refresh"), iosClient)
	if err == nil {
		t.Fatal("the refresh token outlived its registration")
	}
	status, _ = app.postJSON(t, registrationsPath, "app-access", registration())
	equal(t, status, 401)
}

func TestLoggingTheAppOutRemovesItsRegistration(t *testing.T) {
	app := newMobileApp(t)
	app.mobileAppLogin(t, "app-access")
	_, body := app.postJSON(t, registrationsPath, "app-access", registration())
	id := asMap(t, body)["webhook_id"].(string)
	noError(t, app.store.DeleteUserRefreshTokens(bg, 1))
	equal(t, len(app.registrations(t)), 0)
	status, _ := app.hook(t, id, map[string]any{"type": "get_config"})
	equal(t, status, 410)
}

func TestDeletingAUserRemovesTheirRegistrations(t *testing.T) {
	app := newMobileApp(t)
	id := app.registered(t)
	app.secondAdmin(t)
	result, err := app.store.DeleteUserKeepingAnAdmin(bg, 1)
	noError(t, err)
	equal(t, result, store.UserDeleted)
	status, _ := app.hook(t, id, map[string]any{"type": "get_config"})
	equal(t, status, 410)
}

func TestScalarAcceptsExactlyTheFieldLimit(t *testing.T) {
	field := func(length int) map[string]any {
		return map[string]any{"k": strings.Repeat("é", length)}
	}
	text, err := scalar(field(fieldMax), "k")
	noError(t, err)
	if text == nil {
		t.Fatal("a field of exactly the limit is refused")
	}
	if _, err := scalar(field(fieldMax+1), "k"); err == nil {
		t.Fatal("a field over the limit is accepted")
	}
}

func TestScalarReadsNumbersBoolsAndNull(t *testing.T) {
	body, err := parseJSONValue([]byte(`{"n":18,"f":1.50,"b":true,"z":null,"o":{}}`))
	noError(t, err)
	object := body.(map[string]any)
	for key, want := range map[string]string{"n": "18", "f": "1.50", "b": "true"} {
		got, err := scalar(object, key)
		noError(t, err)
		equal(t, *got, want)
	}
	got, err := scalar(object, "z")
	noError(t, err)
	equal(t, got == nil, true)
	got, err = scalar(object, "missing")
	noError(t, err)
	equal(t, got == nil, true)
	if _, err := scalar(object, "o"); err == nil {
		t.Fatal("an object is not a scalar")
	}
}
