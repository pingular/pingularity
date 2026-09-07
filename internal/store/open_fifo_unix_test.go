//go:build !windows

package store

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The remaining shape a -db path can take. A fifo (or a device node) is neither
// a database nor something to move aside, and handing it to the driver gets the
// operator a message about the file not being a database when what is wrong is
// that it is not a file at all.
func TestOpenRefusesAFifoAtTheDBPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable here: %v", err)
	}
	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("Open succeeded on a fifo at the -db path")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("the error does not name the mistake: %v", err)
	}
	if matches, _ := filepath.Glob(path + ".*.corrupt"); len(matches) != 0 {
		t.Fatalf("the fifo was set aside as a corrupt database: %v", matches)
	}
	if fi, lerr := os.Lstat(path); lerr != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the fifo at the -db path is gone or replaced (%v)", lerr)
	}
}
