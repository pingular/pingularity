package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The export envelope's version number is the ONLY thing that stops an older
// build from restoring a file it cannot fully understand. This branch added a
// `pauses` category, and pause rows are the uptime denominator: a build that
// silently drops them restores a history where every unobserved second becomes
// observed-and-up, so the restored box reports a different uptime% than the one
// it was copied from - and reports one for windows the source correctly omitted.
//
// v0.54.0 accepts version 1 and skips keys it does not know, so as long as we
// keep writing 1 that downgrade path is silent data corruption that reports
// success. It already refuses anything above 1, so the version bump is what makes
// it fail loudly instead.

// lastSchemaWithoutPauses is the newest envelope version a build that has never
// heard of pause rows will accept. v0.54.0 writes and accepts exactly 1.
const lastSchemaWithoutPauses = 1

// The invariant, stated so it cannot be satisfied by moving the test: as long as
// an export CARRIES pauses, its declared version must be past the last one an
// unaware build accepts. Binding the expectation to exportSchema instead would
// make this tautological - it would follow the constant anywhere, including back
// to 1.
func TestExportDeclaresTheVersionThatIncludesPauses(t *testing.T) {
	var hasPauses bool
	for _, dc := range dataCategories {
		if dc.key == "pauses" {
			hasPauses = true
		}
	}
	if !hasPauses {
		t.Skip("exports no longer carry pauses; the downgrade hazard this guards is gone")
	}

	s := newTestServer(t)
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/export?downtime=1", nil)
	r.Host = "127.0.0.1:9000"
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Version    int      `json:"pingularity_export"`
		Categories []string `json:"categories"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if env.Version <= lastSchemaWithoutPauses {
		t.Errorf("export carries pauses but declares pingularity_export=%d; a build that predates pauses accepts anything <= %d "+
			"and walks straight past the key, restoring the outages without the observation spans and reporting success",
			env.Version, lastSchemaWithoutPauses)
	}
	// The envelope must stamp the version PAUSES needs (2) - and since v3
	// exists it must specifically not inflate a pauses-only file to the newest
	// constant, or rolled-back v2-readers lose a backup they fully understand.
	if env.Version != 2 {
		t.Errorf("envelope says %d for a pauses-only export; want exactly 2 (the version the file needs)", env.Version)
	}
	if env.Version > exportSchema {
		t.Errorf("envelope says %d but this build's exportSchema is only %d", env.Version, exportSchema)
	}
	var sawPauses bool
	for _, c := range env.Categories {
		if c == "pauses" {
			sawPauses = true
		}
	}
	if !sawPauses {
		t.Errorf("manifest categories %v omit pauses, so the version bump guards nothing", env.Categories)
	}
}

// A backup written before pauses existed must still restore - the bump must not
// orphan every file already on disk.
func TestImportStillAcceptsTheOlderVersion(t *testing.T) {
	for _, ver := range []int{1, exportSchema} {
		s := newTestServer(t)
		body := `{"pingularity_export":` + itoa(ver) + `,"producer_version":"test","exported_at":1,` +
			`"categories":["downtime"],"downtime":[{"ts":1000,"type":"down","duration_s":60}]}`
		rr := postImport(t, s, body)
		if rr.Code != http.StatusOK {
			t.Fatalf("version %d: HTTP %d: %s", ver, rr.Code, strings.TrimSpace(rr.Body.String()))
		}
		// A 200 alone proves nothing: import silently skips every category that is
		// not also named in the query string, so a request without one "succeeds"
		// having applied no rows at all. Assert the row actually landed.
		var got struct {
			Downtime int `json:"downtime"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("version %d: decode result: %v", ver, err)
		}
		if got.Downtime != 1 {
			t.Errorf("version %d: imported %d downtime rows, want 1 (body %s)",
				ver, got.Downtime, strings.TrimSpace(rr.Body.String()))
		}
	}
}

// Anything newer than we understand must still be refused, or the guard that
// makes this whole scheme work is gone.
func TestImportRefusesAFutureVersion(t *testing.T) {
	s := newTestServer(t)
	body := `{"pingularity_export":` + itoa(exportSchema+1) + `,"categories":["downtime"],"downtime":[]}`
	rr := postImport(t, s, body)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("a newer-than-ours export got HTTP %d, want 400", rr.Code)
	}
}

func postImport(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	// ?downtime=1 selects the category: import walks past any category the query
	// string does not name, so omitting it makes the whole request a no-op that
	// still returns 200.
	r := httptest.NewRequest("POST", "/api/import?downtime=1", strings.NewReader(body))
	r.Host = "127.0.0.1:9000"
	r.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rr, r)
	_, _ = io.Copy(io.Discard, r.Body)
	return rr
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
