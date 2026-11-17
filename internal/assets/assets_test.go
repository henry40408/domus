package assets

import (
	"strings"
	"testing"
)

func TestFilesAreServedWithTheirContentType(t *testing.T) {
	for path, want := range map[string]string{
		"index.html": "text/html; charset=utf-8",
		"app.js":     "text/javascript; charset=utf-8",
		"app.css":    "text/css; charset=utf-8",
		"logo.svg":   "image/svg+xml",
		"data.bin":   "application/octet-stream",
	} {
		if got := contentTypeFor(path); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}

func TestLookupFindsEmbeddedFilesOnly(t *testing.T) {
	for _, path := range []string{"", "index.html", "app.js", "external.js", "style.css", "favicon.svg", "auth/login.js"} {
		if _, _, ok := Lookup(path, "v"); !ok {
			t.Errorf("%q not found", path)
		}
	}
	for _, path := range []string{"nope.txt", "auth", "auth/", "../go.mod", "login.html", "static/app.js"} {
		if _, _, ok := Lookup(path, "v"); ok {
			t.Errorf("%q found", path)
		}
	}
}

func TestOnlyTheIndexIsStamped(t *testing.T) {
	index, _, _ := Lookup("", "1.2.3")
	if strings.Contains(string(index), versionPlaceholder) || !strings.Contains(string(index), "/app.js?v=1.2.3") {
		t.Errorf("index not stamped:\n%s", index)
	}
	app, _, _ := Lookup("app.js", "1.2.3")
	raw, err := files.ReadFile("static/app.js")
	if err != nil || string(app) != string(raw) {
		t.Error("app.js was changed")
	}
}
