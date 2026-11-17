package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/store"
	"github.com/henry40408/domus/internal/version"
)

const (
	// wsWriteTimeout is how long a peer may take to accept a message before it
	// is dropped instead of stalling the session.
	wsWriteTimeout = 10 * time.Second
	// wsMaxMessage is the largest message accepted, before and after
	// authentication; the apps send tiny ones.
	wsMaxMessage = 64 * 1024
)

// wsSend writes one JSON message to the client. Only the session goroutine
// calls it, so there is a single writer and frames never interleave.
type wsSend func(ctx context.Context, value any) error

// registerWebsocket mounts the Home Assistant WebSocket API, the subset the
// Companion apps need: authentication, the state snapshot and its updates,
// and enough registry commands for the apps to start. Anything else answers
// unknown_command.
func (a *App) registerWebsocket(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/websocket", a.websocket)
}

// inbound is one thing the reader goroutine saw: a message, or the error that
// ended the connection.
type inbound struct {
	isText bool
	text   string
	err    error
}

// upgradeRefusal is the text axum answers (with status 400) for a request that
// is not a WebSocket handshake; empty when the handshake headers look right.
// The library would answer 426, which the Rust build never did.
func upgradeRefusal(r *http.Request) string {
	switch {
	case !headerHasToken(r.Header, "Connection", "upgrade"):
		return "Connection header did not include 'upgrade'"
	case !headerHasToken(r.Header, "Upgrade", "websocket"):
		return "`Upgrade` header did not include 'websocket'"
	case r.Header.Get("Sec-WebSocket-Version") != "13":
		return "`Sec-WebSocket-Version` header did not include '13'"
	case r.Header.Get("Sec-WebSocket-Key") == "":
		return "`Sec-WebSocket-Key` header missing"
	}
	return ""
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func (a *App) websocket(w http.ResponseWriter, r *http.Request) {
	if refusal := upgradeRefusal(r); refusal != "" {
		writeText(w, http.StatusBadRequest, refusal)
		return
	}
	// Count the handler before Accept hijacks the connection: after the hijack
	// http.Server.Shutdown no longer tracks it, so a count taken later could
	// miss a socket that is being upgraded while the server shuts down.
	a.websockets.Add(1)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The Origin check guards endpoints that a browser authenticates by
		// itself (cookies). This one has no ambient credential: the first
		// message carries the access token, so a page of another origin gains
		// nothing, and the native apps send no Origin at all.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	defer a.websockets.Done()
	if err != nil {
		return // Accept has answered the request
	}
	conn.SetReadLimit(wsMaxMessage)
	ctx, cancel := context.WithCancel(r.Context())
	inbox := make(chan inbound)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		readMessages(ctx, conn, inbox)
	}()
	a.runWebsocket(ctx, wsWriter(conn), inbox)
	_ = conn.Close(websocket.StatusNormalClosure, "")
	cancel()
	<-readerDone
}

// readMessages is the only reader of conn. It ends after the first error, or
// when ctx ends (which makes the pending Read fail).
func readMessages(ctx context.Context, conn *websocket.Conn, inbox chan<- inbound) {
	for {
		kind, data, err := conn.Read(ctx)
		message := inbound{isText: kind == websocket.MessageText, text: string(data), err: err}
		select {
		case inbox <- message:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func wsWriter(conn *websocket.Conn) wsSend {
	return func(ctx context.Context, value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
		defer cancel()
		return conn.Write(ctx, websocket.MessageText, data)
	}
}

// runWebsocket drives one connection until it ends, for whatever reason.
func (a *App) runWebsocket(ctx context.Context, send wsSend, inbox <-chan inbound) {
	if send(ctx, map[string]any{"type": "auth_required", "ha_version": version.HAVersion}) != nil {
		return
	}
	token, tokenHash, ok := a.authenticateWebsocket(ctx, send, inbox)
	if !ok {
		return
	}
	// Subscribe before auth_ok, so nothing the client does after it is missed.
	subscription := a.core.Subscribe()
	defer subscription.Close()
	changes := pumpChanges(ctx, subscription)
	if send(ctx, map[string]any{"type": "auth_ok", "ha_version": version.HAVersion}) != nil {
		return
	}
	session := a.newWebsocketSession(ctx, send, token, tokenHash)
	a.serveSession(ctx, session, inbox, changes)
}

func (a *App) serveSession(ctx context.Context, session *wsSession, inbox <-chan inbound, changes <-chan change) {
	ticker := time.NewTicker(a.timing.WebsocketRecheck)
	defer ticker.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return
		// http.Server.Shutdown ignores hijacked connections, so end this one
		// when shutdown starts; WaitForWebsockets then waits for it.
		case <-a.shutdown:
			return
		case message := <-inbox:
			if message.err != nil {
				return
			}
			// Binary frames are not part of the protocol, and pings are
			// answered by the library.
			if message.isText {
				err = session.command(ctx, message.text)
			}
		case got := <-changes:
			err = session.onChange(ctx, got)
		case <-ticker.C:
			err = session.onRecheck(ctx)
		}
		if err != nil {
			return
		}
	}
}

// authenticateWebsocket waits for the auth message; ok is false when the
// client fails to authenticate in time (auth_invalid is sent for a bad one).
func (a *App) authenticateWebsocket(ctx context.Context, send wsSend, inbox <-chan inbound) (token store.TokenAuth, tokenHash string, ok bool) {
	timeout := time.NewTimer(a.timing.WebsocketAuth)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			return token, "", false
		case <-a.shutdown:
			return token, "", false
		case <-timeout.C:
			return token, "", false
		case message := <-inbox:
			if message.err != nil {
				return token, "", false
			}
			if message.isText {
				return a.checkAuthMessage(ctx, send, message.text)
			}
		}
	}
}

func (a *App) checkAuthMessage(ctx context.Context, send wsSend, text string) (token store.TokenAuth, tokenHash string, ok bool) {
	var body struct {
		Type        string `json:"type"`
		AccessToken string `json:"access_token"`
	}
	// What does not parse stays empty and fails like a wrong token.
	_ = json.Unmarshal([]byte(text), &body)
	if body.Type == "auth" {
		if token, ok := a.tokenAuth(ctx, body.AccessToken); ok {
			return token, auth.SHA256Hex(body.AccessToken), true
		}
	}
	_ = send(ctx, map[string]any{"type": "auth_invalid", "message": "Invalid access token or password"})
	return token, "", false
}
