package web

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// A daemon whose own birth stamp failed keeps a pending claim: "the database I
// read was empty, let me finish recording that". A RESTORE replaces the rows
// that claim is about, so the import path must void it - otherwise the very
// next settings write the import performs completes the stamp, and a restored
// pre-0.62 backup ends up marked as born under this version.
//
// That is worse than the missing marker it replaces: an absent marker reads as
// "unknown" and fails closed (the container ambiguity warning fires), while a
// false one is believed - by the warning that now stays silent, and by any
// future upgrade keyed on it.
//
// This test drives the real handler, because the unit test in internal/settings
// proves only that ForgetBirthWitness works when called; deleting the CALL from
// the import path leaves that test green.
func TestImportVoidsAPendingBirthStamp(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Birth witnessed, stamp lost: writes are refused for the whole of New, which
	// outlasts both of its adjacent attempts.
	if _, err := st.DB().ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		t.Fatalf("deny writes: %v", err)
	}
	set, err := settings.New(ctx, st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, AccessLocalOnly: false,
	}, settings.WithBornVersion("0.62.0-test"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, "PRAGMA query_only = OFF"); err != nil {
		t.Fatalf("allow writes: %v", err)
	}
	if set.BornMarkerErr() == nil {
		t.Fatal("precondition: the birth stamp must have failed, or there is no pending claim to void")
	}
	if all, err := st.AllSettings(ctx); err != nil {
		t.Fatalf("AllSettings: %v", err)
	} else if _, ok := all[settings.KeyInstallBornVersion]; ok {
		t.Fatal("precondition: the store must be unmarked at this point")
	}

	// Restore a MARKERLESS backup - what a pre-0.62 export looks like. Its config
	// rows are settings writes, which is exactly what would otherwise complete
	// the pending stamp.
	s := newTestServerWith(t, st, set)
	backup := `{"pingularity_export":1,"config":[{"key":"retention_s","value":"3600"}]}`
	if rr := importBackup(t, s, "config=1", backup); rr.Code != 200 {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}

	all, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if v, ok := all[settings.KeyInstallBornVersion]; ok {
		t.Fatalf("the restored install was stamped %q: this daemon witnessed the birth of rows the restore has replaced, so the marker claims a birth nobody saw for the data now in the store - and a pre-0.62 restore would stop being reported as ambiguous", v)
	}
}
