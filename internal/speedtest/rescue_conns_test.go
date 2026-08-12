package speedtest

import (
	"testing"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// The starvation ceiling must be derived from the connection count the run's
// transfers actually use - the snapshot in o.uc.MaxConnections that every
// freshManager reuses - not a fresh live ConnectionsFn read. If the user lowers
// the count mid-run, the live read would size the ceiling for the new count
// while the transfers still run at the snapshotted count, disabling the rescue
// and corrupting the starvation diagnosis. runConnections is the single source
// both the manager path and the rescue predicate derive from.
func TestRunConnectionsUsesSnapshotNotLiveRead(t *testing.T) {
	live := 16
	o := &Ookla{ConnectionsFn: func() int { return live }}

	// Before RunReason snapshots (o.uc still nil): the live value is the stand-in.
	if got := o.runConnections(); got != 16 {
		t.Fatalf("with o.uc nil, runConnections()=%d, want the live 16", got)
	}

	// RunReason takes its snapshot into o.uc, exactly as the production path does
	// (uc.MaxConnections = o.ConnectionsFn()).
	o.uc = &ookla.UserConfig{MaxConnections: o.ConnectionsFn()}

	// The user now saves a lower count mid-run.
	live = 1

	if got := o.runConnections(); got != 16 {
		t.Fatalf("after a mid-run change to %d, runConnections()=%d, want the snapshot 16", live, got)
	}
	// And the ceiling the rescue predicate/diagnosis use follows the snapshot: a
	// 16-stream starved upload makes ~16 attempts, which must stay <= the ceiling
	// so the starvation signature is recognised. Derived from the live 1 it would
	// be starvationCeiling(1)=5, and 16 attempts would blow past it - the bug.
	if got, want := starvationCeiling(o.runConnections()), starvationCeiling(16); got != want {
		t.Fatalf("ceiling from snapshot = %d, want %d (starvationCeiling(16))", got, want)
	}
	if bug := starvationCeiling(1); starvationCeiling(o.runConnections()) == bug {
		t.Fatalf("ceiling collapsed to the mid-run live value's %d - rescue would not fire for a 16-stream starve", bug)
	}
}
