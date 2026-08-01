package digest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// humanWindow renders whole days and whole hours and drops the remainder, so
// anything under an hour prints as "0h". Two of its three call sites make that a
// statement about how much the machine watched.
//
// The worst is the coverage disclosure: "observed %s of %s" is the line that tells
// the operator how far to trust every figure above it. A machine that was awake
// for forty-five minutes of the day reports "observed 0h of 1d" - not an
// understatement but an inversion, since 0h is precisely what a machine that
// observed NOTHING would print, and the digest has a separate, differently-worded
// sentence for that case.
func TestHumanWindowDoesNotRoundShortSpansAwayToNothing(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{45 * time.Minute, "45m"},
		{90 * time.Minute, "1h 30m"},
		{30 * time.Second, "30s"},
		{0, "0s"},
		{25 * time.Hour, "1d 1h"},
		{24 * time.Hour, "1d"},
		{2 * time.Hour, "2h"},
	}
	for _, c := range cases {
		got := humanWindow(c.in)
		if got == "0h" && c.in > 0 {
			t.Errorf("humanWindow(%v) = %q: a real span prints as the same string a zero span "+
				"would, and at the \"observed %%s of %%s\" call site that tells the operator the "+
				"machine watched nothing when it watched %v", c.in, got, c.in)
			continue
		}
		if got != c.want {
			t.Errorf("humanWindow(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The formatter's failure only matters because of where the string lands. This is
// the same scenario as TestPartiallyObservedWindowDiscloses, but with the observed
// span just under an hour - the case that test's whole 8h never reached.
//
// The coverage disclosure is the sentence that tells the operator how far to trust
// every figure above it. Printing "observed 0h" for a machine that was watching
// makes it agree, word for word on the part that matters, with the digest's
// separate sentence for a window nobody watched at all.
func TestTheCoverageDisclosureDoesNotReportAWatchingMachineAsZero(t *testing.T) {
	m, st, _ := newManager(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: now.Add(-48 * time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10},
	}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	// Awake for 45 minutes of the last day; paused for the other 23h15m.
	if _, err := st.InsertPause(ctx, now.Add(-24*time.Hour), int64((23*time.Hour+15*time.Minute)/time.Second)); err != nil {
		t.Fatalf("pause: %v", err)
	}

	s, err := m.Summarize(ctx, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if !s.Obs.Defined() {
		t.Fatalf("the window observed 45m, so it is defined; coverage=%.4f", s.Obs.Coverage())
	}
	msg := s.Message()
	t.Logf("digest:\n%s", msg)

	if strings.Contains(msg, "observed 0h") {
		t.Errorf("the machine watched %v of the day and the digest says it observed 0h - the same "+
			"thing it would say if the machine had been off the whole time:\n%s",
			s.Obs.Observed.Round(time.Minute), msg)
	}
	if !strings.Contains(msg, "observed 45m of 1d") {
		t.Errorf("want the disclosure to read \"observed 45m of 1d\":\n%s", msg)
	}
}
