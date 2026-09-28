//go:build unix

package store

import (
	"context"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// seriesTempBytes runs f and reports the temp files this process held open
// meanwhile. SQLite unlinks a temp file as soon as it has opened it, so no
// directory listing shows one: this stats every descriptor instead and keeps,
// for each unlinked regular file, the largest size it saw.
//
// A file that was already open and unlinked before f ran is not f's. A store
// some other test left open looks exactly like that once its directory is
// removed, and would be booked to a scan that wrote nothing.
func seriesTempBytes(f func()) (files int, bytes int64) {
	type file struct{ dev, ino uint64 }
	each := func(note func(file, int64)) {
		for fd := 3; fd < 1024; fd++ {
			var s syscall.Stat_t
			if syscall.Fstat(fd, &s) != nil || s.Mode&syscall.S_IFMT != syscall.S_IFREG || s.Nlink != 0 {
				continue
			}
			note(file{uint64(s.Dev), uint64(s.Ino)}, s.Size)
		}
	}
	before := map[file]bool{}
	each(func(k file, _ int64) { before[k] = true })
	seen := map[file]int64{}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			each(func(k file, size int64) {
				if !before[k] && size > seen[k] {
					seen[k] = size
				}
			})
			time.Sleep(200 * time.Microsecond)
		}
	}()
	f()
	close(stop)
	<-done
	for _, n := range seen {
		bytes += n
	}
	return len(seen), bytes
}

// The old statement wrote 4.3 MB of temp files for one 1d dual-stack chart and
// 147 MB for a 30d one, on every redraw. The scan must write none. The oracle
// runs first over the same rows: if the sampler cannot see ITS temp file, it
// cannot vouch for the new scan either, and the test says so instead of passing.
func TestSeriesScanWritesNoTempFiles(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("seeds 112,320 sample rows")
	}
	ctx := context.Background()
	end := time.Unix(1_788_220_800, 0)
	st := seedSeriesDB(t, benchStacks()[1], 26*time.Hour, end, 5)
	since := end.Add(-26 * time.Hour)
	var err error
	n, b := seriesTempBytes(func() { _, err = seriesSQLOracle(ctx, st.db, since.Unix(), end.Unix(), 57, nil) })
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if b == 0 {
		// Where the sampler is known to work, seeing nothing is a failure: a skip
		// nobody reads would leave the scan unguarded.
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			t.Fatalf("premise broken: the sampler saw no temp file from the sorting statement (%d files), so it cannot vouch for the scan", n)
		}
		t.Skipf("the sampler saw no temp file from the sorting statement (%d files), so it proves nothing here", n)
	}
	t.Logf("the old statement: %d temp files, %d bytes", n, b)
	n, b = seriesTempBytes(func() { _, err = st.seriesQuery(ctx, since, end, 57, []string{"google"}) })
	if err != nil {
		t.Fatalf("seriesQuery: %v", err)
	}
	if n != 0 || b != 0 {
		t.Errorf("the scan wrote %d temp files, %d bytes; it must write none", n, b)
	}
}

// The sampler must not book a file to f that was open before f ran.
func TestSeriesTempSamplerIgnoresWhatWasAlreadyOpen(t *testing.T) {
	path := t.TempDir() + "/held"
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.Write(fd, make([]byte, 4096)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := syscall.Unlink(path); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if n, b := seriesTempBytes(func() { time.Sleep(5 * time.Millisecond) }); n != 0 || b != 0 {
		t.Errorf("a file open and unlinked beforehand was counted: %d files, %d bytes", n, b)
	}
	// And the other half, or the first proves nothing: one that appears while f
	// runs is seen.
	var fd2 int
	n, b := seriesTempBytes(func() {
		p := t.TempDir() + "/made"
		if fd2, err = syscall.Open(p, syscall.O_RDWR|syscall.O_CREAT, 0o600); err != nil {
			return
		}
		if _, err = syscall.Write(fd2, make([]byte, 8192)); err != nil {
			return
		}
		if err = syscall.Unlink(p); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	})
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer syscall.Close(fd2)
	if n != 1 || b != 8192 {
		t.Errorf("a file made and unlinked while f ran: saw %d files, %d bytes; want 1 and 8192", n, b)
	}
}
