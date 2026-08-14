package speedtest

import (
	"context"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// Ookla results must describe themselves as honestly as iperf ones do: a
// successful loss probe knows which way it sampled (the analyzer sends the
// datagrams, so it is the upstream path), and the transfer's IP family is
// knowable from the connections the run REALLY made - and only from those.

// stubOoklaTransfers replaces ping and both transfer seams with no-network
// stand-ins, so measure() runs its full result-population path offline.
func stubOoklaTransfers(t *testing.T) {
	t.Helper()
	oldPing, oldDown, oldUp := ooklaPing, ooklaDownload, ooklaUpload
	ooklaPing = func(ctx context.Context, sv *ookla.Server, cb func(time.Duration)) error {
		cb(10 * time.Millisecond)
		sv.Latency = 10 * time.Millisecond
		return nil
	}
	ooklaDownload = func(ctx context.Context, sv *ookla.Server) error { sv.DLSpeed = 1e7; return nil }
	ooklaUpload = func(ctx context.Context, sv *ookla.Server) error { sv.ULSpeed = 1e7; return nil }
	t.Cleanup(func() { ooklaPing, ooklaDownload, ooklaUpload = oldPing, oldDown, oldUp })
}

// A run whose loss probe yielded data must record WHICH path it sampled: the
// Ookla analyzer sends UDP client->server, so a successful probe is "up".
// Stored without it, loss on an asymmetric link is ambiguous forever.
func TestOoklaLossProbeRecordsUpstreamDirection(t *testing.T) {
	stubOoklaTransfers(t)
	oldLoss := ooklaLoss
	ooklaLoss = func(ctx context.Context, srv *ookla.Server) *float64 { v := 2.5; return &v }
	t.Cleanup(func() { ooklaLoss = oldLoss })

	o := &Ookla{}
	srv := &ookla.Server{ID: "loss-dir", Context: ookla.New()}
	res, err := o.measure(context.Background(), srv, "both", 0)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if res.PacketLoss == nil || *res.PacketLoss != 2.5 {
		t.Fatalf("PacketLoss = %v, want 2.5", res.PacketLoss)
	}
	if res.UDPDirection != "up" {
		t.Errorf("UDPDirection = %q, want up - the loss probe sampled the upstream path", res.UDPDirection)
	}
	// Stubbed transfers made no real connection, so the family is honestly
	// unknown - populated only from a REAL recorded connection, never config.
	if res.IPFamily != "" {
		t.Errorf("IPFamily = %q, want empty with no real transfer connection recorded", res.IPFamily)
	}
}

// No sample, no direction: a failed (or disabled) probe must leave
// UDPDirection empty, exactly like the pre-fix rows.
func TestOoklaLossProbeFailureLeavesDirectionEmpty(t *testing.T) {
	stubOoklaTransfers(t)
	oldLoss := ooklaLoss
	ooklaLoss = func(ctx context.Context, srv *ookla.Server) *float64 { return nil }
	t.Cleanup(func() { ooklaLoss = oldLoss })

	o := &Ookla{}
	srv := &ookla.Server{ID: "loss-none", Context: ookla.New()}
	res, err := o.measure(context.Background(), srv, "both", 0)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if res.PacketLoss != nil || res.UDPDirection != "" {
		t.Errorf("loss=%v dir=%q, want nil/empty - a probe that never sampled has no direction", res.PacketLoss, res.UDPDirection)
	}

	off := &Ookla{LossFn: func() bool { return false }}
	srv2 := &ookla.Server{ID: "loss-off", Context: ookla.New()}
	res2, err := off.measure(context.Background(), srv2, "both", 0)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if res2.UDPDirection != "" {
		t.Errorf("UDPDirection = %q with the probe turned off, want empty", res2.UDPDirection)
	}
}

// The family comes from a REAL recorded connection: a run whose upload rode
// actual loopback sockets (the offline harness from upload_na_test.go, healthy
// mode) records "4" because 127.0.0.1 is what the transfer genuinely dialed.
func TestOoklaMeasureRecordsFamilyFromRealConnection(t *testing.T) {
	res, err := runNACase(t, &naServer{})
	if err != nil {
		t.Fatalf("healthy run failed: %v", err)
	}
	if res.UploadMbps <= 0 {
		t.Fatalf("UploadMbps = %v, want > 0 on the healthy harness", res.UploadMbps)
	}
	if res.IPFamily != "4" {
		t.Errorf("IPFamily = %q, want 4 - the upload's real connections were IPv4 loopback", res.IPFamily)
	}
}

// The classifier itself: families only from IP literals, v4-mapped counts as
// IPv4 on the wire, disagreement across connections is "mixed", and a
// connection to the configured proxy is excluded - its family describes the
// local hop, not the measured path.
func TestConnFamiliesClassification(t *testing.T) {
	t.Run("vocabulary", func(t *testing.T) {
		cases := []struct {
			name    string
			remotes []string
			want    string
		}{
			{"nothing recorded", nil, ""},
			{"v4 only", []string{"203.0.113.9:8080", "203.0.113.9:8080"}, "4"},
			{"v6 only", []string{"[2001:db8::7]:8080"}, "6"},
			{"v4-mapped is IPv4 on the wire", []string{"[::ffff:203.0.113.9]:8080"}, "4"},
			{"both families is mixed", []string{"203.0.113.9:8080", "[2001:db8::7]:8080"}, "mixed"},
			{"non-literal records nothing", []string{"speedtest.example:8080"}, ""},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				rec := &connFamilies{}
				for _, r := range c.remotes {
					rec.note(r)
				}
				if got := rec.family(); got != c.want {
					t.Errorf("family() = %q, want %q", got, c.want)
				}
			})
		}
	})
	t.Run("configured proxy hop is not the path's family", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTP_PROXY", "http://198.51.100.20:3128")
		rec := &connFamilies{}
		rec.note("198.51.100.20:3128") // the proxy CONNECT hop
		if got := rec.family(); got != "" {
			t.Errorf("family() = %q, want empty - the proxy hop says nothing about the path beyond it", got)
		}
	})
}
