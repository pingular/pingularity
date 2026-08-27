package main

import (
	"context"
	"testing"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// newSavedCoordFn is the only reader of the saved picker list (the
// speed_servers setting) outside the picker: a starred server's catalogue pair,
// for a pinned best-of run the live catalogue could not place. Pinned against the real controller because the 0,0 the
// picker stores for a server starred from a by-ID reply has to read as "no
// coordinate" here, or such a run would centre on the Gulf of Guinea.
func TestSavedCoordFnReadsTheStarredCoordinate(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := settings.New(ctx, st, settings.Values{SpeedServers: []settings.SavedServer{
		{ID: "1993", Sponsor: "EBOX", Name: "Montreal, QC", Lat: 45.5, Lon: -73.5},
		{ID: "7", Sponsor: "ByID", Name: "Nowhere"}, // starred from a by-ID reply: no coordinate
	}})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	fn := newSavedCoordFn(set)
	if lat, lon, ok := fn("1993"); !ok || lat != 45.5 || lon != -73.5 {
		t.Errorf("fn(1993) = %v,%v,%v; want the starred pair", lat, lon, ok)
	}
	if _, _, ok := fn("7"); ok {
		t.Error("a row starred without a coordinate must read as none, not as 0,0")
	}
	if _, _, ok := fn("404"); ok {
		t.Error("a server not on the list has no saved coordinate")
	}
	// Live: a star made after boot reaches the next run without a restart.
	if _, err := set.Update(ctx, settings.Patch{SpeedServers: []settings.SavedServer{{ID: "8", Lat: 1, Lon: 2}}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if lat, lon, ok := fn("8"); !ok || lat != 1 || lon != 2 {
		t.Errorf("fn(8) after a live star = %v,%v,%v; want 1,2", lat, lon, ok)
	}
	if _, _, ok := fn("1993"); ok {
		t.Error("an unstarred server must no longer answer")
	}
}
