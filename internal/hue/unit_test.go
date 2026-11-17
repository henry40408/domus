package hue

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/henry40408/domus/internal/core"
)

func TestBrightnessConversion(t *testing.T) {
	for pct, want := range map[float64]int{100: 255, 50: 128, 0: 1} {
		if got := pctToHA(pct); got != want {
			t.Errorf("pctToHA(%v) = %d, want %d", pct, got, want)
		}
	}
	for ha, want := range map[uint8]float64{255: 100, 128: 50.2, 0: 0} {
		if got := haToPct(ha); got != want {
			t.Errorf("haToPct(%d) = %v, want %v", ha, got, want)
		}
	}
}

func TestLightMapping(t *testing.T) {
	l, ok := lightFromResource(lightJSON("abcdef12-3456-7890", "Desk", true, 50))
	if !ok {
		t.Fatal("light not parsed")
	}
	if l.entityID() != "light.hue_abcdef12" || l.haState() != "on" {
		t.Fatalf("got %s %s", l.entityID(), l.haState())
	}
	a := l.attributes()
	sameJSON(t, a["friendly_name"], "Desk")
	sameJSON(t, a["brightness"], 128)
	sameJSON(t, a["supported_color_modes"], []string{"brightness"})
	if len(a) != 3 {
		t.Fatalf("attributes must stay small: %v", a)
	}

	l.On = false
	if _, has := l.attributes()["brightness"]; has {
		t.Fatal("an off light must not report brightness")
	}
	if _, ok := lightFromResource(J{"type": "light"}); ok {
		t.Fatal("a light without id must be rejected")
	}
}

func TestPartialUpdateMerges(t *testing.T) {
	l, _ := lightFromResource(lightJSON("a1", "Desk", true, 50))
	l.applyUpdate(J{"on": J{"on": false}})
	if l.On || l.Brightness == nil || *l.Brightness != 50 {
		t.Fatalf("on-only update must keep brightness: %+v", l)
	}
	l.applyUpdate(J{"dimming": J{"brightness": 10.0}, "metadata": J{"name": "Lamp"}})
	if *l.Brightness != 10 || l.Name != "Lamp" {
		t.Fatalf("got %+v", l)
	}
}

func TestSSEParserHandlesChunksCommentsAndCRLF(t *testing.T) {
	var p sseParser
	if got := p.push([]byte(": hi\n\n")); len(got) != 0 {
		t.Fatalf("comment produced %v", got)
	}
	if got := p.push([]byte("id: 1\r\ndata: {\"a\"")); len(got) != 0 {
		t.Fatalf("partial event produced %v", got)
	}
	sameJSON(t, p.push([]byte(":1}\r\n\r\ndata: x\n\nda")), []string{`{"a":1}`, "x"})
	sameJSON(t, p.push([]byte("ta: y\ndata: z\n\n")), []string{"y\nz"})
}

func TestColorStateAndRequestBodies(t *testing.T) {
	l, _ := lightFromResource(richLightJSON("aaaa1111-0", "dev-a"))
	a := l.attributes()
	sameJSON(t, a["color_mode"], "color_temp")
	sameJSON(t, a["color_temp"], 300)
	sameJSON(t, a["supported_color_modes"], []string{"brightness", "color_temp", "xy"})

	// a color change leaves white mode, a temperature change comes back
	l.applyUpdate(J{"color": J{"xy": J{"x": 0.1, "y": 0.2}}})
	a = l.attributes()
	sameJSON(t, a["color_mode"], "xy")
	sameJSON(t, a["xy_color"], []float64{0.1, 0.2})
	if _, has := a["color_temp"]; has {
		t.Fatal("color_temp must disappear in xy mode")
	}
	l.applyUpdate(J{"color_temperature": J{"mirek": 200.0, "mirek_valid": true}})
	sameJSON(t, l.attributes()["color_temp"], 200)
	// a stale mirek (light showing a color) is ignored
	l.applyUpdate(J{"color_temperature": J{"mirek": 200.0, "mirek_valid": false}})
	sameJSON(t, l.attributes()["color_mode"], "xy")

	on := core.LightAction{On: true, Brightness: ptr(uint8(255)), ColorTemp: ptr(uint16(250)),
		XY: &core.XY{X: 0.3, Y: 0.3}, TransitionMS: ptr(uint32(1500))}
	sameJSON(t, l.requestBody(on), J{"on": J{"on": true}, "dimming": J{"brightness": 100.0},
		"color_temperature": J{"mirek": 250}, "color": J{"xy": J{"x": 0.3, "y": 0.3}},
		"dynamics": J{"duration": 1500}})
	// a dim-only light gets neither color field
	white, _ := lightFromResource(lightJSON("w1", "Bulb", true, 50))
	sameJSON(t, white.requestBody(on), J{"on": J{"on": true}, "dimming": J{"brightness": 100.0},
		"dynamics": J{"duration": 1500}})
	off := core.LightAction{TransitionMS: ptr(uint32(400))}
	sameJSON(t, white.requestBody(off), J{"on": J{"on": false}, "dynamics": J{"duration": 400}})
}

func TestRoomsAndConnectivityParseDefensively(t *testing.T) {
	zone, ok := roomFromResource(J{"metadata": J{"name": "Z"},
		"children": []any{J{"rtype": "light", "rid": "l1"}, J{"rtype": "device", "rid": "d1"},
			J{"rtype": "other", "rid": "x"}, J{"rid": "no-type"}},
		"services": []any{J{"rtype": "grouped_light", "rid": "gl"}}}, "zone")
	if !ok || zone.Kind != "zone" || zone.GroupedLight != "gl" {
		t.Fatalf("got %+v", zone)
	}
	owners := map[string][]string{"d1": {"l2"}}
	if got := zone.lightIDs(owners); !slices.Equal(got, []string{"l1", "l2"}) {
		t.Fatalf("got %v", got)
	}
	// no grouped_light or no name
	if _, ok := roomFromResource(J{"metadata": J{"name": "A"}}, "room"); ok {
		t.Fatal("a room without grouped_light must be rejected")
	}
	if _, ok := roomFromResource(J{"services": []any{J{"rtype": "grouped_light", "rid": "gl"}}}, "room"); ok {
		t.Fatal("a room without name must be rejected")
	}
	bare, ok := roomFromResource(J{"metadata": J{"name": "B"},
		"services": []any{J{"rtype": "grouped_light", "rid": "g"}}}, "room")
	if !ok || len(bare.lightIDs(nil)) != 0 {
		t.Fatalf("got %+v", bare)
	}

	linkFor := func(status string) (link, bool) {
		_, l, ok := connectivityOf(J{"id": "z", "status": status, "owner": J{"rid": "d"}})
		return l, ok
	}
	if l, ok := linkFor("connected"); !ok || l != (link{Device: "d", Reachable: true}) {
		t.Fatalf("got %+v %v", l, ok)
	}
	if l, _ := linkFor("unidirectional_incoming"); !l.Reachable {
		t.Fatal("unidirectional_incoming still carries commands")
	}
	if l, _ := linkFor("connectivity_issue"); l.Reachable {
		t.Fatal("connectivity_issue is unreachable")
	}
	for _, v := range []J{
		{"id": "z", "status": "connected"},
		{"status": "connected", "owner": J{"rid": "d"}},
		{"id": "z", "owner": J{"rid": "d"}},
	} {
		if _, _, ok := connectivityOf(v); ok {
			t.Fatalf("%v must be rejected", v)
		}
	}
}

func TestSceneMapping(t *testing.T) {
	groups := map[string]string{"room-1": "Living Room"}
	s, ok := sceneFromResource(sceneJSON("12345678-aaaa", "Relax", "room-1", "inactive"), groups)
	if !ok || s.entityID() != "scene.hue_12345678" || s.friendlyName() != "Living Room: Relax" || s.Active {
		t.Fatalf("got %+v", s)
	}
	orphan, _ := sceneFromResource(sceneJSON("a", "Solo", "gone", "static"), groups)
	if orphan.friendlyName() != "Solo" || !orphan.Active {
		t.Fatalf("got %+v", orphan)
	}
	unknown, _ := sceneFromResource(J{"id": "b"}, groups)
	if unknown.Active {
		t.Fatal("a scene without status must not count as active")
	}
}

func TestBridgeBaseForms(t *testing.T) {
	if got := BridgeBase("192.168.1.2"); got != "https://192.168.1.2" {
		t.Fatal(got)
	}
	if got := BridgeBase("http://127.0.0.1:9/"); got != "http://127.0.0.1:9" {
		t.Fatal(got)
	}
}

func lightWithModes(supportsCT, supportsColor bool, mirek *uint16) Light {
	l, _ := lightFromResource(lightJSON("abcdef12-3456", "Desk", true, 50))
	l.SupportsCT, l.SupportsColor, l.Mirek = supportsCT, supportsColor, mirek
	l.XY = &core.XY{X: 0.5, Y: 0.4}
	return l
}

func TestWhiteModeIsReportedOnlyForLightsWithColorTemperature(t *testing.T) {
	a := lightWithModes(true, true, ptr(uint16(300))).attributes()
	sameJSON(t, a["color_mode"], "color_temp")
	sameJSON(t, a["color_temp"], 300)

	// a mirek on a light that cannot do white must not leak into the state
	a = lightWithModes(false, true, ptr(uint16(300))).attributes()
	sameJSON(t, a["color_mode"], "xy")
	if _, has := a["color_temp"]; has {
		t.Fatal("color_temp leaked")
	}
}

func TestXYColorIsReportedOnlyForLightsWithColor(t *testing.T) {
	a := lightWithModes(false, true, nil).attributes()
	sameJSON(t, a["color_mode"], "xy")
	sameJSON(t, a["xy_color"], []float64{0.5, 0.4})

	a = lightWithModes(false, false, nil).attributes()
	for _, key := range []string{"color_mode", "xy_color"} {
		if _, has := a[key]; has {
			t.Fatalf("%s leaked", key)
		}
	}
}

func TestPairingSuccessAndButtonNotPressed(t *testing.T) {
	base := serve(t, newMock().handler())
	key, err := Pair(ctx, base)
	if err != nil || key != "app-key-1" {
		t.Fatalf("got %q %v", key, err)
	}

	waiting := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []any{J{"error": J{"type": 101, "address": "", "description": "link button not pressed"}}})
	})
	if _, err := Pair(ctx, serve(t, waiting)); !errors.Is(err, ErrLinkButtonNotPressed) {
		t.Fatalf("got %v", err)
	}
}

func TestAPutFailsOnHTTPErrorsAndBridgeErrors(t *testing.T) {
	checkGoroutines(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/http-error"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/bridge-error"):
			writeJSON(w, J{"errors": []any{J{"description": "nope"}}})
		default:
			writeJSON(w, J{"errors": []any{}, "data": []any{}})
		}
	})
	client := NewClient(serve(t, handler), "key", Timing{}, quietLogger())
	t.Cleanup(client.Close)
	body := J{"on": J{"on": true}}
	noError(t, client.SetLight(ctx, "fine", body))
	if err := client.SetLight(ctx, "http-error", body); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("got %v", err)
	}
	if err := client.SetLight(ctx, "bridge-error", body); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("got %v", err)
	}
}

// A client without a timeout would wait forever, so the outer timeout turns
// that into a failure.
func TestBridgeHTTPAppliesTheRequestedTimeout(t *testing.T) {
	checkGoroutines(t)
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	noError(t, err)
	var held []net.Conn
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			held = append(held, conn) // accept but never answer
		}
	}()
	t.Cleanup(func() {
		_ = silent.Close()
		<-accepted
		for _, conn := range held {
			_ = conn.Close()
		}
	})

	client := newBridgeHTTP(200*time.Millisecond, time.Second)
	t.Cleanup(client.CloseIdleConnections)
	outer, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(outer, http.MethodGet, "http://"+silent.Addr().String(), nil)
	_, err = client.Do(req)
	var timeout interface{ Timeout() bool }
	if err == nil || !errors.As(err, &timeout) || !timeout.Timeout() || outer.Err() != nil {
		t.Fatalf("the client timeout must fire first, got %v", err)
	}
}

func TestSyncSurvivesMissingScenesConnectivityAndRooms(t *testing.T) {
	lightsOnly := http.NewServeMux()
	lightsOnly.HandleFunc("GET /clip/v2/resource/light", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, J{"errors": []any{}, "data": []any{lightJSON("aaaa1111-0", "Desk", true, 50)}})
	})
	checkGoroutines(t)
	c := newCore(t)
	client := NewClient(serve(t, lightsOnly), "key", Timing{}, quietLogger())
	hue := NewIntegration(IntegrationConfig{Client: client, Core: c, Logger: quietLogger()})
	t.Cleanup(hue.Close)
	c.SetIntegration(IntegrationName, hue)

	n, err := hue.Sync(ctx)
	if err != nil || n != 1 {
		t.Fatalf("got %d %v", n, err)
	}
	if state, _ := c.GetState("light.hue_aaaa1111"); state.State != "on" {
		t.Fatalf("got %+v", state)
	}
	if len(c.LightSets()) != 0 {
		t.Fatal("no rooms expected")
	}
}

func TestAddEventRequestsResync(t *testing.T) {
	c := newCore(t)
	client := NewClient("http://127.0.0.1:1", "k", Timing{}, quietLogger())
	hue := NewIntegration(IntegrationConfig{Client: client, Core: c, Logger: quietLogger()})
	t.Cleanup(hue.Close)
	if !hue.ApplyEventPayload(eventJSON("add", J{"id": "x", "type": "light"})) {
		t.Fatal("an added light must request a resync")
	}
	if hue.ApplyEventPayload(eventJSON("update", J{"id": "x", "type": "motion"})) {
		t.Fatal("an unknown resource type must be ignored")
	}
	if hue.ApplyEventPayload("not json") {
		t.Fatal("garbage must be ignored")
	}
}
