package server

import (
	"net/http"
	"testing"
)

func TestHealthMatchesTheRustResponses(t *testing.T) {
	app := newTestApp(t)
	for _, name := range []string{"health_ok", "health_https_forwarded", "health_post_405"} {
		t.Run(name, func(t *testing.T) { app.checkGolden(t, name) })
	}
}

func TestAWrongMethodOnAKnownPathIsMethodNotAllowed(t *testing.T) {
	app := newTestApp(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		rec := app.do(t, request{method: method, path: "/health"})
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET,HEAD" || rec.Body.Len() != 0 {
			t.Errorf("%s: %d allow=%q body=%q", method, rec.Code, rec.Header().Get("Allow"), rec.Body)
		}
	}
	if got := app.do(t, request{method: http.MethodHead, path: "/health"}).Code; got != http.StatusOK {
		t.Errorf("HEAD: %d", got)
	}
}

func TestUnknownPathsFallBackToTheStaticFilesForAnyMethod(t *testing.T) {
	app := newTestApp(t)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if got := app.do(t, request{method: method, path: "/health/"}).Code; got != http.StatusNotFound {
			t.Errorf("%s /health/: %d", method, got)
		}
		if got := app.do(t, request{method: method, path: "/app.js"}).Code; got != http.StatusOK {
			t.Errorf("%s /app.js: %d", method, got)
		}
	}
}
