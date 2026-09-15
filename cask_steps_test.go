package main

import (
	"regexp"
	"strings"
	"testing"
)

// caskBlock returns .goreleaser.yaml's homebrew_casks block, up to the next
// top-level key.
func caskBlock(t *testing.T) string {
	t.Helper()
	gor := mustReadRepoFile(t, goreleaserPath)
	start := strings.Index(gor, "\nhomebrew_casks:")
	if start < 0 {
		t.Fatalf("%s has no homebrew_casks block", goreleaserPath)
	}
	block := gor[start+1:]
	if end := regexp.MustCompile(`\n[a-z]`).FindStringIndex(block[1:]); end != nil {
		block = block[:end[0]+1]
	}
	return block
}

// THE CASK SAYS NOTHING HOMEBREW HAS DEPRECATED. goreleaser's hooks.pre/post
// install and uninstall write Homebrew's raw-Ruby flight stanzas, and Homebrew
// now warns on every brew command that touches a cask carrying one - three
// times on a plain `brew install` - telling the user to report it to the tap.
// The quarantine strip the unsigned binary needs rides custom_block instead, as
// the structured postflight_steps stanza, with Homebrew's install-time token for
// the staged path (escaped, because goreleaser templates custom_block itself).
func TestTheCaskStripsQuarantineWithoutADeprecatedStanza(t *testing.T) {
	block := caskBlock(t)
	if regexp.MustCompile(`(?m)^\s+hooks:`).MatchString(block) {
		t.Errorf("homebrew_casks still sets hooks:, which goreleaser writes as Homebrew's deprecated flight stanzas:\n%s", block)
	}
	for _, want := range []string{
		"postflight_steps do",
		"on_macos do",
		`run "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "{{ "{{staged_path}}" }}/pingularity"], must_succeed: false`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("homebrew_casks' custom_block is missing %q, so the unsigned binary keeps its quarantine and Gatekeeper blocks the first run:\n%s", want, block)
		}
	}
}

// THE CAVEAT SERVES AN UPGRADE AS WELL AS A FIRST INSTALL. It is printed on every
// install and upgrade, and it only said to run `install`: over a service already
// registered, that is refused, and the service keeps running the old binary.
func TestTheCaskCaveatNamesTheRestartForAnInstalledService(t *testing.T) {
	block := caskBlock(t)
	i := strings.Index(block, "caveats:")
	if i < 0 {
		t.Fatalf("homebrew_casks has no caveats:\n%s", block)
	}
	caveats := block[i:]
	for _, want := range []string{"sudo pingularity install", "sudo pingularity restart"} {
		if !strings.Contains(caveats, want) {
			t.Errorf("the cask caveat does not say %q:\n%s", want, caveats)
		}
	}
}
