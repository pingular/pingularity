package web

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
)

// The Data tab's three Keep for boxes carry their ceiling in the markup,
// max="3650", so the browser refuses a longer window at Save instead of the
// daemon cutting it to settings.MaxDuration without a word (settings.normalize
// clamps every retention window there, and POST /api/settings answers 200).
// The page is embedded in this binary, so a page served by this binary always
// carries this binary's figure. A tab left open across an upgrade is the one
// exception - it keeps the old page, and with it the old figure, against the
// new daemon - and that is harmless for as long as MaxDuration stays ten years.
// What can go wrong is a later edit: the figure can drift from the constant,
// and the two ways it can drift are not alike. A page cap above the daemon's
// brings the silent cut back. A page cap below it is worse: a window stored
// above the page's figure, by a flag or the API, makes the box invalid the
// moment it loads, and the save gate then refuses the whole drawer over a box
// nobody touched - the unsavable drawer the number inputs' step rule exists to
// prevent (ui.test.mjs, "number inputs never declare a step coarser than the
// daemon accepts").
func TestRetentionBoxesStopWhereTheDaemonClamps(t *testing.T) {
	ui, err := os.ReadFile("ui/index.html")
	if err != nil {
		t.Fatalf("read ui/index.html: %v", err)
	}
	const day = 24 * time.Hour
	if settings.MaxDuration%day != 0 {
		t.Fatalf("settings.MaxDuration = %v is not a whole number of days, and the boxes count days", settings.MaxDuration)
	}
	want := strconv.FormatInt(int64(settings.MaxDuration/day), 10)
	maxAttr := regexp.MustCompile(`\smax="([^"]*)"`)
	for _, id := range []string{"setRetention", "setSpeedRet", "setDowntime"} {
		tag := regexp.MustCompile(`<input\b[^>]*\bid="` + id + `"[^>]*>`).Find(ui)
		if tag == nil {
			t.Errorf("ui/index.html has no <input id=%q>", id)
			continue
		}
		m := maxAttr.FindSubmatch(tag)
		if m == nil {
			t.Errorf("%s has no max: a longer window saves, and the daemon cuts it to %v without a word", id, settings.MaxDuration)
			continue
		}
		if got := string(m[1]); got != want {
			t.Errorf("%s stops at %s days, but the daemon clamps retention at %s days (settings.MaxDuration)", id, got, want)
		}
	}
}
