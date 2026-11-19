package server

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/henry40408/domus/internal/core"
	"github.com/henry40408/domus/internal/store"
	"github.com/henry40408/domus/internal/timefmt"
)

// wsMaxSubscriptions is how many subscribe_entities / subscribe_events a
// connection may hold open together.
const wsMaxSubscriptions = 16

// wsEmptyLists are the registries domus has nothing in; the apps ask for them
// and expect a list.
var wsEmptyLists = []string{
	"config/device_registry/list",
	"config/entity_registry/list",
	"config/area_registry/list",
	"config/floor_registry/list",
	"config/label_registry/list",
}

// entitySubscription is an open subscribe_entities: its message id, the
// entities it was limited to (nil: all) and the ones the client currently
// has, so removals can be told.
type entitySubscription struct {
	id    uint64
	only  map[string]bool
	known map[string]bool
}

func (s *entitySubscription) wants(entityID string) bool {
	return s.only == nil || s.only[entityID]
}

// wsSession is the state of one authenticated connection. Everything runs on
// the connection's session goroutine, so it needs no locking.
type wsSession struct {
	app  *App
	send wsSend
	// tokenHash and refreshID say which login the connection lives on.
	tokenHash string
	refreshID *int64
	scope     store.Scope
	readOnly  bool
	user      *store.User
	subs      []*entitySubscription
	// events are the ids of subscribe_events, acknowledged but never fed.
	events map[uint64]bool
	// lastID is the highest message id so far; ids must increase, as in Home
	// Assistant.
	lastID uint64
}

func (a *App) newWebsocketSession(ctx context.Context, send wsSend, token store.TokenAuth, tokenHash string) *wsSession {
	session := &wsSession{
		app:       a,
		send:      send,
		tokenHash: tokenHash,
		refreshID: token.RefreshID,
		scope:     token.Scope,
		readOnly:  token.ReadOnly,
		events:    map[uint64]bool{},
	}
	if token.UserID != nil {
		session.user = session.lookupUser(ctx, *token.UserID)
	}
	return session
}

// lookupUser is the user, or nil when there is none; a failing store counts as
// none, which ends the connection at the next recheck.
func (s *wsSession) lookupUser(ctx context.Context, id int64) *store.User {
	user, err := s.app.core.Store().UserByID(ctx, id)
	if err == nil {
		return &user
	}
	s.app.warnUnlessNotFound("websocket user lookup failed", err)
	return nil
}

func wsResult(id uint64, value any) map[string]any {
	return map[string]any{"id": id, "type": "result", "success": true, "result": value}
}

// wsFailure is a failed result; id is nil when the message had none.
func wsFailure(id *uint64, code, message string) map[string]any {
	return map[string]any{
		"id": id, "type": "result", "success": false,
		"error": map[string]string{"code": code, "message": message},
	}
}

// floatJSON prints f the way serde_json does, always with a fraction: the
// apps tell a Float from an Integer.
func floatJSON(f float64) json.RawMessage {
	text := strconv.FormatFloat(f, 'f', -1, 64)
	if strings.Contains(text, ".") {
		return json.RawMessage(text)
	}
	return json.RawMessage(text + ".0")
}

// compressed is a state in the compressed form of subscribe_entities.
func compressed(state core.State) map[string]any {
	return map[string]any{
		"s":  state.State,
		"a":  state.Attributes,
		"c":  state.Context.ID,
		"lc": floatJSON(timefmt.Epoch(state.LastChanged)),
		"lu": floatJSON(timefmt.Epoch(state.LastUpdated)),
	}
}

// sync brings one subscription up to date: states it lacks or has stale are
// added (replacing whatever the client had), those it has but may no longer
// see are removed.
func (s *wsSession) sync(ctx context.Context, sub *entitySubscription) error {
	states := s.app.visibleStates(ctx, s.scope, sub.only)
	now := map[string]bool{}
	added := map[string]any{}
	for _, state := range states {
		now[state.EntityID] = true
		added[state.EntityID] = compressed(state)
	}
	event := map[string]any{"a": added}
	var gone []string
	for id := range sub.known {
		if !now[id] {
			gone = append(gone, id)
		}
	}
	if len(gone) > 0 {
		slices.Sort(gone)
		event["r"] = gone
	}
	if err := s.send(ctx, map[string]any{"id": sub.id, "type": "event", "event": event}); err != nil {
		return err
	}
	sub.known = now
	return nil
}

func (s *wsSession) resync(ctx context.Context) error {
	for _, sub := range s.subs {
		if err := s.sync(ctx, sub); err != nil {
			return err
		}
	}
	return nil
}

// onChange handles one broadcast: a lagging connection missed some changes,
// so every subscriber is brought up to date.
func (s *wsSession) onChange(ctx context.Context, got change) error {
	if got.lagged {
		return s.resync(ctx)
	}
	return s.changed(ctx, got.id)
}

// affectedBy is the entity and every group (and group light) it is a member
// of: what a change of the entity changes.
func (s *wsSession) affectedBy(ctx context.Context, entityID string) []string {
	st := s.app.core.Store()
	affected := []string{entityID}
	names, err := st.GroupNames(ctx)
	if err != nil {
		s.app.logger.Warn("listing groups failed", "error", err)
	}
	for _, name := range names {
		members, err := st.GroupMembers(ctx, name)
		if err != nil {
			s.app.logger.Warn("reading a group failed", "group", name, "error", err)
		}
		if !slices.Contains(members, entityID) {
			continue
		}
		affected = append(affected, "group."+name)
		if exposed, _ := st.GroupExposed(ctx, name); exposed {
			affected = append(affected, core.GroupLightID(name))
		}
	}
	return affected
}

// changed pushes what a change of one entity does to the states the client can
// see.
func (s *wsSession) changed(ctx context.Context, entityID string) error {
	if len(s.subs) == 0 {
		return nil
	}
	// A nil state is one the caller may not see, or that is gone.
	current := map[string]*core.State{}
	affected := s.affectedBy(ctx, entityID)
	for _, id := range affected {
		current[id] = nil
		if !s.app.core.Allowed(ctx, s.scope, id) {
			continue
		}
		if state, ok := s.app.core.Lookup(ctx, id); ok {
			current[id] = &state
		}
	}
	for _, sub := range s.subs {
		if err := s.pushChange(ctx, sub, affected, current); err != nil {
			return err
		}
	}
	return nil
}

func (s *wsSession) pushChange(ctx context.Context, sub *entitySubscription, affected []string, current map[string]*core.State) error {
	added := map[string]any{}
	removed := []string{}
	for _, id := range affected {
		if !sub.wants(id) {
			continue
		}
		if state := current[id]; state != nil {
			added[id] = compressed(*state)
			sub.known[id] = true
			continue
		}
		if sub.known[id] {
			delete(sub.known, id)
			removed = append(removed, id)
		}
	}
	event := map[string]any{}
	if len(added) > 0 {
		event["a"] = added
	}
	if len(removed) > 0 {
		event["r"] = removed
	}
	if len(event) == 0 {
		return nil
	}
	return s.send(ctx, map[string]any{"id": sub.id, "type": "event", "event": event})
}

// onRecheck is the periodic check: it fails when the login is gone (the
// connection ends), and resyncs when what the caller may see changed.
func (s *wsSession) onRecheck(ctx context.Context) error {
	alive, changed := s.recheck(ctx)
	if !alive {
		return errLoginGone
	}
	if changed {
		return s.resync(ctx)
	}
	return nil
}

var errLoginGone = errors.New("the login is gone")

// recheck looks the login up again without counting as use. alive is false
// when the login is gone; changed is true when the scope differs from the
// one before. Like Home Assistant, an app connection lives as long as its
// login (refresh token), not its short-lived access token.
func (s *wsSession) recheck(ctx context.Context) (alive, changed bool) {
	st := s.app.core.Store()
	var user *store.User
	if s.user != nil {
		user = s.lookupUser(ctx, s.user.ID)
	}
	var scope store.Scope
	if s.refreshID != nil {
		exists, err := st.RefreshExists(ctx, *s.refreshID)
		if err != nil || !exists {
			return false, false
		}
		// App logins carry no scope of their own: it is the user's.
		if user != nil {
			scope = user.Scope
		}
	} else {
		view, err := st.TokenView(ctx, s.tokenHash)
		if err != nil {
			return false, false
		}
		scope = view.Scope
	}
	if s.user != nil && user == nil {
		return false, false
	}
	s.user = user
	changed = !sameScope(scope, s.scope)
	s.scope = scope
	return true, changed
}

// sameScope tells unrestricted (nil) from empty, which slices.Equal does not.
func sameScope(a, b store.Scope) bool {
	return (a == nil) == (b == nil) && slices.Equal(a, b)
}

// wsMessage is a client message with its fields still raw, so an id is read
// as the integer it is written as and a service call's numbers keep the shape
// the REST API gives them.
type wsMessage map[string]json.RawMessage

func (m wsMessage) text(key string) string {
	var text string
	_ = json.Unmarshal(m[key], &text) // a non-string stays empty
	return text
}

func (m wsMessage) unsigned(key string) (uint64, bool) {
	number, err := strconv.ParseUint(string(m[key]), 10, 64)
	return number, err == nil
}

func (m wsMessage) value(key string) (any, bool) {
	raw, present := m[key]
	if !present {
		return nil, false
	}
	var value any
	_ = json.Unmarshal(raw, &value)
	return value, true
}

// acceptMessage parses a client message and records its id; the failure to
// send back is returned when it is not acceptable.
func (s *wsSession) acceptMessage(text string) (uint64, wsMessage, map[string]any) {
	var msg wsMessage
	if json.Unmarshal([]byte(text), &msg) != nil || msg == nil {
		return 0, nil, wsFailure(nil, "invalid_format", "Message incorrectly formatted.")
	}
	id, ok := msg.unsigned("id")
	if !ok {
		return 0, nil, wsFailure(nil, "invalid_format", "Message needs an integer id.")
	}
	if id <= s.lastID {
		return 0, nil, wsFailure(&id, "id_reuse", "Identifier values have to increase.")
	}
	s.lastID = id
	return id, msg, nil
}

func (s *wsSession) subscriptionCount() int { return len(s.subs) + len(s.events) }

func (s *wsSession) subscribeEvents(id uint64) map[string]any {
	if s.subscriptionCount() >= wsMaxSubscriptions {
		return wsFailure(&id, "too_many", "Too many subscriptions.")
	}
	s.events[id] = true
	return wsResult(id, nil)
}

func (s *wsSession) unsubscribeEvents(id uint64, msg wsMessage) map[string]any {
	target, _ := msg.unsigned("subscription")
	before := s.subscriptionCount()
	s.subs = slices.DeleteFunc(s.subs, func(sub *entitySubscription) bool { return sub.id == target })
	delete(s.events, target)
	if s.subscriptionCount() == before {
		return wsFailure(&id, "not_found", "Subscription not found.")
	}
	return wsResult(id, nil)
}

// entityFilter is the entities a subscribe_entities is limited to; nil when it
// names none (an empty list limits to nothing).
func entityFilter(msg wsMessage) map[string]bool {
	value, _ := msg.value("entity_ids")
	list, isList := value.([]any)
	if !isList {
		return nil
	}
	only := map[string]bool{}
	for _, item := range list {
		if text, isText := item.(string); isText {
			only[text] = true
		}
	}
	return only
}

func (s *wsSession) subscribeEntities(ctx context.Context, id uint64, msg wsMessage) error {
	if s.subscriptionCount() >= wsMaxSubscriptions {
		return s.send(ctx, wsFailure(&id, "too_many", "Too many subscriptions."))
	}
	sub := &entitySubscription{id: id, only: entityFilter(msg), known: map[string]bool{}}
	s.subs = append(s.subs, sub)
	// The result comes first, then the snapshot as the first event.
	if err := s.send(ctx, wsResult(id, nil)); err != nil {
		return err
	}
	return s.sync(ctx, sub)
}

func (s *wsSession) entityRegistryForDisplay(ctx context.Context, id uint64) map[string]any {
	entities := []any{}
	for _, state := range s.app.visibleStates(ctx, s.scope, nil) {
		entity := map[string]any{"ei": state.EntityID}
		if name, ok := state.Attributes["friendly_name"]; ok {
			entity["en"] = name
		}
		entities = append(entities, entity)
	}
	return wsResult(id, map[string]any{"entity_categories": map[string]any{}, "entities": entities})
}

// command answers one client message.
func (s *wsSession) command(ctx context.Context, text string) error {
	id, msg, rejection := s.acceptMessage(text)
	if rejection != nil {
		return s.send(ctx, rejection)
	}
	kind := msg.text("type")
	var reply any
	switch kind {
	case "ping":
		reply = map[string]any{"id": id, "type": "pong"}
	case "supported_features":
		reply = wsResult(id, nil)
	case "subscribe_events":
		reply = s.subscribeEvents(id)
	case "unsubscribe_events":
		reply = s.unsubscribeEvents(id, msg)
	case "auth/current_user":
		reply = wsResult(id, s.currentUser())
	case "get_config":
		reply = wsResult(id, haConfig())
	case "get_services", "get_panels":
		reply = wsResult(id, map[string]any{})
	case "get_states":
		reply = wsResult(id, s.app.visibleStates(ctx, s.scope, nil))
	case "subscribe_entities":
		return s.subscribeEntities(ctx, id, msg)
	case "config/entity_registry/list_for_display":
		reply = s.entityRegistryForDisplay(ctx, id)
	case "call_service":
		reply = s.callService(ctx, id, msg)
	default:
		reply = wsFailure(&id, "unknown_command", "Unknown command.")
		if slices.Contains(wsEmptyLists, kind) {
			reply = wsResult(id, []any{})
		}
	}
	return s.send(ctx, reply)
}

// callService is call_service for the domains domus has: lights and scenes.
func (s *wsSession) callService(ctx context.Context, id uint64, msg wsMessage) map[string]any {
	if s.readOnly {
		return wsFailure(&id, "unauthorized", "This token is read-only.")
	}
	body := map[string]any{}
	if data, _ := msg.value("service_data"); data != nil {
		if object, isObject := data.(map[string]any); isObject {
			body = object
		}
	}
	if target, present := msg.value("target"); present {
		body["target"] = target
	}
	service := msg.text("service")
	var failure error
	switch msg.text("domain") {
	case "light":
		_, failure = s.app.runLight(ctx, s.scope, service, body)
	case "scene":
		_, failure = s.app.runScene(ctx, s.scope, service, body)
	default:
		failure = errServiceNotFound
	}
	return serviceReply(id, failure)
}

func serviceReply(id uint64, failure error) map[string]any {
	if failure == nil {
		return wsResult(id, map[string]any{
			"context":  map[string]any{"id": "", "parent_id": nil, "user_id": nil},
			"response": nil,
		})
	}
	httpFailure := httpErrorOf(failure)
	switch httpFailure.status {
	case 404:
		return wsFailure(&id, "not_found", "Service not found.")
	case 400:
		return wsFailure(&id, "invalid_format", httpFailure.message)
	default:
		return wsFailure(&id, "home_assistant_error", httpFailure.message)
	}
}

func (s *wsSession) currentUser() map[string]any {
	if s.user == nil {
		return map[string]any{
			"id": "0", "name": "domus", "is_owner": false, "is_admin": false,
			"credentials": []any{}, "mfa_modules": []any{},
		}
	}
	return map[string]any{
		"id":          strconv.FormatInt(s.user.ID, 10),
		"name":        s.user.Username,
		"is_owner":    s.user.IsAdmin,
		"is_admin":    s.user.IsAdmin,
		"credentials": []any{},
		"mfa_modules": []any{},
	}
}
