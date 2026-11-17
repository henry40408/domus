package core

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/henry40408/domus/internal/store"
)

func newLoggedCore(t *testing.T) (*Core, *bytes.Buffer) {
	t.Helper()
	st, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var logs bytes.Buffer
	return New(st, slog.New(slog.NewTextHandler(&logs, nil))), &logs
}

func TestAFailingStoreIsLoggedAndReadsAsNothingThere(t *testing.T) {
	c, logs := newLoggedCore(t)
	noError(t, c.store.Close())

	equal(t, c.groupExists(ctx, "g"), false)
	equal(t, len(c.groupMembers(ctx, "g")), 0)
	equal(t, c.groupExposed(ctx, "g"), false)
	equal(t, len(c.exposedGroupNames(ctx)), 0)

	for _, message := range []string{
		"group lookup failed", "group members lookup failed",
		"group exposure lookup failed", "exposed groups lookup failed",
	} {
		if !strings.Contains(logs.String(), message) {
			t.Errorf("no %q in the log:\n%s", message, logs)
		}
	}
}

func TestAHealthyStoreLogsNothing(t *testing.T) {
	c, logs := newLoggedCore(t)
	noError(t, c.store.GroupSet(ctx, "g", []string{"light.a"}))
	c.groupExists(ctx, "g")
	c.groupMembers(ctx, "g")
	c.groupExposed(ctx, "g")
	c.exposedGroupNames(ctx)
	if logs.Len() != 0 {
		t.Fatalf("unexpected log:\n%s", logs)
	}
}

func TestAFailedGroupLightCallIsLogged(t *testing.T) {
	c, logs := newLoggedCore(t)
	noError(t, c.store.GroupSet(ctx, "g", []string{"light.nobody_owns"}))
	noError(t, c.store.GroupSetExposed(ctx, "g", true))
	if err := c.CallLight(ctx, GroupLightID("g"), LightAction{On: true}); err == nil {
		t.Fatal("a light nobody owns must fail")
	}
	if !strings.Contains(logs.String(), "group light call failed") {
		t.Fatalf("no warning in the log:\n%s", logs)
	}
}

func TestANilLoggerMeansTheDefaultAndAGivenOneIsKept(t *testing.T) {
	st, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	given := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if New(st, nil).logger != slog.Default() {
		t.Error("a nil logger must become slog.Default()")
	}
	if New(st, given).logger != given {
		t.Error("a given logger must be kept")
	}
}
