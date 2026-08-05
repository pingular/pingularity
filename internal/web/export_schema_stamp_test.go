package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		// The speed category carries speed_servers (the v3 key) since the
		// selection reports landed, so any speed export stamps exactly 3 -
		// a literal, not exportSchema, which moved on to 4 for the quarantine
		// key that speed files never carry.
		{"speed=1", 3, "speed_servers"},
		{"config=1&latency=1&speed=1", 3, "speed_servers"},
		// pauses is the v2 key; without a v3 key in the file these stamp
		// exactly 2 - the version the FILE needs, not the newest this build
		// knows (that distinction is this test's whole point).
		{"downtime=1", 2, "pauses"},
		{"config=1&downtime=1", 2, "pauses"},
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
		// The version must track the deciding key, both ways: absent -> the
		// older stamp so older readers keep their downgrade path, present ->
		// past what they accept.
		if tc.carries != "" {
			var has bool
			for _, c := range env.Categories {
				if c == tc.carries {
					has = true
				}
			}
			if !has {
				t.Fatalf("%s: fixture drift: %s not in categories %v", tc.query, tc.carries, env.Categories)
			}
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

// The quarantine key is content-dependent, and the schema stamp follows the
// content: a box with nothing in quarantine writes a downtime file WITHOUT the
// key (stamped 2, restorable on every pauses-aware build), while a box holding
// quarantined history writes the key and stamps 4 - so a pre-quarantine build
// refuses the file loudly instead of silently shedding the held rows on
// restore.
func TestQuarantineKeyIsContentDependent(t *testing.T) {
	export := func(t *testing.T, s *Server) (int, []string, string) {
		t.Helper()
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/export?downtime=1", nil)
		r.Host = "127.0.0.1:9000"
		s.Handler().ServeHTTP(rr, r)
		if rr.Code != 200 {
			t.Fatalf("export HTTP %d: %s", rr.Code, rr.Body.String())
		}
		var env struct {
			Version    int      `json:"pingularity_export"`
			Categories []string `json:"categories"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode export: %v", err)
		}
		return env.Version, env.Categories, rr.Body.String()
	}
	has := func(cats []string, k string) bool {
		for _, c := range cats {
			if c == k {
				return true
			}
		}
		return false
	}

	s := newTestServer(t)
	ver, cats, body := export(t, s)
	if ver != 2 || has(cats, "pauses_quarantine") || strings.Contains(body, `"pauses_quarantine"`) {
		t.Fatalf("empty quarantine: version=%d cats=%v; want 2 with no quarantine key - "+
			"stamping higher locks pre-quarantine builds out of a file they fully understand", ver, cats)
	}

	// Hold one row (a far-future span nothing exonerates) and the same box's
	// next backup must carry the key and demand the version that protects it.
	if _, err := s.store.ImportTable(context.Background(),
		"pauses_quarantine", []map[string]any{{"ts": time.Now().Unix(), "duration_s": int64(9 * 365 * 24 * 3600)}}); err != nil {
		t.Fatalf("seed quarantine: %v", err)
	}
	ver, cats, body = export(t, s)
	if ver != 4 || !has(cats, "pauses_quarantine") || !strings.Contains(body, `"pauses_quarantine"`) {
		t.Fatalf("held row present: version=%d cats=%v; want 4 with the quarantine key - "+
			"anything lower lets a pre-quarantine build silently shed held history on restore", ver, cats)
	}

	// A FULL export must stamp the maximum over every key it carries. This is
	// the pin that keeps exportSchemaFor a max: its pre-fix shape returned 3
	// the moment it saw speed_servers - which comes before pauses_quarantine
	// in dataCategories order - so a full backup carrying held rows stamped 3
	// and a v3-capable pre-quarantine build silently shed them on restore.
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/export?config=1&latency=1&speed=1&downtime=1", nil)
	r.Host = "127.0.0.1:9000"
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != 200 {
		t.Fatalf("full export HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Version    int      `json:"pingularity_export"`
		Categories []string `json:"categories"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode full export: %v", err)
	}
	if env.Version != 4 || !has(env.Categories, "pauses_quarantine") || !has(env.Categories, "speed_servers") {
		t.Fatalf("full export with held rows: version=%d cats=%v; want 4 carrying both the v3 and v4 keys", env.Version, env.Categories)
	}
}
