package util

import (
	"os"
	"path/filepath"
	"testing"
)

// detectContainer, not InContainer: the public wrapper memoizes process-wide,
// so tests drive the uncached probe. t.Setenv isolates the env markers; the
// marker-file list is swapped for temp paths so a containerized CI runner's
// real /.dockerenv can't leak into the "native" case.
func TestDetectContainer(t *testing.T) {
	origMarkers := containerMarkerFiles
	defer func() { containerMarkerFiles = origMarkers }()
	absent := filepath.Join(t.TempDir(), "absent")
	present := filepath.Join(t.TempDir(), "containerenv")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("container", "")
	containerMarkerFiles = []string{absent}
	if detectContainer() {
		t.Fatal("no env marker, no marker file: must read as native")
	}

	// The standard `container` env var: plain LXC/LXD, systemd-nspawn, and
	// podman ships without /run/.containerenv plant this instead of a file.
	for _, v := range []string{"lxc", "systemd-nspawn", "podman"} {
		t.Setenv("container", v)
		if !detectContainer() {
			t.Errorf("container=%s must count as a container marker", v)
		}
	}
	t.Setenv("container", "")

	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	if !detectContainer() {
		t.Error("KUBERNETES_SERVICE_HOST must count as a container marker")
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	containerMarkerFiles = []string{absent, present}
	if !detectContainer() {
		t.Error("a runtime marker file must count as a container marker")
	}
}
