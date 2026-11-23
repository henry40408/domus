// Command screenshots regenerates the README screenshots into screenshots/.
//
//	go build -o bin/domus ./cmd/domus && cd e2e && go run ./cmd/screenshots
//
// It starts domus against a fake Hue bridge, so no real house or private names
// end up in the images. Font rendering and Chromium differ by machine: an
// unrelated image changing in the diff means the browser changed, not the UI.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/henry40408/domus/e2e/internal/harness"
)

const (
	username  = "demo"
	password  = "demo-password-123"
	groupName = "Garmin"
)

// shot is one screen: file stem, route, navigation tab to wait for, viewport size.
type shot struct {
	stem, route, tab string
	width, height    int64
}

var shots = []shot{
	{"dashboard", "#dashboard", "Dashboard", 1280, 900},
	{"groups", "#groups", "Groups", 1280, 900},
	{"dashboard-mobile", "#dashboard", "Dashboard", 390, 844},
}

// groupMembers are the lights the demo group holds, by the end of their names.
var groupMembers = []string{"Ceiling", "Floor lamp", "Desk lamp", "Relax", "Nightlight"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "screenshots: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	out := filepath.Join(harness.RepoRoot(), "screenshots")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	server, err := harness.Start()
	if err != nil {
		return err
	}
	defer server.Stop()
	if err := seed(server); err != nil {
		return err
	}
	for _, theme := range []string{"light", "dark"} {
		if err := capture(server, theme, out); err != nil {
			return err
		}
	}
	fmt.Printf("screenshots: wrote %d images to %s\n", len(shots)*2, out)
	return nil
}

// seed sets up the admin, pairs the fake bridge and builds the demo group, all
// over the API.
func seed(server *harness.Server) error {
	api := server.API()
	if err := api.SetupAdmin(); err != nil {
		return fmt.Errorf("creating the admin: %w", err)
	}
	if err := api.PairBridge(); err != nil {
		return fmt.Errorf("pairing the fake bridge: %w", err)
	}
	members, err := memberIDs(api)
	if err != nil {
		return err
	}
	_, err = api.Do(harness.Request{
		Method: "PUT", Path: "/groups/" + groupName,
		Body: map[string]any{"members": members, "expose_light": true},
	})
	if err != nil {
		return fmt.Errorf("creating the demo group: %w", err)
	}
	return nil
}

func memberIDs(api *harness.API) ([]string, error) {
	entities, err := api.Do(harness.Request{Method: "GET", Path: "/lights"})
	if err != nil {
		return nil, err
	}
	list, _ := entities.([]any)
	var members []string
	for _, wanted := range groupMembers {
		for _, item := range list {
			entity, _ := item.(map[string]any)
			name, _ := entity["name"].(string)
			id, _ := entity["entity_id"].(string)
			if strings.HasSuffix(name, wanted) {
				members = append(members, id)
				break
			}
		}
	}
	if len(members) < len(groupMembers) {
		return nil, fmt.Errorf("the fake bridge did not sync: only %d group members found", len(members))
	}
	return members, nil
}

func capture(server *harness.Server, theme, out string) error {
	browser, err := harness.Open()
	if err != nil {
		return err
	}
	defer browser.Close()
	if err := browser.EmulateColorScheme(theme); err != nil {
		return err
	}
	app := browser.App(server)
	if err := logIn(app); err != nil {
		return err
	}
	suffix := ""
	if theme == "dark" {
		suffix = "-dark"
	}
	for _, s := range shots {
		png, err := screenshot(app, s)
		if err != nil {
			return fmt.Errorf("%s (%s): %w", s.stem, theme, err)
		}
		if err := os.WriteFile(filepath.Join(out, s.stem+suffix+".png"), png, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// logIn signs in through the real form once; the session cookie covers the
// rest.
func logIn(app *harness.App) error {
	if err := app.Open("/"); err != nil {
		return err
	}
	if err := app.Fill("Username", username); err != nil {
		return err
	}
	if err := app.Fill("Password", password); err != nil {
		return err
	}
	return app.Click(harness.Control{Selector: "button", Text: "Log in", TookEffect: func() (bool, error) {
		return app.Exists("nav")
	}})
}

func screenshot(app *harness.App, s shot) ([]byte, error) {
	if err := app.Browser.SetViewport(s.width, s.height); err != nil {
		return nil, err
	}
	if err := app.Open("/" + s.route); err != nil {
		return nil, err
	}
	// Going to the URL already shown only changes the hash, so wait for the
	// navigation to mark this view as the current one.
	if err := harness.EventuallyEq("the current tab", s.tab, func() (string, error) {
		return app.Text("nav.side a[aria-current=page]")
	}); err != nil {
		return nil, err
	}
	// Let the page settle: live state and the watch preview paint after the shell.
	time.Sleep(800 * time.Millisecond)
	// The build's git version would change every image on every commit. Set
	// through the element's style, since the page's CSP refuses an injected
	// <style>; done last because a re-render replaces the element.
	if _, err := harness.Eval[bool](app.Browser, `(() => {
		document.querySelectorAll(".version").forEach((e) => { e.style.display = "none"; });
		return true;
	})()`); err != nil {
		return nil, err
	}
	return app.Browser.Screenshot()
}
