package version

import "testing"

func TestModuleVersionKeepsARealVersionOnly(t *testing.T) {
	tests := map[string]string{"": "unknown", "(devel)": "unknown", "v1.2.3": "v1.2.3"}
	for in, want := range tests {
		if got := moduleVersion(in); got != want {
			t.Errorf("moduleVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
