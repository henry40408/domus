package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/henry40408/domus/internal/core"
	"github.com/henry40408/domus/internal/hue"
	"github.com/henry40408/domus/internal/store"
)

// testVersion is the version a test app reports and stamps into the page.
const testVersion = "0.0.0-test"

// testApp is an App on an in-memory database.
type testApp struct {
	app     *App
	store   *store.Store
	handler http.Handler
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	return newTestAppWithVersion(t, testVersion)
}

func newTestAppWithVersion(t *testing.T, version string) *testApp {
	t.Helper()
	return newTestAppWith(t, func(cfg *Config) { cfg.Version = version })
}

// newTestAppWith builds the test app after tweak adjusted its Config.
func newTestAppWith(t *testing.T, tweak func(cfg *Config)) *testApp {
	t.Helper()
	st, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := core.New(st, nil)
	cfg := Config{Core: c, Hue: hue.NewManager(c, hue.Timing{}, nil), Version: testVersion}
	tweak(&cfg)
	app := New(cfg)
	return &testApp{app: app, store: st, handler: app.Handler()}
}

// request describes one request to send to the handler.
type request struct {
	method  string
	path    string
	headers map[string]string
	body    string
}

func (a *testApp) do(t *testing.T, req request) *httptest.ResponseRecorder {
	t.Helper()
	if req.method == "" {
		req.method = http.MethodGet
	}
	r := httptest.NewRequest(req.method, req.path, strings.NewReader(req.body))
	for name, value := range req.headers {
		r.Header.Set(name, value)
		if strings.EqualFold(name, "Host") {
			r.Host = value
		}
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, r)
	return rec
}

func (a *testApp) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	return a.do(t, request{path: path})
}

// reply is the response a request must get. Only the fields that are set are
// checked, except the body: it is compared as parsed JSON when json is set and
// byte for byte otherwise, so an empty text asserts an empty body.
type reply struct {
	status       int
	contentType  string
	cacheControl string
	headers      map[string]string // other headers, compared exactly
	json         string
	text         string
}

// exchange is a named request with the response it must get.
type exchange struct {
	name    string
	request request
	reply   reply
}

const (
	jsonType = "application/json"
	textType = "text/plain; charset=utf-8"
	noStore  = "no-store"
)

// expect sends req and compares the response with want. Placeholders (see
// normalizeText) in header values and JSON bodies stand for random values.
func (a *testApp) expect(t *testing.T, req request, want reply) *httptest.ResponseRecorder {
	t.Helper()
	rec := a.do(t, req)
	if rec.Code != want.status {
		t.Errorf("status %d, want %d (body %s)", rec.Code, want.status, rec.Body)
	}
	headers := map[string]string{"Content-Type": want.contentType, "Cache-Control": want.cacheControl}
	for name, value := range want.headers {
		headers[name] = value
	}
	for name, value := range headers {
		if value == "" {
			continue
		}
		if got := normalizeText(rec.Header().Get(name)); got != value {
			t.Errorf("header %s = %q, want %q", name, got, value)
		}
	}
	if want.json != "" {
		if !jsonEqual(t, []byte(normalizeText(rec.Body.String())), []byte(want.json)) {
			t.Errorf("body %s, want %s", rec.Body, want.json)
		}
		return rec
	}
	if rec.Body.String() != want.text {
		t.Errorf("body %q, want %q", rec.Body, want.text)
	}
	return rec
}

// expectEach runs every exchange on its own fresh app.
func expectEach(t *testing.T, cases []exchange) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { newTestApp(t).expect(t, c.request, c.reply) })
	}
}

// expectInOrder runs the steps on this app one after the other, so a step sees
// what the earlier ones changed.
func (a *testApp) expectInOrder(t *testing.T, steps []exchange) {
	t.Helper()
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) { a.expect(t, step.request, step.reply) })
	}
}

// placeholders stand for values that differ on every run.
var placeholders = []struct {
	pattern *regexp.Regexp
	to      string
}{
	{regexp.MustCompile(`(^|[^0-9a-f])[0-9a-f]{64}($|[^0-9a-f])`), "${1}<HEX64>${2}"}, // session cookie
	{regexp.MustCompile(`(^|[^0-9a-f])[0-9a-f]{32}($|[^0-9a-f])`), "${1}<HEX32>${2}"}, // OAuth code
}

// normalizeText replaces a run of exactly 64 or 32 hex digits by <HEX64> or
// <HEX32>.
func normalizeText(text string) string {
	for _, rule := range placeholders {
		text = rule.pattern.ReplaceAllString(text, rule.to)
	}
	return text
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("not JSON: %s", a)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return reflect.DeepEqual(x, y)
}

func bodyString(rec *httptest.ResponseRecorder) string { return strings.TrimSpace(rec.Body.String()) }
