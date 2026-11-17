package e2e

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/henry40408/domus/e2e/internal/harness"
)

// world is the state shared by the steps of one scenario: a server, and a
// browser opened by the step that needs it.
type world struct {
	server  *harness.Server
	browser *harness.Browser
	// token is the access token a scenario saw on the page.
	token string
}

// initializeScenario runs once per scenario, so every scenario gets its own
// world through the closure.
func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}
	sc.After(w.cleanUp)
	registerSteps(sc, w)
}

// cleanUp runs on every outcome, so a failed scenario leaves no Chrome and no
// server behind.
func (w *world) cleanUp(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
	if w.browser != nil {
		w.browser.Close()
		w.browser = nil
	}
	if w.server != nil {
		w.server.Stop()
		w.server = nil
	}
	return ctx, nil
}

// start boots the server and the browser of the scenario.
func (w *world) start() error {
	server, err := harness.Start()
	if err != nil {
		return err
	}
	w.server = server
	browser, err := harness.Open()
	if err != nil {
		return fmt.Errorf("could not open a browser session: %w", err)
	}
	w.browser = browser
	return nil
}

// app is the page object of the scenario's browser.
func (w *world) app() (*harness.App, error) {
	if w.browser == nil {
		return nil, fmt.Errorf("no browser session: the scenario has no server yet")
	}
	return w.browser.App(w.server), nil
}
