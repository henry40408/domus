package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

func nowSecs() int64 { return time.Now().Unix() }

func ptr[T any](v T) *T { return &v }

func scopeOf(groups ...string) Scope {
	return append(Scope{}, groups...)
}

func noError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

func plainToken(userID int64, name, hash string) NewToken {
	return NewToken{UserID: userID, Name: name, TokenHash: hash}
}

func exec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

// adminCount is the number of admins.
func adminCount(t *testing.T, s *Store) int64 {
	t.Helper()
	return queryInt(t, s, "SELECT COUNT(*) FROM users WHERE is_admin = 1")
}

func queryInt(t *testing.T, s *Store, query string, args ...any) int64 {
	t.Helper()
	var n int64
	noError(t, s.db.QueryRowContext(ctx, query, args...).Scan(&n))
	return n
}

// deleteUser reports whether the user was deleted. Callers keep another admin
// around, or the store refuses.
func deleteUser(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	result, err := s.DeleteUserKeepingAnAdmin(ctx, id)
	noError(t, err)
	return result == UserDeleted
}

func TestFirstAdminOnlyWhileNoUserExists(t *testing.T) {
	s := openMemory(t)
	n, err := s.UserCount(ctx)
	noError(t, err)
	if n != 0 {
		t.Fatalf("user count = %d", n)
	}
	id, err := s.CreateFirstAdmin(ctx, "Root", "a")
	noError(t, err)
	if _, err := s.CreateFirstAdmin(ctx, "other", "b"); !errors.Is(err, ErrAlreadySetUp) {
		t.Fatalf("second first admin: %v", err)
	}
	user, hash, err := s.UserLogin(ctx, "root")
	noError(t, err)
	if user.ID != id || !user.IsAdmin || hash != "a" {
		t.Fatalf("user = %+v hash = %q", user, hash)
	}
	if user.Username != "Root" {
		t.Fatalf("username = %q, want the stored spelling", user.Username)
	}
}

func TestUserManagement(t *testing.T) {
	s := openMemory(t)
	admin, err := s.CreateFirstAdmin(ctx, "admin", "a")
	noError(t, err)
	bob, err := s.CreateUser(ctx, "bob", "b", false)
	noError(t, err)
	if _, err := s.CreateUser(ctx, "BOB", "x", false); err == nil {
		t.Fatal("names are case-insensitive")
	}
	if n := adminCount(t, s); n != 1 {
		t.Fatalf("admins = %d", n)
	}
	noError(t, s.SetAdmin(ctx, bob, true))
	if n := adminCount(t, s); n != 2 {
		t.Fatalf("admins = %d", n)
	}
	noError(t, s.SetPassword(ctx, bob, "new"))
	hash, err := s.UserHash(ctx, bob)
	noError(t, err)
	if hash != "new" {
		t.Fatalf("hash = %q", hash)
	}
	users, err := s.ListUsers(ctx)
	noError(t, err)
	if n := len(users); n != 2 {
		t.Fatalf("users = %d", n)
	}

	noError(t, s.CreateSession(ctx, "sb", bob, ""))
	_, err = s.CreateToken(ctx, plainToken(bob, "watch", "hb"))
	noError(t, err)
	if !deleteUser(t, s, bob) {
		t.Fatal("delete failed")
	}
	if deleteUser(t, s, bob) {
		t.Fatal("deleted twice")
	}
	if _, err := s.SessionUser(ctx, "sb"); !isNotFound(err) {
		t.Fatalf("session survived its user: %v", err)
	}
	if _, err := s.TokenAuth(ctx, "hb"); !isNotFound(err) {
		t.Fatalf("tokens go with the user: %v", err)
	}
	users, err = s.ListUsers(ctx)
	noError(t, err)
	if users[0].ID != admin {
		t.Fatalf("users = %+v", users)
	}
}

func TestTokenLifecycle(t *testing.T) {
	s := openMemory(t)
	a, err := s.CreateUser(ctx, "a", "x", false)
	noError(t, err)
	b, err := s.CreateUser(ctx, "b", "x", false)
	noError(t, err)
	id, err := s.CreateToken(ctx, plainToken(a, "watch", "h1"))
	noError(t, err)
	_, err = s.CreateToken(ctx, plainToken(b, "other", "h3"))
	noError(t, err)
	tokens, err := s.ListTokens(ctx, &a)
	noError(t, err)
	if got := tokens[0].LastUsed; got != nil {
		t.Fatalf("last_used = %v before any use", *got)
	}
	_, err = s.TokenAuth(ctx, "h1")
	noError(t, err)
	tokens, err = s.ListTokens(ctx, &a)
	noError(t, err)
	if got := tokens[0].LastUsed; got == nil {
		t.Fatal("use not recorded")
	}
	if _, err := s.TokenAuth(ctx, "h2"); !isNotFound(err) {
		t.Fatalf("unknown token: %v", err)
	}
	tokens, err = s.ListTokens(ctx, &a)
	noError(t, err)
	if n := len(tokens); n != 1 {
		t.Fatalf("a's tokens = %d", n)
	}
	tokens, err = s.ListTokens(ctx, &a)
	noError(t, err)
	if name := tokens[0].Name; name != "watch" {
		t.Fatalf("name = %q", name)
	}
	all, err := s.ListTokens(ctx, nil)
	noError(t, err)
	if len(all) != 2 || all[1].Username == nil || *all[1].Username != "b" {
		t.Fatalf("all tokens = %+v", all)
	}
	if err := s.RevokeToken(ctx, id, &b); !isNotFound(err) {
		t.Fatalf("not b's token: %v", err)
	}
	noError(t, s.RevokeToken(ctx, id, &a))
	if _, err := s.TokenAuth(ctx, "h1"); !isNotFound(err) {
		t.Fatalf("revoked token still works: %v", err)
	}
	if err := s.RevokeToken(ctx, id, nil); !isNotFound(err) {
		t.Fatalf("revoked twice: %v", err)
	}
}

func TestTokenScopeRoundtrip(t *testing.T) {
	s := openMemory(t)
	_, err := s.CreateToken(ctx, plainToken(1, "open", "h-open"))
	noError(t, err)
	scoped := plainToken(1, "scoped", "h-scoped")
	scoped.Scope = scopeOf("a", "b")
	_, err = s.CreateToken(ctx, scoped)
	noError(t, err)
	scopeOfToken := func(hash string) (Scope, error) {
		auth, err := s.TokenAuth(ctx, hash)
		return auth.Scope, err
	}
	if got, err := scopeOfToken("h-open"); err != nil || got != nil {
		t.Fatalf("open scope = %#v, %v", got, err)
	}
	if got, err := scopeOfToken("h-scoped"); err != nil || !reflect.DeepEqual(got, scopeOf("a", "b")) {
		t.Fatalf("scoped scope = %#v, %v", got, err)
	}
	if _, err := scopeOfToken("nope"); !isNotFound(err) {
		t.Fatalf("unknown token: %v", err)
	}
	list, err := s.ListTokens(ctx, nil)
	noError(t, err)
	if list[0].Scope != nil || !reflect.DeepEqual(list[1].Scope, scopeOf("a", "b")) {
		t.Fatalf("list scopes = %#v %#v", list[0].Scope, list[1].Scope)
	}
}

func TestScopesIntersect(t *testing.T) {
	cases := []struct {
		name    string
		a, b    Scope
		want    Scope
		wantNil bool
	}{
		{"both unrestricted", nil, nil, nil, true},
		{"left unrestricted", nil, scopeOf("a"), scopeOf("a"), false},
		{"right unrestricted", scopeOf("a"), nil, scopeOf("a"), false},
		{"overlap", scopeOf("a", "b"), scopeOf("b", "c"), scopeOf("b"), false},
		// nothing in common: nothing allowed, never everything
		{"disjoint", scopeOf("a"), scopeOf("b"), scopeOf(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IntersectScope(tc.a, tc.b)
			if (got == nil) != tc.wantNil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestReadOnlyExpiryAndUserScopeShapeAToken(t *testing.T) {
	s := openMemory(t)
	now := nowSecs()
	u, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	ro := plainToken(u, "ro", "h-ro")
	ro.ReadOnly = true
	_, err = s.CreateToken(ctx, ro)
	noError(t, err)
	live := plainToken(u, "live", "h-live")
	live.Scope = scopeOf("a", "b")
	live.Expires = ptr(now + 100)
	_, err = s.CreateToken(ctx, live)
	noError(t, err)
	dead := plainToken(u, "dead", "h-dead")
	dead.Expires = ptr(now - 1)
	_, err = s.CreateToken(ctx, dead)
	noError(t, err)

	token, err := s.TokenAuth(ctx, "h-ro")
	noError(t, err)
	if !token.ReadOnly {
		t.Fatal("h-ro must be read-only")
	}
	token, err = s.TokenAuth(ctx, "h-live")
	noError(t, err)
	if token.ReadOnly {
		t.Fatal("h-live must not be read-only")
	}
	if _, err := s.TokenAuth(ctx, "h-dead"); !isNotFound(err) {
		t.Fatalf("expired token: %v", err)
	}
	list, err := s.ListTokens(ctx, nil)
	noError(t, err)
	if list[1].Expires == nil || *list[1].Expires != now+100 {
		t.Fatalf("expires = %v", list[1].Expires)
	}
	if !list[0].ReadOnly || list[0].Expires != nil {
		t.Fatalf("first token = %+v", list[0])
	}

	// shrinking the owner's groups shrinks the token with it
	noError(t, s.SetUserScope(ctx, u, scopeOf("b")))
	token, err = s.TokenAuth(ctx, "h-live")
	noError(t, err)
	if got := token.Scope; !reflect.DeepEqual(got, scopeOf("b")) {
		t.Fatalf("live scope = %#v", got)
	}
	token, err = s.TokenAuth(ctx, "h-ro")
	noError(t, err)
	if got := token.Scope; !reflect.DeepEqual(got, scopeOf("b")) {
		t.Fatalf("ro scope = %#v", got)
	}
	noError(t, s.SetUserScope(ctx, u, nil))
	token, err = s.TokenAuth(ctx, "h-live")
	noError(t, err)
	if got := token.Scope; !reflect.DeepEqual(got, scopeOf("a", "b")) {
		t.Fatalf("live scope after unrestricting = %#v", got)
	}
	users, err := s.ListUsers(ctx)
	noError(t, err)
	if got := users[0].Scope; got != nil {
		t.Fatalf("user scope = %#v", got)
	}

	// creating a token tidies expired ones away
	_, err = s.CreateToken(ctx, plainToken(u, "new", "h-new"))
	noError(t, err)
	tokens, err := s.ListTokens(ctx, nil)
	noError(t, err)
	if n := len(tokens); n != 3 {
		t.Fatalf("tokens = %d, want 3", n)
	}
}

func TestUnparseableScopeAllowsNothing(t *testing.T) {
	scope := func(raw string, valid bool) Scope {
		return parseScope(sql.NullString{String: raw, Valid: valid})
	}
	if got := scope("", false); got != nil {
		t.Fatalf("NULL = %#v", got)
	}
	if got := scope(`["a"]`, true); !reflect.DeepEqual(got, scopeOf("a")) {
		t.Fatalf(`["a"] = %#v`, got)
	}
	for _, raw := range []string{"not json", "null", `[1]`, `{"a":1}`, ""} {
		if got := scope(raw, true); got == nil || len(got) != 0 {
			t.Fatalf("%q = %#v, want an empty, non-nil scope", raw, got)
		}
	}
}

func TestTheLastAdminSurvivesEvenWhenAdminsActAtOnce(t *testing.T) {
	s := openMemory(t)
	runTheLastAdminScenario(t, s)
}

// The same on a file database, where the pool really runs both at once.
func TestTheLastAdminSurvivesOnAFileDatabase(t *testing.T) {
	s, err := Open(ctx, filepath.Join(t.TempDir(), "domus.db"))
	noError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	runTheLastAdminScenario(t, s)
}

func runTheLastAdminScenario(t *testing.T, s *Store) {
	t.Helper()
	a, err := s.CreateUser(ctx, "a", "h", true)
	noError(t, err)
	b, err := s.CreateUser(ctx, "b", "h", true)
	noError(t, err)

	// two admins demoting each other: exactly one demotion goes through
	errs := concurrently(func() error { return s.SetAdmin(ctx, a, false) }, func() error { return s.SetAdmin(ctx, b, false) })
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("exactly one demotion may succeed: %v", errs)
	}
	if n := adminCount(t, s); n != 1 {
		t.Fatalf("admins = %d", n)
	}
	last, demoted := a, b
	if errs[0] == nil {
		last, demoted = b, a
	}
	if err := s.SetAdmin(ctx, last, false); !isNotFound(err) {
		t.Fatalf("demoting the last admin: %v", err)
	}
	if n := adminCount(t, s); n != 1 {
		t.Fatalf("admins = %d", n)
	}
	// demoting someone who is not an admin is harmless
	noError(t, s.SetAdmin(ctx, demoted, false))

	// the same for two admins deleting each other
	noError(t, s.SetAdmin(ctx, demoted, true))
	var results [2]DeleteResult
	errs = concurrently(
		func() (err error) { results[0], err = s.DeleteUserKeepingAnAdmin(ctx, a); return },
		func() (err error) { results[1], err = s.DeleteUserKeepingAnAdmin(ctx, b); return },
	)
	noError(t, errors.Join(errs[:]...))
	if !slices.Contains(results[:], UserDeleted) || !slices.Contains(results[:], LastAdmin) {
		t.Fatalf("outcomes = %v, want one Deleted and one LastAdmin", results)
	}
	if n := adminCount(t, s); n != 1 {
		t.Fatalf("admins = %d", n)
	}
	result, err := s.DeleteUserKeepingAnAdmin(ctx, 999)
	noError(t, err)
	if result != UserNotFound {
		t.Fatalf("unknown user: %v", result)
	}

	// regular users are always deletable, and their data goes with them
	c, err := s.CreateUser(ctx, "c", "h", false)
	noError(t, err)
	noError(t, s.CreateSession(ctx, "sc", c, ""))
	result, err = s.DeleteUserKeepingAnAdmin(ctx, c)
	noError(t, err)
	if result != UserDeleted {
		t.Fatalf("regular user: %v", result)
	}
	if _, err := s.SessionUser(ctx, "sc"); !isNotFound(err) {
		t.Fatalf("session survived its user: %v", err)
	}
}

// concurrently runs both functions at the same moment and returns their errors.
func concurrently(first, second func() error) [2]error {
	var errs [2]error
	var ready, done sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	for i, fn := range []func() error{first, second} {
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			errs[i] = fn()
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	return errs
}

func TestAppSessionsFollowTheirRefreshToken(t *testing.T) {
	s := openMemory(t)
	id, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	refresh, err := s.RefreshCreate(ctx, id, "client", "r1")
	noError(t, err)
	noError(t, s.CreateAppSession(ctx, NewAppLoginSession{TokenHash: "a", UserID: id, RefreshID: refresh, UserAgent: "ua"}))
	_, err = s.SessionUser(ctx, "a")
	noError(t, err)
	needsReauth, err := s.SessionNeedsReauth(ctx, "a")
	noError(t, err)
	if !needsReauth {
		t.Fatal("a fresh app session needs a password re-entry")
	}
	noError(t, s.SessionElevate(ctx, "a"))
	needsReauth, err = s.SessionNeedsReauth(ctx, "a")
	noError(t, err)
	if needsReauth {
		t.Fatal("an elevated session needs no re-entry")
	}

	// a second exchange for the same app login replaces the first session
	noError(t, s.CreateAppSession(ctx, NewAppLoginSession{TokenHash: "b", UserID: id, RefreshID: refresh, UserAgent: "ua"}))
	if _, err := s.SessionUser(ctx, "a"); !isNotFound(err) {
		t.Fatalf("replaced session: %v", err)
	}
	_, err = s.SessionUser(ctx, "b")
	noError(t, err)
	sessions, err := s.ListSessions(ctx, id, "b")
	noError(t, err)
	if n := len(sessions); n != 1 {
		t.Fatalf("sessions = %d", n)
	}

	// it is short-lived, whatever the plain session lifetime is
	expires := queryInt(t, s, "SELECT expires FROM sessions WHERE token_hash = 'b'")
	day := int64(24 * 3600)
	if now := nowSecs(); expires < now+day-5 || expires > now+day {
		t.Fatalf("expires = %d, now = %d", expires, now)
	}

	// without its refresh token (swept for idleness, say) it is dead everywhere,
	// even though the session row itself is still there
	exec(t, s, "DELETE FROM refresh_tokens WHERE id = ?1", refresh)
	if _, err := s.SessionUser(ctx, "b"); !isNotFound(err) {
		t.Fatalf("session without its login: %v", err)
	}
	valid, err := s.SessionValid(ctx, "b")
	noError(t, err)
	if valid {
		t.Fatal("session without its login is valid")
	}
	sessions, err = s.ListSessions(ctx, id, "b")
	noError(t, err)
	if n := len(sessions); n != 0 {
		t.Fatalf("sessions = %d", n)
	}

	// password sessions never need a re-entry
	noError(t, s.CreateSession(ctx, "p", id, "ua"))
	needsReauth, err = s.SessionNeedsReauth(ctx, "p")
	noError(t, err)
	if needsReauth {
		t.Fatal("password sessions never need a re-entry")
	}
}

func TestIdleSessionsExpireAndActivityIsThrottled(t *testing.T) {
	s := openMemory(t)
	id, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	noError(t, s.CreateSession(ctx, "a", id, strings.Repeat("x", 500)))
	list, err := s.ListSessions(ctx, id, "a")
	noError(t, err)
	if len(list) != 1 || !list[0].Current {
		t.Fatalf("sessions = %+v", list)
	}
	if len(list[0].UserAgent) != 200 {
		t.Fatalf("user agent is %d long, want it truncated to 200", len(list[0].UserAgent))
	}

	setSeen := func(secsAgo int64) { exec(t, s, "UPDATE sessions SET last_seen = ?1", nowSecs()-secsAgo) }
	seen := func() int64 {
		t.Helper()
		sessions, err := s.ListSessions(ctx, id, "a")
		noError(t, err)
		return sessions[0].LastSeen
	}

	// a recent request does not write again
	setSeen(10)
	before := seen()
	_, err = s.SessionUser(ctx, "a")
	noError(t, err)
	if seen() != before {
		t.Fatal("a recent request rewrote last_seen")
	}
	// after a minute it is refreshed
	setSeen(120)
	_, err = s.SessionUser(ctx, "a")
	noError(t, err)
	if seen() < nowSecs()-1 {
		t.Fatal("last_seen was not refreshed")
	}
	// checking validity never counts as activity
	setSeen(120)
	valid, err := s.SessionValid(ctx, "a")
	noError(t, err)
	if !valid {
		t.Fatal("session should be valid")
	}
	if seen() > nowSecs()-119 {
		t.Fatal("SessionValid counted as activity")
	}

	// idle for a day: dead, hidden and swept by the next login
	setSeen(SessionIdleSecs + 5)
	if _, err := s.SessionUser(ctx, "a"); !isNotFound(err) {
		t.Fatalf("idle session: %v", err)
	}
	valid, err = s.SessionValid(ctx, "a")
	noError(t, err)
	if valid {
		t.Fatal("idle session is valid")
	}
	sessions, err := s.ListSessions(ctx, id, "a")
	noError(t, err)
	if n := len(sessions); n != 0 {
		t.Fatalf("idle session listed: %d", n)
	}
	noError(t, s.CreateSession(ctx, "b", id, ""))
	sessions, err = s.ListSessions(ctx, id, "b")
	noError(t, err)
	if n := len(sessions); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := openMemory(t)
	id, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	if _, err := s.SessionUser(ctx, "x"); !isNotFound(err) {
		t.Fatalf("unknown session: %v", err)
	}
	noError(t, s.CreateSession(ctx, "x", id, ""))
	user, err := s.SessionUser(ctx, "x")
	noError(t, err)
	if user.Username != "u" {
		t.Fatalf("user = %+v", user)
	}
	noError(t, s.DeleteSession(ctx, "x"))
	if _, err := s.SessionUser(ctx, "x"); !isNotFound(err) {
		t.Fatalf("deleted session: %v", err)
	}
	noError(t, s.CreateSession(ctx, "y", id, ""))
	noError(t, s.DeleteUserSessions(ctx, id))
	if _, err := s.SessionUser(ctx, "y"); !isNotFound(err) {
		t.Fatalf("user sessions deleted: %v", err)
	}
}

func TestGroupsKeepOrderAndDedupe(t *testing.T) {
	s := openMemory(t)
	exists := func(name string) bool {
		t.Helper()
		got, err := s.GroupExists(ctx, name)
		noError(t, err)
		return got
	}
	exposed := func(name string) bool {
		got, err := s.GroupExposed(ctx, name)
		if err != nil && !isNotFound(err) {
			t.Fatal(err)
		}
		return got
	}
	members := func(name string) []string {
		t.Helper()
		got, err := s.GroupMembers(ctx, name)
		noError(t, err)
		return got
	}
	if exists("garmin") {
		t.Fatal("group exists before it is created")
	}
	noError(t, s.GroupSet(ctx, "garmin", []string{"light.b", "light.a", "light.b"}))
	if !exists("garmin") {
		t.Fatal("group missing")
	}
	if got := members("garmin"); !reflect.DeepEqual(got, []string{"light.b", "light.a"}) {
		t.Fatalf("members = %v", got)
	}
	noError(t, s.GroupSet(ctx, "garmin", []string{"light.c"}))
	if got := members("garmin"); !reflect.DeepEqual(got, []string{"light.c"}) {
		t.Fatalf("members = %v", got)
	}
	got, err := s.GroupNames(ctx)
	noError(t, err)
	if !reflect.DeepEqual(got, []string{"garmin"}) {
		t.Fatalf("names = %v", got)
	}
	if exposed("garmin") {
		t.Fatal("a new group is not exposed")
	}
	noError(t, s.GroupSetExposed(ctx, "garmin", true))
	if !exposed("garmin") {
		t.Fatal("group should be exposed")
	}
	got, err = s.ExposedGroupNames(ctx)
	noError(t, err)
	if !reflect.DeepEqual(got, []string{"garmin"}) {
		t.Fatalf("exposed = %v", got)
	}
	noError(t, s.GroupSet(ctx, "garmin", []string{"light.d"}))
	if !exposed("garmin") {
		t.Fatal("editing members keeps the flag")
	}
	if err := s.GroupSetExposed(ctx, "nope", true); !isNotFound(err) {
		t.Fatalf("unknown group: %v", err)
	}
	noError(t, s.GroupDelete(ctx, "garmin"))
	if exposed("garmin") {
		t.Fatal("deleted group is exposed")
	}
	if got := members("garmin"); len(got) != 0 {
		t.Fatalf("members of a deleted group = %v", got)
	}
}

func TestGroupSetKeepsPositionsGaplessWhenMembersRepeat(t *testing.T) {
	s := openMemory(t)
	noError(t, s.GroupSet(ctx, "g", []string{"a", "b", "a", "c", "b"}))
	rows := queryInt(t, s, "SELECT group_concat(pos) = '0,1,2' FROM (SELECT pos FROM group_members ORDER BY pos)")
	if rows != 1 {
		t.Fatal("positions have a gap")
	}
}

func TestHueBridgeRoundtrip(t *testing.T) {
	s := openMemory(t)
	if _, err := s.HueGet(ctx); !isNotFound(err) {
		t.Fatalf("no bridge yet: %v", err)
	}
	bridge := HueBridge{IP: "192.168.1.2", Key: "k"}
	noError(t, s.HueSet(ctx, bridge))
	got, err := s.HueGet(ctx)
	noError(t, err)
	if got != bridge {
		t.Fatalf("bridge = %+v", got)
	}
	noError(t, s.HueSet(ctx, HueBridge{IP: "10.0.0.9", Key: "k2"}))
	got, err = s.HueGet(ctx)
	noError(t, err)
	if got.IP != "10.0.0.9" {
		t.Fatalf("bridge was not replaced: %+v", got)
	}
}

func TestFileDatabaseReopensWithoutRerunningMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "domus.db")
	s, err := Open(ctx, path)
	noError(t, err)
	_, err = s.CreateFirstAdmin(ctx, "root", "a")
	noError(t, err)
	noError(t, s.Close())
	s, err = Open(ctx, path)
	noError(t, err)
	defer func() { _ = s.Close() }()
	hash, err := s.UserHash(ctx, 1)
	noError(t, err)
	if hash != "a" {
		t.Fatalf("hash = %q", hash)
	}
}

func TestUserCountCountsUsers(t *testing.T) {
	s := openMemory(t)
	_, err := s.CreateFirstAdmin(ctx, "a", "h")
	noError(t, err)
	_, err = s.CreateUser(ctx, "b", "h", false)
	noError(t, err)
	n, err := s.UserCount(ctx)
	noError(t, err)
	if n != 2 {
		t.Fatalf("users = %d", n)
	}
}

func TestChangingAnUnknownUserReportsFailure(t *testing.T) {
	s := openMemory(t)
	if err := s.SetPassword(ctx, 99, "x"); !isNotFound(err) {
		t.Fatalf("SetPassword: %v", err)
	}
	if err := s.SetUserScope(ctx, 99, nil); !isNotFound(err) {
		t.Fatalf("SetUserScope: %v", err)
	}
}

func TestTheDocumentedLifetimesHold(t *testing.T) {
	checks := []struct {
		name      string
		got, want int64
	}{
		{"session ttl", SessionTTLSecs, 7 * 24 * 3600},
		{"session idle", SessionIdleSecs, 24 * 3600},
		{"app session ttl", AppSessionTTLSecs, 24 * 3600},
		{"reauth", ReauthSecs, 10 * 60},
		{"refresh idle", refreshIdleSecs, 90 * 24 * 3600},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestANewSessionExpiresAfterItsLifetimeAndSweepsIdleOnes(t *testing.T) {
	s := openMemory(t)
	id, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	noError(t, s.CreateSession(ctx, "old", id, ""))
	exec(t, s, "UPDATE sessions SET last_seen = ?1 WHERE token_hash = 'old'", nowSecs()-SessionIdleSecs-5)

	before := nowSecs()
	noError(t, s.CreateSession(ctx, "new", id, ""))
	after := nowSecs()

	expires := queryInt(t, s, "SELECT expires FROM sessions WHERE token_hash = 'new'")
	week := int64(7 * 24 * 3600)
	if expires < before+week || expires > after+week {
		t.Fatalf("expires = %d, want within [%d, %d]", expires, before+week, after+week)
	}
	if rows := queryInt(t, s, "SELECT COUNT(*) FROM sessions WHERE token_hash = 'old'"); rows != 0 {
		t.Fatal("an idle session is deleted, not just hidden")
	}
}

func TestElevationLastsTheReauthPeriod(t *testing.T) {
	s := openMemory(t)
	id, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	noError(t, s.CreateSession(ctx, "a", id, ""))
	before := nowSecs()
	noError(t, s.SessionElevate(ctx, "a"))
	after := nowSecs()
	until := queryInt(t, s, "SELECT elevated_until FROM sessions WHERE token_hash = 'a'")
	tenMinutes := int64(10 * 60)
	if until < before+tenMinutes || until > after+tenMinutes {
		t.Fatalf("elevated_until = %d, want within [%d, %d]", until, before+tenMinutes, after+tenMinutes)
	}
}

func TestTokenUseIsRecordedAtMostOnceAMinute(t *testing.T) {
	s := openMemory(t)
	id, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	_, err = s.CreateToken(ctx, plainToken(id, "t", "h"))
	noError(t, err)
	lastUsed := func() *int64 {
		var got *int64
		noError(t, s.db.QueryRowContext(ctx, "SELECT last_used FROM tokens").Scan(&got))
		return got
	}
	setLastUsed := func(secsAgo int64) { exec(t, s, "UPDATE tokens SET last_used = ?1", nowSecs()-secsAgo) }

	if lastUsed() != nil {
		t.Fatal("last_used set before any use")
	}
	_, err = s.TokenAuth(ctx, "h")
	noError(t, err)
	if got := lastUsed(); got == nil || *got < nowSecs()-1 {
		t.Fatalf("last_used = %v", got)
	}

	setLastUsed(10)
	recent := *lastUsed()
	_, err = s.TokenAuth(ctx, "h")
	noError(t, err)
	if got := *lastUsed(); got != recent {
		t.Fatalf("a recent use was rewritten: %d != %d", got, recent)
	}

	setLastUsed(120)
	_, err = s.TokenAuth(ctx, "h")
	noError(t, err)
	if got := *lastUsed(); got < nowSecs()-1 {
		t.Fatalf("an old use was not refreshed: %d", got)
	}
}

func TestRefreshTokensIdleForNinetyDaysStopWorkingAndAreSwept(t *testing.T) {
	s := openMemory(t)
	id, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	stale, err := s.RefreshCreate(ctx, id, "c", "stale")
	noError(t, err)
	live, err := s.RefreshCreate(ctx, id, "c", "live")
	noError(t, err)
	if _, _, err := s.RefreshLookup(ctx, "stale", "c"); err != nil {
		t.Fatalf("creating another one keeps the live ones: %v", err)
	}
	noError(t, s.CreateAppToken(ctx, NewAppToken{UserID: id, Name: "app", TokenHash: "access", Expires: nowSecs() + 60, RefreshID: stale}))
	noError(t, s.CreateAppToken(ctx, NewAppToken{UserID: id, Name: "live app", TokenHash: "live access", Expires: nowSecs() + 60, RefreshID: live}))

	// minting the access token counted as a use, so age both columns
	exec(t, s, "UPDATE refresh_tokens SET created = ?1, last_used = ?1 WHERE id = ?2", nowSecs()-refreshIdleSecs-5, stale)
	if _, _, err := s.RefreshLookup(ctx, "stale", "c"); !isNotFound(err) {
		t.Fatalf("stale lookup: %v", err)
	}
	gotRefresh, gotUser, err := s.RefreshLookup(ctx, "live", "c")
	if err != nil || gotRefresh != live || gotUser != id {
		t.Fatalf("live lookup = %d, %d, %v", gotRefresh, gotUser, err)
	}

	// the next login sweeps the stale one, and the access token minted from it
	_, err = s.RefreshCreate(ctx, id, "c", "another")
	noError(t, err)
	if rows := queryInt(t, s, "SELECT COUNT(*) FROM refresh_tokens"); rows != 2 {
		t.Fatalf("refresh rows = %d", rows)
	}
	if rows := queryInt(t, s, "SELECT COUNT(*) FROM tokens"); rows != 1 {
		t.Fatalf("token rows = %d, only the stale one's access token goes", rows)
	}
}

func TestInTxCommitsOnSuccessAndRollsBackOnFailure(t *testing.T) {
	s := openMemory(t)
	insertUser := func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO users (username, password_hash, created) VALUES ('tx-user', 'h', 0)")
		return err
	}
	errBoom := errors.New("boom")

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := insertUser(tx); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("inTx error = %v, want the callback's error", err)
	}
	if n := queryInt(t, s, "SELECT COUNT(*) FROM users WHERE username = 'tx-user'"); n != 0 {
		t.Fatalf("users after rollback = %d", n)
	}

	noError(t, s.inTx(ctx, insertUser))
	if n := queryInt(t, s, "SELECT COUNT(*) FROM users WHERE username = 'tx-user'"); n != 1 {
		t.Fatalf("users after commit = %d", n)
	}
}

func TestInTxReportsAFailedBegin(t *testing.T) {
	s := openMemory(t)
	noError(t, s.Close())
	called := false
	err := s.inTx(ctx, func(*sql.Tx) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("inTx on a closed database succeeded")
	}
	if called {
		t.Fatal("inTx ran the callback without a transaction")
	}
}

func TestCreateAppTokenRollsBackWhenAStatementFails(t *testing.T) {
	cases := []struct{ name, trigger string }{
		{"insert", "BEFORE INSERT ON tokens"},
		{"delete of the older token", "BEFORE DELETE ON tokens"},
		{"refresh token update", "BEFORE UPDATE ON refresh_tokens"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openMemory(t)
			id, err := s.CreateUser(ctx, "u", "h", false)
			noError(t, err)
			refresh, err := s.RefreshCreate(ctx, id, "c", "refresh")
			noError(t, err)
			noError(t, s.CreateAppToken(ctx, NewAppToken{UserID: id, Name: "app", TokenHash: "old", Expires: nowSecs() + 60, RefreshID: refresh}))
			exec(t, s, "UPDATE refresh_tokens SET last_used = 1")
			exec(t, s, "CREATE TRIGGER fail "+c.trigger+" BEGIN SELECT RAISE(ABORT, 'forced'); END")

			err = s.CreateAppToken(ctx, NewAppToken{UserID: id, Name: "app", TokenHash: "new", Expires: nowSecs() + 60, RefreshID: refresh})
			if err == nil {
				t.Fatal("CreateAppToken succeeded")
			}
			if n := queryInt(t, s, "SELECT COUNT(*) FROM tokens WHERE token_hash = 'old'"); n != 1 {
				t.Fatalf("old token rows = %d, want 1", n)
			}
			if n := queryInt(t, s, "SELECT COUNT(*) FROM tokens WHERE token_hash = 'new'"); n != 0 {
				t.Fatalf("new token rows = %d, want 0", n)
			}
			if n := queryInt(t, s, "SELECT last_used FROM refresh_tokens"); n != 1 {
				t.Fatalf("last_used = %d, want it untouched", n)
			}
		})
	}
}

func TestEndingAppLoginsRollsBackWhenAStatementFails(t *testing.T) {
	ends := map[string]func(*Store, int64, int64) error{
		"one login": func(s *Store, _, refresh int64) error { return s.refreshDelete(ctx, refresh) },
		"all of a user's logins": func(s *Store, user, _ int64) error {
			return s.DeleteUserRefreshTokens(ctx, user)
		},
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) {
			s := openMemory(t)
			id, err := s.CreateUser(ctx, "u", "h", false)
			noError(t, err)
			refresh, err := s.RefreshCreate(ctx, id, "c", "refresh")
			noError(t, err)
			noError(t, s.CreateAppToken(ctx, NewAppToken{UserID: id, Name: "app", TokenHash: "access", Expires: nowSecs() + 60, RefreshID: refresh}))
			exec(t, s, "CREATE TRIGGER fail BEFORE DELETE ON refresh_tokens BEGIN SELECT RAISE(ABORT, 'forced'); END")

			if err := end(s, id, refresh); err == nil {
				t.Fatal("ending the login succeeded")
			}
			if n := queryInt(t, s, "SELECT COUNT(*) FROM tokens"); n != 1 {
				t.Fatalf("access tokens = %d, the earlier delete must roll back", n)
			}
			if n := queryInt(t, s, "SELECT COUNT(*) FROM refresh_tokens"); n != 1 {
				t.Fatalf("refresh tokens = %d, want 1", n)
			}
		})
	}
}

func TestAWebhookMarksTheRegistrationSeenAtMostOnceAMinute(t *testing.T) {
	s := openMemory(t)
	id, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	reg, err := s.RegistrationCreate(ctx, NewRegistration{
		UserID:      id,
		WebhookHash: "hook",
		Fields: RegistrationFields{
			AppID: "app", AppName: "App", AppVersion: "1", DeviceName: "phone",
			Manufacturer: "m", Model: "x", OSName: "os",
		},
	})
	noError(t, err)
	lastSeen := func() *int64 {
		var got *int64
		noError(t, s.db.QueryRowContext(ctx, "SELECT last_seen FROM registrations WHERE id = ?1", reg).Scan(&got))
		return got
	}
	setLastSeen := func(at int64) { exec(t, s, "UPDATE registrations SET last_seen = ?1 WHERE id = ?2", at, reg) }

	if lastSeen() != nil {
		t.Fatal("last_seen set before any call")
	}
	_, err = s.RegistrationByWebhook(ctx, "hook")
	noError(t, err)
	if lastSeen() == nil {
		t.Fatal("the first call marks it seen")
	}

	recent := nowSecs() - 30
	setLastSeen(recent)
	_, err = s.RegistrationByWebhook(ctx, "hook")
	noError(t, err)
	if got := *lastSeen(); got != recent {
		t.Fatalf("seen 30 s ago must be left alone: %d != %d", got, recent)
	}

	setLastSeen(nowSecs() - 120)
	_, err = s.RegistrationByWebhook(ctx, "hook")
	noError(t, err)
	if got := *lastSeen(); got < nowSecs()-5 {
		t.Fatalf("seen 2 min ago must be refreshed: %d", got)
	}
}
