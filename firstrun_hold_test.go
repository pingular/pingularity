package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// A LOADED controller with Quick Setup unanswered and NO offer clock is the
// late-load window: a retry/SIGHUP Reload flips Loaded()==true and broadcasts
// the wake BEFORE materializeQuickSetup seeds the offer clock. QuickSetupHold
// reads the bare zero as "never offered -> release", and monitoringLive used
// to LATCH that release permanently - a fresh install started probing without
// first-run consent. The predicate must fail CLOSED there: hold a fresh
// install, latch nothing, and let the real 48h hold take over once the clock
// is seeded.
func TestMonitoringLiveHoldsFreshInstallWithUnseededOfferClock(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	set, err := settings.New(ctx, st, settings.Values{Monitoring: true})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	// The window's exact state: loaded, offer open, clock unseeded.
	if !set.Loaded() {
		t.Fatal("precondition: controller must be loaded")
	}
	if set.QuickSetupDone() {
		t.Fatal("precondition: quick setup must be unanswered")
	}
	if since, err := set.QuickSetupOfferSinceErr(ctx); err != nil || since != 0 {
		t.Fatalf("precondition: offer clock must be unseeded, got %d, %v", since, err)
	}

	live := newMonitoringLiveFn(ctx, set, nil)
	if live() {
		t.Fatal("fresh install with a loaded controller and an unseeded offer clock must HOLD, not probe (consent bypass)")
	}
	// Nothing may have latched in that window: once the clock IS seeded (the
	// materialize step catching up), the normal 48h hold must be in force, not
	// a permanently released latch.
	if err := set.EnsureQuickSetupOffer(ctx, time.Now().Unix()); err != nil {
		t.Fatalf("EnsureQuickSetupOffer: %v", err)
	}
	if live() {
		t.Fatal("hold released during the unseeded window and LATCHED - the seeded offer clock no longer holds")
	}
	// Answering releases for real.
	if err := set.SetQuickSetupDone(ctx, true); err != nil {
		t.Fatalf("SetQuickSetupDone: %v", err)
	}
	if !live() {
		t.Fatal("monitoring must run once quick setup is answered")
	}
}

// The fail-closed branch must not over-hold: the same window state on an
// install whose store already carries operator configuration (it consented
// long ago; only the answered marker hasn't been materialized yet) keeps
// monitoring running.
func TestMonitoringLiveEstablishedInstallRunsWithUnseededOfferClock(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	// Prior operator configuration = established (see hasPriorConfiguration).
	if err := st.SetSetting(ctx, "monitoring", "true"); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	set, err := settings.New(ctx, st, settings.Values{Monitoring: true})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if set.QuickSetupDone() {
		t.Fatal("precondition: quick setup must be unanswered")
	}
	if !newMonitoringLiveFn(ctx, set, nil)() {
		t.Fatal("established install must keep monitoring through the unseeded-clock window")
	}
}

// The recovery paths (settings-retry loop, SIGHUP) must seed the offer clock
// BEFORE calling Reload: Reload itself flips Loaded()==true and broadcasts
// the wake, so a post-Reload seed leaves the latch window open. This drives
// the pre-seed against a REAL unloaded controller on a REAL working store (a
// canceled context makes New's initial read fail exactly like a transient
// store fault, and a live context recovers it).
func TestQuickSetupOfferPreSeedRunsWhileUnloaded(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	set, err := settings.New(dead, st, settings.Values{Monitoring: true})
	if err == nil {
		t.Fatal("settings.New must fail on the canceled context (simulated failed first load)")
	}
	if set.Loaded() {
		t.Fatal("precondition: controller must be unloaded")
	}

	ctx := context.Background()
	p := &program{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.seedQuickSetupOfferEarly(ctx, set)
	if set.Loaded() {
		t.Fatal("pre-seed must not flip Loaded - only Reload may")
	}
	if since, err := set.QuickSetupOfferSinceErr(ctx); err != nil {
		t.Fatalf("offer clock read: %v", err)
	} else if since == 0 {
		t.Fatal("pre-seed left the offer clock unseeded on a fresh install - the latch window before materializeQuickSetup stays open")
	}
	// The recovery Reload now finds the clock already there: the hold is in
	// force from the first woken round.
	if err := set.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if newMonitoringLiveFn(ctx, set, nil)() {
		t.Fatal("hold must be in force after a recovered first load")
	}
}

// The pre-seed's placement lives in run()'s SIGHUP goroutine and in
// retrySettingsLoad's loop, neither unit-testable end-to-end - the same gap
// TestMainWiresFirstRunAndOptsHooks covers, with the same deliberately literal
// remedy: in both recovery paths the pre-seed must appear, and appear BEFORE
// the set.Reload call it protects.
func TestRecoveryPathsPreSeedBeforeReload(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, tc := range []struct{ name, anchor string }{
		{"settings-retry loop", "func (p *program) retrySettingsLoad"},
		{"SIGHUP handler", "case <-hup:"},
	} {
		i := strings.Index(s, tc.anchor)
		if i < 0 {
			t.Fatalf("%s: anchor %q not found in main.go", tc.name, tc.anchor)
		}
		rest := s[i:]
		j := strings.Index(rest, "set.Reload(")
		if j < 0 {
			t.Fatalf("%s: no set.Reload call after %q", tc.name, tc.anchor)
		}
		if !strings.Contains(rest[:j], "seedQuickSetupOfferEarly") {
			t.Errorf("%s: seedQuickSetupOfferEarly must run BEFORE set.Reload - Reload flips Loaded and broadcasts, and a post-Reload seed reopens the consent-hold latch window", tc.name)
		}
	}
}
