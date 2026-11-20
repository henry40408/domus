package discovery

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henry40408/domus/internal/version"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestLoopbackIsNeverAdvertised(t *testing.T) {
	for _, bind := range []string{"127.0.0.1:8123", "[::1]:8123", "[fe80::1]:8123"} {
		if ip, ok := advertiseAddress(netip.MustParseAddrPort(bind)); ok {
			t.Errorf("%s advertised as %s", bind, ip)
		}
	}
	if Start(netip.MustParseAddrPort("127.0.0.1:8123"), t.TempDir(), quietLogger()) != nil {
		t.Error("Start advertised loopback")
	}
}

func TestASpecificBindAddressIsUsedAsIs(t *testing.T) {
	ip, ok := advertiseAddress(netip.MustParseAddrPort("192.168.1.20:8123"))
	if !ok || ip != netip.MustParseAddr("192.168.1.20") {
		t.Errorf("got %v %v", ip, ok)
	}
}

func TestTheUnspecifiedAddressResolvesToALANAddressOrNothing(t *testing.T) {
	ip, ok := advertiseAddress(netip.MustParseAddrPort("0.0.0.0:8123"))
	if ok && (ip.IsLoopback() || ip.IsUnspecified()) {
		t.Errorf("got %s", ip)
	}
}

func TestInstanceIdIsAStableUuid(t *testing.T) {
	dir := t.TempDir()
	first := instanceID(dir, quietLogger())
	if len(first) != 36 || strings.Count(first, "-") != 4 {
		t.Fatalf("not a uuid: %q", first)
	}
	if again := instanceID(dir, quietLogger()); again != first {
		t.Fatalf("id changed: %q -> %q", first, again)
	}
	bad := []string{"  \n", "abc", "ключ-ключ-ключ", "not.a.uuid.at.all.but.36.chars.long"}
	for _, content := range bad {
		if err := os.WriteFile(filepath.Join(dir, instanceFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		id := instanceID(dir, quietLogger())
		if !isUUID(id) || id == first {
			t.Errorf("%q -> %q", content, id)
		}
	}
}

func TestTxtRecordsCarryTheUrlAndVersion(t *testing.T) {
	p := properties(netip.MustParseAddr("192.168.1.20"), 8123, "u-1")
	want := map[string]string{
		"base_url":              "http://192.168.1.20:8123",
		"internal_url":          "http://192.168.1.20:8123",
		"external_url":          "",
		"location_name":         "domus",
		"requires_api_password": "true",
		"uuid":                  "u-1",
		"version":               version.HAVersion,
	}
	if len(p) != len(want) {
		t.Fatalf("got %v", p)
	}
	for k, v := range want {
		if got, ok := p[k]; !ok || got != v {
			t.Errorf("%s = %q (present %v), want %q", k, got, ok, v)
		}
	}
	p = properties(netip.MustParseAddr("fe80::1"), 80, "u")
	if p["base_url"] != "http://[fe80::1]:80" {
		t.Errorf("base_url = %q", p["base_url"])
	}
}

func TestRegisteringAndStoppingDoesNotPanic(t *testing.T) {
	// Sandboxes may have no multicast; the advertiser is optional.
	r := registration{name: "domus-test", ip: netip.MustParseAddr("192.168.1.20"), port: 8123, uuid: "u"}
	a, err := register(r, quietLogger())
	if err != nil {
		t.Skipf("no multicast here: %v", err)
	}
	a.Stop()
}

func TestInvalidNamesAreRejectedInsteadOfPanicking(t *testing.T) {
	r := registration{name: "", ip: netip.MustParseAddr("192.168.1.20"), port: 8123, uuid: "u"}
	if _, err := register(r, quietLogger()); err == nil {
		t.Fatal("empty name accepted")
	}
}

func TestAnUnwritableDataDirStillGivesAnId(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if !isUUID(instanceID(dir, quietLogger())) {
		t.Fatal("not a uuid")
	}
	if a := Start(netip.MustParseAddrPort("0.0.0.0:8123"), dir, quietLogger()); a != nil {
		a.Stop()
	}
}

func TestARoutableIpv6BindAddressIsAdvertised(t *testing.T) {
	ip, ok := advertiseAddress(netip.MustParseAddrPort("[2001:db8::5]:8123"))
	if !ok || ip != netip.MustParseAddr("2001:db8::5") {
		t.Errorf("got %v %v", ip, ok)
	}
}

func TestLocalIpIsFoundWhenTheHostHasARoute(t *testing.T) {
	// Asks the OS the same question as localIP; the test can only say
	// something on a host with a real route.
	probe, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		t.Skip("no route in this sandbox")
	}
	defer func() { _ = probe.Close() }()
	want := probe.LocalAddr().(*net.UDPAddr).IP
	if want.IsLoopback() || want.IsUnspecified() {
		t.Skip("the route leaves through loopback")
	}
	got, ok := localIP()
	if !ok || got.String() != want.String() {
		t.Errorf("localIP = %v %v, want %v", got, ok, want)
	}
}

func TestIsUuidAcceptsOnlyTheDashed36CharacterForm(t *testing.T) {
	if !isUUID("0123abcd-4567-89ef-ABCD-0123456789ab") {
		t.Error("valid uuid rejected")
	}
	for _, bad := range []string{
		"abc",
		"0123abcd456789efABCD0123456789ab",     // no dashes, 32 chars
		"0123abcd-4567-89ef-ABCD-0123456789a",  // 35 chars
		"0123abcd-4567-89ef-ABCD-0123456789ag", // not hex
		"0123abcd_4567-89ef-ABCD-0123456789ab", // wrong separator
		"0123abcd-4567_89ef-ABCD-0123456789ab",
		"0123abcd-4567-89ef_ABCD-0123456789ab",
		"0123abcd-4567-89ef-ABCD_0123456789ab",
		"0123abcd-4567-89ef-ABCD-0123456789a-", // dash in a hex place
		"-123abcd-4567-89ef-ABCD-0123456789ab",
		"g123abcd-4567-89ef-ABCD-0123456789ab", // not hex at the first place
		"0123abcd-4567-89ef-ABCD-0123456789a/",
		"0123abcd-4567-89ef-ABCD-0123456789a:",
		"0123abcd-4567-89ef-ABCD-0123456789a`",
		"0123abcd-4567-89ef-ABCD-0123456789a@",
		"0123abcd-4567-89ef-ABCD-0123456789aG",
	} {
		if isUUID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
