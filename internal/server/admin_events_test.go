package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henry40408/domus/internal/store"
)

const (
	eventsPath      = "/api/domus/events"
	eventWait       = 2 * time.Second
	testRecheckTime = 20 * time.Millisecond
)

// eventStream reads frames of a server-sent event stream.
type eventStream struct {
	response *http.Response
	frames   chan string
}

// openEvents connects to the stream of a running test server and returns once
// the response headers arrive, which is after the subscription started.
func openEvents(t *testing.T, serverURL, cookie string) (*eventStream, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, serverURL+eventsPath, nil)
	noError(t, err)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	response, err := http.DefaultClient.Do(req)
	noError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	stream := &eventStream{response: response, frames: make(chan string, 64)}
	go stream.readFrames()
	return stream, response.StatusCode
}

func (s *eventStream) readFrames() {
	defer close(s.frames)
	reader := bufio.NewReader(s.response.Body)
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		frame.WriteString(line)
		if line == "\n" {
			s.frames <- frame.String()
			frame.Reset()
		}
	}
}

// next is the next frame that is not a keep-alive comment.
func (s *eventStream) next(t *testing.T) string {
	t.Helper()
	timeout := time.After(eventWait)
	for {
		select {
		case frame, open := <-s.frames:
			if !open {
				t.Fatal("the stream ended")
			}
			if !strings.HasPrefix(frame, ":") {
				return frame
			}
		case <-timeout:
			t.Fatal("no event in time")
		}
	}
}

// ended reports whether the stream closes within the wait.
func (s *eventStream) ended() bool {
	timeout := time.After(eventWait)
	for {
		select {
		case _, open := <-s.frames:
			if !open {
				return true
			}
		case <-timeout:
			return false
		}
	}
}

func startEventServer(t *testing.T, app *testApp) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(app.handler)
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return server
}

func newEventsApp(t *testing.T, shutdown <-chan struct{}) *testApp {
	t.Helper()
	return newTestAppWith(t, func(cfg *Config) {
		cfg.Timing = Timing{EventsRecheck: testRecheckTime}
		cfg.Shutdown = shutdown
	})
}

func TestEventsNeedASessionAndSayTheyAreAStream(t *testing.T) {
	app := newEventsApp(t, nil)
	admin := app.setupAdmin(t)
	server := startEventServer(t, app)
	_, code := openEvents(t, server.URL, "")
	equal(t, code, 401)
	stream, code := openEvents(t, server.URL, admin)
	equal(t, code, 200)
	equal(t, stream.response.Header.Get("Content-Type"), "text/event-stream")
}

func TestEventsStreamOnlyWhatTheUserMaySee(t *testing.T) {
	app := newEventsApp(t, nil)
	admin, bob := app.adminAndMember(t)
	app.setGroup(t, "G", "light.a")
	noError(t, app.store.SetUserScope(bg, app.userID(t, "bob"), store.Scope{"G"}))
	server := startEventServer(t, app)
	adminStream, _ := openEvents(t, server.URL, admin)
	bobStream, _ := openEvents(t, server.URL, bob)

	app.app.core.SetState("light.b", "on", nil)
	app.app.core.SetState("light.a", "on", nil)

	// the admin sees both, in order
	equal(t, adminStream.next(t), "event: change\ndata: light.b\n\n")
	equal(t, adminStream.next(t), "event: change\ndata: light.a\n\n")
	// bob is limited to group G, so light.b never reaches him
	equal(t, bobStream.next(t), "event: change\ndata: light.a\n\n")
}

func TestEventsEndWithTheSession(t *testing.T) {
	app := newEventsApp(t, nil)
	_, bob := app.adminAndMember(t)
	server := startEventServer(t, app)
	stream, _ := openEvents(t, server.URL, bob)

	// the stream outlives rechecks while the session is alive
	time.Sleep(testRecheckTime * 3)
	_, list := app.adminJSON(t, http.MethodGet, "/api/domus/sessions", bob, "")
	id := int64(asMap(t, asList(t, list)[0])["id"].(float64))
	path := "/api/domus/sessions/" + strconv.FormatInt(id, 10)
	equal(t, app.adminStatus(t, http.MethodDelete, path, bob, ""), 200)
	equal(t, stream.ended(), true)
}

func TestEventsEndOnShutdown(t *testing.T) {
	shutdown := make(chan struct{})
	app := newEventsApp(t, shutdown)
	admin := app.setupAdmin(t)
	server := startEventServer(t, app)
	stream, _ := openEvents(t, server.URL, admin)
	close(shutdown)
	equal(t, stream.ended(), true)
}

func TestEventsSendKeepAliveComments(t *testing.T) {
	app := newTestAppWith(t, func(cfg *Config) {
		cfg.Timing = Timing{EventsKeepAlive: testRecheckTime}
	})
	admin := app.setupAdmin(t)
	server := startEventServer(t, app)
	stream, _ := openEvents(t, server.URL, admin)
	select {
	case frame := <-stream.frames:
		equal(t, frame, ":\n\n")
	case <-time.After(eventWait):
		t.Fatal("no keep-alive")
	}
}

func TestEventsSendNoKeepAliveWhileChangesKeepComing(t *testing.T) {
	const keepAlive = 500 * time.Millisecond
	app := newTestAppWith(t, func(cfg *Config) {
		cfg.Timing = Timing{EventsKeepAlive: keepAlive}
	})
	admin := app.setupAdmin(t)
	server := startEventServer(t, app)
	stream, _ := openEvents(t, server.URL, admin)

	// A change every fifth of the interval for more than two intervals: a
	// ticker that is not reset after each frame would fire in between.
	for i := 0; i < 12; i++ {
		app.app.core.SetState("light.a", strconv.Itoa(i), nil)
		time.Sleep(keepAlive / 5)
	}
	for drained := false; !drained; {
		select {
		case frame := <-stream.frames:
			if strings.HasPrefix(frame, ":") {
				t.Fatalf("a keep-alive came while changes were flowing: %q", frame)
			}
		default:
			drained = true
		}
	}
	// Once the stream is quiet, the keep-alive returns.
	select {
	case frame := <-stream.frames:
		equal(t, frame, ":\n\n")
	case <-time.After(eventWait):
		t.Fatal("no keep-alive on the quiet stream")
	}
}

// stalledWriter is a response writer that blocks every body write until
// release is closed, like a client that stopped reading.
type stalledWriter struct {
	header  http.Header
	release chan struct{}
	started chan struct{}
	once    sync.Once

	mu   sync.Mutex
	body strings.Builder
}

func (w *stalledWriter) Header() http.Header { return w.header }
func (w *stalledWriter) WriteHeader(int)     {}
func (w *stalledWriter) Flush()              { w.once.Do(func() { close(w.started) }) }

func (w *stalledWriter) Write(data []byte) (int, error) {
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(data)
}

func (w *stalledWriter) written() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func TestEventsReportMissedChanges(t *testing.T) {
	app := newEventsApp(t, nil)
	admin := app.setupAdmin(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, eventsPath, nil).WithContext(ctx)
	req.Header.Set("Cookie", admin)
	writer := &stalledWriter{header: http.Header{}, release: make(chan struct{}), started: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.handler.ServeHTTP(writer, req)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-writer.started

	// the stalled stream cannot keep up with a burst larger than its buffer
	for i := 0; i < 300; i++ {
		app.app.core.SetState("light.l"+strconv.Itoa(i), "on", nil)
	}
	close(writer.release)

	deadline := time.Now().Add(eventWait)
	for !strings.Contains(writer.written(), "event: change\ndata: *\n\n") {
		if time.Now().After(deadline) {
			t.Fatalf("no missed-changes event in %q", writer.written())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
