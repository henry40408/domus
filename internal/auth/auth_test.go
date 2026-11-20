package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestConstantTimeEqualSeesEveryDifference(t *testing.T) {
	tests := []struct {
		a, b []byte
		want bool
	}{
		{[]byte("abc"), []byte("abc"), true},
		{[]byte("abc"), []byte("abd"), false},
		{[]byte("abc"), []byte("ab"), false},
		// Two differing bytes with the same xor must not cancel each other out.
		{[]byte{1, 1}, []byte{0, 0}, false},
		{[]byte{0xff, 0xff}, []byte{0, 0}, false},
	}
	for _, tt := range tests {
		if got := ConstantTimeEqual(tt.a, tt.b); got != tt.want {
			t.Errorf("ConstantTimeEqual(%v, %v) = %v", tt.a, tt.b, got)
		}
	}
}

func TestSHA256KnownVector(t *testing.T) {
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := SHA256Hex("abc"); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestRandomHexLength(t *testing.T) {
	a, b := RandomHex(16), RandomHex(16)
	if len(a) != 32 {
		t.Fatalf("len %d", len(a))
	}
	if a == b {
		t.Fatal("two random values are equal")
	}
}

func TestRandomBase32UsesOnlyTheAlphabet(t *testing.T) {
	code := RandomBase32(1000)
	if len(code) != 1000 {
		t.Fatalf("len %d", len(code))
	}
	for i := 0; i < len(code); i++ {
		if !strings.ContainsRune(base32Alphabet, rune(code[i])) {
			t.Fatalf("character %q is outside the alphabet", code[i])
		}
	}
}

func TestPasswordRoundtrip(t *testing.T) {
	stored := HashPassword("hunter2")
	if !VerifyPassword("hunter2", stored) {
		t.Error("right password refused")
	}
	if VerifyPassword("hunter3", stored) {
		t.Error("wrong password accepted")
	}
	if VerifyPassword("hunter2", "garbage") {
		t.Error("malformed hash accepted")
	}
	if VerifyPassword("hunter2", "zz$00") {
		t.Error("non-hex salt accepted")
	}
	if stored == HashPassword("hunter2") {
		t.Error("salt must differ")
	}
}

func TestStoredFormatIsSaltDollarHash(t *testing.T) {
	stored := HashPassword("pw")
	if !regexp.MustCompile(`^[0-9a-f]{32}\$[0-9a-f]{64}$`).MatchString(stored) {
		t.Fatalf("unexpected format %q", stored)
	}
}

func TestPasswordsMadeByTheRustServerVerify(t *testing.T) {
	raw, err := os.ReadFile("testdata/password_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		User     string `json:"user"`
		Password string `json:"password"`
		Stored   string `json:"stored"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) < 3 {
		t.Fatalf("expected at least 3 vectors, got %d", len(vectors))
	}
	for _, v := range vectors {
		if !VerifyPassword(v.Password, v.Stored) {
			t.Errorf("%s: Rust hash does not verify", v.User)
		}
		if VerifyPassword(v.Password+"x", v.Stored) {
			t.Errorf("%s: wrong password accepted", v.User)
		}
	}
}

func TestVerifyDummyAndWarmDummyDoNotPanic(t *testing.T) {
	WarmDummy()
	VerifyDummy("anything")
	if !VerifyPassword("domus dummy password", dummyHash()) {
		t.Fatal("the dummy hash does not match its own password")
	}
}

func TestLocksAfterFiveFailuresAndBacksOff(t *testing.T) {
	var g LoginGuard
	for range 4 {
		g.Fail("a", 1000)
		if wait := g.Check("a", 1000); wait != 0 {
			t.Fatalf("locked early: %d", wait)
		}
	}
	g.Fail("a", 1000)
	for _, c := range []struct{ now, want int64 }{{1000, 60}, {1059, 1}, {1060, 0}} {
		if got := g.Check("a", c.now); got != c.want {
			t.Fatalf("Check at %d = %d, want %d", c.now, got, c.want)
		}
	}

	// the next failure doubles it
	g.Fail("a", 1060)
	if got := g.Check("a", 1060); got != 120 {
		t.Fatalf("got %d, want 120", got)
	}
	// and it is capped at 15 minutes
	now := int64(1060)
	for range 10 {
		now += 100
		g.Fail("a", now)
	}
	if got := g.Check("a", now); got != 900 {
		t.Fatalf("got %d, want 900", got)
	}
	// other names are unaffected
	if got := g.Check("b", now); got != 0 {
		t.Fatalf("other name locked: %d", got)
	}
}

func TestLockDurationsDoubleFromSixtyToNineHundred(t *testing.T) {
	var g LoginGuard
	for f, want := range []int64{0, 0, 0, 0, 60, 120, 240, 480, 900, 900} {
		g.Fail("a", 0)
		if got := g.Check("a", 0); got != want {
			t.Fatalf("after %d failures: %d, want %d", f+1, got, want)
		}
	}
}

func TestSuccessResetsAndIdleFailuresExpire(t *testing.T) {
	var g LoginGuard
	for range 5 {
		g.Fail("a", 0)
	}
	if g.Check("a", 0) == 0 {
		t.Fatal("not locked")
	}
	g.Succeed("a")
	if got := g.Check("a", 0); got != 0 {
		t.Fatalf("still locked: %d", got)
	}

	for range 4 {
		g.Fail("b", 0)
	}
	// long idle: the count starts over, so one more failure does not lock
	g.Fail("b", 10_000)
	if got := g.Check("b", 10_000); got != 0 {
		t.Fatalf("locked after idle: %d", got)
	}
}

func TestPrunesIdleEntriesWhenFull(t *testing.T) {
	var g LoginGuard
	for i := range maxEntries {
		g.Fail(fmt.Sprintf("n%d", i), 0)
	}
	g.Fail("fresh", 10_000)
	if len(g.entries) >= maxEntries {
		t.Fatalf("%d entries left", len(g.entries))
	}
}

func TestPruningKeepsNamesThatFailedWithinTheLastLockPeriod(t *testing.T) {
	var g LoginGuard
	const now = 100_000
	g.Fail("recent", now-1)
	// Locked for the full 15 minutes, and that lock ends exactly now.
	for range 9 {
		g.Fail("ends-now", now-maxLockSecs)
	}
	g.Fail("just-inside", now-maxLockSecs+1)
	for i := range maxEntries - 3 {
		g.Fail(fmt.Sprintf("idle%d", i), 0)
	}
	g.Fail("trigger", now)

	for _, key := range []string{"recent", "just-inside", "trigger"} {
		if _, ok := g.entries[key]; !ok {
			t.Errorf("%s was pruned", key)
		}
	}
	for _, key := range []string{"ends-now", "idle0"} {
		if _, ok := g.entries[key]; ok {
			t.Errorf("%s was kept", key)
		}
	}
	if len(g.entries) != 3 {
		t.Fatalf("%d entries, want 3", len(g.entries))
	}
}

func TestFailuresOlderThanTheLongestLockStartOver(t *testing.T) {
	var g LoginGuard
	for range 4 {
		g.Fail("a", 0)
	}
	// Exactly at the limit the old failures still count, so this is the fifth.
	g.Fail("a", maxLockSecs)
	if got := g.Check("a", maxLockSecs); got != baseLockSecs {
		t.Fatalf("got %d, want %d", got, baseLockSecs)
	}
}

func TestKeyIsCaseInsensitiveAndBounded(t *testing.T) {
	if got := ThrottleKey("login", " Admin "); got != "login:admin" {
		t.Fatalf("got %q", got)
	}
	if got := ThrottleKey("login", strings.Repeat("x", 500)); len(got) != len("login:")+64 {
		t.Fatalf("len %d", len(got))
	}
	// 64 characters, not 64 bytes
	if got := ThrottleKey("login", strings.Repeat("字", 100)); got != "login:"+strings.Repeat("字", 64) {
		t.Fatalf("got %q", got)
	}
}

func TestConcurrentUseIsSafe(t *testing.T) {
	var g LoginGuard
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range 200 {
				g.Fail("a", int64(i))
				g.Check("a", int64(i))
				g.Succeed("a")
			}
		}()
	}
	for range 8 {
		<-done
	}
}

func TestUsernameRuleIsTrimmedNonEmpty64CharsNoControl(t *testing.T) {
	ok := func(name string) bool { return ValidUsername(strings.TrimSpace(name)) }
	tests := []struct {
		name string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"  bob  ", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{strings.Repeat("字", 64), true}, // 64 characters, 192 bytes
		{strings.Repeat("字", 65), false},
		{"a\nb", false},
		{"a\x00b", false},
		{"a\u007fb", false},
		{"a b", true},
		{"Bob_1-x.y@example.com", true},
		{"王小明", true},
	}
	for _, tt := range tests {
		if got := ok(tt.name); got != tt.want {
			t.Errorf("ValidUsername(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestPasswordRules(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		password string
		want     bool
	}{
		{"eleven characters", "admin", "elevenchars", false},
		{"same as the username, any case", "administrator", "Administrator", false},
		{"129 characters", "admin", strings.Repeat("x", 129), false},
		{"128 is the limit", "admin", strings.Repeat("x", 128), true},
		{"exactly 12 characters, counted as characters", "admin", "密碼密碼密碼密碼密碼密碼", true},
		{"11 CJK characters", "admin", "密碼密碼密碼密碼密碼密", false},
		{"unicode case is folded", "ÄÄÄÄÄÄÄÄÄÄÄÄ", "ää" + "ääääääääää", false},
	}
	for _, tt := range tests {
		if got := ValidPassword(tt.password, tt.user); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}
