package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/henry40408/domus/internal/version"
)

func TestBuildKnowsItsVersion(t *testing.T) {
	v := version.Version()
	if v == "" || strings.ContainsAny(v, " \t\n") {
		t.Errorf("version %q", v)
	}
}

func TestHealthIsPublicUncachedAndFollowsTheDatabase(t *testing.T) {
	app := newTestAppWithVersion(t, "1.2.3")

	rec := app.get(t, "/health") // no cookie, no token
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, cache-control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if !jsonEqual(t, rec.Body.Bytes(), []byte(`{"status":"ok","version":"1.2.3"}`)) {
		t.Errorf("body %s", rec.Body)
	}
	if got := bodyString(rec); got != `{"status":"ok","version":"1.2.3"}` {
		t.Errorf("key order or spacing differs from serde_json: %s", got)
	}

	if err := app.store.Close(); err != nil {
		t.Fatal(err)
	}
	rec = app.get(t, "/health")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["status"] != "error" {
		t.Errorf("body %s (%v)", rec.Body, err)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	app := newTestApp(t)
	for _, path := range []string{"/", "/nope", "/api/domus/status", "/api/states/light.x", "/health"} {
		h := app.get(t, path).Header()
		if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", path, h.Get("Content-Security-Policy"))
		}
		for name, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := h.Get(name); got != want {
				t.Errorf("%s: %s = %q, want %q", path, name, got, want)
			}
		}
		if h.Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS without https", path)
		}
	}
}

func TestNoStoreOnlyForAdminApiAndHstsOnlyBehindHttps(t *testing.T) {
	app := newTestApp(t)
	if got := app.get(t, "/api/domus/status").Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("admin API: %q", got)
	}
	// The page is revalidated (see static.go), not forbidden from being stored.
	if got := app.get(t, "/").Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("page: %q", got)
	}
	secure := app.do(t, request{path: "/", headers: map[string]string{"X-Forwarded-Proto": "HTTPS"}})
	if got := secure.Header().Get("Strict-Transport-Security"); !strings.HasPrefix(got, "max-age=") {
		t.Errorf("HSTS: %q", got)
	}
}

func TestNoStoreWinsOverTheHandlersCacheControl(t *testing.T) {
	// /auth/login.js is a static file the handler marks no-cache; the path
	// rule must override it (golden static_auth_login_js).
	app := newTestApp(t)
	if got := app.get(t, "/auth/login.js").Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("got %q", got)
	}
}
