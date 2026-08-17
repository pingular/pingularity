package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestOnlyExplicitInputPersistsAccess pins the rule the removed container
// grandfather broke: main may persist an access decision in exactly ONE place,
// reconcileAccess, and only from an EXPLICITLY passed -access /
// PINGULARITY_ACCESS. Every other route - a store's age, a missing birth
// marker, container detection, the network layout - may advise, never write,
// because each of those is an inference and a wrong inference opens an
// unauthenticated dashboard to the LAN silently.
//
// A behavioral test cannot cover this: it can only check the paths it thought
// to build a fixture for, and the defect was precisely a path (stores born
// private under a build too old to record their birth) nobody had a fixture
// for. Reading the source catches the next such writer wherever it is added.
func TestOnlyExplicitInputPersistsAccess(t *testing.T) {
	const setter = "SetAccessLocalOnly"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// enclosing maps a position back to the top-level func it sits in, so a
	// violation names the function an operator's access can be written from.
	type call struct {
		fn  string
		pos string
	}
	var calls []call
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != setter {
				return true
			}
			calls = append(calls, call{fn: name, pos: fset.Position(sel.Pos()).String()})
			return true
		})
	}

	if len(calls) == 0 {
		t.Fatalf("no %s call found in main.go; reconcileAccess is the recovery path for a container locked out of its own published port and must keep persisting explicit operator input", setter)
	}
	for _, c := range calls {
		if c.fn != "reconcileAccess" {
			t.Errorf("%s: %s is called from %s; only reconcileAccess (explicit -access/PINGULARITY_ACCESS) may persist an access decision - an inferred one silently exposes the dashboard",
				c.pos, setter, c.fn)
		}
	}

	// The removed migration by name, so a revert cannot come back quietly.
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if strings.Contains(strings.ToLower(fn.Name.Name), "grandfather") {
			t.Errorf("%s exists again: the container access grandfather persisted network-reachable access on an inference that cannot distinguish a container born private under a build too old to record its birth from a 0.61-or-earlier one", fn.Name.Name)
		}
	}
}
