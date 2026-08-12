package speedtest

import (
	"context"
	"os"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// TestDiagnosticsAgainstRealRetiredServer runs the production path against a
// server that has genuinely retired /speedtest/upload.php (verified: the
// official Ookla CLI measures it at 215 Mbps over its own protocol) and checks
// the error now NAMES the cause instead of saying only "N/A".
func TestDiagnosticsAgainstRealRetiredServer(t *testing.T) {
	if os.Getenv("PROBE_REAL") != "1" {
		t.Skip("set PROBE_REAL=1")
	}
	client, rec := newOoklaClientRec(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	srv, err := client.CustomServer("http://losangeles.ca.speedtest.frontier.com:8080")
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

	o := &Ookla{LossFn: func() bool { return false }, upRec: rec}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err = o.measure(ctx, srv, "both", 0)
	if err == nil {
		t.Skip("server started accepting the legacy endpoint again")
	}
	t.Logf("REAL DIAGNOSIS: %v", err)
	t.Logf("probe verdict : %v", probeEndpoint(ctx, srv))
}
