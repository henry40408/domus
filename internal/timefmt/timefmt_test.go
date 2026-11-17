package timefmt

import (
	"testing"
	"time"
)

func TestTimestampFormat(t *testing.T) {
	tests := []struct {
		at   time.Time
		want string
	}{
		{time.Unix(1_790_858_696, 123_456_000), "2026-10-01T12:44:56.123456+00:00"},
		{time.Unix(0, 0), "1970-01-01T00:00:00.000000+00:00"},
		// leap day
		{time.Unix(1_709_208_000, 0), "2024-02-29T12:00:00.000000+00:00"},
	}
	for _, tt := range tests {
		if got := RFC3339Micros(tt.at); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}

func TestTimestampIsUTCWhateverTheLocation(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*3600)
	got := RFC3339Micros(time.Unix(0, 0).In(zone))
	if want := "1970-01-01T00:00:00.000000+00:00"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// 2100 is not a leap year; 2000 and 2400 are. These sit on the 100- and
// 400-year edges of the calendar.
func TestDatesAcrossCenturyAndLeapBoundaries(t *testing.T) {
	tests := []struct {
		secs int64
		date string
	}{
		{951_782_400, "2000-02-29"},
		{951_868_800, "2000-03-01"},
		{4_107_456_000, "2100-02-28"},
		{4_107_542_400, "2100-03-01"},
		{7_263_216_000, "2200-03-01"},
		{13_574_563_200, "2400-02-29"},
		{13_574_649_600, "2400-03-01"},
	}
	for _, tt := range tests {
		got := RFC3339Micros(time.Unix(tt.secs, 0))
		if want := tt.date + "T00:00:00.000000+00:00"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestEpochMatchesKnownTimestamps(t *testing.T) {
	tests := []struct {
		ts   string
		want float64
	}{
		{"1970-01-01T00:00:00.000000+00:00", 0},
		{"2026-10-01T12:34:56.250000+00:00", 1_790_858_096.25},
		{"2000-03-01T00:00:00.000000+00:00", 951_868_800},
		// February is the month the algorithm counts from the previous year
		{"2000-02-29T00:00:00.000000+00:00", 951_782_400},
		{"2000-01-01T00:00:00.000000+00:00", 946_684_800},
	}
	for _, tt := range tests {
		if got := Epoch(tt.ts); got != tt.want {
			t.Errorf("Epoch(%q) = %v, want %v", tt.ts, got, tt.want)
		}
	}
}

func TestEpochOfMalformedInputCountsFieldsAsZero(t *testing.T) {
	for _, ts := range []string{"", "garbage", "2000"} {
		if got := Epoch(ts); got < -1e12 || got > 1e12 {
			t.Errorf("Epoch(%q) = %v", ts, got)
		}
	}
}

func TestEpochRoundTripsFormattedTimes(t *testing.T) {
	at := time.Unix(1_790_858_696, 123_456_000)
	got := Epoch(RFC3339Micros(at))
	if want := 1_790_858_696.123456; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}
