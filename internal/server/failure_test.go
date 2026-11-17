package server

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henry40408/domus/internal/core"
	"github.com/henry40408/domus/internal/hue"
	"github.com/henry40408/domus/internal/store"
)

// damagedApp is an app on a database file that a second connection can break
// after the setup, so one query fails while the login and the session checks
// still work.
type damagedApp struct {
	*testApp
	logs *logBuffer
	raw  *sql.DB
}

func newDamageableApp(t *testing.T) *damagedApp {
	t.Helper()
	path := filepath.Join(t.TempDir(), "domus.db")
	st, err := store.Open(context.Background(), path)
	noError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	raw, err := sql.Open("sqlite", path)
	noError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	logs := &logBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	c := core.New(st, logger)
	app := New(Config{Core: c, Hue: hue.NewManager(c, hue.Timing{}, nil), Version: goldenVersion, Logger: logger})
	return &damagedApp{testApp: &testApp{app: app, store: st, handler: app.Handler()}, logs: logs, raw: raw}
}

func (d *damagedApp) damage(t *testing.T, statement string) {
	t.Helper()
	if _, err := d.raw.Exec(statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

func (d *damagedApp) wantLogged(t *testing.T, message string) {
	t.Helper()
	if !strings.Contains(d.logs.String(), message) {
		t.Errorf("no %q in the log:\n%s", message, d.logs)
	}
}

func (d *damagedApp) wantQuiet(t *testing.T) {
	t.Helper()
	for _, line := range strings.Split(d.logs.String(), "\n") {
		if line != "" && !strings.Contains(line, "target=audit") {
			t.Errorf("unexpected log line: %s", line)
		}
	}
}

const refuseWrites = "BEGIN SELECT RAISE(ABORT, 'refused'); END"

func TestAFailingStoreIsLoggedWhereTheAnswerStaysUsable(t *testing.T) {
	tests := []struct {
		name    string
		damage  string
		method  string
		path    string
		body    string
		message string
	}{
		{"apps", "DROP TABLE registrations", "GET", "/api/domus/apps", "", "listing app devices failed"},
		{"groups", "DROP TABLE groups", "GET", "/api/domus/groups", "", "listing groups failed"},
		{"group members", "DROP TABLE group_members", "GET", "/api/domus/groups", "", "group members lookup failed"},
		{"hue", "DROP TABLE hue", "GET", "/api/domus/hue", "", "reading the paired bridge failed"},
		{"tokens", "DROP TABLE tokens", "GET", "/api/domus/tokens", "", "listing tokens failed"},
		{"scoped token", "DROP TABLE groups", "POST", "/api/domus/tokens", `{"name":"t","scope":["g"]}`, "group lookup failed"},
		{"logout", "CREATE TRIGGER x BEFORE DELETE ON sessions " + refuseWrites, "POST", "/api/domus/logout", "", "deleting the session failed"},
		{"other sessions", "CREATE TRIGGER x BEFORE DELETE ON sessions " + refuseWrites, "POST", "/api/domus/sessions/revoke-others", "", "deleting other sessions failed"},
		{"expose flag", "CREATE TRIGGER x BEFORE UPDATE ON groups " + refuseWrites, "PUT", "/api/domus/groups/g", `{"members":[],"expose_light":true}`, "setting the group light flag failed"},
		{"delete user", "CREATE TRIGGER x BEFORE DELETE ON users WHEN OLD.id = 2 " + refuseWrites, "DELETE", "/api/domus/users/2", "", "deleting a user failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newDamageableApp(t)
			cookie := app.setupAdmin(t)
			app.addMember(t, "bob", "long enough pw12")
			app.setGroup(t, "g", "light.a")
			app.login(t, "admin", "correct horse")
			app.damage(t, tt.damage)
			app.logs.Reset()
			app.adminDo(t, tt.method, tt.path, cookie, tt.body)
			app.wantLogged(t, tt.message)
		})
	}
}

func TestAHealthyStoreLogsNothingWhileTheAdminPageIsServed(t *testing.T) {
	app := newDamageableApp(t)
	cookie := app.setupAdmin(t)
	app.addMember(t, "bob", "long enough pw12")
	app.setGroup(t, "g", "light.a")
	app.logs.Reset()
	for _, path := range []string{
		"/api/domus/status", "/api/domus/apps", "/api/domus/groups", "/api/domus/hue", "/api/domus/tokens",
		"/api/domus/sessions", "/api/domus/users",
	} {
		equal(t, app.adminStatus(t, http.MethodGet, path, cookie, ""), 200)
	}
	app.adminDo(t, http.MethodPost, "/api/domus/tokens", cookie, `{"name":"t","scope":["g"]}`)
	app.adminDo(t, http.MethodPost, "/api/domus/sessions/revoke-others", cookie, "")
	app.adminDo(t, http.MethodPut, "/api/domus/groups/g", cookie, `{"members":["light.a"],"expose_light":true}`)
	app.adminDo(t, http.MethodPost, "/api/domus/logout", cookie, "")
	app.wantQuiet(t)
}

func TestTheStatusPageLogsWhenUsersCannotBeCounted(t *testing.T) {
	app := newDamageableApp(t)
	app.damage(t, "DROP TABLE users")
	app.adminDo(t, http.MethodGet, "/api/domus/status", "", "")
	app.wantLogged(t, "counting users failed")
}

func TestAnUnpairedBridgeIsNotAnError(t *testing.T) {
	app := newDamageableApp(t)
	cookie := app.setupAdmin(t)
	app.logs.Reset()
	equal(t, app.adminStatus(t, http.MethodGet, "/api/domus/hue", cookie, ""), 200)
	app.wantQuiet(t)
}

func TestAFailedScopeChangeIsLogged(t *testing.T) {
	for name, body := range map[string]string{
		"a new limit":    `{"scope":["g"]}`,
		"a lifted limit": `{"is_admin":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			app := newDamageableApp(t)
			cookie := app.setupAdmin(t)
			bob := app.addMember(t, "bob", "long enough pw12")
			app.setGroup(t, "g", "light.a")
			app.setGroup(t, "h", "light.a")
			noError(t, app.store.SetUserScope(bg, bob, store.Scope{"h"}))
			app.damage(t, "CREATE TRIGGER x BEFORE UPDATE ON users WHEN NEW.scope IS NOT OLD.scope "+refuseWrites)
			app.logs.Reset()
			app.adminDo(t, http.MethodPut, "/api/domus/users/2", cookie, body)
			if name == "a new limit" {
				app.wantLogged(t, "setting the group limit failed")
				return
			}
			app.wantLogged(t, "lifting the group limit failed")
		})
	}
}
