package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/core"
	"github.com/henry40408/domus/internal/discovery"
	"github.com/henry40408/domus/internal/hue"
	"github.com/henry40408/domus/internal/server"
	"github.com/henry40408/domus/internal/store"
	"github.com/henry40408/domus/internal/version"
)

const (
	shutdownTimeout   = 30 * time.Second
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
)

// runServer follows a fixed order. The signal handler goes in before the
// listener, so a signal arriving right after startup still shuts down
// gracefully instead of hitting the default action.
func runServer(p process, e env) error {
	logger := newLogger(p.stdout, e.logLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openStore(ctx, e.dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	setupCode := setupCodeIfNoUsers(ctx, st, p.stderr, logger)

	c := core.New(st, logger)
	hueManager := hue.NewManager(c, hue.Timing{}, logger)
	resumeHue(ctx, st, hueManager, logger)
	stopping := make(chan struct{})
	app := server.New(server.Config{
		Core:      c,
		Hue:       hueManager,
		Logger:    logger,
		SetupCode: setupCode,
		Shutdown:  stopping,
	})

	listener, err := net.Listen("tcp", e.bind.String())
	if err != nil {
		return fmt.Errorf("cannot bind %s: %w", e.bind, err)
	}
	logger.Info(fmt.Sprintf("domus %s listening on http://%s", version.Version(), listener.Addr()))
	var advertiser *discovery.Advertiser
	if e.mdns {
		advertiser = discovery.Start(e.bind, e.dataDir, logger)
	}

	httpServer := newHTTPServer(app.Handler(), logger)
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()

	select {
	case err = <-serveErr:
	case <-ctx.Done():
		logger.Info("shutting down")
		close(stopping)
		shutdownGracefully(httpServer, logger)
		err = <-serveErr
	}
	if advertiser != nil {
		advertiser.Stop()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

func newHTTPServer(handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// No WriteTimeout (zero): it would cut off the event stream and the
		// WebSocket, which stay open for hours.
		WriteTimeout: 0,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}

func shutdownGracefully(httpServer *http.Server, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := httpServer.Shutdown(ctx)
	if err == nil {
		return
	}
	logger.Error("graceful shutdown failed, closing connections", "error", err)
	if err := httpServer.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		logger.Error("failed to close the server", "error", err)
	}
}

// prepareDataDir creates the directory owner-only. The chmod covers a
// directory that already existed with wider permissions.
func prepareDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

func openStore(ctx context.Context, dataDir string) (*store.Store, error) {
	if err := prepareDataDir(dataDir); err != nil {
		return nil, fmt.Errorf("cannot prepare data dir %s: %w", dataDir, err)
	}
	dbPath := filepath.Join(dataDir, "domus.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s: %w", dbPath, err)
	}
	// The database holds password hashes; failing to tighten it is not fatal.
	_ = os.Chmod(dbPath, 0o600)
	return st, nil
}

// setupCodeIfNoUsers returns "" once an admin exists. Until then, creating the
// first admin requires a code only the operator can read.
func setupCodeIfNoUsers(ctx context.Context, st *store.Store, stderr io.Writer, logger *slog.Logger) string {
	count, err := st.UserCount(ctx)
	if err != nil {
		// Like the Rust build: an unreadable user table counts as no users.
		logger.Warn("cannot count the users", "error", err)
	}
	if count != 0 {
		return ""
	}
	// 50 bits (10 base32 chars): wrong guesses are throttled (about 4 an hour)
	// and the code dies with setup.
	chars := auth.RandomBase32(10)
	code := chars[:5] + "-" + chars[5:]
	_, _ = fmt.Fprintf(stderr, "domus: no users yet. Setup code: %s\n", code)
	logger.Warn("no users yet; the setup code is required to create the first admin")
	return code
}

// resumeHue starts the Hue integration if a bridge was paired earlier.
func resumeHue(ctx context.Context, st *store.Store, manager *hue.Manager, logger *slog.Logger) {
	bridge, err := st.HueGet(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		logger.Warn("cannot read the paired Hue bridge", "error", err)
		return
	}
	manager.Start(hue.BridgeBase(bridge.IP), bridge.Key)
}
