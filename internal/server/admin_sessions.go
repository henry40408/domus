package server

import (
	"net/http"
	"strconv"

	"github.com/henry40408/domus/internal/auth"
)

// currentSessionHash is the hash of the caller's session token; guarded has
// already proved there is one.
func currentSessionHash(r *http.Request) string {
	token, _ := sessionToken(r)
	return auth.SHA256Hex(token)
}

// listSessions lists the caller's own live sessions; the one making this
// request is flagged current.
func (a *App) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := a.core.Store().ListSessions(r.Context(), sessionUser(r).ID, currentSessionHash(r))
	if err != nil {
		a.logger.Warn("listing sessions failed", "error", err)
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (a *App) revokeSession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	me := sessionUser(r)
	if err := a.core.Store().DeleteSessionByID(r.Context(), me.ID, id); err != nil {
		a.warnUnlessNotFound("deleting a session failed", err)
		writeMessage(w, http.StatusNotFound, "No such session.")
		return
	}
	a.audit.Info("session revoked", "user", me.Username)
	writeOK(w)
}

func (a *App) revokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	me := sessionUser(r)
	ended, err := a.core.Store().DeleteOtherSessions(r.Context(), me.ID, currentSessionHash(r))
	if err != nil {
		a.logger.Warn("deleting other sessions failed", "error", err)
	}
	a.audit.Info("other sessions revoked", "user", me.Username, "ended", ended)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ended": ended})
}

// pathID reads the {id} path segment as axum's Path<i64> does, answering 400
// itself when it is not a number.
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeText(w, http.StatusBadRequest, "Invalid URL: Cannot parse `"+raw+"` to a `i64`")
		return 0, false
	}
	return id, true
}
