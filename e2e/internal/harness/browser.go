package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// ScenarioTimeout bounds one scenario, so a wedged Chrome cannot hang the run.
const ScenarioTimeout = 3 * time.Minute

const (
	windowWidth  = 1280
	windowHeight = 900
	closeTimeout = 10 * time.Second
)

// Browser is a headless Chrome of its own, scoped to one scenario.
//
// One process per scenario isolates cookies and storage.
type Browser struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// Open starts a headless Chrome.
func Open() (*Browser, error) {
	timeoutCtx, cancelTimeout := context.WithTimeout(context.Background(), ScenarioTimeout)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(timeoutCtx, allocatorOptions()...)
	tabCtx, cancelTab := chromedp.NewContext(allocCtx)
	browser := &Browser{
		ctx: tabCtx,
		cancel: func() {
			cancelTab()
			cancelAlloc()
			cancelTimeout()
		},
	}

	if err := browser.start(); err != nil {
		browser.cancel()
		return nil, err
	}
	return browser, nil
}

// ChromeEnv names the variable that pins the browser binary.
const ChromeEnv = "E2E_CHROME"

// BrowserChoice says which browser Open will start, for the run's log.
func BrowserChoice() string {
	if path := os.Getenv(ChromeEnv); path != "" {
		return ChromeEnv + "=" + path
	}
	return "chromedp's default search (" + ChromeEnv + " is unset)"
}

func allocatorOptions() []chromedp.ExecAllocatorOption {
	// The defaults already carry what the suite needs: headless,
	// no /dev/shm use, and no throttling of a backgrounded renderer (which
	// starves input while CDP script keeps answering, the shape of the clicks
	// CI drops).
	options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	options = append(options, chromedp.WindowSize(windowWidth, windowHeight))
	// chromedp's own search prefers a chromium on PATH over google-chrome, so
	// CI pins the binary it reports.
	if path := os.Getenv(ChromeEnv); path != "" {
		options = append(options, chromedp.ExecPath(path))
	}
	return options
}

func (b *Browser) start() error {
	if err := chromedp.Do(b.ctx); err != nil {
		return fmt.Errorf("could not start a browser (%s): a local Chromium or Chrome is required "+
			"(`brew install --cask ungoogled-chromium`), or set %s to its path: %w", BrowserChoice(), ChromeEnv, err)
	}
	return b.SetViewport(windowWidth, windowHeight)
}

// Close ends the browser, and the profile directory goes with it.
func (b *Browser) Close() {
	closing, cancel := context.WithTimeout(b.ctx, closeTimeout)
	defer cancel()
	_ = chromedp.Cancel(closing)
	b.cancel()
}

// EmulateColorScheme emulates prefers-color-scheme with no stored
// preference: the app's system-follow path, which is what the screenshots are
// meant to show.
func (b *Browser) EmulateColorScheme(scheme string) error {
	return send(b, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{
		Media:    "screen",
		Features: []*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: scheme}},
	})
}

// SetViewport sizes the page, and so the screenshots.
func (b *Browser) SetViewport(width, height int64) error {
	return send(b, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{
		Width:             width,
		Height:            height,
		DeviceScaleFactor: 1,
	})
}

func send[P any](b *Browser, command cdp.Command[P, cdp.Empty], params P) error {
	_, err := chromedp.Call(b.ctx, command, params)
	return err
}

// Goto navigates to a URL and waits for the page to load.
func (b *Browser) Goto(url string) error {
	return chromedp.Do(b.ctx, chromedp.Navigate(url))
}

// Reload reloads the page and waits for it to load.
func (b *Browser) Reload() error {
	if err := chromedp.Do(b.ctx, chromedp.Reload()); err != nil {
		return err
	}
	return b.WaitForLoad()
}

// WaitForLoad waits for document.readyState to be "complete".
//
// CDP does not wait for a page load after a click, so every read or action
// that follows a navigation waits first. Without it, a key press can run
// before app.js has loaded, and a lookup can run against the old document.
func (b *Browser) WaitForLoad() error {
	return Eventually("the page to finish loading", func() (bool, error) {
		state, err := Eval[string](b, "document.readyState")
		return state == "complete", err
	})
}

// URL is the page's current URL.
func (b *Browser) URL() (*url.URL, error) {
	location, err := chromedp.Run(b.ctx, chromedp.Location())
	if err != nil {
		return nil, err
	}
	return url.Parse(location)
}

// Path is the current URL's path, the shape the steps assert against.
func (b *Browser) Path() (string, error) {
	current, err := b.URL()
	if err != nil {
		return "", err
	}
	return current.Path, nil
}

// Screenshot captures the viewport as a PNG.
func (b *Browser) Screenshot() ([]byte, error) {
	return chromedp.Run(b.ctx, chromedp.CaptureScreenshot())
}

// Eval evaluates a JavaScript expression in the page and decodes the result.
//
// Reads go through JavaScript rather than DOM-domain queries, because
// chromedp's queries wait forever by default and the suite's lookups are
// immediate. The page CSP does not apply to it.
func Eval[T any](b *Browser, expression string) (T, error) {
	return chromedp.Run(b.ctx, chromedp.Evaluate[T](expression))
}

// jsLiteral renders a Go value as a JavaScript literal.
func jsLiteral(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		// Only strings, numbers and bools are passed; this cannot happen.
		panic(fmt.Sprintf("encoding %v as a JavaScript literal: %v", value, err))
	}
	return string(encoded)
}
