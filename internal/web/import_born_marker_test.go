package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// install_born_version is provenance, not configuration: it records that THIS
// daemon created THIS database, and the whole point of the machinery around it
// is that only the process which witnessed the empty store may write it. An
// established store can never be stamped, a restart drops the pending witness,
// and a restore voids it.
//
// A backup route around all of that would make the rest theatre. A config
// export carrying the marker lets any destination be handed a birth it never
// had - and unlike a missing marker (which reads as "unknown" and fails closed,
// warning about ambiguous container access) a FORGED one is believed, by that
// warning and by anything later keyed on it.
func TestBornMarkerNeverLeavesOrEntersInABackup(t *testing.T) {
	ctx := context.Background()

	// SOURCE: a brand-new store, so settings.New stamps it for real.
	src := newTestServer(t)
	all, err := src.store.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if _, ok := all[settings.KeyInstallBornVersion]; !ok {
		t.Fatalf("precondition: the source store must carry a birth marker, got keys %v", len(all))
	}

	// EXPORT the config category, exactly as the Export button does.
	rr := do(t, src.Handler(), "GET", "/api/export?config=1", "")
	if rr.Code != 200 {
		t.Fatalf("export: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, settings.KeyInstallBornVersion) {
		t.Errorf("the export carries %s: a backup must not be able to hand another install this one's birth",
			settings.KeyInstallBornVersion)
	}

	// IMPORT a backup that DOES carry it (an older export, or a crafted one)
	// into an established destination that has none.
	dstStore, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { dstStore.Close() })
	if err := dstStore.SetSetting(ctx, "monitoring", "true"); err != nil {
		t.Fatalf("seed configuration: %v", err)
	}
	if err := dstStore.InsertSamples(ctx, []store.Sample{
		{TS: time.Now().Add(-24 * time.Hour), Target: "cloudflare", Family: "ipv4", LatencyMS: 9, Success: true},
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	dstSet, err := settings.New(ctx, dstStore, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, AccessLocalOnly: false,
	}, settings.WithBornVersion("0.62.0-test"))
	if err != nil {
		t.Fatalf("new settings: %v", err)
	}
	before, err := dstStore.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if _, ok := before[settings.KeyInstallBornVersion]; ok {
		t.Fatal("precondition: an ESTABLISHED destination must have no birth marker - that is what makes it ambiguous")
	}

	dst := newTestServerWith(t, dstStore, dstSet)
	backup := `{"pingularity_export":1,"config":[{"key":"` + settings.KeyInstallBornVersion + `","value":"9.9.9-forged"}]}`
	if rr := importBackup(t, dst, "config=1", backup); rr.Code != 200 {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}

	after, err := dstStore.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if v, ok := after[settings.KeyInstallBornVersion]; ok {
		t.Fatalf("the restore implanted a birth marker (%q) on an install that never had one: its provenance is now a lie, "+
			"the ambiguous-container warning goes quiet, and anything later keyed on the marker trusts it", v)
	}
}
