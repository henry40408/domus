package auth

import (
	"strings"
	"testing"
)

func TestTheThrottleKeyKeepsExactlyTheLimitOfRunes(t *testing.T) {
	atLimit := strings.Repeat("é", keyNameRunes)
	if got := ThrottleKey("login", atLimit); got != "login:"+atLimit {
		t.Errorf("a name at the limit was cut: %q", got)
	}
	if got := ThrottleKey("login", atLimit+"x"); got != "login:"+atLimit {
		t.Errorf("a name over the limit was not cut: %q", got)
	}
}

func TestLowerASCIIHandlesTheEdgesOfTheLetterRange(t *testing.T) {
	tests := map[byte]byte{'@': '@', 'A': 'a', 'Z': 'z', '[': '['}
	for in, want := range tests {
		if got := lowerASCII(in); got != want {
			t.Errorf("lowerASCII(%q) = %q, want %q", in, got, want)
		}
	}
}
