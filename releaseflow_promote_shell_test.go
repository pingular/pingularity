package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dockerStub stands in for `docker buildx imagetools`. DOCKER_STUB_MODE picks
// what the registry appears to say:
//
//	ok       - both tags resolve to the same digest (a successful promote)
//	mismatch - the floating tag resolves elsewhere (the move did not land)
//	fail     - inspect cannot read the registry at all (auth, outage, typo)
//
// `create` always succeeds: the question under test is whether the VERIFY step
// believes it.
const dockerStub = `#!/bin/sh
echo "$*" >> "$DOCKER_STUB_LOG"
case "$3" in
  create) exit 0 ;;
esac
case "$DOCKER_STUB_MODE" in
  ok)       echo '"sha256:aaaa"'; exit 0 ;;
  mismatch)
    case "$*" in
      *latest*) echo '"sha256:bbbb"'; exit 0 ;;
      *)        echo '"sha256:aaaa"'; exit 0 ;;
    esac ;;
  fail)     echo "error: failed to read registry" >&2; exit 1 ;;
esac
echo "docker stub: unknown mode" >&2
exit 99
`

// runStepUnderRunnerShell runs a workflow step the way the RUNNER does: GitHub's
// default for `run:` on Linux is `bash -e {0}` - NO pipefail. The older harness
// added pipefail, which is stricter than production and would have made a
// pipeline whose failure is swallowed by its last command look safe.
func runStepUnderRunnerShell(t *testing.T, script, dockerMode, tag string) (int, string, []string) {
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
	logPath := filepath.Join(dir, "docker.log")
	if err := os.WriteFile(filepath.Join(stubDir, "docker"), []byte(dockerStub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "--noprofile", "--norc", "-e", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GITHUB_REF_NAME="+tag,
		"GORELEASER_CURRENT_TAG="+tag,
		"DOCKER_STUB_MODE="+dockerMode,
		"DOCKER_STUB_LOG="+logPath,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running step: %v\n%s", err, out)
		}
		code = ee.ExitCode()
	}
	var calls []string
	if b, rerr := os.ReadFile(logPath); rerr == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				calls = append(calls, l)
			}
		}
	}
	return code, string(out), calls
}

// A verification step that cannot fail is worse than none, because it gets
// quoted as evidence. Reading the two digests with a bare pipeline made both
// reads return the empty string when the registry was unreadable - and two
// empty strings compare equal, so the step announced a verified promotion it
// had not checked. `set -e` does not save it: the pipeline's status is `tr`'s.
func TestPromoteFailsWhenTheRegistryCannotBeRead(t *testing.T) {
	script := extractRunBlock(t, mustReadRepoFile(t, releaseWorkflowPath), "Promote the floating tags (stable only)")
	code, out, _ := runStepUnderRunnerShell(t, script, "fail", "v0.62.0")
	if code == 0 {
		t.Fatalf("the promote step reported success while it could not read a single digest:\n%s", out)
	}
}

// The check must still catch the thing it exists for: a floating tag that did
// not actually move to the attested digest.
func TestPromoteFailsWhenTheTagDidNotMove(t *testing.T) {
	script := extractRunBlock(t, mustReadRepoFile(t, releaseWorkflowPath), "Promote the floating tags (stable only)")
	code, out, _ := runStepUnderRunnerShell(t, script, "mismatch", "v0.62.0")
	if code == 0 {
		t.Fatalf("a floating tag pointing somewhere else was accepted:\n%s", out)
	}
	if !strings.Contains(out, "did not move") {
		t.Errorf("the failure does not say what went wrong:\n%s", out)
	}
}

// And the happy path must promote BOTH tags and verify BOTH.
func TestPromoteMovesBothTagsAndVerifiesThem(t *testing.T) {
	script := extractRunBlock(t, mustReadRepoFile(t, releaseWorkflowPath), "Promote the floating tags (stable only)")
	code, out, calls := runStepUnderRunnerShell(t, script, "ok", "v0.62.0")
	if code != 0 {
		t.Fatalf("a clean promote exited %d:\n%s", code, out)
	}
	var created, inspected int
	for _, c := range calls {
		if strings.Contains(c, "create") {
			created++
		}
		if strings.Contains(c, "inspect") {
			inspected++
		}
	}
	if created != 2 {
		t.Errorf("moved %d tags, want 2 (latest and latest-iperf)", created)
	}
	if inspected != 4 {
		t.Errorf("made %d digest reads, want 4 (both tags, both sides of the comparison)", inspected)
	}
}
