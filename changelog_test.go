package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/web"
)

// What a release does to the installs already running is written down in three
// places. Until v0.100.4 two of them did not exist, and the third described one
// of the toolchain edges it had.
//
//   - CHANGELOG.md. The GitHub release's notes are read by whoever upgrades that
//     week. An operator who skipped four releases, or who is working backwards
//     from a graph that stepped, has nowhere to look - and this release is the
//     one that needed it. The "Auto location" setting was retired on purpose (an
//     automatic test now either uses the server you pinned or finds one itself)
//     and the retirement was named nowhere: speed_auto_loc and speed_auto_label
//     left /api/settings and its defaults, speedtest_auto_label left
//     /api/status, and a provisioning script that had pinned the scope kept
//     POSTing the pair, kept being answered 200, and kept writing nothing.
//
//   - The macOS floor, in prose. Go 1.27 binaries need macOS 13, and the cask
//     declares it so brew refuses cleanly on Monterey - but the cask cannot help
//     the reader taking the darwin archive off the releases page, so the floor
//     also has to be a sentence they meet before they download. Sentences drift:
//     the same toolchain bump that moved this one left the README's Go
//     prerequisite behind (TestREADMEGoVersionMatchesGoMod is what that cost).
//
//   - The URL rule. Moving go.mod from 1.25 to 1.27 unpinned five Go
//     compatibility defaults, not the one the README documents. Three never
//     surface here - nothing in this module hands a custom reader to
//     crypto/..., nothing sets the goroutine labels a crash traceback now
//     carries, and two more post-quantum key exchange groups are between the
//     two ends of the handshake - but net/url now refuses a host whose colons
//     are not a port, which is exactly what a webhook or heartbeat URL carrying
//     an unbracketed IPv6 address looks like. The escape hatch, and its limit,
//     were named nowhere.
//
// These hold the prose to the things that decide it: the cask, the parser this
// build links, and the settings controller an upgrade actually runs.

const changelogPath = "CHANGELOG.md"

// changelogVersionHeading matches one release section: "## v1.2.3", optionally
// with a prerelease suffix, anchored per line so a version named in a sentence
// cannot be mistaken for a heading.
var changelogVersionHeading = regexp.MustCompile(`(?m)^## v[0-9]+\.[0-9]+\.[0-9]+`)

// caskMacOSFloor matches the cask's own floor - the raw cask DSL goreleaser
// carries in custom_block, which is the line brew enforces on install. The
// trailing comment is allowed because that block is Ruby and the line beside it
// is where a maintainer would explain the codename; reading it as "no floor
// declared" would send them hunting for a line that is still there.
var caskMacOSFloor = regexp.MustCompile(`(?m)^\s*depends_on macos:\s*:([a-z]+)\s*(?:#.*)?$`)

// The runbook is followed by hand, so the rule about what earns a line has to
// live where the maintainer already reads - and it has to name the file, or the
// line goes into the GitHub release's notes and nowhere durable. These are the
// three shapes of change that reach a running install without anyone touching
// its settings, and they are what the auto-location retirement was.
var changelogRules = []string{
	"removes a field from an API response",
	"retires a setting",
	"changes a default",
}

func TestChangelogIsWhereTheRunbookSendsIt(t *testing.T) {
	// The runbook first: whether the rule is written down where it will be
	// obeyed is a separate question from whether the file it points at exists,
	// and reading the file first would report only the missing file.
	runbook := mustReadRepoFile(t, releasingPath)
	notes := sectionOf(runbook, "## Release notes")
	if notes == "" {
		t.Fatalf("%s no longer has a `## Release notes` section; the checklist this test holds to %s moved", releasingPath, changelogPath)
	}
	if !strings.Contains(notes, changelogPath) {
		t.Errorf("%s's release-notes checklist never names %s, so a change that earns a line has nowhere durable to go - the notes on a GitHub release are read by whoever upgrades that week and by nobody after:\n%s", releasingPath, changelogPath, notes)
	}
	for _, rule := range changelogRules {
		if !strings.Contains(notes, rule) {
			t.Errorf("%s's release-notes checklist never says that a release which %q earns a line in %s. That is the shape of change this file exists for: the retirement of the auto-location setting removed two settings keys and a status field, answered every POST 200, and was written down nowhere.", releasingPath, rule, changelogPath)
		}
	}

	changelog := mustReadRepoFile(t, changelogPath)
	if !changelogVersionHeading.MatchString(changelog) {
		t.Fatalf("%s has no `## v<version>` release section, so nothing in it is attached to a release:\n%s", changelogPath, changelog)
	}
	// The names themselves, because a consumer greps for the one that stopped
	// answering rather than reading the section. These three are the whole of
	// what v0.100.4 took off the API surface: a struct-tag diff over the tree
	// finds no other removal, and no metric, endpoint or run flag went at all.
	for _, gone := range []string{"speed_auto_loc", "speed_auto_label", "speedtest_auto_label"} {
		if !strings.Contains(changelog, gone) {
			t.Errorf("%s never names %s, which this release stopped answering. Someone whose script broke has that key and nothing else to search for.", changelogPath, gone)
		}
	}

	// And it travels with the artefact. The archive is the only channel that
	// carries prose at all - the packages and the images ship the licences and
	// nothing else - and the reader unpacking a tarball over an install that is
	// already running is the exact reader this file is written for.
	archives := sectionOf(mustReadRepoFile(t, goreleaserPath), "archives:")
	if archives == "" {
		t.Fatalf("%s has no archives block; the release no longer publishes an archive for %s to ride in", goreleaserPath, changelogPath)
	}
	if !regexp.MustCompile(`(?m)^\s*-\s*CHANGELOG\*?\s*$`).MatchString(archives) {
		t.Errorf("%s's archives.files does not list CHANGELOG*, so the tarball carries a README and docs but not the record of what this release changes for an install already running:\n%s", goreleaserPath, archives)
	}
}

func TestMacOSFloorAgreesWithTheCaskThatEnforcesIt(t *testing.T) {
	m := caskMacOSFloor.FindStringSubmatch(mustReadRepoFile(t, goreleaserPath))
	if m == nil {
		t.Fatalf("could not read a `depends_on macos:` floor out of %s's cask block; either the declaration moved or its shape changed, and until it is found this test cannot hold the prose to anything - on a prerelease-skipped channel that declaration is the only thing that refuses an install that cannot run", goreleaserPath)
	}
	floor := m[1] // "ventura"

	// The cask stops `brew upgrade` on an older mac. It cannot stop anyone
	// downloading the darwin archive or the raw binary, both of which are
	// published for every release, so the floor has to be readable before the
	// download too - in the install instructions, and in the record of what
	// changed for an install already running.
	for _, doc := range []string{"README.md", changelogPath} {
		if !strings.Contains(strings.ToLower(mustReadRepoFile(t, doc)), floor) {
			t.Errorf("the cask refuses to install below macOS :%s and %s never names it. brew's refusal is the only guard, and it does not cover the release archive: a Monterey user who downloads the tarball gets a binary that will not launch and a service that stays down.", floor, doc)
		}
	}
}

func TestBareIPv6WebhookRefusalIsDocumentedWithItsEscapeHatch(t *testing.T) {
	// The parser this build links, not a claim about it. go.mod's directive is
	// what decides the compatibility default, so if a later toolchain or a
	// pinned GODEBUG puts the old behaviour back, the sentence below stops being
	// true and has to change with it.
	if _, err := url.Parse("http://fd00::1/hook"); err == nil {
		t.Fatalf("url.Parse accepts an unbracketed IPv6 host again, so the documented refusal no longer happens; the docs describing it are now wrong in the other direction")
	}
	// The advice is only worth printing if it works.
	u, err := url.Parse("http://[fd00::1]/hook")
	if err != nil {
		t.Fatalf("url.Parse rejects the bracketed form too (%v); the docs would be sending operators to a URL this build also refuses", err)
	}
	if u.Hostname() != "fd00::1" {
		t.Fatalf("bracketed IPv6 parses to host %q, not the address that was written; the advice does not survive the parser", u.Hostname())
	}
	readme := mustReadRepoFile(t, "README.md")
	changelog := mustReadRepoFile(t, changelogPath)
	if !strings.Contains(readme, "urlstrictcolons") {
		t.Errorf("README.md documents the certificate-store half of the Go 1.27 change and not this half: a webhook or heartbeat URL written with a bare IPv6 address now fails to parse, and GODEBUG=urlstrictcolons=0 - the escape hatch that restores the old parsing - is named nowhere in the repository. An operator whose alerts stopped has the error and nothing else.")
	}
	if !strings.Contains(strings.ToLower(changelog), "urlstrictcolons") {
		t.Errorf("%s never mentions the URL parsing change, so an operator whose webhook stopped delivering on upgrade cannot find out from the record of what this release changed", changelogPath)
	}
	// Both documents have to say which of the two remedies actually delivers
	// the webhook, or the escape hatch reads as an alternative to bracketing.
	for _, doc := range []struct {
		name, body string
	}{{"README.md", unwrapped(readme)}, {changelogPath, unwrapped(changelog)}} {
		if !strings.Contains(doc.body, "[fd00::1]") {
			t.Errorf("%s names the failure but not the bracketed address that fixes it, which is the only one of the two remedies that gets the webhook delivered", doc.name)
		}
		if !strings.Contains(doc.body, changelogGodebugLimit) {
			t.Errorf("%s offers GODEBUG=urlstrictcolons=0 without saying %q. Restoring the old parsing does not deliver a bare-IPv6 webhook - the old parser read the address as the host fd00: and the port 1, and looked fd00: up as a name - so beside the brackets it reads as a second fix and is not one.", doc.name, changelogGodebugLimit)
		}
	}

	// With a port, the same bare address is the one that actually stops
	// working. The old parser took the last colon for the port and reached the
	// address, so a webhook written that way was delivered by v0.70.1 and is
	// refused by this build - and for this form both remedies bring it back. An
	// entry that shows only the port-less form reads as though no webhook that
	// used to arrive stops arriving.
	const withPort = "http://fd00::1:8080/hook"
	const withPortDelivered = "`http://fd00::1:8080/hook` was delivered by"
	if _, err := url.Parse(withPort); err == nil {
		t.Fatalf("url.Parse accepts %s again, so the documented refusal of a bare IPv6 host with a port no longer happens", withPort)
	}
	t.Setenv("GODEBUG", "urlstrictcolons=0")
	old, err := url.Parse(withPort)
	if err != nil || old.Hostname() != "fd00::1" || old.Port() != "8080" {
		t.Fatalf("under GODEBUG=urlstrictcolons=0, %s is not read as the host fd00::1 and the port 8080 (err %v); the docs promise the old parsing delivers it", withPort, err)
	}
	for _, doc := range []struct {
		name, body string
	}{{"README.md", unwrapped(readme)}, {changelogPath, unwrapped(changelog)}} {
		for _, want := range []string{withPortDelivered, "`http://[fd00::1]:8080/hook`"} {
			if !strings.Contains(doc.body, want) {
				t.Errorf("%s never says %q. A bare IPv6 webhook with a port is the one v0.70.1 delivered and this build refuses, and bracketing it or restoring the old parsing brings it back.", doc.name, want)
			}
		}
	}
}

// changelogGodebugLimit is the clause that keeps the escape hatch from reading
// as a second way to fix a bare-IPv6 webhook. Both documents carry it. The
// hatch restores the old reading of the address, and the old reading took the
// last colon for a port separator: host "fd00:", port "1", a name no resolver
// has ever answered. It is the fix for a URL the stricter parser now refuses,
// not for this one.
const changelogGodebugLimit = "does not deliver"

// The Ookla pair a v0.70.1 operator set and the iperf3 pair they never touched:
// on that build the second was filled in from the first at every load, so this
// is what the machine was doing when it was stopped for the upgrade.
const (
	upgradeOoklaDirection = "down"
	upgradeOoklaRetries   = 3
)

// The two sentences the engine-split entry can carry, one per population. They
// are pinned as strings because the entry is the one with a bill attached:
// `both` measures upload as well as download, so a run that reverts to the
// shipped default moves about twice the data over a metered link.
const (
	changelogIperfReset  = "comes up with iperf3 at the shipped default"
	changelogIperfRemedy = "set iperf3's direction and retries yourself"
	changelogIperfKept   = "keeps the iperf3 direction and retries it had"
	// The population the carry cannot reach, named in the entry so its reader
	// finds the remedy: a v0.70-born database the release candidate already started.
	changelogIperfRCStarted = "already been started by v0.100.0-rc.1"
)

// TestChangelogSaysWhatBecomesOfTheIperfPair holds that entry to the controller
// that decides it rather than to anyone's reading of the code. Two databases,
// the two populations an upgrade from v0.70.1 arrives as: one wearing the birth
// marker v0.70 stamps on the stores it creates, one from before that marker
// existed. Both carry Ookla's pair and no iperf3 pair, which is what "I set the
// Ookla direction and never opened the iperf3 tab" leaves on disk - and which
// of the two sentences above is true of a database depends entirely on which of
// them the settings controller treats as already split.
func TestChangelogSaysWhatBecomesOfTheIperfPair(t *testing.T) {
	// Wrapping collapsed: these are sentences in a paragraph that gets re-flowed
	// whenever a word above them changes, and a line break landing in the middle
	// of one is not the drift this test is here to catch.
	changelog := unwrapped(mustReadRepoFile(t, changelogPath))
	shipped := defaultSettings(config.Config{})

	// The older store is seeded once, under iperf3's own names, so the machine
	// goes on doing what it did. That has been true in every build with the
	// split in it, and the changelog says so for that reader.
	oldDir, oldRetries := iperfPairAfterUpgrade(t, false, false)
	if oldDir != upgradeOoklaDirection || oldRetries != upgradeOoklaRetries {
		t.Errorf("a database from before the birth marker came up with iperf3 at %s/%d, not the %s/%d it had been running: the pair is no longer carried across for anyone, and %s still tells that reader it is",
			oldDir, oldRetries, upgradeOoklaDirection, upgradeOoklaRetries, changelogPath)
	} else if !strings.Contains(changelog, changelogIperfKept) {
		t.Errorf("%s never says %q. A database older than the birth marker keeps the iperf3 direction and retries it had, and that half of the entry is what tells a reader on an old install that nothing is about to change under them.", changelogPath, changelogIperfKept)
	}

	// The one population the carry cannot reach: a v0.70-born database the release
	// candidate has already started. It recorded the split without carrying the
	// pair, so iperf3 sits at the shipped default and nothing on disk says what it
	// had been. Whatever the other two populations get, the entry owes this reader
	// the values it now runs at and what to do about them - checked here and not
	// below, where a carried v0.70 database returns before reaching those checks.
	rcDir, rcRetries := iperfPairAfterUpgrade(t, true, true)
	if rcDir == shipped.IperfDirection && rcRetries == shipped.IperfRetries {
		if !strings.Contains(changelog, changelogIperfRCStarted) {
			t.Errorf("a v0.70 database the release candidate already started comes up with iperf3 at the shipped %s/%d, and %s never names that population (%q), so its reader cannot tell the entry is about them", shipped.IperfDirection, shipped.IperfRetries, changelogPath, changelogIperfRCStarted)
		}
		if !strings.Contains(changelog, changelogIperfRemedy) {
			t.Errorf("%s never tells a reader whose release-candidate database lost the iperf3 pair to %q, which is the whole of what they can do about it", changelogPath, changelogIperfRemedy)
		}
		// The values have to be in the sentence about this population: the same
		// words elsewhere in the entry describe a different one.
		rcEntry := changelog
		if i := strings.Index(changelog, changelogIperfRCStarted); i >= 0 {
			rcEntry = changelog[i:]
			if j := strings.Index(rcEntry, changelogIperfRemedy); j >= 0 {
				rcEntry = rcEntry[:j]
			}
		}
		for _, want := range []string{"`" + shipped.IperfDirection + "`", retriesInWords(shipped.IperfRetries)} {
			if !strings.Contains(rcEntry, want) {
				t.Errorf("%s never names %s where it describes a v0.70 database the release candidate already started, which is what that database runs iperf3 at", changelogPath, want)
			}
		}
	} else if strings.Contains(changelog, changelogIperfRCStarted) {
		t.Errorf("a v0.70 database the release candidate already started now comes up with iperf3 at %s/%d, not the shipped default, and %s still says that population is left at the default", rcDir, rcRetries, changelogPath)
	}

	bornDir, bornRetries := iperfPairAfterUpgrade(t, true, false)
	if bornDir == upgradeOoklaDirection && bornRetries == upgradeOoklaRetries {
		// The pair is carried across for this population too now. The entry
		// must stop saying it is reset, or it is telling most of the installs
		// that will read it the opposite of what happens to them.
		if strings.Contains(changelog, changelogIperfReset) {
			t.Errorf("a database the birth marker calls v0.70-or-later now keeps iperf3 at %s/%d - the pair is carried across - but %s still says %q. Drop that half of the entry and let %q cover both populations.",
				bornDir, bornRetries, changelogPath, changelogIperfReset, changelogIperfKept)
		}
		return
	}
	if bornDir != shipped.IperfDirection || bornRetries != shipped.IperfRetries {
		t.Fatalf("a database the birth marker calls v0.70-or-later came up with iperf3 at %s/%d, which is neither what it had been running (%s/%d) nor the shipped default (%s/%d). %s can only describe one of those two outcomes; it now has to describe a third.",
			bornDir, bornRetries, upgradeOoklaDirection, upgradeOoklaRetries, shipped.IperfDirection, shipped.IperfRetries, changelogPath)
	}
	// It is reset. Say so, say what it costs, and name the values it resets to.
	if !strings.Contains(changelog, changelogIperfReset) {
		t.Errorf("a database the birth marker calls v0.70-or-later comes up with iperf3 at the shipped %s/%d whatever Ookla was set to - on this store, %s/%d before the upgrade - and %s never says %q. That is the entry with a bill attached: %q measures upload as well as download, so an iperf3 run over a metered link moves about twice the data it did yesterday.",
			shipped.IperfDirection, shipped.IperfRetries, upgradeOoklaDirection, upgradeOoklaRetries, changelogPath, changelogIperfReset, shipped.IperfDirection)
	}
	if !strings.Contains(changelog, changelogIperfRemedy) {
		t.Errorf("%s says the iperf3 pair is reset but never tells the reader to %q, which is the whole of what they can do about it", changelogPath, changelogIperfRemedy)
	}
	for _, want := range []string{"`" + shipped.IperfDirection + "`", retriesInWords(shipped.IperfRetries)} {
		if !strings.Contains(changelog, want) {
			t.Errorf("%s never names %s, which is what an upgraded v0.70 database now runs iperf3 at. A reader who cannot see the new value from the entry cannot tell whether it is the one they wanted.", changelogPath, want)
		}
	}
}

// iperfPairAfterUpgrade builds the on-disk shape a v0.70.1 install leaves - the
// Ookla pair set, no iperf3 pair, the birth marker present or not - loads it the
// way the daemon's first start does, and answers with the iperf3 direction and
// retry count that install now runs at. rcStarted adds the split marker a
// release candidate that has already started the database left in it.
//
// File-backed, not :memory:: the load reads the table, seeds from it and writes
// the seed back, and a single-connection store hides what that costs.
func iperfPairAfterUpgrade(t *testing.T, born, rcStarted bool) (string, int) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "pingularity.db"))
	if err != nil {
		t.Fatalf("open the upgraded database: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	rows := map[string]string{
		"speed_direction":  upgradeOoklaDirection,
		"speed_retries":    strconv.Itoa(upgradeOoklaRetries),
		"quick_setup_done": "1",
	}
	if born {
		rows[settings.KeyInstallBornVersion] = "0.70.1"
	}
	if rcStarted {
		rows["engine_split_done"] = "1"
	}
	for k, v := range rows {
		if err := st.SetSetting(ctx, k, v); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	set, err := settings.New(ctx, st, defaultSettings(config.Config{}))
	if err != nil {
		t.Fatalf("first load on the new build: %v", err)
	}
	// The premise, checked rather than assumed: the marker decides which
	// population this is, and a load that stamped one on the older store would
	// quietly turn this into the same case twice.
	if _, split := settingsSnapshot(t, st)["engine_split_done"]; rcStarted && !split {
		t.Fatal("seeded the split marker a release candidate leaves and the store no longer holds it; this fixture is no longer the population it names")
	}
	if _, marked := settingsSnapshot(t, st)[settings.KeyInstallBornVersion]; marked != born {
		t.Fatalf("seeded the store with born=%v and the load left the birth marker present=%v; this fixture is no longer the population it names", born, marked)
	}
	return set.IperfDirection(), set.IperfRetries()
}

// unwrapped collapses every run of whitespace in a document to a single space,
// so a sentence this file pins can be re-wrapped in the markdown without the
// pin coming loose.
func unwrapped(doc string) string { return strings.Join(strings.Fields(doc), " ") }

// retriesInWords writes a retry count the way the prose does: the shipped one
// is spelled out, anything else is a number.
func retriesInWords(n int) string {
	if n == 1 {
		return "one retry"
	}
	return strconv.Itoa(n) + " retries"
}

// sectionOf returns one "## Heading" section of a markdown document, heading
// included, up to the next heading at the same level. The runbook's checklists
// are per-section, and an assertion that read the whole file would pass on a
// sentence sitting under a different one. A top-level YAML key ("archives:")
// has the same shape - the block runs to the next line in column zero - so the
// same walk reads one of those too.
func sectionOf(doc, heading string) string {
	lines := strings.Split(doc, "\n")
	sameLevel := "## "
	if !strings.HasPrefix(heading, "#") {
		sameLevel = ""
	}
	for i, l := range lines {
		if strings.TrimSpace(l) != heading {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if sameLevel == "" {
				if lines[j] != "" && !strings.HasPrefix(lines[j], " ") && !strings.HasPrefix(lines[j], "\t") {
					return strings.Join(lines[i:j], "\n")
				}
				continue
			}
			if strings.HasPrefix(lines[j], sameLevel) {
				return strings.Join(lines[i:j], "\n")
			}
		}
		return strings.Join(lines[i:], "\n")
	}
	return ""
}

// The two entries a sibling change moved out from under while this file was
// being written. The `-db` entry said a link was refused after the daemon had
// learned to follow one; the schedule entry said a no-day window was dropped
// after the drop had been taken back out. Both describe what an upgrade does to
// a machine that is already running, both were true of some build, and neither
// was held to anything - which is how a sentence outlives the behaviour it
// describes. These hold them to the store and the settings controller that
// decide them.

// changelogLinkFollowed is the clause the `-db` entry turns on: the link leads
// the daemon to the file it names, or it stops the daemon.
const changelogLinkFollowed = "link to a real file is followed once"

func TestChangelogSaysWhatADbSymlinkDoes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// The arrangement the entry is written for: the database moved to the
	// bigger disk, a link left where the unit file still points.
	target := filepath.Join(dir, "moved.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("place the database the link names: %v", err)
	}
	link := filepath.Join(dir, "pingularity.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this filesystem will not make a symlink: %v", err)
	}
	changelog := unwrapped(mustReadRepoFile(t, changelogPath))
	saysFollowed := strings.Contains(changelog, changelogLinkFollowed)

	st, err := store.Open(link)
	if err != nil {
		if saysFollowed {
			t.Fatalf("%s says a %q and this build refuses one: %v. Which of the two happens decides whether an install whose -db is a link comes back at all on the first restart after the upgrade, so the entry cannot be left saying the other one.", changelogPath, changelogLinkFollowed, err)
		}
		return
	}
	if err := st.SetSetting(ctx, "quick_setup_done", "1"); err != nil {
		t.Fatalf("write to the store opened through the link: %v", err)
	}
	st.Close()
	if !saysFollowed {
		t.Errorf("this build opens a -db path that is a link to a real file, and %s never says so. That entry is what a reader checks before pointing -db at a link of their own.", changelogPath)
	}
	// "Followed once" means two things the entry promises separately: the file
	// at the far end is the one that was written, and the link is still a link.
	// A build that renamed or replaced the link instead is the split the
	// resolve exists to remove.
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("the link is gone after the open (%v); %s promises it is never renamed or replaced", err, changelogPath)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the open replaced the link with a %s; %s promises the link itself is never renamed or replaced", fi.Mode().Type(), changelogPath)
	}
	far, err := store.Open(target)
	if err != nil {
		t.Fatalf("re-open the file the link names: %v", err)
	}
	defer far.Close()
	if settingsSnapshot(t, far)["quick_setup_done"] != "1" {
		t.Errorf("a write through the link did not reach the file the link names, so %s's promise that the file at the far end is the one opened is not what this build does - and a repair would be about a different file again", changelogPath)
	}
	// Where the key and the log go is the half of the entry an operator acts
	// on. Pointed at the file the link names instead of at the link, -db finds
	// no key beside it and makes a new one, and every saved iperf3 password
	// stops decrypting - so the entry has to say which of the two they follow.
	byLink := keyAndLogFollowTheDBPathAsTyped(t)
	if says := strings.Contains(changelog, dbLinkKeyStays); byLink != says {
		t.Errorf("pingularity.key and logs.txt stay beside a -db link = %v, and %s says %q = %v. An operator who reads the entry the other way points -db at the file the link names and the daemon makes a new key.", byLink, changelogPath, dbLinkKeyStays, says)
	}
}

// changelogScheduleParked is the clause the schedule entry turns on: a window
// selecting no weekday parks what its schedule gates, or it is dropped and the
// feature runs around the clock instead.
const changelogScheduleParked = "parks what it gates"

func TestChangelogSaysWhatANoDayScheduleWindowDoes(t *testing.T) {
	ctx := context.Background()
	// File-backed: the load reads the table and writes back what it normalized,
	// and a single-connection store hides what that costs.
	st, err := store.Open(filepath.Join(t.TempDir(), "pingularity.db"))
	if err != nil {
		t.Fatalf("open the upgraded database: %v", err)
	}
	defer st.Close()
	// What a provisioning script, a restored backup or a hand edit leaves: the
	// latency schedule on, one window, every weekday off.
	if err := st.SetSettings(ctx, map[string]string{
		"sched_lat_enabled": "1",
		"sched_lat_windows": `[{"days":"0000000","start":540,"end":1020}]`,
		"quick_setup_done":  "1",
	}); err != nil {
		t.Fatalf("seed the schedule: %v", err)
	}

	set, err := settings.New(ctx, st, defaultSettings(config.Config{}))
	if err != nil {
		t.Fatalf("first load on the new build: %v", err)
	}
	// Parked means parked all week, not merely outside office hours, so the
	// whole week is asked rather than one convenient hour.
	week := time.Date(2026, time.January, 5, 0, 0, 0, 0, time.UTC) // a Monday
	allowed := ""
	for h := 0; h < 7*24 && allowed == ""; h++ {
		if at := week.Add(time.Duration(h) * time.Hour); set.LatencyAllowed(at) {
			allowed = at.Format(time.RFC1123)
		}
	}
	changelog := unwrapped(mustReadRepoFile(t, changelogPath))
	if allowed != "" {
		t.Fatalf("a latency schedule whose only window selects no weekday allows probing at %s, so the window no longer parks anything and %s still says it does. An install that was deliberately quiet starts probing round the clock on the first restart after the upgrade.", allowed, changelogPath)
	}
	if !strings.Contains(changelog, changelogScheduleParked) {
		t.Errorf("a window selecting no weekday parks the feature its schedule gates, and %s never says %q - which is the whole of what an operator carrying such a schedule needs to know has not changed under them.", changelogPath, changelogScheduleParked)
	}
	// The flag is what the operator wrote down, and the entry says it is saved
	// as written: a build that turned it off would be answering the settings
	// API something other than what is on disk.
	if !set.Snapshot().SchedLatEnabled {
		t.Errorf("the load turned the latency schedule off, so the API answers off where the store says on; %s says such a window is saved as written", changelogPath)
	}
	// And the line the entry promises at every start, in place of the silence it
	// describes.
	got := set.NeverActiveSchedules()
	if len(got) != 1 || got[0] != "latency" {
		t.Errorf("the controller names %v as never able to be active, not [latency], so the boot line %s promises for a parked feature is not printed for this one", got, changelogPath)
	} else if line := schedParkedLine(got[0]); !strings.Contains(line, "latency schedule") || !strings.Contains(changelog, changelogScheduleBootLine) {
		t.Errorf("the daemon names this parked schedule at every start (%q), and %s never says %q - which is the whole of what changed for an install carrying one", line, changelogPath, changelogScheduleBootLine)
	}
}

// changelogScheduleBootLine is what the schedule entry says the daemon now does
// about a parked feature.
const changelogScheduleBootLine = "prints one line per parked feature on stdout at every start"

// The Removed entry is the one a provisioning script's author reads when a run
// that used to pass starts failing, and every sentence in it is a behaviour:
// what a write gets back - the city the install already carries included - what
// a blank one gets, what a dashboard page loaded before the upgrade has to do,
// and which installs are told at boot. Hold each to the settings handler and to
// the boot path's own judgement, over a store shaped the way v0.70.1 left it.
const (
	changelogRetiredWrite     = "comes back `400`"
	changelogRetiredEcho      = "even when it sends the city this install already has"
	changelogRetiredBlank     = "blank is still `200`"
	changelogRetiredStaleTab  = "reload the page and save again"
	changelogRetiredBootLine  = "gets one line on stdout"
	changelogRetiredOoklaOnly = "runs its speedtests on Ookla"
)

func TestChangelogSaysWhatBecomesOfARetiredCity(t *testing.T) {
	ctx := context.Background()
	changelog := unwrapped(mustReadRepoFile(t, changelogPath))
	// File-backed: the settings POST writes through the controller and reads back
	// what it stored, and a single-connection store hides what that costs.
	upgraded := func(t *testing.T, extra map[string]string) (*store.Store, *settings.Controller) {
		t.Helper()
		st, err := store.Open(filepath.Join(t.TempDir(), "pingularity.db"))
		if err != nil {
			t.Fatalf("open the upgraded database: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		rows := map[string]string{
			"speed_auto_loc":   "49.2827,-123.1207",
			"speed_auto_label": "Vancouver, BC",
			"quick_setup_done": "1",
		}
		for k, v := range extra {
			rows[k] = v
		}
		if _, err := st.SetSettingsDiff(ctx, rows); err != nil {
			t.Fatalf("seed the rows v0.70.1 left: %v", err)
		}
		set, err := settings.New(ctx, st, defaultSettings(config.Config{}))
		if err != nil {
			t.Fatalf("first load on the new build: %v", err)
		}
		return st, set
	}
	post := func(st *store.Store, set *settings.Controller, body string) (int, string) {
		h := web.New(st, nil, nil, set, nil, "test", slog.New(slog.DiscardHandler)).Handler()
		req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "127.0.0.1:54321" // local-only access is the default
		req.Host = "127.0.0.1:9000"        // and the DNS-rebinding guard refuses anything else
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	holds := func(behaves bool, clause, what string) {
		t.Helper()
		if says := strings.Contains(changelog, clause); behaves != says {
			t.Errorf("%s = %v, and %s says %q = %v", what, behaves, changelogPath, clause, says)
		}
	}

	st, set := upgraded(t, nil)
	code, body := post(st, set, `{"speed_auto_loc":"48.8566,2.3522","speed_auto_label":"Paris, FR"}`)
	holds(code == http.StatusBadRequest, changelogRetiredWrite, "a non-empty retired key is refused (HTTP "+strconv.Itoa(code)+")")
	holds(strings.Contains(body, "reload it"), changelogRetiredStaleTab, "the refusal tells a dashboard page loaded before the upgrade to reload")
	echo, _ := post(st, set, `{"speed_auto_loc":"49.2827,-123.1207","speed_auto_label":"Vancouver, BC"}`)
	holds(echo == http.StatusBadRequest, changelogRetiredEcho, "the city this install already carries is refused too (HTTP "+strconv.Itoa(echo)+")")
	blank, _ := post(st, set, `{"speed_auto_loc":"","speed_auto_label":""}`)
	holds(blank == http.StatusOK, changelogRetiredBlank, "blank retired keys are accepted (HTTP "+strconv.Itoa(blank)+")")
	holds(retiredCityScope(set) != "", changelogRetiredBootLine, "an install carrying the city and running Ookla is told at boot")

	// An install whose speedtests run on iperf3 is not told: every run measures
	// the server it names, which the city never chose. The stand-in iperf3 is a
	// shell script.
	if runtime.GOOS != "windows" {
		withIperf := t.TempDir()
		if err := os.WriteFile(filepath.Join(withIperf, "iperf3"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", withIperf)
		_, onIperf := upgraded(t, map[string]string{"speed_engine": "iperf3"})
		holds(retiredCityScope(onIperf) == "", changelogRetiredOoklaOnly, "an install whose speedtests run on iperf3 is left out of the boot line")
	}
}

// The package entry makes two promises an operator reads during an upgrade: the
// one line a handover that did not come back prints, quoted so it can be found
// in an apt or dnf log, and which units the upgrade hands over at all. Run the
// real script under the packaging tests' stubs and hold both to what it does.
const (
	changelogUpgradeStoppedUnit  = "stopped before the upgrade is not restarted and says nothing"
	changelogUpgradeDisabledUnit = "disabled but still running is restarted onto the new binary and watched"
)

func TestChangelogSaysWhatAPackageUpgradePrints(t *testing.T) {
	changelog := unwrapped(mustReadRepoFile(t, changelogPath))
	_, out, _ := runPostinstall(t, postinstallEnv{states: []string{"active", "active", "activating"}, withDebHelper: true}, "configure", "0.70.1")
	if line := strings.TrimSpace(out); line == "" || !strings.Contains(changelog, "`"+line+"`") {
		t.Errorf("a deb upgrade whose daemon did not come back prints %q, and %s does not quote that line - the only sign there is, and what an operator searches an upgrade log for", line, changelogPath)
	}
	_, out, _ = runPostinstall(t, postinstallEnv{states: []string{"inactive"}, withDebHelper: true}, "configure", "0.70.1")
	if quiet, says := strings.TrimSpace(out) == "", strings.Contains(changelog, changelogUpgradeStoppedUnit); quiet != says {
		t.Errorf("an upgrade over a unit stopped beforehand prints nothing = %v (%q), and %s says %q = %v", quiet, out, changelogPath, changelogUpgradeStoppedUnit, says)
	}
	// Disabled governs boot, not now: the upgrade leaves the unit's links as the
	// admin left them, and try-restart still acts on a unit that is running.
	_, _, calls := runPostinstall(t, postinstallEnv{states: []string{"active"}, wasEnabledRC: "1", withDebHelper: true}, "configure", "0.70.1")
	watched := countCalls(calls, "systemctl try-restart") == 1 && countCalls(calls, "systemctl is-active") > 1
	if says := strings.Contains(changelog, changelogUpgradeDisabledUnit); watched != says {
		t.Errorf("an upgrade over a disabled unit that is still running hands it over and watches it = %v, and %s says %q = %v:\n%s", watched, changelogPath, changelogUpgradeDisabledUnit, says, strings.Join(calls, "\n"))
	}
}

// The entries an operator acts on when a database is damaged, held to the store
// and the server that decide them: that a start on a damaged database stops
// unless the rebuild was asked for, and that a rebuilt store says so on /readyz
// while /healthz stays up.
const (
	changelogDamagedStops   = "the daemon now stops instead of replacing the file"
	changelogRebuildOnlyIf  = "this build does that only under `-on-corrupt rebuild`"
	changelogRebuiltReadyz  = "`/readyz` answers `503` for as long as the start that rebuilt it runs"
	changelogRebuiltHealthz = "while `/healthz` stays `200`"
	// What ends the hold a rebuilt store keeps on network access: a password set
	// from the machine counts only once the daemon restarts.
	changelogHoldEndsAtRestart = "set on it from the machine itself and the daemon is restarted or reloaded"
)

func TestChangelogSaysWhatADamagedDatabaseDoesToAStart(t *testing.T) {
	ctx := context.Background()
	changelog := unwrapped(mustReadRepoFile(t, changelogPath))
	holds := func(behaves bool, clause, what string) {
		t.Helper()
		if says := strings.Contains(changelog, clause); behaves != says {
			t.Errorf("%s = %v, and %s says %q = %v", what, behaves, changelogPath, clause, says)
		}
	}
	torn := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "pingularity.db")
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetSetting(ctx, "quick_setup_done", "1"); err != nil {
			t.Fatal(err)
		}
		st.Close()
		tearTableRoot(t, path, "pauses")
		return path
	}

	path := torn(t)
	st, err := store.Open(path)
	if err == nil {
		st.Close()
	}
	setAside, _ := filepath.Glob(path + ".*.corrupt")
	_, statErr := os.Stat(path)
	refused := err != nil && strings.Contains(err.Error(), "is damaged") && len(setAside) == 0 && statErr == nil
	holds(refused, changelogDamagedStops, "a start on a damaged database with nobody asking for a rebuild stops and leaves the file where it is")
	holds(refused, changelogRebuildOnlyIf, "the damaged database is replaced only when -on-corrupt rebuild is given")

	st, err = store.Open(torn(t), store.RebuildOnCorruption())
	if err != nil {
		t.Fatalf("a start armed with -on-corrupt rebuild refused the damaged database: %v", err)
	}
	defer st.Close()
	set, err := settings.New(ctx, st, defaultSettings(config.Config{}))
	if err != nil {
		t.Fatalf("settings on the rebuilt store: %v", err)
	}
	status := func() web.LiveStatus { return web.LiveStatus{Online: true} }
	h := web.New(st, status, nil, set, nil, "test", slog.New(slog.DiscardHandler)).Handler()
	probe := func(path string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "127.0.0.1:54321"
		req.Host = "127.0.0.1:9000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	holds(probe("/readyz") == http.StatusServiceUnavailable, changelogRebuiltReadyz, "a store rebuilt after damage answers /readyz 503")
	holds(probe("/healthz") == http.StatusOK, changelogRebuiltHealthz, "a store rebuilt after damage still answers /healthz 200")
	if !strings.Contains(changelog, changelogHoldEndsAtRestart) {
		t.Errorf("%s no longer says a password set on a rebuilt store ends the hold once the daemon restarts or reloads (%q): an operator who sets one and waits sees the network stay closed", changelogPath, changelogHoldEndsAtRestart)
	}
}

// Rolling back is the one step of an upgrade whose failure the new binary never
// sees, so the entry has to carry both warnings itself: take -on-corrupt out of a
// unit before v0.70.1 reads it, and set a password before a held store goes back
// to a release that does not know the hold.
const (
	changelogRollbackFlag     = "take it out again before rolling back"
	changelogRollbackPassword = "Set a password before you roll such a store back"
)

func TestChangelogSaysWhatRollingBackCosts(t *testing.T) {
	changelog := unwrapped(mustReadRepoFile(t, changelogPath))
	// A release that never defined -on-corrupt answers it the way the flag package
	// answers any flag it was not given, and does not start. The entry quotes that
	// answer, so the quote is held to the package that writes it.
	older := flag.NewFlagSet("run", flag.ContinueOnError)
	older.SetOutput(io.Discard)
	err := older.Parse([]string{"-on-corrupt", "rebuild"})
	if err == nil {
		t.Fatal("a flag set without -on-corrupt accepted it")
	}
	if quote := "`" + err.Error() + "`"; !strings.Contains(changelog, quote) {
		t.Errorf("a release without -on-corrupt refuses the flag with %q, and %s does not quote it - the line an operator rolling back finds in the unit's log", err, changelogPath)
	}
	for _, want := range []string{changelogRollbackFlag, changelogRollbackPassword} {
		if !strings.Contains(changelog, want) {
			t.Errorf("%s no longer says %q, which is what an operator has to do before rolling back and cannot learn from the release they roll back to", changelogPath, want)
		}
	}
}

// Two more promises about a -db link, both about damage: the refusal names the
// file the link leads to, so the recovered copy goes back where the link points;
// and the one link to nothing that is not refused is the one a rebuild cut short
// leaves, whose next start puts the finished store in place.
const (
	changelogLinkRefusalNames = "what the refusal of a damaged database names"
	readmeLinkRefusalNames    = "what the refusal above names"
	changelogLinkCutShort     = "One link to nothing is not refused"
)

func TestChangelogSaysWhatDamageBehindADbLinkDoes(t *testing.T) {
	changelog := unwrapped(mustReadRepoFile(t, changelogPath))
	readme := unwrapped(mustReadRepoFile(t, "README.md"))
	holds := func(behaves bool, doc, name, clause, what string) {
		t.Helper()
		if says := strings.Contains(doc, clause); behaves != says {
			t.Errorf("%s = %v, and %s says %q = %v", what, behaves, name, clause, says)
		}
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "moved"), 0o700); err != nil {
		t.Fatal(err)
	}
	far := filepath.Join(dir, "moved", "elsewhere.db")
	link := filepath.Join(dir, "pingularity.db")
	if err := os.Symlink(far, link); err != nil {
		t.Skipf("this filesystem will not make a symlink: %v", err)
	}

	st, err := store.Open(far)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	tearTableRoot(t, far, "pauses")
	_, err = store.Open(link)
	namesFar := err != nil && strings.Contains(err.Error(), filepath.Join("moved", "elsewhere.db")) && !strings.Contains(err.Error(), filepath.Base(link))
	holds(namesFar, changelog, changelogPath, changelogLinkRefusalNames, "the refusal of a damaged database behind a -db link names the file the link leads to")
	holds(namesFar, readme, "README.md", readmeLinkRefusalNames, "the refusal of a damaged database behind a -db link names the file the link leads to")

	// A rebuild cut short between its two renames: the file the link names has
	// moved, and the store the rebuild finished sits beside where it was.
	if err := os.Remove(far); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(far)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := os.Rename(far, far+".rebuilt"); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(link)
	if err == nil {
		st.Close()
	}
	lfi, lerr := os.Lstat(link)
	_, farErr := os.Stat(far)
	_, builtErr := os.Lstat(far + ".rebuilt")
	finished := err == nil && lerr == nil && lfi.Mode()&os.ModeSymlink != 0 && farErr == nil && os.IsNotExist(builtErr)
	holds(finished, changelog, changelogPath, changelogLinkCutShort, "a link to nothing left by a cut-short rebuild is finished by the next start")
	holds(finished, readme, "README.md", changelogLinkCutShort, "a link to nothing left by a cut-short rebuild is finished by the next start")
}
