// Package harness drives a domus server and a browser for the e2e suite.
package harness

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/henry40408/domus/e2e/internal/fakehue"
)

const startupTimeout = 30 * time.Second

const setupCodeMarker = "Setup code: "

// Server is a domus server with its own data directory and fake Hue bridge.
// Every scenario starts one, so scenarios share no state.
type Server struct {
	// BaseURL is where the browser goes.
	BaseURL string
	// SetupCode is the one-time code printed at startup.
	SetupCode string
	// BridgeURL is the fake Hue bridge that can be paired.
	BridgeURL string

	cmd     *exec.Cmd
	bridge  *httptest.Server
	dataDir string
}

// Start runs bin/domus, building it first when it is missing.
func Start() (*Server, error) {
	binary, err := ensureBinary()
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	dataDir, err := os.MkdirTemp("", "domus-e2e-")
	if err != nil {
		return nil, err
	}
	bridge := fakehue.Start()
	server := &Server{
		BaseURL:   "http://127.0.0.1:" + strconv.Itoa(port),
		BridgeURL: bridge.URL,
		bridge:    bridge,
		dataDir:   dataDir,
	}
	if err := server.launch(binary, port); err != nil {
		server.Stop()
		return nil, err
	}
	return server, nil
}

func (s *Server) launch(binary string, port int) error {
	cmd := exec.Command(binary)
	cmd.Dir = RepoRoot()
	cmd.Env = append(os.Environ(),
		"DOMUS_BIND=127.0.0.1:"+strconv.Itoa(port),
		"DOMUS_DATA_DIR="+s.dataDir,
		"DOMUS_MDNS=0",
		"DOMUS_LOG_LEVEL=warn",
	)
	cmd.Stdout = os.Stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawning %s: %w", binary, err)
	}
	s.cmd = cmd
	code, err := readSetupCode(stderr)
	if err != nil {
		return err
	}
	s.SetupCode = code
	return s.waitUntilHealthy()
}

// Stop kills the server and removes its data. Safe to call twice.
func (s *Server) Stop() {
	if s.cmd != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
		s.cmd = nil
	}
	// After domus is gone, so its event stream no longer holds the bridge open.
	if s.bridge != nil {
		s.bridge.Close()
		s.bridge = nil
	}
	_ = os.RemoveAll(s.dataDir)
}

// readSetupCode reads stderr up to the setup code line, then keeps forwarding
// the rest so the server never blocks on a full pipe.
func readSetupCode(stderr io.Reader) (string, error) {
	lines := bufio.NewScanner(stderr)
	for lines.Scan() {
		line := lines.Text()
		fmt.Fprintln(os.Stderr, line)
		if _, code, found := strings.Cut(line, setupCodeMarker); found {
			go forward(lines)
			return strings.TrimSpace(code), nil
		}
	}
	return "", fmt.Errorf("domus exited before printing a setup code")
}

func forward(lines *bufio.Scanner) {
	for lines.Scan() {
		fmt.Fprintln(os.Stderr, lines.Text())
	}
}

func (s *Server) waitUntilHealthy() error {
	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		response, err := http.Get(s.BaseURL + "/health")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("domus did not answer %s/health within %v", s.BaseURL, startupTimeout)
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("probing for a free port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// ensureBinary returns bin/domus, building it first when it is not there.
//
// An existing binary is never rebuilt, so rebuild by hand after changing
// server code: the web files are embedded, a stale binary shows stale UI.
func ensureBinary() (string, error) {
	binary := filepath.Join(RepoRoot(), "bin", "domus")
	if isFile(binary) {
		return binary, nil
	}
	fmt.Fprintf(os.Stderr, "e2e: %s is missing, building it\n", binary)
	build := exec.Command("go", "build", "-mod=vendor", "-o", "bin/domus", "./cmd/domus")
	build.Dir = RepoRoot()
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("running go build: %w", err)
	}
	return binary, nil
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// RepoRoot is the repository root, three levels above e2e/internal/harness. It
// comes from this file's compile-time path, so the suite works from any
// working directory.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}
