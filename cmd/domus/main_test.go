package main

import (
	"io"
	"testing"
)

func TestRunSucceeds(t *testing.T) {
	p := process{stdout: io.Discard, stderr: io.Discard}
	if code := run(p); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}
