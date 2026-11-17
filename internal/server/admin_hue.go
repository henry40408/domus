package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/henry40408/domus/internal/hue"
	"github.com/henry40408/domus/internal/store"
)

func (a *App) hueStatus(w http.ResponseWriter, r *http.Request) {
	bridge, err := a.core.Store().HueGet(r.Context())
	paired := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.logger.Warn("reading the paired bridge failed", "error", err)
	}
	var ip any
	if paired {
		ip = bridge.IP
	}
	writeJSON(w, http.StatusOK, map[string]any{"paired": paired, "ip": ip, "running": a.hue.IsRunning()})
}

func (a *App) pairHue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IP *string `json:"ip"`
	}
	raw, ok := decodeJSONBody(w, r, &body)
	if !ok || rejectMissing(w, raw, requiredField{"ip", body.IP != nil}) {
		return
	}
	ip := strings.TrimSpace(*body.IP)
	if ip == "" || strings.IndexFunc(ip, unicode.IsSpace) >= 0 {
		writeMessage(w, http.StatusBadRequest, "Enter the bridge IP address.")
		return
	}
	base := hue.BridgeBase(ip)
	key, err := hue.Pair(r.Context(), base)
	if errors.Is(err, hue.ErrLinkButtonNotPressed) {
		writeMessage(w, http.StatusConflict, "Press the link button on the bridge, then try again.")
		return
	}
	if err != nil {
		a.logger.Warn("hue pairing failed", "error", err)
		writeMessage(w, http.StatusBadGateway, fmt.Sprintf("Could not pair with the bridge: %v", err))
		return
	}
	if err := a.core.Store().HueSet(r.Context(), store.HueBridge{IP: ip, Key: key}); err != nil {
		a.logger.Warn("storing the paired bridge failed", "error", err)
	}
	// The pairing is stored; the first sync finishes even if the browser leaves.
	a.hue.StartSynced(context.WithoutCancel(r.Context()), base, key)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
