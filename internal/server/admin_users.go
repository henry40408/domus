package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/store"
)

func (a *App) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.core.Store().ListUsers(r.Context())
	if err != nil {
		a.logger.Warn("listing users failed", "error", err)
	}
	if users == nil {
		users = []store.User{}
	}
	writeJSON(w, http.StatusOK, users)
}

func (a *App) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username *string `json:"username"`
		Password *string `json:"password"`
		IsAdmin  bool    `json:"is_admin"`
	}
	raw, ok := decodeJSONBody(w, r, &body)
	if !ok || rejectMissing(w, raw, requiredField{"username", body.Username != nil}, requiredField{"password", body.Password != nil}) {
		return
	}
	username := strings.TrimSpace(*body.Username)
	if !auth.ValidUsername(username) {
		writeMessage(w, http.StatusBadRequest, auth.UsernameHint)
		return
	}
	if !auth.ValidPassword(*body.Password, username) {
		writeMessage(w, http.StatusBadRequest, auth.PasswordHint)
		return
	}
	id, err := a.core.Store().CreateUser(r.Context(), username, auth.HashPassword(*body.Password), body.IsAdmin)
	if err != nil {
		// The only constraint a new user can break is the unique name.
		writeMessage(w, http.StatusConflict, "That username is taken.")
		return
	}
	a.audit.Info("user created", "by", sessionUser(r).Username, "user", username, "admin", body.IsAdmin)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "username": username})
}

// userUpdate is the body of PUT /users/{id}. Scope is raw because an absent
// field (leave alone), null (remove the limit) and a list (set it) differ.
type userUpdate struct {
	IsAdmin  *bool           `json:"is_admin"`
	Password *string         `json:"password"`
	Scope    json.RawMessage `json:"scope"`
}

// scopeChange is what a request asks for the user's group limit.
type scopeChange struct {
	set    bool
	groups store.Scope
}

func (a *App) updateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body userUpdate
	if !decodeJSON(w, r, &body) {
		return
	}
	target, found := a.findUser(r, id)
	if !found {
		writeMessage(w, http.StatusNotFound, "No such user.")
		return
	}
	if body.Password != nil && !auth.ValidPassword(*body.Password, target.Username) {
		writeMessage(w, http.StatusBadRequest, auth.PasswordHint)
		return
	}
	change, ok := a.requestedScopeChange(w, r, body, target)
	if !ok {
		return
	}
	me := sessionUser(r)
	// The role goes first: demoting the last admin is refused atomically, and
	// nothing else of this request may have been applied by then.
	if body.IsAdmin != nil && !a.applyRole(w, r, userEdit{me, target}, *body.IsAdmin) {
		return
	}
	if change.set {
		a.applyScope(r, me, target, change.groups)
	}
	if body.Password != nil && !a.resetPassword(w, r, userEdit{me, target}, *body.Password) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) findUser(r *http.Request, id int64) (store.User, bool) {
	users, err := a.core.Store().ListUsers(r.Context())
	if err != nil {
		a.logger.Warn("listing users failed", "error", err)
	}
	for _, user := range users {
		if user.ID == id {
			return user, true
		}
	}
	return store.User{}, false
}

// requestedScopeChange validates the scope field; it answers the request
// itself and returns false when the field is invalid.
func (a *App) requestedScopeChange(w http.ResponseWriter, r *http.Request, body userUpdate, target store.User) (scopeChange, bool) {
	if len(body.Scope) == 0 {
		return scopeChange{}, true
	}
	if string(body.Scope) == "null" {
		return scopeChange{set: true}, true
	}
	var groups []string
	if err := json.Unmarshal(body.Scope, &groups); err != nil {
		writeText(w, http.StatusUnprocessableEntity, "Failed to deserialize the JSON body into the target type: "+err.Error())
		return scopeChange{}, false
	}
	admin := target.IsAdmin
	if body.IsAdmin != nil {
		admin = *body.IsAdmin
	}
	if admin {
		writeMessage(w, http.StatusBadRequest, "Admins are not limited to groups.")
		return scopeChange{}, false
	}
	valid, problem := a.validateScope(r, groups)
	if problem != "" {
		writeMessage(w, http.StatusBadRequest, problem)
		return scopeChange{}, false
	}
	return scopeChange{set: true, groups: valid}, true
}

// userEdit is an admin changing another account.
type userEdit struct {
	me, target store.User
}

// applyRole sets the role and reports false after answering a refusal.
func (a *App) applyRole(w http.ResponseWriter, r *http.Request, edit userEdit, admin bool) bool {
	me, target := edit.me, edit.target
	st := a.core.Store()
	if err := st.SetAdmin(r.Context(), target.ID, admin); err != nil {
		// Deleted by someone else meanwhile, or the last admin.
		if _, lookupErr := st.UserByID(r.Context(), target.ID); lookupErr != nil {
			writeMessage(w, http.StatusNotFound, "No such user.")
			return false
		}
		writeMessage(w, http.StatusBadRequest, "Keep at least one admin.")
		return false
	}
	if admin {
		// Admins are never limited.
		if err := st.SetUserScope(r.Context(), target.ID, nil); err != nil {
			a.logger.Warn("lifting the group limit failed", "error", err)
		}
	}
	a.audit.Info("role changed", "by", me.Username, "user", target.Username, "admin", admin)
	return true
}

func (a *App) applyScope(r *http.Request, me, target store.User, groups store.Scope) {
	if err := a.core.Store().SetUserScope(r.Context(), target.ID, groups); err != nil {
		a.logger.Warn("setting the group limit failed", "error", err)
	}
	a.audit.Info("group limit changed", "by", me.Username, "user", target.Username, "scope", groups)
}

// resetPassword sets another user's password and ends their sessions; it
// reports false after answering a failure.
func (a *App) resetPassword(w http.ResponseWriter, r *http.Request, edit userEdit, password string) bool {
	me, target := edit.me, edit.target
	if err := a.replacePassword(r, target.ID, password); err != nil {
		a.logger.Warn("resetting the password failed", "error", err)
		writeMessage(w, http.StatusInternalServerError, "Could not change the password.")
		return false
	}
	a.audit.Info("password reset", "by", me.Username, "user", target.Username)
	return true
}

func (a *App) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	me := sessionUser(r)
	if id == me.ID {
		writeMessage(w, http.StatusBadRequest, "You cannot delete yourself.")
		return
	}
	result, err := a.core.Store().DeleteUserKeepingAnAdmin(r.Context(), id)
	if err != nil {
		a.logger.Warn("deleting a user failed", "error", err)
	}
	switch result {
	case store.UserDeleted:
		a.audit.Info("user deleted", "by", me.Username, "id", id)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case store.LastAdmin:
		writeMessage(w, http.StatusBadRequest, "Keep at least one admin.")
	case store.UserNotFound:
		writeMessage(w, http.StatusNotFound, "No such user.")
	default:
		writeMessage(w, http.StatusInternalServerError, "Could not delete the user.")
	}
}
