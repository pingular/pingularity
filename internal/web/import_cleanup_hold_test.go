package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// NO CLEANUP STARTS WHILE A BACKUP IS BEING RESTORED.
//
// Our exports write the config last, so a backup's retention windows come into
// force at the reload that ends its restore, after every row. The hourly
// cleanup that came due in the middle cut at the windows in force at that
// moment. For a backup that keeps history for longer than the install did -
// a long history moved onto a new install, where the first cleanup comes ten
// minutes after the start - that deleted the rows restored so far, just before
// the window that keeps them arrived, and the reply then told the operator to
// raise a window that was raised already. The restore now holds the cleanup
// off until it has replied (store.HoldCleanup). These tests send the file in
// parts, on a real file, the daemon's pool of connections.

// The regression, in our own export order: an install that keeps latency for
// thirty days restores readings forty days old and a config that keeps them
// forever, and the hourly cleanup comes due once the readings are in and
// before the config. It deleted both readings. It is now skipped, the readings
// stay, and the reply has nothing to warn about.
func TestNoCleanupStartsWhileABackupIsBeingRestored(t *testing.T) {
	const day = 24 * time.Hour
	s, st := batchedServer(t, 0)
	setRetention(t, s, 30*day, 30*day, 30*day)
	skipped := stats.Lifetime().Counters["db.prune_skipped_restore"]

	send, finish := pipedImport(t, s, "latency=1&config=1")
	send(`{"pingularity_export":2,"latency":[` + oldLatencyRow(40*day) + `,` + oldLatencyRow(41*day) + `],"config":[`)
	storedRows(t, st, "samples", 2)
	n, err := st.PruneLive(context.Background(), liveWindows(s))
	if !errors.Is(err, store.ErrCleanupHeld) || n != 0 {
		t.Errorf("a cleanup that came due in the middle of the restore returned %d rows, %v; want it skipped "+
			"(store.ErrCleanupHeld) with nothing deleted", n, err)
	}
	if got := stats.Lifetime().Counters["db.prune_skipped_restore"] - skipped; got != 1 {
		t.Errorf("db.prune_skipped_restore moved by %d for the one skipped pass, want 1", got)
	}
	send(`{"key":"retention_s","value":"0"}]}`)
	rr := finish()
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if got := s.settings.Retention(); got != 0 {
		t.Fatalf("fixture: the backup's keep-forever window did not go live (%v)", got)
	}
	if got := stored(t, st, "samples"); got != 2 {
		t.Errorf("%d of the 2 restored readings are left; the backup's own window keeps both", got)
	}
	if got := warningsOf(t, rr); len(got) != 0 {
		t.Errorf("nothing was lost and nothing is due, but the reply warns %q", got)
	}
	// The pass after the restore runs, at the windows the restore left.
	if n := pruneNow(t, s); n != 0 {
		t.Errorf("the cleanup after the restore removed %d rows under a keep-forever window", n)
	}
}

// The hold ends with the restore, however the restore ends. A hold left
// behind would stop every later cleanup until the daemon restarted.
func TestTheCleanupHoldEndsHoweverTheRestoreEnds(t *testing.T) {
	row := oldLatencyRow(time.Minute)
	for _, tc := range []struct {
		name, query, body string
		gone, panics      bool
		status            int
	}{
		{name: "a restore that succeeds", query: "latency=1",
			body: `{"pingularity_export":2,"latency":[` + row + `]}`, status: http.StatusOK},
		{name: "a restore that fails part way", query: "latency=1&speed=1",
			body:   `{"pingularity_export":2,"latency":[` + row + `],"speed":[{"ts":1,"server":"s","warp_factor":9}]}`,
			status: http.StatusBadRequest},
		{name: "a file cut short", query: "latency=1",
			body: `{"pingularity_export":2,"latency":[` + row, status: http.StatusBadRequest},
		{name: "a client that has gone", query: "latency=1", gone: true,
			body: `{"pingularity_export":2,"latency":[` + row + `]}`, status: http.StatusInternalServerError},
		{name: "a restore that panics", query: "config=1", panics: true,
			body: `{"pingularity_export":2,"config":[{"key":"latency_interval_s","value":"10"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			if tc.panics {
				importReconcileHook = func() { panic("a fault in the middle of a restore") }
				t.Cleanup(func() { importReconcileHook = nil })
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.gone {
				cancel()
			}
			rr := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/api/import?"+tc.query, strings.NewReader(tc.body)).WithContext(ctx)
			r.Host = "127.0.0.1:9000"
			r.RemoteAddr = "127.0.0.1:54321"
			r.Header.Set("Content-Type", "application/json")
			func() {
				defer func() {
					if p := recover(); p != nil && !tc.panics {
						panic(p)
					}
				}()
				s.Handler().ServeHTTP(rr, r)
			}()
			if !tc.panics && rr.Code != tc.status {
				t.Fatalf("fixture: the restore answered HTTP %d, want %d: %s", rr.Code, tc.status,
					strings.TrimSpace(rr.Body.String()))
			}
			if _, err := s.store.PruneLive(context.Background(), liveWindows(s)); err != nil {
				t.Errorf("the cleanup after %s returned %v; the restore's hold on it must end with the restore",
					tc.name, err)
			}
		})
	}
}

// A cleanup that was already running when the restore began goes on, and until
// the backup's config lands it cuts at the install's own window. The reply
// says what it deleted, and its advice has to be true of the windows now in
// force. Where the backup lengthened the window the cleanup cut at, the reply
// used to say to raise that window and import again, with the window raised
// already: importing again is all there is to do, and it says that. Where one
// of the windows is as it was, the old advice still holds for that kind of
// row, and the reply says which.
func TestACleanupUnderWayAsABackupLengthensAWindowSaysToImportAgain(t *testing.T) {
	const day = 24 * time.Hour
	for _, tc := range []struct {
		name, config string
		says, omits  []string
	}{
		{"the backup lengthens every window the cleanup cut at",
			`{"key":"retention_s","value":"0"},{"key":"speed_retention_s","value":"0"}`,
			[]string{"the cleanup had started on a shorter retention window than the one in force now",
				"Import this backup again to bring back the ones it deleted."},
			[]string{"raise that window"}},
		{"the backup lengthens one of them",
			`{"key":"retention_s","value":"0"}`,
			[]string{"The latency retention now in force keeps every row of that kind this backup holds, so importing " +
				"this backup again brings those back.", "To keep the speed rows, raise that window in the Data tab first."},
			nil},
		{"the backup lengthens none",
			`{"key":"latency_interval_s","value":"10"}`,
			[]string{"to keep those, raise that window in the Data tab and import this backup again."},
			[]string{"now in force"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := batchedServer(t, 0)
			setRetention(t, s, 30*day, 30*day, 30*day)
			finishPass := cleanupUnderWay(t, s, liveWindows(s))

			send, finish := pipedImport(t, s, "latency=1&speed=1&config=1")
			send(`{"pingularity_export":2,"latency":[` + oldLatencyRow(40*day) + `,` + oldLatencyRow(41*day) + `],` +
				fmt.Sprintf(`"speed":[{"ts":%d,"server":"s"}],"config":[`, time.Now().Add(-40*day).Unix()))
			storedRows(t, st, "samples", 2)
			storedRows(t, st, "speed", 1)
			if removed := finishPass(); removed != 3 {
				t.Fatalf("fixture: the cleanup that was under way removed %d rows, want the 3 restored so far", removed)
			}
			send(tc.config + `]}`)
			rr := finish()
			if rr.Code != http.StatusOK {
				t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			w := cleanupRan(t, rr)
			if !strings.HasPrefix(w, "The cleanup ran while this backup was being restored and deleted 2 latency rows and 1 speed row. ") {
				t.Fatalf("the reply does not say what the cleanup deleted during the restore (warnings: %q)", warningsOf(t, rr))
			}
			for _, want := range tc.says {
				if !strings.Contains(w, want) {
					t.Errorf("the notice does not say %q:\n%s", want, w)
				}
			}
			for _, not := range tc.omits {
				if strings.Contains(w, not) {
					t.Errorf("the notice says %q, which is not true of the windows now in force:\n%s", not, w)
				}
			}
		})
	}
}
