package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"github.com/pingular/pingularity/internal/store"
)

// A first speedtest that FAILS after moving bytes leaves a usage-accounting row
// behind - flagged, invisible to every measurement read, there only to bill the
// traffic. Counting raw speed rows mistook that row for history, so the next
// run believed it had a baseline and skipped the first-run ping-only path: the
// very first REAL measurement on the install would be judged against throughput
// nothing had ever vetted. The install still has no history, and the predicate
// must say so.
func TestFailedFirstSpeedtestIsNotSpeedHistory(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Exactly what the scheduler's failure path writes (recordFailedUsage): no
	// speed, no ping, no verdict - only the bytes the doomed attempt spent.
	down, up := int64(125_000_000), int64(4_000_000)
	if err := st.InsertSpeed(ctx, store.SpeedSample{
		TS: 1_700_000_000, Server: "Somewhere", Trigger: "startup", Engine: "ookla",
		Failed: true, DownBytes: &down, UpBytes: &up,
	}); err != nil {
		t.Fatalf("insert accounting row: %v", err)
	}

	// The two counts genuinely disagree on this store - that disagreement IS the
	// bug, so pin it rather than assume it.
	counts, err := st.TableCounts(ctx)
	if err != nil {
		t.Fatalf("TableCounts: %v", err)
	}
	if counts["speed"] == 0 {
		t.Fatal("precondition: the accounting row must be in the speed table")
	}
	n, err := st.SpeedCount(ctx)
	if err != nil {
		t.Fatalf("SpeedCount: %v", err)
	}
	if n != 0 {
		t.Fatalf("precondition: SpeedCount counts measurements only, got %d", n)
	}

	if newPriorDataFn(st)() {
		t.Errorf("a failed first speedtest (only an accounting row, raw table count %d) counted as speed history: the next run skips the first-run ping-only bootstrap and judges the install's first real measurement against a baseline that was never measured", counts["speed"])
	}

	// A real measurement IS history - the predicate must still flip, or every
	// run forever would take the first-run path.
	if err := st.InsertSpeed(ctx, store.SpeedSample{
		TS: 1_700_000_100, DownMbps: 94.2, UpMbps: 11.7, PingMS: 12.5, Server: "Somewhere",
	}); err != nil {
		t.Fatalf("insert measurement: %v", err)
	}
	if !newPriorDataFn(st)() {
		t.Error("a recorded measurement must count as speed history; otherwise every run re-takes the first-run ping-only path")
	}
}

// The test above proves the PREDICATE and nothing about whether the daemon uses
// it. Replacing main's `tester.PriorDataFn = newPriorDataFn(p.store)` with
// `func() bool { return true }` left this whole package green (verified), and
// an install wired that way goes straight back to ranking its first best-of
// round on throughput nothing has vetted - the exact defect the predicate
// exists to prevent, with every test still passing.
//
// Nothing observable proves the wiring at runtime: run() assembles the tester
// inside a call that blocks until shutdown, PriorDataFn is consulted only from
// inside a speedtest, and its effect (which server a best-of round keeps) needs
// a real network round to show. So read the assembly itself - the same approach,
// for the same reason, as TestOnlyExplicitInputPersistsAccess.
func TestMainWiresPriorDataFnToTheStore(t *testing.T) {
	const field = "PriorDataFn"
	const want = "newPriorDataFn(p.store)"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var installed []string
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != field || i >= len(as.Rhs) {
				continue
			}
			installed = append(installed, types.ExprString(as.Rhs[i]))
		}
		return true
	})

	if len(installed) != 1 {
		t.Fatalf("main.go assigns .%s %d times %v, want exactly 1: the engine reads it live, so a second writer decides the first-run rule by line order", field, len(installed), installed)
	}
	if installed[0] != want {
		t.Errorf("main.go installs .%s = %s, want %s - the predicate this file's other test pins, reading the daemon's OWN store. Anything else and a fresh install's first best-of round is ranked on throughput no measurement has vetted (or counted out of a different database).", field, installed[0], want)
	}
}
