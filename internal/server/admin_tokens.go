package server

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/store"
)

const (
	tokenPrefix    = "domus_"
	maxTokenName   = 64
	maxExpiryDays  = 3650
	secondsPerDay  = 86_400
	tokenRandBytes = 16
)

// tokenOwner limits what a caller sees: admins see and manage every token,
// everyone else only their own. Nil means everyone's.
func tokenOwner(me store.User) *int64 {
	if me.IsAdmin {
		return nil
	}
	return &me.ID
}

func (a *App) listTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := a.core.Store().ListTokens(r.Context(), tokenOwner(sessionUser(r)))
	if err != nil {
		a.logger.Warn("listing tokens failed", "error", err)
	}
	writeJSON(w, http.StatusOK, tokens)
}

// validateScope sorts and dedups a requested group list and checks it is
// non-empty and every group exists. The second result is the refusal text, or
// empty when the list is fine.
func (a *App) validateScope(r *http.Request, groups []string) (store.Scope, string) {
	groups = slices.Clone(groups)
	slices.Sort(groups)
	groups = slices.Compact(groups)
	if len(groups) == 0 {
		return nil, "Pick at least one group, or leave it unrestricted."
	}
	for _, group := range groups {
		exists, err := a.core.Store().GroupExists(r.Context(), group)
		if err != nil {
			a.logger.Warn("group lookup failed", "group", group, "error", err)
		}
		if !exists {
			return nil, fmt.Sprintf("No such group: %s.", group)
		}
	}
	return groups, ""
}

// tokenScope is the groups a new token is limited to; nil means everything
// its owner may use.
func (a *App) tokenScope(r *http.Request, owner store.User, requested *[]string) (store.Scope, error) {
	var groups store.Scope
	if requested != nil {
		valid, problem := a.validateScope(r, *requested)
		if problem != "" {
			return nil, &httpError{http.StatusBadRequest, problem}
		}
		groups = valid
	}
	// A limited user can only hand out what they have: no scope means "all of mine".
	if owner.Scope == nil {
		return groups, nil
	}
	if groups == nil {
		return slices.Clone(owner.Scope), nil
	}
	for _, group := range groups {
		if !slices.Contains(owner.Scope, group) {
			return nil, &httpError{http.StatusForbidden, fmt.Sprintf("Your account may not use the group %s.", group)}
		}
	}
	return groups, nil
}

// tokenExpiry is the Unix time a new token stops working; nil is never.
func (a *App) tokenExpiry(days *int64) (*int64, error) {
	if days == nil {
		return nil, nil
	}
	if *days < 1 || *days > maxExpiryDays {
		return nil, &httpError{http.StatusBadRequest, "Expiry must be 1 to 3650 days."}
	}
	expires := a.now().Unix() + *days*secondsPerDay
	return &expires, nil
}

func (a *App) createToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        *string   `json:"name"`
		Scope       *[]string `json:"scope"`
		ReadOnly    bool      `json:"read_only"`
		ExpiresDays *int64    `json:"expires_days"`
	}
	raw, ok := decodeJSONBody(w, r, &body)
	if !ok || rejectMissing(w, raw, requiredField{"name", body.Name != nil}) {
		return
	}
	name := strings.TrimSpace(*body.Name)
	if name == "" || utf8.RuneCountInString(name) > maxTokenName {
		writeMessage(w, http.StatusBadRequest, "Give the token a name (max 64 characters).")
		return
	}
	me := sessionUser(r)
	scope, refusal := a.tokenScope(r, me, body.Scope)
	if refusal != nil {
		writeHTTPError(w, refusal)
		return
	}
	expires, refusal := a.tokenExpiry(body.ExpiresDays)
	if refusal != nil {
		writeHTTPError(w, refusal)
		return
	}
	// 128 bits of entropy; the prefix makes the token recognisable to people
	// and secret scanners.
	token := tokenPrefix + auth.RandomHex(tokenRandBytes)
	id, err := a.core.Store().CreateToken(r.Context(), store.NewToken{
		UserID: me.ID, Name: name, TokenHash: auth.SHA256Hex(token),
		Scope: scope, ReadOnly: body.ReadOnly, Expires: expires,
	})
	if err != nil {
		a.logger.Warn("creating a token failed", "error", err)
		writeMessage(w, http.StatusInternalServerError, "Could not create the token.")
		return
	}
	a.audit.Info("token created", "user", me.Username, "name", name, "id", id)
	// The plaintext is returned exactly once; only its hash is stored.
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "token": token})
}

func (a *App) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	me := sessionUser(r)
	if err := a.core.Store().RevokeToken(r.Context(), id, tokenOwner(me)); err != nil {
		writeMessage(w, http.StatusNotFound, "No such token.")
		return
	}
	a.audit.Info("token revoked", "user", me.Username, "id", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
