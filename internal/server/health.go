package server

import "net/http"

type healthBody struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// health is public and unauthenticated, so it says as little as possible:
// whether the database answers and which build this is. The Hue bridge is
// deliberately not checked; it being offline is normal and must not get the
// container restarted.
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	if err := a.core.Store().Ping(r.Context()); err != nil {
		a.logger.Warn("health check: database does not answer", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, healthBody{Status: "error", Version: a.version})
		return
	}
	writeJSON(w, http.StatusOK, healthBody{Status: "ok", Version: a.version})
}
