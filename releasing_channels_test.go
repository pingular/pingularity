package main

// The release runbook (RELEASING.md) is followed by hand, and two of its
// sentences spoke for files that said otherwise.
//
//   - The prerequisites told the maintainer TAP_GITHUB_TOKEN "needs `repo`/
//     `contents` scope on those two repos". `repo` is a classic-PAT scope, and a
//     classic PAT cannot be limited to two repositories: it writes to every
//     repo its owner can reach, which is exactly the blast radius release.yml's
//     own comment at the secret forbids ("a FINE-GRAINED PAT scoped to ONLY the
//     tap and winget repos"). Nothing in the pipeline can tell which kind was
//     stored, so a maintainer who took the first name offered provisioned the
//     forbidden token for good.
//
//   - The rollback step said Docker was "the one channel with its own moving
//     pointer" and gave retreat commands for the two floating GHCR tags only.
//     The Homebrew tap is a second one: every stable release commits a cask
//     naming the new version to pingular/homebrew-tap, and `brew upgrade`
//     installs whatever the tap's HEAD names, so a rollback performed as
//     written kept shipping the withdrawn build to every macOS user - and the
//     step's own parenthetical (delete the release) turned that into 404s
//     rather than a rollback. The moving pointers are derived from the release
//     config itself here, so a channel added later cannot be left out of the
//     runbook the same way.

import (
	"regexp"
	"strings"
	"testing"
)

const (
	releasingPath   = "RELEASING.md"
	promoteStepName = "Promote the floating tags (stable only)"
)

// tokenContractComment returns the comment release.yml keeps directly above the
// line that passes TAP_GITHUB_TOKEN - the pipeline's own statement of what the
// secret may be.
func tokenContractComment(t *testing.T, workflow string) string {
	t.Helper()
	lines := strings.Split(workflow, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "TAP_GITHUB_TOKEN:") {
			continue
		}
		var block []string
		for j := i - 1; j >= 0 && strings.HasPrefix(strings.TrimSpace(lines[j]), "#"); j-- {
			block = append([]string{strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[j]), "#"))}, block...)
		}
		return strings.Join(block, " ")
	}
	t.Fatalf("%s no longer passes TAP_GITHUB_TOKEN; the cross-repo secret moved and this test reads the wrong place", releaseWorkflowPath)
	return ""
}

func TestReleasingRunbookProvisionsTheTokenTheWorkflowRequires(t *testing.T) {
	contract := tokenContractComment(t, mustReadRepoFile(t, releaseWorkflowPath))
	if !strings.Contains(strings.ToLower(contract), "fine-grained") {
		t.Fatalf("release.yml no longer asks for a fine-grained PAT where it passes TAP_GITHUB_TOKEN; the contract this test holds the runbook to has changed:\n%s", contract)
	}
	guidance := paragraphsMentioning(mustReadRepoFile(t, releasingPath), "TAP_GITHUB_TOKEN")
	if guidance == "" {
		t.Fatalf("%s never mentions TAP_GITHUB_TOKEN; the runbook no longer tells the maintainer how to provision the secret", releasingPath)
	}
	lower := strings.ToLower(guidance)
	if !strings.Contains(lower, "fine-grained") {
		t.Errorf("%s never tells the maintainer TAP_GITHUB_TOKEN must be a fine-grained PAT, which release.yml requires at the line that passes it:\n%s", releasingPath, guidance)
	}
	if strings.Contains(guidance, "`repo`") {
		t.Errorf("%s offers the classic `repo` scope for TAP_GITHUB_TOKEN; that scope cannot be limited to two repositories, and release.yml forbids a classic PAT there:\n%s", releasingPath, guidance)
	}
	for _, need := range []string{"contents", "expir"} {
		if !strings.Contains(lower, need) {
			t.Errorf("%s's TAP_GITHUB_TOKEN guidance never mentions %q, which the workflow's contract names (the one permission, and the expiry):\n%s", releasingPath, need, guidance)
		}
	}
}

// movingPointers derives every channel a stable release re-points, from the
// release config rather than from a list kept here: the floating GHCR tags the
// workflow promotes, and the tap repository the cask is committed to.
func movingPointers(t *testing.T) []string {
	t.Helper()
	promote := extractRunBlock(t, mustReadRepoFile(t, releaseWorkflowPath), promoteStepName)
	var names []string
	for _, m := range regexp.MustCompile(`"([a-z][a-z0-9-]*):\$\{version\}`).FindAllStringSubmatch(promote, -1) {
		names = append(names, m[1])
	}
	if len(names) == 0 {
		t.Fatalf("the %q step names no floating tags; the promote loop changed shape and this test reads nothing", promoteStepName)
	}
	gor := mustReadRepoFile(t, goreleaserPath)
	start := strings.Index(gor, "\nhomebrew_casks:")
	if start < 0 {
		t.Fatalf("%s has no homebrew_casks block; the tap is no longer published from here", goreleaserPath)
	}
	block := gor[start+1:]
	if end := regexp.MustCompile(`\n[a-z]`).FindStringIndex(block[1:]); end != nil {
		block = block[:end[0]+1]
	}
	repo := regexp.MustCompile(`(?m)^\s+name:\s*(\S+)`).FindStringSubmatch(block)
	if repo == nil {
		t.Fatalf("homebrew_casks names no repository in %s", goreleaserPath)
	}
	return append(names, repo[1])
}

// rollbackStep returns the runbook's "Roll back a bad release" list item, with
// its indented continuation and code fences, and nothing after it.
func rollbackStep(t *testing.T, runbook string) string {
	t.Helper()
	const marker = "- **Roll back a bad release:**"
	at := strings.Index(runbook, marker)
	if at < 0 {
		t.Fatalf("%s no longer has a %q step", releasingPath, marker)
	}
	lines := strings.Split(runbook[at:], "\n")
	out := []string{lines[0]}
	for _, l := range lines[1:] {
		if l != "" && !strings.HasPrefix(l, " ") {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func TestReleasingRollbackRetreatsEveryMovingPointer(t *testing.T) {
	step := rollbackStep(t, mustReadRepoFile(t, releasingPath))
	for _, ptr := range movingPointers(t) {
		// The name closed by its backtick: `latest` is not `latest-iperf`, and
		// the tap is written out as `pingular/homebrew-tap`.
		if !strings.Contains(step, ptr+"`") {
			t.Errorf("a stable release moves `%s`, but the rollback step never names it - a rollback performed as written leaves that channel serving the withdrawn build:\n%s", ptr, step)
		}
	}
	// The tap is a git repository whose HEAD is what brew resolves; the retreat
	// is the cask commit reverted, and the step has to say so in as many words.
	if !strings.Contains(step, "revert") {
		t.Errorf("the rollback step gives the tap no revert: the cask commit stays at HEAD and `brew upgrade` keeps installing the withdrawn version:\n%s", step)
	}
}
