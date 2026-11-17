package server

import (
	"net/http"
	"slices"
	"strings"

	"github.com/henry40408/domus/internal/core"
)

const maxGroupNameBytes = 64

func (a *App) listGroups(w http.ResponseWriter, r *http.Request) {
	ctx, st := r.Context(), a.core.Store()
	names, err := st.GroupNames(ctx)
	if err != nil {
		a.logger.Warn("listing groups failed", "error", err)
	}
	scope := sessionUser(r).Scope
	list := []map[string]any{}
	for _, name := range names {
		if scope != nil && !slices.Contains(scope, name) {
			continue
		}
		list = append(list, a.groupJSON(r, name))
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *App) groupJSON(r *http.Request, name string) map[string]any {
	st := a.core.Store()
	members, err := st.GroupMembers(r.Context(), name)
	if err != nil {
		a.logger.Warn("group members lookup failed", "group", name, "error", err)
	}
	if members == nil {
		members = []string{}
	}
	exposed, err := st.GroupExposed(r.Context(), name)
	if err != nil {
		a.logger.Warn("group exposure lookup failed", "group", name, "error", err)
	}
	return map[string]any{
		"members":         members,
		"expose_light":    exposed,
		"light_entity_id": core.GroupLightID(name),
		"name":            name,
	}
}

// validGroupName allows 1 to 64 bytes of ASCII letters, digits, _ and -.
func validGroupName(name string) bool {
	if name == "" || len(name) > maxGroupNameBytes {
		return false
	}
	for _, c := range []byte(name) {
		isWord := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
		if !isWord {
			return false
		}
	}
	return true
}

func (a *App) putGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Members *[]string `json:"members"`
		// ExposeLight omitted keeps the current setting.
		ExposeLight *bool `json:"expose_light"`
	}
	raw, ok := decodeJSONBody(w, r, &body)
	if !ok || rejectMissing(w, raw, requiredField{"members", body.Members != nil}) {
		return
	}
	name := r.PathValue("name")
	if refusal := groupRefusal(name, *body.Members); refusal != "" {
		writeMessage(w, http.StatusBadRequest, refusal)
		return
	}
	st := a.core.Store()
	if err := st.GroupSet(r.Context(), name, *body.Members); err != nil {
		a.logger.Error("group_set failed", "error", err)
		writeMessage(w, http.StatusInternalServerError, "Could not save the group.")
		return
	}
	if body.ExposeLight != nil {
		if err := st.GroupSetExposed(r.Context(), name, *body.ExposeLight); err != nil {
			a.logger.Warn("setting the group light flag failed", "group", name, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// groupRefusal is why a group may not be saved, or empty when it may.
func groupRefusal(name string, members []string) string {
	if !validGroupName(name) {
		return "Group name: letters, digits, _ and - only (max 64)."
	}
	for _, member := range members {
		if !strings.HasPrefix(member, "light.") && !strings.HasPrefix(member, "scene.") {
			return "Members must be light or scene entities."
		}
	}
	if slices.Contains(members, core.GroupLightID(name)) {
		return "A group cannot contain its own light."
	}
	return ""
}

func (a *App) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := a.core.Store().GroupDelete(r.Context(), r.PathValue("name")); err != nil {
		writeMessage(w, http.StatusNotFound, "No such group.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
