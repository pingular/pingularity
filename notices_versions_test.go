package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// noticesHeading matches one section heading in THIRD-PARTY-NOTICES.md:
// "## <module path> <version>", the line above that dependency's reproduced
// licence. Anchored per line so a path or version inside a licence body cannot
// be mistaken for a heading.
var noticesHeading = regexp.MustCompile(`(?m)^## (\S+) (v\S+)[ \t]*$`)

// TestThirdPartyNoticesMatchGoMod holds THIRD-PARTY-NOTICES.md to the module
// graph it claims to describe. That file is maintained BY HAND - one heading
// per dependency with its licence reproduced in full underneath - and nothing
// regenerates it, yet it ships to users through every channel: the release
// archives carry it beside README and LICENSE (.goreleaser.yaml:29) and the
// deb/rpm install it at /usr/share/doc/pingularity/THIRD-PARTY-NOTICES.md
// (.goreleaser.yaml:68-69). So a dependency bump that edits go.mod and forgets
// the heading ships an attribution naming a version the binary does not
// contain. That is not hypothetical - it is the drift this test was written
// after, where the speedtest-go heading sat at v1.7.10 while go.mod pinned
// v1.7.11.
//
// The check runs both ways round on purpose. A module in go.mod with no section
// is a dependency shipping with its licence unreproduced; a section with no
// module in go.mod is an attribution for something no longer linked in. Either
// is a defect in the same file, and only the pair of them pins it.
//
// What this does NOT check is the licence TEXT under a heading. Whether a bump
// changed the licence still needs a human to read it (for the v1.7.10 -> v1.7.11
// bump above, upstream's LICENSE was byte-identical). This test only makes the
// heading impossible to leave behind.
func TestThirdPartyNoticesMatchGoMod(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	root := filepath.Dir(thisFile) // this file lives at the module root

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}

	required := requiredModules(t, read("go.mod"))
	if len(required) == 0 {
		t.Fatal("parsed no requirements out of go.mod - the parser has lost the file's shape, and this test would pass on anything")
	}

	documented := map[string]string{}
	for _, m := range noticesHeading.FindAllStringSubmatch(read("THIRD-PARTY-NOTICES.md"), -1) {
		path, version := m[1], m[2]
		if prev, dup := documented[path]; dup {
			t.Errorf("THIRD-PARTY-NOTICES.md has two sections for %s (%s and %s) - one module, one section", path, prev, version)
		}
		documented[path] = version
	}

	for path, version := range required {
		switch got, ok := documented[path]; {
		case !ok:
			t.Errorf("go.mod requires %s %s but THIRD-PARTY-NOTICES.md has no section for it - add \"## %s %s\" with that release's licence reproduced in full",
				path, version, path, version)
		case got != version:
			t.Errorf("THIRD-PARTY-NOTICES.md says %s %s but go.mod pins %s - update the heading, and check whether the licence text under it changed between the two releases",
				path, got, version)
		}
	}
	for path, version := range documented {
		if _, ok := required[path]; !ok {
			t.Errorf("THIRD-PARTY-NOTICES.md carries a section for %s %s, which go.mod no longer requires - drop it, or restore the dependency",
				path, version)
		}
	}
}

// goDirective matches go.mod's language floor - "go 1.27.0", or "go 1.27".
// Anchored per line AND to end-of-line so a tab-indented require inside a
// require block ("\tgithub.com/... v1.2.3") can never be read as the directive.
// The trailing-comment branch is not hypothetical tidiness: `go 1.27.0 // see
// RELEASING.md` is legal go.mod, and without it this guard fails the build on a
// perfectly good edit while claiming the parser "has lost the file's shape".
// The `^go ` prefix is what keeps a tab-indented require line out.
var goDirective = regexp.MustCompile(`(?m)^go (\d+\.\d+(?:\.\d+)?)[ \t]*(?://.*)?$`)

// readmeGoFloor matches the Quick start's build prerequisite, "requires Go
// 1.27.0+". Every occurrence is checked, so a second copy added elsewhere in
// the README is covered without touching this test.
var readmeGoFloor = regexp.MustCompile(`requires Go (\d+\.\d+(?:\.\d+)?)\+`)

// TestREADMEGoVersionMatchesGoMod holds README.md's build prerequisite to
// go.mod's `go` directive, for the same reason as the notices test above: a
// hand-maintained mirror of go.mod drifts. This one sits in the four most-read
// lines in the repository and nothing regenerates it, while every CI workflow
// takes its toolchain from `go-version-file: go.mod` - so the mirror can be
// wrong with the whole suite green.
//
// The pair has been hand-carried once and dropped once: 510e9d3 carried
// 1.25.12 -> 1.25.13 in both files, then d8fe73f bumped go.mod to 1.27.0 and
// left the README on 1.25.13 - and that commit's own subject was "Build with
// Go 1.27", which is how little a human reviewer catches this line.
//
// The cost is invisible under the default GOTOOLCHAIN=auto, which quietly
// downloads the newer toolchain and builds anyway. Under GOTOOLCHAIN=local (how
// distro-packaged Go ships) or on an offline builder, a contributor who
// installed exactly what the README asked for gets "go.mod requires go >=
// 1.27.0" from the command the README just gave them.
func TestREADMEGoVersionMatchesGoMod(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	root := filepath.Dir(thisFile) // this file lives at the module root

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}

	want := goDirective.FindStringSubmatch(read("go.mod"))
	if want == nil {
		t.Fatal("no `go <version>` directive found in go.mod - the parser has lost the file's shape, and this test would pass on anything")
	}
	found := readmeGoFloor.FindAllStringSubmatch(read("README.md"), -1)
	if len(found) == 0 {
		t.Fatal("README.md no longer says \"requires Go <version>+\" - the Quick start's prerequisite was reworded, so this test can no longer see the number it holds to go.mod")
	}
	for _, m := range found {
		if m[1] != want[1] {
			t.Errorf("README.md says %q but go.mod requires go %s - update the Quick start comment; under GOTOOLCHAIN=local a build following the README fails outright",
				m[0], want[1])
		}
	}
}

// requiredModules returns the module path -> version pairs of every require in
// a go.mod, direct and indirect alike (both are linked into the binary, so both
// need attribution). Only require blocks and single-line requires are read, so a
// replace/exclude/retract directive cannot be mistaken for a dependency.
func requiredModules(t *testing.T, gomod string) map[string]string {
	t.Helper()
	req := map[string]string{}
	inBlock := false
	for _, raw := range strings.Split(gomod, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "//"); i >= 0 { // "// indirect" and friends
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "require (":
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case inBlock:
			if f := strings.Fields(line); len(f) == 2 {
				req[f[0]] = f[1]
			}
		case strings.HasPrefix(line, "require "):
			if f := strings.Fields(line); len(f) == 3 {
				req[f[1]] = f[2]
			}
		}
	}
	return req
}
