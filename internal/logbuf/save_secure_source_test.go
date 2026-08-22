package logbuf

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// SaveFile's write path must secure the scratch file between creating it and
// renaming it into place, and on Windows nothing running can check that: the
// create mode is ignored there, a new file inherits the parent's ACEs, and a
// supported layout puts this beside the database in C:\ProgramData, whose
// inherited ACEs grant BUILTIN\Users read of the unmasked log lines. osperm
// cannot read a DACL back either, so no test there can assert on the result.
//
// On Unix, TestSaveFileSecuresTheScratchFileAgainstTheUmask masks a bit off
// os.CreateTemp's 0600 so only the securing call can put it back; it carries
// //go:build unix, and without that trick the mode CreateTemp sets is already the
// mode the call would set, so an ordinary save shows nothing.
//
// So reading the source is what is left, as it is elsewhere in this repo for
// behaviour nothing can observe from outside - and no build tag here on purpose,
// since a guard that skipped on Windows would be missing from the one runner
// nothing else covers. BEFORE the rename, not merely present: applied after it,
// the snapshot is briefly readable under its final name.
func TestSaveFileSecuresTheScratchFileBeforePublishingIt(t *testing.T) {
	const src = "logbuf.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	var body *ast.BlockStmt
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if ok && body == nil && fn.Recv != nil && fn.Name.Name == "SaveFile" && fn.Body != nil {
			body = fn.Body
		}
		return body == nil
	})
	if body == nil {
		t.Fatalf("no SaveFile method in %s: this guard works by reading that function's shape, "+
			"so it is stale and is now guarding nothing", src)
	}

	// firstAfter reports where the first call to pkg.name after `after` sits, or
	// NoPos. Position, not argument name, is what separates the write path's
	// securing call from the skip path's osperm.SecureFile(path) higher up in the
	// same function. It skips function literals: `fail` is a closure between the
	// create and the rename, and a securing call inside it would satisfy the
	// ordering while running only on the error paths.
	firstAfter := func(pkg, name string, after token.Pos) token.Pos {
		at := token.NoPos
		ast.Inspect(body, func(n ast.Node) bool {
			if n == nil || at.IsValid() {
				return false
			}
			if _, isClosure := n.(*ast.FuncLit); isClosure {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != name {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg && call.Pos() > after {
				at = call.Pos()
			}
			return true
		})
		return at
	}

	created := firstAfter("os", "CreateTemp", token.NoPos)
	if !created.IsValid() {
		t.Fatalf("SaveFile no longer creates its scratch file with os.CreateTemp: this guard is "+
			"anchored on that call, so it is stale and is now guarding nothing (%s)", src)
	}
	renamed := firstAfter("os", "Rename", created)
	if !renamed.IsValid() {
		t.Fatalf("SaveFile no longer renames its scratch file into place after creating it at %s: "+
			"this guard checks the securing call is ordered before that rename, so it is stale "+
			"and is now guarding nothing", fset.Position(created))
	}
	secured := firstAfter("osperm", "SecureFile", created)
	if !secured.IsValid() {
		t.Fatalf("SaveFile creates its scratch file at %s and never secures it: os.CreateTemp's "+
			"0600 is IGNORED on Windows, where a new file inherits the parent's ACEs - beside the "+
			"database in C:\\ProgramData those grant BUILTIN\\Users read of the snapshot's "+
			"unmasked log lines. osperm.SecureFile is the only thing that writes this file's own "+
			"DACL, and no test can observe that it ran, which is why %s is read instead",
			fset.Position(created), src)
	}
	if secured > renamed {
		t.Errorf("SaveFile secures the scratch file at %s, AFTER the rename at %s: the snapshot is "+
			"then briefly readable by other accounts under its final name",
			fset.Position(secured), fset.Position(renamed))
	}
}
