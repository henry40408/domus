package auth

import "testing"

func TestThePasswordMayNotEqualTheUsernameIgnoringCase(t *testing.T) {
	tests := []struct {
		name     string
		password string
		username string
		valid    bool
	}{
		{"same case", "administrator1", "administrator1", false},
		{"mixed case", "AdMiNiStRaToR1", "administrator1", false},
		{"upper case", "ADMINISTRATOR1", "administrator1", false},
		{"digits and punctuation", "1234567890-=", "1234567890-=", false},
		{"different text", "administrator1", "administrator2", true},
		{"different length", "administrator1", "administrator", true},
		{"empty username", "administrator1", "", true},
		{"punctuation next to the letters", "@[`{@[`{@[`{", "@[`{@[`{@[`{", false},
		{"at sign is not a", "a@a@a@a@a@a@", "aAaAaAaAaAaA", true},
		{"turkish dotted capital i", "İstanbulkullanici", "istanbulkullanici", true},
		{"turkish dotless i", "ıstanbulkullanici", "Istanbulkullanici", true},
		{"long s", "passwordſafe12", "passwordsafe12", true},
		{"kelvin sign", "Kelvinuser123", "kelvinuser123", true},
		{"full-width letters", "ＡＢＣＤＥＦＧＨＩＪＫＬ", "ａｂｃｄｅｆｇｈｉｊｋｌ", true},
		{"accented letters", "ÉCOLEPRIMAIRE", "écoleprimaire", true},
		{"combining mark against precomposed", "cafécafecafe", "cafécafecafe", true},
		{"same non-ASCII text", "ÉCOLEPRIMAIRE", "ÉCOLEPRIMAIRE", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidPassword(tt.password, tt.username); got != tt.valid {
				t.Errorf("ValidPassword(%q, %q) = %v, want %v", tt.password, tt.username, got, tt.valid)
			}
		})
	}
}
