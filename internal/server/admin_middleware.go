package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/store"
)

// adminPrefix is where the admin API is mounted.
const adminPrefix = "/api/domus"

// access is the set of checks a route needs before its handler runs. They run
// in the order session, admin, recent password.
type access uint8

const (
	// open needs nothing: anyone may call the route.
	open access = 0
	// needSession needs a logged-in user (401 otherwise).
	needSession access = 1 << iota
	// needAdmin needs an admin (403 otherwise); it implies a session.
	needAdmin
	// needRecentAuth blocks an app-started session whose password was not
	// re-entered lately (403 with reauth).
	needRecentAuth
)

type adminHandler struct {
	method  string
	access  access
	handler http.HandlerFunc
}

// adminRoutes collects the admin API's routes before they are mounted, so a
// path with several methods gets one handler, like an axum method router.
type adminRoutes struct {
	app    *App
	mux    *http.ServeMux
	paths  []string
	byPath map[string][]adminHandler
}

func newAdminRoutes(app *App, mux *http.ServeMux) *adminRoutes {
	return &adminRoutes{app: app, mux: mux, byPath: map[string][]adminHandler{}}
}

// route registers pattern ("POST /login") behind the checks in level. Level
// needAdmin implies needSession.
func (ar *adminRoutes) route(pattern string, level access, handler http.HandlerFunc) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		panic("admin route needs a method: " + pattern)
	}
	if level&needAdmin != 0 || level&needRecentAuth != 0 {
		level |= needSession
	}
	if _, seen := ar.byPath[path]; !seen {
		ar.paths = append(ar.paths, path)
	}
	ar.byPath[path] = append(ar.byPath[path], adminHandler{method: method, access: level, handler: handler})
}

// mount registers every collected path. Paths carry no method in the mux, so
// the checks run before the method is judged: a wrong method on a protected
// path is a 401 first, like axum's layers around a method router.
func (ar *adminRoutes) mount() {
	for _, path := range ar.paths {
		ar.mux.Handle(adminPrefix+path, sameOrigin(ar.dispatch(ar.byPath[path])))
	}
}

func (ar *adminRoutes) dispatch(handlers []adminHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range handlers {
			if routeServes(h.method, r.Method) {
				ar.app.guarded(h.access, h.handler).ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Allow", allowHeader(handlers))
		ar.app.guarded(commonSessionAccess(handlers), methodNotAllowed).ServeHTTP(w, r)
	})
}

func methodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusMethodNotAllowed)
}

// routeServes is true when a route registered for method answers requestMethod
// (a GET route answers HEAD too).
func routeServes(method, requestMethod string) bool {
	return method == requestMethod || (method == http.MethodGet && requestMethod == http.MethodHead)
}

func allowHeader(handlers []adminHandler) string {
	var methods []string
	for _, h := range handlers {
		methods = append(methods, h.method)
		if h.method == http.MethodGet {
			methods = append(methods, http.MethodHead)
		}
	}
	return strings.Join(methods, ",")
}

// commonSessionAccess is needSession when every method on the path needs one,
// so a wrong method does not reveal a protected path to anonymous callers.
func commonSessionAccess(handlers []adminHandler) access {
	for _, h := range handlers {
		if h.access&needSession == 0 {
			return open
		}
	}
	return needSession
}

// guarded runs the checks in level, then handler.
func (a *App) guarded(level access, handler http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if level&needSession != 0 {
			user, ok := a.currentUser(r)
			if !ok {
				writeMessage(w, http.StatusUnauthorized, "Login required.")
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, user))
			if level&needAdmin != 0 && !user.IsAdmin {
				writeMessage(w, http.StatusForbidden, "Admin only.")
				return
			}
		}
		if level&needRecentAuth != 0 && a.sessionNeedsReauth(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"message": "Enter your password to continue.", "reauth": true})
			return
		}
		handler(w, r)
	})
}

type userKey struct{}

// sessionUser is the user guarded found; only handlers behind needSession may
// call it.
func sessionUser(r *http.Request) store.User {
	user, _ := r.Context().Value(userKey{}).(store.User)
	return user
}

// currentUser is the user of the request's session cookie.
func (a *App) currentUser(r *http.Request) (store.User, bool) {
	token, ok := sessionToken(r)
	if !ok {
		return store.User{}, false
	}
	user, err := a.core.Store().SessionUser(r.Context(), auth.SHA256Hex(token))
	if err == nil {
		return user, true
	}
	a.warnUnlessNotFound("session lookup failed", err)
	return store.User{}, false
}

// sessionNeedsReauth fails closed: when the store cannot say, the sensitive
// action waits for the password.
func (a *App) sessionNeedsReauth(r *http.Request) bool {
	token, ok := sessionToken(r)
	if !ok {
		return false
	}
	needs, err := a.core.Store().SessionNeedsReauth(r.Context(), auth.SHA256Hex(token))
	if err != nil {
		a.logger.Warn("reauth lookup failed", "error", err)
		return true
	}
	return needs
}

// sameOrigin refuses cross-site state-changing requests, as defence in depth
// next to SameSite=Strict. It wraps every admin route, login and setup
// included.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if crossSite(r) {
			writeMessage(w, http.StatusForbidden, "Cross-site request refused.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// crossSite decides by Sec-Fetch-Site when the browser sends it, and by
// Origin otherwise. A request without either (curl, the Companion app) passes.
func crossSite(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	if site, ok := firstHeader(r, "Sec-Fetch-Site"); ok {
		return site != "same-origin" && site != "none"
	}
	origin, ok := firstHeader(r, "Origin")
	if !ok {
		return false
	}
	_, originHost, hasScheme := strings.Cut(origin, "://")
	if !hasScheme {
		return true
	}
	return !matchesHost(r, originHost)
}

// matchesHost compares the host of the Origin with the one the client
// addressed: the proxy's X-Forwarded-Host or the Host header, each reduced to
// its first comma-separated entry.
func matchesHost(r *http.Request, originHost string) bool {
	forwarded, _ := firstHeader(r, "X-Forwarded-Host")
	for _, host := range []string{forwarded, r.Host} {
		first, _, _ := strings.Cut(host, ",")
		if host != "" && strings.EqualFold(strings.TrimSpace(first), originHost) {
			return true
		}
	}
	return false
}

func firstHeader(r *http.Request, name string) (string, bool) {
	values := r.Header.Values(name)
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}
