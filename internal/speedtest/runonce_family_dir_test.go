package speedtest

import (
	"context"
	"testing"

	"github.com/pingular/pingularity/internal/stats"
)

// I5/I9 plumbing: the engine's Result.IPFamily / Result.UDPDirection must cross
// the RunOnce seam into store.SpeedSample and out to durable history - the
// engine records them, but only this mapping makes them exist anywhere a reader
// can see. Empty stays empty: a run that recorded neither must persist as
// unknown, never as a fabricated family or direction.
func TestRunOncePersistsFamilyAndUDPDirection(t *testing.T) {
	stats.ResetForTest()
	s, st := newRunOnceScheduler(t, Result{
		DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S",
		DownloadBytes: 5_000_000, UploadBytes: 1_000_000,
		IPFamily: "6", UDPDirection: "down",
	})
	sp, err := s.RunOnce(context.Background(), "manual")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sp.IPFamily != "6" || sp.UDPDirection != "down" {
		t.Fatalf("RunOnce mapping dropped the fields: ip_family=%q udp_direction=%q", sp.IPFamily, sp.UDPDirection)
	}
	got, err := st.LatestSpeed(context.Background())
	if err != nil || got == nil {
		t.Fatalf("latest: %v (nil=%v)", err, got == nil)
	}
	if got.IPFamily != "6" || got.UDPDirection != "down" {
		t.Fatalf("persisted row lost the fields: ip_family=%q udp_direction=%q", got.IPFamily, got.UDPDirection)
	}
}

func TestRunOnceLeavesUnrecordedFamilyAndDirectionEmpty(t *testing.T) {
	stats.ResetForTest()
	s, st := newRunOnceScheduler(t, Result{DownloadMbps: 5, Server: "S", DownloadBytes: 5_000_000})
	if _, err := s.RunOnce(context.Background(), "manual"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, err := st.LatestSpeed(context.Background())
	if err != nil || got == nil {
		t.Fatalf("latest: %v (nil=%v)", err, got == nil)
	}
	if got.IPFamily != "" || got.UDPDirection != "" {
		t.Fatalf("unrecorded fields must persist empty, got ip_family=%q udp_direction=%q", got.IPFamily, got.UDPDirection)
	}
}
