package server

import (
	"net/http"

	"github.com/henry40408/domus/internal/store"
)

// listApps lists Companion app devices: all of them for an admin, otherwise
// only the caller's own.
func (a *App) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := a.core.Store().ListRegistrations(r.Context(), tokenOwner(sessionUser(r)))
	if err != nil {
		a.logger.Warn("listing app devices failed", "error", err)
	}
	if apps == nil {
		apps = []store.Registration{}
	}
	writeJSON(w, http.StatusOK, apps)
}

func (a *App) revokeApp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	me := sessionUser(r)
	if err := a.core.Store().DeleteRegistration(r.Context(), id, tokenOwner(me)); err != nil {
		writeMessage(w, http.StatusNotFound, "No such app device.")
		return
	}
	a.audit.Info("app device removed", "user", me.Username, "id", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
