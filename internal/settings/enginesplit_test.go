package settings

import (
	"context"
	"os"
	"path/filepath"
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

// A store born on a split build - every fresh install since the birth marker
// arrived - carries the Ookla pair and no iperf3 pair for the ordinary reason:
// the operator set Ookla and left iperf3 alone. Its first boot on a build that
// records the split must read that absence as the default, not as a shared
// value to bring across; the birth marker proves no pre-split build ever wrote
// to this table. Seeding here would have written Ookla's values under iperf3's
// keys one last time, and for good, on exactly the installs the leak had been
// biting.
func TestStoreBornAfterSplitIsNotSeeded(t *testing.T) {
	b := newFileBoot(t)
	b.seed(map[string]string{
		"install_born_version": "0.70.1",
		"speed_direction":      "down",
		"speed_retries":        "3",
	})
	c := b.boot()
	if c.IperfDirection() != "both" || c.IperfRetries() != 1 {
		t.Errorf("iperf3 on a store born after the split = %q/%d, want the untouched both/1 (Ookla's pair was brought across as if the store predated the split)",
			c.IperfDirection(), c.IperfRetries())
	}
	if c.SpeedDirection() != "down" || c.SpeedRetries() != 3 {
		t.Errorf("Ookla pair = %q/%d, want down/3", c.SpeedDirection(), c.SpeedRetries())
	}
	all := b.settings()
	for _, k := range []string{"iperf_direction", "iperf_retries"} {
		if v, ok := all[k]; ok {
			t.Errorf("%s=%q was written on a store that never shared the pair", k, v)
		}
	}
	// The split is still recorded under the table's own marker - the birth
	// marker vouches for the table's past, the split marker is the note that
	// recordMigrations has been over it - and the birth marker is not touched.
	if _, ok := all["engine_split_done"]; !ok {
		t.Error("the split was not recorded on a store born after it")
	}
	if got := all["install_born_version"]; got != "0.70.1" {
		t.Errorf("birth marker = %q, want 0.70.1 untouched", got)
	}
	c = b.boot()
	if c.IperfDirection() != "both" || c.IperfRetries() != 1 {
		t.Errorf("second boot: iperf3 = %q/%d, want both/1", c.IperfDirection(), c.IperfRetries())
	}
}

// The same store when the first boot could not write, so the split marker is
// still absent at the next reload: the birth marker alone must keep the seed
// off on the reload path as on the first load, and the reload then records the
// split without writing anything for iperf3.
func TestStoreBornAfterSplitIsNotSeededOnReload(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.SetSettingsDiff(ctx, map[string]string{
		"install_born_version": "0.70.1",
		"speed_direction":      "down",
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
	if c.IperfDirection() != "both" || c.IperfRetries() != 1 {
		t.Errorf("first load: iperf3 = %q/%d, want both/1", c.IperfDirection(), c.IperfRetries())
	}
	allowStoreWrites(t, st)
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if c.IperfDirection() != "both" || c.IperfRetries() != 1 {
		t.Errorf("after the reload iperf3 = %q/%d, want both/1", c.IperfDirection(), c.IperfRetries())
	}
	all, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"iperf_direction", "iperf_retries"} {
		if v, ok := all[k]; ok {
			t.Errorf("the reload wrote %s=%q on a store that never shared the pair", k, v)
		}
	}
	if _, ok := all["engine_split_done"]; !ok {
		t.Error("the reload did not record the split")
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
