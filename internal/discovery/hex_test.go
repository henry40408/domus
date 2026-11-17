package discovery

import "testing"

func TestIsHexDigitAtTheEdgesOfEachRange(t *testing.T) {
	for _, c := range []byte("09afAF") {
		if !isHexDigit(c) {
			t.Errorf("%q is a hex digit", c)
		}
	}
	for _, c := range []byte("/:`g@G") {
		if isHexDigit(c) {
			t.Errorf("%q is not a hex digit", c)
		}
	}
}
