package main

import (
	"strings"
	"testing"
)

// `latest` is the tag almost everyone actually pulls, and GoReleaser runs
// BEFORE the attestation steps. Tagging latest at build time therefore
// published that channel while its provenance did not yet exist, and a run that
// died at any attestation left latest pointing at an unattested image with no
// visible symptom - the draft-until-complete flow protected the GitHub release
// and missed the container channel entirely.
//
// So: GoReleaser pushes only immutable version tags, and the floating tags are
// moved by digest after every attestation succeeds.
func TestGoReleaserDoesNotPushFloatingTags(t *testing.T) {
	// Read the TAG LISTS, rather than matching strings. The first version of this
	// test asserted the ABSENCE of two exact pre-fix template literals - a
	// denylist - so adding `- "latest"` back unconditionally, which reinstates the
	// defect AND worsens it (an rc would then move latest too), sailed straight
	// through. What matters is the set of tags GoReleaser is configured to push,
	// however they are spelled. Scanned by hand rather than with a YAML library:
	// this repo keeps its dependency list short on purpose, and a test is a poor
	// reason to grow it.
	lists := goreleaserTagLists(t)
	if len(lists) == 0 {
		t.Fatalf("no dockers_v2 tag lists found in %s", goreleaserPath)
	}
	for i, tags := range lists {
		var versioned int
		for _, tag := range tags {
			// A floating tag is one whose value does not depend on the version:
			// anything with no .Version template is a channel name.
			if !strings.Contains(tag, ".Version") {
				t.Errorf("dockers_v2[%d] pushes the floating tag %q at BUILD time, before any attestation exists; "+
					"floating tags are promoted by digest once the attestations have succeeded", i, tag)
				continue
			}
			versioned++
		}
		if versioned == 0 {
			t.Errorf("dockers_v2[%d] pushes no version-pinned tag at all; everything else is built on those", i)
		}
	}
}

// goreleaserTagLists returns the `tags:` list of every dockers_v2 entry. The
// file is this repo's own, with a fixed two-space layout, so an indentation scan
// is enough - and keeps the dependency list where it is.
func goreleaserTagLists(t *testing.T) [][]string {
	t.Helper()
	var out [][]string
	var cur []string
	inDockers, inTags := false, false
	for _, line := range strings.Split(mustReadRepoFile(t, goreleaserPath), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case line == "dockers_v2:":
			inDockers = true
			continue
		case inDockers && line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t"):
			// A new top-level key ends the block.
			if inTags && cur != nil {
				out = append(out, cur)
			}
			inDockers, inTags, cur = false, false, nil
			continue
		}
		if !inDockers || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "tags:" {
			if inTags && cur != nil {
				out = append(out, cur)
			}
			inTags, cur = true, []string{}
			continue
		}
		if inTags {
			if strings.HasPrefix(trimmed, "- ") {
				cur = append(cur, strings.Trim(strings.TrimPrefix(trimmed, "- "), `"`))
				continue
			}
			// Any other key at this level ends the tag list.
			if strings.HasSuffix(trimmed, ":") || strings.Contains(trimmed, ": ") {
				out = append(out, cur)
				inTags, cur = false, nil
			}
		}
	}
	if inTags && cur != nil {
		out = append(out, cur)
	}
	return out
}

// Order is the whole point: promote must come after EVERY attestation and
// before the flip. A promote that drifted above an attestation would silently
// restore the defect this test exists to prevent.
func TestFloatingTagsPromotedOnlyAfterEveryAttestation(t *testing.T) {
	wf := mustReadRepoFile(t, releaseWorkflowPath)
	idx := func(step string) int {
		i := strings.Index(wf, "- name: "+step)
		if i < 0 {
			t.Fatalf("step %q not found in %s", step, releaseWorkflowPath)
		}
		return i
	}
	artifacts := idx("Attest build provenance (release artifacts)")
	def := idx("Attest build provenance (default image)")
	iperf := idx("Attest build provenance (iperf image)")
	promote := idx("Promote the floating tags (stable only)")
	flip := idx("Publish the release (flip out of draft)")

	for _, a := range []struct {
		name string
		at   int
	}{{"release artifacts", artifacts}, {"default image", def}, {"iperf image", iperf}} {
		if promote < a.at {
			t.Errorf("the floating tags are promoted BEFORE the %s attestation: latest would point at an image whose provenance may never be written", a.name)
		}
	}
	if flip < promote {
		t.Error("the release is flipped public before the floating tags move; the two must not be reordered without rethinking which channel leads")
	}

	// The promotion must be a stable-only, digest-verified move.
	block := extractRunBlock(t, wf, "Promote the floating tags (stable only)")
	if !strings.Contains(block, "imagetools create") {
		t.Error("promotion should re-point the tag at the existing index, not rebuild or re-push it")
	}
	if !strings.Contains(block, "did not move") {
		t.Error("the promotion does not verify the floating tag landed on the digest that was attested")
	}
	if !strings.Contains(wf, "if: ${{ !contains(github.ref_name, '-') }}") {
		t.Error("promotion is not gated to stable tags; a release candidate must never move latest")
	}
}
