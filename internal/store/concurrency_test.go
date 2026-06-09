package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The store runs WAL with a multi-connection pool and a documented out-of-tx
// old-value read in SetSettingsDiff (a real SQLITE_BUSY_SNAPSHOT fix). A
// file-backed concurrent writer mix under -race guards that invariant: no BUSY
// errors, and the last settings write wins.
func TestConcurrentWriters(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "concurrent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	var wg sync.WaitGroup
	errc := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			if err := st.InsertSamples(ctx, []Sample{{TS: time.Now(), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10}}); err != nil {
				errc <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			if err := st.SetSettings(ctx, map[string]string{"k": fmt.Sprintf("v%d", i)}); err != nil {
				errc <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatalf("concurrent writer error (SQLITE_BUSY?): %v", err)
	}
	if m, _ := st.AllSettings(ctx); m["k"] != "v59" {
		t.Errorf("final settings value = %q, want v59 (last write wins)", m["k"])
	}
}
