package main

import (
	"fmt"
	"io"
	"log/slog"
)

// parseLogLevel reads a DOMUS_LOG_LEVEL value. There is no per-target filter
// syntax: one level applies to everything.
func parseLogLevel(text string) (slog.Level, error) {
	switch text {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", text)
}

// newLogger writes to w, which is stdout in server mode, so stderr stays for
// the setup code and fatal errors.
func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
