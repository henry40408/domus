package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/henry40408/domus/internal/core"
)

// colorDetailKeys are the attributes /lights reports only for lights that have
// them.
var colorDetailKeys = []string{"color_mode", "color_temp", "xy_color", "supported_color_modes"}

func (a *App) listLights(w http.ResponseWriter, r *http.Request) {
	ctx, scope := r.Context(), sessionUser(r).Scope
	states := append(a.core.AllStates(), a.core.GroupLights(ctx)...)
	list := []map[string]any{}
	for _, state := range states {
		isDevice := strings.HasPrefix(state.EntityID, "light.") || strings.HasPrefix(state.EntityID, "scene.")
		if !isDevice || !a.core.Allowed(ctx, scope, state.EntityID) {
			continue
		}
		list = append(list, lightJSON(state))
	}
	writeJSON(w, http.StatusOK, list)
}

func lightJSON(state core.State) map[string]any {
	item := map[string]any{
		"entity_id":  state.EntityID,
		"name":       state.Attributes["friendly_name"],
		"state":      state.State,
		"brightness": state.Attributes["brightness"],
	}
	for _, key := range colorDetailKeys {
		if value, ok := state.Attributes[key]; ok {
			item[key] = value
		}
	}
	return item
}

// listRooms lists the rooms and zones whose lights can be controlled
// together, limited to what the user may see.
func (a *App) listRooms(w http.ResponseWriter, r *http.Request) {
	ctx, scope := r.Context(), sessionUser(r).Scope
	list := []core.LightSet{}
	for _, set := range a.core.LightSets() {
		visible := len(set.EntityIDs) > 0
		for _, id := range set.EntityIDs {
			visible = visible && a.core.Allowed(ctx, scope, id)
		}
		if visible {
			list = append(list, set)
		}
	}
	writeJSON(w, http.StatusOK, list)
}

// testEntityIDs reads entity_id (one id) or entity_ids (several) from the
// request body.
func testEntityIDs(body any) []string {
	if id, ok := field(body, "entity_id"); ok {
		if text, isText := id.(string); isText {
			return []string{text}
		}
	}
	list, _ := field(body, "entity_ids")
	items, _ := list.([]any)
	ids := []string{}
	for _, item := range items {
		if text, ok := item.(string); ok {
			ids = append(ids, text)
		}
	}
	return ids
}

// testDevice runs a light or scene action from the admin page, so the setup
// can be tested without a watch. It takes the same color, brightness and
// transition fields as the Home Assistant services, plus "on" (default true)
// and either entity_id or entity_ids (lights to drive together).
func (a *App) testDevice(w http.ResponseWriter, r *http.Request) {
	var body any
	if !decodeJSON(w, r, &body) {
		return
	}
	ids := testEntityIDs(body)
	if len(ids) == 0 {
		writeMessage(w, http.StatusBadRequest, "Missing entity_id.")
		return
	}
	for _, id := range ids {
		if !a.core.Allowed(r.Context(), sessionUser(r).Scope, id) {
			writeMessage(w, http.StatusNotFound, "Unknown entity.")
			return
		}
	}
	refusal, err := a.runTest(r, body, ids)
	if refusal != nil {
		writeMessage(w, refusal.status, refusal.message)
		return
	}
	a.answerTest(w, r, ids[0], err)
}

// runTest drives the entities. The refusal is a bad request; err is the
// outcome of the call itself.
func (a *App) runTest(r *http.Request, body any, ids []string) (refusal *httpError, err error) {
	ctx, first := r.Context(), ids[0]
	if strings.HasPrefix(first, "scene.") {
		return nil, a.core.ActivateScene(ctx, first)
	}
	if !allLights(ids) {
		return &httpError{http.StatusBadRequest, "Only lights and scenes can be tested."}, nil
	}
	service := "turn_on"
	if on, ok := field(body, "on"); ok && on == false {
		service = "turn_off"
	}
	action, paramErr := lightAction(service, body)
	if paramErr != nil {
		return &httpError{http.StatusBadRequest, paramErr.Error()}, nil
	}
	if len(ids) == 1 {
		return nil, a.core.CallLight(ctx, first, *action)
	}
	return nil, a.core.CallLights(ctx, ids, *action)
}

func allLights(ids []string) bool {
	for _, id := range ids {
		if !strings.HasPrefix(id, "light.") {
			return false
		}
	}
	return true
}

func (a *App) answerTest(w http.ResponseWriter, r *http.Request, id string, err error) {
	if errors.Is(err, core.ErrNoIntegration) {
		writeMessage(w, http.StatusNotFound, "Unknown entity.")
		return
	}
	if err != nil {
		a.logger.Warn("test call failed", "entity", id, "error", err)
		writeMessage(w, http.StatusBadGateway, "The bridge rejected the request.")
		return
	}
	var state any
	if found, ok := a.core.Lookup(r.Context(), id); ok {
		state = found.State
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": state})
}
