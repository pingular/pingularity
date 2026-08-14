package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const redraftStepName = "Re-draft a published prerelease before replacing its bytes"

// Re-cutting an rc replaces the assets of a release that is ALREADY PUBLIC
// (replace_existing_artifacts - the amend loop this repo actually uses). For the
// length of that run the rc's download URLs served bytes whose new digests
// nothing had attested yet: draft-until-complete covered a tag's first cut and
// not its second, and both workflow guards deliberately skip prereleases.
//
// Putting the release back to draft first restores the property: the tag goes
// dark while it is incomplete, and the same final step publishes it again.
func TestRecutRedraftsAPublishedPrerelease(t *testing.T) {
	wf := mustReadRepoFile(t, releaseWorkflowPath)
	script := extractRunBlock(t, wf, redraftStepName)
	log := filepath.Join(t.TempDir(), "gh.log")

	code, out := runWorkflowScript(t, script, "published", "v0.62.0-rc.5", log)
	if code != 0 {
		t.Fatalf("re-draft of a published rc exited %d, want 0:\n%s", code, out)
	}
	var edited bool
	for _, c := range ghCalls(t, log) {
		if strings.Contains(c, "release edit") && strings.Contains(c, "--draft=true") {
			edited = true
		}
	}
	if !edited {
		t.Fatalf("a published rc was NOT put back to draft before its assets were replaced; the tag stays public while it is incomplete:\n%v", ghCalls(t, log))
	}
}

// The first cut of a tag has no published release, and must not be disturbed -
// that is the ordinary path.
func TestRecutLeavesAFirstCutAlone(t *testing.T) {
	wf := mustReadRepoFile(t, releaseWorkflowPath)
	script := extractRunBlock(t, wf, redraftStepName)
	log := filepath.Join(t.TempDir(), "gh.log")

	code, out := runWorkflowScript(t, script, "absent", "v0.62.0-rc.5", log)
	if code != 0 {
		t.Fatalf("first cut exited %d, want 0 (nothing to re-draft):\n%s", code, out)
	}
	for _, c := range ghCalls(t, log) {
		if strings.Contains(c, "--draft=true") {
			t.Fatalf("a first cut tried to re-draft something: %q", c)
		}
	}
}

// FAIL CLOSED. If the API cannot be reached, continuing would replace the bytes
// of a live release with nothing attesting them - exactly what the step exists
// to prevent - so an unreachable API stops the run instead.
func TestRecutFailsClosedWhenTheAPIIsUnreachable(t *testing.T) {
	wf := mustReadRepoFile(t, releaseWorkflowPath)
	script := extractRunBlock(t, wf, redraftStepName)
	log := filepath.Join(t.TempDir(), "gh.log")

	code, out := runWorkflowScript(t, script, "flaky", "v0.62.0-rc.5", log)
	if code == 0 {
		t.Fatalf("an unreachable API let the re-cut proceed (exit 0): a published prerelease would be overwritten in place\n%s", out)
	}
}

// Gating: the step is prerelease-only. A stable tag never needs it (its own
// guard refuses a re-cut outright), and running it there would put a published
// stable release back into draft, which is precisely what must not happen.
func TestRecutStepIsPrereleaseOnly(t *testing.T) {
	wf := mustReadRepoFile(t, releaseWorkflowPath)
	i := strings.Index(wf, "- name: "+redraftStepName)
	if i < 0 {
		t.Fatalf("step %q not found", redraftStepName)
	}
	// The condition sits directly under the step name.
	window := wf[i:min(i+300, len(wf))]
	if !strings.Contains(window, "if: ${{ contains(github.ref_name, '-') }}") {
		t.Errorf("the re-draft step is not gated to prerelease tags; on a stable tag it would un-publish a live release:\n%s", window)
	}
	// And it must run BEFORE the bytes are replaced.
	gore := strings.Index(wf, "- name: Run GoReleaser")
	if gore < i {
		t.Error("the re-draft step runs AFTER GoReleaser, so the replacement bytes are published before the tag goes dark")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ghStubEditFails answers the "is it published?" probe successfully but refuses
// every `release edit`. Neither existing stub mode reaches the retry tail: the
// `published` mode succeeds on the first edit, and `flaky` never gets past the
// probe - so the fail-closed branch that matters most went untested, and two
// mutations of it survived the suite byte-identically.
const ghStubEditFails = `#!/bin/sh
echo "$*" >> "$GH_STUB_LOG"
case "$1 $2" in
  "api repos/pingular/pingularity/releases/tags/v0.62.0-rc.5")
    echo '{"id":123,"draft":false}'
    exit 0 ;;
esac
case "$1" in
  release) echo "gh: could not edit release" >&2; exit 1 ;;
esac
exit 0
`

// If the release cannot be put back to draft, the run must STOP. Continuing
// would replace the assets of a live release with bytes nothing has attested,
// which is the whole reason the step exists.
func TestRecutStopsWhenItCannotRedraft(t *testing.T) {
	wf := mustReadRepoFile(t, releaseWorkflowPath)
	script := extractRunBlock(t, wf, redraftStepName)

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	stubDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(stubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubDir, "gh"), []byte(ghStubEditFails), 0o755); err != nil {
		t.Fatal(err)
	}
	// sleep is stubbed out so the retry loop does not cost the suite 15 seconds.
	if err := os.WriteFile(filepath.Join(stubDir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "gh.log")
	cmd := exec.Command("bash", "--noprofile", "--norc", "-e", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GITHUB_REF_NAME=v0.62.0-rc.5",
		"GITHUB_REPOSITORY=pingular/pingularity",
		"GH_TOKEN=stub-token",
		"GH_STUB_LOG="+logPath,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the step reported success although it never managed to re-draft the release:\n%s", out)
	}
	if !strings.Contains(string(out), "refusing to overwrite a published prerelease") {
		t.Errorf("the failure does not explain itself:\n%s", out)
	}
	// It must have actually tried more than once before giving up.
	b, _ := os.ReadFile(logPath)
	if n := strings.Count(string(b), "release edit"); n < 2 {
		t.Errorf("gave up after %d edit attempt(s); the retry loop is meant to absorb a transient API failure", n)
	}
}
