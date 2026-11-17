package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestWildcardBindsAreReachedThroughLoopback(t *testing.T) {
	url := func(bind string) string { return healthURL(netip.MustParseAddrPort(bind)) }
	cases := map[string]string{
		"0.0.0.0:8123":     "http://127.0.0.1:8123/health",
		"[::]:9000":        "http://[::1]:9000/health",
		"192.168.1.5:80":   "http://192.168.1.5:80/health",
		"127.0.0.1:60000":  "http://127.0.0.1:60000/health",
		"[::1]:8123":       "http://[::1]:8123/health",
		"[2001:db8::5]:80": "http://[2001:db8::5]:80/health",
	}
	for bind, want := range cases {
		if got := url(bind); got != want {
			t.Errorf("healthURL(%s) = %q, want %q", bind, got, want)
		}
	}
}

func TestASpecificIPv6BindIsKept(t *testing.T) {
	got := healthURL(netip.MustParseAddrPort("[2001:db8::5]:80"))
	if got != "http://[2001:db8::5]:80/health" {
		t.Fatalf("url = %q", got)
	}
}

func TestCheckFollowsTheServerStatus(t *testing.T) {
	// Nothing listens here: connection refused.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := healthURL(netip.MustParseAddrPort(closed.Addr().String()))
	_ = closed.Close()
	if checkHealth(url) == nil {
		t.Fatal("a closed port was healthy")
	}

	// A live server answering 200 is healthy, 503 is not.
	for _, tc := range []struct {
		status  int
		healthy bool
	}{{http.StatusOK, true}, {http.StatusServiceUnavailable, false}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		err := checkHealth(server.URL + "/health")
		server.Close()
		if (err == nil) != tc.healthy {
			t.Errorf("status %d: err = %v", tc.status, err)
		}
		if err != nil && !strings.Contains(err.Error(), "answered") {
			t.Errorf("status %d: err = %v does not say what answered", tc.status, err)
		}
	}
}
