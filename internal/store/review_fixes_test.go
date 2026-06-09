package store

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// C-03: a '?' in the DB path must open the INTENDED file (the driver splits a
// plain DSN at the first '?', so the old string-concatenation silently opened a
// truncated path outside the secured dir at the driver's default 0644 mode).
func TestOpenPathWithQuestionMark(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "net?home.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open %q: %v", dbPath, err)
	}
	if err := st.InsertEvent(context.Background(), time.Unix(1000, 0), "down", -1, ""); err != nil {
		t.Fatalf("insert: %v", err)
	}
	st.Close()

	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("db not created at the intended path %q: %v", dbPath, err)
	}
	if truncated := filepath.Join(dir, "net"); fileExists(truncated) {
		t.Errorf("db was created at the TRUNCATED path %q - the '?' split the DSN", truncated)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(dbPath); err == nil && fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("db mode = %o, want owner-only (no group/world bits)", fi.Mode().Perm())
		}
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// C-02: a data directory that already existed (a shared/system path) must NOT be
// re-permissioned, while a directory pingularity created is tightened. The DB file
// itself is always owner-only.
func TestOpenDoesNotRepermissionExistingDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are POSIX; the Windows DACL path is covered by osperm")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { // pre-existing, group/world-traversable
		t.Fatalf("chmod: %v", err)
	}
	st, err := Open(filepath.Join(dir, "pingularity.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if fi, err := os.Stat(dir); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if fi.Mode().Perm() != 0o755 {
		t.Errorf("pre-existing data dir mode = %o, want 0755 unchanged (must not chmod a dir we did not create)", fi.Mode().Perm())
	}
	if fi, err := os.Stat(filepath.Join(dir, "pingularity.db")); err == nil && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("db mode = %o, want owner-only even in a shared dir", fi.Mode().Perm())
	}
}

// C-07: a trailing 'down' that recovered without a closing 'up' (the monitor
// restarts optimistically online) is a resolved outage that UptimeSince books but
// ResolvedOutagesSince used to miss - printing "no outages" while the heatmap
// showed one. Both must now agree.
func TestResolvedOutagesReconcilesTrailingRecoveredDown(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()

	eventAt(t, st, now, 300, "down", -1)           // down 5 min ago, never closed
	sampleAt(t, st, now, 300, "cf", "ipv4", false) // failing at the down
	sampleAt(t, st, now, 200, "cf", "ipv4", true)  // quorum recovery ~200s ago
	sampleAt(t, st, now, 100, "cf", "ipv4", true)

	count, downtime, err := st.ResolvedOutagesSince(ctx, now.Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("ResolvedOutagesSince: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (a recovered trailing down is a resolved outage)", count)
	}
	if downtime < 90 || downtime > 110 {
		t.Errorf("downtime = %d, want ~100s (down at -300s, recovered at -200s)", downtime)
	}
}
