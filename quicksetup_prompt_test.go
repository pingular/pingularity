package main

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// bootQuickSetup runs the exact first-boot sequence run() performs for the
// given argv on a fresh store - config.ParseFlags -> settings.New ->
// materializeQuickSetup - and returns the pieces the assertions need. Any
// warn from the materialize step fails the test: on a working store the
// first-run decision must land cleanly.
func bootQuickSetup(t *testing.T, args []string) (context.Context, *settings.Controller, func() bool) {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.ParseFlags(args)
	if err != nil {
		t.Fatalf("ParseFlags(%v): %v", args, err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := settings.New(ctx, st, defaultSettings(cfg))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	p := &program{cfg: cfg}
	p.materializeQuickSetup(ctx, set, func(msg string, e error) {
		t.Fatalf("materializeQuickSetup warned: %s: %v", msg, e)
	})
	return ctx, set, newMonitoringLiveFn(ctx, set, nil)
}

// REPRO of the ignored explicit -quick-setup=prompt: the flag help promises
// the dialog is left for the first visit, but monitoring flags on the same
// command line used to auto-consent (config.MonitoringConsent), so
// materializeQuickSetup marked Quick Setup answered at boot - monitoring
// started immediately and the dialog was suppressed permanently. Explicitly
// passed, prompt must be authoritative: dialog pending, monitoring held.
func TestExplicitPromptWithMonitoringFlagsHoldsFirstRun(t *testing.T) {
	ctx, set, live := bootQuickSetup(t, []string{"-quick-setup=prompt", "-interval", "1s"})
	if set.QuickSetupDone() {
		t.Fatal("-quick-setup=prompt was overridden by -interval: Quick Setup marked answered, the dialog is suppressed permanently")
	}
	since, err := set.QuickSetupOfferSinceErr(ctx)
	if err != nil {
		t.Fatalf("offer clock read: %v", err)
	}
	if since == 0 {
		t.Fatal("offer clock not seeded; the dialog would never be offered")
	}
	if !settings.QuickSetupHold(set.QuickSetupDone(), since, time.Now().Unix()) {
		t.Fatal("first-run hold not in force despite an explicit -quick-setup=prompt")
	}
	if live() {
		t.Fatal("monitoring started immediately despite an explicit -quick-setup=prompt; the dialog's Start monitoring button would be lying")
	}
}

// The implicit spelling is unchanged: monitoring flags WITHOUT -quick-setup
// are headless consent, so the service starts monitoring at boot with the
// dialog marked answered.
func TestImplicitMonitoringFlagsStillAutoConsent(t *testing.T) {
	_, set, live := bootQuickSetup(t, []string{"-interval", "1s"})
	if !set.QuickSetupDone() {
		t.Fatal("implicit monitoring flags must still consent (headless installs would be held 48h)")
	}
	if !live() {
		t.Fatal("monitoring must run immediately on consent-by-flags")
	}
}

// Explicit skip is unchanged: direct headless consent, answered at boot,
// monitoring running - with or without monitoring flags beside it.
func TestExplicitSkipStillConsents(t *testing.T) {
	for _, args := range [][]string{
		{"-quick-setup=skip"},
		{"-quick-setup=skip", "-interval", "1s"},
	} {
		_, set, live := bootQuickSetup(t, args)
		if !set.QuickSetupDone() {
			t.Fatalf("%v: explicit skip must mark Quick Setup answered", args)
		}
		if !live() {
			t.Fatalf("%v: explicit skip must start monitoring immediately", args)
		}
	}
}
