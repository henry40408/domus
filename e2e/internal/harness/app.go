package harness

import (
	"fmt"
	"strings"
)

// App is the domus single-page app in a browser. The page has no test ids, so
// controls are found by their placeholder or label.
type App struct {
	Browser *Browser
	server  *Server
}

// App returns the page object for a server.
func (b *Browser) App(server *Server) *App {
	return &App{Browser: b, server: server}
}

// Open navigates to a path of the server, such as "/" or "/#groups".
func (a *App) Open(path string) error {
	return a.Browser.Goto(a.server.BaseURL + path)
}

// Fill types into the input whose placeholder starts with prefix, once the
// page has rendered it.
func (a *App) Fill(placeholderPrefix, text string) error {
	selector := fmt.Sprintf("input[placeholder^=%s]", jsLiteral(placeholderPrefix))
	var field *Element
	err := Eventually("the field "+placeholderPrefix, func() (bool, error) {
		var err error
		field, err = a.Browser.Optional(selector)
		return field != nil, err
	})
	if err != nil {
		return err
	}
	return field.Type(text)
}

// Control says what to click and how to tell that the page reacted.
type Control struct {
	// Selector narrows the candidates, such as "button" or "nav a".
	Selector string
	// Text is the exact label of the control.
	Text string
	// TookEffect reports whether the page reacted.
	TookEffect func() (bool, error)
}

// Click clicks the control, retrying until TookEffect holds.
func (a *App) Click(control Control) error {
	what := fmt.Sprintf("%s %q", control.Selector, control.Text)
	return clickUntil(a.Browser, clickSpec{
		what: what,
		locate: func() (*Element, error) {
			var found *Element
			err := Eventually(what+" is on the page", func() (bool, error) {
				var err error
				found, err = a.Browser.WithText(control.Selector, control.Text)
				return found != nil, err
			})
			return found, err
		},
		tookEffect: control.TookEffect,
	})
}

// Hash is the page's current location hash, such as "#groups".
func (a *App) Hash() (string, error) {
	return Eval[string](a.Browser, "location.hash")
}

// HasText reports whether the visible page text contains text.
func (a *App) HasText(text string) (bool, error) {
	return Eval[bool](a.Browser, "document.body.innerText.includes("+jsLiteral(text)+")")
}

// WaitForText waits until the visible page text contains text.
func (a *App) WaitForText(text string) error {
	return Eventually("the page to show "+jsLiteral(text), func() (bool, error) {
		return a.HasText(text)
	})
}

// WaitForNoText waits until the visible page text no longer contains text.
func (a *App) WaitForNoText(text string) error {
	return Eventually("the page to stop showing "+jsLiteral(text), func() (bool, error) {
		found, err := a.HasText(text)
		return !found, err
	})
}

// ToastText is the message of the toast, empty while none shows.
func (a *App) ToastText() (string, error) {
	return Eval[string](a.Browser, `(() => {
		const toast = document.getElementById("toast");
		return toast && toast.classList.contains("show") ? toast.textContent : "";
	})()`)
}

// WaitForToast waits for a toast that contains text. A failure names the last
// toast seen.
func (a *App) WaitForToast(text string) error {
	var last string
	err := Eventually("a toast with "+jsLiteral(text), func() (bool, error) {
		var err error
		last, err = a.ToastText()
		return strings.Contains(last, text), err
	})
	if err != nil {
		return fmt.Errorf("%w (last toast: %q)", err, last)
	}
	return nil
}

// SwitchIsOn reports whether the on/off switch of the named light is on, and
// fails when the page has no such light.
func (a *App) SwitchIsOn(lightName string) (bool, error) {
	state, err := Eval[string](a.Browser, `(() => {
		const row = [...document.querySelectorAll("li.light")].find(
			(li) => li.querySelector(".name")?.textContent === `+jsLiteral(lightName)+`);
		if (!row) { return "missing"; }
		return row.querySelector("button.switch").classList.contains("on") ? "on" : "off";
	})()`)
	if err != nil {
		return false, err
	}
	if state == "missing" {
		return false, fmt.Errorf("the page lists no light named %q", lightName)
	}
	return state == "on", nil
}

// ClickSwitch flips the on/off switch of the named light in the Dashboard and
// waits until it shows the opposite state.
func (a *App) ClickSwitch(lightName string) error {
	before, err := a.SwitchIsOn(lightName)
	if err != nil {
		return err
	}
	return clickUntil(a.Browser, clickSpec{
		what: "the switch of " + lightName,
		locate: func() (*Element, error) {
			return a.Browser.Query(fmt.Sprintf("li.light:nth-child(%d) button.switch", a.lightPosition(lightName)))
		},
		tookEffect: func() (bool, error) {
			now, err := a.SwitchIsOn(lightName)
			return now != before, err
		},
	})
}

func (a *App) lightPosition(lightName string) int {
	position, _ := Eval[int](a.Browser, `[...document.querySelectorAll("li.light")].findIndex(
		(li) => li.querySelector(".name")?.textContent === `+jsLiteral(lightName)+`) + 1`)
	return position
}

// CheckByLabel ticks the checkbox of the label that reads text.
func (a *App) CheckByLabel(text string) error {
	label, err := a.Browser.WithText("label", text)
	if err != nil || label == nil {
		return fmt.Errorf("no label %q: %v", text, err)
	}
	box, err := label.Find("input[type=checkbox]")
	if err != nil {
		return err
	}
	return clickUntil(a.Browser, clickSpec{
		what:       "the checkbox " + text,
		locate:     func() (*Element, error) { return box, nil },
		tookEffect: func() (bool, error) { return callOn[bool](box, "function() { return this.checked; }") },
	})
}

// ActivateScene clicks Activate in the row of the named scene. The row's
// button is tagged first, because the page has no test ids and every row has
// the same button.
func (a *App) ActivateScene(sceneName string) error {
	const tag = "data-e2e-target"
	return clickUntil(a.Browser, clickSpec{
		what: "Activate " + sceneName,
		locate: func() (*Element, error) {
			err := Eventually("the scene "+sceneName+" is on the page", func() (bool, error) {
				return Eval[bool](a.Browser, `(() => {
					document.querySelectorAll("[`+tag+`]").forEach((e) => e.removeAttribute("`+tag+`"));
					const row = [...document.querySelectorAll("li")].find(
						(li) => li.querySelector(".name")?.textContent.endsWith(`+jsLiteral(sceneName)+`) && li.querySelector("button.act")?.textContent === "Activate");
					row?.querySelector("button.act").setAttribute("`+tag+`", "");
					return !!row;
				})()`)
			})
			if err != nil {
				return nil, err
			}
			return a.Browser.Query("[" + tag + "]")
		},
		tookEffect: func() (bool, error) {
			toast, err := a.ToastText()
			return strings.Contains(toast, "Activated"), err
		},
	})
}

// Exists reports whether the CSS selector matches an element on the page.
func (a *App) Exists(selector string) (bool, error) {
	return Eval[bool](a.Browser, "document.querySelector("+jsLiteral(selector)+") !== null")
}

// Text is the trimmed text of the first element matching the selector, empty
// when there is none.
func (a *App) Text(selector string) (string, error) {
	return Eval[string](a.Browser, "document.querySelector("+jsLiteral(selector)+")?.textContent.trim() ?? ''")
}
