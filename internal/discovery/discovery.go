// Package discovery advertises domus over mDNS (Bonjour) so the Home
// Assistant Companion apps list it as a server instead of asking the user to
// type its URL. It only advertises; it never browses.
package discovery

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brutella/dnssd"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/version"
)

const (
	serviceType  = "_home-assistant._tcp"
	instanceFile = "instance-id"
	stopWait     = time.Second
)

// advertiseAddress is the address to put in the advertised URL: the bind
// address itself, or this host's LAN address when it listens on all
// interfaces. ok is false for loopback, which no other device can reach.
func advertiseAddress(bind netip.AddrPort) (ip netip.Addr, ok bool) {
	addr := bind.Addr()
	switch {
	case addr.IsLoopback():
		return netip.Addr{}, false
	case addr.IsUnspecified():
		return localIP()
	// Without a zone ID a link-local IPv6 address is of no use to the phone.
	case addr.Is6() && addr.IsLinkLocalUnicast():
		return netip.Addr{}, false
	}
	return addr, true
}

// localIP is the address of the interface that would reach the LAN.
// Connecting a UDP socket sends nothing; it only makes the OS pick a route.
func localIP() (netip.Addr, bool) {
	conn, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return netip.Addr{}, false
	}
	defer func() { _ = conn.Close() }()
	udp, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	ip, ok := netip.AddrFromSlice(udp.IP)
	if !ok || ip.IsLoopback() || ip.IsUnspecified() {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

// instanceID is a UUID that stays the same across restarts, so the apps
// recognise the server as one they know.
func instanceID(dataDir string, logger *slog.Logger) string {
	path := filepath.Join(dataDir, instanceFile)
	if saved, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(saved)); isUUID(id) {
			return id
		}
	}
	h := auth.RandomHex(16)
	id := strings.Join([]string{h[:8], h[8:12], h[12:16], h[16:20], h[20:]}, "-")
	// Write then rename, so a crash cannot leave a half-written id behind.
	// Without a saved id the apps see a new server after every restart.
	if err := saveAtomically(path, id); err != nil {
		logger.Warn("cannot save the instance id; apps will see a new server after a restart",
			"path", path, "error", err)
	}
	return id
}

func saveAtomically(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func isUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(s[:8] + s[9:13] + s[14:18] + s[19:23] + s[24:])
	return err == nil
}

// properties are the TXT records Home Assistant publishes; the apps read the
// URL and the name from them. The library keeps them in a map, so the order
// on the wire is not fixed.
func properties(ip netip.Addr, port uint16, uuid string) map[string]string {
	url := "http://" + netip.AddrPortFrom(ip, port).String()
	return map[string]string{
		"base_url":              url,
		"internal_url":          url,
		"external_url":          "",
		"location_name":         "domus",
		"requires_api_password": "true",
		"uuid":                  uuid,
		"version":               version.HAVersion,
	}
}

// Advertiser keeps the service announced until Stop is called.
type Advertiser struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

// Start begins advertising. Failures only cost discovery, so they are logged
// and nil is returned; the server carries on.
func Start(bind netip.AddrPort, dataDir string, logger *slog.Logger) *Advertiser {
	ip, ok := advertiseAddress(bind)
	if !ok {
		logger.Info("not advertising over mDNS: not an address other devices can reach",
			"bind", bind.String())
		return nil
	}
	uuid := instanceID(dataDir, logger)
	// The short id keeps two domus instances on one LAN from replacing each other.
	name := "domus-" + uuid[:8]
	advertiser, err := register(registration{name: name, ip: ip, port: bind.Port(), uuid: uuid}, logger)
	if err != nil {
		logger.Warn("mDNS discovery disabled", "error", err)
		return nil
	}
	logger.Info("advertising on the LAN", "service", serviceType+".local.", "address", netip.AddrPortFrom(ip, bind.Port()).String())
	return advertiser
}

type registration struct {
	name string
	ip   netip.Addr
	port uint16
	uuid string
}

func register(r registration, logger *slog.Logger) (*Advertiser, error) {
	service, err := dnssd.NewService(dnssd.Config{
		Name: r.name,
		Type: serviceType,
		Host: r.name,
		// Pin the address: the library would otherwise announce every
		// interface address, while the TXT URL names this one.
		IPs:  []net.IP{r.ip.AsSlice()},
		Port: int(r.port),
		Text: properties(r.ip, r.port, r.uuid),
	})
	if err != nil {
		return nil, err
	}
	responder, err := dnssd.NewResponder()
	if err != nil {
		return nil, err
	}
	if _, err := responder.Add(service); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Cancelling the context is how Stop ends the responder; the library
		// then sends the goodbye packets.
		if err := responder.Respond(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("mDNS responder stopped", "error", err)
		}
	}()
	return &Advertiser{cancel: cancel, done: done}, nil
}

// Stop ends the advertisement. The goodbye packets go out while the responder
// shuts down, so wait a moment for them: phones then drop the entry instead of
// listing a stopped server until its TTL runs out.
func (a *Advertiser) Stop() {
	a.cancel()
	select {
	case <-a.done:
	case <-time.After(stopWait):
	}
}
