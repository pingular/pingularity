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

// A file can carry a retention key more than once: hand-built ones do. Each
// row was weighed against the window in force as it was read, and the window
// in force does not move while the config streams in, so a later, shorter row
// was measured against the window from before the restore and not against
// the longer one the same file had just set. Ninety days and then one day,
// onto thirty: the ninety landed, the one was left out, and the note said the
// install "still keeps" ninety days, which it had never kept. With sixty days
// after those two, the sixty landed over the ninety, unless a reload signal
// happened to come between them, in which case it did not. A row is now
// weighed against the longer of the window from before the restore and the
// one this file has already set, so the longest row of the file stands
// whatever reloads in between, and the note says "now keeps" where the window
// moved.
func TestARetentionKeyRepeatedInOneFileKeepsItsLongestWindow(t *testing.T) {
	const day = 24 * time.Hour
	row := func(d time.Duration) string {
		return fmt.Sprintf(`{"key":"retention_s","value":"%d"}`, int64(d/time.Second))
	}
	for _, tc := range []struct {
		name   string
		rows   []time.Duration
		reload bool // a reload signal lands while the config streams in
		want   time.Duration
		note   string
	}{
		{"a longer window and then a shorter one", []time.Duration{90 * day, day}, false, 90 * day,
			"This backup keeps latency history for 1 day, but a restore never shortens how long history is kept, so " +
				"this install now keeps latency history for 90 days. If you want the backup's window, lower it in the Data tab."},
		{"a longer, a shorter, and one in between", []time.Duration{90 * day, day, 60 * day}, false, 90 * day,
			"This backup keeps latency history for 60 days, but a restore never shortens how long history is kept, so " +
				"this install now keeps latency history for 90 days. If you want the backup's window, lower it in the Data tab."},
		{"the same with a reload signal in between", []time.Duration{90 * day, day, 60 * day}, true, 90 * day,
			"This backup keeps latency history for 60 days, but a restore never shortens how long history is kept, so " +
				"this install now keeps latency history for 90 days. If you want the backup's window, lower it in the Data tab."},
		{"a shorter window and then a longer one", []time.Duration{day, 90 * day}, false, 90 * day, ""},
		{"two shorter windows", []time.Duration{day, 7 * day}, false, 30 * day,
			"This backup keeps latency history for 7 days, but a restore never shortens how long history is kept, so " +
				"this install still keeps latency history for 30 days. If you want the backup's window, lower it in the Data tab."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := batchedServer(t, 0)
			setRetention(t, s, 30*day, 30*day, 30*day)
			send, finish := pipedImport(t, s, "config=1")
			send(`{"pingularity_export":2,"config":[` + row(tc.rows[0]))
			for _, d := range tc.rows[1:] {
				if tc.reload {
					// The rows so far are in memory, not stored: a batch is stored
					// when it is full or the category ends. What a reload signal can
					// publish early is a row an earlier batch stored, so store the
					// first row the way that batch would have, and reload.
					if err := st.SetSettings(context.Background(), map[string]string{"retention_s": fmt.Sprint(int64(tc.rows[0] / time.Second))}); err != nil {
						t.Fatal(err)
					}
					if err := s.settings.Reload(context.Background()); err != nil {
						t.Fatalf("reload: %v", err)
					}
				}
				send(`,` + row(d))
			}
			send(`]}`)
			rr := finish()
			if rr.Code != http.StatusOK {
				t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if got := s.settings.Retention(); got != tc.want {
				t.Errorf("latency retention is %v after the restore, want %v", got, tc.want)
			}
			if got := keptNote(t, rr); got != tc.note {
				t.Errorf("the reply's note about the windows it kept is\n  %q\nwant\n  %q", got, tc.note)
			}
		})
	}
}

// A settings save and a restore's config take turns. The restore weighs each
// retention row of the backup against the window the install keeps, and the
// row is stored some way after that, so a save that raised the window in
// between was overwritten by a backup's window shorter than the one just
// saved, with nothing in the reply: the guard turns requests away once a
// restore has reached its config, but not a save already past it. Here a save
// raising latency from thirty days to ninety is under way when a restore whose
// backup keeps sixty arrives. The restore waits for it, weighs the sixty
// against the ninety, and leaves it out.
//
// Without the turn the restore does not wait: it finishes within milliseconds,
// on the thirty, and says nothing. restoreHold is how long the test gives it
// to do that before it lets the save go on, so a slow machine can only make
// this pass without the turn, never fail with it.
func TestASettingsSaveUnderWayFinishesBeforeARestoreWeighsItsWindows(t *testing.T) {
	const day = 24 * time.Hour
	s, _ := batchedServer(t, 0)
	setRetention(t, s, 30*day, 30*day, 30*day)

	inSave, goOn := make(chan struct{}), make(chan struct{})
	heldInSave := false
	settingsSaveHook = func() {
		if s.importMu.TryLock() {
			s.importMu.Unlock()
		} else {
			heldInSave = true
		}
		close(inSave)
		<-goOn
	}
	t.Cleanup(func() { settingsSaveHook = nil })
	saved := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"retention_seconds":7776000}`))
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = "127.0.0.1:54321"
		r.Header.Set("Content-Type", "application/json")
		s.Handler().ServeHTTP(rr, r)
		saved <- rr
	}()
	select {
	case <-inSave:
	case rr := <-saved:
		t.Fatalf("fixture: the save answered HTTP %d before it wrote: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if !heldInSave {
		t.Errorf("a settings save writes without the lock a restore's config holds (importMu)")
	}

	restored := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		restored <- importBackup(t, s, "config=1", `{"pingularity_export":2,"config":[{"key":"retention_s","value":"5184000"}]}`)
	}()
	var rr *httptest.ResponseRecorder
	select {
	case rr = <-restored: // only without the turn
	case <-time.After(restoreHold):
	}
	close(goOn)
	if got := <-saved; got.Code != http.StatusOK {
		t.Fatalf("save: HTTP %d: %s", got.Code, strings.TrimSpace(got.Body.String()))
	}
	if rr == nil {
		select {
		case rr = <-restored:
		case <-time.After(30 * time.Second):
			t.Fatal("the restore never finished once the save had")
		}
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if got := s.settings.Retention(); got != 90*day {
		t.Errorf("latency retention is %v after the save and the restore, want the 90 days just saved", got)
	}
	want := "This backup keeps latency history for 60 days, but a restore never shortens how long history is kept, so " +
		"this install still keeps latency history for 90 days. If you want the backup's window, lower it in the Data tab."
	if got := keptNote(t, rr); got != want {
		t.Errorf("the restore weighed the backup's sixty days against a window from before the save: its note is %q, "+
			"want %q", got, want)
	}
}
