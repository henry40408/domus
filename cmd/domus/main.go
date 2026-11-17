// Command domus serves a Home Assistant compatible API for Philips Hue lights.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	exitFailure = 1
	exitUsage   = 2
)

// process is everything run needs from the outside world.
type process struct {
	args      []string
	lookupEnv func(string) (string, bool)
	stdout    io.Writer
	stderr    io.Writer
}

func main() {
	os.Exit(run(process{
		args:      os.Args[1:],
		lookupEnv: os.LookupEnv,
		stdout:    os.Stdout,
		stderr:    os.Stderr,
	}))
}

// usageError marks a configuration problem, which exits with exitUsage.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// run returns the exit code. Logs go to stdout; stderr carries the setup code
// and fatal errors.
func run(p process) int {
	err := runCommand(p)
	if err == nil {
		return 0
	}
	_, _ = fmt.Fprintf(p.stderr, "domus: %v\n", err)
	var usage usageError
	if errors.As(err, &usage) {
		return exitUsage
	}
	return exitFailure
}

// runCommand has no flags and one subcommand; any other argument is ignored
// and the server starts, as in the Rust build.
func runCommand(p process) error {
	// First: a retired variable must stop startup whatever else is set. It
	// exits with exitFailure, as in the other Go services, not exitUsage.
	if err := ensureNoLegacyEnvVars(p.lookupEnv); err != nil {
		return err
	}
	e, err := loadEnv(p.lookupEnv)
	if err != nil {
		return usageError{err}
	}
	if len(p.args) > 0 && p.args[0] == "healthcheck" {
		return healthcheck(e)
	}
	return runServer(p, e)
}
