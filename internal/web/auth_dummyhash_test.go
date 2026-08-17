package web

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// The dummy hash is what makes a wrong username cost the same as a wrong
// password, so it has to stay a real bcrypt hash at the same cost the login
// password is stored at. It is computed lazily (see dummyHash), and the failure
// mode of a lazy value is that a refactor quietly turns it into a no-op: a nil
// or malformed slice still compiles and still "compares", but
// CompareHashAndPassword rejects it in microseconds without hashing anything,
// which silently restores the username oracle the dummy exists to close.
func TestDummyHashIsAValidBcryptHashAtLoginCost(t *testing.T) {
	h := dummyHash()
	if len(h) == 0 {
		t.Fatal("dummyHash() is empty: the wrong-username branch has nothing to burn a compare against")
	}

	// A nil or truncated value fails here with ErrHashTooShort instead, which is
	// exactly the no-op a broken refactor would leave behind.
	err := bcrypt.CompareHashAndPassword(h, []byte("not the throwaway string"))
	if !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		t.Fatalf("compare against dummyHash() = %v, want ErrMismatchedHashAndPassword (a real hash that did the work)", err)
	}

	// Equal cost is the whole point: bcrypt time is set by the cost embedded in
	// the hash, so a dummy at a different cost than the stored password hash
	// makes the two branches take visibly different times.
	dummyCost, err := bcrypt.Cost(h)
	if err != nil {
		t.Fatalf("bcrypt.Cost(dummyHash()) = %v, want a parseable hash", err)
	}
	stored, err := hashPassword("a password set through the normal path")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	storedCost, err := bcrypt.Cost([]byte(stored))
	if err != nil {
		t.Fatalf("bcrypt.Cost(hashPassword(...)) = %v", err)
	}
	if dummyCost != storedCost {
		t.Fatalf("dummy hash cost %d != stored password hash cost %d: the wrong-username branch no longer costs what the wrong-password branch costs", dummyCost, storedCost)
	}

	// sync.OnceValue must hand back the same hash every time. A refactor that
	// re-generates per call would put a 40ms+ bcrypt generate on every failed
	// login, on top of the compare.
	if again := dummyHash(); string(again) != string(h) {
		t.Fatal("dummyHash() returned a different hash on the second call: it is being regenerated, not cached")
	}
}

// End-to-end version of the same property: a login with an unknown username must
// take about as long as a login with the right username and a wrong password.
// This is the assertion that survives someone deleting the dummy compare from
// checkPassword altogether, which the hash-shape checks above would not notice.
func TestWrongUsernameTakesAsLongAsWrongPassword(t *testing.T) {
	s := newTestServer(t)
	// Not setPassword: that helper stores a bcrypt.MinCost hash to keep other
	// tests fast, which would make the real-username branch about 60x cheaper
	// than the DefaultCost dummy and prove nothing about production timing.
	hash, err := hashPassword("correct horse")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if err := s.settings.SetAuthPassword(context.Background(), "admin", hash); err != nil {
		t.Fatalf("SetAuthPassword: %v", err)
	}

	// Force the lazy dummy hash first so its one-time generation is not counted
	// in the samples below. Note this warm-up uses the RIGHT username, so it only
	// forces the value while checkPassword forces it unconditionally;
	// TestCheckPasswordForcesTheDummyHashUnconditionallyBeforeAnyBcryptWork is
	// what pins that, and the comment on it explains why this test cannot.
	s.checkPassword("admin", "wrong")

	// Minimum of a few runs: bcrypt work is CPU-bound, so the fastest sample is
	// the least polluted by scheduling noise on a loaded CI box.
	best := func(user, pass string) time.Duration {
		lo := time.Duration(1<<63 - 1)
		for i := 0; i < 3; i++ {
			start := time.Now()
			if s.checkPassword(user, pass) {
				t.Fatalf("checkPassword(%q, %q) accepted bad credentials", user, pass)
			}
			if d := time.Since(start); d < lo {
				lo = d
			}
		}
		return lo
	}
	wrongPass := best("admin", "wrong")
	wrongUser := best("nosuchuser", "wrong")

	// Generous margin: the two branches should be within noise of each other,
	// while a missing or invalid dummy hash returns in microseconds - three
	// orders of magnitude below the bcrypt compare it is supposed to mimic.
	if wrongUser < wrongPass/4 {
		t.Fatalf("wrong username took %v but wrong password took %v: response time is a username oracle again", wrongUser, wrongPass)
	}
}

// The property this guard pins: checkPassword forces the lazy dummy hash
// UNCONDITIONALLY, ahead of every bcrypt call. auth.go declares dummyHash as a
// sync.OnceValue, and checkPassword is its only caller outside tests, so the
// generator runs on the first login attempt that reaches it and never again: at
// most one attempt in the life of the process pays a full DefaultCost generate.
// WHICH attempt that is, is the whole security question.
// Forced from straight-line code ahead of any branch, it is simply whichever
// attempt arrives first, whatever username it used, and every attempt costs the
// same. Forced from inside the wrong-username branch instead, it is the first
// attempt carrying an UNKNOWN username: after a restart an unknown username then
// costs generate plus compare and a known one costs compare alone. That does not
// remove the oracle, it inverts it - whoever gets the first attempt in after a
// restart reads one clean username answer straight off the response time.
//
// So the assertions below are about reachability and order, not about one
// spelling: the first mention of dummyHash() must sit in a straight-line
// statement, with nothing ahead of it that can return or jump past it, and ahead
// of the first bcrypt call. This deliberately does NOT constrain where the
// username is read. Hoisting `want := s.settings.AuthUser()` above an
// unconditional force preserves behaviour - the generator only hashes a
// constant and never touches settings, so both arms still pay identical work and
// the generate still lands on whichever attempt is first - and failing that
// rearrangement would be crying wolf.
//
// This reads auth.go instead of timing a login because the generation happens at
// most once per process, so only the very first checkPassword call in the whole
// process can show the asymmetry at all. Every call after it finds the hash
// cached and both branches cost one compare, whichever placement is in the
// source. The timing test above cannot see the move even when it is the only
// test in the binary: its warm-up call uses the right username, which under a
// branch-forced placement never forces the value, and best() keeps the minimum
// of several samples, which discards the single expensive call by construction.
// Verified by making the move: that test still passed, alone in a fresh process.
//
// Reading the assembly is the same approach, for the same reason, as
// TestMainWiresPriorDataFnToTheStore in the root package.
func TestCheckPasswordForcesTheDummyHashUnconditionallyBeforeAnyBcryptWork(t *testing.T) {
	const src = "auth.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	var body *ast.BlockStmt
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if ok && body == nil && fn.Recv != nil && fn.Name.Name == "checkPassword" && fn.Body != nil {
			body = fn.Body
		}
		return body == nil
	})
	if body == nil {
		t.Fatalf("no checkPassword method in %s: this guard works by reading that function's shape, so it is stale and is now guarding nothing", src)
	}

	// find reports where the first matching node sits inside stmt, or NoPos. It
	// does not descend into function literals, because a call written inside a
	// closure does not run where it is written. Applied to TOP-LEVEL statements
	// only (see first below), so a call buried in a branch is credited to that
	// branch's own statement rather than to a line number.
	find := func(stmt ast.Stmt, want func(ast.Node) bool) token.Pos {
		at := token.NoPos
		ast.Inspect(stmt, func(n ast.Node) bool {
			if n == nil || at.IsValid() {
				return false
			}
			if _, isClosure := n.(*ast.FuncLit); isClosure {
				return false
			}
			if want(n) {
				at = n.Pos()
			}
			return true
		})
		return at
	}
	first := func(want func(ast.Node) bool) (int, token.Pos) {
		for i, stmt := range body.List {
			if at := find(stmt, want); at.IsValid() {
				return i, at
			}
		}
		return -1, token.NoPos
	}
	forcesDummy := func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := call.Fun.(*ast.Ident)
		return ok && id.Name == "dummyHash"
	}
	// Any bcrypt.X(...) call. checkPassword's two password paths both go through
	// one; the local helpers it calls first (bcryptAcquire, bcryptRelease) are
	// plain identifiers, not selectors on the package, so they do not match.
	callsBcrypt := func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "bcrypt"
	}
	// A statement that can end the call before the next one runs.
	leavesEarly := func(n ast.Node) bool {
		switch b := n.(type) {
		case *ast.ReturnStmt:
			return true
		case *ast.BranchStmt:
			return b.Tok == token.GOTO
		}
		return false
	}

	forceIdx, forcePos := first(forcesDummy)
	bcryptIdx, bcryptPos := first(callsBcrypt)
	exitIdx, exitPos := first(leavesEarly)
	line := func(p token.Pos) int { return fset.Position(p).Line }

	if forceIdx < 0 {
		t.Fatalf("checkPassword in %s never calls dummyHash(): with no bcrypt work to burn, an unknown username returns in microseconds while a known one pays a full compare, which is the plain username oracle the dummy exists to close", src)
	}
	switch body.List[forceIdx].(type) {
	case *ast.AssignStmt, *ast.ExprStmt, *ast.DeclStmt:
	default:
		t.Fatalf("the force is not unconditional: checkPassword reaches dummyHash() at %s:%d from inside a %T rather than from a straight-line statement.\n"+
			"A force some attempts skip puts the one-time generate on whichever kind of attempt happens to reach it first, so the first attempt after a restart costs a different amount depending on the username it carried.",
			src, line(forcePos), body.List[forceIdx])
	}
	if exitIdx >= 0 && exitIdx <= forceIdx {
		t.Fatalf("the force is not unconditional: checkPassword can return at %s:%d, before it reaches dummyHash() at %s:%d.\n"+
			"Attempts that take that exit never force the value, so the one-time generate lands on the first attempt that gets past it rather than on the first attempt of any kind.",
			src, line(exitPos), src, line(forcePos))
	}
	if bcryptIdx < 0 {
		t.Fatalf("no bcrypt call left in checkPassword in %s: this guard checks that the dummy hash is forced ahead of the password comparison, and it finds that comparison by looking for a bcrypt call here, so it is stale and is now guarding nothing", src)
	}
	if forceIdx >= bcryptIdx {
		t.Fatalf("the force does not precede the bcrypt work: checkPassword reaches dummyHash() at %s:%d, at or after the first bcrypt call at %s:%d.\n"+
			"Any branch chosen before the force decides which kind of attempt pays the one-time generate. It has to land on whichever login attempt is first after a restart, whatever username that attempt used, or the first attempt still leaks whether the username exists. Where the username is READ does not matter; being unconditional and ahead of every bcrypt call does.",
			src, line(forcePos), src, line(bcryptPos))
	}
}
