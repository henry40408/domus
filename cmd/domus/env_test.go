package main

import (
	"errors"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestAUsageErrorExposesTheErrorItWraps(t *testing.T) {
	cause := errors.New("bad bind")
	if !errors.Is(usageError{cause}, cause) {
		t.Fatal("errors.Is cannot reach the wrapped error")
	}
}

func envOf(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func mustLoad(t *testing.T, values map[string]string) env {
	t.Helper()
	e, err := loadEnv(envOf(values))
	if err != nil {
		t.Fatalf("loadEnv(%v): %v", values, err)
	}
	return e
}

func TestDefaultsAndOverrides(t *testing.T) {
	e := mustLoad(t, nil)
	if e.bind != netip.MustParseAddrPort("127.0.0.1:8123") || e.dataDir != "./data" ||
		e.logLevel != slog.LevelInfo || !e.mdns {
		t.Fatalf("defaults = %+v", e)
	}
	if e := mustLoad(t, map[string]string{"DOMUS_LOG_LEVEL": "debug"}); e.logLevel != slog.LevelDebug {
		t.Fatalf("level = %v", e.logLevel)
	}
	if e := mustLoad(t, map[string]string{"DOMUS_BIND": "0.0.0.0:9000"}); e.bind.Port() != 9000 {
		t.Fatalf("bind = %v", e.bind)
	}
	if e := mustLoad(t, map[string]string{"DOMUS_DATA_DIR": "/srv/domus"}); e.dataDir != "/srv/domus" {
		t.Fatalf("data dir = %q", e.dataDir)
	}
	for _, off := range []string{"0", "false", "OFF", "no"} {
		if mustLoad(t, map[string]string{"DOMUS_MDNS": off}).mdns {
			t.Errorf("DOMUS_MDNS=%s left mDNS on", off)
		}
	}
	for _, on := range []string{"1", "true", ""} {
		if !mustLoad(t, map[string]string{"DOMUS_MDNS": on}).mdns {
			t.Errorf("DOMUS_MDNS=%q turned mDNS off", on)
		}
	}
}

func TestABindThatIsNotASocketAddressIsRefused(t *testing.T) {
	for _, bad := range []string{"nope", "localhost:8123", "127.0.0.1", ":8123", ""} {
		_, err := loadEnv(envOf(map[string]string{"DOMUS_BIND": bad}))
		if err == nil || !strings.HasPrefix(err.Error(), `DOMUS_BIND="`+bad+`" is not a valid socket address: `) {
			t.Errorf("DOMUS_BIND=%q: err = %v", bad, err)
		}
	}
}

func TestALogLevelThatIsUnknownIsRefused(t *testing.T) {
	for _, bad := range []string{"trace", "INFO", "error,domus=info", ""} {
		_, err := loadEnv(envOf(map[string]string{"DOMUS_LOG_LEVEL": bad}))
		if err == nil || !strings.Contains(err.Error(), "DOMUS_LOG_LEVEL=") {
			t.Errorf("DOMUS_LOG_LEVEL=%q: err = %v", bad, err)
		}
	}
}

func TestLegacyEnvVarsAreReported(t *testing.T) {
	err := ensureNoLegacyEnvVars(envOf(map[string]string{"RUST_LOG": ""}))
	want := "these environment variables no longer exist; rename (or unset) them to continue:\n" +
		"  RUST_LOG -> DOMUS_LOG_LEVEL"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
	if ensureNoLegacyEnvVars(envOf(nil)) != nil {
		t.Fatal("an empty environment was refused")
	}
}

func TestEveryLegacyVariableNamesALiveReplacement(t *testing.T) {
	for _, legacy := range legacyEnvVars {
		if !slices.Contains(liveEnvVars, legacy.replacement) {
			t.Errorf("%s points at %s, which nothing reads", legacy.old, legacy.replacement)
		}
		if slices.Contains(liveEnvVars, legacy.old) {
			t.Errorf("%s is both legacy and live", legacy.old)
		}
	}
}

func TestLiveEnvVarsAreTheOnesLoadEnvReads(t *testing.T) {
	var asked []string
	_, _ = loadEnv(func(key string) (string, bool) {
		asked = append(asked, key)
		return "", false
	})
	slices.Sort(asked)
	want := slices.Clone(liveEnvVars)
	slices.Sort(want)
	if !slices.Equal(asked, want) {
		t.Fatalf("loadEnv reads %v, liveEnvVars lists %v", asked, want)
	}
}
