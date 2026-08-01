package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/config"
)

// The import must accept the product's own exports, whatever their size. The
// old whole-body cap (importBodyCap, 256 MiB) sat BELOW a default install's
// 30-day backup - the samples array alone outgrows it (see the arithmetic test
// below) - so a restore died mid-file, after earlier categories had already
// durably committed, and selecting only a small category did not help because
// every byte still streams through the capped reader. Worse, the failure in
// the skip path surfaced as 400 "invalid JSON" rather than a size message.

// postImportBody runs one import with a streamed body.
func postImportBody(t *testing.T, s *Server, query string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/import?"+query, body)
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rr, r)
	return rr
}

// A file bigger than the old whole-body cap, with the selected category sitting
// AFTER the bulk (as config does in our own exports), must restore. Streamed,
// so the test allocates megabytes rather than the ~270 MB it sends.
func TestImportAcceptsABackupBiggerThanTheOldBodyCap(t *testing.T) {
	s := newTestServer(t)
	chunk := `"` + strings.Repeat("a", 4<<20) + `"`
	parts := []io.Reader{strings.NewReader(`{"pingularity_export":2,"categories":["latency","downtime"],"latency":[`)}
	for i := 0; i < 66; i++ { // 66 x ~4 MiB = ~264 MiB of skipped elements, past the old 256 MiB cap
		if i > 0 {
			parts = append(parts, strings.NewReader(","))
		}
		parts = append(parts, strings.NewReader(chunk))
	}
	parts = append(parts, strings.NewReader(`],"downtime":[{"ts":1000,"type":"down","duration_s":60}]}`))

	rr := postImportBody(t, s, "downtime=1", io.MultiReader(parts...))
	if rr.Code != http.StatusOK {
		t.Fatalf("a backup bigger than the old whole-body cap got HTTP %d: %s - the import rejects the "+
			"product's own exports, mid-file, with earlier categories already committed",
			rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	var got struct {
		Downtime int `json:"downtime"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || got.Downtime != 1 {
		t.Fatalf("the selected category after the big skipped one did not land (err %v, body %s)",
			err, strings.TrimSpace(rr.Body.String()))
	}
}

// One element outgrowing the read allowance is a size problem and must say so:
// 413 naming the limit, not 400 "invalid JSON" - and it is caught whether or
// not its category was selected, since unselected values stream through the
// same reader. The allowance equals the old whole-body cap, so the element
// here is bigger than 256 MiB (streamed from a shared 4 MiB chunk).
func TestImportRefusesASingleOversizedElementWithASizeError(t *testing.T) {
	s := newTestServer(t)
	chunk := strings.Repeat("a", 4<<20)
	parts := []io.Reader{
		strings.NewReader(`{"pingularity_export":2,"categories":["latency","downtime"],"latency":[`),
		strings.NewReader(`"`),
	}
	for i := 0; i < 68; i++ { // 68 x 4 MiB = 272 MiB in ONE string token, past the 256 MiB allowance
		parts = append(parts, strings.NewReader(chunk))
	}
	parts = append(parts,
		strings.NewReader(`"`),
		strings.NewReader(`],"downtime":[{"ts":1000,"type":"down","duration_s":60}]}`))
	rr := postImportBody(t, s, "downtime=1", io.MultiReader(parts...))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a 272 MiB single element got HTTP %d, want 413: %s",
			rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if body := rr.Body.String(); !strings.Contains(body, "MiB") {
		t.Errorf("the 413 does not name the size limit: %s", strings.TrimSpace(body))
	}
}

// The arithmetic the no-whole-body-cap decision rests on, pinned to the config
// defaults it is computed from: a default install (dual-stack targets, 5s
// probe interval, 30-day latency retention) exports a samples array that alone
// outgrows the old 256 MiB cap. Latency values carry float64 precision, as the
// monitor's real measurements do. If defaults ever shrink this below the old
// cap, revisit importReadBurst's comment - and whether a whole-body cap became
// workable.
func TestDefaultExportOutgrowsAnyWorkableBodyCap(t *testing.T) {
	def := config.Default()
	rounds := int64(def.Retention / def.Interval) // retained probe rounds; one sample row per target per round
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	var total int64
	for _, tgt := range def.Targets {
		buf.Reset()
		// One exported sample row, encoded the way handleExport does (enc.Encode of
		// the scanned map, plus the ',' separator).
		if err := enc.Encode(map[string]any{
			"ts": int64(1721968000), "target": tgt.Name, "latency_ms": float64(23456789) / 1e6,
			"success": true, "family": tgt.Family,
		}); err != nil {
			t.Fatal(err)
		}
		total += rounds * int64(buf.Len()+1)
	}
	const oldCap = 256 << 20 // the whole-body cap handleImport used to enforce
	if total <= oldCap {
		t.Errorf("default-config samples array is %d MiB, inside the old %d MiB cap: the arithmetic in "+
			"importReadBurst's comment no longer holds", total>>20, oldCap>>20)
	}
	t.Logf("default-config 30-day samples array ~= %d MiB (dns rides along on top); old cap was %d MiB",
		total>>20, oldCap>>20)
}
