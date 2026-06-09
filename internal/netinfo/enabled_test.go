package netinfo

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func quietManager() *Manager {
	return NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A disabled Manager must make no network call at all. Refresh stamps UpdatedAt
// on EVERY attempt, including a failed one (it carries last-known data forward),
// so an UpdatedAt still at zero after Refresh is proof that nothing was even
// attempted. That matters more than it looks: this is the gate that stops the
// hourly connection-info loop sending the host's public IP to third parties
// while the dashboard says monitoring is paused.
func TestRefreshMakesNoCallWhenDisabled(t *testing.T) {
	m := quietManager()
	m.EnabledFn = func() bool { return false }

	m.Refresh(context.Background())

	if got := m.Get(); got.UpdatedAt != 0 {
		t.Errorf("disabled Refresh stamped UpdatedAt=%d; a lookup was attempted", got.UpdatedAt)
	}
}

// RefreshNow backs the dashboard's manual refresh button, and it must work even
// when automatic lookups are off. The setting means "stop doing this on your
// own", not "refuse when I click the button" - so an explicit click still
// fetches. This is the one path that is deliberately NOT gated, which is exactly
// why it needs a test: making it obey EnabledFn would look like a tightening
// rather than the regression it is.
func TestRefreshNowWorksWhenAutomaticLookupsAreOff(t *testing.T) {
	m := quietManager()
	m.EnabledFn = func() bool { return false }

	if got := m.RefreshNow(context.Background()); got.UpdatedAt == 0 {
		t.Error("RefreshNow made no attempt while disabled; the manual refresh button would be dead")
	}
}

// Turning the gate back on must let lookups through again. Asserting only that
// an attempt was MADE (UpdatedAt stamped) keeps this offline-safe: a sandbox
// with no network fails the fetch, and a failed fetch still stamps.
func TestRefreshResumesWhenReEnabled(t *testing.T) {
	on := false
	m := quietManager()
	m.EnabledFn = func() bool { return on }

	m.Refresh(context.Background())
	if m.Get().UpdatedAt != 0 {
		t.Fatal("lookup attempted while disabled")
	}

	on = true
	m.Refresh(context.Background())
	if m.Get().UpdatedAt == 0 {
		t.Error("re-enabled Refresh made no attempt; the gate is stuck off")
	}
}

// A nil hook means always on, so every caller that never sets one (tests, and
// any future embedder) behaves exactly as it did before the gate existed.
func TestNilEnabledFnMeansAlwaysOn(t *testing.T) {
	m := quietManager()
	if !m.enabled() {
		t.Error("nil EnabledFn reported disabled; existing callers would silently stop working")
	}
}
