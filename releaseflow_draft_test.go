package main

// Regression tests for the draft-until-complete release flow.
//
// Release run 31756904642 (tag v0.62.0-rc.3) is the reproduction these tests
// encode: "Run GoReleaser" succeeded - GitHub release, assets, GHCR images,
// and (on a stable tag) the brew cask and winget branch all published - and
// only then did "Attest build provenance (default image)" fail ("Invalid
// image name: oci://ghcr.io/pingular/pingularity"). That stranded a PUBLIC
// release with no attestation, and for a STABLE tag the "Forbid re-cutting a
// published stable release" guard would have refused the very re-run needed
// to complete it (the rc tag only escaped because prerelease tags skip the
// guard). The fix is sequencing: GoReleaser now creates the release as a
// DRAFT, the workflow flips it public only after every attestation succeeds,
// and the guard's published-only lookup (GET /repos/{owner}/{repo}/releases/
// tags/{tag} returns "a published release with the specified tag" per the
// GitHub REST docs, so a draft yields HTTP 404) makes a stranded draft
// resumable by design.
//
// The workflow steps are shell contracts, so the tests extract the actual
// `run: |` scripts from release.yml and drive them under bash with a stubbed
// gh (and a no-op sleep to skip retry backoffs), the way the runner would
// (GitHub's default shell is `bash -e -o pipefail`).

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	releaseWorkflowPath = ".github/workflows/release.yml"
	goreleaserPath      = ".goreleaser.yaml"

	guardStepName   = "Forbid re-cutting a published stable release"
	recheckStepName = "Recheck no published stable release before publishing"
	flipStepName    = "Publish the release (flip out of draft)"
)

// ghStub simulates the gh CLI. GH_STUB_MODE selects the GitHub API state:
//
//	published - a PUBLISHED release exists for the tag (gh api ... exits 0)
//	absent    - no published release: the tags endpoint 404s, which is also
//	            exactly what a stranded DRAFT release looks like ("Get a
//	            release by tag name" only returns published releases)
//	flaky     - transient API failure (non-404 error)
//	ok        - every gh invocation succeeds (for the flip step)
//
// Every invocation is appended to GH_STUB_LOG for call-count/argv assertions.
const ghStub = `#!/bin/sh
echo "$*" >> "$GH_STUB_LOG"
case "$GH_STUB_MODE" in
  published)
    echo '{"id":123,"draft":false}'
    exit 0 ;;
  absent)
    echo "gh: Not Found (HTTP 404)" >&2
    exit 1 ;;
  flaky)
    echo "gh: connect: connection refused" >&2
    exit 1 ;;
  ok)
    exit 0 ;;
esac
echo "gh stub: unknown mode '$GH_STUB_MODE'" >&2
exit 99
`

func mustReadRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	// Normalise CRLF: everything below splits on "\n" and compares whole lines
	// (`line == "release:"`), and a Windows checkout leaves a trailing "\r" on
	// every one of them - which reads as "the key is absent" and fails the
	// contract these tests exist to pin, on the one platform that never runs
	// the release workflow.
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

// extractRunBlock returns the literal `run: |` script of the named step. The
// workflow file is owned by this repo with a fixed layout (steps at 6 spaces,
// step keys at 8, script body at 10), so a targeted scan is reliable without
// a YAML dependency.
func extractRunBlock(t *testing.T, workflow, stepName string) string {
	t.Helper()
	lines := strings.Split(workflow, "\n")
	nameIdx := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "- name: "+stepName {
			nameIdx = i
			break
		}
	}
	if nameIdx < 0 {
		t.Fatalf("step %q not found in %s", stepName, releaseWorkflowPath)
	}
	runIdx, runIndent := -1, 0
	for i := nameIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "- name:") || strings.HasPrefix(trimmed, "- uses:") {
			break
		}
		if trimmed == "run: |" {
			runIdx = i
			runIndent = len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
			break
		}
	}
	if runIdx < 0 {
		t.Fatalf("step %q has no literal run block", stepName)
	}
	bodyIndent := runIndent + 2
	var body []string
	for i := runIdx + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			body = append(body, "")
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent < bodyIndent {
			break
		}
		body = append(body, line[bodyIndent:])
	}
	if len(body) == 0 {
		t.Fatalf("step %q has an empty run block", stepName)
	}
	return strings.Join(body, "\n") + "\n"
}

// goreleaserReleaseValue returns the scalar value of a key directly under the
// top-level `release:` block of .goreleaser.yaml, or "" when absent.
func goreleaserReleaseValue(t *testing.T, key string) string {
	t.Helper()
	lines := strings.Split(mustReadRepoFile(t, goreleaserPath), "\n")
	inBlock := false
	for _, line := range lines {
		if line == "release:" {
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		if line != "" && !strings.HasPrefix(line, " ") {
			break // next top-level key
		}
		if !strings.HasPrefix(line, "  "+key+":") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), key+":"))
		if i := strings.Index(value, "#"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		return value
	}
	return ""
}

// runWorkflowScript executes an extracted step script the way the Actions
// runner would, with gh and sleep stubbed. Returns the exit code and output.
func runWorkflowScript(t *testing.T, script, mode, tag, ghLog string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	stubDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(stubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubDir, "gh"), []byte(ghStub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubDir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "--noprofile", "--norc", "-e", "-o", "pipefail", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GITHUB_REF_NAME="+tag,
		"GITHUB_REPOSITORY=pingular/pingularity",
		"GH_TOKEN=stub-token",
		"GH_STUB_MODE="+mode,
		"GH_STUB_LOG="+ghLog,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running step script: %v\n%s", err, out)
		}
		return exitErr.ExitCode(), string(out)
	}
	return 0, string(out)
}

func ghCalls(t *testing.T, ghLog string) []string {
	t.Helper()
	data, err := os.ReadFile(ghLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// TestReleaseflowStrandedStableRunIsResumable is the reproduction of the
// residual finding. It derives the state a stable release is stranded in
// when a run dies AFTER GoReleaser published but BEFORE the workflow's last
// step - with release.draft unset/false that is a PUBLISHED release (the
// tags endpoint answers 200), with draft: true it is a DRAFT (the tags
// endpoint answers 404) - and requires both stable-immutability guards to
// let the completing re-run proceed. On the pre-fix tree this fails exactly
// the way run 31756904642 would have on a stable tag: stranded = published,
// guard sees 200, refuses, and the release can never be completed.
func TestReleaseflowStrandedStableRunIsResumable(t *testing.T) {
	workflow := mustReadRepoFile(t, releaseWorkflowPath)
	draft := goreleaserReleaseValue(t, "draft")
	strandedMode := "published"
	if draft == "true" {
		strandedMode = "absent" // drafts are invisible to the published-only tags endpoint
	}
	for _, step := range []string{guardStepName, recheckStepName} {
		script := extractRunBlock(t, workflow, step)
		ghLog := filepath.Join(t.TempDir(), "gh.log")
		code, out := runWorkflowScript(t, script, strandedMode, "v9.9.9", ghLog)
		if code != 0 {
			t.Errorf("%s: a stable release stranded before completion (release.draft=%q, so the stranded state answers %q) must be resumable, but the guard exited %d and the release can never be completed:\n%s",
				step, draft, strandedMode, code, out)
		}
	}
}

// A published stable release must still be immutable: both guards refuse it.
func TestReleaseflowGuardBlocksPublishedStable(t *testing.T) {
	workflow := mustReadRepoFile(t, releaseWorkflowPath)
	for _, step := range []string{guardStepName, recheckStepName} {
		script := extractRunBlock(t, workflow, step)
		ghLog := filepath.Join(t.TempDir(), "gh.log")
		code, out := runWorkflowScript(t, script, "published", "v9.9.9", ghLog)
		if code == 0 {
			t.Errorf("%s: a PUBLISHED stable release must block re-cutting, but the guard exited 0:\n%s", step, out)
		}
	}
}

// Transient API errors (not a definite 404) must still fail closed, after
// exhausting the retry budget.
func TestReleaseflowGuardFailsClosedOnAPIErrors(t *testing.T) {
	workflow := mustReadRepoFile(t, releaseWorkflowPath)
	for _, step := range []string{guardStepName, recheckStepName} {
		script := extractRunBlock(t, workflow, step)
		ghLog := filepath.Join(t.TempDir(), "gh.log")
		code, out := runWorkflowScript(t, script, "flaky", "v9.9.9", ghLog)
		if code == 0 {
			t.Errorf("%s: transient API errors must fail closed, but the guard exited 0:\n%s", step, out)
		}
		if calls := ghCalls(t, ghLog); len(calls) != 3 {
			t.Errorf("%s: expected 3 gh attempts before failing closed, got %d: %q", step, len(calls), calls)
		}
	}
}

// Prerelease tags skip the stable guard entirely (the rc re-cut loop).
func TestReleaseflowGuardSkipsPrerelease(t *testing.T) {
	workflow := mustReadRepoFile(t, releaseWorkflowPath)
	for _, step := range []string{guardStepName, recheckStepName} {
		script := extractRunBlock(t, workflow, step)
		ghLog := filepath.Join(t.TempDir(), "gh.log")
		code, out := runWorkflowScript(t, script, "flaky", "v9.9.9-rc.1", ghLog)
		if code != 0 {
			t.Errorf("%s: prerelease tags must skip the stable guard, got exit %d:\n%s", step, code, out)
		}
		if calls := ghCalls(t, ghLog); len(calls) != 0 {
			t.Errorf("%s: prerelease path must not consult the API, but gh was called: %q", step, calls)
		}
	}
}

// TestReleaseflowDraftUntilCompleteContract pins the two halves of the
// sequencing fix: GoReleaser creates the release as a draft (with stranded
// drafts replaced on resume, so a re-run never mixes assets from two
// builds), and the workflow only flips it public AFTER every attestation -
// the flip must be the step that completes the release.
func TestReleaseflowDraftUntilCompleteContract(t *testing.T) {
	if v := goreleaserReleaseValue(t, "draft"); v != "true" {
		t.Errorf(".goreleaser.yaml release.draft = %q, want \"true\": without it GoReleaser publishes the release before attestation, and a late failure strands a public release", v)
	}
	if v := goreleaserReleaseValue(t, "replace_existing_draft"); v != "true" {
		t.Errorf(".goreleaser.yaml release.replace_existing_draft = %q, want \"true\": a resumed run must replace the stranded draft wholesale, never mix assets from two builds", v)
	}
	if v := goreleaserReleaseValue(t, "replace_existing_artifacts"); v != "true" {
		t.Errorf(".goreleaser.yaml release.replace_existing_artifacts = %q, want \"true\": the rc re-cut loop overwrites same-named assets on the already-published rc release", v)
	}

	workflow := mustReadRepoFile(t, releaseWorkflowPath)
	stepOffset := func(name string) int {
		idx := strings.Index(workflow, "- name: "+name)
		if idx < 0 {
			t.Fatalf("step %q not found in %s", name, releaseWorkflowPath)
		}
		return idx
	}
	flip := stepOffset(flipStepName)
	for _, mustPrecede := range []string{
		"Run GoReleaser",
		"Attest build provenance (release artifacts)",
		"Attest build provenance (default image)",
		"Attest build provenance (iperf image)",
	} {
		if off := stepOffset(mustPrecede); off > flip {
			t.Errorf("step %q must run before %q: the release may only go public once every artifact and attestation is in place", mustPrecede, flipStepName)
		}
	}
	tail := workflow[flip:]
	if rest := tail[strings.Index(tail, "\n")+1:]; strings.Contains(rest, "- name: ") || strings.Contains(rest, "- uses: ") {
		t.Errorf("%q must be the FINAL step of the release job: anything after it could fail with the release already public", flipStepName)
	}
}

// TestReleaseflowFlipStepContract drives the extracted flip script: stable
// tags are published and explicitly marked latest, prerelease tags are
// published without --latest (GitHub refuses to mark drafts/prereleases as
// latest), and a gh failure fails the step after its retry budget so the
// release verifiably stays a draft.
func TestReleaseflowFlipStepContract(t *testing.T) {
	workflow := mustReadRepoFile(t, releaseWorkflowPath)
	script := extractRunBlock(t, workflow, flipStepName)

	editLine := regexp.MustCompile(`^release edit (\S+) --draft=false( --latest)? --repo pingular/pingularity$`)

	t.Run("stable", func(t *testing.T) {
		ghLog := filepath.Join(t.TempDir(), "gh.log")
		code, out := runWorkflowScript(t, script, "ok", "v9.9.9", ghLog)
		if code != 0 {
			t.Fatalf("flip step failed on a stable tag (exit %d):\n%s", code, out)
		}
		calls := ghCalls(t, ghLog)
		if len(calls) != 1 {
			t.Fatalf("expected exactly 1 gh call, got %d: %q", len(calls), calls)
		}
		m := editLine.FindStringSubmatch(calls[0])
		if m == nil || m[1] != "v9.9.9" || m[2] != " --latest" {
			t.Errorf("stable flip must be `release edit v9.9.9 --draft=false --latest --repo ...`, got %q", calls[0])
		}
	})

	t.Run("prerelease", func(t *testing.T) {
		ghLog := filepath.Join(t.TempDir(), "gh.log")
		code, out := runWorkflowScript(t, script, "ok", "v9.9.9-rc.1", ghLog)
		if code != 0 {
			t.Fatalf("flip step failed on a prerelease tag (exit %d):\n%s", code, out)
		}
		calls := ghCalls(t, ghLog)
		if len(calls) != 1 {
			t.Fatalf("expected exactly 1 gh call, got %d: %q", len(calls), calls)
		}
		m := editLine.FindStringSubmatch(calls[0])
		if m == nil || m[1] != "v9.9.9-rc.1" || m[2] != "" {
			t.Errorf("prerelease flip must be `release edit v9.9.9-rc.1 --draft=false --repo ...` without --latest, got %q", calls[0])
		}
	})

	t.Run("fails closed", func(t *testing.T) {
		ghLog := filepath.Join(t.TempDir(), "gh.log")
		code, out := runWorkflowScript(t, script, "flaky", "v9.9.9", ghLog)
		if code == 0 {
			t.Fatalf("flip step must fail when gh cannot publish the release, got exit 0:\n%s", out)
		}
		if calls := ghCalls(t, ghLog); len(calls) != 3 {
			t.Errorf("expected 3 flip attempts before failing, got %d: %q", len(calls), calls)
		}
	})
}
