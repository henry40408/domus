package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/core"
	"github.com/henry40408/domus/internal/store"
	"github.com/henry40408/domus/internal/version"
)

const (
	wsToken = "ws-token"
	// wsAuthTimeout and wsRecheckTime are the shortened intervals of the
	// tests, the same as the Rust ones.
	wsAuthTimeout = 300 * time.Millisecond
	wsRecheckTime = 100 * time.Millisecond
	wsWait        = 5 * time.Second
)

// wsApp is a test app (admin id 1, one unrestricted token, the fake lights)
// behind a real HTTP server, since a WebSocket needs a real connection.
type wsApp struct {
	*testApp
	fake   *fakeLights
	server *httptest.Server
}

func newWSApp(t *testing.T, shutdown <-chan struct{}) *wsApp {
	t.Helper()
	// Registered first, so it runs after every connection and the server are
	// gone: no goroutine of a connection may outlive its test.
	t.Cleanup(func() { waitForNoWebsocketGoroutines(t) })
	app := newTestAppWith(t, func(cfg *Config) {
		cfg.Timing = Timing{WebsocketAuth: wsAuthTimeout, WebsocketRecheck: wsRecheckTime}
		cfg.Shutdown = shutdown
	})
	_, err := app.store.CreateFirstAdmin(bg, "admin", "x")
	noError(t, err)
	app.addToken(t, wsToken, store.NewToken{UserID: 1, Name: "t"})
	fake := &fakeLights{}
	app.app.core.SetIntegration("fake", fake)
	server := httptest.NewServer(app.handler)
	t.Cleanup(server.Close)
	return &wsApp{testApp: app, fake: fake, server: server}
}

func waitForNoWebsocketGoroutines(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(wsWait)
	for {
		buffer := make([]byte, 1<<20)
		dump := string(buffer[:runtime.Stack(buffer, true)])
		if !strings.Contains(dump, "server.readMessages") && !strings.Contains(dump, "(*App).websocket") {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("a WebSocket goroutine is still running:\n%s", dump)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
}

func (a *wsApp) connect(t *testing.T) *wsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg, wsWait)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(a.server.URL, "http")+"/api/websocket", nil)
	noError(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &wsClient{t: t, conn: conn}
}

func (a *wsApp) authed(t *testing.T, token string) *wsClient {
	t.Helper()
	client := a.connect(t)
	equal(t, client.recv()["type"], "auth_required")
	client.tell(map[string]any{"type": "auth", "access_token": token})
	equal(t, client.recv()["type"], "auth_ok")
	return client
}

func (c *wsClient) recv() map[string]any {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(bg, wsWait)
	defer cancel()
	kind, data, err := c.conn.Read(ctx)
	noError(c.t, err)
	equal(c.t, kind, websocket.MessageText)
	var message map[string]any
	noError(c.t, json.Unmarshal(data, &message))
	return message
}

func (c *wsClient) tell(message any) {
	c.t.Helper()
	data, err := json.Marshal(message)
	noError(c.t, err)
	c.tellText(string(data))
}

func (c *wsClient) tellText(text string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(bg, wsWait)
	defer cancel()
	noError(c.t, c.conn.Write(ctx, websocket.MessageText, []byte(text)))
}

// closed reports whether the server ends the connection soon.
func (c *wsClient) closed() bool {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(bg, wsWait)
	defer cancel()
	for {
		if _, _, err := c.conn.Read(ctx); err != nil {
			return ctx.Err() == nil
		}
	}
}

// ask sends a command and returns its answer.
func (c *wsClient) ask(message map[string]any) map[string]any {
	c.t.Helper()
	c.tell(message)
	return c.recv()
}

func (a *wsApp) addUserToken(t *testing.T, name string) int64 {
	t.Helper()
	user, err := a.store.CreateUser(bg, name, "x", false)
	noError(t, err)
	a.addToken(t, name, store.NewToken{UserID: user, Name: name})
	return user
}

func TestRecheckReportsAChangeOfScopeOnce(t *testing.T) {
	app := newWSApp(t, nil)
	bob := app.addUserToken(t, "bob")
	session := app.app.newWebsocketSession(bg, nil, store.TokenAuth{UserID: &bob}, auth.SHA256Hex("bob"))

	alive, changed := session.recheck(bg)
	equal(t, [2]bool{alive, changed}, [2]bool{true, false})
	noError(t, app.store.SetUserScope(bg, bob, store.Scope{"One"}))
	alive, changed = session.recheck(bg)
	equal(t, [2]bool{alive, changed}, [2]bool{true, true})
	equal(t, session.scope, store.Scope{"One"})
	alive, changed = session.recheck(bg)
	equal(t, [2]bool{alive, changed}, [2]bool{true, false})
}

func TestRecheckTellsUnrestrictedFromEmpty(t *testing.T) {
	equal(t, sameScope(nil, store.Scope{}), false)
	equal(t, sameScope(store.Scope{}, store.Scope{}), true)
	equal(t, sameScope(store.Scope{"A"}, store.Scope{"A"}), true)
	equal(t, sameScope(store.Scope{"A"}, store.Scope{"B"}), false)
}

func TestRecheckEndsWhenTheLoginIsGone(t *testing.T) {
	app := newWSApp(t, nil)
	bob := app.addUserToken(t, "bob")
	token := store.TokenAuth{UserID: &bob}

	// a long-lived token: revoking it ends the session
	session := app.app.newWebsocketSession(bg, nil, token, auth.SHA256Hex("bob"))
	tokens, err := app.store.ListTokens(bg, &bob)
	noError(t, err)
	noError(t, app.store.RevokeToken(bg, tokens[0].ID, nil))
	alive, _ := session.recheck(bg)
	equal(t, alive, false)

	// an app login: the refresh token is what counts
	refresh, err := app.store.RefreshCreate(bg, bob, "https://home-assistant.io/iOS", auth.SHA256Hex("r"))
	noError(t, err)
	token.RefreshID = &refresh
	session = app.app.newWebsocketSession(bg, nil, token, auth.SHA256Hex("gone-access-token"))
	alive, _ = session.recheck(bg)
	equal(t, alive, true)
	noError(t, app.store.DeleteUserRefreshTokens(bg, bob))
	alive, _ = session.recheck(bg)
	equal(t, alive, false)

	// a deleted user
	session = app.app.newWebsocketSession(bg, nil, store.TokenAuth{UserID: &bob}, auth.SHA256Hex("bob"))
	result, err := app.store.DeleteUserKeepingAnAdmin(bg, bob)
	noError(t, err)
	equal(t, result, store.UserDeleted)
	alive, _ = session.recheck(bg)
	equal(t, alive, false)
}

func TestServicesAndPanelsAreEmptyObjects(t *testing.T) {
	app := newWSApp(t, nil)
	client := app.authed(t, wsToken)
	// Ids must ascend on one connection, so the order is fixed.
	for _, c := range []struct {
		id   int
		kind string
	}{{1, "get_services"}, {2, "get_panels"}} {
		answer := client.ask(map[string]any{"id": c.id, "type": c.kind})
		equal(t, answer, map[string]any{"id": float64(c.id), "type": "result", "success": true, "result": map[string]any{}})
	}
}

func TestScenesCanBeCalledButOnlyTurnedOn(t *testing.T) {
	app := newWSApp(t, nil)
	client := app.authed(t, wsToken)
	for _, c := range []struct {
		id      int
		service string
		success bool
	}{{1, "turn_on", true}, {2, "nope", false}} {
		answer := client.ask(map[string]any{
			"id": c.id, "type": "call_service", "domain": "scene", "service": c.service,
			"service_data": map[string]any{},
		})
		equal(t, answer["success"], c.success)
	}
}

func TestAMessageJustUnderTheLimitIsServed(t *testing.T) {
	app := newWSApp(t, nil)
	client := app.authed(t, wsToken)
	padding := strings.Repeat("x", 60*1024)
	answer := client.ask(map[string]any{"id": 1, "type": "ping", "padding": padding})
	equal(t, answer, map[string]any{"id": float64(1), "type": "pong"})
	equal(t, wsMaxMessage, 64*1024)
}

func TestBadTokenAndSilenceAreRefused(t *testing.T) {
	app := newWSApp(t, nil)
	client := app.connect(t)
	equal(t, client.recv()["type"], "auth_required")
	client.tell(map[string]any{"type": "auth", "access_token": "nope"})
	equal(t, client.recv()["type"], "auth_invalid")
	equal(t, client.closed(), true)

	// not an auth message at all
	client = app.connect(t)
	client.recv()
	client.tell(map[string]any{"id": 1, "type": "ping"})
	equal(t, client.recv()["type"], "auth_invalid")

	// an auth message that is no JSON, or has a token of the wrong type
	for _, text := range []string{"not json", `{"type":"auth","access_token":5}`, `{"type":"auth"}`} {
		client = app.connect(t)
		client.recv()
		client.tellText(text)
		equal(t, client.recv()["type"], "auth_invalid")
	}

	// nothing sent: the auth timeout ends it
	client = app.connect(t)
	client.recv()
	equal(t, client.closed(), true)
}

func TestAnswersTheCommandsTheAppsSend(t *testing.T) {
	app := newWSApp(t, nil)
	app.app.core.SetState("light.lamp", "on", core.Attributes{"brightness": 200})
	client := app.authed(t, wsToken)

	equal(t, client.ask(map[string]any{"id": 1, "type": "ping"}), map[string]any{"id": float64(1), "type": "pong"})

	answer := client.ask(map[string]any{"id": 2, "type": "auth/current_user"})
	equal(t, asMap(t, answer["result"])["name"], "admin")
	equal(t, asMap(t, answer["result"])["is_admin"], true)

	answer = client.ask(map[string]any{"id": 3, "type": "get_config"})
	config := asMap(t, answer["result"])
	equal(t, config["version"], version.HAVersion)

	answer = client.ask(map[string]any{"id": 4, "type": "get_states"})
	first := asMap(t, asList(t, answer["result"])[0])
	equal(t, first["entity_id"], "light.lamp")
	equal(t, asMap(t, first["attributes"])["brightness"], float64(200))

	for i, kind := range wsEmptyLists {
		id := 10 + i
		equal(t, client.ask(map[string]any{"id": id, "type": kind}),
			map[string]any{"id": float64(id), "type": "result", "success": true, "result": []any{}})
	}

	answer = client.ask(map[string]any{"id": 20, "type": "supported_features", "features": map[string]any{"coalesce_messages": 1}})
	equal(t, answer["success"], true)

	answer = client.ask(map[string]any{"id": 21, "type": "no/such_command"})
	equal(t, [3]any{answer["id"], answer["success"], asMap(t, answer["error"])["code"]},
		[3]any{float64(21), false, "unknown_command"})

	answer = client.ask(map[string]any{"type": "ping"})
	equal(t, asMap(t, answer["error"])["code"], "invalid_format")
	client.tellText("not json")
	equal(t, asMap(t, client.recv()["error"])["code"], "invalid_format")
}

func TestGetConfigPrintsTheLocationAsFloats(t *testing.T) {
	app := newWSApp(t, nil)
	client := app.authed(t, wsToken)
	client.tell(map[string]any{"id": 1, "type": "get_config"})
	ctx, cancel := context.WithTimeout(bg, wsWait)
	defer cancel()
	_, data, err := client.conn.Read(ctx)
	noError(t, err)
	for _, want := range []string{`"latitude":0.0`, `"longitude":0.0`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("%s is missing from %s", want, data)
		}
	}
}

func TestIDsAreCheckedBeforeTheTypeIsRead(t *testing.T) {
	app := newWSApp(t, nil)
	client := app.authed(t, wsToken)
	for _, text := range []string{
		`{"id":1.5,"type":"ping"}`, `{"id":1.0,"type":"ping"}`, `{"id":-1,"type":"ping"}`,
		`{"id":"1","type":"ping"}`, `[1]`, `null`, `{"type":"no/such_command"}`,
	} {
		client.tellText(text)
		answer := client.recv()
		equal(t, [2]any{answer["id"], asMap(t, answer["error"])["code"]}, [2]any{nil, "invalid_format"})
	}
	equal(t, client.ask(map[string]any{"id": 4, "type": "no/such_command"})["success"], false)
	// a reused id is refused whatever the type is
	answer := client.ask(map[string]any{"id": 4, "type": "no/such_command"})
	equal(t, [2]any{answer["id"], asMap(t, answer["error"])["code"]}, [2]any{float64(4), "id_reuse"})
	answer = client.ask(map[string]any{"id": 3, "type": "subscribe_entities"})
	equal(t, asMap(t, answer["error"])["code"], "id_reuse")
}

func TestSubscribeEntitiesSendsASnapshotThenChanges(t *testing.T) {
	app := newWSApp(t, nil)
	c := app.app.core
	c.SetState("light.a", "on", nil)
	c.SetState("light.b", "off", nil)
	client := app.authed(t, wsToken)

	answer := client.ask(map[string]any{"id": 5, "type": "subscribe_entities"})
	equal(t, answer, map[string]any{"id": float64(5), "type": "result", "success": true, "result": nil})
	snapshot := client.recv()
	equal(t, snapshot["id"], float64(5))
	equal(t, snapshot["type"], "event")
	added := asMap(t, asMap(t, snapshot["event"])["a"])
	equal(t, len(added), 2)
	equal(t, asMap(t, added["light.a"])["s"], "on")
	if asMap(t, added["light.a"])["lu"].(float64) <= 1.7e9 {
		t.Error("lu is not a timestamp in seconds")
	}

	c.SetState("light.a", "off", nil)
	event := asMap(t, client.recv()["event"])
	equal(t, asMap(t, asMap(t, event["a"])["light.a"])["s"], "off")
	c.RemoveState("light.b")
	equal(t, client.recv()["event"], map[string]any{"r": []any{"light.b"}})

	answer = client.ask(map[string]any{"id": 6, "type": "unsubscribe_events", "subscription": 5})
	equal(t, answer["success"], true)
	answer = client.ask(map[string]any{"id": 7, "type": "unsubscribe_events", "subscription": 5})
	equal(t, asMap(t, answer["error"])["code"], "not_found")
	// nothing is pushed any more: the next reply is the pong
	c.SetState("light.a", "on", nil)
	equal(t, client.ask(map[string]any{"id": 8, "type": "ping"})["type"], "pong")
}

func TestTimesInAnEventAreFloats(t *testing.T) {
	app := newWSApp(t, nil)
	app.app.core.SetState("light.a", "on", nil)
	client := app.authed(t, wsToken)
	client.tell(map[string]any{"id": 1, "type": "subscribe_entities"})
	client.recv()
	ctx, cancel := context.WithTimeout(bg, wsWait)
	defer cancel()
	_, data, err := client.conn.Read(ctx)
	noError(t, err)
	if !strings.Contains(string(data), `"lc":`) || strings.Contains(string(data), `"lc":e`) {
		t.Fatalf("no lc in %s", data)
	}
	for _, key := range []string{`"lc"`, `"lu"`} {
		rest := string(data)[strings.Index(string(data), key)+len(key)+1:]
		number := rest[:strings.IndexAny(rest, ",}")]
		if !strings.Contains(number, ".") {
			t.Errorf("%s is %s, not a float", key, number)
		}
	}
}

func TestFloatJSONAlwaysHasAFraction(t *testing.T) {
	equal(t, string(floatJSON(0)), "0.0")
	equal(t, string(floatJSON(1790858096)), "1790858096.0")
	equal(t, string(floatJSON(1790858096.25)), "1790858096.25")
	equal(t, string(floatJSON(math.Pi)), "3.141592653589793")
}

func TestSubscriptionCanBeLimitedToEntities(t *testing.T) {
	app := newWSApp(t, nil)
	c := app.app.core
	c.SetState("light.a", "on", nil)
	c.SetState("light.b", "on", nil)
	client := app.authed(t, wsToken)
	client.tell(map[string]any{"id": 1, "type": "subscribe_entities", "entity_ids": []string{"light.b"}})
	client.recv()
	snapshot := client.recv()
	equal(t, len(asMap(t, asMap(t, snapshot["event"])["a"])), 1)
	c.SetState("light.a", "off", nil)
	c.SetState("light.b", "off", nil)
	added := asMap(t, asMap(t, client.recv()["event"])["a"])
	equal(t, asMap(t, added["light.b"])["s"], "off")
}

func TestAnEmptyEntityListLimitsToNothing(t *testing.T) {
	app := newWSApp(t, nil)
	app.app.core.SetState("light.a", "on", nil)
	client := app.authed(t, wsToken)
	client.tell(map[string]any{"id": 1, "type": "subscribe_entities", "entity_ids": []string{}})
	client.recv()
	equal(t, len(asMap(t, asMap(t, client.recv()["event"])["a"])), 0)
}

func TestScopedTokensOnlySeeTheirGroups(t *testing.T) {
	app := newWSApp(t, nil)
	c := app.app.core
	app.addToken(t, "scoped", store.NewToken{UserID: 1, Name: "scoped", Scope: store.Scope{"Garmin"}})
	c.SetState("light.a", "on", nil)
	c.SetState("light.other", "on", nil)
	app.setGroup(t, "Garmin", "light.a")
	app.exposeGroup(t, "Garmin")
	client := app.authed(t, "scoped")

	// the real member, the group and its group light; not what is outside the group
	answer := client.ask(map[string]any{"id": 1, "type": "get_states"})
	ids := entityIDsOfList(t, answer["result"])
	equal(t, ids, []string{"group.Garmin", "light.a", "light.domus_group_Garmin"})

	client.tell(map[string]any{"id": 2, "type": "subscribe_entities"})
	client.recv()
	snapshot := client.recv()
	equal(t, len(asMap(t, asMap(t, snapshot["event"])["a"])), 3)
	// outside the scope: nothing is sent, so the next event is about the group
	c.SetState("light.other", "off", nil)
	c.SetState("light.a", "off", nil)
	added := asMap(t, asMap(t, client.recv()["event"])["a"])
	equal(t, asMap(t, added["light.a"])["s"], "off")
	equal(t, asMap(t, added["group.Garmin"])["s"], "off")
	equal(t, asMap(t, added["light.domus_group_Garmin"])["s"], "off")
	if _, present := added["light.other"]; present {
		t.Error("an entity outside the scope was sent")
	}
}

func TestNarrowingAUsersGroupsTakesEffectOnOpenConnections(t *testing.T) {
	app := newWSApp(t, nil)
	c := app.app.core
	bob := app.addUserToken(t, "bob")
	c.SetState("light.a", "on", nil)
	c.SetState("light.b", "on", nil)
	app.setGroup(t, "One", "light.a")
	client := app.authed(t, "bob")
	client.tell(map[string]any{"id": 1, "type": "subscribe_entities"})
	client.recv()
	equal(t, len(asMap(t, asMap(t, client.recv()["event"])["a"])), 3)

	noError(t, app.store.SetUserScope(bg, bob, store.Scope{"One"}))
	event := asMap(t, client.recv()["event"])
	equal(t, event["r"], []any{"light.b"})
	answer := client.ask(map[string]any{"id": 2, "type": "get_states"})
	equal(t, len(asList(t, answer["result"])), 2)
}

func TestAppConnectionsOutliveTheirAccessToken(t *testing.T) {
	app := newWSApp(t, nil)
	refresh, err := app.store.RefreshCreate(bg, 1, "https://home-assistant.io/iOS", auth.SHA256Hex("r"))
	noError(t, err)
	mint := func(secret string) {
		noError(t, app.store.CreateAppToken(bg, store.NewAppToken{
			UserID: 1, Name: "app", TokenHash: auth.SHA256Hex(secret), Expires: math.MaxInt64, RefreshID: refresh,
		}))
	}
	mint("first")
	client := app.authed(t, "first")
	// the app refreshed: the old access token is replaced by a new one
	mint("second")
	time.Sleep(wsRecheckTime * 3)
	equal(t, client.ask(map[string]any{"id": 1, "type": "ping"})["type"], "pong")
	// logging the app out ends it
	noError(t, app.store.DeleteUserRefreshTokens(bg, 1))
	equal(t, client.closed(), true)
}

func TestIDsMustIncreaseAndSubscriptionsAreCapped(t *testing.T) {
	app := newWSApp(t, nil)
	client := app.authed(t, wsToken)
	equal(t, client.ask(map[string]any{"id": 5, "type": "ping"})["type"], "pong")
	answer := client.ask(map[string]any{"id": 5, "type": "ping"})
	equal(t, asMap(t, answer["error"])["code"], "id_reuse")
	// the cap is shared by both kinds of subscription
	for id := 6; id < 6+wsMaxSubscriptions-1; id++ {
		equal(t, client.ask(map[string]any{"id": id, "type": "subscribe_events"})["success"], true)
	}
	equal(t, client.ask(map[string]any{"id": 50, "type": "subscribe_entities"})["success"], true)
	client.recv() // its snapshot
	answer = client.ask(map[string]any{"id": 99, "type": "subscribe_events"})
	equal(t, asMap(t, answer["error"])["code"], "too_many")
	answer = client.ask(map[string]any{"id": 100, "type": "subscribe_entities"})
	equal(t, asMap(t, answer["error"])["code"], "too_many")
	answer = client.ask(map[string]any{"id": 101, "type": "unsubscribe_events", "subscription": 6})
	equal(t, answer["success"], true)
	// the freed slot is usable again
	equal(t, client.ask(map[string]any{"id": 102, "type": "subscribe_events"})["success"], true)
}

func TestEntityRegistryForDisplayListsVisibleEntities(t *testing.T) {
	app := newWSApp(t, nil)
	app.app.core.SetState("light.hue_a", "on", core.Attributes{"friendly_name": "Desk"})
	app.app.core.SetState("light.hue_b", "on", nil)
	client := app.authed(t, wsToken)
	answer := client.ask(map[string]any{"id": 1, "type": "config/entity_registry/list_for_display"})
	result := asMap(t, answer["result"])
	equal(t, result["entity_categories"], map[string]any{})
	equal(t, result["entities"], []any{
		map[string]any{"ei": "light.hue_a", "en": "Desk"},
		map[string]any{"ei": "light.hue_b"},
	})
}

func TestCallServiceRunsLightsAndRejectsTheRest(t *testing.T) {
	app := newWSApp(t, nil)
	app.app.core.SetState("light.hue_a", "on", nil)
	client := app.authed(t, wsToken)
	answer := client.ask(map[string]any{
		"id": 1, "type": "call_service", "domain": "light", "service": "toggle",
		"target": map[string]any{"entity_id": "light.hue_a"},
	})
	equal(t, [2]any{answer["id"], answer["success"]}, [2]any{float64(1), true})
	equal(t, asMap(t, answer["result"])["response"], nil)
	equal(t, app.fake.recorded(), []lightCall{{id: "light.hue_a", action: core.ActionOff()}})

	for _, c := range []struct {
		domain, service, code string
		data                  map[string]any
	}{
		{"switch", "toggle", "not_found", map[string]any{}},
		{"light", "nope", "not_found", map[string]any{}},
		{"light", "turn_on", "invalid_format", map[string]any{"entity_id": 5}},
	} {
		client = app.authed(t, wsToken)
		answer = client.ask(map[string]any{
			"id": 2, "type": "call_service", "domain": c.domain, "service": c.service, "service_data": c.data,
		})
		equal(t, asMap(t, answer["error"])["code"], c.code)
		// the connection is still good after a refusal
		equal(t, client.ask(map[string]any{"id": 3, "type": "ping"})["type"], "pong")
	}
}

func TestCallServiceReportsFailuresAndRefusesReadOnlyTokens(t *testing.T) {
	app := newWSApp(t, nil)
	app.app.core.SetState("light.hue_bad", "off", nil)
	app.addToken(t, "ro-token", store.NewToken{UserID: 1, Name: "ro", ReadOnly: true})
	call := map[string]any{
		"id": 1, "type": "call_service", "domain": "light", "service": "turn_on",
		"target": map[string]any{"entity_id": "light.hue_bad"},
	}
	client := app.authed(t, wsToken)
	answer := client.ask(call)
	equal(t, asMap(t, answer["error"])["code"], "home_assistant_error")
	client = app.authed(t, "ro-token")
	answer = client.ask(call)
	equal(t, asMap(t, answer["error"])["code"], "unauthorized")
}

func TestALaggingConnectionIsBroughtUpToDate(t *testing.T) {
	app := newWSApp(t, nil)
	c := app.app.core
	c.SetState("light.a", "start", nil)
	client := app.authed(t, wsToken)
	client.tell(map[string]any{"id": 1, "type": "subscribe_entities"})
	client.recv()
	client.recv()
	// More changes than the hub holds for a subscriber: whether or not this one
	// lags, the last state is what the client ends up with.
	for i := 0; i < 1000; i++ {
		c.SetState("light.a", "s"+strconv.Itoa(i), nil)
	}
	for {
		added := asMap(t, asMap(t, client.recv()["event"])["a"])
		if asMap(t, added["light.a"])["s"] == "s999" {
			return
		}
	}
}

func TestLagResyncsEverySubscription(t *testing.T) {
	app := newWSApp(t, nil)
	c := app.app.core
	c.SetState("light.a", "on", nil)
	c.SetState("light.b", "on", nil)
	var sent []map[string]any
	record := func(_ context.Context, value any) error {
		sent = append(sent, value.(map[string]any))
		return nil
	}
	session := app.app.newWebsocketSession(bg, record, store.TokenAuth{}, "")
	session.subs = []*entitySubscription{
		{id: 1, known: map[string]bool{"light.gone": true}},
		{id: 2, only: map[string]bool{"light.b": true}, known: map[string]bool{}},
	}
	noError(t, session.onChange(bg, change{lagged: true}))

	equal(t, len(sent), 2)
	first := sent[0]["event"].(map[string]any)
	equal(t, len(first["a"].(map[string]any)), 2)
	equal(t, first["r"], []string{"light.gone"})
	second := sent[1]["event"].(map[string]any)
	equal(t, len(second["a"].(map[string]any)), 1)
	if _, present := second["r"]; present {
		t.Error("a subscription that knew nothing got a removal")
	}
	equal(t, session.subs[0].known, map[string]bool{"light.a": true, "light.b": true})
}

func TestBinaryFramesAreIgnoredAndCloseEndsTheSession(t *testing.T) {
	app := newWSApp(t, nil)
	client := app.authed(t, wsToken)
	noError(t, client.conn.Write(bg, websocket.MessageBinary, []byte{1, 2, 3}))
	equal(t, client.ask(map[string]any{"id": 1, "type": "ping"})["type"], "pong")
	_ = client.conn.Close(websocket.StatusNormalClosure, "")
	equal(t, client.closed(), true)
}

func TestDeletingTheUserEndsTheConnection(t *testing.T) {
	app := newWSApp(t, nil)
	bob := app.addUserToken(t, "bob")
	client := app.authed(t, "bob")
	result, err := app.store.DeleteUserKeepingAnAdmin(bg, bob)
	noError(t, err)
	equal(t, result, store.UserDeleted)
	equal(t, client.closed(), true)
}

func TestOversizedMessagesEndTheConnection(t *testing.T) {
	app := newWSApp(t, nil)
	client := app.connect(t)
	client.recv()
	client.tellText(strings.Repeat("x", wsMaxMessage+1))
	equal(t, client.closed(), true)
}

func TestConnectionsEndOnRevocationAndOnShutdown(t *testing.T) {
	down := make(chan struct{})
	app := newWSApp(t, down)
	temp, err := app.store.CreateToken(bg, store.NewToken{UserID: 1, Name: "temp", TokenHash: auth.SHA256Hex("temp")})
	noError(t, err)
	revoked := app.authed(t, "temp")
	staying := app.authed(t, wsToken)
	noError(t, app.store.RevokeToken(bg, temp, nil))
	equal(t, revoked.closed(), true)

	equal(t, staying.ask(map[string]any{"id": 1, "type": "ping"})["type"], "pong")
	close(down)
	equal(t, staying.closed(), true)
}

func TestAConnectionThatIsNotUpgradedIsRefused(t *testing.T) {
	app := newWSApp(t, nil)
	rec := app.get(t, "/api/websocket")
	equal(t, rec.Code, 400)
	equal(t, rec.Body.String(), "Connection header did not include 'upgrade'")
	rec = app.do(t, request{method: "POST", path: "/api/websocket"})
	equal(t, rec.Code, 405)
}

func TestAHandshakeWithMissingHeadersIsRefusedLikeAxum(t *testing.T) {
	app := newWSApp(t, nil)
	good := map[string]string{
		"Connection": "keep-alive, Upgrade", "Upgrade": "websocket",
		"Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==",
	}
	cases := []struct{ drop, want string }{
		{"Connection", "Connection header did not include 'upgrade'"},
		{"Upgrade", "`Upgrade` header did not include 'websocket'"},
		{"Sec-WebSocket-Version", "`Sec-WebSocket-Version` header did not include '13'"},
		{"Sec-WebSocket-Key", "`Sec-WebSocket-Key` header missing"},
	}
	for _, tc := range cases {
		headers := map[string]string{}
		for name, value := range good {
			if name != tc.drop {
				headers[name] = value
			}
		}
		rec := app.do(t, request{path: "/api/websocket", headers: headers})
		equal(t, rec.Code, 400)
		equal(t, rec.Body.String(), tc.want)
	}
}
