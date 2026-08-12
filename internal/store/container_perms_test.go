package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The container carve-out: inside a container, at exactly the image's own data
// path, owned by this user, a group/world-accessible pre-existing directory is
// TIGHTENED to 0700 instead of warned about - Linux Docker engines recreate a
// fresh volume's root loose during copy-up, and that directory is ours by
// construction. Outside those three conditions the never-repermission rule
// stands untouched.
func TestContainerDataDirCarveOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("GroupOrWorldAccessible reports known=false on Windows; the branch never fires")
	}
	restore := func(dir string, in func() bool) { containerDataDir = dir; inContainerFn = in }
	origDir, origIn := containerDataDir, inContainerFn
	defer restore(origDir, origIn)

	mkLoose := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "data")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// TempDir may be created under a umask; force the loose mode explicitly.
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// The image-lineage marker: fresh named volumes carry it (copy-up from
		// the image); bind mounts and PVCs do not. Tests peel it off to model
		// those.
		if err := os.WriteFile(filepath.Join(dir, ".pingularity-image-dir"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	mode := func(t *testing.T, dir string) os.FileMode {
		t.Helper()
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	open := func(t *testing.T, dir string) {
		t.Helper()
		st, err := Open(filepath.Join(dir, "p.db"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		st.Close()
	}

	t.Run("container + own path: tightened", func(t *testing.T) {
		dir := mkLoose(t)
		containerDataDir, inContainerFn = dir, func() bool { return true }
		open(t, dir)
		if got := mode(t, dir); got != 0o700 {
			t.Errorf("mode = %o, want 700 (the carve-out must tighten the image's own volume dir)", got)
		}
	})

	t.Run("not in a container: untouched", func(t *testing.T) {
		dir := mkLoose(t)
		containerDataDir, inContainerFn = dir, func() bool { return false }
		open(t, dir)
		if got := mode(t, dir); got != 0o755 {
			t.Errorf("mode = %o, want 755 (never repermission outside a container)", got)
		}
	})

	t.Run("container but a different path: untouched", func(t *testing.T) {
		dir := mkLoose(t)
		containerDataDir, inContainerFn = filepath.Join(t.TempDir(), "elsewhere"), func() bool { return true }
		open(t, dir)
		if got := mode(t, dir); got != 0o755 {
			t.Errorf("mode = %o, want 755 (a bind mount at a custom -db path is not ours to tighten)", got)
		}
	})

	t.Run("no lineage marker: untouched", func(t *testing.T) {
		// A bind-mounted host directory (or an empty PVC) at the default path:
		// right place, right owner, but the content never came from our image.
		dir := mkLoose(t)
		if err := os.Remove(filepath.Join(dir, ".pingularity-image-dir")); err != nil {
			t.Fatal(err)
		}
		containerDataDir, inContainerFn = dir, func() bool { return true }
		open(t, dir)
		if got := mode(t, dir); got != 0o755 {
			t.Errorf("mode = %o, want 755 (no image lineage = a host directory we must not touch)", got)
		}
	})

	t.Run("symlinked default path: target untouched", func(t *testing.T) {
		// The carve-out must never chmod THROUGH a link, matching the
		// pre-existence probe's own Lstat stance.
		target := mkLoose(t)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		containerDataDir, inContainerFn = link, func() bool { return true }
		open(t, link)
		if got := mode(t, target); got != 0o755 {
			t.Errorf("target mode = %o, want 755 (the carve-out followed a symlink)", got)
		}
	})
}
