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
		setupCode: cfg.SetupCode,
	}
}
