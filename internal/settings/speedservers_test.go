package settings

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pingular/pingularity/internal/store"
)

// idsOf lists the saved server IDs so a failure message names the survivors.
func idsOf(ss []SavedServer) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

// The key name and the serialized shape are the on-disk contract: rename either
// and every already-saved list is orphaned, read back as empty, with no error to
// say so. Pinned as literals, because a test written against the constants would
// follow a rename right past the databases that cannot.
func TestSpeedServersStorageContract(t *testing.T) {
	if keySpeedServers != "speed_servers" {
		t.Errorf("settings key = %q; already-saved lists are stored under %q", keySpeedServers, "speed_servers")
	}
	got := speedServersJSON([]SavedServer{
		{ID: "1234", Sponsor: "Bell", Name: "Montreal, QC", Country: "Canada", Lat: 45.5, Lon: -73.5},
		{ID: "5678"}, // no sponsor, name, country, or coordinate
	})
	want := `[{"id":"1234","sponsor":"Bell","name":"Montreal, QC","country":"Canada","lat":45.5,"lon":-73.5},{"id":"5678"}]`
	if got != want {
		t.Errorf("stored JSON shape changed\n want %s\n got  %s", want, got)
	}
	// "[]", not "null": an empty list must read back as an empty list.
	if got := speedServersJSON(nil); got != "[]" {
		t.Errorf("empty list serialized as %q, want %q", got, "[]")
	}
	// json.Marshal refuses a non-finite float and returns no bytes with its error.
	// sanitizeSpeedServers rules that out, but a caller that skipped it must still
	// not store an empty value where a JSON array belongs.
	if got := speedServersJSON([]SavedServer{{ID: "1", Lat: math.NaN()}}); !json.Valid([]byte(got)) {
		t.Errorf("a non-finite coordinate serialized to %q, which is not JSON", got)
	}
}

// THE MIGRATION PROOF. A database written by an older build carries no
// speed_servers row at all. Loading it must yield an empty list, must not error,
// and must not WRITE the key back: a settings row that appears on mere read
// counts as operator configuration (hasPriorConfiguration), which would make a
// never-configured install read as established.
func TestLoadWithoutSpeedServersKey(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// A settings row set shaped like the release before this key existed.
	seed := map[string]string{
		keyLatency:      "5",
		keySpeedServer:  "1234",
		keySpeedEngine:  "ookla",
		keySpeedBestOf:  "1",
		keyIperfServers: `[{"label":"NAS","addr":"10.0.0.5:5201"}]`,
	}
	if _, err := st.SetSettingsDiff(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c, err := New(ctx, st, Values{Latency: 5e9, Speed: 3600e9, Timeout: 2e9, DownAfter: 3, UpAfter: 2})
	if err != nil {
		t.Fatalf("New on a pre-key database: %v", err)
	}
	if got := c.Snapshot().SpeedServers; len(got) != 0 {
		t.Errorf("absent key must load as an empty list, got %+v", got)
	}
	if got := c.SpeedServers(); len(got) != 0 {
		t.Errorf("accessor must report an empty list, got %+v", got)
	}

	after, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if raw, ok := after[keySpeedServers]; ok {
		t.Errorf("loading wrote %q (%q); a read must not persist the key", keySpeedServers, raw)
	}
	// Nothing else may move either - the load is a read.
	if !reflect.DeepEqual(after, seed) {
		t.Errorf("loading rewrote the settings rows\n want %v\n got  %v", seed, after)
	}

	// Reload takes the same path and must reach the same answer.
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := c.Snapshot().SpeedServers; len(got) != 0 {
		t.Errorf("Reload on a pre-key database gave %+v, want empty", got)
	}
}

// Stored JSON is untrusted: a hand-edited row or a crafted import can hold
// anything. Every shape below must load without panicking and leave a list the
// rest of the code can use.
func TestOverlaySpeedServersMalformedJSON(t *testing.T) {
	huge := make([]SavedServer, 0, maxSpeedServers+50)
	for i := 0; i < maxSpeedServers+50; i++ {
		huge = append(huge, SavedServer{ID: strconv.Itoa(i)})
	}
	hugeRaw, err := json.Marshal(huge)
	if err != nil {
		t.Fatalf("marshal huge: %v", err)
	}

	for name, raw := range map[string]string{
		"empty":            "",
		"null":             "null",
		"object":           `{"id":"1"}`,
		"numbers":          `[1,2,3]`,
		"strings":          `["1234"]`,
		"truncated":        `[{"id":"1"`,
		"id not a string":  `[{"id":1234}]`,
		"coord as string":  `[{"id":"1","lat":"45.5","lon":"-73.5"}]`,
		"coord out of map": `[{"id":"1","lat":1e9,"lon":-4e9}]`,
		"nested garbage":   `[{"id":"1","sponsor":{"deep":[1,2]}}]`,
		"huge array":       string(hugeRaw),
	} {
		t.Run(name, func(t *testing.T) {
			got := normalize(overlay(Values{}, map[string]string{keySpeedServers: raw})).SpeedServers
			if len(got) > maxSpeedServers {
				t.Errorf("loaded %d servers, cap is %d", len(got), maxSpeedServers)
			}
			for _, s := range got {
				if s.ID == "" {
					t.Errorf("loaded an entry with no ID: %+v", got)
				}
				if !validCoord(s.Lat, s.Lon) {
					t.Errorf("loaded an unusable coordinate %v,%v", s.Lat, s.Lon)
				}
			}
			// Whatever survived must be storable again; json.Marshal refuses a
			// non-finite float, which would persist the list as an empty value.
			if js := speedServersJSON(got); !json.Valid([]byte(js)) {
				t.Errorf("reserializing gave invalid JSON %q", js)
			}
		})
	}
}

// Go's decoder fills the entries it read before it hits a bad one, so a stored
// list with one broken entry would otherwise load half-present and then be saved
// back that way, losing the rest. A list that does not decode whole is not used
// at all; the running list stands.
// A blob that fails to decode is discarded whole. Both production callers hand
// overlay the defaults, so the outcome is the empty default rather than anything
// preserved - what this pins is that a partly-decoded list is never adopted.
func TestOverlaySpeedServersDiscardsAPartlyDecodedList(t *testing.T) {
	cur := []SavedServer{{ID: "1234", Sponsor: "Bell"}}
	raw := `[{"id":"5678","sponsor":"Rogers"},{"id":"9999","lat":"not a number"}]`

	got := overlay(Values{SpeedServers: cur}, map[string]string{keySpeedServers: raw}).SpeedServers
	if !reflect.DeepEqual(got, cur) {
		t.Errorf("a half-decoded list replaced the running one\n want %+v\n got  %+v", cur, got)
	}
}

// sanitizeSpeedServers is the one gate between stored JSON and everything that
// reads the list: trim, drop entries with no ID, dedupe by ID keeping the first,
// and cap the length.
func TestSanitizeSpeedServers(t *testing.T) {
	out := sanitizeSpeedServers([]SavedServer{
		{ID: "  1234  ", Sponsor: "  Bell  ", Name: " Montreal, QC ", Lat: 45.5, Lon: -73.5},
		{ID: "   "},                        // whitespace only - no ID
		{ID: "", Sponsor: "no id"},         // no ID
		{ID: "1234", Sponsor: "duplicate"}, // same ID, arrives later
		{ID: "5678", Name: "Toronto"},
	})
	if len(out) != 2 {
		t.Fatalf("got %d servers %q, want 2", len(out), idsOf(out))
	}
	want := SavedServer{ID: "1234", Sponsor: "Bell", Name: "Montreal, QC", Lat: 45.5, Lon: -73.5}
	if out[0] != want {
		t.Errorf("first entry not trimmed/kept intact\n want %+v\n got  %+v", want, out[0])
	}
	if out[1].ID != "5678" {
		t.Errorf("second entry = %+v, want ID 5678", out[1])
	}

	// The cap bounds what a crafted import can store.
	var many []SavedServer
	for i := 0; i < maxSpeedServers+5; i++ {
		many = append(many, SavedServer{ID: strconv.Itoa(i)})
	}
	if n := len(sanitizeSpeedServers(many)); n != maxSpeedServers {
		t.Errorf("cap: got %d, want %d", n, maxSpeedServers)
	}

	// Dedupe runs on the CAPPED ID, or two IDs that store the same value both survive.
	prefix := strings.Repeat("9", maxServerID)
	dup := sanitizeSpeedServers([]SavedServer{
		{ID: prefix + "1", Sponsor: "one"},
		{ID: prefix + "2", Sponsor: "two"},
	})
	if len(dup) != 1 || dup[0].Sponsor != "one" {
		t.Errorf("IDs colliding at the cap must dedupe to the first, got %+v", dup)
	}

	// Free-form text is length-capped and never cut mid-rune.
	// The cut has to fall INSIDE a rune or a naive byte slice passes: any string of
	// one repeated rune lands on a boundary, because maxLabelLen divides by 2, 3
	// and 4. 119 single-byte runes put the 120th byte in the middle of the next.
	multi := strings.Repeat("a", maxLabelLen-1) + strings.Repeat("é", 50)
	long := sanitizeSpeedServers([]SavedServer{{ID: "1", Sponsor: multi, Name: multi}})
	if len(long) != 1 || len(long[0].Sponsor) > maxLabelLen || len(long[0].Name) > maxLabelLen {
		t.Errorf("sponsor/name not capped at %d: %+v", maxLabelLen, long)
	}
	// Checked on the string, not on the JSON: Marshal replaces invalid UTF-8 with
	// U+FFFD and still emits a valid document, so json.Valid can never see a split
	// rune.
	if !utf8.ValidString(long[0].Sponsor) || !utf8.ValidString(long[0].Name) {
		t.Errorf("capping split a rune: sponsor=%q name=%q", long[0].Sponsor, long[0].Name)
	}
}

// A coordinate is kept only when it can centre a later run. NaN/Inf also break
// json.Marshal outright, which would store the whole list as an empty value.
func TestSanitizeSpeedServersCoordinates(t *testing.T) {
	for name, in := range map[string][2]float64{
		"NaN lat":       {math.NaN(), -73.5},
		"NaN lon":       {45.5, math.NaN()},
		"+Inf":          {math.Inf(1), math.Inf(1)},
		"-Inf":          {math.Inf(-1), 0},
		"lat too big":   {90.1, 0},
		"lon too small": {0, -180.1},
		"lon too big":   {0, 180.1},
	} {
		t.Run(name, func(t *testing.T) {
			got := sanitizeSpeedServers([]SavedServer{{ID: "1", Lat: in[0], Lon: in[1]}})
			if len(got) != 1 {
				t.Fatalf("entry dropped: %+v", got)
			}
			if got[0].Lat != 0 || got[0].Lon != 0 {
				t.Errorf("unusable coordinate kept: %v,%v", got[0].Lat, got[0].Lon)
			}
			if js := speedServersJSON(got); !json.Valid([]byte(js)) {
				t.Errorf("stored value is not JSON: %q", js)
			}
		})
	}
	// The extremes are real places and must survive.
	edge := sanitizeSpeedServers([]SavedServer{{ID: "1", Lat: -90, Lon: 180}})
	if len(edge) != 1 || edge[0].Lat != -90 || edge[0].Lon != 180 {
		t.Errorf("in-range extremes must survive, got %+v", edge)
	}
}

// The list has to survive the settings map in both directions, coordinates
// included - a value that serializes but does not read back is lost at restart.
func TestSpeedServersRoundTripThroughSettingsMap(t *testing.T) {
	want := []SavedServer{
		{ID: "1234", Sponsor: "Bell", Name: "Montreal, QC", Lat: 45.5, Lon: -73.5},
		{ID: "5678", Sponsor: "Rogers", Name: "Toronto, ON", Lat: 43.65, Lon: -79.38},
	}
	got := overlay(Values{}, formKeys(Values{SpeedServers: want})).SpeedServers
	if !reflect.DeepEqual(want, got) {
		t.Errorf("formKeys->overlay lost the list\n want %+v\n got  %+v", want, got)
	}
}

// Update is a PATCH: a nil list keeps what is stored, an explicit empty one
// clears it, and either way the result must survive a restart.
func TestUpdateSpeedServers(t *testing.T) {
	c := newController(t)
	ctx := context.Background()
	saved := []SavedServer{
		{ID: "1234", Sponsor: "Bell", Name: "Montreal, QC", Lat: 45.5, Lon: -73.5},
		{ID: "5678", Sponsor: "Rogers", Name: "Toronto, ON"},
	}
	v, err := c.Update(ctx, Patch{SpeedServers: saved, SpeedServerID: pv("1234")})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !reflect.DeepEqual(v.SpeedServers, saved) {
		t.Fatalf("saved list = %+v, want %+v", v.SpeedServers, saved)
	}

	// An unrelated save must not disturb the list, and the selection stays put.
	if _, err := c.Update(ctx, Patch{SpeedtestEnabled: pv(true)}); err != nil {
		t.Fatalf("partial Update: %v", err)
	}
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := c.Snapshot(); !reflect.DeepEqual(got.SpeedServers, saved) || got.SpeedServerID != "1234" {
		t.Errorf("list or selection lost across a restart: %+v / %q", got.SpeedServers, got.SpeedServerID)
	}

	if _, err := c.Update(ctx, Patch{SpeedServers: []SavedServer{}}); err != nil {
		t.Fatalf("clear Update: %v", err)
	}
	if got := c.Snapshot().SpeedServers; len(got) != 0 {
		t.Errorf("explicit empty list must clear, got %+v", got)
	}
	if got := c.Snapshot().SpeedServerID; got != "1234" {
		t.Errorf("clearing the list must not touch the selection, got %q", got)
	}
}

// The accessor hands out a copy: get() returns the controller's Values by value,
// so the slice's backing array is shared with the scheduler's run goroutine (the
// Ookla tester's SavedCoordFn reads it during a pinned best-of run's selection
// phase), and a caller that edits what it got would race it.
func TestSpeedServersAccessorCopies(t *testing.T) {
	c := newController(t)
	if _, err := c.Update(context.Background(), Patch{SpeedServers: []SavedServer{{ID: "1234", Sponsor: "Bell"}}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := c.SpeedServers()
	if len(got) != 1 {
		t.Fatalf("got %d servers, want 1", len(got))
	}
	got[0].Sponsor = "clobbered"
	if again := c.SpeedServers(); again[0].Sponsor != "Bell" {
		t.Errorf("mutating the returned slice changed the controller's: %+v", again)
	}

	snap := c.Snapshot().SpeedServers
	snap[0].Sponsor = "clobbered"
	if again := c.Snapshot().SpeedServers; again[0].Sponsor != "Bell" {
		t.Errorf("mutating Snapshot's slice changed the controller's: %+v", again)
	}
}

// The saved list is ordinary configuration, not a credential, so a backup must
// carry it and a restore must apply it. Nothing in SavedServer is secret; this
// also fails if a future field stops surviving the export.
func TestSpeedServersSurviveExportImport(t *testing.T) {
	ctx := context.Background()
	src, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer src.Close()

	saved := []SavedServer{{ID: "1234", Sponsor: "Bell", Name: "Montreal, QC", Country: "Canada", Lat: 45.5, Lon: -73.5}}
	raw := speedServersJSON(saved)
	if _, err := src.SetSettingsDiff(ctx, map[string]string{keySpeedServers: raw}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var rows []map[string]any
	if err := src.ExportTableRows(ctx, "settings", func(m map[string]any) error {
		rows = append(rows, m)
		return nil
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	var exported string
	for _, r := range rows {
		if k, _ := r["key"].(string); k == keySpeedServers {
			exported, _ = r["value"].(string)
		}
	}
	if exported == "" {
		t.Fatalf("%q was dropped by the export denylist; the saved list belongs in a backup", keySpeedServers)
	}
	// Every field must be in the exported blob, or a restore silently loses it.
	rt := reflect.TypeOf(SavedServer{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("SavedServer.%s has no json tag; it cannot round-trip a backup", rt.Field(i).Name)
		}
		if !strings.Contains(exported, `"`+name+`"`) {
			t.Errorf("SavedServer field %q is missing from the export - either the export "+
				"drops it, or this test's fixture never set it: %s", name, exported)
		}
	}

	// Restore into a fresh database and read it back through the real load path.
	dst, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open destination: %v", err)
	}
	defer dst.Close()
	if _, err := dst.ImportTable(ctx, "settings", rows); err != nil {
		t.Fatalf("import: %v", err)
	}
	c, err := New(ctx, dst, Values{Latency: 5e9, Speed: 3600e9, Timeout: 2e9, DownAfter: 3, UpAfter: 2})
	if err != nil {
		t.Fatalf("New on the restored database: %v", err)
	}
	if got := c.Snapshot().SpeedServers; !reflect.DeepEqual(got, saved) {
		t.Errorf("restored list = %+v, want %+v", got, saved)
	}
}
