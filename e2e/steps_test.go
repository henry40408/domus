package e2e

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/cucumber/godog"

	"github.com/henry40408/domus/e2e/internal/harness"
)

var tokenPattern = regexp.MustCompile(`^domus_[0-9a-f]{32}$`)

// registerSteps wires every step to the scenario. A step returns its failure;
// godog reports it against the step.
func registerSteps(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a new domus server$`, w.start)
	sc.Step(`^a domus server with the admin "([^"]*)" and the Hue bridge paired$`, w.startSeeded)
	sc.Step(`^I am logged in as "([^"]*)"$`, w.loggedInAs)
	sc.Step(`^I open the app$`, w.openTheApp)
	sc.Step(`^I should see the first-time setup form$`, w.seeSetupForm)
	sc.Step(`^I create the admin "([^"]*)" with password "([^"]*)" using the setup code$`, w.createAdmin)
	sc.Step(`^I log in as "([^"]*)" with password "([^"]*)"$`, w.logIn)
	sc.Step(`^I should see the dashboard for "([^"]*)"$`, w.seeDashboardFor)
	sc.Step(`^I should see the login form$`, w.seeLoginForm)
	sc.Step(`^I should see the toast "([^"]*)"$`, w.seeToast)
	sc.Step(`^I log out$`, w.logOut)
	sc.Step(`^I go to the "([^"]*)" tab$`, w.goToTab)
	sc.Step(`^I reload the page$`, w.reload)
	registerGroupSteps(sc, w)
	registerDeviceSteps(sc, w)
	registerTokenSteps(sc, w)
}

func registerGroupSteps(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^I name the new group "([^"]*)"$`, w.nameGroup)
	sc.Step(`^I pick the lights "([^"]*)" and "([^"]*)"$`, w.pickLights)
	sc.Step(`^I expose the group as a light$`, w.exposeGroup)
	sc.Step(`^I save the group$`, w.saveGroup)
	sc.Step(`^the group list should show "([^"]*)" with (\d+) members$`, w.groupListShows)
}

func registerDeviceSteps(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^the light "([^"]*)" should be (on|off)$`, w.lightShouldBe)
	sc.Step(`^I switch the light "([^"]*)"$`, w.switchLight)
	sc.Step(`^I activate the scene "([^"]*)"$`, w.activateScene)
}

func registerTokenSteps(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^I create the token "([^"]*)"$`, w.createToken)
	sc.Step(`^the page should show the new token$`, w.seeNewToken)
	sc.Step(`^the page should not show the new token$`, w.dontSeeNewToken)
	sc.Step(`^the token "([^"]*)" should be listed$`, w.tokenListed)
}

func (w *world) startSeeded(username string) error {
	if username != harness.AdminUsername {
		return fmt.Errorf("only the admin %q is seeded", harness.AdminUsername)
	}
	if err := w.start(); err != nil {
		return err
	}
	api := w.server.API()
	if err := api.SetupAdmin(); err != nil {
		return err
	}
	return api.PairBridge()
}

func (w *world) openTheApp() error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return app.Open("/")
}

func (w *world) seeSetupForm() error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return app.WaitForText("First-time setup")
}

func (w *world) createAdmin(username, password string) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	for placeholder, text := range map[string]string{
		"Username": username, "Password": password, "Setup code": w.server.SetupCode,
	} {
		if err := app.Fill(placeholder, text); err != nil {
			return err
		}
	}
	return app.Click(harness.Control{Selector: "button", Text: "Create admin", TookEffect: w.leftTheForm(app)})
}

func (w *world) logIn(username, password string) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	if err := app.Fill("Username", username); err != nil {
		return err
	}
	if err := app.Fill("Password", password); err != nil {
		return err
	}
	return app.Click(harness.Control{Selector: "button", Text: "Log in", TookEffect: w.answered(app)})
}

// leftTheForm is true once the shell replaced the auth form.
func (w *world) leftTheForm(app *harness.App) func() (bool, error) {
	return func() (bool, error) { return app.Exists("nav") }
}

// answered is true once the login led to the shell or to an error toast.
func (w *world) answered(app *harness.App) func() (bool, error) {
	return func() (bool, error) {
		in, err := app.Exists("nav")
		if in || err != nil {
			return in, err
		}
		toast, err := app.ToastText()
		return toast != "", err
	}
}

func (w *world) loggedInAs(username string) error {
	if username != harness.AdminUsername {
		return fmt.Errorf("only the admin %q can sign in here", harness.AdminUsername)
	}
	if err := w.openTheApp(); err != nil {
		return err
	}
	if err := w.logIn(username, harness.AdminPassword); err != nil {
		return err
	}
	return w.seeDashboardFor(username)
}

func (w *world) seeDashboardFor(username string) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return harness.EventuallyEq("the signed-in user", username, func() (string, error) {
		return app.Text(".account .who")
	})
}

func (w *world) seeLoginForm() error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return app.WaitForText("Log in to continue.")
}

func (w *world) seeToast(text string) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return app.WaitForToast(text)
}

func (w *world) logOut() error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return app.Click(harness.Control{Selector: "button", Text: "Log out", TookEffect: func() (bool, error) {
		return app.HasText("Log in to continue.")
	}})
}

func (w *world) goToTab(name string) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return app.Click(harness.Control{Selector: "nav a.tab", Text: name, TookEffect: func() (bool, error) {
		hash, err := app.Hash()
		return hash == "#"+strings.ToLower(name), err
	}})
}

func (w *world) reload() error {
	if w.browser == nil {
		return fmt.Errorf("no browser session")
	}
	return w.browser.Reload()
}

func (w *world) nameGroup(name string) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return app.Fill("Group name", name)
}

func (w *world) pickLights(first, second string) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	if err := app.CheckByLabel(first); err != nil {
		return err
	}
	return app.CheckByLabel(second)
}

func (w *world) exposeGroup() error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return app.CheckByLabel("Add an all-lights switch")
}

func (w *world) saveGroup() error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return app.Click(harness.Control{Selector: "button", Text: "Save group", TookEffect: func() (bool, error) {
		toast, err := app.ToastText()
		return strings.Contains(toast, "saved"), err
	}})
}

func (w *world) groupListShows(name string, members int) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return harness.EventuallyEq("the group list entry", fmt.Sprintf("%s %d", name, members), func() (string, error) {
		entry, err := app.Text(".glist button.active")
		return strings.Join(strings.Fields(entry), " "), err
	})
}

func (w *world) lightShouldBe(name, state string) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return harness.EventuallyEq("the switch of "+name, state == "on", func() (bool, error) {
		return app.SwitchIsOn(name)
	})
}

func (w *world) switchLight(name string) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return app.ClickSwitch(name)
}

func (w *world) activateScene(name string) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return app.ActivateScene(name)
}

func (w *world) createToken(name string) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	if err := app.Fill("Token name", name); err != nil {
		return err
	}
	return app.Click(harness.Control{Selector: "button", Text: "Create token", TookEffect: func() (bool, error) {
		return app.Exists("code.tok")
	}})
}

// newToken is the token the page shows, once.
func (w *world) newToken(app *harness.App) (string, error) {
	var token string
	err := harness.Eventually("the new token", func() (bool, error) {
		var err error
		token, err = app.Text("code.tok")
		return token != "", err
	})
	return token, err
}

func (w *world) seeNewToken() error {
	app, err := w.app()
	if err != nil {
		return err
	}
	token, err := w.newToken(app)
	if err != nil {
		return err
	}
	if !tokenPattern.MatchString(token) {
		return fmt.Errorf("the page shows %q, which is not a domus token", token)
	}
	w.token = token
	return nil
}

func (w *world) dontSeeNewToken() error {
	app, err := w.app()
	if err != nil {
		return err
	}
	if w.token == "" {
		return fmt.Errorf("the scenario never saw a token")
	}
	return app.WaitForNoText(w.token)
}

func (w *world) tokenListed(name string) error {
	app, err := w.app()
	if err != nil {
		return err
	}
	return app.WaitForText(name)
}
