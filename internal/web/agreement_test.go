package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/digest"
	"github.com/pingular/pingularity/internal/store"
)

// ---------------------------------------------------------------------------
// The agreement test this cluster has been missing.
//
// Six defects in a row came from ONE invariant - paused/unobserved wall time is
// neither up nor down - being re-derived in six places and enforced only by prose.
// Every one of them was found the same way: two components describing the same
// window and disagreeing. Nothing in the tree could catch that, because the
// cross-component tests stop at the package boundary (internal/store's fixtures
// compare the three STORE consumers; nothing compares a store reading with what
// the HTTP surfaces and the digest actually emit).
//
// So: one fixture, four renderers, one assertion. /api/status, /metrics, the
// heatmap and the digest each describe the same 24 hours; if any of them ever
// disagrees about how much of it was observed, how much of that was down, or what
// uptime that implies, this fails and says which pair diverged.
// ---------------------------------------------------------------------------

// story is what one renderer says about the shared window. It is deliberately the
// three numbers an OPERATOR would compare across surfaces, not internal state.
type story struct {
	who string
	// observedS: how much of the window the renderer says was actually monitored.
	observedS int
	// downS: the observed downtime it books in that window.
	downS int
	// uptimePct: the percentage it would present, or notMeasured when it declines
	// to present one (coverage 0). "Declines" is part of the story: a renderer that
	// shows 100% where another shows nothing is exactly the six-defect failure.
	uptimePct float64
}

const notMeasured = -1

func (s story) String() string {
	pct := "not measured"
	if s.uptimePct != notMeasured {
		pct = fmt.Sprintf("%.4f%%", s.uptimePct)
	}
	return fmt.Sprintf("%-10s observed %6ds · down %4ds · uptime %s", s.who, s.observedS, s.downS, pct)
}

// agreementWindow is the span every renderer is asked about. 24h because it is the
// one window all four can describe: /metrics only offers the preset set, and the
// heatmap only offers whole days back from now.
const agreementWindow = 24 * time.Hour

// agreementFixture builds the shared world: a monitor installed two days ago that
// watched 16 of the last 24 hours and saw one 10-minute outage inside the watched
// part.
//
// Monitoring is anchored 48h back on purpose, so the 24h window is NOT clamped by
// monitoringSince - the heatmap does not apply that clamp, and this test is about
// the pause invariant, not that (separate) asymmetry.
func agreementFixture(t *testing.T, now time.Time) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: now.Add(-48 * time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10},
		{TS: now.Add(-time.Minute), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	// 8h of the window unobserved: a closed schedule window, a paused monitor, or a
	// sleeping host - the store records all three the same way.
	if _, err := st.InsertPause(ctx, now.Add(-24*time.Hour), int64(8*time.Hour/time.Second)); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// One completed 600s outage, well clear of the pause so the two cannot mask
	// each other's arithmetic.
	down := now.Add(-10 * time.Hour)
	if err := st.InsertEvent(ctx, down, "down", -1, ""); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := st.InsertEvent(ctx, down.Add(600*time.Second), "up", 600, ""); err != nil {
		t.Fatalf("up: %v", err)
	}
	return st
}

var (
	reRatio    = regexp.MustCompile(`(?m)^pingularity_uptime_ratio\{window="24h"\} (\S+)$`)
	reCoverage = regexp.MustCompile(`(?m)^pingularity_uptime_coverage_ratio\{window="24h"\} (\S+)$`)
)

// statusStory reads the dashboard's uptime pill: the ratio it would print and the
// coverage that qualifies it.
func statusStory(t *testing.T, s *Server, windowS int) story {
	t.Helper()
	out := getStatus(t, s, "")
	ratios, _ := out["uptime"].(map[string]any)
	cov, _ := out["uptime_coverage"].(map[string]any)
	if ratios == nil || cov == nil {
		t.Fatalf("/api/status must carry uptime AND uptime_coverage; got %v", keysOf(out))
	}
	r, okR := ratios["24h"].(float64)
	c, okC := cov["24h"].(float64)
	if !okR || !okC {
		t.Fatalf("/api/status 24h uptime/coverage missing: %v / %v", ratios["24h"], cov["24h"])
	}
	return storyFrom("status", float64(windowS)*c, r, c > 0)
}

// metricsStory reads the exporter: the same two series, parsed from the text
// format a Prometheus server would see.
func metricsStory(t *testing.T, s *Server, windowS int) story {
	t.Helper()
	body := scrape(t, s)
	cm := reCoverage.FindStringSubmatch(body)
	if cm == nil {
		t.Fatalf("no uptime_coverage_ratio{24h} in the scrape:\n%s", body)
	}
	c, err := strconv.ParseFloat(cm[1], 64)
	if err != nil {
		t.Fatalf("coverage %q: %v", cm[1], err)
	}
	rm := reRatio.FindStringSubmatch(body)
	if rm == nil { // the exporter declines to state a ratio - a valid story
		return storyFrom("metrics", float64(windowS)*c, 0, false)
	}
	r, err := strconv.ParseFloat(rm[1], 64)
	if err != nil {
		t.Fatalf("ratio %q: %v", rm[1], err)
	}
	return storyFrom("metrics", float64(windowS)*c, r, true)
}

// heatmapStory reads the year grid over the same 24 hours (?days=1).
//
// It sums UNOBSERVED seconds rather than observed ones: a day that was watched end
// to end and had no event emits no row at all (so a healthy year stays a handful
// of rows), and such a day contributes exactly 0 to the unobserved total. Observed
// is then the window minus that - the same 24h window the other three describe.
func heatmapStory(t *testing.T, s *Server, windowS int) story {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/heatmap?days=1&tz=UTC", nil)
	r.Host = "127.0.0.1:9000"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/heatmap: code=%d body=%q", w.Code, w.Body.String())
	}
	var rows []store.DowntimeDay
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode heatmap: %v", err)
	}
	var unobserved, down int
	for _, d := range rows {
		if d.WindowS > d.ObservedS {
			unobserved += d.WindowS - d.ObservedS
		}
		down += d.DowntimeS
	}
	observed := float64(windowS - unobserved)
	if observed <= 0 {
		return storyFrom("heatmap", observed, 0, false)
	}
	return storyFrom("heatmap", observed, 1-float64(down)/observed, true)
}

// digestStory reads the webhook summary for the same window - the prose an
// operator gets by mail, via the same Summarize the sender uses.
func digestStory(t *testing.T, st *store.Store, now time.Time) story {
	t.Helper()
	m := &digest.Manager{
		Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		FreqFn: func() string { return "daily" },
	}
	s, err := m.Summarize(context.Background(), now.Add(-agreementWindow), now)
	if err != nil {
		t.Fatalf("digest summary: %v", err)
	}
	// Read the EMITTED payload, not the Summary's internals: this renderer has to
	// be what an operator receives, or the test cannot see the digest lying. The
	// percentage is absent from the payload exactly when the digest declines to
	// state one, mirroring the absent uptime_ratio series on /metrics.
	f := s.Fields()
	pct := float64(notMeasured)
	if v, ok := f["uptime_pct"].(float64); ok {
		pct = v
	}
	// downtime_s is ResolvedOutagesSince's figure, not the Observation's - that is
	// the point. The digest sends this number beside the percentage, and in 5b037fc
	// the two came from branches that disagreed about pause overlap.
	return story{who: "digest", observedS: intField(t, f, "observed_s"), downS: intField(t, f, "downtime_s"), uptimePct: pct}
}

func intField(t *testing.T, f map[string]any, key string) int {
	t.Helper()
	v, ok := f[key].(int)
	if !ok {
		t.Fatalf("digest payload is missing %q (it must disclose the span it describes): %v", key, f)
	}
	return v
}

func storyFrom(who string, observed, ratio float64, measured bool) story {
	down := 0.0
	if observed > 0 {
		down = observed * (1 - ratio)
	}
	return story{
		who: who, observedS: int(math.Round(observed)), downS: int(math.Round(down)),
		uptimePct: pctOrNot(ratio*100, measured),
	}
}

func pctOrNot(pct float64, measured bool) float64 {
	if !measured {
		return notMeasured
	}
	return pct
}

// ONE fixture, FOUR renderers, ONE assertion: for a window containing pause spans,
// the status pill, the exporter, the heatmap and the digest must tell the same
// story.
//
// This is the durable guard for the whole pause/coverage seam. Each of the six
// defects in this cluster would have surfaced here as a row that disagrees with
// its neighbours - the restored-backup coverage jump, the pruned straddling pause,
// the digest's raw-wall-gap downtime, the suspend the denominator never saw, and
// the two disclosure drops this change fixes.
func TestFourRenderersTellTheSameStory(t *testing.T) {
	now := time.Now()
	st := agreementFixture(t, now)
	srv := statusServer(t, st)
	windowS := int(agreementWindow.Seconds())

	got := []story{
		statusStory(t, srv, windowS),
		metricsStory(t, srv, windowS),
		heatmapStory(t, srv, windowS),
		digestStory(t, st, now),
	}

	// Sanity-check the fixture itself before comparing: if it stopped producing a
	// partly-observed window with an outage in it, agreement would be vacuous.
	want := story{who: "fixture", observedS: 16 * 3600, downS: 600, uptimePct: 100 * (1 - 600.0/(16*3600))}
	if !agrees(want, got[0]) {
		t.Fatalf("fixture no longer describes 16h observed of 24h with one 600s outage:\n  %v\n  %v", want, got[0])
	}

	for i := 1; i < len(got); i++ {
		if agrees(got[0], got[i]) {
			continue
		}
		t.Errorf("%s and %s disagree about the same 24 hours.\n"+
			"  %v\n  %v\n"+
			"All four render ONE invariant: paused/unobserved wall time is neither up nor down.\n"+
			"A renderer that books unobserved time as up reports MORE observed time and a\n"+
			"higher uptime; one that books it as down reports more downtime. Whichever moved,\n"+
			"it stopped agreeing with the other three - fix the derivation, not this test.\n"+
			"Full set:\n%s", got[0].who, got[i].who, got[0], got[i], render(got))
	}
}

// A window nobody watched: the four must agree on that too, and none of them may
// present a percentage. This is the case that used to split them cleanly in half -
// /metrics published nothing while /api/status showed 100.000% and the digest
// mailed "Uptime 100.00% · no outages".
func TestFourRenderersAgreeOnAnUnobservedWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := agreementFixture(t, now)
	// Blanket the whole window: +120s of slack so the span still covers the window
	// end when each renderer reads its own clock.
	if _, err := st.InsertPause(ctx, now.Add(-24*time.Hour), int64(24*time.Hour/time.Second)+120); err != nil {
		t.Fatalf("pause: %v", err)
	}
	srv := statusServer(t, st)
	windowS := int(agreementWindow.Seconds())

	got := []story{
		statusStory(t, srv, windowS),
		metricsStory(t, srv, windowS),
		heatmapStory(t, srv, windowS),
		digestStory(t, st, now),
	}
	for _, s := range got {
		if s.uptimePct != notMeasured {
			t.Errorf("%s presented %.4f%% for a window that observed nothing; "+
				"there is no measurement there, only the sentinel ratio.\nFull set:\n%s",
				s.who, s.uptimePct, render(got))
		}
		if s.observedS > 5 {
			t.Errorf("%s says %ds were observed in a window that was unobserved end to end.\nFull set:\n%s",
				s.who, s.observedS, render(got))
		}
	}
}

// An outage that is still open when the window ends, with monitoring switched off
// for part of it. This is the case the two fixtures above cannot reach, and it is
// the one that makes the heatmap's proration load-bearing: a CLOSED outage carries
// duration_s, which already excludes paused time, and prorate caps its credit at
// that figure - so for a closed outage prorate's own pause subtraction is a no-op
// and the guard above cannot see it at all. An outage with no closing 'up' has no
// duration_s to cap against (prorate is called with limit < 0), so subtracting the
// pause spans is the ONLY thing keeping unobserved wall time out of the heatmap's
// downtime, and the other three renderers do the same subtraction in their own
// code. Without a case like this, the whole cluster's heatmap arithmetic could be
// deleted and every test would still pass.
func TestFourRenderersAgreeOnAnOngoingPartlyUnobservedOutage(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := agreementFixture(t, now)
	// Monitoring off for 2h in the middle of what is about to become an outage.
	if _, err := st.InsertPause(ctx, now.Add(-6*time.Hour), int64(2*time.Hour/time.Second)); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// A 'down' with no closing 'up': the link went down 8h ago and the window ends
	// with it still open (the fixture's trailing success sample bounds it, exactly
	// as UptimeSince and DowntimeByDay both bound it).
	if err := st.InsertEvent(ctx, now.Add(-8*time.Hour), "down", -1, ""); err != nil {
		t.Fatalf("down: %v", err)
	}
	srv := statusServer(t, st)
	windowS := int(agreementWindow.Seconds())

	got := []story{
		statusStory(t, srv, windowS),
		metricsStory(t, srv, windowS),
		heatmapStory(t, srv, windowS),
		digestStory(t, st, now),
	}
	// The fixture must still be the shape this test is about: 24h minus 8h minus
	// 2h observed, and downtime that EXCLUDES the 2h nobody watched.
	if o := got[0].observedS; o < 14*3600-120 || o > 14*3600+120 {
		t.Fatalf("fixture no longer observes ~14h of the window (got %ds); the case is vacuous", o)
	}
	// The open outage runs from 8h ago to the fixture's trailing success sample a
	// minute ago, less the 2h nobody watched, plus the fixture's own 600s outage.
	wantDown := int((8*time.Hour - time.Minute - 2*time.Hour).Seconds()) + 600
	if d := got[0].downS; d < wantDown-120 || d > wantDown+120 {
		t.Fatalf("fixture books %ds of downtime, want ~%ds: an open 8h outage with 2h unobserved "+
			"inside it must book only the observed part, or this test is not exercising the subtraction",
			d, wantDown)
	}
	for i := 1; i < len(got); i++ {
		if agrees(got[0], got[i]) {
			continue
		}
		t.Errorf("%s and %s disagree about an ongoing outage with unobserved time inside it.\n"+
			"  %v\n  %v\n"+
			"Unobserved wall time is neither up nor down - including inside an outage that has\n"+
			"no duration_s to cap against.\nFull set:\n%s", got[0].who, got[i].who, got[0], got[i], render(got))
	}
}

// agrees compares two stories with the slack every renderer needs: each reads
// time.Now() a few milliseconds apart, so second-granularity spans can differ by
// one or two.
func agrees(a, b story) bool {
	if (a.uptimePct == notMeasured) != (b.uptimePct == notMeasured) {
		return false
	}
	if a.uptimePct != notMeasured && math.Abs(a.uptimePct-b.uptimePct) > 0.01 {
		return false
	}
	return math.Abs(float64(a.observedS-b.observedS)) <= 5 && math.Abs(float64(a.downS-b.downS)) <= 2
}

func render(ss []story) string {
	out := ""
	for _, s := range ss {
		out += "  " + s.String() + "\n"
	}
	return out
}
