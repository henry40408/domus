package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/henry40408/domus/internal/store"
)

func TestAGroupNameMayUseEveryEdgeOfItsCharacterRanges(t *testing.T) {
	for _, name := range []string{"a", "z", "A", "Z", "0", "9", "_", "-", "a-Z_09", strings.Repeat("x", maxGroupNameBytes)} {
		if !validGroupName(name) {
			t.Errorf("%q must be valid", name)
		}
	}
	for _, name := range []string{"", "`", "{", "@", "[", "/", ":", " ", "é", strings.Repeat("x", maxGroupNameBytes+1)} {
		if validGroupName(name) {
			t.Errorf("%q must be invalid", name)
		}
	}
}

func TestATokenMayExpireInOneToThirtySixFiftyDays(t *testing.T) {
	app := newTestApp(t).app
	for days, wantOK := range map[int64]bool{0: false, 1: true, 3650: true, 3651: false, -1: false} {
		_, refusal := app.tokenExpiry(&days)
		if (refusal == nil) != wantOK {
			t.Errorf("%d days: refusal %v, want accepted %v", days, refusal, wantOK)
		}
	}
	expires, refusal := app.tokenExpiry(nil)
	if expires != nil || refusal != nil {
		t.Errorf("no expiry asked: %v, %v", expires, refusal)
	}
}

func TestATransitionOfZeroSecondsIsAccepted(t *testing.T) {
	ms, err := requestedTransition(map[string]any{"transition": 0.0})
	if err != nil || ms == nil || *ms != 0 {
		t.Fatalf("got %v, %v", ms, err)
	}
	if _, err := requestedTransition(map[string]any{"transition": -0.5}); err == nil {
		t.Fatal("a negative transition must be refused")
	}
}

func TestInUnitRangeIncludesBothEnds(t *testing.T) {
	for v, want := range map[float64]bool{0: true, 1: true, 0.5: true, -0.001: false, 1.001: false} {
		if got := inUnitRange(v); got != want {
			t.Errorf("inUnitRange(%v) = %v, want %v", v, got, want)
		}
	}
}

func TestRemEuclidIsNeverNegativeAndKeepsZero(t *testing.T) {
	for _, tt := range []struct{ v, divisor, want float64 }{
		{360, 360, 0}, {0, 360, 0}, {-30, 360, 330}, {30, 360, 30}, {-360, 360, 0},
	} {
		if got := remEuclid(tt.v, tt.divisor); got != tt.want {
			t.Errorf("remEuclid(%v, %v) = %v, want %v", tt.v, tt.divisor, got, tt.want)
		}
	}
}

func TestAMissingFieldIsReportedWhereTheObjectEnds(t *testing.T) {
	app := newTestApp(t)
	body := "{\n  \"username\": \"a\"\n  }"
	rec := app.adminDo(t, http.MethodPost, "/api/domus/login", "", body)
	want := "missing field `password` at line 3 column 3"
	if rec.Code != http.StatusUnprocessableEntity || !strings.HasSuffix(rec.Body.String(), want) {
		t.Fatalf("%d %q, want it to end with %q", rec.Code, rec.Body, want)
	}
}

func TestHeadOnAGetRouteIsNotAWrongMethod(t *testing.T) {
	app := newTestApp(t)
	_, err := app.store.CreateFirstAdmin(bg, "admin", "x")
	noError(t, err)
	app.addToken(t, "secret", store.NewToken{UserID: 1, Name: "t"})
	rec := app.do(t, request{method: http.MethodHead, path: "/api/states", headers: map[string]string{"Authorization": "Bearer secret"}})
	if rec.Code != http.StatusOK || rec.Header().Get("Allow") != "" {
		t.Fatalf("status %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = app.do(t, request{method: http.MethodPost, path: "/api/states", headers: map[string]string{"Authorization": "Bearer secret"}})
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET,HEAD" {
		t.Fatalf("status %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestEveryDefaultTimingIsPositive(t *testing.T) {
	got := Timing{}.withDefaults()
	want := Timing{
		EventsRecheck: defaultEventsRecheck, EventsKeepAlive: defaultEventsKeepAlive,
		WebsocketAuth: defaultWebsocketAuth, WebsocketRecheck: defaultWebsocketRecheck,
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestACodeThatExpiresRightNowIsSweptFromAFullTable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	codes := newAuthCodes(func() time.Time { return now })
	for i := range maxCodes {
		codes.codes[string(rune('a'+i%26))+strings.Repeat("x", i)] = pendingCode{userID: 1, clientID: iosClient, expires: now.Unix()}
	}
	codes.issue(1, iosClient)
	if len(codes.codes) != 1 {
		t.Fatalf("%d codes left, want only the new one", len(codes.codes))
	}
}
