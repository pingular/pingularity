package settings

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The settings table is a sparse overlay: a value left at its shipped default
// is never written. Two migrations read an ABSENT per-feature key as "an older
// install still carrying the shared value" and seed it from that value on every
// load - which is right exactly once, on the first boot after the upgrade, and
// wrong on every boot after: from then on absence means the operator never
// moved the field, and the seed hands it whatever the shared value was last
// set to. These tests boot file-backed stores across real restarts (a fresh
// store over the same file), since the leak only shows on the next load.
//
// The on-disk key names are spelled out rather than taken from the constants,
// as the schedule and Best-of migration tests do: they are the contract an
// older database carries in, and a renamed constant must not quietly re-point
// the test at a different row.

// splitDefaults mirrors main's shipped seed for the fields under test. Both
// engines ship the same direction and retry count, which is what let an absent
// iperf3 key pass for a pre-split install.
func splitDefaults() Values {
	return Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 3 * time.Second,
		DownAfter: 2, UpAfter: 1, Monitoring: true, IPv6Mode: "auto",
		SpeedDirection: "both", SpeedRetries: 1,
		IperfDirection: "both", IperfRetries: 1,
	}
}

// fileBoot is one database file and whichever store currently has it open; a
// restart closes that store and opens a new one, so nothing from the previous
// boot's connection or in-memory values can carry over.
type fileBoot struct {
	t    *testing.T
	path string
	st   *store.Store
}

func newFileBoot(t *testing.T) *fileBoot {
	t.Helper()
	dir := t.TempDir()
	// Owner-only, as a real data directory is, so the store does not warn about it.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b := &fileBoot{t: t, path: filepath.Join(dir, "p.db")}
	t.Cleanup(func() {
		if b.st != nil {
			b.st.Close()
		}
	})
	return b
}

// seed writes rows straight into the file the way an older build left them,
// before any controller has loaded it.
func (b *fileBoot) seed(kv map[string]string) {
	b.t.Helper()
	st, err := store.Open(b.path)
	if err != nil {
		b.t.Fatalf("open %s: %v", b.path, err)
	}
	if _, err := st.SetSettingsDiff(context.Background(), kv); err != nil {
		b.t.Fatalf("seed: %v", err)
	}
	st.Close()
}

// boot opens the file and loads a controller over it, as the daemon does.
func (b *fileBoot) boot() *Controller {
	b.t.Helper()
	if b.st != nil {
		b.st.Close()
	}
	st, err := store.Open(b.path)
	if err != nil {
		b.t.Fatalf("open %s: %v", b.path, err)
	}
	b.st = st
	c, err := New(context.Background(), st, splitDefaults())
	if err != nil {
		b.t.Fatalf("New: %v", err)
	}
	return c
}

// settings reads the table as it sits on disk.
func (b *fileBoot) settings() map[string]string {
	b.t.Helper()
	all, err := b.st.AllSettings(context.Background())
	if err != nil {
		b.t.Fatalf("AllSettings: %v", err)
	}
	return all
}

// Changing only the Ookla direction and retries - the dashboard posts the whole
// form, so the untouched iperf3 pair rides along at its default and stays
// unpersisted - must leave iperf3 exactly where it was after a restart. It
// used to come back wearing the Ookla values, and the next save then pinned
// them.
func TestOoklaKnobsStayOutOfIperfAcrossRestart(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	c := b.boot()
	if _, err := c.Update(ctx, Patch{
		SpeedDirection: pv("down"), SpeedRetries: pv(3),
		IperfDirection: pv("both"), IperfRetries: pv(1),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	c = b.boot()
	if got := c.IperfDirection(); got != "both" {
		t.Errorf("after a restart iperf3 direction = %q, want the untouched both (the Ookla direction leaked across the split)", got)
	}
	if got := c.IperfRetries(); got != 1 {
		t.Errorf("after a restart iperf3 retries = %d, want the untouched 1 (the Ookla retries leaked across the split)", got)
	}
	// The next save echoes the form back. It must not pin anything for iperf3:
	// the pair never moved, so the store stays sparse for it.
	if _, err := c.Update(ctx, Patch{
		SpeedDirection: pv("down"), SpeedRetries: pv(3),
		IperfDirection: pv(c.IperfDirection()), IperfRetries: pv(c.IperfRetries()),
	}); err != nil {
		t.Fatalf("second Update: %v", err)
	}
	all := b.settings()
	if v, ok := all["iperf_direction"]; ok {
		t.Errorf("iperf_direction=%q was persisted by a save that never moved it", v)
	}
	if v, ok := all["iperf_retries"]; ok {
		t.Errorf("iperf_retries=%q was persisted by a save that never moved it", v)
	}
	// An API caller that patches only the Ookla pair (iperf3 unsubmitted) gets
	// the same guarantee.
	if _, err := c.Update(ctx, Patch{SpeedDirection: pv("up"), SpeedRetries: pv(2)}); err != nil {
		t.Fatalf("Ookla-only Update: %v", err)
	}
	c = b.boot()
	if c.IperfDirection() != "both" || c.IperfRetries() != 1 {
		t.Errorf("after an Ookla-only patch and a restart iperf3 = %q/%d, want both/1",
			c.IperfDirection(), c.IperfRetries())
	}
	if c.SpeedDirection() != "up" || c.SpeedRetries() != 2 {
		t.Errorf("Ookla pair after restart = %q/%d, want up/2", c.SpeedDirection(), c.SpeedRetries())
	}
}

// A database from before the split carries only the shared speed_* pair. Its
// first boot on the split build seeds iperf3 from it - and must record that it
// did, under iperf3's own keys and with the split marked applied, so the
// second boot reads the pair back instead of deriving it again from whatever
// Ookla has become.
func TestPreSplitStoreMigratesOnce(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	b.seed(map[string]string{"speed_direction": "down", "speed_retries": "3"})
	c := b.boot()
	if c.IperfDirection() != "down" || c.IperfRetries() != 3 {
		t.Fatalf("pre-split install did not migrate: iperf3 = %q/%d, want down/3", c.IperfDirection(), c.IperfRetries())
	}
	all := b.settings()
	if all["iperf_direction"] != "down" || all["iperf_retries"] != "3" {
		t.Errorf("the seeded pair was not written under iperf3's own keys: iperf_direction=%q iperf_retries=%q",
			all["iperf_direction"], all["iperf_retries"])
	}
	if _, ok := all["engine_split_done"]; !ok {
		t.Error("the split was applied but not recorded, so every later boot would apply it again")
	}
	c = b.boot()
	if c.IperfDirection() != "down" || c.IperfRetries() != 3 {
		t.Errorf("migration lost at the second boot: iperf3 = %q/%d, want down/3", c.IperfDirection(), c.IperfRetries())
	}
	// From here the pair is iperf3's own. Putting it back to the shipped default
	// sticks (Reset to defaults posts exactly this)...
	if _, err := c.Update(ctx, Patch{IperfDirection: pv("both"), IperfRetries: pv(1)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// ...and a later Ookla change no longer reaches it.
	if _, err := c.Update(ctx, Patch{SpeedDirection: pv("up"), SpeedRetries: pv(0)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	c = b.boot()
	if c.IperfDirection() != "both" || c.IperfRetries() != 1 {
		t.Errorf("iperf3 after reset and an Ookla change = %q/%d, want both/1", c.IperfDirection(), c.IperfRetries())
	}
	if c.SpeedDirection() != "up" || c.SpeedRetries() != 0 {
		t.Errorf("Ookla pair = %q/%d, want up/0", c.SpeedDirection(), c.SpeedRetries())
	}
}

// A restore lands the backup's rows with the store's own upsert and then
// reloads. A backup taken from a sparse store carries the Ookla pair and no
// iperf3 pair; the reload must not read that absence as a pre-split install.
func TestImportedOoklaKnobsStayOutOfIperfOnReload(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	c := b.boot()
	if _, err := b.st.SetSettingsDiff(ctx, map[string]string{"speed_direction": "down", "speed_retries": "3"}); err != nil {
		t.Fatalf("import rows: %v", err)
	}
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if c.IperfDirection() != "both" || c.IperfRetries() != 1 {
		t.Errorf("after an import reload iperf3 = %q/%d, want both/1", c.IperfDirection(), c.IperfRetries())
	}
	if c.SpeedDirection() != "down" || c.SpeedRetries() != 3 {
		t.Errorf("imported Ookla pair = %q/%d, want down/3", c.SpeedDirection(), c.SpeedRetries())
	}
}

// When the first boot's write fails (the store refuses writes while a volume
// remounts, say), the migration is still live in memory and the next reload
// that finds the store writable again records it - the same recovery the
// legacy password re-seal gets.
func TestPreSplitMigrationRecordedByALaterReload(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.SetSettingsDiff(ctx, map[string]string{"speed_direction": "down", "speed_retries": "3"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	denyStoreWrites(t, st)
	var c *Controller
	said := captureStderr(t, func() {
		var err error
		if c, err = New(ctx, st, splitDefaults()); err != nil {
			t.Fatalf("New: %v", err)
		}
	})
	if !strings.Contains(said, "could not record migrated settings") {
		t.Errorf("a failed migration write must be said on stderr (the logger is not up yet); got %q", said)
	}
	if c.IperfDirection() != "down" || c.IperfRetries() != 3 {
		t.Fatalf("migration not live after a failed write: iperf3 = %q/%d", c.IperfDirection(), c.IperfRetries())
	}
	allowStoreWrites(t, st)
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	all, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if all["iperf_direction"] != "down" || all["iperf_retries"] != "3" {
		t.Errorf("the reload did not record the migration: iperf_direction=%q iperf_retries=%q", all["iperf_direction"], all["iperf_retries"])
	}
	if _, ok := all["engine_split_done"]; !ok {
		t.Error("the reload did not mark the split applied")
	}
}

// The split marker is bookkeeping about the table, not operator configuration:
// a fresh install carries it from its first boot and must still read as fresh.
func TestSplitMarkerIsNotConfiguration(t *testing.T) {
	b := newFileBoot(t)
	c := b.boot()
	all := b.settings()
	if _, ok := all["engine_split_done"]; !ok {
		t.Fatal("a fresh store was not marked at its first boot; the first Ookla save would then read as a pre-split install at the next restart")
	}
	if hasPriorConfiguration(all) {
		t.Errorf("a fresh install reads as configured after its first boot: %v", all)
	}
	est, err := c.EstablishedInStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if est {
		t.Error("a fresh install reads as established after its first boot")
	}
}

// The legacy master toggle (schedule_enabled) is read once to seed the
// per-feature flags, and the per-feature flag must then be written under its
// own key. Left unwritten, "schedule off" could never be persisted on an
// upgraded database: a save of the off toggle matched the shipped default and
// wrote nothing, and the next boot read the legacy toggle again - now with a
// window to gate on.
func TestLegacyScheduleToggleConsumedOnce(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	b.seed(map[string]string{"schedule_enabled": "1"})
	c := b.boot()
	if c.get().SchedLatEnabled {
		t.Fatal("precondition: with no window the schedule must load off")
	}
	// The operator draws a window, leaves the toggle off, and saves the form.
	win := []Window{{Days: AllDays, Start: 540, End: 1020}}
	if _, err := c.Update(ctx, Patch{SchedLatEnabled: pv(false), SchedLatWindows: win}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	c = b.boot()
	if c.get().SchedLatEnabled {
		t.Error("schedule off did not persist: the legacy toggle turned it back on at the next boot, gating probing to a window the operator left switched off")
	}
	if got := c.get().SchedLatWindows; len(got) != 1 || got[0] != win[0] {
		t.Errorf("window list after restart = %+v, want %+v", got, win)
	}
	// Turning it on still works and still survives a restart.
	if _, err := c.Update(ctx, Patch{SchedLatEnabled: pv(true)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	c = b.boot()
	if !c.get().SchedLatEnabled {
		t.Error("schedule on did not persist")
	}
}

// The legacy single window migrates on the first boot and is written under the
// new windows key, so the second boot reads it back rather than re-deriving it.
func TestLegacyScheduleWindowMigratesOnce(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	b.seed(map[string]string{
		"schedule_enabled": "1",
		"sched_lat_days":   "0111110", "sched_lat_start": "540", "sched_lat_end": "1020",
	})
	c := b.boot()
	want := []Window{{Days: "0111110", Start: 540, End: 1020}}
	if got := c.get().SchedLatWindows; !c.get().SchedLatEnabled || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("legacy window did not migrate: enabled=%v windows=%+v", c.get().SchedLatEnabled, got)
	}
	all := b.settings()
	if all["sched_lat_enabled"] != "1" || all["sched_lat_windows"] != windowsJSON(want) {
		t.Errorf("migrated schedule not written under its own keys: sched_lat_enabled=%q sched_lat_windows=%q",
			all["sched_lat_enabled"], all["sched_lat_windows"])
	}
	// Speedtests had no legacy window: the toggle is consumed for that feature
	// too, and its empty list keeps it off.
	if all["sched_speed_enabled"] != "0" {
		t.Errorf("sched_speed_enabled=%q, want the consumed legacy toggle written as off (no window)", all["sched_speed_enabled"])
	}
	c = b.boot()
	if got := c.get().SchedLatWindows; !c.get().SchedLatEnabled || len(got) != 1 || got[0] != want[0] {
		t.Errorf("schedule lost at the second boot: enabled=%v windows=%+v", c.get().SchedLatEnabled, got)
	}
	if _, err := c.Update(ctx, Patch{SchedLatEnabled: pv(false)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	c = b.boot()
	if c.get().SchedLatEnabled {
		t.Error("schedule off did not persist over the migrated legacy toggle")
	}
}

// A store born under 0.70 - a birth marker, the Ookla pair, and no iperf3 pair,
// which is what an ordinary save on that release leaves - was NOT running the
// iperf3 defaults. That release re-seeded the pair from Ookla at every load, so
// upload-only with 3 retries is what its iperf3 runs had actually been doing.
// The first boot on a build that records the split has to carry that across:
// the marker is what changes the meaning of the absent key, and until it is
// written the absent key still means "whatever Ookla is set to". Reading it as
// the default instead turns an upload-only run into a down-and-up one, roughly
// twice the transfer, with nothing changed in the tab to explain it.
func TestStoreBornOnASharingBuildKeepsWhatItWasRunning(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	b.seed(map[string]string{
		"install_born_version": "0.70.1",
		"speed_direction":      "up",
		"speed_retries":        "3",
	})
	c := b.boot()
	if c.IperfDirection() != "up" || c.IperfRetries() != 3 {
		t.Errorf("iperf3 after the upgrade = %q/%d, want the up/3 it had been running (the pair it was inheriting was dropped)",
			c.IperfDirection(), c.IperfRetries())
	}
	if c.SpeedDirection() != "up" || c.SpeedRetries() != 3 {
		t.Errorf("Ookla pair = %q/%d, want up/3", c.SpeedDirection(), c.SpeedRetries())
	}
	all := b.settings()
	if all["iperf_direction"] != "up" || all["iperf_retries"] != "3" {
		t.Errorf("the carried pair was not written under iperf3's own keys: iperf_direction=%q iperf_retries=%q",
			all["iperf_direction"], all["iperf_retries"])
	}
	if _, ok := all["engine_split_done"]; !ok {
		t.Error("the split was applied but not recorded, so every later boot would apply it again")
	}
	if got := all["install_born_version"]; got != "0.70.1" {
		t.Errorf("birth marker = %q, want 0.70.1 untouched", got)
	}
	c = b.boot()
	if c.IperfDirection() != "up" || c.IperfRetries() != 3 {
		t.Errorf("second boot: iperf3 = %q/%d, want up/3", c.IperfDirection(), c.IperfRetries())
	}
	// From here the pair is iperf3's own: the default sticks, and Ookla moving
	// no longer reaches it.
	if _, err := c.Update(ctx, Patch{IperfDirection: pv("both"), IperfRetries: pv(1)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := c.Update(ctx, Patch{SpeedDirection: pv("down"), SpeedRetries: pv(0)}); err != nil {
		t.Fatalf("Ookla Update: %v", err)
	}
	c = b.boot()
	if c.IperfDirection() != "both" || c.IperfRetries() != 1 {
		t.Errorf("iperf3 after reset and an Ookla change = %q/%d, want both/1", c.IperfDirection(), c.IperfRetries())
	}
}

// The same store when the first boot could not write, so the split marker is
// still absent at the next reload: the carry is live from the first load and
// the reload records it, pair and marker together.
func TestStoreBornOnASharingBuildKeepsWhatItWasRunningOnReload(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.SetSettingsDiff(ctx, map[string]string{
		"install_born_version": "0.70.1",
		"speed_direction":      "up",
		"speed_retries":        "3",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	denyStoreWrites(t, st)
	var c *Controller
	captureStderr(t, func() {
		var err error
		if c, err = New(ctx, st, splitDefaults()); err != nil {
			t.Fatalf("New: %v", err)
		}
	})
	if c.IperfDirection() != "up" || c.IperfRetries() != 3 {
		t.Errorf("first load: iperf3 = %q/%d, want up/3", c.IperfDirection(), c.IperfRetries())
	}
	allowStoreWrites(t, st)
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if c.IperfDirection() != "up" || c.IperfRetries() != 3 {
		t.Errorf("after the reload iperf3 = %q/%d, want up/3", c.IperfDirection(), c.IperfRetries())
	}
	all, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if all["iperf_direction"] != "up" || all["iperf_retries"] != "3" {
		t.Errorf("the reload did not record the carry: iperf_direction=%q iperf_retries=%q", all["iperf_direction"], all["iperf_retries"])
	}
	if _, ok := all["engine_split_done"]; !ok {
		t.Error("the reload did not mark the split applied")
	}
}

// An iperf3 pair the operator set themselves is a row of its own, and a row is
// taken as-is: the carry must not touch it, and there is nothing to announce.
func TestExplicitIperfPairIsNotOverwrittenByTheCarry(t *testing.T) {
	b := newFileBoot(t)
	b.seed(map[string]string{
		"install_born_version": "0.70.1",
		"speed_direction":      "up",
		"speed_retries":        "3",
		"iperf_direction":      "bidir",
		"iperf_retries":        "0",
	})
	var c *Controller
	said := captureStderr(t, func() { c = b.boot() })
	if c.IperfDirection() != "bidir" || c.IperfRetries() != 0 {
		t.Errorf("iperf3 = %q/%d, want the bidir/0 the operator set", c.IperfDirection(), c.IperfRetries())
	}
	all := b.settings()
	if all["iperf_direction"] != "bidir" || all["iperf_retries"] != "0" {
		t.Errorf("stored iperf3 pair = %q/%q, want bidir/0", all["iperf_direction"], all["iperf_retries"])
	}
	if strings.Contains(said, "had been taking") {
		t.Errorf("a store whose iperf3 pair was set explicitly announced a carry: %q", said)
	}
}

// The carry is the one moment the values change hands, and the numbers
// themselves do not move - so if the boot says nothing, nobody finds out. It
// names what it carried, once: the second boot has the marker and is silent,
// and so is a fresh install, which has no Ookla pair to carry.
func TestTheCarryIsAnnouncedOnceAndOnlyWhenItHappens(t *testing.T) {
	b := newFileBoot(t)
	b.seed(map[string]string{
		"install_born_version": "0.70.1",
		"speed_direction":      "up",
		"speed_retries":        "3",
	})
	said := captureStderr(t, func() { b.boot() })
	for _, want := range []string{"iperf3", `direction "up" and retries 3`, "Ookla"} {
		if !strings.Contains(said, want) {
			t.Errorf("the carry was not said on stderr; missing %q in %q", want, said)
		}
	}
	if again := captureStderr(t, func() { b.boot() }); strings.Contains(again, "had been taking") {
		t.Errorf("the second boot announced the carry again: %q", again)
	}
	fresh := newFileBoot(t)
	if said := captureStderr(t, func() { fresh.boot() }); strings.Contains(said, "had been taking") {
		t.Errorf("a fresh install announced a carry it never made: %q", said)
	}
	if all := fresh.settings(); all["iperf_direction"] != "" || all["iperf_retries"] != "" {
		t.Errorf("a fresh install stored an iperf3 pair: %q/%q", all["iperf_direction"], all["iperf_retries"])
	}
}

// An upgraded store - established before the birth marker existed, so never
// stamped - has only the split marker to go on once its first boot on this
// build has written it. The Ookla save that used to leak must stay out of
// iperf3 there too: every install carried up from an older release is this
// shape.
func TestOoklaKnobsStayOutOfIperfOnUpgradedStore(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	b.seed(map[string]string{"speed_engine": "ookla"})
	c := b.boot()
	all := b.settings()
	if _, born := all["install_born_version"]; born {
		t.Fatal("precondition: an established store must never be stamped with a birth marker")
	}
	if _, ok := all["engine_split_done"]; !ok {
		t.Fatal("the first boot did not record the split on an upgraded store")
	}
	if _, err := c.Update(ctx, Patch{
		SpeedDirection: pv("down"), SpeedRetries: pv(3),
		IperfDirection: pv("both"), IperfRetries: pv(1),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	c = b.boot()
	if c.IperfDirection() != "both" || c.IperfRetries() != 1 {
		t.Errorf("after a restart iperf3 = %q/%d on an upgraded store, want the untouched both/1", c.IperfDirection(), c.IperfRetries())
	}
	if c.SpeedDirection() != "down" || c.SpeedRetries() != 3 {
		t.Errorf("Ookla pair after restart = %q/%d, want down/3", c.SpeedDirection(), c.SpeedRetries())
	}
}

// The line says the pair is STORED under iperf3's own names. A load whose write
// could not land has not stored anything, so it must not say it - it has the
// migration warning instead, and the next load that finds the store writable
// seeds the same pair from the same rows and says it then. Told otherwise, an
// operator reading "out of reach of the next Ookla change" beside a failure
// notice cannot tell which of the two to believe.
func TestTheCarryIsAnnouncedOnlyByTheLoadThatStoresIt(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.SetSettingsDiff(ctx, map[string]string{"speed_direction": "up", "speed_retries": "3"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	denyStoreWrites(t, st)
	var c *Controller
	said := captureStderr(t, func() {
		var err error
		if c, err = New(ctx, st, splitDefaults()); err != nil {
			t.Fatalf("New: %v", err)
		}
	})
	if !strings.Contains(said, "could not record migrated settings") {
		t.Errorf("a failed migration write must still be said on stderr; got %q", said)
	}
	if strings.Contains(said, "had been taking") {
		t.Errorf("a load that stored nothing announced the carry anyway: %q", said)
	}
	if c.IperfDirection() != "up" || c.IperfRetries() != 3 {
		t.Fatalf("the carry is live even when the write fails: iperf3 = %q/%d, want up/3", c.IperfDirection(), c.IperfRetries())
	}
	allowStoreWrites(t, st)
	said = captureStderr(t, func() {
		if err := c.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
	})
	if !strings.Contains(said, "had been taking") {
		t.Errorf("the load that stored the pair did not announce it: %q", said)
	}
	all, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if all["iperf_direction"] != "up" || all["iperf_retries"] != "3" {
		t.Errorf("stored pair = %q/%q, want up/3", all["iperf_direction"], all["iperf_retries"])
	}
}

// The two halves are separate rows and a store can carry one without the other:
// an operator who set the iperf3 direction on 0.70 and never touched its retry
// count left iperf_direction written and iperf_retries following Ookla. The
// carry takes only the half that has no row, and names only the half it took.
func TestOnlyTheHalfWithNoRowOfItsOwnIsCarried(t *testing.T) {
	b := newFileBoot(t)
	b.seed(map[string]string{
		"install_born_version": "0.70.1",
		"speed_direction":      "up",
		"speed_retries":        "3",
		"iperf_direction":      "bidir",
	})
	var c *Controller
	said := captureStderr(t, func() { c = b.boot() })
	if c.IperfDirection() != "bidir" || c.IperfRetries() != 3 {
		t.Errorf("iperf3 = %q/%d, want the bidir it was set to with the 3 retries it was taking", c.IperfDirection(), c.IperfRetries())
	}
	all := b.settings()
	if all["iperf_direction"] != "bidir" || all["iperf_retries"] != "3" {
		t.Errorf("stored iperf3 pair = %q/%q, want bidir/3", all["iperf_direction"], all["iperf_retries"])
	}
	if !strings.Contains(said, "retries 3") {
		t.Errorf("the carried retry count was not named: %q", said)
	}
	if strings.Contains(said, "direction") {
		t.Errorf("a direction the operator had set was named as carried: %q", said)
	}
}

// A retry count is read as a number or not at all. A row that does not parse -
// a hand-edited "" or "abc", or a backup carrying one that a release before
// 0.100 restored as it came - is read straight past, to Ookla's count, by those
// releases at every load and by this one until the split is recorded. So the
// carry has to ask whether the row reads as a count, not whether it is there:
// asked the second way, the first start ran Ookla's 3, recorded the split, left
// the unreadable row where it was and said the pair was kept, and the second
// start ran the default 1 with nothing said at all.
func TestAnIperfRetryRowThatIsNotACountIsCarriedLikeAMissingOne(t *testing.T) {
	for _, bad := range []string{"", "abc", " 3"} {
		t.Run(strconv.Quote(bad), func(t *testing.T) {
			b := newFileBoot(t)
			b.seed(map[string]string{
				"install_born_version": "0.70.1",
				"speed_direction":      "up",
				"speed_retries":        "3",
				"iperf_retries":        bad,
			})
			var c *Controller
			said := captureStderr(t, func() { c = b.boot() })
			if c.IperfRetries() != 3 {
				t.Fatalf("first start: iperf3 retries = %d, want the 3 an unreadable row has always fallen back to", c.IperfRetries())
			}
			if !strings.Contains(said, "retries 3") {
				t.Errorf("the retry count iperf3 had been taking from Ookla was not named: %q", said)
			}
			if got := b.settings()["iperf_retries"]; got != "3" {
				t.Errorf("stored iperf_retries = %q, want the carried 3 in place of the row no build reads", got)
			}
			c = b.boot()
			if c.IperfRetries() != 3 {
				t.Errorf("second start: iperf3 retries = %d, want the 3 the first start ran and said it kept", c.IperfRetries())
			}
		})
	}
}

// A restore is the one door the shared pair can still come through, and the only
// thing in the file that can say a build shared it is the version that wrote it.
// Read it off anything less and an ordinary modern backup - identical rows,
// identical envelope version - gets told it lost a setting it never had.
func TestSharedIperfPairIsReadOffAReleaseVersionOnly(t *testing.T) {
	for _, tc := range []struct {
		version string
		shared  bool
	}{
		{"0.59.0", true}, {"0.70.0", true}, {"0.70.1", true}, {"v0.70.1", true}, {"0.99.9", true},
		{"0.100.0", false}, {"0.100.0-rc.1", false}, {"0.101.2", false}, {"1.0.0", false},
		{"dev", false}, {"", false}, {"0.x.1", false}, {"nonsense", false},
		// What follows the minor is not read: a build stamped from a release tag by
		// git describe, or with a suffix of its own, is that release's code.
		{"v0.70.1-3-gabc1234", true}, {"0.70.2-SNAPSHOT-e3778ad", true}, {"0.70.1+dirty", true},
		// Both numbers are plain digits or it is no release at all.
		{"x.70.1", false}, {"+0.70.1", false}, {"0.-5.0", false}, {"0.+70.1", false}, {" 0.70.1", false},
		{".70.1", false}, {"0..1", false},
	} {
		if got := SharedIperfPair(tc.version); got != tc.shared {
			t.Errorf("SharedIperfPair(%q) = %v, want %v", tc.version, got, tc.shared)
		}
	}
}

// What a restore asks once a backup's rows have landed: which half of the
// iperf3 pair did the machine that wrote the file run on its Ookla values, and
// is that not what iperf3 runs here? It is asked of the FILE's rows - the table
// afterwards also holds whatever this install set for itself - and the rows are
// read the way the sharing build read them, clamps and refusals included.
func TestIperfPairAbsentReadsTheBackupsRowsNotTheTable(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	c := b.boot()
	if c.IperfDirection() != "both" || c.IperfRetries() != 1 {
		t.Fatalf("precondition: iperf3 here = %q/%d, want the shipped both/1", c.IperfDirection(), c.IperfRetries())
	}
	for _, tc := range []struct {
		name             string
		rows             map[string]string
		dir              string
		retries          int
		noDir, noRetries bool
	}{
		{"no pair at all", map[string]string{}, "", 0, false, false},
		{"the whole pair", map[string]string{"speed_direction": "up", "speed_retries": "3"}, "up", 3, true, true},
		{"the direction only", map[string]string{"speed_direction": "up"}, "up", 0, true, false},
		{"the retry count only", map[string]string{"speed_retries": "3"}, "", 3, false, true},
		{"a pair that is what runs here", map[string]string{"speed_direction": "both", "speed_retries": "1"}, "", 0, false, false},
		{"an iperf3 direction of its own", map[string]string{"speed_direction": "up", "speed_retries": "3", "iperf_direction": "bidir"}, "", 3, false, true},
		{"an iperf3 retry count of its own", map[string]string{"speed_direction": "up", "speed_retries": "3", "iperf_retries": "0"}, "up", 0, true, false},
		{"values that build clamped or refused", map[string]string{"speed_direction": "sideways", "speed_retries": "99"}, "", 3, false, true},
		{"a direction only iperf3 could take", map[string]string{"speed_direction": "bidir"}, "bidir", 0, true, false},
		{"rows that say nothing", map[string]string{"speed_direction": "", "speed_retries": "many"}, "", 0, false, false},
		{"an iperf3 retry row that is not a count", map[string]string{"speed_direction": "up", "speed_retries": "3", "iperf_retries": ""}, "up", 3, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, retries, noDir, noRetries := c.IperfPairAbsent(tc.rows)
			if noDir != tc.noDir || noRetries != tc.noRetries {
				t.Errorf("left behind: direction %v, retries %v; want %v, %v", noDir, noRetries, tc.noDir, tc.noRetries)
			}
			if tc.noDir && dir != tc.dir {
				t.Errorf("the direction that machine ran = %q, want %q", dir, tc.dir)
			}
			if tc.noRetries && retries != tc.retries {
				t.Errorf("the retry count that machine ran = %d, want %d", retries, tc.retries)
			}
		})
	}

	// An install already running what the file's machine ran - from rows of its
	// own this time - lost nothing. And a file with no pair in it says nothing
	// even where this install's own iperf3 is not the default that machine ran.
	if _, err := c.Update(ctx, Patch{IperfDirection: pv("up"), IperfRetries: pv(3)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, _, noDir, noRetries := c.IperfPairAbsent(map[string]string{"speed_direction": "up", "speed_retries": "3"}); noDir || noRetries {
		t.Errorf("an install whose own iperf3 runs up/3 was told a backup's up/3 was left behind: direction %v, retries %v", noDir, noRetries)
	}
	if _, _, noDir, noRetries := c.IperfPairAbsent(map[string]string{}); noDir || noRetries {
		t.Errorf("a file with no pair in it was said to leave one behind over this install's own iperf3 pair: direction %v, retries %v", noDir, noRetries)
	}
}

// A table the split has not reached carries the pair across at its next load -
// the restore's own reload - so iperf3 here already runs what the file's
// machine ran, and there is nothing left to report.
func TestIperfPairAbsentSaysNothingWhereTheReloadCarriedThePair(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	c := b.boot()
	// Back to the shape a store carries in from an older release: no marker yet.
	if _, err := b.st.DB().ExecContext(ctx, `DELETE FROM settings WHERE key = 'engine_split_done'`); err != nil {
		t.Fatalf("drop the split marker: %v", err)
	}
	rows := map[string]string{"speed_direction": "up", "speed_retries": "3"}
	if _, err := b.st.SetSettingsDiff(ctx, rows); err != nil {
		t.Fatalf("land the backup's rows: %v", err)
	}
	captureStderr(t, func() {
		if err := c.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
	})
	if c.IperfDirection() != "up" || c.IperfRetries() != 3 {
		t.Fatalf("precondition: a reload of a table without the marker carries the pair; iperf3 = %q/%d, want up/3",
			c.IperfDirection(), c.IperfRetries())
	}
	if _, _, noDir, noRetries := c.IperfPairAbsent(rows); noDir || noRetries {
		t.Errorf("the reload carried the pair across, yet it was reported as left behind: direction %v, retries %v", noDir, noRetries)
	}
}

// A release before 0.100 exports the split marker along with every other
// setting and lands it again on restore, so a backup it takes of a database
// this release has started carries the marker onto a machine that has never
// run this release - one whose iperf3 has taken Ookla's up/3 at every load
// since. On that file the marker records no carry. Its first start here has to
// make the carry as any upgrade does: run up/3, store it under iperf3's own
// names and say so. So does the start after an Open that set the marker aside
// and went no further. The same rows in a file this release has already opened
// - the database that backup came from, stepped down and brought back up - are
// the step-down README describes, and nothing is carried there.
func TestASplitMarkerAnOlderReleaseRestoredIsNotTakenForACarry(t *testing.T) {
	ctx := context.Background()
	rows := map[string]string{
		"install_born_version": "0.70.1",
		"engine_split_done":    "1",
		"speed_engine":         "iperf3",
		"speed_direction":      "up",
		"speed_retries":        "3",
	}
	// restoredByAnOlderRelease leaves a file the way that release's restore
	// does: these rows, and none of the tables only this release creates.
	restoredByAnOlderRelease := func() *fileBoot {
		t.Helper()
		b := newFileBoot(t)
		b.seed(rows)
		st, err := store.Open(b.path)
		if err != nil {
			t.Fatalf("open %s: %v", b.path, err)
		}
		if _, err := st.DB().ExecContext(ctx, `DROP TABLE server_health`); err != nil {
			t.Fatalf("drop the table this release creates: %v", err)
		}
		st.Close()
		return b
	}
	carried := func(when string, b *fileBoot) {
		t.Helper()
		var c *Controller
		said := captureStderr(t, func() { c = b.boot() })
		if c.IperfDirection() != "up" || c.IperfRetries() != 3 {
			t.Errorf("%s: iperf3 = %q/%d, want the up/3 the older release was running", when, c.IperfDirection(), c.IperfRetries())
		}
		if !strings.Contains(said, `direction "up" and retries 3`) {
			t.Errorf("%s: the carry was not said on stderr: %q", when, said)
		}
		all := b.settings()
		if all["iperf_direction"] != "up" || all["iperf_retries"] != "3" {
			t.Errorf("%s: stored iperf3 pair = %q/%q, want up/3", when, all["iperf_direction"], all["iperf_retries"])
		}
		for k, v := range rows {
			if all[k] != v {
				t.Errorf("%s: %s = %q afterwards, want %q", when, k, all[k], v)
			}
		}
		if again := captureStderr(t, func() { c = b.boot() }); strings.Contains(again, "had been taking") || c.IperfRetries() != 3 {
			t.Errorf("%s, then a second start: iperf3 retries = %d and it said %q; want 3 and nothing said", when, c.IperfRetries(), again)
		}
	}

	carried("first start", restoredByAnOlderRelease())

	// An Open that set the marker aside and stopped there, before any settings
	// load, leaves the next start the same carry to make.
	cut := restoredByAnOlderRelease()
	st, err := store.Open(cut.path)
	if err != nil {
		t.Fatalf("open %s: %v", cut.path, err)
	}
	all, err := st.AllSettings(ctx)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := all["engine_split_done"]; ok {
		t.Error("Open kept a split marker on a file no build of this release had opened")
	}
	carried("a start after an Open cut short", cut)

	// The step-down: the same rows in a file this release has opened before.
	back := newFileBoot(t)
	back.seed(rows)
	var c *Controller
	said := captureStderr(t, func() { c = back.boot() })
	if c.IperfDirection() != "both" || c.IperfRetries() != 1 || strings.Contains(said, "had been taking") {
		t.Errorf("a file this release had opened was carried: iperf3 = %q/%d, and it said %q; want the both/1 it ran here and nothing said",
			c.IperfDirection(), c.IperfRetries(), said)
	}
	if all := back.settings(); all["iperf_direction"] != "" || all["iperf_retries"] != "" || all["engine_split_done"] != "1" {
		t.Errorf("a file this release had opened had its rows changed: iperf3 %q/%q, split marker %q",
			all["iperf_direction"], all["iperf_retries"], all["engine_split_done"])
	}
}
