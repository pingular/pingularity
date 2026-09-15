package settings

import (
	"context"
	"testing"

	"github.com/pingular/pingularity/internal/store"
)

// The auto-selection city is the one setting this release removed outright, and
// the rows a pre-picker install wrote are still sitting in its settings table.
// They must stay there - an export carries them, a downgrade honours them
// again, and nothing here is entitled to delete an operator's record of what
// they once asked for - while being reported, once, so the daemon can tell that
// operator their selection is no longer scoped.
//
// File-backed and across a real restart, like the migration tests beside this
// one: the rows are read at load, and a load is the only place this can be
// wrong.

// The on-disk key names are spelled out rather than taken from the constants,
// as the neighbouring tests do: they are the contract an older database carries
// in, and a renamed constant must not quietly re-point the test at another row.
func TestRetiredCityScopeIsReportedAndTheRowsAreLeftAlone(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	b.seed(map[string]string{
		"speed_auto_loc":   "49.2827,-123.1207",
		"speed_auto_label": "Vancouver, BC",
	})
	c := b.boot()
	if got := c.RetiredCityScope(); got != "Vancouver, BC" {
		t.Errorf("RetiredCityScope() = %q, want the place name the operator typed", got)
	}
	// Reading it must not have applied it to anything: the picker's list is the
	// only selection scope this build has, and it is empty.
	if got := c.SpeedServers(); len(got) != 0 {
		t.Errorf("the retired scope was turned into %d saved server(s); it is evidence, not configuration", len(got))
	}
	// An ordinary save is the moment a sparse settings table gets rewritten.
	if _, err := c.Update(ctx, Patch{SpeedDirection: pv("down")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	m := b.settings()
	if m["speed_auto_loc"] != "49.2827,-123.1207" || m["speed_auto_label"] != "Vancouver, BC" {
		t.Errorf("the retired rows read %q / %q after a load and a save; they must be left exactly as the older build wrote them, or a rollback loses the scope",
			m["speed_auto_loc"], m["speed_auto_label"])
	}
	// And again after a restart, from the rows on disk rather than this
	// controller's memory.
	if got := b.boot().RetiredCityScope(); got != "Vancouver, BC" {
		t.Errorf("after a restart RetiredCityScope() = %q, want the stored place name", got)
	}
}

// A scope written through the API need not carry a label - the coordinate is
// the half that steered anything - so the coordinate is what gets quoted back
// when there is no name to quote.
func TestRetiredCityScopeFallsBackToTheCoordinate(t *testing.T) {
	b := newFileBoot(t)
	b.seed(map[string]string{"speed_auto_loc": "49.2827,-123.1207"})
	if got := b.boot().RetiredCityScope(); got != "49.2827,-123.1207" {
		t.Errorf("RetiredCityScope() = %q, want the coordinate: a scope with no label still scoped selection", got)
	}
}

// Every install that never used it, and every one created since, must read
// clean - a notice that fires for an install with nothing to notice teaches
// operators to ignore it. A label with no coordinate is one of those: the label
// was the name shown beside the coordinate, and on its own it centred nothing.
func TestRetiredCityScopeIsEmptyWithoutTheRows(t *testing.T) {
	b := newFileBoot(t)
	if got := b.boot().RetiredCityScope(); got != "" {
		t.Errorf("RetiredCityScope() = %q on an install that never had one, want \"\"", got)
	}
	b2 := newFileBoot(t)
	b2.seed(map[string]string{"speed_auto_label": "Vancouver, BC"})
	if got := b2.boot().RetiredCityScope(); got != "" {
		t.Errorf("RetiredCityScope() = %q for a label with no coordinate, want \"\": nothing was ever centred on it", got)
	}
}

// Whitespace is not a scope. The older build trimmed both values before it
// used them, so a coordinate that is only blanks centred nothing and is nothing
// to announce, a blank label beside a real coordinate is no name to quote, and
// padding around a real name is not part of it. Anything else would announce a
// city nobody chose, or quote one back in a shape the operator never typed.
func TestRetiredCityScopeIgnoresWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows map[string]string
		want string
	}{
		{"blank coordinate", map[string]string{"speed_auto_loc": "   ", "speed_auto_label": "Vancouver, BC"}, ""},
		{"blank label", map[string]string{"speed_auto_loc": " 49.2827,-123.1207 ", "speed_auto_label": " \t "}, "49.2827,-123.1207"},
		{"padded label", map[string]string{"speed_auto_loc": "49.2827,-123.1207", "speed_auto_label": "  Vancouver, BC\n"}, "Vancouver, BC"},
	} {
		b := newFileBoot(t)
		b.seed(tc.rows)
		if got := b.boot().RetiredCityScope(); got != tc.want {
			t.Errorf("%s: RetiredCityScope() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A load that could not read the table still hands back a controller, and the
// daemon keeps booting on it - it retries the load in the background and asks
// that controller for its boot notices before it serves. A scope nobody read is
// no scope, so the accessor has to answer "" there, not dereference a value the
// failed load never stored and take the boot down with it.
func TestRetiredCityScopeOnAControllerWhoseLoadFailed(t *testing.T) {
	b := newFileBoot(t)
	b.seed(map[string]string{
		"speed_auto_loc":   "49.2827,-123.1207",
		"speed_auto_label": "Vancouver, BC",
	})
	st, err := store.Open(b.path)
	if err != nil {
		t.Fatalf("open %s: %v", b.path, err)
	}
	st.Close() // every read from here fails, as the first one can on a real boot
	c, err := New(context.Background(), st, Values{})
	if err == nil {
		t.Fatal("New read a closed store without an error; this test no longer models a failed load")
	}
	if c == nil {
		t.Fatal("New returned no controller with its error; the daemon boots on that controller")
	}
	if got := c.RetiredCityScope(); got != "" {
		t.Errorf("RetiredCityScope() = %q on a controller whose load failed, want \"\"", got)
	}
}

// A restored backup brings the rows in without a restart (import reloads the
// controller in place), so the reload path has to read them too - otherwise the
// same install reports a scope after a restart and none before it.
func TestRetiredCityScopeIsRefreshedOnReload(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	c := b.boot()
	if got := c.RetiredCityScope(); got != "" {
		t.Fatalf("RetiredCityScope() = %q before anything was stored", got)
	}
	if _, err := b.st.SetSettingsDiff(ctx, map[string]string{
		"speed_auto_loc":   "48.8566,2.3522",
		"speed_auto_label": "Paris, FR",
	}); err != nil {
		t.Fatalf("write the rows behind the controller: %v", err)
	}
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := c.RetiredCityScope(); got != "Paris, FR" {
		t.Errorf("after a reload RetiredCityScope() = %q, want the rows that just arrived", got)
	}
}
