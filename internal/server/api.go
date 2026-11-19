package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/core"
	"github.com/henry40408/domus/internal/store"
)

// tokenHandler serves a request that carries a valid access token.
type tokenHandler func(w http.ResponseWriter, r *http.Request, token store.TokenAuth)

// registerAPI mounts the Home Assistant compatible REST subset.
func (a *App) registerAPI(mux *http.ServeMux) {
	mux.Handle("/api/states", a.tokenRoute(http.MethodGet, a.listStates))
	mux.Handle("/api/states/{entity_id}", a.tokenRoute(http.MethodGet, a.getState))
	mux.Handle("/api/services/light/{service}", a.tokenRoute(http.MethodPost, a.lightService))
	mux.Handle("/api/services/scene/{service}", a.tokenRoute(http.MethodPost, a.sceneService))
}

// tokenRoute checks the token before the method, like axum's layer around a
// method router: a wrong method with a bad token is a 401, not a 405, and the
// 401 still carries the Allow header.
func (a *App) tokenRoute(method string, handler tokenHandler) http.Handler {
	allow := method
	if method == http.MethodGet {
		allow = "GET,HEAD"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrongMethod := r.Method != method && (method != http.MethodGet || r.Method != http.MethodHead)
		if wrongMethod {
			w.Header().Set("Allow", allow)
		}
		token, ok := a.bearerAuth(r)
		if !ok {
			writeText(w, http.StatusUnauthorized, "401: Unauthorized")
			return
		}
		if wrongMethod {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		handler(w, r, token)
	})
}

// writeText answers a plain-text body without a trailing newline.
func writeText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(text))
}

// bearerToken is the token of an "Authorization: Bearer <token>" header; the
// scheme is case-insensitive.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	return strings.TrimSpace(token), true
}

func (a *App) bearerAuth(r *http.Request) (store.TokenAuth, bool) {
	token, ok := bearerToken(r)
	if !ok {
		return store.TokenAuth{}, false
	}
	return a.tokenAuth(r.Context(), token)
}

// tokenAuth authenticates a raw access token and records the use. A store
// failure counts as not authenticated.
func (a *App) tokenAuth(ctx context.Context, token string) (store.TokenAuth, bool) {
	tokenAuth, err := a.core.Store().TokenAuth(ctx, auth.SHA256Hex(token))
	if err == nil {
		return tokenAuth, true
	}
	if !errors.Is(err, store.ErrNotFound) {
		a.logger.Warn("token lookup failed", "error", err)
	}
	return store.TokenAuth{}, false
}

func (a *App) getState(w http.ResponseWriter, r *http.Request, token store.TokenAuth) {
	entityID := r.PathValue("entity_id")
	if !a.core.Allowed(r.Context(), token.Scope, entityID) {
		writeMessage(w, http.StatusNotFound, "Entity not found.")
		return
	}
	state, ok := a.core.Lookup(r.Context(), entityID)
	if !ok {
		writeMessage(w, http.StatusNotFound, "Entity not found.")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// listStates is GET /api/states: every state the token may see, like Home
// Assistant's. The apps list it to offer entities in their widgets.
func (a *App) listStates(w http.ResponseWriter, r *http.Request, token store.TokenAuth) {
	writeJSON(w, http.StatusOK, a.visibleStates(r.Context(), token.Scope, nil))
}

// visibleStates is every state scope may see, synthesised groups and group
// lights included, sorted by id. A non-nil only keeps just those ids.
func (a *App) visibleStates(ctx context.Context, scope store.Scope, only map[string]bool) []core.State {
	all := a.core.AllStates()
	all = append(all, a.core.GroupLights(ctx)...)
	names, err := a.core.Store().GroupNames(ctx)
	if err != nil {
		a.logger.Warn("listing groups failed", "error", err)
	}
	for _, name := range names {
		if state, ok := a.core.GroupState(ctx, name); ok {
			all = append(all, state)
		}
	}
	out := []core.State{}
	for _, state := range all {
		if only != nil && !only[state.EntityID] {
			continue
		}
		if a.core.Allowed(ctx, scope, state.EntityID) {
			out = append(out, state)
		}
	}
	slices.SortStableFunc(out, func(x, y core.State) int { return strings.Compare(x.EntityID, y.EntityID) })
	return out
}

// httpError is a refusal or failed call with the HTTP status and message the
// REST and admin APIs answer with.
type httpError struct {
	status  int
	message string
}

func (e *httpError) Error() string { return e.message }

func writeHTTPError(w http.ResponseWriter, err error) {
	failure := httpErrorOf(err)
	writeMessage(w, failure.status, failure.message)
}

// httpErrorOf reads the status and message of err; an error that is not an
// httpError answers 500.
func httpErrorOf(err error) *httpError {
	var failure *httpError
	if errors.As(err, &failure) {
		return failure
	}
	return &httpError{status: http.StatusInternalServerError, message: err.Error()}
}

func badRequest(err error) error {
	return &httpError{status: http.StatusBadRequest, message: err.Error()}
}

func callFailed(entityID string) error {
	return &httpError{status: http.StatusBadGateway, message: fmt.Sprintf("Call to %s failed.", entityID)}
}

var errServiceNotFound error = &httpError{status: http.StatusNotFound, message: "Service not found."}

func (a *App) lightService(w http.ResponseWriter, r *http.Request, token store.TokenAuth) {
	a.serveService(w, r, token, a.runLight)
}

func (a *App) sceneService(w http.ResponseWriter, r *http.Request, token store.TokenAuth) {
	a.serveService(w, r, token, a.runScene)
}

func (a *App) serveService(w http.ResponseWriter, r *http.Request, token store.TokenAuth,
	run func(ctx context.Context, scope store.Scope, service string, body any) ([]core.State, error)) {
	body, ok := serviceBody(w, r, token)
	if !ok {
		return
	}
	changed, failure := run(r.Context(), token.Scope, r.PathValue("service"), body)
	writeServiceReply(w, changed, failure)
}

// serviceBody refuses read-only tokens, then reads the request body. It
// answers the failure itself and reports whether the caller may go on.
func serviceBody(w http.ResponseWriter, r *http.Request, token store.TokenAuth) (any, bool) {
	if token.ReadOnly {
		writeMessage(w, http.StatusForbidden, "This token is read-only.")
		return nil, false
	}
	raw, ok := readBody(w, r)
	if !ok {
		return nil, false
	}
	body, err := parseBody(raw)
	if err != nil {
		writeMessage(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return body, true
}

// parseBody reads the body as JSON, whatever its Content-Type. An empty body
// counts as null.
func parseBody(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, requestError("Invalid JSON.")
	}
	return body, nil
}

func writeServiceReply(w http.ResponseWriter, changed []core.State, failure error) {
	if failure != nil {
		writeHTTPError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, changed)
}

// runScene runs a scene service and returns the states it changed.
func (a *App) runScene(ctx context.Context, scope store.Scope, service string, body any) ([]core.State, error) {
	if service != "turn_on" {
		return nil, errServiceNotFound
	}
	ids, err := requestedEntities(body)
	if err != nil {
		return nil, badRequest(err)
	}
	changed := []core.State{}
	for _, id := range ids {
		// Out-of-scope entities are ignored, like unknown ones.
		if !strings.HasPrefix(id, "scene.") || !a.core.Allowed(ctx, scope, id) {
			continue
		}
		err := a.core.ActivateScene(ctx, id)
		if errors.Is(err, core.ErrNoIntegration) {
			continue
		}
		if err != nil {
			a.logger.Warn("scene call failed", "entity_id", id, "error", err)
			return nil, callFailed(id)
		}
		if state, ok := a.core.Lookup(ctx, id); ok {
			changed = append(changed, state)
		}
	}
	return changed, nil
}

// lightJob is the lights to act on together with the action to run on them.
type lightJob struct {
	ids    []string
	action core.LightAction
}

// runLight runs a light service and returns the states it changed.
func (a *App) runLight(ctx context.Context, scope store.Scope, service string, body any) ([]core.State, error) {
	requested, err := requestedEntities(body)
	if err != nil {
		return nil, badRequest(err)
	}
	if service == "toggle" {
		transition, err := requestedTransition(body)
		if err != nil {
			return nil, badRequest(err)
		}
		return a.applyLightJobs(ctx, scope, a.toggleJobs(ctx, requested, transition))
	}
	action, err := lightAction(service, body)
	if err != nil {
		return nil, badRequest(err)
	}
	if action == nil {
		return nil, errServiceNotFound
	}
	jobs := []lightJob{{ids: a.core.ExpandEntities(ctx, requested), action: *action}}
	return a.applyLightJobs(ctx, scope, jobs)
}

// toggleJobs decides per requested entity from its own state, so a group
// light that is on turns all of its members off.
func (a *App) toggleJobs(ctx context.Context, requested []string, transition *uint32) []lightJob {
	var jobs []lightJob
	seen := map[string]bool{}
	for _, id := range requested {
		action := core.LightAction{On: true, TransitionMS: transition}
		if state, found := a.core.Lookup(ctx, id); found && state.State == "on" {
			action = core.LightAction{TransitionMS: transition}
		}
		// Overlapping targets toggle once, like Home Assistant's
		// de-duplicated target set.
		var ids []string
		for _, expanded := range a.core.ExpandEntities(ctx, []string{id}) {
			if !seen[expanded] {
				seen[expanded] = true
				ids = append(ids, expanded)
			}
		}
		jobs = append(jobs, lightJob{ids: ids, action: action})
	}
	return jobs
}

func (a *App) applyLightJobs(ctx context.Context, scope store.Scope, jobs []lightJob) ([]core.State, error) {
	changed := []core.State{}
	for _, job := range jobs {
		for _, id := range job.ids {
			// A group may also hold scenes; those are not lights.
			if strings.HasPrefix(id, "scene.") || !a.core.Allowed(ctx, scope, id) {
				continue
			}
			err := a.core.CallLight(ctx, id, job.action)
			// Home Assistant ignores unknown entities in service calls.
			if errors.Is(err, core.ErrNoIntegration) {
				continue
			}
			if err != nil {
				a.logger.Warn("service call failed", "entity_id", id, "error", err)
				return nil, callFailed(id)
			}
			if state, ok := a.core.Lookup(ctx, id); ok {
				changed = append(changed, state)
			}
		}
	}
	return changed, nil
}
