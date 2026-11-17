package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/henry40408/domus/internal/core"
	"github.com/henry40408/domus/internal/store"
)

// missedEvents is the data of a change event that tells the page to reload
// everything because some events were lost.
const missedEvents = "*"

// change is one received broadcast: an entity id, or lagged when the
// subscriber fell behind.
type change struct {
	id     string
	lagged bool
}

// events is the server-sent event stream of the admin page: one "change" event
// per entity the caller may see, carrying only the entity id (the page reloads
// the list). "*" means events were missed, reload everything. The stream ends
// when the session does, and when the server shuts down.
func (a *App) events(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	subscription := a.core.Subscribe()
	defer subscription.Close()
	changes := pumpChanges(ctx, subscription)
	sessionHash := currentSessionHash(r)
	me := sessionUser(r)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher := http.NewResponseController(w)
	if flusher.Flush() != nil {
		return
	}
	recheck := time.NewTicker(a.timing.EventsRecheck)
	defer recheck.Stop()
	keepAlive := time.NewTicker(a.timing.EventsKeepAlive)
	defer keepAlive.Stop()
	for {
		var frame string
		select {
		case <-ctx.Done():
			return
		case <-a.shutdown:
			return
		case <-recheck.C:
			if !a.sessionStillValid(ctx, sessionHash) {
				return
			}
			continue
		case <-keepAlive.C:
			frame = ":\n\n"
		case got := <-changes:
			id, send := a.visibleChange(ctx, me, got)
			if !send {
				continue
			}
			frame = fmt.Sprintf("event: change\ndata: %s\n\n", id)
		}
		if _, err := fmt.Fprint(w, frame); err != nil || flusher.Flush() != nil {
			return
		}
		// Like axum's KeepAlive, only a quiet stream gets a comment.
		keepAlive.Reset(a.timing.EventsKeepAlive)
	}
}

// pumpChanges turns the blocking Recv into a channel the stream can select on
// together with its timers. It stops when ctx ends.
func pumpChanges(ctx context.Context, subscription *core.Subscription) <-chan change {
	changes := make(chan change)
	go func() {
		for {
			id, lagged, err := subscription.Recv(ctx)
			if err != nil {
				return
			}
			select {
			case changes <- change{id: id, lagged: lagged}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return changes
}

// sessionStillValid re-checks the session without counting as activity. A
// store failure ends the stream; the page reconnects and learns the truth.
func (a *App) sessionStillValid(ctx context.Context, hash string) bool {
	valid, err := a.core.Store().SessionValid(ctx, hash)
	if err != nil {
		a.logger.Warn("session recheck failed", "error", err)
		return false
	}
	return valid
}

// visibleChange is the data to send for got, and whether to send anything.
func (a *App) visibleChange(ctx context.Context, me store.User, got change) (string, bool) {
	if got.lagged {
		return missedEvents, true
	}
	return got.id, a.core.Allowed(ctx, me.Scope, got.id)
}
