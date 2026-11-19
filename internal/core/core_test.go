package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/henry40408/domus/internal/store"
)

var ctx = context.Background()

func newCore(t *testing.T) *Core {
	t.Helper()
	st, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func noError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func equal(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func mustState(t *testing.T, c *Core, id string) State {
	t.Helper()
	state, ok := c.GetState(id)
	if !ok {
		t.Fatalf("no state for %s", id)
	}
	return state
}

func mustLookup(t *testing.T, c *Core, id string) State {
	t.Helper()
	state, ok := c.Lookup(ctx, id)
	if !ok {
		t.Fatalf("no lookup result for %s", id)
	}
	return state
}

func groupSet(t *testing.T, c *Core, name string, members ...string) {
	t.Helper()
	noError(t, c.store.GroupSet(ctx, name, members))
}

func setExposed(t *testing.T, c *Core, name string, exposed bool) {
	t.Helper()
	noError(t, c.store.GroupSetExposed(ctx, name, exposed))
}

func ptr[T any](v T) *T { return &v }

// receivedIDs reads what is queued right now, without waiting.
func receivedIDs(sub *Subscription) []string {
	var ids []string
	for {
		select {
		case id := <-sub.ids:
			ids = append(ids, id)
		default:
			return ids
		}
	}
}

func TestStateChangesAreBroadcastOnce(t *testing.T) {
	c := newCore(t)
	sub := c.Subscribe()
	defer sub.Close()
	c.SetState("light.a", "on", nil)
	c.SetState("light.a", "on", nil) // identical: no event
	c.SetState("light.a", "off", nil)
	c.RemoveState("light.a")
	c.RemoveState("light.a") // already gone: no event
	equal(t, receivedIDs(sub), []string{"light.a", "light.a", "light.a"})
}

func TestLastChangedOnlyMovesOnStateChange(t *testing.T) {
	c := newCore(t)
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c.now = func() time.Time { return clock }
	c.SetState("light.a", "on", nil)
	first := mustState(t, c, "light.a")

	clock = clock.Add(2 * time.Millisecond)
	c.SetState("light.a", "on", nil)
	same := mustState(t, c, "light.a")
	equal(t, same.LastChanged, first.LastChanged)
	if same.LastUpdated == first.LastUpdated {
		t.Fatal("last_updated must move on every set")
	}

	clock = clock.Add(2 * time.Millisecond)
	c.SetState("light.a", "off", nil)
	if mustState(t, c, "light.a").LastChanged == first.LastChanged {
		t.Fatal("last_changed must move when the state changes")
	}
}

func TestGroupStateFollowsMembers(t *testing.T) {
	c := newCore(t)
	c.SetState("light.a", "off", nil)
	c.SetState("light.b", "off", nil)
	if _, ok := c.GroupState(ctx, "garmin"); ok {
		t.Fatal("a missing group must not have a state")
	}
	groupSet(t, c, "garmin", "light.a", "light.b")
	g := mustLookup(t, c, "group.garmin")
	equal(t, g.EntityID, "group.garmin")
	equal(t, g.State, "off")
	equal(t, g.Attributes["entity_id"], []string{"light.a", "light.b"})
	c.SetState("light.b", "on", nil)
	equal(t, mustLookup(t, c, "group.garmin").State, "on")

	// exposing lists the group's own light first, but never as a stored member
	setExposed(t, c, "garmin", true)
	g = mustLookup(t, c, "group.garmin")
	equal(t, g.Attributes["entity_id"], []string{"light.domus_group_garmin", "light.a", "light.b"})
	members, err := c.store.GroupMembers(ctx, "garmin")
	noError(t, err)
	equal(t, members, []string{"light.a", "light.b"})
	equal(t, c.ExpandEntities(ctx, []string{"group.garmin"}), []string{"light.a", "light.b"})
	if _, ok := c.Lookup(ctx, "group.nope"); ok {
		t.Fatal("unknown group")
	}
}

func TestEmptyGroupListsNoMembersAsEmptyArray(t *testing.T) {
	c := newCore(t)
	groupSet(t, c, "empty")
	g := mustLookup(t, c, "group.empty")
	equal(t, g.Attributes["entity_id"], []string{})
}

func TestExpandGroupsAndDedupe(t *testing.T) {
	c := newCore(t)
	groupSet(t, c, "g", "light.a", "light.b")
	out := c.ExpandEntities(ctx, []string{"group.g", "light.b", "light.c"})
	equal(t, out, []string{"light.a", "light.b", "light.c"})
}

// fakeLights owns lights whose id starts with prefix; ids ending in "bad" fail.
type fakeLights struct {
	BaseIntegration
	prefix string
}

func (f fakeLights) Owns(id string) bool { return strings.HasPrefix(id, f.prefix) }

func (f fakeLights) CallLight(_ context.Context, id string, _ LightAction) error {
	if strings.HasSuffix(id, "bad") {
		return errors.New("boom")
	}
	return nil
}

func TestCallLightAppliesOptimisticState(t *testing.T) {
	c := newCore(t)
	c.SetIntegration("fake", fakeLights{prefix: "light.fake"})
	c.SetState("light.fake1", "off", nil)
	noError(t, c.CallLight(ctx, "light.fake1", LightAction{On: true, Brightness: ptr(uint8(128))}))
	s := mustState(t, c, "light.fake1")
	equal(t, s.State, "on")
	equal(t, s.Attributes["brightness"], 128)
	noError(t, c.CallLight(ctx, "light.fake1", LightAction{}))
	s = mustState(t, c, "light.fake1")
	equal(t, s.State, "off")
	if _, ok := s.Attributes["brightness"]; ok {
		t.Fatal("turning off must drop the brightness")
	}
}

func TestCallLightErrors(t *testing.T) {
	c := newCore(t)
	c.SetIntegration("fake", fakeLights{prefix: "light.fake"})
	if err := c.CallLight(ctx, "light.other", LightAction{}); !errors.Is(err, ErrNoIntegration) {
		t.Fatalf("got %v, want ErrNoIntegration", err)
	}
	err := c.CallLight(ctx, "light.fake_bad", LightAction{})
	if err == nil || err.Error() != "boom" || errors.Is(err, ErrNoIntegration) {
		t.Fatalf("got %v, want the integration's error", err)
	}
}

func groupCore(t *testing.T, members ...string) *Core {
	t.Helper()
	c := newCore(t)
	c.SetIntegration("flaky", fakeLights{prefix: "light.flaky"})
	for _, member := range members {
		c.SetState(member, "off", nil)
	}
	groupSet(t, c, "room", members...)
	return c
}

func TestGroupLightExistsOnlyWhenExposed(t *testing.T) {
	c := groupCore(t, "light.flaky_a", "light.flaky_b")
	id := GroupLightID("room")
	if _, ok := c.Lookup(ctx, id); ok {
		t.Fatal("an unexposed group has no light")
	}
	equal(t, len(c.GroupLights(ctx)), 0)
	setExposed(t, c, "room", true)
	s := mustLookup(t, c, id)
	equal(t, [2]string{s.State, s.EntityID}, [2]string{"off", id})
	equal(t, len(s.Attributes), 1) // only friendly_name
	c.SetState("light.flaky_b", "on", nil)
	equal(t, mustLookup(t, c, id).State, "on")
	equal(t, len(c.GroupLights(ctx)), 1)
	noError(t, c.store.GroupDelete(ctx, "room"))
	if _, ok := c.Lookup(ctx, id); ok {
		t.Fatal("a deleted group has no light")
	}
}

func TestGroupLightFansOutToMembers(t *testing.T) {
	c := groupCore(t, "light.flaky_a", "light.flaky_b")
	setExposed(t, c, "room", true)
	id := GroupLightID("room")
	noError(t, c.CallLight(ctx, id, LightAction{On: true}))
	equal(t, mustState(t, c, "light.flaky_a").State, "on")
	equal(t, mustState(t, c, "light.flaky_b").State, "on")
	noError(t, c.CallLight(ctx, id, LightAction{}))
	equal(t, mustLookup(t, c, id).State, "off")
}

func TestGroupLightPartialAndTotalFailure(t *testing.T) {
	c := groupCore(t, "light.flaky_a", "light.flaky_bad")
	setExposed(t, c, "room", true)
	noError(t, c.CallLight(ctx, GroupLightID("room"), LightAction{On: true}))
	equal(t, mustState(t, c, "light.flaky_a").State, "on")
	equal(t, mustState(t, c, "light.flaky_bad").State, "off")

	c = groupCore(t, "light.flaky_bad")
	setExposed(t, c, "room", true)
	err := c.CallLight(ctx, GroupLightID("room"), LightAction{})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("got %v, want boom", err)
	}
}

func TestGroupLightWithoutExposureOrMembers(t *testing.T) {
	c := groupCore(t, "light.flaky_a")
	if err := c.CallLight(ctx, GroupLightID("room"), LightAction{}); !errors.Is(err, ErrNoIntegration) {
		t.Fatalf("got %v, want ErrNoIntegration", err)
	}
	groupSet(t, c, "empty")
	setExposed(t, c, "empty", true)
	noError(t, c.CallLight(ctx, GroupLightID("empty"), LightAction{}))
}

// roomy controls its light.set_* lights as one room; light.set_bad makes the
// room request fail.
type roomy struct{ BaseIntegration }

func (roomy) Owns(id string) bool { return strings.HasPrefix(id, "light.set_") }

func (roomy) CallLight(context.Context, string, LightAction) error {
	return errors.New("must not go light by light")
}

func (roomy) LightSets() []LightSet {
	return []LightSet{{Name: "Room", Kind: "room", EntityIDs: []string{"light.set_a"}}}
}

func (roomy) CallLightSet(_ context.Context, ids []string, _ LightAction) (bool, error) {
	if slices.Contains(ids, "light.set_bad") {
		return true, errors.New("room down")
	}
	return true, nil
}

func TestLightSetGoesThroughOneRequestAndAppliesOptimistically(t *testing.T) {
	c := newCore(t)
	c.SetIntegration("roomy", roomy{})
	equal(t, len(c.LightSets()), 1)
	for _, id := range []string{"light.set_a", "light.set_b"} {
		c.SetState(id, "off", nil)
	}
	groupSet(t, c, "g", "light.set_a", "light.set_b")
	setExposed(t, c, "g", true)
	id := GroupLightID("g")
	noError(t, c.CallLight(ctx, id, LightAction{On: true, ColorTemp: ptr(uint16(300))}))
	s := mustState(t, c, "light.set_b")
	equal(t, s.State, "on")
	equal(t, s.Attributes["color_temp"], 300)
	equal(t, s.Attributes["color_mode"], "color_temp")

	// xy replaces a previous white temperature
	noError(t, c.CallLight(ctx, id, LightAction{On: true, XY: &XY{X: 0.5, Y: 0.4}}))
	s = mustState(t, c, "light.set_a")
	equal(t, s.Attributes["color_mode"], "xy")
	if _, ok := s.Attributes["color_temp"]; ok {
		t.Fatal("xy must drop color_temp")
	}

	// an unreachable light is left alone
	c.SetState("light.set_a", "unavailable", nil)
	noError(t, c.CallLight(ctx, id, LightAction{}))
	equal(t, mustState(t, c, "light.set_a").State, "unavailable")

	// a failing room request is the call's error
	c.SetState("light.set_bad", "off", nil)
	groupSet(t, c, "g", "light.set_a", "light.set_bad")
	err := c.CallLight(ctx, id, LightAction{})
	if err == nil || err.Error() != "room down" {
		t.Fatalf("got %v, want room down", err)
	}
}

func TestLightsFromDifferentIntegrationsAreNotOneSet(t *testing.T) {
	c := newCore(t)
	c.SetIntegration("roomy", roomy{})
	c.SetIntegration("flaky", fakeLights{prefix: "light.flaky"})
	for name, ids := range map[string][]string{
		"no lights": nil,
		"mixed":     {"light.set_a", "light.flaky_a"},
		"unowned":   {"light.other"},
	} {
		handled, err := c.callLightSet(ctx, ids, LightAction{On: true})
		if handled || err != nil {
			t.Fatalf("%s: handled=%v err=%v", name, handled, err)
		}
	}
}

func TestGroupLightsThatIncludeEachOtherDoNotRecurse(t *testing.T) {
	c := groupCore(t, "light.flaky_a")
	a, b := GroupLightID("room"), GroupLightID("other")
	groupSet(t, c, "room", "light.flaky_a", b)
	groupSet(t, c, "other", "light.flaky_a", a)
	setExposed(t, c, "room", true)
	setExposed(t, c, "other", true)
	noError(t, c.CallLight(ctx, a, LightAction{On: true}))
	equal(t, mustLookup(t, c, b).State, "on")
	equal(t, mustLookup(t, c, "group.room").State, "on")
}

func TestRemovingAnIntegrationStopsItServing(t *testing.T) {
	c := newCore(t)
	c.SetIntegration("roomy", roomy{})
	equal(t, len(c.LightSets()), 1)
	c.RemoveIntegration("roomy")
	equal(t, len(c.LightSets()), 0)
}

func TestEntityIDsWithPrefixListsOnlyMatchingIDs(t *testing.T) {
	c := newCore(t)
	for _, id := range []string{"light.hue_a", "light.hue_b", "light.other", "scene.hue_a"} {
		c.SetState(id, "on", nil)
	}
	ids := c.EntityIDsWithPrefix("light.hue_")
	slices.Sort(ids)
	equal(t, ids, []string{"light.hue_a", "light.hue_b"})
	equal(t, len(c.EntityIDsWithPrefix("nothing.")), 0)
}

// The tests below have no Rust counterpart.

func TestSlowSubscriberLagsAndTheStatesStayCurrent(t *testing.T) {
	c := newCore(t)
	slow, quick := c.Subscribe(), c.Subscribe()
	defer slow.Close()
	defer quick.Close()
	for i := range 1000 {
		c.SetState("light.a", []string{"on", "off"}[i%2], nil)
		_, _, _ = quick.Recv(ctx) // keeps up
	}
	id, lagged, err := slow.Recv(ctx)
	noError(t, err)
	equal(t, [2]any{id, lagged}, [2]any{"", true})
	if len(slow.ids) != 0 {
		t.Fatalf("lagging must drop the queue, %d left", len(slow.ids))
	}
	equal(t, mustState(t, c, "light.a").State, "off")
	equal(t, c.AllStates()[0].State, "off")

	// after the lag report the subscriber follows along again
	c.SetState("light.b", "on", nil)
	id, lagged, err = slow.Recv(ctx)
	noError(t, err)
	equal(t, [2]any{id, lagged}, [2]any{"light.b", false})
}

func TestPublishNeverBlocksOnAFullSubscriber(t *testing.T) {
	c := newCore(t)
	sub := c.Subscribe()
	defer sub.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 5 * subscriptionBuffer {
			c.SetState("light.a", "x", Attributes{"n": 1})
			c.RemoveState("light.a")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the publisher blocked on a subscriber that does not read")
	}
}

func TestRecvStopsOnContextAndClose(t *testing.T) {
	c := newCore(t)
	sub := c.Subscribe()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := sub.Recv(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	sub.Close()
	sub.Close() // twice is fine
	if _, _, err := sub.Recv(ctx); !errors.Is(err, ErrSubscriptionClosed) {
		t.Fatalf("got %v, want ErrSubscriptionClosed", err)
	}
	c.SetState("light.a", "on", nil) // a closed subscription is no longer fed
	equal(t, len(sub.ids), 0)
}

func TestStatesAreCopiedOnTheWayIn(t *testing.T) {
	c := newCore(t)
	attributes := Attributes{"friendly_name": "A"}
	c.SetState("light.a", "on", attributes)
	attributes["friendly_name"] = "changed"
	got := mustState(t, c, "light.a")
	got.Attributes["friendly_name"] = "changed too"
	equal(t, mustState(t, c, "light.a").Attributes["friendly_name"], "A")
}

// blockingLight blocks in CallLight until released, to prove the core holds no
// lock around integration calls.
type blockingLight struct {
	BaseIntegration
	entered chan struct{}
	release chan struct{}
}

func (blockingLight) Owns(id string) bool { return id == "light.slow" }

func (b blockingLight) CallLight(context.Context, string, LightAction) error {
	close(b.entered)
	<-b.release
	return nil
}

func TestNoLockIsHeldDuringAnIntegrationCall(t *testing.T) {
	c := newCore(t)
	slow := blockingLight{entered: make(chan struct{}), release: make(chan struct{})}
	c.SetIntegration("slow", slow)
	c.SetState("light.slow", "off", nil)
	result := make(chan error, 1)
	go func() { result <- c.CallLight(ctx, "light.slow", LightAction{On: true}) }()
	<-slow.entered

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		c.SetState("light.other", "on", nil)
		c.SetIntegration("other", fakeLights{prefix: "light.other"})
		c.RemoveIntegration("other")
		_ = c.AllStates()
		_ = c.LightSets()
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the core blocked while an integration call was in flight")
	}
	close(slow.release)
	noError(t, <-result)
	equal(t, mustState(t, c, "light.slow").State, "on")
}

func TestAllowed(t *testing.T) {
	c := newCore(t)
	groupSet(t, c, "g", "light.a")
	setExposed(t, c, "g", true)
	for _, tc := range []struct {
		scope store.Scope
		id    string
		want  bool
	}{
		{nil, "light.anything", true},
		{store.Scope{}, "light.a", false},
		{store.Scope{"g"}, "group.g", true},
		{store.Scope{"g"}, "group.other", false},
		{store.Scope{"g"}, GroupLightID("g"), true},
		{store.Scope{"other"}, GroupLightID("g"), false},
		{store.Scope{"g"}, "light.a", true},
		{store.Scope{"g"}, "light.b", false},
	} {
		if got := c.Allowed(ctx, tc.scope, tc.id); got != tc.want {
			t.Errorf("Allowed(%v, %s) = %v, want %v", tc.scope, tc.id, got, tc.want)
		}
	}
}

func TestCallLightsReturnsTheFirstMembersErrorNotTheFirstToFinish(t *testing.T) {
	c := newCore(t)
	c.SetIntegration("delayed", delayedFailures{})
	err := c.CallLights(ctx, []string{"light.d_slow_bad", "light.d_fast_bad"}, LightAction{})
	if err == nil || err.Error() != "light.d_slow_bad failed" {
		t.Fatalf("got %v, want the first member's error", err)
	}
}

// delayedFailures fails every call, the "slow" ones after the "fast" ones.
type delayedFailures struct{ BaseIntegration }

func (delayedFailures) Owns(id string) bool { return strings.HasPrefix(id, "light.d_") }

func (delayedFailures) CallLight(_ context.Context, id string, _ LightAction) error {
	if strings.Contains(id, "slow") {
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New(id + " failed")
}

func TestActivateSceneMarksTheActivationTime(t *testing.T) {
	c := newCore(t)
	c.SetIntegration("scenes", sceneMaker{})
	c.SetState("scene.x", "unknown", Attributes{"friendly_name": "X"})
	noError(t, c.ActivateScene(ctx, "scene.x"))
	s := mustState(t, c, "scene.x")
	if !strings.Contains(s.State, "T") {
		t.Fatalf("state %q is not a timestamp", s.State)
	}
	equal(t, s.Attributes["friendly_name"], "X")
	if err := c.ActivateScene(ctx, "scene.nobody"); !errors.Is(err, ErrNoIntegration) {
		t.Fatalf("got %v, want ErrNoIntegration", err)
	}
	c.SetIntegration("fake", fakeLights{prefix: "light.fake"})
	if err := c.ActivateScene(ctx, "light.fake1"); err == nil || err.Error() != "scenes are not supported" {
		t.Fatalf("got %v, want the default scene error", err)
	}
}

type sceneMaker struct{ BaseIntegration }

func (sceneMaker) Owns(id string) bool { return strings.HasPrefix(id, "scene.x") }

func (sceneMaker) CallLight(context.Context, string, LightAction) error { return nil }

func (sceneMaker) ActivateScene(context.Context, string) error { return nil }
