package main

import (
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
)

const (
	defaultBind    = "127.0.0.1:8123"
	defaultDataDir = "./data"
)

// env is the startup configuration, read from environment variables only.
type env struct {
	bind     netip.AddrPort
	dataDir  string
	logLevel slog.Level
	// mdns is whether to advertise on the LAN (only when bound to a
	// non-loopback address).
	mdns bool
}

// legacyEnvVar pairs a retired environment variable with its replacement.
type legacyEnvVar struct{ old, replacement string }

// legacyEnvVars lists the retired variables. Update it whenever an environment
// variable is added, renamed or removed: a silently ignored one is worse than a
// startup failure.
var legacyEnvVars = []legacyEnvVar{
	// The Rust build read RUST_LOG; a leftover one would otherwise be ignored
	// without a word.
	{"RUST_LOG", "DOMUS_LOG_LEVEL"},
}

// liveEnvVars are the variables loadEnv reads.
var liveEnvVars = []string{"DOMUS_BIND", "DOMUS_DATA_DIR", "DOMUS_LOG_LEVEL", "DOMUS_MDNS"}

// ensureNoLegacyEnvVars fails when a retired variable is set, even empty.
func ensureNoLegacyEnvVars(lookupEnv func(string) (string, bool)) error {
	var found []string
	for _, legacy := range legacyEnvVars {
		if _, ok := lookupEnv(legacy.old); ok {
			found = append(found, fmt.Sprintf("  %s -> %s", legacy.old, legacy.replacement))
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("these environment variables no longer exist; "+
		"rename (or unset) them to continue:\n%s", strings.Join(found, "\n"))
}

// loadEnv reads the configuration. A variable that is set but empty counts as
// set, as it did in the Rust build.
func loadEnv(lookupEnv func(string) (string, bool)) (env, error) {
	bindText := valueOr(lookupEnv, "DOMUS_BIND", defaultBind)
	// netip.ParseAddrPort takes an IP literal and a port, like Rust's
	// SocketAddr: "localhost:8123" is invalid.
	bind, err := netip.ParseAddrPort(bindText)
	if err != nil {
		return env{}, fmt.Errorf("DOMUS_BIND=%q is not a valid socket address: %w", bindText, err)
	}
	levelText := valueOr(lookupEnv, "DOMUS_LOG_LEVEL", "info")
	level, err := parseLogLevel(levelText)
	if err != nil {
		return env{}, fmt.Errorf("DOMUS_LOG_LEVEL=%q: %w", levelText, err)
	}
	return env{
		bind:     bind,
		dataDir:  valueOr(lookupEnv, "DOMUS_DATA_DIR", defaultDataDir),
		logLevel: level,
		mdns:     mdnsEnabled(valueOr(lookupEnv, "DOMUS_MDNS", "")),
	}, nil
}

func valueOr(lookupEnv func(string) (string, bool), key, fallback string) string {
	if value, ok := lookupEnv(key); ok {
		return value
	}
	return fallback
}

// mdnsEnabled is true unless the value switches it off; an empty value does not.
func mdnsEnabled(value string) bool {
	switch strings.ToLower(value) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}
