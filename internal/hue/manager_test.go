package hue

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/henry40408/domus/internal/core"
)

func newManagerEnv(t *testing.T, mock *mockBridge, timing Timing) (*Manager, *core.Core, string) {
	t.Helper()
	checkGoroutines(t)
	base := serve(t, mock.handler())
	c := newCore(t)
	manager := NewManager(c, timing, quietLogger())
	t.Cleanup(manager.Reset)
	return manager, c, base
}

func TestStartSyncedHasTheLightsReadyOnReturn(t *testing.T) {
	mock := newMock()
	mock.set("light", lightJSON("aaaaaaaa-0000", "Desk", true, 40))
	mock.holdOpen = true
	manager, c, base := newManagerEnv(t, mock, Timing{})

	manager.StartSynced(ctx, base, "key")
	if !manager.IsRunning() {
		t.Fatal("the manager must be running")
	}
	if state, _ := c.GetState("light.hue_aaaaaaaa"); state.State != "on" {
		t.Fatalf("got %+v", state)
	}
	// the task starts on the stream: the lights were synced exactly once
	waitFor(t, "event stream", func() bool { return mock.streamCount() == 1 })
	if got := mock.listCount("light"); got != 1 {
		t.Fatalf("got %d light syncs", got)
	}
}

func TestTwoPairingsAtOnceLeaveOnlyTheLastBridgesLights(t *testing.T) {
	checkGoroutines(t)
	slow := newMock()
	slow.set("light", lightJSON("aaaaaaaa-0000", "Slow", true, 40))
	slow.holdOpen = true
	slowBase := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		slow.handler().ServeHTTP(w, r)
	}))
	fast := newMock()
	fast.set("light", lightJSON("bbbbbbbb-0000", "Fast", true, 40))
	fast.holdOpen = true
	fastBase := serve(t, fast.handler())
	c := newCore(t)
	manager := NewManager(c, Timing{}, quietLogger())
	t.Cleanup(manager.Reset)

	first := make(chan struct{})
	go func() {
		defer close(first)
		manager.StartSynced(ctx, slowBase, "key")
	}()
	// Let the first pairing reach its slow sync before the second starts.
	time.Sleep(50 * time.Millisecond)
	manager.StartSynced(ctx, fastBase, "key")
	<-first

	// Without the lock the first pairing finishes last and leaves its light
	// next to the second one's.
	ids := c.EntityIDsWithPrefix(entityPrefix)
	if len(ids) != 1 || ids[0] != "light.hue_bbbbbbbb" {
		t.Fatalf("got %v", ids)
	}
}

func TestManagerRunsBetweenStartAndResetAndOwnsTheHueEntities(t *testing.T) {
	manager, c, base := newManagerEnv(t, newMock(), Timing{})
	if manager.IsRunning() {
		t.Fatal("nothing should run yet")
	}
	manager.Start(base, "key")
	if !manager.IsRunning() {
		t.Fatal("the manager must be running")
	}

	c.SetState("light.hue_a", "on", core.Attributes{})
	c.SetState("scene.hue_a", "unknown", core.Attributes{})
	c.SetState("light.other", "on", core.Attributes{})
	manager.Reset()
	if manager.IsRunning() {
		t.Fatal("the manager must be stopped")
	}
	for _, id := range []string{"light.hue_a", "scene.hue_a"} {
		if _, ok := c.GetState(id); ok {
			t.Fatalf("%s must be removed", id)
		}
	}
	if _, ok := c.GetState("light.other"); !ok {
		t.Fatal("other entities must stay")
	}
}

func TestResetWaitsForTheTaskAndLeavesNoHueState(t *testing.T) {
	mock := newMock()
	mock.set("light", lightJSON("aaaaaaaa-0000", "Desk", true, 40))
	mock.holdOpen = true
	manager, c, base := newManagerEnv(t, mock, Timing{})
	manager.Start(base, "key")
	waitFor(t, "first sync", func() bool { _, ok := c.GetState("light.hue_aaaaaaaa"); return ok })

	manager.Reset()
	// A task still running after Reset would write the light back.
	time.Sleep(100 * time.Millisecond)
	if ids := c.EntityIDsWithPrefix(entityPrefix); len(ids) != 0 {
		t.Fatalf("got %v", ids)
	}
	if err := c.CallLight(ctx, "light.hue_aaaaaaaa", core.LightAction{On: true}); err == nil {
		t.Fatal("the integration must be gone")
	}
}

func TestRestartReplacesTheRunningTask(t *testing.T) {
	mock := newMock()
	mock.set("light", lightJSON("aaaaaaaa-0000", "Desk", true, 40))
	mock.holdOpen = true
	manager, c, base := newManagerEnv(t, mock, Timing{})
	manager.Start(base, "key")
	waitFor(t, "first stream", func() bool { return mock.streamCount() == 1 })
	manager.Start(base, "key")
	waitFor(t, "second stream", func() bool { return mock.streamCount() == 2 })
	if got := len(c.EntityIDsWithPrefix(entityPrefix)); got != 1 {
		t.Fatalf("got %d lights", got)
	}
}

func TestStreamIdleTimeoutEndsTheStream(t *testing.T) {
	mock := newMock()
	mock.holdOpen = true
	env := newEnv(t, mock, Timing{StreamIdle: 50 * time.Millisecond})

	started := time.Now()
	err := env.hue.streamOnce(ctx)
	if !errors.Is(err, errEventStreamIdle) {
		t.Fatalf("got %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("the idle timeout did not fire in time")
	}
}

func TestAResyncEventSyncsOnceWithoutRecursing(t *testing.T) {
	mock := newMock()
	mock.set("light", lightJSON("aaaaaaaa-0000", "Desk", true, 40))
	mock.setEvents(eventJSON("add", J{"id": "x", "type": "light"}))
	mock.holdOpen = true
	env := newEnv(t, mock, Timing{})
	env.sync(t)

	streamCtx, cancel := contextWithCancel()
	done := make(chan error, 1)
	go func() { done <- env.hue.streamOnce(streamCtx) }()
	waitFor(t, "resync", func() bool { return mock.listCount("light") == 2 })
	time.Sleep(100 * time.Millisecond)
	if got := mock.listCount("light"); got != 2 {
		t.Fatalf("got %d light lists, want exactly one resync", got)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, streamCtx.Err()) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("streamOnce did not return after cancel")
	}
}

func TestTheStreamReconnectsAndResyncsEveryTime(t *testing.T) {
	mock := newMock()
	mock.set("light", lightJSON("aaaaaaaa-0000", "Desk", true, 40))
	mock.streamStatus = 500
	manager, c, base := newManagerEnv(t, mock, Timing{ReconnectDelay: 10 * time.Millisecond})

	manager.Start(base, "key")
	waitFor(t, "three connections", func() bool { return mock.streamCount() >= 3 })
	if got := mock.listCount("light"); got < 3 {
		t.Fatalf("got %d syncs for %d streams", got, mock.streamCount())
	}
	if state, _ := c.GetState("light.hue_aaaaaaaa"); state.State != "on" {
		t.Fatalf("got %+v", state)
	}
}

func TestSyncAndLogReportsTheOutcomeUnlessTheContextEnded(t *testing.T) {
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	reachable := newMock()
	reachable.set("light", lightJSON("aaaaaaaa-0000", "Desk", true, 40))

	tests := []struct {
		name      string
		bridgeURL func(t *testing.T) string
		cancelled bool
		want      string
	}{
		{"a sync that works is logged with the light count", func(t *testing.T) string { return serve(t, reachable.handler()) }, false, "level=INFO msg=\"hue: synced lights\" count=1"},
		{"a sync that fails is logged as a warning", func(*testing.T) string { return unreachable.URL }, false, "level=WARN msg=\"hue: sync failed\""},
		{"a sync that fails because the context ended is not logged", func(*testing.T) string { return unreachable.URL }, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			hue := NewIntegration(IntegrationConfig{
				Client: NewClient(tt.bridgeURL(t), "key", Timing{}, quietLogger()),
				Core:   newCore(t),
				Logger: logger,
			})
			syncCtx, cancel := contextWithCancel()
			defer cancel()
			if tt.cancelled {
				cancel()
			}

			hue.syncAndLog(syncCtx)

			if tt.want == "" && logs.Len() != 0 {
				t.Fatalf("got log %q, want none", logs.String())
			}
			if !strings.Contains(logs.String(), tt.want) {
				t.Fatalf("got log %q, want it to contain %q", logs.String(), tt.want)
			}
		})
	}
}

func TestSyncStopsWhenTheContextEnds(t *testing.T) {
	mock := newMock()
	mock.set("light", lightJSON("aaaaaaaa-0000", "Desk", true, 40))
	env := newEnv(t, mock, Timing{})
	cancelled, cancel := contextWithCancel()
	cancel()
	if _, err := env.hue.Sync(cancelled); err == nil {
		t.Fatal("a cancelled sync must fail")
	}
	if _, ok := env.core.GetState("light.hue_aaaaaaaa"); ok {
		t.Fatal("a cancelled sync must not apply anything")
	}
}
