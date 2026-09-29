package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// A RESTORE NEVER SHORTENS A RETENTION WINDOW.
//
// A backup carries its install's retention windows with the rest of its
// settings, and the Config chip is on by default. A config restore used to land
// them like any other row, so a keep-forever install restoring a backup set to
// an hour went to an hour at the reload, and the next cleanup deleted the
// install's OWN history down to that hour. Nothing warned: the warning only ever
// looked at the rows in the file, and a config-only restore has none.
//
// A retention row that would keep history for less time than the install keeps
// it now is left out, and the reply says which windows stayed and what the
// backup asked for. A longer window, keep forever included, still lands.

// keptNote returns the reply's note about the retention windows a restore left
// alone, or "" when there is none.
func keptNote(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	for _, w := range warningsOf(t, rr) {
		if strings.HasPrefix(w, "This backup keeps ") {
			return w
		}
	}
	return ""
}

// ownSamples gives the install history of its own: one latency sample per age.
func ownSamples(t *testing.T, st *store.Store, ages ...time.Duration) {
	t.Helper()
	for i, age := range ages {
		if err := st.InsertSamples(context.Background(), []store.Sample{{
			TS: time.Now().Add(-age), Target: fmt.Sprintf("own-%d", i), Family: "ipv4", LatencyMS: 12, Success: true,
		}}); err != nil {
			t.Fatalf("insert sample: %v", err)
		}
	}
}

// storedSetting reads one row of the settings table as the store holds it.
func storedSetting(t *testing.T, st *store.Store, key string) string {
	t.Helper()
	all, err := st.AllSettings(context.Background())
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	return all[key]
}

// THE REGRESSION, as it was found: a config-only restore of a backup that keeps
// latency for an hour, onto an install that keeps everything and holds two days
// of its own samples. The reply was a clean success with no warning, and the
// next cleanup deleted the samples.
func TestAConfigRestoreCannotShortenRetentionUnderTheInstallsOwnHistory(t *testing.T) {
	s, st := batchedServer(t, 0)
	ownSamples(t, st, 48*time.Hour, 47*time.Hour)

	rr := importBackup(t, s, "config=1", `{"pingularity_export":1,"config":[{"key":"retention_s","value":"3600"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if got := s.settings.Retention(); got != 0 {
		t.Errorf("latency retention is %v after the restore, want keep forever (0): a restore must not shorten it", got)
	}
	if got := storedSetting(t, st, "retention_s"); got == "3600" {
		t.Errorf("the backup's one-hour window was stored (%q); the next restart would make it live", got)
	}
	note := keptNote(t, rr)
	for _, want := range []string{"latency history for 1 hour", "latency history forever", "Data tab"} {
		if !strings.Contains(note, want) {
			t.Errorf("the reply does not say which window was kept and what the backup asked for (want %q in %q; warnings %q)",
				want, note, warningsOf(t, rr))
		}
	}
	if n := pruneNow(t, s); n != 0 {
		t.Errorf("the next cleanup removed %d of the install's own rows", n)
	}
	if n := stored(t, st, "samples"); n != 2 {
		t.Errorf("%d of the install's 2 samples are left", n)
	}
}

// The same through a whole restore: the backup's rows are recent, so nothing
// about them is old, but its config carries an hour. The old warning looked only
// at the file's rows and said nothing, and the next cleanup deleted the
// install's own two-day-old sample.
func TestAFullRestoreOfRecentRowsCannotShortenRetentionEither(t *testing.T) {
	s, st := batchedServer(t, 0)
	ownSamples(t, st, 48*time.Hour)

	body := `{"pingularity_export":2,"latency":[` + oldLatencyRow(time.Minute) + `],` +
		`"config":[{"key":"retention_s","value":"3600"}]}`
	rr := importBackup(t, s, "latency=1&config=1", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if n := importedCount(t, rr, "latency"); n != 1 {
		t.Fatalf("fixture: %d latency rows landed, want 1", n)
	}
	if got := s.settings.Retention(); got != 0 {
		t.Errorf("latency retention is %v after the restore, want keep forever (0)", got)
	}
	if keptNote(t, rr) == "" {
		t.Errorf("the reply does not say the backup's shorter window was left out (warnings %q)", warningsOf(t, rr))
	}
	if prunedWarning(t, rr, "latency") {
		t.Errorf("warned about restored rows that are a minute old (warnings %q)", warningsOf(t, rr))
	}
	if n := pruneNow(t, s); n != 0 {
		t.Errorf("the next cleanup removed %d rows; the install's own 48h-old sample must survive", n)
	}
	if n := stored(t, st, "samples"); n != 2 {
		t.Errorf("%d samples are left, want the install's own and the restored one", n)
	}
}

// Every window, both ways. A backup's window lands when it keeps history at
// least as long as the install does now - keep forever (0) is the longest - and
// is left out, with a note, when it keeps it for less time.
func TestARestoreLengthensARetentionWindowButNeverShortensOne(t *testing.T) {
	const day = 24 * time.Hour
	windows := []struct {
		key, cat string
		get      func(*Server) time.Duration
	}{
		{"retention_s", "latency", func(s *Server) time.Duration { return s.settings.Retention() }},
		{"speed_retention_s", "speed", func(s *Server) time.Duration { return s.settings.SpeedRetention() }},
		{"downtime_retention_s", "downtime", func(s *Server) time.Duration { return s.settings.DowntimeRetention() }},
	}
	cases := []struct {
		name   string
		here   time.Duration
		backup string
		want   time.Duration
		kept   bool
	}{
		{"a longer window lands", time.Hour, "2592000", 30 * day, false},
		{"keep forever lands", 30 * day, "0", 0, false},
		{"the same window lands", 30 * day, "2592000", 30 * day, false},
		{"a shorter window is left out", 30 * day, "3600", 30 * day, true},
		{"nothing is longer than keep forever", 0, "315360000", 0, true},
		{"keep forever onto keep forever", 0, "0", 0, false},
	}
	for _, w := range windows {
		for _, tc := range cases {
			t.Run(w.cat+"/"+tc.name, func(t *testing.T) {
				s := newTestServer(t)
				setRetention(t, s, tc.here, tc.here, tc.here)
				rr := importBackup(t, s, "config=1",
					`{"pingularity_export":1,"config":[{"key":"`+w.key+`","value":"`+tc.backup+`"}]}`)
				if rr.Code != http.StatusOK {
					t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
				}
				if got := w.get(s); got != tc.want {
					t.Errorf("%s retention is %v after restoring %s=%s onto %v, want %v", w.cat, got, w.key, tc.backup, tc.here, tc.want)
				}
				note := keptNote(t, rr)
				if (note != "") != tc.kept {
					t.Errorf("note %q, want one: %v (warnings %q)", note, tc.kept, warningsOf(t, rr))
				}
				if tc.kept && !strings.Contains(note, w.cat+" history") {
					t.Errorf("the note does not name the %s window: %q", w.cat, note)
				}
				// Nothing else in the note: the other two windows were not in the file.
				for _, other := range windows {
					if other.cat != w.cat && strings.Contains(note, other.cat+" history") {
						t.Errorf("the note names the %s window, which the backup did not carry: %q", other.cat, note)
					}
				}
			})
		}
	}
}

// A backup carrying all three windows, two of them shorter: one note names both
// and what each is kept at, and the longer third lands.
func TestTheNoteNamesEveryWindowTheRestoreLeftAlone(t *testing.T) {
	s := newTestServer(t)
	setRetention(t, s, 0, 7*24*time.Hour, 365*24*time.Hour)
	rr := importBackup(t, s, "config=1", `{"pingularity_export":1,"config":[`+
		`{"key":"retention_s","value":"86400"},`+
		`{"key":"speed_retention_s","value":"0"},`+
		`{"key":"downtime_retention_s","value":"604800"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if s.settings.Retention() != 0 || s.settings.SpeedRetention() != 0 || s.settings.DowntimeRetention() != 365*24*time.Hour {
		t.Errorf("windows after the restore: latency %v, speed %v, downtime %v; want forever, forever (lengthened), 365 days",
			s.settings.Retention(), s.settings.SpeedRetention(), s.settings.DowntimeRetention())
	}
	const want = "This backup keeps latency history for 1 day and downtime history for 7 days, but a restore never " +
		"shortens how long history is kept, so this install still keeps latency history forever and downtime history " +
		"for 365 days. If you want the backup's windows, lower them in the Data tab."
	if got := keptNote(t, rr); got != want {
		t.Errorf("note:\n got %q\nwant %q", got, want)
	}
}
