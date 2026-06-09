package speedtest

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
)

func TestAdaptiveInterval(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewScheduler(nil, nil, time.Hour, log)

	if got := s.curInterval(); got != time.Hour {
		t.Fatalf("base interval = %v, want 1h", got)
	}
	s.AdaptiveFn = func() bool { return true }
	if got := s.curInterval(); got != time.Hour {
		t.Fatalf("adaptive but last run healthy = %v, want base 1h", got)
	}
	s.lastUnhealthy.Store(true)
	// 1h/4 = 15m, but capped to adaptiveCap (5m) so even a long base samples densely.
	if got := s.curInterval(); got != adaptiveCap {
		t.Fatalf("adaptive+unhealthy from 1h = %v, want cap %v", got, adaptiveCap)
	}
	// A base whose quarter sits between the floor and the cap is used directly.
	s.interval = 8 * time.Minute // /4 = 2m
	if got := s.curInterval(); got != 2*time.Minute {
		t.Fatalf("adaptive+unhealthy from 8m = %v, want 2m", got)
	}
	// Already at the floor: no room to speed up, so it stays at the base.
	s.interval = settings.MinSpeed
	if got := s.curInterval(); got != settings.MinSpeed {
		t.Fatalf("adaptive at the floor = %v, want %v", got, settings.MinSpeed)
	}
	// Toggle off restores the base regardless of health.
	s.AdaptiveFn = func() bool { return false }
	s.interval = time.Hour
	if got := s.curInterval(); got != time.Hour {
		t.Fatalf("adaptive off = %v, want base 1h", got)
	}
}
