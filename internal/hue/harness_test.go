package hue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/henry40408/domus/internal/core"
	"github.com/henry40408/domus/internal/store"
)

var ctx = context.Background()

// J is a JSON object literal.
type J = map[string]any

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func noError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// sameJSON compares two values by their JSON form, so numeric types and map
// key order do not matter.
func sameJSON(t *testing.T, got, want any) {
	t.Helper()
	g, errG := json.Marshal(got)
	w, errW := json.Marshal(want)
	if errG != nil || errW != nil {
		t.Fatalf("cannot marshal: %v %v", errG, errW)
	}
	if string(g) != string(w) {
		t.Fatalf("got  %s\nwant %s", g, w)
	}
}

func ptr[T any](v T) *T { return &v }

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

// checkGoroutines fails the test when it leaves goroutines behind. Call it
// first: cleanups run in reverse order, so this one runs after all the others.
func checkGoroutines(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for runtime.NumGoroutine() > before {
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<16)
				buf = buf[:runtime.Stack(buf, true)]
				t.Errorf("goroutine leak: %d before, %d after\n%s", before, runtime.NumGoroutine(), buf)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// waitFor polls until condition holds or five seconds pass.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type put struct {
	ID   string
	Body any
}

// mockBridge is a fake Hue bridge.
type mockBridge struct {
	mu        sync.Mutex
	resources map[string][]any
	puts      []put
	events    []string
	// holdOpen keeps the event stream open after the events were sent.
	holdOpen bool
	// streamStatus makes the event stream answer with this status when not 0.
	streamStatus int
	lists        map[string]int
	streams      int
}

func newMock() *mockBridge {
	return &mockBridge{resources: map[string][]any{}, lists: map[string]int{}}
}

func (m *mockBridge) set(kind string, items ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resources[kind] = items
}

func (m *mockBridge) setEvents(events ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = events
}

func (m *mockBridge) listCount(kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lists[kind]
}

func (m *mockBridge) streamCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams
}

func (m *mockBridge) recordedPuts() []put {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.puts)
}

func (m *mockBridge) clearPuts() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts = nil
}

func (m *mockBridge) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /clip/v2/resource/{kind}", m.list)
	mux.HandleFunc("PUT /clip/v2/resource/{kind}/{id}", m.put)
	mux.HandleFunc("GET /eventstream/clip/v2", m.eventStream)
	mux.HandleFunc("POST /api", m.pair)
	return mux
}

func (m *mockBridge) list(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	switch kind {
	case "light", "scene", "smart_scene", "room", "zone", "zigbee_connectivity":
	default:
		http.NotFound(w, r)
		return
	}
	m.mu.Lock()
	m.lists[kind]++
	items := m.resources[kind]
	m.mu.Unlock()
	if items == nil {
		items = []any{}
	}
	writeJSON(w, J{"errors": []any{}, "data": items})
}

func (m *mockBridge) put(w http.ResponseWriter, r *http.Request) {
	var body any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Header.Get("hue-application-key") == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.puts = append(m.puts, put{ID: r.PathValue("id"), Body: body})
	m.mu.Unlock()
	writeJSON(w, J{"errors": []any{}, "data": []any{}})
}

func (m *mockBridge) eventStream(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.streams++
	events, hold, status := slices.Clone(m.events), m.holdOpen, m.streamStatus
	m.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, event := range events {
		_, _ = fmt.Fprintf(w, "id: 1:0\ndata: %s\n\n", event)
	}
	w.(http.Flusher).Flush()
	if hold {
		<-r.Context().Done()
	}
}

func (m *mockBridge) pair(w http.ResponseWriter, r *http.Request) {
	var body J
	if json.NewDecoder(r.Body).Decode(&body) != nil || body["generateclientkey"] != true {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	writeJSON(w, []any{J{"success": J{"username": "app-key-1", "clientkey": "x"}}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// serve starts the mock and returns its base URL.
func serve(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

// testEnv is a core with a Hue integration registered and pointed at a mock.
type testEnv struct {
	core *core.Core
	hue  *Integration
	mock *mockBridge
	base string
}

func newCore(t *testing.T) *core.Core {
	t.Helper()
	st, err := store.OpenMemory(ctx)
	noError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return core.New(st, quietLogger())
}

func newEnv(t *testing.T, mock *mockBridge, timing Timing) testEnv {
	t.Helper()
	checkGoroutines(t)
	base := serve(t, mock.handler())
	c := newCore(t)
	hue := NewIntegration(IntegrationConfig{
		Client: NewClient(base, "key", timing, quietLogger()),
		Core:   c,
		Timing: timing,
		Logger: quietLogger(),
	})
	t.Cleanup(hue.Close)
	c.SetIntegration(IntegrationName, hue)
	return testEnv{core: c, hue: hue, mock: mock, base: base}
}

func (e testEnv) state(t *testing.T, id string) core.State {
	t.Helper()
	state, ok := e.core.GetState(id)
	if !ok {
		t.Fatalf("no state for %s", id)
	}
	return state
}

func (e testEnv) sync(t *testing.T) int {
	t.Helper()
	n, err := e.hue.Sync(ctx)
	noError(t, err)
	return n
}

func eventJSON(kind string, item any) string {
	encoded, _ := json.Marshal([]any{J{"type": kind, "data": []any{item}}})
	return string(encoded)
}

func lightJSON(id, name string, on bool, brightness float64) J {
	return J{"id": id, "type": "light", "metadata": J{"name": name}, "on": J{"on": on},
		"dimming": J{"brightness": brightness}}
}

func richLightJSON(id, owner string) J {
	return J{"id": id, "type": "light", "metadata": J{"name": id}, "on": J{"on": true},
		"dimming": J{"brightness": 50.0}, "owner": J{"rid": owner, "rtype": "device"},
		"color_temperature": J{"mirek": 300.0, "mirek_valid": true},
		"color":             J{"xy": J{"x": 0.5, "y": 0.4}}}
}

func sceneJSON(id, name, room, active string) J {
	return J{"id": id, "type": "scene", "metadata": J{"name": name},
		"group": J{"rid": room, "rtype": "room"}, "status": J{"active": active}}
}

func smartSceneJSON(id, name, room, state string) J {
	return J{"id": id, "type": "smart_scene", "metadata": J{"name": name},
		"group": J{"rid": room, "rtype": "room"}, "state": state}
}
