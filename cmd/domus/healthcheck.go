package main

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"time"
)

const healthcheckTimeout = 4 * time.Second

// healthURL is the address a local process reaches the server on: a wildcard
// bind is not connectable.
func healthURL(bind netip.AddrPort) string {
	ip := bind.Addr()
	switch {
	case ip.Is4() && ip.IsUnspecified():
		ip = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	case ip.Is6() && ip.IsUnspecified():
		ip = netip.IPv6Loopback()
	}
	return "http://" + netip.AddrPortFrom(ip, bind.Port()).String() + "/health"
}

// checkHealth asks the running server whether it is healthy; the error says why not.
func checkHealth(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return nil
}

// healthcheck is `domus healthcheck`: it asks the server on this host whether
// it is healthy. The container's HEALTHCHECK uses it because the image has no
// shell or curl.
func healthcheck(e env) error {
	if err := checkHealth(healthURL(e.bind)); err != nil {
		return fmt.Errorf("unhealthy: %w", err)
	}
	return nil
}
