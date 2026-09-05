package main

// Regression tests for the documented data-usage claim about the row a FAILED
// run leaves behind (docs/metrics.md, the pingularity_speed_data_used_bytes
// bullet - it lived in README.md until the metrics inventory was split out).
//
// History, because it is the point: recordFailedUsage used to write an UNMARKED
// row - byte counts set, every measurement left at its zero value. Since
// SpeedSample.DownMbps/UpMbps/PingMS carry no omitempty, that row was served as
// 0/0/0, and the dashboard's measured-signal is BYTE PRESENCE rather than a
// null speed (spMeasured in internal/web/ui/index.html), which a usage row
// satisfies for whichever direction moved bytes. A failed run therefore read as
// a real 0.0 Mbps measurement. The repair added store.SpeedSample.Failed and
// filters it out of every MEASUREMENT query (speedNotFailed) while the
// data-usage sums keep counting its bytes.
//
// These tests assert the repair AT THE SURFACE, through the HTTP API the
// dashboard actually reads - the store's own tests prove the filter is complete
// across every query, but they never cross into JSON serialization, and the
// original bug was only visible there. TestREADMEUsageBulletMatchesTheRowTheCodeServes
// then holds the README to whichever row the API really serves, so the sentence
// cannot go stale in either direction.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/web"
)

const (
	uiPath = "internal/web/ui/index.html"
	// The sentence the data-usage bullet's failed-run claim hangs off.
	usageBulletMarker = "still contributes the bytes its"
	// What each row in the fixture moved, distinct so a sum identifies its parts.
	usageRowBytes = int64(37_000_000)
	realRunBytes  = int64(11_000_000)
)

// speedFixture is the API under test plus the timestamps of the two rows behind
// it, so an assertion can name a row without hardcoding a date.
type speedFixture struct {
	h               http.Handler
	usageTS, realTS int64
}

// speedAPI serves one store holding exactly two rows through the real HTTP
// handler: the accounting row recordFailedUsage writes for a run that died
// after moving download bytes (scheduler.go:695 - Failed set, byte counts set,
// every measurement left at its zero value), and one ordinary measured run to
// prove the filter removes the marked row rather than emptying the table. Both
// are dated minutes ago so every data-usage window covers them.
func speedAPI(t *testing.T) speedFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	now := time.Now().Unix()
	fx := speedFixture{usageTS: now - 60, realTS: now - 120}
	moved, real := usageRowBytes, realRunBytes
	if err := st.InsertSpeed(ctx, store.SpeedSample{
		TS: fx.usageTS, Server: "Sponsor, City", Trigger: "scheduled", Engine: "ookla",
		Failed:    true,
		DownBytes: &moved, // UpBytes stays nil: that direction never ran
	}); err != nil {
		t.Fatalf("insert usage row: %v", err)
	}
	if err := st.InsertSpeed(ctx, store.SpeedSample{
		TS: fx.realTS, Server: "Sponsor, City", Trigger: "scheduled", Engine: "ookla",
		DownMbps: 94.5, UpMbps: 12.5, PingMS: 8.5, DownBytes: &real,
	}); err != nil {
		t.Fatalf("insert measured run: %v", err)
	}

	set, err := settings.New(ctx, st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, AccessLocalOnly: true,
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	fx.h = web.New(st, nil, nil, set, nil, "test", slog.New(slog.DiscardHandler)).Handler()
	return fx
}

// getJSON drives one API request from loopback (local-only access is the
// default) and returns the decoded body.
func getJSON(t *testing.T, h http.Handler, path string, into any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	// Loopback peer AND a loopback Host: local-only access is the default, and
	// the DNS-rebinding guard refuses httptest's stock example.com Host.
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "127.0.0.1:9000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("GET %s: status %d, body %s", path, rec.Code, body)
	}
	if err := json.NewDecoder(rec.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// servedRuns is what /api/speed/runs hands the dashboard.
func servedRuns(t *testing.T, fx speedFixture) []map[string]json.RawMessage {
	t.Helper()
	var page struct {
		Runs  []map[string]json.RawMessage `json:"runs"`
		Total int                          `json:"total"`
	}
	getJSON(t, fx.h, "/api/speed/runs?limit=50", &page)
	return page.Runs
}

// usageRowIsInvisibleToMeasurements reports the state the README must describe:
// the accounting row is filtered out of the runs the API serves, while its
// bytes still land in the data-usage totals.
func usageRowIsInvisibleToMeasurements(t *testing.T, fx speedFixture) bool {
	t.Helper()
	for _, r := range servedRuns(t, fx) {
		if string(r["ts"]) == strconv.FormatInt(fx.usageTS, 10) {
			return false
		}
	}
	var usage struct {
		All int64 `json:"all"`
	}
	getJSON(t, fx.h, "/api/speed/usage", &usage)
	return usage.All == usageRowBytes+realRunBytes
}

func TestFailedUsageRowIsFilteredOutOfTheAPIButStillCounted(t *testing.T) {
	fx := speedAPI(t)

	runs := servedRuns(t, fx)
	if len(runs) != 1 {
		t.Fatalf("/api/speed/runs served %d runs, want 1 (the measured run only; the accounting row must be filtered)", len(runs))
	}
	if got, want := string(runs[0]["ts"]), strconv.FormatInt(fx.realTS, 10); got != want {
		t.Fatalf("served run ts = %s, want %s (the measured run) - the accounting row reached the dashboard", got, want)
	}
	// The original defect, asserted at the surface it appeared on: the served
	// row must not be a zero speed sitting next to a byte count, which is what
	// the UI's measured-signal reads as a real 0.0 reading.
	if string(runs[0]["down_mbps"]) == "0" {
		t.Error("served run carries down_mbps:0 - a zero speed beside a byte count still reads as a measurement in the UI")
	}

	// Both the retention-wide total and the windows count the failed run's
	// bytes - the README's "totals and windows above count its bytes".
	var usage struct {
		All int64 `json:"all"`
		D30 int64 `json:"30d"`
		H24 int64 `json:"24h"`
	}
	getJSON(t, fx.h, "/api/speed/usage", &usage)
	want := usageRowBytes + realRunBytes
	for _, got := range []struct {
		name string
		v    int64
	}{{"all", usage.All}, {"30d", usage.D30}, {"24h", usage.H24}} {
		if got.v != want {
			t.Errorf("/api/speed/usage %s = %d, want %d (the failed run's bytes landed on the user's bill and must still be counted)",
				got.name, got.v, want)
		}
	}

	// A byte count still makes a zero speed read as a measurement in the UI,
	// which is WHY the row had to be filtered rather than given null speeds (see
	// the schema comment at store.go's failed column). The gate also accepts a
	// positive speed with no byte count at all - an old row that measured before
	// those columns existed - but that arm cannot rescue an accounting row,
	// whose speeds are exactly zero.
	ui, err := os.ReadFile(uiPath)
	if err != nil {
		t.Fatalf("read %s: %v", uiPath, err)
	}
	if !strings.Contains(string(ui), `if(key==='down_mbps') return isNum(p[key]) && (p.download_bytes!=null || p[key]>0);`) {
		t.Errorf("%s: spMeasured's byte-presence gate changed - recheck the README bullet and this test's premise", uiPath)
	}
}

func TestREADMEUsageBulletMatchesTheRowTheCodeServes(t *testing.T) {
	// The data-usage bullet moved to docs/metrics.md when the metrics inventory
	// was split out of the README; the claim it makes is unchanged.
	doc, err := os.ReadFile(filepath.Join("docs", "metrics.md"))
	if err != nil {
		t.Fatalf("read docs/metrics.md: %v", err)
	}
	bullet := bulletContaining(t, string(doc), usageBulletMarker)

	if usageRowIsInvisibleToMeasurements(t, speedAPI(t)) {
		// The row exists for accounting and is filtered out of everything that
		// reads as a measurement. The bullet must not describe the old wart.
		for _, forbidden := range []string{"0.0", "pulls the speed chart", "wart"} {
			if strings.Contains(bullet, forbidden) {
				t.Errorf("docs/metrics.md data-usage bullet still describes the pre-repair wart (%q), but the accounting row no longer reaches /api/speed/runs", forbidden)
			}
		}
		if !strings.Contains(bullet, "flagged") {
			t.Error("docs/metrics.md data-usage bullet does not say the accounting row is flagged (and so kept out of the measurement views)")
		}
		return
	}
	// The other branch: the row is reaching measurement surfaces again. Then
	// the reader has to be warned, in the words the wart earned.
	if !strings.Contains(bullet, "0.0") {
		t.Error("the accounting row is reaching /api/speed/runs (or its bytes stopped counting) - docs/metrics.md must warn that its zeros read as a 0.0 measurement")
	}
}

// bulletContaining returns the single top-level "- " list item of doc that
// holds marker, so an assertion about one claim can't accidentally be satisfied
// by wording elsewhere in the file.
func bulletContaining(t *testing.T, doc, marker string) string {
	t.Helper()
	at := strings.Index(doc, marker)
	if at < 0 {
		t.Fatalf("docs/metrics.md no longer contains %q - the data-usage bullet was reworded; recheck this test's claim", marker)
	}
	start := strings.LastIndex(doc[:at], "\n- ")
	if start < 0 {
		t.Fatalf("no list item starts before %q", marker)
	}
	rest := doc[start+1:]
	if end := strings.Index(rest[1:], "\n- "); end >= 0 {
		rest = rest[:end+1]
	}
	return rest
}
