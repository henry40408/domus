package server

import (
	"net/http"
	"strings"
)

const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; base-uri 'none'; " +
	"form-action 'self'; frame-ancestors 'none'"

// securityHeaders adds browser hardening headers to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if !isNoStorePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		nw := &noStoreWriter{ResponseWriter: w}
		next.ServeHTTP(nw, r)
		nw.forceNoStore()
	})
}

// isNoStorePath is true for the paths neither the admin API nor a health
// verdict may be cached on.
func isNoStorePath(path string) bool {
	return strings.HasPrefix(path, "/api/domus") || strings.HasPrefix(path, "/auth/") || path == "/health"
}

// noStoreWriter replaces whatever Cache-Control the handler chose with
// no-store, just before the headers go out.
type noStoreWriter struct {
	http.ResponseWriter
	done bool
}

func (w *noStoreWriter) forceNoStore() {
	if w.done {
		return
	}
	w.done = true
	w.Header().Set("Cache-Control", "no-store")
}

func (w *noStoreWriter) WriteHeader(status int) {
	w.forceNoStore()
	w.ResponseWriter.WriteHeader(status)
}

func (w *noStoreWriter) Write(p []byte) (int, error) {
	w.forceNoStore()
	return w.ResponseWriter.Write(p)
}

// FlushError sends the headers early (SSE does this), so no-store must be
// set first.
func (w *noStoreWriter) FlushError() error {
	w.forceNoStore()
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the real writer for the rest.
func (w *noStoreWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
