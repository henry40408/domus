package server

import (
	"net/http"
	"testing"
)

func TestHealthAnswersWithTheVersionAndRefusesOtherMethods(t *testing.T) {
	healthy := `{"status":"ok","version":"` + testVersion + `"}`
	expectEach(t, []exchange{
		{
			name:    "a plain request",
			request: request{path: "/health"},
			reply:   reply{status: 200, contentType: jsonType, cacheControl: noStore, json: healthy},
		},
		{
			name:    "behind an https proxy the answer adds HSTS",
			request: request{path: "/health", headers: map[string]string{"X-Forwarded-Proto": "https"}},
			reply: reply{
				status: 200, contentType: jsonType, cacheControl: noStore, json: healthy,
				headers: map[string]string{"Strict-Transport-Security": "max-age=31536000; includeSubDomains"},
			},
		},
		{
			name:    "a POST is refused with the allowed methods",
			request: request{method: http.MethodPost, path: "/health"},
			reply:   reply{status: 405, cacheControl: noStore, headers: map[string]string{"Allow": "GET,HEAD"}},
		},
	})
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
