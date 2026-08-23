package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// dockerfilePaths are the two shipped image definitions. Both are read as plain
// text: the contract under test (digest pins, lockstep, healthcheck, labels) is
// entirely in the FROM/HEALTHCHECK/LABEL lines, so no Dockerfile parser is
// needed - and a parser would only add a dependency that could disagree with
// what BuildKit reads.
var dockerfilePaths = []string{"Dockerfile", "Dockerfile.iperf"}

var fromLineRe = regexp.MustCompile(`(?m)^FROM\s+(?:--platform=\S+\s+)?(\S+)`)

// pinnedRefRe is a full image ref pinned by digest: name[:tag]@sha256:<64 hex>.
var pinnedRefRe = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// TestDockerfilesEveryFromIsDigestPinned fails if any FROM line in either
// Dockerfile references a base by floating tag alone. A digest is the only
// immutable reference; a bare tag would silently change the base under a
// rebuild, which is exactly what the pins exist to prevent.
func TestDockerfilesEveryFromIsDigestPinned(t *testing.T) {
	for _, path := range dockerfilePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		froms := fromLineRe.FindAllStringSubmatch(string(data), -1)
		if len(froms) == 0 {
			t.Fatalf("%s: no FROM lines found - the test's line matching has rotted", path)
		}
		for _, m := range froms {
			if !pinnedRefRe.MatchString(m[1]) {
				t.Errorf("%s: FROM %q is not digest-pinned (want name[:tag]@sha256:<64 hex>)", path, m[1])
			}
		}
	}
}

// TestDockerfilesDebianDigestsLockstep fails if the debian:13-slim digest
// drifts between ANY of its occurrences (one in Dockerfile, two in
// Dockerfile.iperf). The two images must build from the same base rebuild:
// half-bumped pins mean the setcap stage and the iperf final stage - or the
// two published images - carry different base layers, and nothing else in the
// build would ever report that.
func TestDockerfilesDebianDigestsLockstep(t *testing.T) {
	type pin struct {
		path   string
		digest string
	}
	var debianPins []pin
	for _, path := range dockerfilePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range fromLineRe.FindAllStringSubmatch(string(data), -1) {
			name, digest, ok := strings.Cut(m[1], "@")
			if !ok {
				continue // the pinned-FROM test reports unpinned refs
			}
			if name == "debian:13-slim" {
				debianPins = append(debianPins, pin{path, digest})
			}
		}
	}
	// One in Dockerfile (setcap stage), two in Dockerfile.iperf (setcap + final).
	// Fewer means a base was renamed or dropped and this lockstep check went
	// vacuous - fail so the test is updated alongside that change, not skipped.
	if len(debianPins) != 3 {
		t.Fatalf("found %d debian:13-slim FROM pins across %v, want 3: %+v", len(debianPins), dockerfilePaths, debianPins)
	}
	first := debianPins[0]
	for _, p := range debianPins[1:] {
		if p.digest != first.digest {
			t.Errorf("debian:13-slim digest drift: %s has %s but %s has %s - bump every pin in the same change",
				first.path, first.digest, p.path, p.digest)
		}
	}
}

// TestDockerfilesHealthcheckAndAttribution pins the shipped-image metadata both
// Dockerfiles must carry: an exec-form HEALTHCHECK on the healthz subcommand
// (the string form needs a shell, which distroless does not have), and the
// static OCI attribution labels. Substring checks against the raw text keep
// this in step with what BuildKit actually reads.
func TestDockerfilesHealthcheckAndAttribution(t *testing.T) {
	required := []string{
		`CMD ["/pingularity", "healthz"]`,
		`org.opencontainers.image.source="https://github.com/pingular/pingularity"`,
		`org.opencontainers.image.licenses="MIT"`,
		`org.opencontainers.image.title=`,
		`org.opencontainers.image.description=`,
	}
	for _, path := range dockerfilePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(data)
		if !strings.Contains(text, "HEALTHCHECK") {
			t.Errorf("%s: no HEALTHCHECK instruction", path)
		}
		for _, want := range required {
			if !strings.Contains(text, want) {
				t.Errorf("%s: missing %q", path, want)
			}
		}
	}
	// The iperf image must not start in / (unwritable for uid 65532); the
	// distroless base of the default image provides /home/nonroot on its own.
	iperf, err := os.ReadFile("Dockerfile.iperf")
	if err != nil {
		t.Fatalf("read Dockerfile.iperf: %v", err)
	}
	if !strings.Contains(string(iperf), "WORKDIR /var/lib/pingularity") {
		t.Error("Dockerfile.iperf: missing WORKDIR /var/lib/pingularity (process would start in unwritable /)")
	}
}

// The data directory has to ship at 0700, and the only way to get that in a
// distroless stage is to seed it one level down and copy it as an ENTRY: a
// directory COPY creates as its own destination is 0755 and --chmod never
// reaches it. The image shipped world-readable for a release because nothing
// static checked this - the only guard was a Docker-dependent CI leg.
func TestDockerfileShipsTheDataDirAt0700(t *testing.T) {
	b, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	d := string(b)
	if !strings.Contains(d, "chmod 0700 /seed/pingularity") {
		t.Error("Dockerfile: the seeded data dir is not made 0700 in the builder stage")
	}
	if !strings.Contains(d, "COPY --from=setcap --chown=65532:65532 --chmod=0700 /seed/ /var/lib/") {
		t.Error("Dockerfile: data dir must be copied as an entry inside /seed, not as the COPY destination")
	}
	if strings.Contains(d, "/data /var/lib/pingularity") {
		t.Error("Dockerfile: copying onto /var/lib/pingularity makes it the destination, which ships 0755")
	}
}
