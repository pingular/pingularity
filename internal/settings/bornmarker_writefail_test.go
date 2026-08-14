package settings

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The birth marker records something that can only be OBSERVED, never inferred:
// that this daemon created this database. The controller that watched the store
// come into existence may therefore finish its own stamp at any point in its
// life - the observation does not expire when the store fills up. A controller
// that first met the database later may never write it, however markerless the
// store looks, because for it the birth is a guess. These tests exercise both
// halves against real faults: PRAGMA query_only makes the database genuinely
// reject writes while reads keep working (settings load fine; only the stamp
// cannot land), and hiding a table makes the establishment READ genuinely fail.

// denyStoreWrites makes every subsequent write to st fail. The :memory: pool is
// pinned to one connection, so the per-connection pragma sticks across calls.
func denyStoreWrites(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(), "PRAGMA query_only = ON"); err != nil {
		t.Fatalf("query_only on: %v", err)
	}
}

// allowStoreWrites lifts denyStoreWrites, so the test can go on to build the
// store's later life (history, configuration) on a working database.
func allowStoreWrites(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(), "PRAGMA query_only = OFF"); err != nil {
		t.Fatalf("query_only off: %v", err)
	}
}

// breakEstablishmentRead makes the brand-new-vs-established question genuinely
// UNANSWERABLE, without a test seam: EstablishedInStore's first leg
// (store.HasHistory) queries the samples table, so renaming it away turns that
// read into a real SQL error while AllSettings - and therefore the settings load
// itself - keeps working. That is the shape of the fault this path must handle:
// the settings are fine, only the judgement cannot be made. The returned func
// puts the table back, so the same store can go on to be read normally.
func breakEstablishmentRead(t *testing.T, st *store.Store) (restore func()) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, "ALTER TABLE samples RENAME TO samples_hidden"); err != nil {
		t.Fatalf("hide the samples table: %v", err)
	}
	if _, err := st.HasHistory(ctx); err == nil {
		t.Fatal("fault injection did not take: HasHistory still answers, so the establishment read is not broken")
	}
	return func() {
		if _, err := st.DB().ExecContext(ctx, "ALTER TABLE samples_hidden RENAME TO samples"); err != nil {
			t.Fatalf("restore the samples table: %v", err)
		}
	}
}

// establishStore gives the store the ordinary life that shuts the stamping
// window for a controller that did not witness its birth: operator
// configuration and measurement history, both written straight to the store as
// a monitor and a later daemon would.
func establishStore(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	if err := st.SetSetting(ctx, "monitoring", "true"); err != nil {
		t.Fatalf("seed configuration: %v", err)
	}
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: time.Now(), Target: "cloudflare", Family: "ipv4", LatencyMS: 9, Success: true},
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}
}

func openBornStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// countBornMarkerAttempts wraps the marker's write seam with a counter while
// still performing the REAL write, so a test can prove the retry actually
// happened without faking the failure it retries.
func countBornMarkerAttempts(t *testing.T, n *int) {
	t.Helper()
	write := setBornMarker
	setBornMarker = func(ctx context.Context, st *store.Store, v string) error {
		*n++
		return write(ctx, st, v)
	}
	t.Cleanup(func() { setBornMarker = write })
}

// captureStderr collects what fn writes to os.Stderr. The marker failure is
// printed there on purpose - it happens during settings load, when the
// configured log level is still "off" - so the operator-visible half of
// "surfaced" is asserted rather than assumed.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("temp stderr: %v", err)
	}
	defer f.Close()
	orig := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = orig }()
	fn()
	os.Stderr = orig
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(b)
}

// A stamp that cannot be written must be SURFACED, not discarded: the store is
// left markerless (nothing pretends otherwise), settings still load, the daemon
// is not stopped - and both the immediate stderr warning and the controller's
// BornMarkerErr say what happened, because the ambiguity warning this causes
// would otherwise recur forever with no recorded cause.
func TestBornMarkerWriteFailureIsSurfacedNotSwallowed(t *testing.T) {
	ctx := context.Background()
	st := openBornStore(t)
	denyStoreWrites(t, st)

	var attempts int
	countBornMarkerAttempts(t, &attempts)

	var c *Controller
	var err error
	out := captureStderr(t, func() {
		c, err = New(ctx, st, bornMarkerDefaults, WithBornVersion("0.62.0-test"))
	})
	if err != nil {
		t.Fatalf("New = %v; a failed birth stamp must not fail the settings load - monitoring is the job", err)
	}
	if !c.Loaded() {
		t.Fatal("the controller reports unloaded settings; only the marker write failed")
	}
	if attempts != 2 {
		t.Fatalf("marker write attempts = %d, want 2: the stamp is retried ONCE in place, because the window never reopens", attempts)
	}
	if _, ok := bornMarkerValue(t, st); ok {
		t.Fatal("the store carries a marker though every write was refused")
	}
	if c.BornMarkerErr() == nil {
		t.Fatal("the failed stamp was swallowed: the install is permanently markerless and nothing recorded why")
	}
	low := strings.ToLower(out)
	for _, want := range []string{KeyInstallBornVersion, "permanently"} {
		if !strings.Contains(low, strings.ToLower(want)) {
			t.Errorf("stderr warning does not mention %q; it is the only notice an operator gets at load time:\n%s", want, out)
		}
	}
}

// A TRANSIENT failure is what the retry exists for: the first attempt fails,
// the second lands the real write, and the install is marked normally - no
// error surfaced, nothing for an operator to chase.
func TestBornMarkerTransientWriteFailureIsRetriedAndSucceeds(t *testing.T) {
	ctx := context.Background()
	st := openBornStore(t)

	write := setBornMarker
	var attempts int
	setBornMarker = func(ctx context.Context, s *store.Store, v string) error {
		attempts++
		if attempts == 1 {
			return errors.New("injected transient store hiccup")
		}
		return write(ctx, s, v) // the retry writes to the real store
	}
	t.Cleanup(func() { setBornMarker = write })

	var c *Controller
	var err error
	out := captureStderr(t, func() {
		c, err = New(ctx, st, bornMarkerDefaults, WithBornVersion("0.62.0-test"))
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("marker write attempts = %d, want 2 (one failure, one retry)", attempts)
	}
	if v, ok := bornMarkerValue(t, st); !ok || v != "0.62.0-test" {
		t.Fatalf("marker = %q, %v; the retry must land the stamp a transient fault lost", v, ok)
	}
	if c.BornMarkerErr() != nil {
		t.Fatalf("BornMarkerErr = %v after a recovered write; a retry that succeeded is not a failure to report", c.BornMarkerErr())
	}
	if out != "" {
		t.Errorf("a recovered transient failure warned the operator anyway:\n%s", out)
	}
}

// While the store is still BRAND-NEW, a lost stamp is not yet permanent: the
// next load (here the recovery path's Reload) may still take it, and doing so
// clears the recorded failure. Permanence begins when the store becomes
// established - see the test below.
func TestBornMarkerRestampedOnReloadWhileStoreStillFresh(t *testing.T) {
	ctx := context.Background()
	st := openBornStore(t)
	denyStoreWrites(t, st)

	var c *Controller
	var err error
	_ = captureStderr(t, func() { c, err = New(ctx, st, bornMarkerDefaults, WithBornVersion("0.62.0-test")) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.BornMarkerErr() == nil {
		t.Fatal("precondition: the birth stamp must have failed")
	}

	allowStoreWrites(t, st) // the hiccup passes; the store is still untouched
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if v, ok := bornMarkerValue(t, st); !ok || v != "0.62.0-test" {
		t.Fatalf("marker = %q, %v; a still-fresh store must still be stampable", v, ok)
	}
	if c.BornMarkerErr() != nil {
		t.Fatalf("BornMarkerErr = %v though the marker is now stamped", c.BornMarkerErr())
	}
}

// THE INVARIANT. A controller that did NOT witness the birth must refuse
// forever, and "the store is markerless and I would like to stamp it" is not a
// reason: after a restart, the daemon that saw the empty database is gone, and
// what is left on disk (history, configuration, no marker) is exactly what a
// genuinely pre-marker install looks like. Stamping here would invent
// provenance, which is the whole thing the marker's immutability protects
// against. This is also why the write error at birth is surfaced: for a process
// that has already exited, that warning was the only notice there would ever be.
// The later boot itself stays silent - it is not re-reporting a failure it did
// not attempt.
//
// The counterpart - the SAME store, stamped by the controller that did witness
// the birth - is TestBornMarkerCompletedByTheControllerThatWitnessedTheBirth.
func TestBornMarkerNeverStampedByAControllerThatDidNotWitnessTheBirth(t *testing.T) {
	ctx := context.Background()
	st := openBornStore(t)
	denyStoreWrites(t, st)
	_ = captureStderr(t, func() {
		if _, err := New(ctx, st, bornMarkerDefaults, WithBornVersion("0.62.0-test")); err != nil {
			t.Errorf("New: %v", err)
		}
	})
	if _, ok := bornMarkerValue(t, st); ok {
		t.Fatal("precondition: the store must be markerless")
	}
	allowStoreWrites(t, st)
	// The witnessing controller above is DROPPED here, deliberately: this models
	// the daemon exiting before it could finish its stamp. Everything from now on
	// is a process that never saw the database empty.
	establishStore(t, st)

	var attempts int
	countBornMarkerAttempts(t, &attempts)
	var c *Controller
	out := captureStderr(t, func() {
		var err error
		c, err = New(ctx, st, bornMarkerDefaults, WithBornVersion("0.63.0-test"))
		if err != nil {
			t.Errorf("New (later boot): %v", err)
		}
	})
	if attempts != 0 {
		t.Fatalf("an established store was stamped (%d write attempts) by a controller that never saw it fresh; a marker added later claims a birth that cannot be proven", attempts)
	}
	if _, ok := bornMarkerValue(t, st); ok {
		t.Fatal("an established store gained the birth marker retroactively")
	}
	if c.BornMarkerErr() != nil {
		t.Fatalf("BornMarkerErr = %v on a boot that had nothing to stamp", c.BornMarkerErr())
	}
	if out != "" {
		t.Errorf("a later boot re-warned about a stamp it never attempted:\n%s", out)
	}
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := bornMarkerValue(t, st); ok {
		t.Fatal("Reload stamped an established store on a controller that did not witness its birth")
	}
	// Nor may a settings write on that controller sneak the stamp in: the
	// completion path is gated on having WITNESSED the birth, not on the marker
	// being absent.
	if err := c.SetMonitoring(ctx, true); err != nil {
		t.Fatalf("SetMonitoring: %v", err)
	}
	if _, ok := bornMarkerValue(t, st); ok {
		t.Fatal("a settings write stamped an established store on a controller that did not witness its birth")
	}
}

// THE RECOVERY, and the distinction that makes it sound. A fault that outlasts
// both adjacent attempts - a read-only window while a volume remounts, a stalled
// disk - used to lose the stamp for good: New returns cleanly, so main arms no
// retry, and by the time anything looked again the store had accrued history and
// was "established". But this controller SAW the empty database. Completing its
// own pending stamp later is truthful, not retroactive, so it may - and on the
// very same store, at the very same moment, a fresh controller may not.
func TestBornMarkerCompletedByTheControllerThatWitnessedTheBirth(t *testing.T) {
	ctx := context.Background()
	st := openBornStore(t)
	denyStoreWrites(t, st)

	var attempts int
	countBornMarkerAttempts(t, &attempts)

	var witness *Controller
	_ = captureStderr(t, func() {
		var err error
		witness, err = New(ctx, st, bornMarkerDefaults, WithBornVersion("0.62.0-test"))
		if err != nil {
			t.Errorf("New: %v", err)
		}
	})
	if attempts != 2 {
		t.Fatalf("marker write attempts at birth = %d, want 2: the two adjacent attempts stay the fast path", attempts)
	}
	if witness.BornMarkerErr() == nil {
		t.Fatal("precondition: both attempts must have failed and been recorded")
	}

	// The fault outlasts both attempts by seconds, then clears - and by then the
	// install has started living. Written straight to the store, as the monitor
	// and an import do, so nothing on the controller's own write path is what
	// rescues the stamp here.
	allowStoreWrites(t, st)
	establishStore(t, st)
	if est, err := witness.EstablishedInStore(ctx); err != nil || !est {
		t.Fatalf("EstablishedInStore = %v, %v; the point of this test is that the window has SHUT", est, err)
	}

	// A restart at this instant gets nothing: the store looks pre-marker to any
	// controller that did not watch it come into existence.
	attempts = 0
	var restart *Controller
	out := captureStderr(t, func() {
		var err error
		restart, err = New(ctx, st, bornMarkerDefaults, WithBornVersion("0.63.0-test"))
		if err != nil {
			t.Errorf("New (restart): %v", err)
		}
	})
	if attempts != 0 || out != "" {
		t.Fatalf("a non-witnessing controller attempted the stamp (%d attempts) or warned about it:\n%s", attempts, out)
	}
	if _, ok := bornMarkerValue(t, st); ok {
		t.Fatal("a controller that did not witness the birth stamped the established store")
	}
	if restart.BornMarkerErr() != nil {
		t.Fatalf("BornMarkerErr = %v on a controller that had nothing to stamp", restart.BornMarkerErr())
	}

	// The witness, still running, finishes what it started.
	if err := witness.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if v, ok := bornMarkerValue(t, st); !ok || v != "0.62.0-test" {
		t.Fatalf("marker = %q, %v; the controller that witnessed the birth must be able to complete its own stamp, with the version it was born under", v, ok)
	}
	if witness.BornMarkerErr() != nil {
		t.Fatalf("BornMarkerErr = %v though the stamp has landed", witness.BornMarkerErr())
	}
	// And it is done exactly once: a further Reload finds the marker and leaves it.
	attempts = 0
	if err := witness.Reload(ctx); err != nil {
		t.Fatalf("second Reload: %v", err)
	}
	if attempts != 0 {
		t.Fatalf("a completed stamp was rewritten (%d attempts); the marker is immutable", attempts)
	}
}

// The realistic recovery is not a Reload at all. When New itself SUCCEEDS - the
// settings loaded; only the stamp failed - main arms no retry loop, so the next
// Reload is a SIGHUP or an import that may never come. What does come is the
// first settings write after the fault clears, and it is usually the very write
// that establishes the store (Quick Setup's answer, the power button, an access
// choice). Completing the stamp on the back of that write is what closes the
// window instead of racing it - no goroutine, no timer, no clock at all.
func TestBornMarkerCompletedByTheNextSettingsWrite(t *testing.T) {
	ctx := context.Background()
	st := openBornStore(t)
	denyStoreWrites(t, st)

	var c *Controller
	_ = captureStderr(t, func() {
		var err error
		c, err = New(ctx, st, bornMarkerDefaults, WithBornVersion("0.62.0-test"))
		if err != nil {
			t.Errorf("New: %v", err)
		}
	})
	if c.BornMarkerErr() == nil {
		t.Fatal("precondition: the birth stamp must have failed")
	}
	allowStoreWrites(t, st)

	// The operator turns monitoring on. That write ESTABLISHES the store - and
	// carries the pending stamp in with it.
	if err := c.SetMonitoring(ctx, true); err != nil {
		t.Fatalf("SetMonitoring: %v", err)
	}
	if v, ok := bornMarkerValue(t, st); !ok || v != "0.62.0-test" {
		t.Fatalf("marker = %q, %v; the first write that proved the store writable again should have completed the stamp", v, ok)
	}
	if c.BornMarkerErr() != nil {
		t.Fatalf("BornMarkerErr = %v though the stamp has landed", c.BornMarkerErr())
	}
	if est, err := c.EstablishedInStore(ctx); err != nil || !est {
		t.Fatalf("EstablishedInStore = %v, %v; the write that completed the stamp is the same one that shut the window", est, err)
	}
}

// A store read that FAILS answers nothing, and used to be treated as "not our
// problem": the same early return as an established store, before anything was
// recorded or printed. The two are not the same. Skipping the stamp stays right
// - the store may already be established, and stamping on a guess is what the
// marker's immutability forbids - but the operator gets told, through the same
// channels a failed write uses, and the question is retaken at the next load.
func TestBornMarkerUndeterminedEstablishmentIsSurfacedAndRetaken(t *testing.T) {
	// The read comes back, the store is still brand-new: the stamp lands after
	// all, and the recorded failure clears with it.
	t.Run("re-decided as a birth once the read works", func(t *testing.T) {
		ctx := context.Background()
		st := openBornStore(t)
		restore := breakEstablishmentRead(t, st)

		var attempts int
		countBornMarkerAttempts(t, &attempts)

		var c *Controller
		out := captureStderr(t, func() {
			var err error
			c, err = New(ctx, st, bornMarkerDefaults, WithBornVersion("0.62.0-test"))
			if err != nil {
				t.Errorf("New = %v; an unanswerable establishment read must not fail the settings load", err)
			}
		})
		if !c.Loaded() {
			t.Fatal("the controller reports unloaded settings; only the establishment read failed")
		}
		if attempts != 0 {
			t.Fatalf("%d stamp attempts on an UNDETERMINED read; a store that may already be established must never be stamped", attempts)
		}
		if _, ok := bornMarkerValue(t, st); ok {
			t.Fatal("the store was stamped though nothing could be determined about it")
		}
		if c.BornMarkerErr() == nil {
			t.Fatal("an unanswerable establishment read was swallowed: no error recorded, so a permanently unmarked install would have no traceable cause")
		}
		low := strings.ToLower(out)
		for _, want := range []string{KeyInstallBornVersion, "determine", "brand-new"} {
			if !strings.Contains(low, strings.ToLower(want)) {
				t.Errorf("stderr warning does not mention %q; it is the only load-time notice an operator gets:\n%s", want, out)
			}
		}

		restore() // the store reads again, and it really was brand-new
		if err := c.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if v, ok := bornMarkerValue(t, st); !ok || v != "0.62.0-test" {
			t.Fatalf("marker = %q, %v; once the question can be answered, a brand-new store must still be stamped", v, ok)
		}
		if c.BornMarkerErr() != nil {
			t.Fatalf("BornMarkerErr = %v though the stamp has landed", c.BornMarkerErr())
		}
	})

	// And the direction that matters: "I could not tell" must never be recorded
	// as "I saw an empty database". If the store turns out to have been
	// established all along, the same controller must refuse - exactly as a
	// controller that never witnessed a birth does.
	t.Run("an undetermined read witnesses nothing", func(t *testing.T) {
		ctx := context.Background()
		st := openBornStore(t)
		establishStore(t, st) // established BEFORE anyone looks
		restore := breakEstablishmentRead(t, st)

		var c *Controller
		_ = captureStderr(t, func() {
			var err error
			c, err = New(ctx, st, bornMarkerDefaults, WithBornVersion("0.62.0-test"))
			if err != nil {
				t.Errorf("New: %v", err)
			}
		})
		if c.BornMarkerErr() == nil {
			t.Fatal("precondition: the establishment read must have failed and been recorded")
		}
		restore()

		if err := c.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if _, ok := bornMarkerValue(t, st); ok {
			t.Fatal("an unanswerable read armed the pending stamp; an established store was then stamped on what was only ever a guess")
		}
		if err := c.SetMonitoring(ctx, true); err != nil {
			t.Fatalf("SetMonitoring: %v", err)
		}
		if _, ok := bornMarkerValue(t, st); ok {
			t.Fatal("a settings write completed a stamp that no controller had witnessed")
		}
	})
}

// A markerless birth must not disturb the fresh-install machinery either: the
// marker is bookkeeping, the failed WRITE wrote nothing at all, so the store
// still reads fresh and the consent hold still applies. (The success case is
// TestBornMarkerExemptFromEstablishmentAndConsentHold; this is its failure twin,
// so neither outcome of the stamp can release the hold.)
func TestLostBornMarkerLeavesConsentHoldIntact(t *testing.T) {
	ctx := context.Background()
	st := openBornStore(t)
	denyStoreWrites(t, st)
	var c *Controller
	_ = captureStderr(t, func() {
		var err error
		c, err = New(ctx, st, bornMarkerDefaults, WithBornVersion("0.62.0-test"))
		if err != nil {
			t.Errorf("New: %v", err)
		}
	})
	allowStoreWrites(t, st)

	if est, err := c.EstablishedInStore(ctx); err != nil || est {
		t.Fatalf("EstablishedInStore = %v, %v; a store nothing was written to is still fresh", est, err)
	}
	now := time.Now().Unix()
	if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
		t.Fatalf("EnsureQuickSetupOffer: %v", err)
	}
	if c.QuickSetupDone() {
		t.Fatal("a failed birth stamp marked the install answered; the first-run dialog is lost")
	}
	since, err := c.QuickSetupOfferSinceErr(ctx)
	if err != nil || since == 0 {
		t.Fatalf("offer clock = %d, %v; a fresh install must still be offered", since, err)
	}
	if !QuickSetupHold(c.QuickSetupDone(), since, now) {
		t.Fatal("the consent hold is not in force on a fresh install whose birth stamp failed")
	}
}
