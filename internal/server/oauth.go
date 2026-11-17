package server

import (
	_ "embed"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/store"
)

// accessTTLSecs is the lifetime of an access token; the app refreshes it
// shortly before it ends.
const accessTTLSecs = 30 * 60

const (
	releaseRedirect = "homeassistant://auth-callback"
	debugRedirect   = "homeassistant-dev://auth-callback"
)

// client is an official app and the one custom-scheme redirect it uses.
type client struct {
	clientID    string
	redirectURI string
}

// officialClients are the release builds, then the debug builds. Anything else
// would hand out a code to a stranger.
var officialClients = [...]client{
	{"https://home-assistant.io/iOS", releaseRedirect},
	{"https://home-assistant.io/android", releaseRedirect},
	{"https://home-assistant.io/iOS/dev-auth", debugRedirect},
	{"https://home-assistant.io/android/dev-auth", debugRedirect},
}

//go:embed oauth_login.html
var loginPage []byte

func validClient(clientID, redirectURI string) bool {
	for _, c := range officialClients {
		if c.clientID == clientID && c.redirectURI == redirectURI {
			return true
		}
	}
	return false
}

// registerOAuth mounts the Home Assistant login flow the Companion apps use.
// Every response is no-store through the middleware on /auth/.
func (a *App) registerOAuth(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/authorize", a.authorize)
	mux.HandleFunc("POST /auth/login_flow", a.loginFlow)
	mux.HandleFunc("POST /auth/token", a.token)
	mux.HandleFunc("POST /auth/revoke", a.revoke)
}

func oauthError(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code})
}

func (a *App) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if !validClient(query.Get("client_id"), query.Get("redirect_uri")) {
		writeText(w, http.StatusBadRequest, "Unknown client or redirect URI.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(loginPage)
}

// loginFlow checks the credentials with the same throttle as the admin login
// and answers a one-time code.
func (a *App) loginFlow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username    *string `json:"username"`
		Password    *string `json:"password"`
		ClientID    *string `json:"client_id"`
		RedirectURI *string `json:"redirect_uri"`
	}
	raw, ok := decodeJSONBody(w, r, &body)
	if !ok || rejectMissing(w, raw,
		requiredField{"username", body.Username != nil},
		requiredField{"password", body.Password != nil},
		requiredField{"client_id", body.ClientID != nil},
		requiredField{"redirect_uri", body.RedirectURI != nil}) {
		return
	}
	if !validClient(*body.ClientID, *body.RedirectURI) {
		oauthError(w, "invalid_request")
		return
	}
	name := strings.TrimSpace(*body.Username)
	key := auth.ThrottleKey("login", name)
	if wait := a.guard.Check(key, a.now().Unix()); wait > 0 {
		a.audit.Warn("app login refused: locked", "user", name, "wait", wait)
		writeTooMany(w, wait)
		return
	}
	user, ok := a.verifyLogin(r, name, *body.Password)
	if !ok {
		a.guard.Fail(key, a.now().Unix())
		a.audit.Warn("app login failed", "user", name)
		writeMessage(w, http.StatusUnauthorized, "Wrong username or password.")
		return
	}
	a.guard.Succeed(key)
	a.audit.Info("app login ok", "user", user.Username, "client", *body.ClientID)
	writeJSON(w, http.StatusOK, map[string]string{"code": a.codes.issue(user.ID, *body.ClientID)})
}

// token is the OAuth token endpoint: the code and refresh grants, and the
// revocation the iOS app sends here.
func (a *App) token(w http.ResponseWriter, r *http.Request) {
	form, ok := decodeForm(w, r)
	if !ok {
		return
	}
	if form.Get("action") == "revoke" {
		a.revokeValue(r, form)
		w.WriteHeader(http.StatusOK)
		return
	}
	if !form.Has("grant_type") {
		oauthError(w, "invalid_request")
		return
	}
	switch form.Get("grant_type") {
	case "authorization_code":
		a.exchangeCode(w, r, form)
	case "refresh_token":
		a.refreshAccess(w, r, form)
	default:
		oauthError(w, "unsupported_grant_type")
	}
}

func (a *App) exchangeCode(w http.ResponseWriter, r *http.Request, form url.Values) {
	if !form.Has("code") {
		oauthError(w, "invalid_request")
		return
	}
	clientID := form.Get("client_id")
	userID, ok := a.codes.redeem(form.Get("code"), clientID)
	if !ok {
		oauthError(w, "invalid_grant")
		return
	}
	ctx, st := r.Context(), a.core.Store()
	refresh := tokenPrefix + "r_" + auth.RandomHex(32)
	refreshID, err := st.RefreshCreate(ctx, userID, clientID, auth.SHA256Hex(refresh))
	if err != nil {
		a.logger.Warn("creating a refresh token failed", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	access, err := a.mintAccess(r, userID, clientID, refreshID)
	if err != nil {
		a.logger.Warn("creating an access token failed", "error", err)
		_ = st.RefreshRevoke(ctx, auth.SHA256Hex(refresh))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"refresh_token": refresh,
		"expires_in":    accessTTLSecs,
	})
}

// refreshAccess mints a new access token for a refresh token. The refresh
// token itself is not rotated.
func (a *App) refreshAccess(w http.ResponseWriter, r *http.Request, form url.Values) {
	if !form.Has("refresh_token") {
		oauthError(w, "invalid_request")
		return
	}
	clientID := form.Get("client_id")
	refreshID, userID, err := a.core.Store().RefreshLookup(r.Context(), auth.SHA256Hex(form.Get("refresh_token")), clientID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.logger.Warn("refresh token lookup failed", "error", err)
		}
		oauthError(w, "invalid_grant")
		return
	}
	access, err := a.mintAccess(r, userID, clientID, refreshID)
	if err != nil {
		a.logger.Warn("creating an access token failed", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   accessTTLSecs,
	})
}

// mintAccess stores a new access token of the refresh token and returns it.
// The store retires the older ones.
func (a *App) mintAccess(r *http.Request, userID int64, clientID string, refreshID int64) (string, error) {
	token := tokenPrefix + auth.RandomHex(tokenRandBytes)
	name := "Home Assistant app (Android)"
	if strings.Contains(clientID, "/iOS") {
		name = "Home Assistant app (iOS)"
	}
	err := a.core.Store().CreateAppToken(r.Context(), store.NewAppToken{
		UserID:    userID,
		Name:      name,
		TokenHash: auth.SHA256Hex(token),
		Expires:   a.now().Unix() + accessTTLSecs,
		RefreshID: refreshID,
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// revoke is POST /auth/revoke (form field token).
func (a *App) revoke(w http.ResponseWriter, r *http.Request) {
	form, ok := decodeForm(w, r)
	if !ok {
		return
	}
	a.revokeValue(r, form)
	w.WriteHeader(http.StatusOK)
}

// revokeValue ends an access or refresh token. Like Home Assistant it makes
// no difference whether the token existed, so the caller answers 200 either
// way and the endpoint cannot be used to probe for valid tokens.
func (a *App) revokeValue(r *http.Request, form url.Values) {
	if !form.Has("token") {
		return
	}
	hash := auth.SHA256Hex(form.Get("token"))
	st := a.core.Store()
	err := st.RefreshRevoke(r.Context(), hash)
	if err == nil {
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		a.logger.Warn("revoking a refresh token failed", "error", err)
	}
	if err := st.AccessRevoke(r.Context(), hash); err != nil && !errors.Is(err, store.ErrNotFound) {
		a.logger.Warn("revoking an access token failed", "error", err)
	}
}
