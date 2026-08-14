package store

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The fsGroup shape: group-writable, not world-writable, not ours. Only the
// message keys on this - the directory is left unchanged either way.
func TestFsGroupShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		perm    os.FileMode
		ownedBy bool // owned by this user
		want    bool
	}{
		{"kubelet default (0770, root-owned)", 0o770, false, true},
		{"CSI variant with world r-x (0775)", 0o775, false, true},
		{"group read-only is not fsGroup write access", 0o750, false, false},
		{"world-writable is not the kubelet shape", 0o777, false, false},
		{"ours: the carve-out/generic rule owns this case", 0o770, true, false},
		{"owner-only never warns at all", 0o700, false, false},
	} {
		if got := fsGroupShape(tc.perm, tc.ownedBy); got != tc.want {
			t.Errorf("%s: fsGroupShape(%o, %v) = %v, want %v", tc.name, tc.perm, tc.ownedBy, got, tc.want)
		}
	}
}

// Message selection: the k8s fsGroup wording (chown to 65532 /
// fsGroupChangePolicy) only for the fsGroup shape inside a container;
// everywhere else the generic dedicated-directory advice stands.
func TestLooseDataDirWarningSelection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shapes under test are Unix modes (group-writable, not world); NTFS cannot represent them")
	}
	restore := func(in func() bool, owned func(string) bool) { inContainerFn = in; ownedByThisUserFn = owned }
	origIn, origOwned := inContainerFn, ownedByThisUserFn
	defer restore(origIn, origOwned)

	mkDir := func(t *testing.T, perm os.FileMode) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "data")
		if err := os.Mkdir(dir, perm); err != nil {
			t.Fatal(err)
		}
		// Mkdir is umask-filtered; force the mode under test explicitly.
		if err := os.Chmod(dir, perm); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	for _, tc := range []struct {
		name        string
		perm        os.FileMode
		inContainer bool
		ownedByUs   bool
		wantFsGroup bool
	}{
		{"container + fsGroup shape: k8s wording", 0o770, true, false, true},
		{"native, same shape: generic (fsGroup advice is meaningless there)", 0o770, false, false, false},
		{"container but dir is ours: generic", 0o770, true, true, false},
		{"container, world-writable: generic", 0o777, true, false, false},
		{"container, group read-only: generic", 0o755, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := mkDir(t, tc.perm)
			inContainerFn = func() bool { return tc.inContainer }
			ownedByThisUserFn = func(string) bool { return tc.ownedByUs }
			msg := looseDataDirWarning(dir)
			gotFsGroup := strings.Contains(msg, "fsGroup")
			if gotFsGroup != tc.wantFsGroup {
				t.Fatalf("message = %q, want fsGroup wording: %v", msg, tc.wantFsGroup)
			}
			if tc.wantFsGroup {
				for _, must := range []string{"65532", "chown", "fsGroupChangePolicy"} {
					if !strings.Contains(msg, must) {
						t.Errorf("fsGroup message lacks the actionable piece %q: %q", must, msg)
					}
				}
			} else if !strings.Contains(msg, "Consider a dedicated -db directory") {
				t.Errorf("generic message lost its advice: %q", msg)
			}
		})
	}
}

// End to end through Open: a loose pre-existing dir with the PVC/fsGroup shape
// must log the actionable k8s warning (and not the generic one), and must
// still be left unchanged - the message got calmer, not the check weaker.
func TestOpenLogsFsGroupWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("GroupOrWorldAccessible reports known=false on Windows; the warning branch never fires")
	}
	restore := func(cd string, in func() bool, owned func(string) bool) {
		containerDataDir, inContainerFn, ownedByThisUserFn = cd, in, owned
	}
	origCD, origIn, origOwned := containerDataDir, inContainerFn, ownedByThisUserFn
	defer restore(origCD, origIn, origOwned)

	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	// A PVC, not the image volume: default path elsewhere so the lineage
	// carve-out is out of the picture, ownership modelled as root:<fsGroup>.
	containerDataDir = filepath.Join(t.TempDir(), "elsewhere")
	inContainerFn = func() bool { return true }
	ownedByThisUserFn = func(string) bool { return false }

	var buf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origOut)

	st, err := Open(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Close()

	logged := buf.String()
	if !strings.Contains(logged, "fsGroup") || !strings.Contains(logged, "65532") {
		t.Errorf("boot log lacks the actionable fsGroup warning: %q", logged)
	}
	if strings.Contains(logged, "Consider a dedicated -db directory") {
		t.Errorf("generic advice logged where the fsGroup wording applies: %q", logged)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o770 {
		t.Errorf("dir mode = %o, want 770 untouched (the warning must never re-permission a PVC)", got)
	}
}
