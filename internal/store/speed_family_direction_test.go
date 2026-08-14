package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// I5/I9: the address family a run actually measured and the UDP probe's sampled
// direction persist per run and read back exactly - family "auto" resolves
// invisibly and loss on an asymmetric path differs by direction, so without
// these columns the stored history is ambiguous.
func TestSpeedFamilyAndUDPDirectionRoundTrip(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.InsertSpeed(ctx, SpeedSample{TS: 100, DownMbps: 50, Server: "S", IPFamily: "6", UDPDirection: "down"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	sp, err := st.LatestSpeed(ctx)
	if err != nil || sp == nil {
		t.Fatalf("latest: %v (nil=%v)", err, sp == nil)
	}
	if sp.IPFamily != "6" || sp.UDPDirection != "down" {
		t.Fatalf("round-trip: ip_family=%q udp_direction=%q, want 6/down", sp.IPFamily, sp.UDPDirection)
	}
	// The API serializes SpeedSample directly - present values must reach it.
	b, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ip_family":"6"`) || !strings.Contains(string(b), `"udp_direction":"down"`) {
		t.Fatalf("JSON missing recorded fields: %s", b)
	}
}

// Rows written before the columns existed stay NULL and must read back as
// ABSENT (empty field, key omitted from JSON) - never a fabricated value.
func TestSpeedFamilyAndUDPDirectionNullBackCompat(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// A pre-migration row: inserted without naming the new columns, so they are
	// SQL NULL - exactly what ALTER TABLE ADD COLUMN leaves on existing rows.
	seedRaw(t, st, `INSERT INTO speed (ts, down_mbps, server) VALUES (?, 42, 'old')`, int64(100))
	var famType, dirType string
	if err := st.db.QueryRow(`SELECT typeof(ip_family), typeof(udp_direction) FROM speed`).Scan(&famType, &dirType); err != nil {
		t.Fatalf("typeof: %v", err)
	}
	if famType != "null" || dirType != "null" {
		t.Fatalf("legacy row must stay NULL, got ip_family=%s udp_direction=%s", famType, dirType)
	}

	sp, err := st.LatestSpeed(ctx)
	if err != nil || sp == nil {
		t.Fatalf("latest: %v (nil=%v)", err, sp == nil)
	}
	if sp.IPFamily != "" || sp.UDPDirection != "" {
		t.Fatalf("NULL must read as absent, got ip_family=%q udp_direction=%q", sp.IPFamily, sp.UDPDirection)
	}
	b, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ip_family") || strings.Contains(string(b), "udp_direction") {
		t.Fatalf("absent values must omit their JSON keys entirely: %s", b)
	}
}

// A backup must carry the two columns (losing them silently re-opens the
// ambiguity they exist to remove), and a NULL-bearing legacy row must survive
// the trip as NULL-shaped, not as an empty-string lookalike of a real value.
func TestSpeedFamilyAndUDPDirectionExportImport(t *testing.T) {
	src, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()
	ctx := context.Background()
	if err := src.InsertSpeed(ctx, SpeedSample{TS: 100, DownMbps: 50, Server: "S", IPFamily: "4", UDPDirection: "up"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seedRaw(t, src, `INSERT INTO speed (ts, down_mbps, server) VALUES (?, 42, 'old')`, int64(50))

	rows, err := src.ExportTable(ctx, "speed")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("exported %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if _, ok := r["ip_family"]; !ok {
			t.Fatalf("export row missing ip_family key: %v", r)
		}
		if _, ok := r["udp_direction"]; !ok {
			t.Fatalf("export row missing udp_direction key: %v", r)
		}
	}

	dst, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dst.Close()
	if n, err := dst.ImportTable(ctx, "speed", rows); err != nil || n != 2 {
		t.Fatalf("import: n=%d err=%v", n, err)
	}
	sp, err := dst.LatestSpeed(ctx)
	if err != nil || sp == nil {
		t.Fatalf("latest after import: %v (nil=%v)", err, sp == nil)
	}
	if sp.IPFamily != "4" || sp.UDPDirection != "up" {
		t.Fatalf("import round-trip: ip_family=%q udp_direction=%q, want 4/up", sp.IPFamily, sp.UDPDirection)
	}
	hist, err := dst.SpeedHistory(ctx, time.Unix(0, 0))
	if err != nil || len(hist) != 2 {
		t.Fatalf("history after import: %v (n=%d)", err, len(hist))
	}
	if hist[0].IPFamily != "" || hist[0].UDPDirection != "" {
		t.Fatalf("legacy row gained fake values across export/import: %+v", hist[0])
	}
}
