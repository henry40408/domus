// Package assets embeds the admin page and its scripts and serves them with
// the version stamp the cache rules rely on.
package assets

import (
	"bytes"
	"embed"
	"strings"
)

//go:embed static
var files embed.FS

// versionPlaceholder is replaced by the build version in index.html, so every
// asset URL on the page changes with each release.
const versionPlaceholder = "__ASSET_VERSION__"

// indexPath is the only file that carries the version placeholder.
const indexPath = "index.html"

// Lookup returns the embedded file at path (no leading slash; "" is the
// index). index.html has its version placeholder replaced by version.
func Lookup(path, version string) (body []byte, contentType string, ok bool) {
	if path == "" {
		path = indexPath
	}
	body, err := files.ReadFile("static/" + path)
	if err != nil {
		return nil, "", false
	}
	if path == indexPath {
		body = bytes.ReplaceAll(body, []byte(versionPlaceholder), []byte(version))
	}
	return body, contentTypeFor(path), true
}

// contentTypeFor goes by the text after the last dot, or the whole path when
// there is no dot.
func contentTypeFor(path string) string {
	switch path[strings.LastIndex(path, ".")+1:] {
	case "html":
		return "text/html; charset=utf-8"
	case "js":
		return "text/javascript; charset=utf-8"
	case "css":
		return "text/css; charset=utf-8"
	case "svg":
		return "image/svg+xml"
	}
	return "application/octet-stream"
}
