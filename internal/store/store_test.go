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

// must returns the value or panics, which fails the test with a stack trace.
func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
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

func queryInt(t *testing.T, s *Store, query string, args ...any) int64 {
	t.Helper()
	var n int64
	noError(t, s.db.QueryRowContext(ctx, query, args...).Scan(&n))
	return n
}

// deleteUser removes the user even when they are the last admin (tests only:
// the server refuses that, see DeleteUserKeepingAnAdmin).
func deleteUser(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	result, err := s.removeUser(ctx, id, false)
	noError(t, err)
	return result == UserDeleted
}

func TestFirstAdminOnlyWhileNoUserExists(t *testing.T) {
	s := openMemory(t)
	if n := must(s.UserCount(ctx)); n != 0 {
		t.Fatalf("user count = %d", n)
	}
	id := must(s.CreateFirstAdmin(ctx, "Root", "a"))
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
	admin := must(s.CreateFirstAdmin(ctx, "admin", "a"))
	bob := must(s.CreateUser(ctx, "bob", "b", false))
	if _, err := s.CreateUser(ctx, "BOB", "x", false); err == nil {
		t.Fatal("names are case-insensitive")
	}
	if n := must(s.AdminCount(ctx)); n != 1 {
		t.Fatalf("admins = %d", n)
	}
	noError(t, s.SetAdmin(ctx, bob, true))
	if n := must(s.AdminCount(ctx)); n != 2 {
		t.Fatalf("admins = %d", n)
	}
	noError(t, s.SetPassword(ctx, bob, "new"))
	if hash := must(s.UserHash(ctx, bob)); hash != "new" {
		t.Fatalf("hash = %q", hash)
	}
	if n := len(must(s.ListUsers(ctx))); n != 2 {
		t.Fatalf("users = %d", n)
	}

	noError(t, s.CreateSession(ctx, "sb", bob, ""))
	must(s.CreateToken(ctx, plainToken(bob, "watch", "hb")))
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
	if users := must(s.ListUsers(ctx)); users[0].ID != admin {
		t.Fatalf("users = %+v", users)
	}
}

func TestTokenLifecycle(t *testing.T) {
	s := openMemory(t)
	a := must(s.CreateUser(ctx, "a", "x", false))
	b := must(s.CreateUser(ctx, "b", "x", false))
	id := must(s.CreateToken(ctx, plainToken(a, "watch", "h1")))
	must(s.CreateToken(ctx, plainToken(b, "other", "h3")))
	if got := must(s.ListTokens(ctx, &a))[0].LastUsed; got != nil {
		t.Fatalf("last_used = %v before any use", *got)
	}
	must(s.TokenAuth(ctx, "h1"))
	if got := must(s.ListTokens(ctx, &a))[0].LastUsed; got == nil {
		t.Fatal("use not recorded")
	}
	if _, err := s.TokenAuth(ctx, "h2"); !isNotFound(err) {
		t.Fatalf("unknown token: %v", err)
	}
	if n := len(must(s.ListTokens(ctx, &a))); n != 1 {
		t.Fatalf("a's tokens = %d", n)
	}
	if name := must(s.ListTokens(ctx, &a))[0].Name; name != "watch" {
		t.Fatalf("name = %q", name)
	}
	all := must(s.ListTokens(ctx, nil))
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
	must(s.CreateToken(ctx, plainToken(1, "open", "h-open")))
	scoped := plainToken(1, "scoped", "h-scoped")
	scoped.Scope = scopeOf("a", "b")
	must(s.CreateToken(ctx, scoped))
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
	list := must(s.ListTokens(ctx, nil))
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
	u := must(s.CreateUser(ctx, "u", "h", false))
	ro := plainToken(u, "ro", "h-ro")
	ro.ReadOnly = true
	must(s.CreateToken(ctx, ro))
	live := plainToken(u, "live", "h-live")
	live.Scope = scopeOf("a", "b")
	live.Expires = ptr(now + 100)
	must(s.CreateToken(ctx, live))
	dead := plainToken(u, "dead", "h-dead")
	dead.Expires = ptr(now - 1)
	must(s.CreateToken(ctx, dead))

	if !must(s.TokenAuth(ctx, "h-ro")).ReadOnly {
		t.Fatal("h-ro must be read-only")
	}
	if must(s.TokenAuth(ctx, "h-live")).ReadOnly {
		t.Fatal("h-live must not be read-only")
	}
	if _, err := s.TokenAuth(ctx, "h-dead"); !isNotFound(err) {
		t.Fatalf("expired token: %v", err)
	}
	list := must(s.ListTokens(ctx, nil))
	if list[1].Expires == nil || *list[1].Expires != now+100 {
		t.Fatalf("expires = %v", list[1].Expires)
	}
	if !list[0].ReadOnly || list[0].Expires != nil {
		t.Fatalf("first token = %+v", list[0])
	}

	// shrinking the owner's groups shrinks the token with it
	noError(t, s.SetUserScope(ctx, u, scopeOf("b")))
	if got := must(s.TokenAuth(ctx, "h-live")).Scope; !reflect.DeepEqual(got, scopeOf("b")) {
		t.Fatalf("live scope = %#v", got)
	}
	if got := must(s.TokenAuth(ctx, "h-ro")).Scope; !reflect.DeepEqual(got, scopeOf("b")) {
		t.Fatalf("ro scope = %#v", got)
	}
	noError(t, s.SetUserScope(ctx, u, nil))
	if got := must(s.TokenAuth(ctx, "h-live")).Scope; !reflect.DeepEqual(got, scopeOf("a", "b")) {
		t.Fatalf("live scope after unrestricting = %#v", got)
	}
	if got := must(s.ListUsers(ctx))[0].Scope; got != nil {
		t.Fatalf("user scope = %#v", got)
	}

	// creating a token tidies expired ones away
	must(s.CreateToken(ctx, plainToken(u, "new", "h-new")))
	if n := len(must(s.ListTokens(ctx, nil))); n != 3 {
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
	a := must(s.CreateUser(ctx, "a", "h", true))
	b := must(s.CreateUser(ctx, "b", "h", true))

	// two admins demoting each other: exactly one demotion goes through
	errs := concurrently(func() error { return s.SetAdmin(ctx, a, false) }, func() error { return s.SetAdmin(ctx, b, false) })
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("exactly one demotion may succeed: %v", errs)
	}
	if n := must(s.AdminCount(ctx)); n != 1 {
		t.Fatalf("admins = %d", n)
	}
	last, demoted := a, b
	if errs[0] == nil {
		last, demoted = b, a
	}
	if err := s.SetAdmin(ctx, last, false); !isNotFound(err) {
		t.Fatalf("demoting the last admin: %v", err)
	}
	if n := must(s.AdminCount(ctx)); n != 1 {
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
	if n := must(s.AdminCount(ctx)); n != 1 {
		t.Fatalf("admins = %d", n)
	}
	if result := must(s.DeleteUserKeepingAnAdmin(ctx, 999)); result != UserNotFound {
		t.Fatalf("unknown user: %v", result)
	}

	// regular users are always deletable, and their data goes with them
	c := must(s.CreateUser(ctx, "c", "h", false))
	noError(t, s.CreateSession(ctx, "sc", c, ""))
	if result := must(s.DeleteUserKeepingAnAdmin(ctx, c)); result != UserDeleted {
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
	id := must(s.CreateUser(ctx, "u", "h", false))
	refresh := must(s.RefreshCreate(ctx, id, "client", "r1"))
	noError(t, s.CreateAppSession(ctx, "a", id, refresh, "ua"))
	must(s.SessionUser(ctx, "a"))
	if !must(s.SessionNeedsReauth(ctx, "a")) {
		t.Fatal("a fresh app session needs a password re-entry")
	}
	noError(t, s.SessionElevate(ctx, "a"))
	if must(s.SessionNeedsReauth(ctx, "a")) {
		t.Fatal("an elevated session needs no re-entry")
	}

	// a second exchange for the same app login replaces the first session
	noError(t, s.CreateAppSession(ctx, "b", id, refresh, "ua"))
	if _, err := s.SessionUser(ctx, "a"); !isNotFound(err) {
		t.Fatalf("replaced session: %v", err)
	}
	must(s.SessionUser(ctx, "b"))
	if n := len(must(s.ListSessions(ctx, id, "b"))); n != 1 {
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
	if valid := must(s.SessionValid(ctx, "b")); valid {
		t.Fatal("session without its login is valid")
	}
	if n := len(must(s.ListSessions(ctx, id, "b"))); n != 0 {
		t.Fatalf("sessions = %d", n)
	}

	// password sessions never need a re-entry
	noError(t, s.CreateSession(ctx, "p", id, "ua"))
	if must(s.SessionNeedsReauth(ctx, "p")) {
		t.Fatal("password sessions never need a re-entry")
	}
}

func TestIdleSessionsExpireAndActivityIsThrottled(t *testing.T) {
	s := openMemory(t)
	id := must(s.CreateUser(ctx, "u", "h", false))
	noError(t, s.CreateSession(ctx, "a", id, strings.Repeat("x", 500)))
	list := must(s.ListSessions(ctx, id, "a"))
	if len(list) != 1 || !list[0].Current {
		t.Fatalf("sessions = %+v", list)
	}
	if len(list[0].UserAgent) != 200 {
		t.Fatalf("user agent is %d long, want it truncated to 200", len(list[0].UserAgent))
	}

	setSeen := func(secsAgo int64) { exec(t, s, "UPDATE sessions SET last_seen = ?1", nowSecs()-secsAgo) }
	seen := func() int64 { return must(s.ListSessions(ctx, id, "a"))[0].LastSeen }

	// a recent request does not write again
	setSeen(10)
	before := seen()
	must(s.SessionUser(ctx, "a"))
	if seen() != before {
		t.Fatal("a recent request rewrote last_seen")
	}
	// after a minute it is refreshed
	setSeen(120)
	must(s.SessionUser(ctx, "a"))
	if seen() < nowSecs()-1 {
		t.Fatal("last_seen was not refreshed")
	}
	// checking validity never counts as activity
	setSeen(120)
	if !must(s.SessionValid(ctx, "a")) {
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
	if must(s.SessionValid(ctx, "a")) {
		t.Fatal("idle session is valid")
	}
	if n := len(must(s.ListSessions(ctx, id, "a"))); n != 0 {
		t.Fatalf("idle session listed: %d", n)
	}
	noError(t, s.CreateSession(ctx, "b", id, ""))
	if n := len(must(s.ListSessions(ctx, id, "b"))); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := openMemory(t)
	id := must(s.CreateUser(ctx, "u", "h", false))
	if _, err := s.SessionUser(ctx, "x"); !isNotFound(err) {
		t.Fatalf("unknown session: %v", err)
	}
	noError(t, s.CreateSession(ctx, "x", id, ""))
	if user := must(s.SessionUser(ctx, "x")); user.Username != "u" {
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
	exists := func(name string) bool { return must(s.GroupExists(ctx, name)) }
	exposed := func(name string) bool {
		got, err := s.GroupExposed(ctx, name)
		if err != nil && !isNotFound(err) {
			t.Fatal(err)
		}
		return got
	}
	members := func(name string) []string { return must(s.GroupMembers(ctx, name)) }
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
	if got := must(s.GroupNames(ctx)); !reflect.DeepEqual(got, []string{"garmin"}) {
		t.Fatalf("names = %v", got)
	}
	if exposed("garmin") {
		t.Fatal("a new group is not exposed")
	}
	noError(t, s.GroupSetExposed(ctx, "garmin", true))
	if !exposed("garmin") {
		t.Fatal("group should be exposed")
	}
	if got := must(s.ExposedGroupNames(ctx)); !reflect.DeepEqual(got, []string{"garmin"}) {
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
	if got := must(s.HueGet(ctx)); got != bridge {
		t.Fatalf("bridge = %+v", got)
	}
	noError(t, s.HueSet(ctx, HueBridge{IP: "10.0.0.9", Key: "k2"}))
	if got := must(s.HueGet(ctx)); got.IP != "10.0.0.9" {
		t.Fatalf("bridge was not replaced: %+v", got)
	}
}

func TestLegacyGroupsTableIsMigrated(t *testing.T) {
	db, err := openMemoryPool(ctx)
	noError(t, err)
	_, err = db.ExecContext(ctx, `CREATE TABLE groups (name TEXT PRIMARY KEY);
	    INSERT INTO groups (name) VALUES ('old');`)
	noError(t, err)
	s, err := initStore(ctx, db)
	noError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	if !must(s.GroupExists(ctx, "old")) {
		t.Fatal("old group lost")
	}
	if must(s.GroupExposed(ctx, "old")) {
		t.Fatal("an old group is not exposed")
	}
	noError(t, s.GroupSetExposed(ctx, "old", true))
}

func TestV010DatabaseKeepsItsData(t *testing.T) {
	db, err := openMemoryPool(ctx)
	noError(t, err)
	_, err = db.ExecContext(ctx, `CREATE TABLE owner (id INTEGER PRIMARY KEY CHECK (id = 1), password_hash TEXT NOT NULL);
	    CREATE TABLE sessions (token_hash TEXT PRIMARY KEY, expires INTEGER NOT NULL);
	    CREATE TABLE tokens (
	        id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL,
	        token_hash TEXT NOT NULL UNIQUE, created INTEGER NOT NULL, last_used INTEGER);
	    CREATE TABLE hue (id INTEGER PRIMARY KEY CHECK (id = 1), ip TEXT NOT NULL, app_key TEXT NOT NULL);
	    CREATE TABLE groups (name TEXT PRIMARY KEY, expose_light INTEGER NOT NULL DEFAULT 0);
	    CREATE TABLE group_members (
	        group_name TEXT NOT NULL REFERENCES groups(name) ON DELETE CASCADE,
	        entity_id TEXT NOT NULL, pos INTEGER NOT NULL, PRIMARY KEY (group_name, entity_id));
	    INSERT INTO owner VALUES (1, 'hash');
	    INSERT INTO tokens (name, token_hash, created) VALUES ('watch', 'th', 1);
	    INSERT INTO hue VALUES (1, '10.0.0.2', 'key');
	    INSERT INTO groups VALUES ('garmin', 1);
	    INSERT INTO group_members VALUES ('garmin', 'light.a', 0);`)
	noError(t, err)
	s, err := initStore(ctx, db)
	noError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	admin, hash, err := s.UserLogin(ctx, "admin")
	noError(t, err)
	if admin.ID != 1 || !admin.IsAdmin || hash != "hash" {
		t.Fatalf("admin = %+v hash = %q", admin, hash)
	}
	one := int64(1)
	if n := len(must(s.ListTokens(ctx, &one))); n != 1 {
		t.Fatalf("token now belongs to the admin: %d", n)
	}
	must(s.TokenAuth(ctx, "th"))
	if got := must(s.HueGet(ctx)).IP; got != "10.0.0.2" {
		t.Fatalf("hue ip = %q", got)
	}
	if !must(s.GroupExposed(ctx, "garmin")) {
		t.Fatal("garmin lost its flag")
	}
	if got := must(s.GroupMembers(ctx, "garmin")); !reflect.DeepEqual(got, []string{"light.a"}) {
		t.Fatalf("members = %v", got)
	}
}

func TestFileDatabaseReopensWithoutRerunningMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "domus.db")
	s, err := Open(ctx, path)
	noError(t, err)
	must(s.CreateFirstAdmin(ctx, "root", "a"))
	noError(t, s.Close())
	s, err = Open(ctx, path)
	noError(t, err)
	defer func() { _ = s.Close() }()
	if hash := must(s.UserHash(ctx, 1)); hash != "a" {
		t.Fatalf("hash = %q", hash)
	}
}

func TestUserCountCountsUsers(t *testing.T) {
	s := openMemory(t)
	must(s.CreateFirstAdmin(ctx, "a", "h"))
	must(s.CreateUser(ctx, "b", "h", false))
	if n := must(s.UserCount(ctx)); n != 2 {
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
	id := must(s.CreateUser(ctx, "u", "h", false))
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
	id := must(s.CreateUser(ctx, "u", "h", false))
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
	id := must(s.CreateUser(ctx, "u", "h", false))
	must(s.CreateToken(ctx, plainToken(id, "t", "h")))
	lastUsed := func() *int64 {
		var got *int64
		noError(t, s.db.QueryRowContext(ctx, "SELECT last_used FROM tokens").Scan(&got))
		return got
	}
	setLastUsed := func(secsAgo int64) { exec(t, s, "UPDATE tokens SET last_used = ?1", nowSecs()-secsAgo) }

	if lastUsed() != nil {
		t.Fatal("last_used set before any use")
	}
	must(s.TokenAuth(ctx, "h"))
	if got := lastUsed(); got == nil || *got < nowSecs()-1 {
		t.Fatalf("last_used = %v", got)
	}

	setLastUsed(10)
	recent := *lastUsed()
	must(s.TokenAuth(ctx, "h"))
	if got := *lastUsed(); got != recent {
		t.Fatalf("a recent use was rewritten: %d != %d", got, recent)
	}

	setLastUsed(120)
	must(s.TokenAuth(ctx, "h"))
	if got := *lastUsed(); got < nowSecs()-1 {
		t.Fatalf("an old use was not refreshed: %d", got)
	}
}

func TestRefreshTokensIdleForNinetyDaysStopWorkingAndAreSwept(t *testing.T) {
	s := openMemory(t)
	id := must(s.CreateUser(ctx, "u", "h", false))
	stale := must(s.RefreshCreate(ctx, id, "c", "stale"))
	live := must(s.RefreshCreate(ctx, id, "c", "live"))
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
	must(s.RefreshCreate(ctx, id, "c", "another"))
	if rows := queryInt(t, s, "SELECT COUNT(*) FROM refresh_tokens"); rows != 2 {
		t.Fatalf("refresh rows = %d", rows)
	}
	if rows := queryInt(t, s, "SELECT COUNT(*) FROM tokens"); rows != 1 {
		t.Fatalf("token rows = %d, only the stale one's access token goes", rows)
	}
}

func TestAWebhookMarksTheRegistrationSeenAtMostOnceAMinute(t *testing.T) {
	s := openMemory(t)
	id := must(s.CreateUser(ctx, "u", "h", false))
	reg := must(s.RegistrationCreate(ctx, NewRegistration{
		UserID:      id,
		WebhookHash: "hook",
		Fields: RegistrationFields{
			AppID: "app", AppName: "App", AppVersion: "1", DeviceName: "phone",
			Manufacturer: "m", Model: "x", OSName: "os",
		},
	}))
	lastSeen := func() *int64 {
		var got *int64
		noError(t, s.db.QueryRowContext(ctx, "SELECT last_seen FROM registrations WHERE id = ?1", reg).Scan(&got))
		return got
	}
	setLastSeen := func(at int64) { exec(t, s, "UPDATE registrations SET last_seen = ?1 WHERE id = ?2", at, reg) }

	if lastSeen() != nil {
		t.Fatal("last_seen set before any call")
	}
	must(s.RegistrationByWebhook(ctx, "hook"))
	if lastSeen() == nil {
		t.Fatal("the first call marks it seen")
	}

	recent := nowSecs() - 30
	setLastSeen(recent)
	must(s.RegistrationByWebhook(ctx, "hook"))
	if got := *lastSeen(); got != recent {
		t.Fatalf("seen 30 s ago must be left alone: %d != %d", got, recent)
	}

	setLastSeen(nowSecs() - 120)
	must(s.RegistrationByWebhook(ctx, "hook"))
	if got := *lastSeen(); got < nowSecs()-5 {
		t.Fatalf("seen 2 min ago must be refreshed: %d", got)
	}
}
