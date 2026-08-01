package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// The schema constant went to 2 for exactly one reason: the pauses key, which a
// v1 reader silently drops. A file that carries no pauses key is v1-shaped in
// every byte, yet stamping the constant unconditionally marks it 2 - and
// v0.54.0 rejects anything above 1 outright ("update before restoring it"),
// so a settings-only backup taken here cannot restore on a rolled-back build
// even though that build understands all of it. The stamp must be the version
// the FILE needs, not the newest this build knows.
func TestExportStampsTheVersionTheFileNeeds(t *testing.T) {
	cases := []struct {
		query   string
		want    int
		carries string // key whose presence/absence decides the version
	}{
		{"config=1", 1, ""},
		{"latency=1", 1, ""},
		{"speed=1", 1, ""},
		{"config=1&latency=1&speed=1", 1, ""},
		{"downtime=1", exportSchema, "pauses"},
		{"config=1&downtime=1", exportSchema, "pauses"},
	}
	for _, tc := range cases {
		s := newTestServer(t)
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/export?"+tc.query, nil)
		r.Host = "127.0.0.1:9000"
		s.Handler().ServeHTTP(rr, r)
		if rr.Code != 200 {
			t.Fatalf("%s: export HTTP %d: %s", tc.query, rr.Code, rr.Body.String())
		}
		var env struct {
			Version    int      `json:"pingularity_export"`
			Categories []string `json:"categories"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: decode export: %v", tc.query, err)
		}
		// The version must track the v2-only key, both ways: absent -> 1 so v1
		// readers keep their downgrade path, present -> past what they accept.
		var hasPauses bool
		for _, c := range env.Categories {
			if c == "pauses" {
				hasPauses = true
			}
		}
		if hasPauses != (tc.carries == "pauses") {
			t.Fatalf("%s: fixture drift: pauses in categories = %v", tc.query, hasPauses)
		}
		if env.Version != tc.want {
			t.Errorf("%s: pingularity_export=%d, want %d (categories %v): a file without v2-only data "+
				"stamped 2 is refused by v0.54.0 though every byte of it is v1-shaped; one WITH pauses "+
				"stamped lower would be silently half-restored", tc.query, env.Version, tc.want, env.Categories)
		}
		if !strings.Contains(rr.Body.String(), `"pingularity_export":`) {
			t.Fatalf("%s: envelope missing version field", tc.query)
		}
	}
}
