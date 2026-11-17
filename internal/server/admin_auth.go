package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/store"
)

// registerAdmin mounts the admin API's authentication and session routes.
func (a *App) registerAdmin(mux *http.ServeMux) {
	routes := newAdminRoutes(a, mux)
	routes.route("GET /status", open, a.status)
	routes.route("POST /setup", open, a.setup)
	routes.route("POST /login", open, a.login)
	routes.route("POST /session/from-token", open, a.sessionFromToken)

	routes.route("POST /logout", needSession, a.logout)
	routes.route("POST /password", needSession, a.passwordChange)
	routes.route("POST /session/reauth", needSession, a.reauth)
	routes.route("GET /sessions", needSession, a.listSessions)
	routes.route("POST /sessions/revoke-others", needSession, a.revokeOtherSessions)
	routes.route("DELETE /sessions/{id}", needSession, a.revokeSession)
	routes.route("GET /events", needSession, a.events)
	a.registerManagement(routes)
	routes.mount()
}

func (a *App) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var user any
	if current, ok := a.currentUser(r); ok {
		user = current
	}
	users, err := a.core.Store().UserCount(ctx)
	if err != nil {
		a.logger.Warn("counting users failed", "error", err)
	}
	_, hueErr := a.core.Store().HueGet(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"setup_done":          users > 0,
		"setup_code_required": a.setupCodeRequired(),
		"logged_in":           user != nil,
		"user":                user,
		"hue_paired":          hueErr == nil,
		"version":             a.version,
	})
}

func (a *App) setupCodeRequired() bool {
	a.setupMu.Lock()
	defer a.setupMu.Unlock()
	return a.setupCode != ""
}

// credentials is a login or setup body. Fields are pointers so a missing one
// can be told from an empty one.
type credentials struct {
	Username  *string `json:"username"`
	Password  *string `json:"password"`
	SetupCode string  `json:"setup_code"`
}

func (c credentials) missing() []requiredField {
	return []requiredField{{"username", c.Username != nil}, {"password", c.Password != nil}}
}

func writeTooMany(w http.ResponseWriter, waitSecs int64) {
	w.Header().Set("Retry-After", strconv.FormatInt(waitSecs, 10))
	writeMessage(w, http.StatusTooManyRequests,
		fmt.Sprintf("Too many failed attempts. Try again in %d seconds.", waitSecs))
}

func (a *App) setup(w http.ResponseWriter, r *http.Request) {
	var body credentials
	raw, ok := decodeJSONBody(w, r, &body)
	if !ok || rejectMissing(w, raw, body.missing()...) {
		return
	}
	if !a.checkSetupCode(w, body.SetupCode) {
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
	id, err := a.core.Store().CreateFirstAdmin(r.Context(), username, auth.HashPassword(*body.Password))
	if err != nil {
		if !errors.Is(err, store.ErrAlreadySetUp) {
			a.logger.Warn("creating the first admin failed", "error", err)
		}
		writeMessage(w, http.StatusConflict, "Already set up.")
		return
	}
	a.spendSetupCode()
	a.audit.Info("first admin created", "user", username)
	a.startSession(w, r, id)
}

// checkSetupCode answers the request itself when the code is wrong or the
// caller is locked out, and reports whether setup may go on.
func (a *App) checkSetupCode(w http.ResponseWriter, supplied string) bool {
	a.setupMu.Lock()
	expected := a.setupCode
	a.setupMu.Unlock()
	if expected == "" {
		return true
	}
	key := auth.ThrottleKey("setup", "")
	if wait := a.guard.Check(key, a.now().Unix()); wait > 0 {
		writeTooMany(w, wait)
		return false
	}
	if !auth.ConstantTimeEqual([]byte(plainSetupCode(supplied)), []byte(plainSetupCode(expected))) {
		a.guard.Fail(key, a.now().Unix())
		a.audit.Warn("setup refused: wrong setup code")
		writeMessage(w, http.StatusForbidden, "Wrong setup code. Find it in the domus log.")
		return false
	}
	return true
}

// plainSetupCode ignores dashes, which are only there for readability
// (A1B2C-D3E4F), and case.
func plainSetupCode(code string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
}

func (a *App) spendSetupCode() {
	a.setupMu.Lock()
	a.setupCode = ""
	a.setupMu.Unlock()
	a.guard.Succeed(auth.ThrottleKey("setup", ""))
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	var body credentials
	raw, ok := decodeJSONBody(w, r, &body)
	if !ok || rejectMissing(w, raw, body.missing()...) {
		return
	}
	name := strings.TrimSpace(*body.Username)
	key := auth.ThrottleKey("login", name)
	if wait := a.guard.Check(key, a.now().Unix()); wait > 0 {
		a.audit.Warn("login refused: locked", "user", name, "wait", wait)
		writeTooMany(w, wait)
		return
	}
	user, ok := a.verifyLogin(r, name, *body.Password)
	if !ok {
		a.guard.Fail(key, a.now().Unix())
		a.audit.Warn("login failed", "user", name)
		writeMessage(w, http.StatusUnauthorized, "Wrong username or password.")
		return
	}
	a.guard.Succeed(key)
	a.audit.Info("login ok", "user", user.Username)
	a.startSession(w, r, user.ID)
}

// verifyLogin checks the password. An unknown user still costs one hash, so
// the response time does not tell which names exist.
func (a *App) verifyLogin(r *http.Request, name, password string) (store.User, bool) {
	user, hash, err := a.core.Store().UserLogin(r.Context(), name)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.logger.Warn("login lookup failed", "error", err)
		}
		auth.VerifyDummy(password)
		return store.User{}, false
	}
	if !auth.VerifyPassword(password, hash) {
		return store.User{}, false
	}
	return user, true
}

// startSession opens a password-login session and sets its cookie.
func (a *App) startSession(w http.ResponseWriter, r *http.Request, userID int64) {
	token := auth.RandomHex(32)
	err := a.core.Store().CreateSession(r.Context(), auth.SHA256Hex(token), userID, userAgent(r))
	if err != nil {
		a.logger.Warn("creating a session failed", "error", err)
		writeMessage(w, http.StatusInternalServerError, "Could not start a session.")
		return
	}
	writeSessionCookie(w, r, token, store.SessionTTLSecs)
}

// sessionFromToken trades the access token of an app login (a Bearer header,
// never a body or URL) for an admin-page session, so the Companion WebView
// does not ask for the password again. Only tokens minted from a refresh
// token qualify: API tokens may be read-only or narrowly scoped, and a
// session is neither. The session ends together with the app login.
func (a *App) sessionFromToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, refreshID, ok := a.appLoginOf(r)
	if !ok {
		writeMessage(w, http.StatusUnauthorized, "Invalid token.")
		return
	}
	st := a.core.Store()
	user, err := st.UserByID(ctx, userID)
	if err != nil {
		writeMessage(w, http.StatusUnauthorized, "Invalid token.")
		return
	}
	if live, err := st.RefreshExists(ctx, refreshID); err != nil || !live {
		writeMessage(w, http.StatusUnauthorized, "Invalid token.")
		return
	}
	session := auth.RandomHex(32)
	agent := "HA app · " + userAgent(r)
	if err := st.CreateAppSession(ctx, auth.SHA256Hex(session), userID, refreshID, agent); err != nil {
		a.logger.Warn("creating an app session failed", "error", err)
		writeMessage(w, http.StatusInternalServerError, "Could not start a session.")
		return
	}
	a.audit.Info("app session started", "user", user.Username, "refresh_id", refreshID)
	writeSessionCookie(w, r, session, store.AppSessionTTLSecs)
}

// appLoginOf is the user and refresh token behind the request's Bearer token,
// when it is an app login's access token.
func (a *App) appLoginOf(r *http.Request) (userID, refreshID int64, ok bool) {
	token, ok := bearerToken(r)
	if !ok {
		return 0, 0, false
	}
	view, err := a.core.Store().TokenView(r.Context(), auth.SHA256Hex(token))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.logger.Warn("token lookup failed", "error", err)
		}
		return 0, 0, false
	}
	if view.RefreshID == nil || view.UserID == nil {
		a.audit.Warn("app session refused: not an app login token")
		return 0, 0, false
	}
	return *view.UserID, *view.RefreshID, true
}

// reauth re-enters the password to unlock sensitive actions in the current
// session for a few minutes.
func (a *App) reauth(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password *string `json:"password"`
	}
	raw, ok := decodeJSONBody(w, r, &body)
	if !ok || rejectMissing(w, raw, requiredField{"password", body.Password != nil}) {
		return
	}
	me := sessionUser(r)
	if !a.confirmPassword(w, r, me, *body.Password, "Password is wrong.", "reauth refused: wrong password") {
		return
	}
	if token, ok := sessionToken(r); ok {
		if err := a.core.Store().SessionElevate(r.Context(), auth.SHA256Hex(token)); err != nil {
			a.logger.Warn("elevating the session failed", "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// confirmPassword is the throttled check of the caller's own password that
// password change and reauth share. A success clears the failure count. It
// answers the request itself when it refuses.
func (a *App) confirmPassword(w http.ResponseWriter, r *http.Request, me store.User, password, refusal, auditText string) bool {
	key := auth.ThrottleKey("password", me.Username)
	if wait := a.guard.Check(key, a.now().Unix()); wait > 0 {
		writeTooMany(w, wait)
		return false
	}
	hash, err := a.core.Store().UserHash(r.Context(), me.ID)
	if err != nil || !auth.VerifyPassword(password, hash) {
		a.guard.Fail(key, a.now().Unix())
		a.audit.Warn(auditText, "user", me.Username)
		writeMessage(w, http.StatusUnauthorized, refusal)
		return false
	}
	a.guard.Succeed(key)
	return true
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if token, ok := sessionToken(r); ok {
		if err := a.core.Store().DeleteSession(r.Context(), auth.SHA256Hex(token)); err != nil {
			a.logger.Warn("deleting the session failed", "error", err)
		}
	}
	writeSessionCookie(w, r, "", 0)
}

// passwordChange changes the caller's own password; every session and app
// login ends and a fresh session starts.
func (a *App) passwordChange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Current *string `json:"current"`
		New     *string `json:"new"`
	}
	raw, ok := decodeJSONBody(w, r, &body)
	if !ok || rejectMissing(w, raw, requiredField{"current", body.Current != nil}, requiredField{"new", body.New != nil}) {
		return
	}
	me := sessionUser(r)
	if !a.confirmPassword(w, r, me, *body.Current, "Current password is wrong.", "password change refused: wrong current password") {
		return
	}
	if !auth.ValidPassword(*body.New, me.Username) {
		writeMessage(w, http.StatusBadRequest, auth.PasswordHint)
		return
	}
	if err := a.replacePassword(r, me.ID, *body.New); err != nil {
		a.logger.Warn("changing the password failed", "error", err)
		writeMessage(w, http.StatusInternalServerError, "Could not change the password.")
		return
	}
	a.audit.Info("password changed", "user", me.Username)
	a.startSession(w, r, me.ID)
}

// replacePassword stores the new password and ends every session and app
// login of the user.
func (a *App) replacePassword(r *http.Request, userID int64, password string) error {
	ctx, st := r.Context(), a.core.Store()
	if err := st.SetPassword(ctx, userID, auth.HashPassword(password)); err != nil {
		return err
	}
	if err := st.DeleteUserSessions(ctx, userID); err != nil {
		return err
	}
	return st.DeleteUserRefreshTokens(ctx, userID)
}
