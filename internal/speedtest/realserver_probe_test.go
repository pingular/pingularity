package speedtest

import (
	"context"
	"os"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// TestRealServerProbe hits LIVE Ookla servers. Opt-in only (PROBE_REAL=1) so it
// never runs in CI or burns a contributor's bandwidth. Capture time is cut to 3s
// to keep the data cost small.
func TestRealServerProbe(t *testing.T) {
	if os.Getenv("PROBE_REAL") != "1" {
		t.Skip("set PROBE_REAL=1 to probe live Ookla servers")
	}
	cases := []struct{ name, base string }{
		// LEGACY: the `url` field speedtest-go uses verbatim - now 307s on migrated servers.
		{"59030 #17  LEGACY url", "http://speedtest3.b4rn.org.uk:8080"},
		{"59030 #17  FIXED  host", "https://speedtest3.b4rn.org.uk.prod.hosts.ooklaserver.net:8080"},
		{"72887 #18  LEGACY url", "http://speedtest.xpert-tic.com:8080"},
		{"72887 #18  FIXED  host", "https://speedtest.xpert-tic.com.prod.hosts.ooklaserver.net:8080"},
		{"1993  ctrl LEGACY url", "http://speedtest.ebox.ca:8080"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := ookla.New()
			srv, err := client.CustomServer(c.base)
			if err != nil {
				t.Fatal(err)
			}
			srv.Context.SetCaptureTime(3 * time.Second)

			oldPing, oldDown := ooklaPing, ooklaDownload
			ooklaPing = func(ctx context.Context, sv *ookla.Server, cb func(time.Duration)) error {
				cb(20 * time.Millisecond)
				sv.Latency = 20 * time.Millisecond
				return nil
			}
			ooklaDownload = func(ctx context.Context, sv *ookla.Server) error { sv.DLSpeed = 1e7; return nil }
			t.Cleanup(func() { ooklaPing, ooklaDownload = oldPing, oldDown })

			o := &Ookla{LossFn: func() bool { return false }}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			res, err := o.measure(ctx, srv, "both", 0)
			t.Logf("PROBE %-24s UploadMbps=%.1f UploadBytes=%d err=%v",
				c.name, res.UploadMbps, res.UploadBytes, err)
		})
	}
}

// TestCurrentEndpointIntegration proves the FIX rather than the URL: it walks the
// real pinned-server path - fetch the catalogue entry by ID, apply the rewrite
// exactly as RunReason now does - and measures through it. Before the rewrite
// this is issue #17/#18; after it, a number.
func TestCurrentEndpointIntegration(t *testing.T) {
	if os.Getenv("PROBE_REAL") != "1" {
		t.Skip("set PROBE_REAL=1 to probe live Ookla servers")
	}
	for _, id := range []string{"59030", "72887"} {
		t.Run("id"+id, func(t *testing.T) {
			client := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
			srv, err := client.FetchServerByIDContext(context.Background(), id)
			if err != nil {
				t.Skipf("catalogue fetch failed (network): %v", err)
			}
			before := srv.URL
			currentEndpoint(srv) // list-derived servers: rewrite from the Host field
			if srv.URL == before {
				// by-ID returns Host="" - production now probes and follows the hop
				if st := probeEndpoint(context.Background(), srv); st != endpointOK {
					t.Logf("  probe verdict: %v", st)
				}
			}
			t.Logf("  %s -> %s", before, srv.URL)
			if srv.Host != "" && before == srv.URL {
				t.Logf("  (no-op: server has not migrated)")
			}
			srv.Context.SetCaptureTime(3 * time.Second)

			oldPing, oldDown := ooklaPing, ooklaDownload
			ooklaPing = func(ctx context.Context, sv *ookla.Server, cb func(time.Duration)) error {
				cb(20 * time.Millisecond)
				sv.Latency = 20 * time.Millisecond
				return nil
			}
			ooklaDownload = func(ctx context.Context, sv *ookla.Server) error { sv.DLSpeed = 1e7; return nil }
			t.Cleanup(func() { ooklaPing, ooklaDownload = oldPing, oldDown })

			o := &Ookla{LossFn: func() bool { return false }}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			res, err := o.measure(ctx, srv, "both", 0)
			if err != nil {
				t.Fatalf("FIX FAILED: %v", err)
			}
			t.Logf("  MEASURED %.1f Mbps up via %s", res.UploadMbps, srv.URL)
		})
	}
}
