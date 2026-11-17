package version

import "testing"

func TestInjectedVersionWins(t *testing.T) {
	t.Cleanup(func() { version = "" })
	version = "v1.2.3"
	if got := Version(); got != "v1.2.3" {
		t.Fatalf("got %q", got)
	}
}

func TestDevIsTreatedAsUnset(t *testing.T) {
	t.Cleanup(func() { version = "" })
	version = "dev"
	if got := Version(); got == "dev" || got == "" {
		t.Fatalf("got %q", got)
	}
}

// A test binary has no module version ("" or "(devel)"); neither may leak out.
func TestAnEmptyOrDevelModuleVersionFallsBackToUnknown(t *testing.T) {
	t.Cleanup(func() { version = "" })
	version = ""
	if got := Version(); got != "unknown" {
		t.Fatalf("got %q, want unknown", got)
	}
}

func TestVersionIsNeverEmpty(t *testing.T) {
	if Version() == "" {
		t.Fatal("empty version")
	}
}

func TestCacheable(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"v1.2.3", true},
		{"0.9.0-3-gabc1234", true},
		{"", false},
		{"dev", false},
		{"unknown", false},
		{"0.9.0-dirty", false},
		{"v1.2.4-0.20261117-abc+dirty", false},
	}
	for _, tt := range tests {
		if got := Cacheable(tt.version); got != tt.want {
			t.Errorf("Cacheable(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}
