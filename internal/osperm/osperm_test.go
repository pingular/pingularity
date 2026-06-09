package osperm

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSecureFileAndDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "secret.key")
	if err := os.WriteFile(file, []byte("k"), 0o666); err != nil {
		t.Fatal(err)
	}

	if err := SecureDir(dir); err != nil {
		t.Fatalf("SecureDir: %v", err)
	}
	if err := SecureFile(file); err != nil {
		t.Fatalf("SecureFile: %v", err)
	}

	if runtime.GOOS == "windows" {
		return // owner-only enforced by DACL, not by synthetic mode bits
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("dir perms = %o, want 700", got)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("file perms = %o, want 600", got)
	}
}
