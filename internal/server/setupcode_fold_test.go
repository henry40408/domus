package server

import "testing"

func TestPlainSetupCodeTable(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"only dashes and spaces", "-- - --", "  "},
		{"lower case", "a1b2c-d3e4f", "A1B2CD3E4F"},
		{"mixed case", "aB1-Cd2", "AB1CD2"},
		{"spaces around", " a1b2c-d3e4f ", "A1B2CD3E4F"},
		{"inner space kept", "ab cd", "AB CD"},
		{"digits and punctuation", "1_2.3", "1_2.3"},
		{"edges of the letter range", "`a@z{", "`A@Z{"},
		{"turkish dotted capital i", "İ", "İ"},
		{"turkish dotless i", "ı", "I"},
		{"long s", "ſ", "S"},
		{"kelvin sign", "K", "K"},
		{"full-width letters", "ａｂｃ", "ＡＢＣ"},
		{"accented letter", "é", "É"},
		{"combining mark", "é", "É"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			equal(t, plainSetupCode(tt.in), tt.want)
		})
	}
}
