package timefmt

import "testing"

func TestEpochReadsAStampThatEndsAtTheSeconds(t *testing.T) {
	if got := Epoch("2026-10-01T12:44:56"); got != 1_790_858_696 {
		t.Fatalf("got %v", got)
	}
}

func TestEpochReadsAFractionThatEndsAtTheLastDigit(t *testing.T) {
	if got := Epoch("2026-10-01T12:44:56.500000"); got != 1_790_858_696.5 {
		t.Fatalf("got %v", got)
	}
}

func TestFloorDivRoundsDown(t *testing.T) {
	tests := []struct{ a, b, want int64 }{
		{7, 400, 0}, {-1, 400, -1}, {-400, 400, -1}, {-401, 400, -2}, {-800, 400, -2},
	}
	for _, tt := range tests {
		if got := floorDiv(tt.a, tt.b); got != tt.want {
			t.Errorf("floorDiv(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
