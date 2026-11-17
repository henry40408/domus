package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		text    string
		want    slog.Level
		wantErr bool
	}{
		{"debug", slog.LevelDebug, false},
		{"info", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"trace", 0, true},
		{"INFO", 0, true},
		{"", 0, true},
	}
	for _, tc := range cases {
		got, err := parseLogLevel(tc.text)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("parseLogLevel(%q) = %v, %v", tc.text, got, err)
		}
		if tc.wantErr && !strings.Contains(err.Error(), tc.text) {
			t.Errorf("error %q does not name %q", err, tc.text)
		}
	}
}

func TestNewLoggerHonoursTheLevel(t *testing.T) {
	var buffer bytes.Buffer
	logger := newLogger(&buffer, slog.LevelWarn)
	logger.Info("hidden")
	logger.Warn("shown")
	out := buffer.String()
	if strings.Contains(out, "hidden") || !strings.Contains(out, "shown") {
		t.Fatalf("out = %q", out)
	}
}

func TestTheAuditTargetStaysAnAttribute(t *testing.T) {
	var buffer bytes.Buffer
	newLogger(&buffer, slog.LevelInfo).With("target", "audit").Info("login")
	if !strings.Contains(buffer.String(), "target=audit") {
		t.Fatalf("out = %q", buffer.String())
	}
}
