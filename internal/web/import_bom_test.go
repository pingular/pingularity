package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The client streams the export File straight to /api/import (load-bearing for
// large restores), so the handler decodes the body as a stream. A leading UTF-8
// BOM (0xEF 0xBB 0xBF) is legal at the start of a UTF-8 file and some
// exporters/editors prepend one, but json.Decoder.Token rejects it - which the
// handler reported as the misleading "not a Pingularity export file" 400. The
// handler must strip a single leading BOM before decoding.
func TestImportStripsLeadingUTF8BOM(t *testing.T) {
	s := newTestServer(t)
	const valid = `{"pingularity_export":2,"categories":["config"],"config":[{"key":"speedtest_enabled","value":"1"}]}`
	const bom = "\ufeff" // U+FEFF -> the three BOM bytes 0xEF 0xBB 0xBF in UTF-8

	post := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/import?config=1", strings.NewReader(body))
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = "127.0.0.1:54321"
		r.Header.Set("Content-Type", "application/json")
		s.Handler().ServeHTTP(rr, r)
		return rr
	}

	if rr := post(bom + valid); rr.Code != http.StatusOK {
		t.Fatalf("a BOM-prefixed valid export was rejected: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	// A normal export (no BOM) must still import - the strip must not eat real bytes.
	if rr := post(valid); rr.Code != http.StatusOK {
		t.Fatalf("a normal export was rejected: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
}
