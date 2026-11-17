// Command domus serves a Home Assistant compatible API for Philips Hue lights.
package main

import (
	"io"
	"os"
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

// run returns the exit code. It is a skeleton until the server is ported.
func run(process) int {
	return 0
}
