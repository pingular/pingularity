package settings

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// One rule decides both the dialog offer and the boot-time monitoring hold;
// its table is small enough to pin whole.
func TestQuickSetupHoldTable(t *testing.T) {
	now := int64(1_800_000_000)
	cases := []struct {
		name  string
		done  bool
		since int64
		now   int64
		want  bool
	}{
		{"unseeded clock never holds", false, 0, now, false},
		// now < grace: if the !=0 leg were dropped, now-0 would be inside the
		// grace and this case would wrongly hold - the mutation that survives
		// the plain zero case above.
		{"unseeded clock never holds even young", false, 0, 100, false},
		{"open offer holds", false, now - 3600, now, true},
		{"grace boundary releases", false, now - quickSetupGrace, now, false},
		{"expired offer releases", false, now - quickSetupGrace - 1, now, false},
		{"answered releases regardless", true, now - 3600, now, false},
		{"answered with no clock releases", true, 0, now, false},
		// A future offer clock (now < since) still HOLDS - releasing it would
		// start monitoring on a fresh install with the user never having answered
		// Quick Setup (default Monitoring is true, so the hold is the only thing
		// pausing a fresh install). The years-long-fallback bug is fixed by
		// re-anchoring at boot (EnsureQuickSetupOffer), not by expiring here.
		{"future offer clock still holds (no consent bypass)", false, now + 10*365*24*3600, now, true},
		{"future offer clock by one second still holds", false, now + 1, now, true},
	}
	for _, c := range cases {
		if got := QuickSetupHold(c.done, c.since, c.now); got != c.want {
			t.Errorf("%s: QuickSetupHold(%v,%d) = %v, want %v", c.name, c.done, c.since, got, c.want)
		}
	}
}

// Boot's one-time decision. The deadlock this design exists to avoid: the
// install anchor (first_seen_ts) only persists once samples exist, and the
// hold prevents samples - so the offer runs on its own clock and ALWAYS
// expires, browser or no browser.
func TestEnsureQuickSetupOffer(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Unix()
	open := func(t *testing.T) *Controller {
		t.Helper()
		st, err := store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		c, err := New(ctx, st, Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	t.Run("fresh install opens the offer", func(t *testing.T) {
		c := open(t)
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if since := c.QuickSetupOfferSince(ctx); since != now {
			t.Fatalf("offer clock = %d, want %d", since, now)
		}
		if c.QuickSetupDone() {
			t.Fatal("a fresh install must not be marked answered")
		}
	})

	t.Run("a future offer clock is re-anchored, not left to stall the fallback", func(t *testing.T) {
		// The machine booted skewed ahead (offer clock in the future), then the
		// clock was corrected. A later boot must pull the offer clock back to now
		// so the 48h fallback measures from now - NOT leave it years out. The
		// offer is still open (not answered); the hold stays active meanwhile.
		c := open(t)
		future := now + 10*365*24*3600
		if err := c.store.SetSetting(ctx, "quick_setup_offer_since", strconv.FormatInt(future, 10)); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if since := c.QuickSetupOfferSince(ctx); since != now {
			t.Fatalf("offer clock = %d, want re-anchored to %d", since, now)
		}
		if c.QuickSetupDone() {
			t.Fatal("re-anchoring must not answer the offer")
		}
	})

	t.Run("a future clock on an ESTABLISHED install is answered, not re-held", func(t *testing.T) {
		// The F3 regression: an install that already ran (has history) and whose
		// offer clock is now in the future (backward clock correction) must be
		// materialized as answered - NOT re-anchored, which would re-hold and
		// re-pause a running install for a fresh 48h.
		c := open(t)
		future := now + 10*365*24*3600
		if err := c.store.SetSetting(ctx, "quick_setup_offer_since", strconv.FormatInt(future, 10)); err != nil {
			t.Fatal(err)
		}
		if err := c.store.InsertSpeed(ctx, store.SpeedSample{TS: now - 3600, DownMbps: 100, UpMbps: 20, PingMS: 10, Server: "s"}); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if !c.QuickSetupDone() {
			t.Fatal("an established install with a future clock must be answered, not re-held")
		}
	})

	t.Run("a past-young offer clock on an ESTABLISHED install is answered, not re-held", func(t *testing.T) {
		// #6: a fresh install expired at 48h and ran (gained samples); a small
		// backward clock step made the offer clock young again (<48h, still past).
		// History must win: mark answered, never re-hold a running install.
		c := open(t)
		young := now - 3600 // 1h old, well inside the 48h grace
		if err := c.store.SetSetting(ctx, "quick_setup_offer_since", strconv.FormatInt(young, 10)); err != nil {
			t.Fatal(err)
		}
		if err := c.store.InsertSpeed(ctx, store.SpeedSample{TS: now - 1800, DownMbps: 100, UpMbps: 20, PingMS: 10, Server: "s"}); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if !c.QuickSetupDone() {
			t.Fatal("an established install with a young offer clock must be answered, not held")
		}
	})

	t.Run("a normal past offer clock is left untouched", func(t *testing.T) {
		c := open(t)
		past := now - 3600
		if err := c.store.SetSetting(ctx, "quick_setup_offer_since", strconv.FormatInt(past, 10)); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if since := c.QuickSetupOfferSince(ctx); since != past {
			t.Fatalf("offer clock = %d, want unchanged %d (only future clocks re-anchor)", since, past)
		}
	})

	t.Run("a young upgrade is still an upgrade", func(t *testing.T) {
		// A day-old 0.60.0 install upgrading has a young anchor AND running
		// monitoring: holding it would silently stop probing on an established
		// install. Any anchor at all means history happened - answered.
		c := open(t)
		if err := c.store.SetSetting(ctx, "first_seen_ts", strconv.FormatInt(now-3600, 10)); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if !c.QuickSetupDone() {
			t.Fatal("an install with an anchor must be materialized as answered")
		}
		if c.QuickSetupOfferSince(ctx) != 0 {
			t.Fatal("no offer clock for an upgrade")
		}
	})

	t.Run("history without an anchor is an upgrade", func(t *testing.T) {
		// The speedtest-only / headless case: months of rows, no anchor ever
		// persisted (the anchor needs a dashboard or digest to compute uptime).
		c := open(t)
		if err := c.store.InsertSpeed(ctx, store.SpeedSample{TS: now - 90*24*3600, DownMbps: 500, UpMbps: 300}); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if !c.QuickSetupDone() {
			t.Fatal("measurement history must read as an upgrade even with no anchor")
		}
	})

	t.Run("an upgrade is answered outright", func(t *testing.T) {
		c := open(t)
		if err := c.store.SetSetting(ctx, "first_seen_ts", strconv.FormatInt(now-30*24*3600, 10)); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if !c.QuickSetupDone() {
			t.Fatal("months of history must materialize as answered")
		}
		if c.QuickSetupOfferSince(ctx) != 0 {
			t.Fatal("an upgrade must not get an offer clock")
		}
	})

	t.Run("configured but no history reads as established, not fresh", func(t *testing.T) {
		// The reviewer's case: a DB configured via UI/CLI/import (real settings
		// keys persisted) but with zero samples/events/speed runs and no anchor -
		// e.g. an upgrade from before the anchor existed, or a config restored
		// ahead of the first probe. It must be answered, not held for 48h.
		c := open(t)
		if err := c.store.SetSetting(ctx, keySpeedEnabled, "true"); err != nil { // a real config write (as an import/old save leaves)
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if !c.QuickSetupDone() {
			t.Fatal("a configured install with no history must be answered, not held")
		}
		if c.QuickSetupOfferSince(ctx) != 0 {
			t.Fatal("a configured install must not get an offer clock")
		}
	})

	t.Run("only the offer clock is NOT prior configuration", func(t *testing.T) {
		// A genuinely fresh install mid-hold (second boot) carries only the offer
		// clock - which is bookkeeping, not configuration. It must stay fresh and
		// keep holding, never be misread as configured.
		c := open(t)
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil { // seeds the clock
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now+60); err != nil { // "next boot"
			t.Fatal(err)
		}
		if c.QuickSetupDone() {
			t.Fatal("an offer clock alone must not classify a fresh install as configured")
		}
	})

	t.Run("idempotent across boots", func(t *testing.T) {
		c := open(t)
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now+7200); err != nil {
			t.Fatal(err)
		}
		if since := c.QuickSetupOfferSince(ctx); since != now {
			t.Fatalf("second boot moved the clock: %d, want %d", since, now)
		}
	})

	t.Run("a transient offer-clock read error takes no decision", func(t *testing.T) {
		// The store holds a mid-countdown offer clock, but its boot read fails
		// transiently (the history reads before it succeeded, so the est gate
		// can't catch this). The masked read (QuickSetupOfferSince) returns 0 -
		// "never seeded" - and would rewrite the clock to now, restarting the
		// 48h consent countdown off one failed read. The contract: a read error
		// takes NO decision and makes NO write; seedOfferClock takes the read
		// RESULT so the failing read can be staged (the concrete store can't
		// fail one read while the ones just before it succeed).
		c := open(t)
		past := now - 1000
		if err := c.store.SetSetting(ctx, keyQuickSetupOffer, strconv.FormatInt(past, 10)); err != nil {
			t.Fatal(err)
		}
		rerr := errors.New("transient settings read failure")
		if err := c.seedOfferClock(ctx, 0, rerr, now); !errors.Is(err, rerr) {
			t.Fatalf("seedOfferClock = %v, want the read error surfaced", err)
		}
		if since := c.QuickSetupOfferSince(ctx); since != past {
			t.Fatalf("offer clock = %d, want untouched %d (a failed read must not restart the countdown)", since, past)
		}
		if c.QuickSetupDone() {
			t.Fatal("a failed read must not answer the offer")
		}
	})
}

// hasPriorConfiguration must key off REAL config keys only: bookkeeping/install
// state (the offer clock, the answer marker, session epoch, the anchor, legacy
// telemetry rows) must never read as "configured", or a fresh install would be
// released from the consent hold without an answer.
func TestHasPriorConfiguration(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]string
		want bool
	}{
		{"empty", map[string]string{}, false},
		{"only offer clock", map[string]string{keyQuickSetupOffer: "123"}, false},
		{"only answer marker", map[string]string{keyQuickSetup: "true"}, false},
		{"only session epoch", map[string]string{keyAuthSessEpoch: "4"}, false},
		{"only anchor", map[string]string{"first_seen_ts": "123"}, false},
		{"legacy telemetry only", map[string]string{"telemetry_id": "x", "telemetry_last_send_ts": "9"}, false},
		{"digest delivery state only", map[string]string{"digest_last_sent": "9"}, false},
		{"all bookkeeping together", map[string]string{
			keyQuickSetupOffer: "1", keyQuickSetup: "false", keyAuthSessEpoch: "2",
			"first_seen_ts": "3", "telemetry_id": "x", "digest_last_sent": "4",
		}, false},
		{"a real config key", map[string]string{keySpeedEnabled: "true"}, true},
		{"auth configured", map[string]string{keyAuthHash: "$2a$..."}, true},
		{"config mixed with bookkeeping", map[string]string{keyQuickSetupOffer: "1", keyTimeout: "4"}, true},
	}
	for _, c := range cases {
		if got := hasPriorConfiguration(c.m); got != c.want {
			t.Errorf("%s: hasPriorConfiguration(%v) = %v, want %v", c.name, c.m, got, c.want)
		}
	}
}

// Guard: a genuinely fresh boot (New + EnsureQuickSetupOffer) must persist ONLY
// keys that hasPriorConfiguration ignores. If a future change makes fresh boot
// write a config key, the establishment discriminator would silently start
// releasing fresh installs from the hold - this catches that at the source.
func TestFreshBootWritesOnlyBookkeeping(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, err := New(ctx, st, Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureQuickSetupOffer(ctx, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	m, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hasPriorConfiguration(m) {
		t.Fatalf("fresh boot wrote a non-bookkeeping key; settings table = %v", m)
	}
}

// EstablishedInStore is the single "is this an upgrade?" signal, read straight
// from the store so it works even when the in-memory settings failed to load
// (the monitoring hold relies on that). Fresh reads false; history, an anchor,
// or persisted config each read true.
func TestEstablishedInStore(t *testing.T) {
	ctx := context.Background()
	mk := func(t *testing.T) (*Controller, *store.Store) {
		st, err := store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		c, err := New(ctx, st, Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2})
		if err != nil {
			t.Fatal(err)
		}
		return c, st
	}

	c, _ := mk(t)
	if est, err := c.EstablishedInStore(ctx); err != nil || est {
		t.Errorf("fresh store: est=%v err=%v, want false/nil", est, err)
	}

	c2, st2 := mk(t)
	if err := st2.InsertSpeed(ctx, store.SpeedSample{TS: time.Now().Unix() - 100, DownMbps: 100, UpMbps: 20, PingMS: 10, Server: "s"}); err != nil {
		t.Fatal(err)
	}
	if est, _ := c2.EstablishedInStore(ctx); !est {
		t.Error("measurement history must read established")
	}

	c3, st3 := mk(t)
	if err := st3.SetSetting(ctx, keySpeedEnabled, "true"); err != nil { // a real config key
		t.Fatal(err)
	}
	if est, _ := c3.EstablishedInStore(ctx); !est {
		t.Error("persisted operator configuration must read established")
	}

	c4, st4 := mk(t)
	if err := st4.SetSetting(ctx, "first_seen_ts", strconv.FormatInt(time.Now().Unix()-3600, 10)); err != nil {
		t.Fatal(err)
	}
	if est, _ := c4.EstablishedInStore(ctx); !est {
		t.Error("an install anchor must read established")
	}
}

// SetMonitoringAnsweringSetup sets the power state and answers the first-run
// offer in ONE transaction, so the UI can never show monitoring on while the
// boot hold still blocks probes (the marker never landed). Power-off never
// answers; an already-answered install is not re-marked.
func TestSetMonitoringAnsweringSetup(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	def := Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2}
	c, err := New(ctx, st, def)
	if err != nil {
		t.Fatal(err)
	}

	marked, err := c.SetMonitoringAnsweringSetup(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !marked || !c.Monitoring() || !c.QuickSetupDone() {
		t.Fatalf("power-on of a fresh install: marked=%v monitoring=%v done=%v, want all true", marked, c.Monitoring(), c.QuickSetupDone())
	}
	// Both persisted in the same tx: a reloaded controller sees both.
	c2, err := New(ctx, st, def)
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Monitoring() || !c2.QuickSetupDone() {
		t.Errorf("power-on did not persist atomically: monitoring=%v done=%v", c2.Monitoring(), c2.QuickSetupDone())
	}
	// Power OFF must not touch the marker.
	if marked, _ := c.SetMonitoringAnsweringSetup(ctx, false); marked || c.Monitoring() {
		t.Errorf("power-off: marked=%v monitoring=%v, want false/false", marked, c.Monitoring())
	}
	// Power ON again when already answered: no re-mark.
	if marked, _ := c.SetMonitoringAnsweringSetup(ctx, true); marked {
		t.Error("already-answered install must not be re-marked on power-on")
	}
}

// Update must persist ONLY the keys that actually changed. A no-op or partial
// settings POST must not freeze the whole form (that shadows CLI flags AND makes
// a fresh install look "established", skipping first-run consent).
func TestUpdatePersistsOnlyChangedKeys(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, err := New(ctx, st, Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 3 * time.Second, DownAfter: 3, UpAfter: 2})
	if err != nil {
		t.Fatal(err)
	}

	base, _ := st.AllSettings(ctx)
	// A no-op Update writes nothing - the table (and the establishment signal)
	// is unchanged.
	if _, err := c.Update(ctx, Patch{}); err != nil {
		t.Fatal(err)
	}
	afterNoop, _ := st.AllSettings(ctx)
	if len(afterNoop) != len(base) {
		t.Errorf("a no-op Update froze the form: %d keys, was %d", len(afterNoop), len(base))
	}
	if hasPriorConfiguration(afterNoop) {
		t.Error("a no-op settings save must not make a fresh install look established")
	}

	// A one-field change persists exactly that key, not the ~50-key snapshot.
	newTO := 9 * time.Second
	if _, err := c.Update(ctx, Patch{Timeout: &newTO}); err != nil {
		t.Fatal(err)
	}
	after, _ := st.AllSettings(ctx)
	if len(after) != len(afterNoop)+1 {
		t.Errorf("one-field Update persisted %d new keys, want exactly 1 (no formKeys freeze): %v", len(after)-len(afterNoop), after)
	}
	if after["timeout_s"] == "" {
		t.Error("the changed key (timeout_s) must be persisted")
	}
}

// QuickSetupOfferSinceErr must SURFACE a store read error, not mask it as 0 -
// the monitoring hold latches on a "no clock" reading, so a masked error would
// permanently release the hold off one failed read.
func TestQuickSetupOfferSinceErr(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(ctx, st, Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 3 * time.Second, DownAfter: 3, UpAfter: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, keyQuickSetupOffer, "1700000000"); err != nil {
		t.Fatal(err)
	}
	if n, err := c.QuickSetupOfferSinceErr(ctx); err != nil || n != 1700000000 {
		t.Fatalf("healthy read: got %d,%v want 1700000000,nil", n, err)
	}
	st.Close() // now reads fail
	if _, err := c.QuickSetupOfferSinceErr(ctx); err == nil {
		t.Error("a store read error must be surfaced, not masked as 0 (the hold must fail-safe)")
	}
}
