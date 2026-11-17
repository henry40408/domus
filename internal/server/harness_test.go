package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/henry40408/domus/internal/core"
	"github.com/henry40408/domus/internal/hue"
	"github.com/henry40408/domus/internal/store"
)

// goldenVersion is the version the Rust goldens were normalised to, so a test
// app that reports it stamps the same bytes the goldens hash.
const goldenVersion = "<VERSION>"

// testApp is an App on an in-memory database.
type testApp struct {
	app     *App
	store   *store.Store
	handler http.Handler
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	return newTestAppWithVersion(t, goldenVersion)
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
	cfg := Config{Core: c, Hue: hue.NewManager(c, hue.Timing{}, nil), Version: goldenVersion}
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

// golden is one response captured from the Rust server (testdata/README).
type golden struct {
	Request struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		// Body is a JSON body, BodyText any other; golden checks that need a
		// body send it themselves.
		Body     json.RawMessage `json:"body"`
		BodyText *string         `json:"body_text"`
	} `json:"request"`
	Response goldenResponse `json:"response"`
}

type goldenResponse struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       json.RawMessage   `json:"body"`
	BodyText   *string           `json:"body_text"`
	BodyEmpty  bool              `json:"body_empty"`
	BodySHA256 string            `json:"body_sha256"`
	BodyLength *int              `json:"body_length"`
}

func loadGolden(t *testing.T, name string) golden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "rust", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return g
}

// checkGolden sends the golden's request and compares status, the headers the
// golden lists, and the body.
func (a *testApp) checkGolden(t *testing.T, name string) {
	t.Helper()
	g := loadGolden(t, name)
	rec := a.do(t, request{method: g.Request.Method, path: g.Request.Path, headers: g.Request.Headers})
	want := g.Response
	if rec.Code != want.Status {
		t.Errorf("%s: status %d, want %d", name, rec.Code, want.Status)
	}
	for header, value := range want.Headers {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s: header %s = %q, want %q", name, header, got, value)
		}
	}
	checkGoldenBody(t, name, rec.Body.Bytes(), want)
}

func checkGoldenBody(t *testing.T, name string, got []byte, want goldenResponse) {
	t.Helper()
	switch {
	case want.BodyEmpty:
		if len(got) != 0 {
			t.Errorf("%s: body %q, want empty", name, got)
		}
	case want.BodySHA256 != "":
		sum := sha256.Sum256(got)
		if hex.EncodeToString(sum[:]) != want.BodySHA256 || len(got) != *want.BodyLength {
			t.Errorf("%s: body differs from the Rust one (%d bytes, want %d)", name, len(got), *want.BodyLength)
		}
	case want.BodyText != nil:
		if string(got) != *want.BodyText {
			t.Errorf("%s: body %q, want %q", name, got, *want.BodyText)
		}
	case want.Body != nil:
		if !jsonEqual(t, got, want.Body) {
			t.Errorf("%s: body %s, want %s", name, got, want.Body)
		}
	}
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
