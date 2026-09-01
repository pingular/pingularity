package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// moduleRoot locates the repository root from this file's own path, so these
// tests do not depend on the working directory a runner happens to use.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	return filepath.Dir(thisFile)
}

func readRoot(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// licenceFiles are the two documents every distribution channel must carry: the
// project's own licence, and the third-party notices for everything linked into
// the binary.
var licenceFiles = []string{"LICENSE", "THIRD-PARTY-NOTICES.md"}

// TestImagesShipTheLicenceFiles holds the container images to the same
// attribution the other channels already provide. The archives ship these two
// files beside the binary (.goreleaser.yaml archives.files) and the deb/rpm
// install them under /usr/share/doc/pingularity (nfpms.contents), but the images
// shipped neither for their whole history: both Dockerfiles COPY'd only the
// binary, and org.opencontainers.image.licenses="MIT" is a label declaring
// Pingularity's own licence, not a reproduction of anyone else's.
//
// That matters because an image is a binary distribution like any other, and the
// binary inside it carries BSD-3 code (golang.org/x/crypto, golang.org/x/sys,
// speedtest-go, the modernc musl-derived libc) plus an OFL font, each of which
// asks for its notice to travel with the distribution.
//
// The two halves are checked together on purpose. goreleaser stages only the
// built binaries into the docker build context, so a COPY without a matching
// extra_files entry fails the build - loudly, which is fine - while an
// extra_files entry without a COPY silently ships an image with no notices,
// which is the failure worth a test.
func TestImagesShipTheLicenceFiles(t *testing.T) {
	for _, dockerfile := range []string{"Dockerfile", "Dockerfile.iperf"} {
		body := readRoot(t, dockerfile)
		for _, want := range licenceFiles {
			// The COPY must name the file and land it under the conventional
			// package-documentation path, matching where the deb/rpm put it.
			re := regexp.MustCompile(`(?m)^COPY\b[^\n]*\b` + regexp.QuoteMeta(want) + `\b[^\n]*/usr/share/doc/pingularity/`)
			if !re.MatchString(body) {
				t.Errorf("%s: no COPY placing %s into /usr/share/doc/pingularity/ - the image would ship the binary with that attribution missing",
					dockerfile, want)
			}
		}
	}

	// Every dockers_v2 builder must stage both files into its build context.
	cfg := readRoot(t, ".goreleaser.yaml")
	dockersV2 := cfg[strings.Index(cfg, "\ndockers_v2:"):]
	if !strings.HasPrefix(dockersV2, "\ndockers_v2:") {
		t.Fatal(".goreleaser.yaml has no dockers_v2 block - this test's premise is gone")
	}
	builders := regexp.MustCompile(`(?m)^  - id: (\S+)`).FindAllStringSubmatch(dockersV2, -1)
	if len(builders) == 0 {
		t.Fatal("parsed no dockers_v2 builders - the test's matching has rotted")
	}
	for _, b := range builders {
		id := b[1]
		start := strings.Index(dockersV2, "  - id: "+id)
		end := len(dockersV2)
		if next := strings.Index(dockersV2[start+1:], "\n  - id: "); next >= 0 {
			end = start + 1 + next
		}
		block := dockersV2[start:end]
		for _, want := range licenceFiles {
			if !regexp.MustCompile(`(?m)^\s*-\s*` + regexp.QuoteMeta(want) + `\s*$`).MatchString(block) {
				t.Errorf("dockers_v2 builder %q does not list %s in extra_files - the Dockerfile's COPY of it will fail the build",
					id, want)
			}
		}
	}
}

// fontFace matches the dashboard's embedded @font-face: the family it declares,
// and the fact that the face is a data: URI rather than a fetched URL.
var fontFace = regexp.MustCompile(`@font-face\{font-family:'([^']+)'[^}]*src:url\(data:font/`)

// TestEmbeddedFontIsAttributed holds the notices to whatever font the dashboard
// actually embeds. internal/web/ui/index.html carries a base64 woff2 inside a
// data: URI, and web.go compiles that file into the binary with go:embed - so
// the font ships through every channel, in every image and package.
//
// Quicksand is under the SIL Open Font License, whose clause 2 requires that
// every copy of the Font Software carry the copyright notice and the licence.
// The font shipped for a long time with neither, because THIRD-PARTY-NOTICES.md
// was built from go.mod and a font is not a Go module - nothing in the toolchain
// could have noticed it.
//
// The family name is read out of the CSS rather than hard-coded here, so
// swapping the embedded face for a different one fails this test until its
// licence is reproduced too.
func TestEmbeddedFontIsAttributed(t *testing.T) {
	ui := readRoot(t, filepath.Join("internal", "web", "ui", "index.html"))
	m := fontFace.FindStringSubmatch(ui)
	if m == nil {
		// No embedded face is a legitimate state (a rewrite could drop it), but
		// it invalidates this test's premise, so say so rather than pass quietly.
		if strings.Contains(ui, "data:font/") {
			t.Fatal("index.html embeds a data: font this test's @font-face pattern no longer recognises - recheck the pattern")
		}
		t.Skip("the dashboard embeds no font")
	}
	family := m[1]
	notices := readRoot(t, "THIRD-PARTY-NOTICES.md")
	if !strings.Contains(notices, family) {
		t.Errorf("index.html embeds the %q font but THIRD-PARTY-NOTICES.md never names it - add a section reproducing its licence", family)
	}
	// The OFL is the licence Quicksand ships under; if the embedded face changes
	// to something under a different licence, the family check above fires first
	// and this line is updated with it.
	if !strings.Contains(notices, "SIL OPEN FONT LICENSE Version 1.1") {
		t.Errorf("THIRD-PARTY-NOTICES.md names %q but does not reproduce the SIL Open Font License, which that font requires to travel with every copy", family)
	}
}
