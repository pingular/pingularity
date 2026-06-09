package util

import (
	"testing"
	"time"
)

func TestB2I(t *testing.T) {
	if B2I(true) != 1 || B2I(false) != 0 {
		t.Fatalf("B2I: got %d/%d, want 1/0", B2I(true), B2I(false))
	}
}

func TestRound1(t *testing.T) {
	for in, want := range map[float64]float64{1.24: 1.2, 1.26: 1.3, -1.26: -1.3, 0: 0} {
		if got := Round1(in); got != want {
			t.Errorf("Round1(%v)=%v want %v", in, got, want)
		}
	}
}

func TestRound2(t *testing.T) {
	for in, want := range map[float64]float64{3.14159: 3.14, 2.718: 2.72, -1.236: -1.24} {
		if got := Round2(in); got != want {
			t.Errorf("Round2(%v)=%v want %v", in, got, want)
		}
	}
}

func TestDurMS(t *testing.T) {
	for in, want := range map[time.Duration]float64{1500 * time.Microsecond: 1.5, 2 * time.Millisecond: 2, 0: 0} {
		if got := DurMS(in); got != want {
			t.Errorf("DurMS(%v)=%v want %v", in, got, want)
		}
	}
}

// HumanDur: compact format, and negative input clamps to 0 (the one non-obvious branch).
func TestHumanDur(t *testing.T) {
	for in, want := range map[int]string{45: "45s", 90: "1m 30s", 3700: "1h 1m", 0: "0s", -5: "0s"} {
		if got := HumanDur(in); got != want {
			t.Errorf("HumanDur(%d)=%q want %q", in, got, want)
		}
	}
}
