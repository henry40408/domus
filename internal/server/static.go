package server

import (
	"net/http"
	"strings"

	"github.com/henry40408/domus/internal/assets"
	"github.com/henry40408/domus/internal/version"
)

const (
	cacheForever = "public, max-age=31536000, immutable"
	cacheRevalid = "no-cache"
)

// staticFiles serves the embedded page for any method; a missing file is an
// empty 404 (there is no single-page-app fallback).
//
// index.html stamps every asset URL with the build version, so a new release
// means new URLs and no cache (browser or CDN) can serve a stale file. Only a
// URL carrying the current version is cached for good; anything else, and the
// page itself, is revalidated every time.
func (a *App) staticFiles(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimLeft(r.URL.EscapedPath(), "/")
	body, contentType, ok := assets.Lookup(path, a.version)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControlFor(r, a.version))
	_, _ = w.Write(body)
}

// cacheControlFor pins a stamped URL only for a build whose version
// identifies its content: a dev or dirty version does not change between
// edits, so it is never cached.
func cacheControlFor(r *http.Request, buildVersion string) string {
	path := r.URL.EscapedPath()
	if !version.Cacheable(buildVersion) || path == "/" || path == "/index.html" {
		return cacheRevalid
	}
	if !hasVersionParam(r.URL.RawQuery, buildVersion) {
		return cacheRevalid
	}
	return cacheForever
}

// hasVersionParam looks for an exact v=<version> pair in the raw query.
func hasVersionParam(rawQuery, buildVersion string) bool {
	for _, pair := range strings.Split(rawQuery, "&") {
		if value, ok := strings.CutPrefix(pair, "v="); ok && value == buildVersion {
			return true
		}
	}
	return false
}
