package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/henry40408/domus/internal/assets"
)

func TestStaticIndexIsServed(t *testing.T) {
	app := newTestApp(t)
	if got := app.get(t, "/").Code; got != http.StatusOK {
		t.Errorf("/: %d", got)
	}
	if got := app.get(t, "/nope.txt").Code; got != http.StatusNotFound {
		t.Errorf("/nope.txt: %d", got)
	}
	icon := app.get(t, "/favicon.svg")
	if icon.Code != http.StatusOK || icon.Header().Get("Content-Type") != "image/svg+xml" {
		t.Errorf("favicon: %d %q", icon.Code, icon.Header().Get("Content-Type"))
	}
}

func TestAssetsAreStampedAndCachedByVersion(t *testing.T) {
	const version = "1.2.3"
	app := newTestAppWithVersion(t, version)
	cache := func(rec *httptest.ResponseRecorder) string { return rec.Header().Get("Cache-Control") }

	index := app.get(t, "/")
	if cache(index) != "no-cache" {
		t.Errorf("index: %q", cache(index))
	}
	html := index.Body.String()
	if strings.Contains(html, "__ASSET_VERSION__") {
		t.Error("placeholder left in the page")
	}
	stamped := "/app.js?v=" + version
	if !strings.Contains(html, stamped) {
		t.Errorf("page does not reference %s", stamped)
	}
	// The Companion apps' bridge is part of the page and defines what they wait for.
	if !strings.Contains(html, "/external.js?v="+version) {
		t.Error("page does not load external.js")
	}
	bridge := app.get(t, "/external.js").Body.String()
	for _, want := range []string{"external_auth", "connection-status", "config_screen/show"} {
		if !strings.Contains(bridge, want) {
			t.Errorf("external.js lacks %q", want)
		}
	}

	// bare URLs are never pinned, so a stale reference cannot stick
	if got := cache(app.get(t, "/app.js")); got != "no-cache" {
		t.Errorf("bare: %q", got)
	}
	// stamped ones are pinned for a build whose version identifies its content
	if got := cache(app.get(t, stamped)); got != "public, max-age=31536000, immutable" {
		t.Errorf("stamped: %q", got)
	}
	if got := cache(app.get(t, "/app.js?v=other")); got != "no-cache" {
		t.Errorf("wrong version: %q", got)
	}
	if got := cache(app.get(t, "/?v="+version)); got != "no-cache" {
		t.Errorf("stamped index: %q", got)
	}
}

func TestAnUnstableVersionIsNeverPinned(t *testing.T) {
	for _, version := range []string{"dev", "1.2.3-dirty", "unknown", "v1.2.3+dirty"} {
		app := newTestAppWithVersion(t, version)
		rec := app.get(t, "/app.js?v="+version)
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: %q", version, got)
		}
	}
}

func TestOnlyAStableVersionStampedAssetIsCachedForGood(t *testing.T) {
	const forever = "public, max-age=31536000, immutable"
	cache := func(target, version string) string {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		return cacheControlFor(r, version)
	}
	for _, c := range []struct{ target, version, want string }{
		{"/app.js?v=1.2.3", "1.2.3", forever},
		{"/app.js?x=1&v=1.2.3", "1.2.3", forever},
		{"/app.js?v=1.2.2", "1.2.3", "no-cache"},
		{"/app.js", "1.2.3", "no-cache"},
		// the page itself is always revalidated, whatever its query says
		{"/?v=1.2.3", "1.2.3", "no-cache"},
		{"/index.html?v=1.2.3", "1.2.3", "no-cache"},
		// a version that does not change between edits must never be cached
		{"/app.js?v=dev", "dev", "no-cache"},
		{"/app.js?v=1.2.3-dirty", "1.2.3-dirty", "no-cache"},
	} {
		if got := cache(c.target, c.version); got != c.want {
			t.Errorf("%s (%s): got %q, want %q", c.target, c.version, got, c.want)
		}
	}
}

// embedded is the body the server must send for an asset: the file as the
// assets package ships it, with the version stamped into the page.
func embedded(t *testing.T, path string) string {
	t.Helper()
	body, _, ok := assets.Lookup(path, testVersion)
	if !ok {
		t.Fatalf("no embedded file %q", path)
	}
	return string(body)
}

func TestStaticFilesAreServedWithTheirTypeAndCacheRule(t *testing.T) {
	const forever = "public, max-age=31536000, immutable"
	asset := func(path, contentType, cacheControl string) reply {
		return reply{status: 200, contentType: contentType, cacheControl: cacheControl, text: embedded(t, path)}
	}
	html, js := "text/html; charset=utf-8", "text/javascript; charset=utf-8"
	newTestApp(t).expectInOrder(t, []exchange{
		{name: "the index at the root", request: request{path: "/"}, reply: asset("", html, "no-cache")},
		{name: "the index by name", request: request{path: "/index.html"}, reply: asset("", html, "no-cache")},
		{
			name:    "the index is revalidated even with the current version",
			request: request{path: "/index.html?v=" + testVersion},
			reply:   asset("", html, "no-cache"),
		},
		{name: "a script without a version", request: request{path: "/app.js"}, reply: asset("app.js", js, "no-cache")},
		{
			name:    "a script with the current version is cached for good",
			request: request{path: "/app.js?v=" + testVersion},
			reply:   asset("app.js", js, forever),
		},
		{
			name:    "a script with another version is revalidated",
			request: request{path: "/app.js?v=other"},
			reply:   asset("app.js", js, "no-cache"),
		},
		{name: "the bridge script", request: request{path: "/external.js"}, reply: asset("external.js", js, "no-cache")},
		{
			name:    "the stylesheet",
			request: request{path: "/style.css"},
			reply:   asset("style.css", "text/css; charset=utf-8", "no-cache"),
		},
		{
			name:    "the icon",
			request: request{path: "/favicon.svg"},
			reply:   asset("favicon.svg", "image/svg+xml", "no-cache"),
		},
		{
			name:    "the login script is never stored",
			request: request{path: "/auth/login.js"},
			reply:   asset("auth/login.js", js, noStore),
		},
		{
			name:    "any method falls back to the static files",
			request: request{method: http.MethodPost, path: "/app.js"},
			reply:   asset("app.js", js, "no-cache"),
		},
		{name: "a missing file is an empty 404", request: request{path: "/nope.txt"}, reply: reply{status: 404}},
		{name: "the OAuth page is not a static file", request: request{path: "/login.html"}, reply: reply{status: 404}},
	})
}

func TestLoginHTMLIsNotAStaticFile(t *testing.T) {
	app := newTestApp(t)
	for _, path := range []string{"/login.html", "/oauth_login.html", "/auth/login.html"} {
		if got := app.get(t, path).Code; got != http.StatusNotFound {
			t.Errorf("%s: %d", path, got)
		}
	}
}

func TestStaticDirectoriesAreNotFiles(t *testing.T) {
	app := newTestApp(t)
	for _, path := range []string{"/auth", "/auth/", "/static/app.js"} {
		if got := app.get(t, path).Code; got != http.StatusNotFound {
			t.Errorf("%s: %d", path, got)
		}
	}
}
