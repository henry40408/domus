package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/store"
)

// logWait bounds a wedged binary rather than pacing the tests, which finish in
// milliseconds; generous so a loaded machine never reaches it.
const logWait = 30 * time.Second

var domusBinary string

// coverDir is where a `-cover` binary writes its profile; empty means the
// binary is built plain. `go test -coverprofile` does not see the exec'd binary.
var coverDir = os.Getenv("DOMUS_COVERDIR")

func TestMain(m *testing.M) {
	os.Exit(buildAndRun(m))
}

// buildAndRun builds the binary once, so every test drives the real thing.
func buildAndRun(m *testing.M) int {
	dir, err := os.MkdirTemp("", "domus-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	domusBinary = filepath.Join(dir, "domus")
	buildArgs := []string{"build", "-mod=vendor"}
	if coverDir != "" {
		if err := os.MkdirAll(coverDir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		buildArgs = append(buildArgs, "-cover", "-coverpkg=github.com/henry40408/domus/...")
	}
	build := exec.Command("go", append(buildArgs, "-o", domusBinary, ".")...)
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build failed: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

// serverEnv is the environment of a server under test: any free port, no mDNS,
// a data directory of its own.
func serverEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	return append([]string{
		"DOMUS_BIND=127.0.0.1:0",
		"DOMUS_DATA_DIR=" + filepath.Join(t.TempDir(), "data"),
		"DOMUS_MDNS=0",
	}, extra...)
}

func command(ctx context.Context, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, domusBinary, args...)
	cmd.Env = env
	if coverDir != "" {
		cmd.Env = append(append([]string(nil), env...), "GOCOVERDIR="+coverDir)
	}
	return cmd
}

func runToCompletion(t *testing.T, env []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), logWait)
	defer cancel()
	cmd := command(ctx, env, args...)
	var out, errw bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errw
	if err := cmd.Run(); err != nil && cmd.ProcessState == nil {
		t.Fatalf("failed to run domus: %v", err)
	}
	return out.String(), errw.String(), cmd.ProcessState.ExitCode()
}

// runningServer is a domus process whose stdout and stderr are read line by line.
type runningServer struct {
	cmd    *exec.Cmd
	stdout chan string
	stderr chan string
	logs   []string
	errs   []string
}

func startServer(t *testing.T, env []string, args ...string) *runningServer {
	t.Helper()
	cmd := command(context.Background(), env, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	server := &runningServer{cmd: cmd, stdout: make(chan string, 64), stderr: make(chan string, 64)}
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start domus: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	go pump(stdout, server.stdout)
	go pump(stderr, server.stderr)
	return server
}

func pump(r io.Reader, lines chan<- string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		lines <- scanner.Text()
	}
	close(lines)
}

// waitFor reads lines until one matches pattern, recording every line.
func waitFor(t *testing.T, lines <-chan string, record *[]string, pattern *regexp.Regexp) string {
	t.Helper()
	timeout := time.After(logWait)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("output closed before %v; saw:\n%s", pattern, strings.Join(*record, "\n"))
			}
			*record = append(*record, line)
			if pattern.MatchString(line) {
				return line
			}
		case <-timeout:
			t.Fatalf("%v never appeared; saw:\n%s", pattern, strings.Join(*record, "\n"))
		}
	}
}

var listeningLine = regexp.MustCompile(`listening on http://([^\s"]+)`)

// waitListening returns the address the server reports.
func (s *runningServer) waitListening(t *testing.T) string {
	t.Helper()
	line := waitFor(t, s.stdout, &s.logs, listeningLine)
	return listeningLine.FindStringSubmatch(line)[1]
}

func (s *runningServer) terminate(t *testing.T) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
}

// drain collects the remaining output until both streams close, then returns
// the exit code.
func (s *runningServer) finish(t *testing.T) int {
	t.Helper()
	timeout := time.After(logWait)
	for s.stdout != nil || s.stderr != nil {
		select {
		case line, ok := <-s.stdout:
			if !ok {
				s.stdout = nil
				continue
			}
			s.logs = append(s.logs, line)
		case line, ok := <-s.stderr:
			if !ok {
				s.stderr = nil
				continue
			}
			s.errs = append(s.errs, line)
		case <-timeout:
			t.Fatalf("the server never closed its output; logs:\n%s", strings.Join(s.logs, "\n"))
		}
	}
	if err := s.cmd.Wait(); err != nil && s.cmd.ProcessState == nil {
		t.Fatal(err)
	}
	return s.cmd.ProcessState.ExitCode()
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

var setupCodeLine = regexp.MustCompile(`^domus: no users yet\. Setup code: ([0-9A-Z]{5}-[0-9A-Z]{5})$`)

func TestTheSetupCodeIsPrintedOnStderr(t *testing.T) {
	server := startServer(t, serverEnv(t))
	line := waitFor(t, server.stderr, &server.errs, setupCodeLine)
	code := setupCodeLine.FindStringSubmatch(line)[1]
	addr := server.waitListening(t)

	status, body := get(t, "http://"+addr+"/api/domus/status")
	if status != 200 || !strings.Contains(body, `"setup_code_required":true`) {
		t.Fatalf("status %d, body %s", status, body)
	}
	server.terminate(t)
	if exit := server.finish(t); exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	// The code is for the operator's eyes only: stdout has the warning, not the code.
	for _, line := range server.logs {
		if strings.Contains(line, code) {
			t.Fatalf("the setup code reached the log: %s", line)
		}
	}
}

func TestSIGTERMShutsDownGracefully(t *testing.T) {
	server := startServer(t, serverEnv(t))
	server.waitListening(t)
	server.terminate(t)
	if exit := server.finish(t); exit != 0 {
		t.Fatalf("exit %d; logs:\n%s", exit, strings.Join(server.logs, "\n"))
	}
	if !strings.Contains(strings.Join(server.logs, "\n"), "shutting down") {
		t.Fatalf("no shutdown line; logs:\n%s", strings.Join(server.logs, "\n"))
	}
}

func TestAnOpenEventStreamDoesNotHoldBackShutdown(t *testing.T) {
	server := startServer(t, serverEnv(t))
	addr := server.waitListening(t)
	cookie := setUpAdmin(t, server, addr)

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/api/domus/events", nil)
	req.Header.Set("Cookie", cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("events: status %d", resp.StatusCode)
	}
	server.terminate(t)
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the event stream was still open ten seconds after SIGTERM")
	}
	if exit := server.finish(t); exit != 0 {
		t.Fatalf("exit %d", exit)
	}
}

// setUpAdmin creates the first admin and returns its session cookie.
func setUpAdmin(t *testing.T, server *runningServer, addr string) string {
	t.Helper()
	line := waitFor(t, server.stderr, &server.errs, setupCodeLine)
	code := setupCodeLine.FindStringSubmatch(line)[1]
	body, _ := json.Marshal(map[string]string{
		"username": "admin", "password": "a-long-enough-password", "setup_code": code,
	})
	resp, err := http.Post("http://"+addr+"/api/domus/setup", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("setup: status %d", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "domus_session" {
			return cookie.Name + "=" + cookie.Value
		}
	}
	t.Fatal("setup set no session cookie")
	return ""
}

func TestHealthcheckExitCodes(t *testing.T) {
	server := startServer(t, serverEnv(t))
	addr := server.waitListening(t)

	stdout, stderr, code := runToCompletion(t, []string{"DOMUS_BIND=" + addr}, "healthcheck")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("healthy: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	server.terminate(t)
	server.finish(t)
	stdout, stderr, code = runToCompletion(t, []string{"DOMUS_BIND=" + addr}, "healthcheck")
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "domus: unhealthy: ") {
		t.Fatalf("stopped: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestHealthcheckWithAnInvalidBindExitsWithTwo(t *testing.T) {
	_, stderr, code := runToCompletion(t, []string{"DOMUS_BIND=localhost:8123"}, "healthcheck")
	if code != 2 || !strings.HasPrefix(stderr, `domus: DOMUS_BIND="localhost:8123" is not a valid socket address: `) {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestAnInvalidBindExitsWithTwo(t *testing.T) {
	stdout, stderr, code := runToCompletion(t, serverEnv(t, "DOMUS_BIND=nope"))
	if code != 2 || stdout != "" || !strings.HasPrefix(stderr, `domus: DOMUS_BIND="nope" is not a valid socket address: `) {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestAnAddressInUseExitsWithOne(t *testing.T) {
	first := startServer(t, serverEnv(t))
	addr := first.waitListening(t)
	stdout, stderr, code := runToCompletion(t, serverEnv(t, "DOMUS_BIND="+addr))
	if code != 1 || !strings.Contains(stderr, "domus: cannot bind "+addr+": ") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestRustLogIsARetiredVariable(t *testing.T) {
	stdout, stderr, code := runToCompletion(t, serverEnv(t, "RUST_LOG=debug"))
	want := "domus: these environment variables no longer exist; rename (or unset) them to continue:\n" +
		"  RUST_LOG -> DOMUS_LOG_LEVEL\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestAnUnknownLogLevelExitsWithTwo(t *testing.T) {
	_, stderr, code := runToCompletion(t, serverEnv(t, "DOMUS_LOG_LEVEL=trace"))
	if code != 2 || !strings.Contains(stderr, `DOMUS_LOG_LEVEL="trace"`) {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestTheLogLevelFiltersTheLog(t *testing.T) {
	server := startServer(t, serverEnv(t, "DOMUS_LOG_LEVEL=error"))
	waitFor(t, server.stderr, &server.errs, setupCodeLine)
	server.terminate(t)
	server.finish(t)
	if len(server.logs) != 0 {
		t.Fatalf("level error still logged:\n%s", strings.Join(server.logs, "\n"))
	}
}

func TestUnknownArgumentsAreIgnored(t *testing.T) {
	server := startServer(t, serverEnv(t), "--version", "whatever")
	server.waitListening(t)
}

func TestTheDataDirIsOwnerOnlyAndTheDatabaseToo(t *testing.T) {
	env := serverEnv(t)
	server := startServer(t, env)
	server.waitListening(t)
	dir := strings.TrimPrefix(env[1], "DOMUS_DATA_DIR=")
	assertMode(t, dir, 0o700)
	assertMode(t, filepath.Join(dir, "domus.db"), 0o600)
}

// seedAdminDatabase leaves the data directory a previous run would have left:
// a database with one admin.
func seedAdminDatabase(t *testing.T, dataDir, password string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dataDir, "domus.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFirstAdmin(context.Background(), "admin", auth.HashPassword(password)); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// A database that already has users opens as it is: the admin logs in with
// their password, and no setup code is asked for.
func TestADatabaseWithUsersIssuesNoSetupCodeAndAcceptsTheirLogin(t *testing.T) {
	env := serverEnv(t)
	seedAdminDatabase(t, strings.TrimPrefix(env[1], "DOMUS_DATA_DIR="), "seeded-admin-password")

	server := startServer(t, env)
	addr := server.waitListening(t)
	body := `{"username":"admin","password":"seeded-admin-password"}`
	resp, err := http.Post("http://"+addr+"/api/domus/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login: status %d", resp.StatusCode)
	}
	status, text := get(t, "http://"+addr+"/api/domus/status")
	if status != 200 || !strings.Contains(text, `"setup_done":true`) || !strings.Contains(text, `"setup_code_required":false`) {
		t.Fatalf("status %d, body %s", status, text)
	}
	server.terminate(t)
	if exit := server.finish(t); exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	// Only after finish has read all of stderr is "no line" worth anything.
	for _, line := range server.errs {
		if strings.Contains(line, "Setup code") {
			t.Fatalf("a setup code was issued for a database with users: %s", line)
		}
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s has mode %o, want %o", path, got, want)
	}
}

func TestADataDirThatCannotBeMadeExitsWithOne(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "data")
	stdout, stderr, code := runToCompletion(t, serverEnv(t, "DOMUS_DATA_DIR="+dir))
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "domus: cannot prepare data dir "+dir+": ") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// mDNS is on by default. Which line the log gets depends on the host's
// network (a LAN address, none, or a refused socket), so the test checks that
// the server reaches a decision and still shuts down cleanly.
func TestTheServerStopsCleanlyWithMDNSEnabled(t *testing.T) {
	server := startServer(t, serverEnv(t, "DOMUS_BIND=0.0.0.0:0", "DOMUS_MDNS=1"))
	server.waitListening(t)
	server.terminate(t)
	if exit := server.finish(t); exit != 0 {
		t.Fatalf("exit %d; logs:\n%s", exit, strings.Join(server.logs, "\n"))
	}
	logs := strings.Join(server.logs, "\n")
	if !strings.Contains(logs, "advertising on the LAN") &&
		!strings.Contains(logs, "not advertising over mDNS") &&
		!strings.Contains(logs, "mDNS discovery disabled") {
		t.Fatalf("mDNS made no decision; logs:\n%s", logs)
	}
}
