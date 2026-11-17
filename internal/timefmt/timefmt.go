// Package timefmt formats and parses the Home Assistant style timestamps
// ("2026-10-01T12:34:56.123456+00:00").
package timefmt

import (
	"strconv"
	"time"
)

const layoutMicros = "2006-01-02T15:04:05.000000+00:00"

// RFC3339Micros formats t in UTC with six fractional digits.
func RFC3339Micros(t time.Time) string {
	return t.UTC().Format(layoutMicros)
}

// Epoch returns the seconds since 1970-01-01 of a RFC3339Micros timestamp, as
// the apps want them. The zone is ignored and only six fractional digits are
// read. Malformed fields count as 0.
func Epoch(ts string) float64 {
	y, m, d := field(ts, 0, 4), field(ts, 5, 7), field(ts, 8, 10)
	secs := daysFromCivil(y, m, d)*86_400 + field(ts, 11, 13)*3600 + field(ts, 14, 16)*60 + field(ts, 17, 19)
	return float64(secs) + fraction(ts)/1_000_000
}

func field(ts string, from, to int) int64 {
	if to > len(ts) {
		return 0
	}
	n, err := strconv.ParseInt(ts[from:to], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// fraction is the microseconds after the "." that follows the seconds.
func fraction(ts string) float64 {
	if len(ts) < 26 || ts[19] != '.' {
		return 0
	}
	n, err := strconv.ParseInt(ts[20:26], 10, 64)
	if err != nil {
		return 0
	}
	return float64(n)
}

// daysFromCivil is Howard Hinnant's algorithm: days since 1970-01-01.
func daysFromCivil(y, m, d int64) int64 {
	if m <= 2 {
		y--
	}
	era := floorDiv(y, 400)
	yoe := y - era*400
	shifted := m + 9
	if m > 2 {
		shifted = m - 3
	}
	doy := (153*shifted+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146_097 + doe - 719_468
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b < 0 {
		q--
	}
	return q
}
