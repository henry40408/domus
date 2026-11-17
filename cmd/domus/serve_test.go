package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henry40408/domus/internal/core"
	"github.com/henry40408/domus/internal/hue"
	"github.com/henry40408/domus/internal/store"
)

func memoryStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestASetupCodeIsIssuedOnlyWhileThereIsNoUser(t *testing.T) {
	st := memoryStore(t)
	ctx := context.Background()
	var stderr bytes.Buffer
	first := setupCodeIfNoUsers(ctx, st, &stderr, discardLogger())
	left, right, found := strings.Cut(first, "-")
	if !found || len(left) != 5 || len(right) != 5 {
		t.Fatalf("code = %q", first)
	}
	for _, c := range first {
		if c != '-' && !('0' <= c && c <= '9' || 'A' <= c && c <= 'Z') {
			t.Fatalf("code = %q has %q", first, c)
		}
	}
	if want := "domus: no users yet. Setup code: " + first + "\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	second := setupCodeIfNoUsers(ctx, st, io.Discard, discardLogger())
	if first == second {
		t.Fatal("every start draws a new code")
	}

	if _, err := st.CreateFirstAdmin(ctx, "admin", "hash"); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := setupCodeIfNoUsers(ctx, st, &stderr, discardLogger()); code != "" || stderr.Len() != 0 {
		t.Fatalf("code = %q, stderr = %q once a user exists", code, stderr.String())
	}
}

func TestTheSetupCodeIsNotLogged(t *testing.T) {
	var logs bytes.Buffer
	code := setupCodeIfNoUsers(context.Background(), memoryStore(t), io.Discard, newLogger(&logs, slog.LevelDebug))
	if logs.Len() == 0 || strings.Contains(logs.String(), code) {
		t.Fatalf("logs = %q", logs.String())
	}
}

func TestAnExistingDataDirIsTightened(t *testing.T) {
	// A directory that already exists with wider permissions is tightened.
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	assertMode(t, dir, 0o700)
	assertMode(t, filepath.Join(dir, "domus.db"), 0o600)

	nested := filepath.Join(t.TempDir(), "a", "b")
	if err := prepareDataDir(nested); err != nil {
		t.Fatal(err)
	}
	assertMode(t, nested, 0o700)
}

func TestADataDirThatCannotBeMadeIsReported(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "data")
	_, err := openStore(context.Background(), dir)
	if err == nil || !strings.HasPrefix(err.Error(), "cannot prepare data dir "+dir+": ") {
		t.Fatalf("err = %v", err)
	}
}

func TestADatabaseThatCannotBeOpenedIsReported(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "domus.db"), []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := openStore(context.Background(), dir)
	if err == nil || !strings.HasPrefix(err.Error(), "cannot open "+filepath.Join(dir, "domus.db")+": ") {
		t.Fatalf("err = %v", err)
	}
}

func TestHueResumesOnlyWhenABridgeWasPaired(t *testing.T) {
	ctx := context.Background()
	st := memoryStore(t)
	manager := hue.NewManager(core.New(st, nil), hue.Timing{}, discardLogger())
	t.Cleanup(manager.Reset)

	resumeHue(ctx, st, manager, discardLogger())
	if manager.IsRunning() {
		t.Fatal("Hue started without a paired bridge")
	}

	// Nothing answers on this port: the task runs and keeps retrying.
	if err := st.HueSet(ctx, store.HueBridge{IP: "http://127.0.0.1:1", Key: "key"}); err != nil {
		t.Fatal(err)
	}
	resumeHue(ctx, st, manager, discardLogger())
	if !manager.IsRunning() {
		t.Fatal("Hue did not start for a paired bridge")
	}
}

func TestTheServerHasNoWriteTimeout(t *testing.T) {
	server := newHTTPServer(http.NotFoundHandler(), discardLogger())
	if server.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v: event streams and WebSockets would be cut off", server.WriteTimeout)
	}
	if server.ReadHeaderTimeout <= 0 {
		t.Fatal("a client may hold a connection open without sending headers")
	}
}

func TestRunReportsARetiredVariableWithTheFailureCode(t *testing.T) {
	for _, args := range [][]string{nil, {"healthcheck"}} {
		var stdout, stderr bytes.Buffer
		code := run(process{args: args, lookupEnv: envOf(map[string]string{"RUST_LOG": ""}), stdout: &stdout, stderr: &stderr})
		if code != exitFailure || stdout.Len() != 0 || !strings.Contains(stderr.String(), "  RUST_LOG -> DOMUS_LOG_LEVEL\n") {
			t.Fatalf("args %v: exit %d, stdout %q, stderr %q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestRunReportsConfigurationErrorsWithTheUsageCode(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"bad bind", map[string]string{"DOMUS_BIND": "nope"}, nil, `domus: DOMUS_BIND="nope" is not a valid socket address: `},
		{"bad bind for healthcheck", map[string]string{"DOMUS_BIND": "nope"}, []string{"healthcheck"}, `DOMUS_BIND="nope"`},
		{"bad level", map[string]string{"DOMUS_LOG_LEVEL": "trace"}, nil, `DOMUS_LOG_LEVEL="trace"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(process{args: tc.args, lookupEnv: envOf(tc.env), stdout: &stdout, stderr: &stderr})
			if code != exitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}
