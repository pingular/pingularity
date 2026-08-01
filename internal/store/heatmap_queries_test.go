package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The sizing rule this file guards: a read endpoint issues one query per table
// per request and intersects in Go. It does NOT issue one query per row of the
// answer.
//
// DowntimeByDay broke that rule in prorate, which asked pausedOverlap - an
// unbounded SUM over the whole `pauses` table - once per outage DAY-SEGMENT. A
// year on the monitoring schedule is ~52k pause rows (one per five-minute
// checkpoint) and a flaky link is ~700 outages, so a single /api/heatmap poll
// ran ~705 full table scans and took 4.2s; the dashboard re-issues it every 60s
// per visible tab. The fix is not a new idea - pauseSpans (and its own comment,
// which argues exactly this) already existed one function away, wired into the
// observation loop and not into prorate.
//
// A wall-clock assertion would be flaky and would not say WHY it got slow, so
// this counts the queries instead: it swaps the Store's handle for one on a
// driver that records every statement, and asserts the pause table is read a
// bounded number of times regardless of how many outages there are.
// ---------------------------------------------------------------------------

type sqlCounter struct {
	mu   sync.Mutex
	seen []string
}

func (c *sqlCounter) record(q string) {
	c.mu.Lock()
	c.seen = append(c.seen, q)
	c.mu.Unlock()
}

// countMatching reports how many recorded statements read the named table.
func (c *sqlCounter) countMatching(table string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, q := range c.seen {
		if strings.Contains(q, " "+table) {
			n++
		}
	}
	return n
}

func (c *sqlCounter) reset() {
	c.mu.Lock()
	c.seen = nil
	c.mu.Unlock()
}

// countingDriver wraps a real driver and records the SQL of every statement
// prepared through it. It deliberately implements only Prepare (not
// QueryerContext), so database/sql routes every query through PrepareContext
// where the counting happens.
type countingDriver struct {
	inner driver.Driver
	c     *sqlCounter
}

func (d *countingDriver) Open(name string) (driver.Conn, error) {
	cn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{inner: cn, c: d.c}, nil
}

type countingConn struct {
	inner driver.Conn
	c     *sqlCounter
}

func (cn *countingConn) Prepare(q string) (driver.Stmt, error) {
	cn.c.record(q)
	return cn.inner.Prepare(q)
}

func (cn *countingConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	cn.c.record(q)
	if p, ok := cn.inner.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, q)
	}
	return cn.inner.Prepare(q)
}

func (cn *countingConn) Close() error              { return cn.inner.Close() }
func (cn *countingConn) Begin() (driver.Tx, error) { return cn.inner.Begin() }

func (cn *countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := cn.inner.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return cn.inner.Begin()
}

var countingSeq atomic.Uint64

// countingStore opens a file-backed store normally (so every migration and
// pragma runs on the real driver) and then re-opens the same file through the
// counting driver, swapping the handle in.
func countingStore(t *testing.T) (*Store, *sqlCounter) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "count.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Reach the registered driver through a throwaway handle; sql.Drivers() only
	// gives names.
	probe, err := sql.Open("sqlite", buildDSN(path))
	if err != nil {
		t.Fatalf("probe open: %v", err)
	}
	inner := probe.Driver()
	probe.Close()

	c := &sqlCounter{}
	name := fmt.Sprintf("sqlite-counting-%d", countingSeq.Add(1))
	sql.Register(name, &countingDriver{inner: inner, c: c})
	db, err := sql.Open(name, buildDSN(path))
	if err != nil {
		t.Fatalf("counting open: %v", err)
	}
	db.SetMaxOpenConns(4)
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	st.db.Close()
	st.db = db
	return st, c
}

// The heatmap's work per request must not scale with the number of outages in
// the window. Ten times the outages over the same pause history must cost the
// same number of reads of `pauses`.
func TestDowntimeByDayReadsPausesOnce(t *testing.T) {
	ctx := context.Background()
	run := func(outages int) int {
		st, c := countingStore(t)
		now := time.Now()
		since := now.Add(-30 * 24 * time.Hour)
		// A schedule user's pause history: five-minute checkpoint rows, 12h a day.
		for d := 0; d < 30; d++ {
			day := since.Add(time.Duration(d) * 24 * time.Hour)
			for k := 0; k < 12; k++ {
				if _, err := st.InsertPause(ctx, day.Add(time.Duration(k)*5*time.Minute), 300); err != nil {
					t.Fatalf("pause: %v", err)
				}
			}
		}
		// Outages spread across the window, each closed, each inside a monitored hour.
		step := 29 * 24 * time.Hour / time.Duration(outages)
		for i := 0; i < outages; i++ {
			at := since.Add(time.Duration(i)*step + 16*time.Hour)
			if err := st.InsertEvent(ctx, at, "down", 0, ""); err != nil {
				t.Fatalf("down: %v", err)
			}
			if err := st.InsertEvent(ctx, at.Add(4*time.Minute), "up", 240, ""); err != nil {
				t.Fatalf("up: %v", err)
			}
		}
		c.reset()
		if _, err := st.DowntimeByDay(ctx, since, time.UTC); err != nil {
			t.Fatalf("DowntimeByDay: %v", err)
		}
		return c.countMatching("pauses")
	}
	few, many := run(20), run(200)
	if few != many {
		t.Fatalf("DowntimeByDay read `pauses` %d times for 20 outages and %d times for 200: the work per "+
			"request must not scale with the answer's row count", few, many)
	}
	// One bounded query for the window, not one per day and not one per segment.
	if many > 2 {
		t.Fatalf("DowntimeByDay read `pauses` %d times; the rule is one query per table per request", many)
	}
}

// The hoist must not change a single second of the answer. prorate's per-segment
// SUM and the in-Go intersection clamp identically, and pauseSpans has already
// clamped to [since, now], inside which every segment prorate builds lies - so
// paused time is still subtracted from an outage exactly once, and unobserved
// wall time is still neither up nor down.
func TestDowntimeByDayStillExcludesPausedTime(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	now := time.Now().Truncate(time.Hour)
	since := now.Add(-48 * time.Hour)
	down := now.Add(-30 * time.Hour)
	up := down.Add(2 * time.Hour)
	// Monitoring was off for the middle hour of the two-hour outage, so only one
	// hour of it was observed - which is what duration_s records.
	if _, err := st.InsertPause(ctx, down.Add(30*time.Minute), 3600); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := st.InsertEvent(ctx, down, "down", 0, ""); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := st.InsertEvent(ctx, up, "up", 3600, ""); err != nil {
		t.Fatalf("up: %v", err)
	}
	days, err := st.DowntimeByDay(ctx, since, time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	total := 0
	for _, d := range days {
		total += d.DowntimeS
	}
	if total != 3600 {
		t.Fatalf("heatmap books %ds of downtime for a 2h outage with 1h unobserved inside it, want 3600 - "+
			"the paused hour is neither up nor down", total)
	}
}
