package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The heatmap walks a year one local day at a time by asking nextLocalDay for the
// next boundary, so every day's window, every day's observed span and every day's
// label rest on that one function. These tests pin it against oracles that share
// none of its reasoning, and then check what the operator actually reads.
//
// The shipped guard for this seam (TestDowntimeByDayDSTSkippedMidnight) passed with
// the boundary logic deleted outright and with a zone-blind d.Add(24h), because the
// two callers' "a boundary must advance" fallbacks keep the DOWNTIME arithmetic
// exact whatever nextLocalDay returns. Only the disclosure moves. So the tests
// below assert on nextLocalDay directly and on window_s/observed_s, not on totals.

// ---------------------------------------------------------------------------
// The oracles - and neither of them is a search.
//
// The first version of these tests answered "where does d's local date end" by
// bisecting on the date. That is only sound while the date grows with unix time,
// and it does not: Newfoundland put the clock back at 00:01 until 2011, so on
// 2010-11-06 the local date reached the 7th, dropped back to the 6th a minute
// later, and only stayed on the 7th an hour after that. A bisection lands in
// either stretch and calls both of them the answer, so an implementation that
// answered an hour late agreed with its own oracle - and the property check below
// accepted it too, because "the second before still carries d's date" is perfectly
// true of the late answer. An oracle that shares the implementation's assumption
// is not an oracle.
// ---------------------------------------------------------------------------

// oracleByScan is the dumbest answer there is: step forward one second at a time
// until the local date is no longer u's. It assumes nothing at all about how the
// local date behaves, and it costs a zone lookup for every second of the day - so
// it is the yardstick the cheap oracle is held to rather than the one the sweeps
// use.
func oracleByScan(u int64, loc *time.Location) int64 {
	y, m, d := time.Unix(u, 0).In(loc).Date()
	for t := u + 1; t < u+5*86400; t++ {
		if ay, am, ad := time.Unix(t, 0).In(loc).Date(); ay != y || am != m || ad != d {
			return t
		}
	}
	return -1
}

// zoneStretch is one run of constant offset: it begins at start, and the next one
// in the list begins where it ends.
type zoneStretch struct {
	start int64
	off   int
}

// zoneStretches lists loc's runs of constant offset across [from, to], oldest first.
//
// It walks ZoneBounds BACKWARDS from the end, because ZoneBounds' START is the half
// that can be relied on. Its END goes stale for the last day before a zone's stored
// transitions run out and its rule takes over - on this tzdata, 2040-12-31, where 192
// of the 598 zones answer with an end EARLIER than the instant asked about, so a walk
// that steps onto one never moves again. Every start is at or before the instant it
// was asked about, in every zone, at every six-hour probe from 1990 to 2041.
func zoneStretches(loc *time.Location, from, to int64) []zoneStretch {
	var starts []int64
	for x := to; x > from; {
		st, _ := time.Unix(x, 0).In(loc).ZoneBounds()
		if st.IsZero() || st.Unix() <= from || st.Unix() > x {
			break
		}
		starts = append(starts, st.Unix())
		x = st.Unix() - 1
	}
	off := func(u int64) int { _, o := time.Unix(u, 0).In(loc).Zone(); return o }
	out := make([]zoneStretch, 0, len(starts)+1)
	out = append(out, zoneStretch{from, off(from)})
	for i := len(starts) - 1; i >= 0; i-- {
		out = append(out, zoneStretch{starts[i], off(starts[i])})
	}
	return out
}

// oracleNextLocalDay solves for the answer instead of looking for it.
//
// Inside one run of constant offset, the seconds carrying a given local date are the
// solution of an inequality: with offset o, the date D covers exactly
// [midnight(D)-o, midnight(D)+86400-o), clipped to the run. So the seconds carrying
// u's date are a handful of computable PIECES, and a date that comes back simply has
// more than one of them. The answer is the end of the piece u is standing in - a
// question about u, not a search over time, which is why a local date that runs
// backwards costs this nothing. The dates themselves are read as u+offset in UTC, so
// the zone is asked for its offsets and for nothing else.
//
// TestTheCheapOracleAgreesWithASecondBySecondWalk holds it against oracleByScan,
// which reads nothing but the clock.
func oracleNextLocalDay(u int64, st []zoneStretch) int64 {
	i := sort.Search(len(st), func(k int) bool { return st[k].start > u }) - 1
	if i < 0 {
		return -1
	}
	y, m, d := time.Unix(u+int64(st[i].off), 0).UTC().Date()
	mid := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
	end := u
	for ; i < len(st); i++ {
		lo, hi := st[i].start, int64(1)<<62
		if i+1 < len(st) {
			hi = st[i+1].start
		}
		a, b := mid-int64(st[i].off), mid+86400-int64(st[i].off)
		if a < lo {
			a = lo
		}
		if b > hi {
			b = hi
		}
		if a >= b || a > end {
			return end // this run does not carry the date on from where it stopped
		}
		end = b
		if b < hi {
			return end // the date ran out before the clock changed
		}
	}
	return end
}

// localDateWentBack reports whether the local date at v is EARLIER than the one at
// u - the shape a fall-back makes when it carries the clock back past midnight, and
// the reason nothing here may assume the local date grows with unix time.
func localDateWentBack(u, v int64, loc *time.Location) bool {
	ay, am, ad := time.Unix(u, 0).In(loc).Date()
	by, bm, bd := time.Unix(v, 0).In(loc).Date()
	return by < ay || (by == ay && (bm < am || (bm == am && bd < ad)))
}

func sameLocalDate(u int64, d time.Time, loc *time.Location) bool {
	ay, am, ad := time.Unix(u, 0).In(loc).Date()
	by, bm, bd := d.In(loc).Date()
	return ay == by && am == bm && ad == bd
}

// tzdataZones lists every zone name in the tzdata this build reads, so the sweeps
// below cannot miss a zone by not having thought of it. LoadLocation searches
// $ZONEINFO and then the system directories; walk the first one that exists and
// let LoadLocation reject anything that is not a zone.
func tzdataZones(t *testing.T) []string {
	t.Helper()
	dirs := []string{os.Getenv("ZONEINFO"), "/usr/share/zoneinfo", "/usr/share/lib/zoneinfo", "/usr/lib/locale/TZ"}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		// /usr/share/zoneinfo is a symlink on macOS, and WalkDir does not follow one
		// at its root - it would report the link itself and stop.
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			dir = r
		}
		var names []string
		err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil //nolint:nilerr // an unreadable corner is not a reason to skip the rest
			}
			name := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
			// The index files and the leap-second variants shipped beside the zones.
			// Named exactly rather than by pattern: Etc/GMT+3 and friends are real
			// zones, and a filter on "+" quietly drops thirteen of them.
			if strings.HasSuffix(name, ".tab") || strings.HasSuffix(name, ".zi") ||
				strings.HasSuffix(name, ".list") || strings.HasPrefix(name, "posix/") ||
				strings.HasPrefix(name, "right/") || name == "+VERSION" ||
				name == "leapseconds" || name == "posixrules" {
				return nil
			}
			if _, err := time.LoadLocation(name); err == nil {
				names = append(names, name)
			}
			return nil
		})
		if err == nil && len(names) > 20 {
			return names
		}
	}
	t.Skip("no tzdata directory to enumerate")
	return nil
}

// The years the sweeps cover. They reach back to 1990 because the shape that broke
// the search - a fall-back that carries the clock back past midnight - was retired
// from the Canadian zones in 2011, and a guard that only looks at the present would
// not have caught it going in. tzdata rewrites history as often as it writes the
// future, so both ends matter.
var sweepFrom = time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
var sweepTo = time.Date(2042, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

// midnightJumpZones: the zones whose DST jump lands EXACTLY on local midnight, the
// two shapes that break a boundary built with time.Date(y, m, d, 0, 0, 0, 0, loc).
// Named explicitly as well as swept, so the sweep failing to reach them is itself
// visible.
var midnightJumpZones = []string{
	// Spring forward at 00:00 - the next local midnight does not exist.
	"America/Havana", "Cuba", "America/Santiago", "Chile/Continental", "Atlantic/Azores",
	// Fall back at 00:00 - the local midnight happens TWICE, and the day starts at
	// the first one.
	"Asia/Gaza", "Asia/Hebron", "Asia/Amman", "Antarctica/Casey", "Antarctica/Vostok",
	// Fall back PAST midnight - the local date goes backwards and comes again.
	"America/St_Johns", "Canada/Newfoundland", "America/Goose_Bay", "America/Moncton",
	// Historic non-advance, before the rule changed.
	"America/Asuncion", "America/Sao_Paulo", "America/Scoresbysund", "Antarctica/Palmer",
	// Controls: an ordinary 02:00 jump, half-hour and quarter-hour offsets, the
	// southern hemisphere, a zone with no DST at all.
	"America/New_York", "Europe/London", "Australia/Lord_Howe", "Pacific/Chatham",
	"Asia/Kathmandu", "Asia/Tehran", "Africa/Cairo", "UTC",
}

// ---------------------------------------------------------------------------
// (a) The pure tests. They read no clock, so they hold on every date rather than
// on whatever date the suite happens to run.
// ---------------------------------------------------------------------------

// The cheap oracle is what the sweeps lean on, so it gets held against the walk that
// assumes nothing: every second of four hours around every clock change that carries
// the local date backwards, two minutes around a sample of the rest, and a spread of
// ordinary days in every zone tzdata ships.
func TestTheCheapOracleAgreesWithASecondBySecondWalk(t *testing.T) {
	zones := tzdataZones(t)
	checked, windows, fail := 0, 0, 0
	// walk compares the two oracles at every second of [lo, hi).
	//
	// The second-by-second walk answers for a whole RUN of a date at once, so it is
	// asked once per run rather than once per second - and once per run, never once
	// per DATE, because the date is exactly the thing that comes back. It is also
	// started as late as it can be while still deriving its own answer: ten minutes
	// before the end the cheap oracle claims, or at the current second when that is
	// later. A claim that is too late puts that starting point past the date's real
	// end, which the walk sees as a different date; a claim that is too early makes
	// the walk return the real end instead. Either way the two disagree.
	walk := func(zn string, loc *time.Location, st []zoneStretch, lo, hi int64) {
		windows++
		var runEnd, want int64 = -1, -1
		for u := lo; u < hi; u++ {
			got := oracleNextLocalDay(u, st)
			if u >= runEnd {
				p := got - 600
				if p < u {
					p = u
				}
				if got <= u || !sameLocalDate(p, time.Unix(u, 0).In(loc), loc) {
					if fail++; fail <= 10 {
						t.Errorf("%s: the cheap oracle says %s's date runs to %s, but %s is already a different date",
							zn, time.Unix(u, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
							time.Unix(got, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
							time.Unix(p, 0).In(loc).Format("2006-01-02 15:04:05 MST"))
					}
					return
				}
				want = oracleByScan(p, loc)
				runEnd = want
			}
			checked++
			if got != want {
				if fail++; fail <= 10 {
					t.Errorf("%s: the cheap oracle says %s's date ends at %s, the second-by-second walk says %s",
						zn, time.Unix(u, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
						time.Unix(got, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
						time.Unix(want, 0).In(loc).Format("2006-01-02 15:04:05 MST"))
				}
				return
			}
		}
	}
	for _, zn := range zones {
		loc, err := time.LoadLocation(zn)
		if err != nil {
			continue
		}
		st := zoneStretches(loc, sweepFrom-30*86400, sweepTo+30*86400)
		for i := 1; i < len(st); i++ {
			c := st[i].start
			if c < sweepFrom || c >= sweepTo {
				continue
			}
			if localDateWentBack(c-1, c, loc) {
				walk(zn, loc, st, c-2*3600, c+2*3600)
			} else if i%8 == 0 {
				walk(zn, loc, st, c-120, c+120)
			}
		}
		// And away from every clock change, where the answer is pure arithmetic.
		for k := int64(0); k < 3; k++ {
			u := sweepFrom + (sweepTo-sweepFrom)*(3*k+1)/10
			walk(zn, loc, st, u, u+2)
		}
	}
	t.Logf("compared the two oracles at %d instants across %d windows in %d zones", checked, windows, len(zones))
	if checked < 100000 {
		t.Errorf("only %d instants compared: the window discovery found almost nothing to look at", checked)
	}
}

// A boundary is right when four things are true of it at once, and those four
// things are the WHOLE definition:
//
//	it is after d; d's local date has changed by then; it had not changed one
//	second earlier; and it had not changed at any second in between.
//
// The fourth is the one the old shape of this test was missing, and the one a
// bisection cannot see: where a local date comes back after an hour away, the
// LATER of its two endings satisfies the first three perfectly.
//
// Checked over every zone tzdata ships, at every clock change any of them makes
// between 1990 and 2041 and on a spread of ordinary days, and asked from several
// seconds of each day rather than only its first - proration enters nextLocalDay
// at an outage's start, which is any second at all, and the observation loop
// enters at the day's beginning, and the two must not be told different things.
func TestNextLocalDayIsTheFirstSecondOfTheNextLocalDate(t *testing.T) {
	zones := tzdataZones(t)
	badZones, badAsks, runs, asks := 0, 0, 0, 0
	for _, zn := range zones {
		loc, err := time.LoadLocation(zn)
		if err != nil {
			continue
		}
		st := zoneStretches(loc, sweepFrom-30*86400, sweepTo+30*86400)
		bad := 0
		walk := func(from, to int64) {
			for u := from; u < to; {
				d := time.Unix(u, 0).In(loc)
				end := oracleNextLocalDay(u, st)
				// The oracle's own answer has to look like a day boundary, so a wrong
				// oracle cannot quietly bless a wrong implementation.
				if end <= u || sameLocalDate(end, d, loc) || !sameLocalDate(end-1, d, loc) {
					t.Fatalf("%s: the oracle answered %d for %s, which is not a day boundary at all",
						zn, end, d.Format("2006-01-02 15:04:05 MST"))
				}
				runs++
				for _, a := range [...]int64{u, u + 1, u + (end-u)/2, u + (end-u)*3/4, end - 1} {
					if a < u || a >= end {
						continue
					}
					asks++
					got := nextLocalDay(time.Unix(a, 0).In(loc), loc)
					if got == end {
						continue
					}
					bad++
					badAsks++
					if bad <= 2 {
						t.Errorf("%s: asked from %s, nextLocalDay = %s, want %s",
							zn, time.Unix(a, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
							time.Unix(got, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
							time.Unix(end, 0).In(loc).Format("2006-01-02 15:04:05 MST"))
					}
				}
				u = end
			}
		}
		// A day and a quarter either side of every clock change the zone makes.
		for i := 1; i < len(st); i++ {
			if c := st[i].start; c >= sweepFrom && c < sweepTo {
				walk(c-30*3600, c+30*3600)
			}
		}
		// And a spread of ordinary days, so a zone whose arithmetic is wrong away
		// from every clock change cannot hide behind one.
		for k := int64(0); k < 24; k++ {
			u := sweepFrom + (sweepTo-sweepFrom)*k/24
			walk(u, u+2*86400)
		}
		if bad > 0 {
			badZones++
			t.Errorf("%s: %d wrong answers", zn, bad)
		}
	}
	t.Logf("swept %d zones over 1990-2041: %d runs of local date, %d asks, %d zones wrong, %d wrong answers",
		len(zones), runs, asks, badZones, badAsks)
}

// The shape that a search for the next date cannot answer: a fall-back that
// carries the clock back PAST midnight, so the local date is not increasing and
// the day the operator is living in has already been left once and come back.
// Found rather than listed - the zones that do it and the years they did it in
// are tzdata's business, and it has changed its mind about them before.
func TestNextLocalDayWhenTheClockGoesBackPastMidnight(t *testing.T) {
	zones := tzdataZones(t)
	found, checked := 0, 0
	var names []string
	for _, zn := range zones {
		loc, err := time.LoadLocation(zn)
		if err != nil {
			continue
		}
		st := zoneStretches(loc, sweepFrom-30*86400, sweepTo+30*86400)
		hit := false
		for i := 1; i < len(st); i++ {
			c := st[i].start
			if c < sweepFrom || c >= sweepTo || !localDateWentBack(c-1, c, loc) {
				continue
			}
			found++
			if !hit {
				hit = true
				names = append(names, zn)
			}
			// Every second of two hours either side, against the walk that assumes
			// nothing. The answer is the same for every second of one RUN of the date
			// - so ask the walk once per run, never once per date, because the date
			// itself comes back and asking per date is how this was missed.
			var runEnd, want int64 = -1, -1
			for u := c - 2*3600; u < c+2*3600; u++ {
				if u >= runEnd {
					want = oracleByScan(u, loc)
					runEnd = want
				}
				checked++
				if got := nextLocalDay(time.Unix(u, 0).In(loc), loc); got != want {
					t.Errorf("%s: asked from %s, nextLocalDay = %s, want %s",
						zn, time.Unix(u, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
						time.Unix(got, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
						time.Unix(want, 0).In(loc).Format("2006-01-02 15:04:05 MST"))
					break
				}
			}
		}
	}
	t.Logf("%d clock changes carry the date backwards past midnight in %d zones (%s); %d seconds checked",
		found, len(names), strings.Join(names, ", "), checked)
	if found == 0 {
		t.Skip("this tzdata has no zone that puts the clock back past midnight")
	}
}

// Every second of the disturbed hour itself, one at a time, in the zones and on the
// days where the jump lands on midnight. A stride can step over a one-hour hole; a
// per-second walk cannot.
func TestNextLocalDayThroughEverySecondOfAMidnightJump(t *testing.T) {
	for _, tc := range []struct {
		zone string
		day  string // the LOCAL date the jump disturbs
	}{
		// Midnight never happens: the day starts at 01:00.
		{"America/Santiago", "2025-09-07"}, {"America/Santiago", "2026-09-06"},
		{"America/Havana", "2025-03-09"}, {"America/Havana", "2026-03-08"},
		{"Atlantic/Azores", "2025-03-30"}, {"Atlantic/Azores", "2026-03-29"},
		// Midnight happens twice: the day starts at the FIRST one. Havana and the
		// Azores fall back onto midnight every autumn and the old code got them
		// right only by accident - time.Date resolves an ambiguous wall time by
		// looking the offset up at the naive UTC instant, which for a zone west of
		// Greenwich lands before the transition and so happens to pick the earlier
		// midnight. East of it the same lookup lands after, and the answer is an
		// hour late.
		{"America/Havana", "2025-11-02"}, {"America/Havana", "2026-11-01"},
		{"Atlantic/Azores", "2025-10-26"}, {"Atlantic/Azores", "2026-10-25"},
		{"Asia/Gaza", "2018-10-27"}, {"Asia/Hebron", "2020-10-24"},
		{"Asia/Amman", "2017-10-27"}, {"Antarctica/Casey", "2019-03-17"},
		{"Antarctica/Vostok", "2023-12-18"},
		// The clock going back past midnight, on days it happened.
		{"America/St_Johns", "2010-11-06"}, {"America/Goose_Bay", "2005-10-29"},
		{"America/Moncton", "2004-10-30"}, {"Antarctica/Casey", "2010-03-04"},
	} {
		loc, err := time.LoadLocation(tc.zone)
		if err != nil {
			t.Skipf("tzdata unavailable: %v", err)
		}
		day, err := time.ParseInLocation("2006-01-02", tc.day, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		// A full 30 hours from 03:00 UTC the day before covers the local day and both
		// of its edges whatever the zone's offset is.
		from := day.Unix() - 21*3600
		st := zoneStretches(loc, from-4*86400, from+5*86400)
		bad := 0
		// The oracle's answer is the same for every second of one RUN of the date, so
		// ask it once per run rather than once per second. Once per DATE would be
		// wrong: the date comes back, and the answer the second time is not the
		// answer the first time.
		var runEnd, want int64 = -1, -1
		for u := from; u < from+30*3600; u++ {
			if u >= runEnd {
				want = oracleNextLocalDay(u, st)
				runEnd = want
			}
			got := nextLocalDay(time.Unix(u, 0).In(loc), loc)
			if got != want {
				if bad++; bad <= 3 {
					t.Errorf("%s around %s: nextLocalDay(%s) = %s, want %s",
						tc.zone, tc.day, time.Unix(u, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
						time.Unix(got, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
						time.Unix(want, 0).In(loc).Format("2006-01-02 15:04:05 MST"))
				}
			}
		}
		if bad > 0 {
			t.Errorf("%s around %s: %d seconds answered wrongly", tc.zone, tc.day, bad)
		}
	}
}

// Two shapes no zone tzdata ships has, built by hand out of the bytes
// LoadLocationFromTZData reads. Neither is idle arithmetic: both are ordinary
// consequences of rules a zone could publish tomorrow, and each of them is the
// difference between the right day boundary and one a whole day out. tzdata has
// written stranger rules than these - Newfoundland's 00:01 changeover was one.
//
// handBuiltZone starts a zone at UTC+0 and then applies the clock changes given,
// each to its own offset.
func handBuiltZone(t *testing.T, name string, changes []struct {
	when int64
	off  int32
}) *time.Location {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("TZif")
	b.Write(make([]byte, 16)) // version 0, then its padding
	abbrev := "XX0\x00" + strings.Repeat("XXX\x00", len(changes))
	// counts: UTC/local indicators, standard/wall indicators, leap seconds, clock
	// changes, offsets, abbreviation bytes.
	for _, n := range []uint32{0, 0, 0, uint32(len(changes)), uint32(len(changes) + 1), uint32(len(abbrev))} {
		if err := binary.Write(&b, binary.BigEndian, n); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range changes {
		if err := binary.Write(&b, binary.BigEndian, int32(c.when)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range changes { // which offset each change switches to
		b.Write([]byte{byte(i + 1)})
	}
	if err := binary.Write(&b, binary.BigEndian, int32(0)); err != nil { // the offset it starts at
		t.Fatal(err)
	}
	b.Write([]byte{0, 0})
	for i, c := range changes {
		if err := binary.Write(&b, binary.BigEndian, c.off); err != nil {
			t.Fatal(err)
		}
		b.Write([]byte{0, byte(4 * (i + 1))})
	}
	b.WriteString(abbrev)
	loc, err := time.LoadLocationFromTZData(name, b.Bytes())
	if err != nil {
		t.Fatalf("building %s: %v", name, err)
	}
	return loc
}

// perSecondAgainstTheWalk compares nextLocalDay with oracleByScan at every second
// of [from, to). The walk's answer is the same for every second of one run of the
// date, so it is asked once per run.
func perSecondAgainstTheWalk(t *testing.T, zn string, loc *time.Location, from, to int64) {
	t.Helper()
	bad := 0
	var runEnd, want int64 = -1, -1
	for u := from; u < to; u++ {
		if u >= runEnd {
			want = oracleByScan(u, loc)
			runEnd = want
		}
		if got := nextLocalDay(time.Unix(u, 0).In(loc), loc); got != want {
			if bad++; bad <= 3 {
				t.Errorf("%s: asked from %s, nextLocalDay = %s, want %s",
					zn, time.Unix(u, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
					time.Unix(got, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
					time.Unix(want, 0).In(loc).Format("2006-01-02 15:04:05 MST"))
			}
		}
	}
	if bad > 0 {
		t.Errorf("%s: %d seconds answered wrongly", zn, bad)
	}
}

// TWO clock changes inside one local day, with the date turning at the FIRST of
// them. The closest pair of changes anywhere in the 598 zones tzdata ships, between
// 1990 and 2041, is just under seven days apart, so the walk back to the earliest
// change in the day cannot be exercised against real data - and a boundary that
// stopped at the LAST change instead passes every other test here.
//
// The zone: UTC until 20:00 on 2000-06-15, when the clock jumps four hours and the
// date turns with it, and two hours later back one hour, still on the 16th. The
// boundary is the first change.
func TestNextLocalDayEndsTheDayAtTheFirstClockChangeThatTurnsIt(t *testing.T) {
	first := time.Date(2000, 6, 15, 20, 0, 0, 0, time.UTC).Unix()
	second := time.Date(2000, 6, 15, 22, 0, 0, 0, time.UTC).Unix()
	loc := handBuiltZone(t, "Test/TwoClockChanges", []struct {
		when int64
		off  int32
	}{{first, 4 * 3600}, {second, 3 * 3600}})
	for _, c := range []struct {
		at   int64
		want string
	}{{first, "2000-06-16 00:00:00"}, {second, "2000-06-16 01:00:00"}} {
		if got := time.Unix(c.at, 0).In(loc).Format("2006-01-02 15:04:05"); got != c.want {
			t.Fatalf("the hand-built zone does not do what it was built to do: %s, want %s", got, c.want)
		}
	}
	perSecondAgainstTheWalk(t, "Test/TwoClockChanges", loc, first-26*3600, second+26*3600)
}

// A clock change that lands in the LAST SECOND of a local date, so the date turns
// one second after it rather than at it. The walk has to carry on from the change
// itself; resuming from the second after it reads a clock that has already turned
// over, and the boundary comes back a whole day late.
//
// The zone: UTC until 20:00 on 2000-06-15, when the clock jumps to 23:59:59 the
// same evening.
func TestNextLocalDayWhenAClockChangeLandsInTheLastSecondOfADate(t *testing.T) {
	when := time.Date(2000, 6, 15, 20, 0, 0, 0, time.UTC).Unix()
	loc := handBuiltZone(t, "Test/LastSecond", []struct {
		when int64
		off  int32
	}{{when, 4*3600 - 1}})
	if got := time.Unix(when, 0).In(loc).Format("2006-01-02 15:04:05"); got != "2000-06-15 23:59:59" {
		t.Fatalf("the hand-built zone does not do what it was built to do: %s", got)
	}
	perSecondAgainstTheWalk(t, "Test/LastSecond", loc, when-26*3600, when+26*3600)
}

// loc is the zone the answer is in, and d is only an instant: the same second
// asked about must give the same boundary whether the caller happens to be
// holding it as UTC, as loc, or as somewhere else entirely. prorate and the
// observation loop both pass a Time already converted to loc, so nothing today
// notices - which is exactly why it is worth pinning before something does.
func TestNextLocalDayIgnoresTheLocationTheInstantCarries(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	begin := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix()
	for u := begin; u < begin+10*86400; u += 1801 {
		want := nextLocalDay(time.Unix(u, 0).In(loc), loc)
		for _, other := range []*time.Location{time.UTC, tokyo} {
			if got := nextLocalDay(time.Unix(u, 0).In(other), loc); got != want {
				t.Fatalf("the same second read as %s answered %s, but read in the target zone it answered %s",
					other, time.Unix(got, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
					time.Unix(want, 0).In(loc).Format("2006-01-02 15:04:05 MST"))
			}
		}
	}
}

// A whole calendar DATE that never happens. Pacific/Apia crossed the date line at
// the end of 2011 and 2011-12-30 does not exist there, so "the day after the 29th"
// cannot be constructed at all - a boundary that walks towards it an hour at a time
// never arrives. The answer is the 31st, and it has to come back.
func TestNextLocalDaySkippedCalendarDate(t *testing.T) {
	loc, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	d := time.Date(2011, 12, 29, 0, 0, 0, 0, loc)
	done := make(chan int64, 1)
	go func() { done <- nextLocalDay(d, loc) }()
	select {
	case got := <-done:
		if want := oracleByScan(d.Unix(), loc); got != want {
			t.Errorf("nextLocalDay(%s) = %s, want %s",
				d.Format("2006-01-02 15:04:05 MST"),
				time.Unix(got, 0).In(loc).Format("2006-01-02 15:04:05 MST"),
				time.Unix(want, 0).In(loc).Format("2006-01-02 15:04:05 MST"))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nextLocalDay never returned: it is looking for a local date this zone does not have")
	}
}

// It runs once per local day per heatmap poll - 366 times a minute for a year-long
// window, and again for every day-segment of every outage in it. Keep it off the
// heap.
func TestNextLocalDayDoesNotAllocate(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	d := time.Date(2026, 9, 5, 0, 0, 0, 0, loc)
	if n := testing.AllocsPerRun(200, func() { _ = nextLocalDay(d, loc) }); n != 0 {
		t.Errorf("nextLocalDay allocates %.1f times per call, want 0", n)
	}
	// And from inside a day whose clock changes, where the walk takes a second pass.
	nfld, err := time.LoadLocation("America/St_Johns")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	e := time.Date(2010, 11, 6, 12, 0, 0, 0, nfld)
	if n := testing.AllocsPerRun(200, func() { _ = nextLocalDay(e, nfld) }); n != 0 {
		t.Errorf("nextLocalDay allocates %.1f times per call across a clock change, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// (b) What the operator reads. window_s is the number the heatmap tooltip renders
// as "of 23h 0m", so it has to be the day's real length.
// ---------------------------------------------------------------------------

// dstFileStore: file-backed, because a :memory: store is one connection and hides
// every multi-connection failure.
func dstFileStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "heatmap.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// localDayStart finds the first instant of a named local date by searching for it,
// never by building it: on the days this test is about, building it is the thing
// that goes wrong.
func localDayStart(t *testing.T, date string, loc *time.Location) time.Time {
	t.Helper()
	day, err := time.ParseInLocation("2006-01-02", date, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	for u := day.Unix() - 24*3600; u < day.Unix()+24*3600; u++ {
		if time.Unix(u, 0).In(loc).Format("2006-01-02") == date {
			return time.Unix(u, 0).In(loc)
		}
	}
	t.Fatalf("%s never happens in %s", date, loc)
	return time.Time{}
}

// A day whose leading hour is watched and whose afternoon is paused must disclose
// the day's REAL length. The observation loop mints a day's row on the first tile
// that is not fully observed and ASSIGNS the tile's span to window_s, so any part
// of the day the boundary walk chopped into fully-observed slivers before that
// point is simply not in the number: on the eve of a midnight spring-forward the
// heatmap told a Chilean operator it had "monitored 19h 0m of 23h 0m" of an
// ordinary 24-hour Saturday.
func TestDowntimeByDayDisclosesTheWholeDayAcrossAMidnightJump(t *testing.T) {
	for _, tc := range []struct {
		zone    string
		date    string
		wantLen int // the day's true length in seconds
	}{
		// The eve of a spring-forward that lands on midnight: a full 24 hours whose
		// LAST second is 23:59:59, followed by 01:00:00 the next day.
		{"America/Santiago", "2026-09-05", 24 * 3600},
		{"America/Santiago", "2026-09-06", 23 * 3600}, // the short day itself
		{"America/Santiago", "2026-09-03", 24 * 3600}, // control: an ordinary day
		{"America/Havana", "2026-03-07", 24 * 3600},
		{"America/Havana", "2026-03-08", 23 * 3600},
		{"Atlantic/Azores", "2026-03-28", 24 * 3600},
		{"Atlantic/Azores", "2026-03-29", 23 * 3600},
		// Controls: a jump that lands at 02:00 like most of the world's, and a zone
		// with no jump at all.
		{"America/New_York", "2026-03-07", 24 * 3600},
		{"America/New_York", "2026-03-08", 23 * 3600},
		{"UTC", "2026-03-08", 24 * 3600},
	} {
		t.Run(tc.zone+" "+tc.date, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Skipf("tzdata unavailable: %v", err)
			}
			start := localDayStart(t, tc.date, loc)
			st := dstFileStore(t)
			ctx := context.Background()
			// Monitoring began well before the window, so nothing in it is unobserved
			// except the pause.
			if err := st.InsertSamples(ctx, []Sample{{
				TS: start.Add(-10 * 24 * time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10,
			}}); err != nil {
				t.Fatal(err)
			}
			// Four hours off in the afternoon: the day's first hour stays watched, so
			// the row is minted by a LATER tile and whatever the boundary walk did to
			// the first hour has to have been carried into it.
			const pauseLen = 4 * 3600
			if ok, err := st.InsertPause(ctx, start.Add(10*time.Hour), pauseLen); err != nil || !ok {
				t.Fatalf("insert pause: stored=%v err=%v", ok, err)
			}
			nowU := start.Unix() + 3*86400
			rows, err := st.downtimeByDayAt(ctx, start.Add(-2*24*time.Hour), loc, nowU)
			if err != nil {
				t.Fatal(err)
			}
			var got *DowntimeDay
			for i := range rows {
				if rows[i].Date == tc.date {
					got = &rows[i]
				}
			}
			if got == nil {
				t.Fatalf("no row for %s at all (rows: %v)", tc.date, rows)
			}
			if got.WindowS != tc.wantLen {
				t.Errorf("window_s = %d, want %d: the tooltip reads %q for a day that is %s long",
					got.WindowS, tc.wantLen,
					(time.Duration(got.WindowS) * time.Second).String(),
					(time.Duration(tc.wantLen) * time.Second).String())
			}
			if want := tc.wantLen - pauseLen; got.ObservedS != want {
				t.Errorf("observed_s = %d, want %d (the day minus the four-hour pause)", got.ObservedS, want)
			}
		})
	}
}

// The other half of the same walk: whatever the boundaries do, the tiles must still
// partition the window exactly once, so a multi-day outage's seconds are neither
// lost nor double-booked and no single day can hold more than its own length.
func TestDowntimeByDayKeepsOutageTotalsAcrossAMidnightJump(t *testing.T) {
	for _, zone := range []string{"UTC", "America/Santiago", "America/Havana", "Atlantic/Azores", "Asia/Gaza"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Skipf("tzdata unavailable: %v", err)
		}
		st := dstFileStore(t)
		ctx := context.Background()
		down := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
		up := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
		if err := st.InsertEvent(ctx, down, "down", -1, ""); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertEvent(ctx, up, "up", int(up.Sub(down).Seconds()), ""); err != nil {
			t.Fatal(err)
		}
		rows, err := st.downtimeByDayAt(ctx, down.Add(-24*time.Hour), loc, up.Unix()+86400)
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, r := range rows {
			if r.DowntimeS > 25*3600 {
				t.Errorf("%s: day %s books %ds, longer than any local day", zone, r.Date, r.DowntimeS)
			}
			total += r.DowntimeS
		}
		if want := int(up.Sub(down).Seconds()); total != want {
			t.Errorf("%s: prorated downtime = %ds, want %ds", zone, total, want)
		}
	}
}

// The two loops meeting from opposite sides. On 2010-11-06 in Newfoundland the
// clock reads a date it has already read: 00:00:59 on the 7th, then 23:01 on the
// 6th again, then the 7th for good an hour later. The observation loop enters that
// hour from the day's beginning and proration enters it from the outage's start,
// and they have to bucket it the same way - which is the whole reason they share
// this function. A day credited with more downtime than it has window is what
// disagreement looks like from the tooltip.
func TestDowntimeByDayBooksEverySecondUnderItsOwnLocalDate(t *testing.T) {
	loc, err := time.LoadLocation("America/St_Johns")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	st := dstFileStore(t)
	ctx := context.Background()
	down := localDayStart(t, "2010-11-06", loc).Add(time.Hour)
	up := localDayStart(t, "2010-11-08", loc).Add(time.Hour)
	if err := st.InsertEvent(ctx, down, "down", -1, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, up, "up", int(up.Sub(down).Seconds()), ""); err != nil {
		t.Fatal(err)
	}
	since := localDayStart(t, "2010-11-04", loc)
	nowU := up.Unix() + 2*3600
	rows, err := st.downtimeByDayAt(ctx, since, loc, nowU)
	if err != nil {
		t.Fatal(err)
	}
	window, downtime, total := map[string]int{}, map[string]int{}, 0
	for _, r := range rows {
		window[r.Date] += r.WindowS
		downtime[r.Date] += r.DowntimeS
		total += r.WindowS
		if r.DowntimeS > r.WindowS {
			t.Errorf("%s: downtime_s %d is more than the day's own window_s %d - proration and the "+
				"observation loop disagree about which day the repeated hour belongs to",
				r.Date, r.DowntimeS, r.WindowS)
		}
	}
	if want := int(nowU - since.Unix()); total != want {
		t.Errorf("the day tiles cover %ds of a %ds window: they must partition it exactly once", total, want)
	}
	// The clock says "6 November" for 24 hours, then for another 59 minutes after
	// the hour it spends on the 7th - so the 6th is 24h59m long and the 7th 24h01m,
	// and every second of both is booked under the date the clock was showing.
	for _, tc := range []struct {
		date             string
		window, downtime int
	}{
		{"2010-11-05", 86400, 0},
		{"2010-11-06", 24*3600 + 59*60, 23*3600 + 59*60},
		{"2010-11-07", 24*3600 + 60, 24*3600 + 60},
		{"2010-11-08", 3 * 3600, 3600},
	} {
		if window[tc.date] != tc.window {
			t.Errorf("%s: window_s = %d, want %d", tc.date, window[tc.date], tc.window)
		}
		if downtime[tc.date] != tc.downtime {
			t.Errorf("%s: downtime_s = %d, want %d", tc.date, downtime[tc.date], tc.downtime)
		}
	}
	if want := int(up.Sub(down).Seconds()); downtime["2010-11-06"]+downtime["2010-11-07"]+downtime["2010-11-08"] != want {
		t.Errorf("prorated downtime totals %d, want %d",
			downtime["2010-11-06"]+downtime["2010-11-07"]+downtime["2010-11-08"], want)
	}
}
