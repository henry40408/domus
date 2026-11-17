package server

import (
	"net/http"
	"net/url"
	"strings"
)

// Handler returns the whole HTTP application behind the security headers.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.health)
	a.registerAPI(mux)
	a.registerAdmin(mux)
	a.registerOAuth(mux)
	a.registerMobile(mux)
	a.registerWebsocket(mux)
	mux.Handle("/", fallback(mux, a.staticFiles))
	return securityHeaders(mux)
}

// fallbackPattern is the mux pattern the fallback handler is registered on.
const fallbackPattern = "/"

// probeMethods are the methods fallback tries, in the order of the Allow
// header it builds.
var probeMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodDelete, http.MethodPatch, http.MethodOptions,
}

// fallback serves everything no route claims, for any method, like axum's
// fallback. A path that a route serves only for other methods is answered
// 405 with an Allow header instead, like axum's method router; Go's mux
// cannot do that once a catch-all pattern exists.
func fallback(mux *http.ServeMux, notRouted http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := allowedMethods(mux, r)
		if len(allowed) == 0 {
			notRouted(w, r)
			return
		}
		w.Header().Set("Allow", strings.Join(allowed, ","))
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
}

// allowedMethods lists the methods some route (not the fallback) serves for
// the request's path.
func allowedMethods(mux *http.ServeMux, r *http.Request) []string {
	var allowed []string
	for _, method := range probeMethods {
		probe := &http.Request{Method: method, Host: r.Host, URL: &url.URL{Path: r.URL.Path}}
		if _, pattern := mux.Handler(probe); pattern != "" && pattern != fallbackPattern {
			allowed = append(allowed, method)
		}
	}
	return allowed
}
