package server

import (
	"net/http"
	"strconv"
	"strings"
)

const sessionCookieName = "domus_session"

// sessionToken is the raw session token from the Cookie header. Only the first
// Cookie header counts, like axum's HeaderMap::get.
func sessionToken(r *http.Request) (string, bool) {
	for _, part := range strings.Split(r.Header.Get("Cookie"), ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && name == sessionCookieName {
			return value, true
		}
	}
	return "", false
}

// secureRequest is true behind a TLS-terminating proxy.
func secureRequest(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// sessionCookie is the Set-Cookie value. It is written by hand because the
// attribute order is part of the contract with the Rust server's clients.
func sessionCookie(value string, maxAge int64, secure bool) string {
	cookie := sessionCookieName + "=" + value + "; HttpOnly; SameSite=Strict; Path=/; Max-Age=" +
		strconv.FormatInt(maxAge, 10)
	if secure {
		cookie += "; Secure"
	}
	return cookie
}

// writeSessionCookie answers {"ok":true} and sets the session cookie.
func writeSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int64) {
	w.Header().Set("Set-Cookie", sessionCookie(value, maxAge, secureRequest(r)))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func userAgent(r *http.Request) string {
	return r.Header.Get("User-Agent")
}
