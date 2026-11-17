// Package server wires domus's HTTP routes, middleware and handlers.
package server

import (
	"log/slog"
	"sync"
	"time"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/core"
	"github.com/henry40408/domus/internal/hue"
	"github.com/henry40408/domus/internal/version"
)

// Config is everything the server needs from the outside.
type Config struct {
	Core *core.Core
	Hue  *hue.Manager
	// Version is reported by /health and stamped on the asset URLs. Empty
	// means the build's own version.
	Version string
	Logger  *slog.Logger
	// SetupCode is the one-time code the first admin must enter; empty means
	// none is required.
	SetupCode string
	// Now is the clock for login throttling; nil means time.Now.
	Now func() time.Time
	// Timing holds the intervals tests shorten; zero fields mean the default.
	Timing Timing
	// Shutdown is closed when the server starts shutting down, so long-lived
	// responses (event streams) end and do not hold the graceful stop. Nil
	// means never.
	Shutdown <-chan struct{}
}

// Timing is the set of intervals the server waits on.
type Timing struct {
	// EventsRecheck is how often an open event stream re-checks its session.
	EventsRecheck time.Duration
	// EventsKeepAlive is how often an idle event stream sends a comment, so
	// proxies keep the connection open.
	EventsKeepAlive time.Duration
	// WebsocketAuth is how long a WebSocket client may take to authenticate.
	WebsocketAuth time.Duration
	// WebsocketRecheck is how often an open WebSocket re-checks its login and
	// the groups of its user.
	WebsocketRecheck time.Duration
}

const (
	defaultEventsRecheck    = 30 * time.Second
	defaultEventsKeepAlive  = 15 * time.Second
	defaultWebsocketAuth    = 10 * time.Second
	defaultWebsocketRecheck = 30 * time.Second
)

func (t Timing) withDefaults() Timing {
	if t.EventsRecheck <= 0 {
		t.EventsRecheck = defaultEventsRecheck
	}
	if t.EventsKeepAlive <= 0 {
		t.EventsKeepAlive = defaultEventsKeepAlive
	}
	if t.WebsocketAuth <= 0 {
		t.WebsocketAuth = defaultWebsocketAuth
	}
	if t.WebsocketRecheck <= 0 {
		t.WebsocketRecheck = defaultWebsocketRecheck
	}
	return t
}

// App is the domus server: its state and its HTTP handler.
type App struct {
	core    *core.Core
	hue     *hue.Manager
	version string
	logger  *slog.Logger
	// audit carries security events (logins, setup, session changes).
	audit *slog.Logger
	now   func() time.Time
	guard auth.LoginGuard
	codes *authCodes

	timing   Timing
	shutdown <-chan struct{}

	setupMu   sync.Mutex
	setupCode string
}

// New builds the app. It does no I/O, except that it computes the dummy
// password hash once, so the first login for an unknown user does not stand
// out by its timing.
func New(cfg Config) *App {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	appVersion := cfg.Version
	if appVersion == "" {
		appVersion = version.Version()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	auth.WarmDummy()
	return &App{
		core:      cfg.Core,
		hue:       cfg.Hue,
		version:   appVersion,
		logger:    logger,
		audit:     logger.With("target", "audit"),
		now:       now,
		codes:     newAuthCodes(now),
		timing:    cfg.Timing.withDefaults(),
		shutdown:  cfg.Shutdown,
		setupCode: cfg.SetupCode,
	}
}
