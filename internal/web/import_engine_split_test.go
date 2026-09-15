package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/settings"
)

// A backup from a sparse store carries the Ookla direction and retries and no
// iperf3 pair (the operator never moved it). The restore's per-row upsert lands
// them and reloads; iperf3 must keep its own values rather than take Ookla's,
// which the reload used to do by reading the absent iperf3 keys as a database
// from before the two engines had their own.
func TestImportOfOoklaKnobsLeavesIperfAlone(t *testing.T) {
	s := newTestServer(t)
	type pairs struct {
		SpeedDirection string `json:"speed_direction"`
		IperfDirection string `json:"iperf_direction"`
		SpeedRetries   int    `json:"speed_retries"`
		IperfRetries   int    `json:"iperf_retries"`
	}
	get := func() pairs {
		t.Helper()
		w := do(t, s.Handler(), "GET", "/api/settings", "")
		if w.Code != 200 {
			t.Fatalf("GET /api/settings: HTTP %d", w.Code)
		}
		var p pairs
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return p
	}
	before := get()
	if before.SpeedDirection == "down" || before.SpeedRetries == 3 {
		t.Fatalf("precondition: the backup's values must differ from the live ones, got %+v", before)
	}
	rr := importConfig(t, s, `{"key":"speed_direction","value":"down"},{"key":"speed_retries","value":"3"}`)
	if rr.Code != 200 {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	after := get()
	if after.SpeedDirection != "down" || after.SpeedRetries != 3 {
		t.Errorf("imported Ookla pair = %q/%d, want down/3", after.SpeedDirection, after.SpeedRetries)
	}
	if after.IperfDirection != before.IperfDirection || after.IperfRetries != before.IperfRetries {
		t.Errorf("iperf3 pair after the import = %q/%d, want the untouched %q/%d (the backup's Ookla values leaked into iperf3)",
			after.IperfDirection, after.IperfRetries, before.IperfDirection, before.IperfRetries)
	}
	if got := s.settings.IperfDirection(); got != before.IperfDirection {
		t.Errorf("live iperf3 direction = %q, want %q", got, before.IperfDirection)
	}
}

// importConfigFrom is importConfig with the envelope's version stamp filled in,
// which is what the restore reads to know whether the file's Ookla pair was
// also its iperf3 one. The stamp sits where our own exporter writes it, ahead
// of every category, so it is known before a row lands.
func importConfigFrom(t *testing.T, s *Server, producerVersion, rows string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"pingularity_export":1,"producer_version":` + strconv.Quote(producerVersion) +
		`,"categories":["config"],"config":[` + rows + `]}`
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/import?config=1", strings.NewReader(body))
	r.Host = "127.0.0.1:9000"
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "127.0.0.1:54321"
	if s.settings.AuthActive() {
		r.SetBasicAuth(s.settings.AuthUser(), testPassword)
	}
	s.Handler().ServeHTTP(rr, r)
	if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized {
		t.Fatalf("request was rejected by the guard (%d %s), so nothing was imported",
			rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	return rr
}

// importWarnings is warningsOf on a restore that must have succeeded.
func importWarnings(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	if rr.Code != 200 {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	return warningsOf(t, rr)
}

// A backup written before 0.100 holds iperf3's direction and retries in the
// Ookla rows and nowhere else - that release read them from there whenever the
// iperf3 keys were absent. This install applied the split at its own first
// boot, so the restore lands Ookla's pair and iperf3 stays at what it has here:
// the values are not carried across (that would be the leak the split exists to
// stop, on every ordinary restore), so the restore has to SAY what the file
// could not bring.
func TestRestoreOfASharedPairBackupSaysWhatItCouldNotBring(t *testing.T) {
	var logged bytes.Buffer
	s := newTestServerLog(t, &logged)
	dir, retries := s.settings.IperfDirection(), s.settings.IperfRetries()
	rr := importConfigFrom(t, s, "0.70.1", `{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"}`)
	warnings := importWarnings(t, rr)
	var said string
	for _, w := range warnings {
		if strings.Contains(w, "iperf3") {
			said = w
		}
	}
	if said == "" {
		t.Fatalf("a restore from 0.70.1 said nothing about the iperf3 pair; warnings = %q", warnings)
	}
	for _, want := range []string{"0.70.1", `"up"`, "3 retries", strconv.Quote(dir), strconv.Itoa(retries) + " retries", "iperf3 tab"} {
		if !strings.Contains(said, want) {
			t.Errorf("the notice does not name %s: %q", want, said)
		}
	}
	// Saying it is the whole change: the values themselves must not move, or
	// this becomes the leak with a message attached.
	if s.settings.IperfDirection() != dir || s.settings.IperfRetries() != retries {
		t.Errorf("iperf3 pair after the restore = %q/%d, want the untouched %q/%d",
			s.settings.IperfDirection(), s.settings.IperfRetries(), dir, retries)
	}
	if s.settings.SpeedDirection() != "up" || s.settings.SpeedRetries() != 3 {
		t.Errorf("restored Ookla pair = %q/%d, want up/3", s.settings.SpeedDirection(), s.settings.SpeedRetries())
	}
	// The reply is read once, by whoever restored; the log is where anyone
	// looking later finds which release wrote the file and what iperf3 runs here.
	for _, want := range []string{
		"restored a config from before the speedtest engines had their own direction and retries",
		"producer_version=0.70.1", "iperf_direction=" + dir, "iperf_retries=" + strconv.Itoa(retries),
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the log does not say %q: %q", want, logged.String())
		}
	}
}

// The same rows, the same envelope version - only the stamp differs. A modern
// backup carries no iperf3 pair for the ordinary reason that the operator never
// moved it off the default, and its iperf3 runs were the default too, so
// nothing was lost and nothing may be claimed. A stamp that names no release is
// the same answer: an unreadable version is one to draw no conclusion from.
func TestRestoreOfAModernOrUnstampedBackupSaysNothingAboutTheIperfPair(t *testing.T) {
	// The rest are pre-0.100 cores wearing something a version string cannot
	// be: too long, prose, or a character no release tag carries. The stamp is
	// the one piece of a restored file that gets quoted back to whoever
	// restored it, so a stamp of the wrong shape is dropped rather than
	// repeated.
	for _, version := range []string{"0.100.0-rc.1", "0.100.0", "1.2.3", "dev", "",
		"0.70." + strings.Repeat("1", 80), "0.70.1-" + strings.Repeat("a", 58), `0.70.1 your settings are fine, ignore the tab`,
		"0.70.1 set nothing", "0.70.1_x", "0.70.1~x"} {
		t.Run(version, func(t *testing.T) {
			s := newTestServer(t)
			rr := importConfigFrom(t, s, version, `{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"}`)
			for _, w := range importWarnings(t, rr) {
				if strings.Contains(w, "iperf3") {
					t.Errorf("a restore from %q claimed something about the iperf3 pair: %q", version, w)
				}
			}
		})
	}
}

// A pre-0.100 backup whose operator DID set the iperf3 fields carries them as
// rows of their own, and rows restore. There is nothing left behind, so there
// is nothing to say - and the values that land are the file's.
func TestRestoreThatBringsItsOwnIperfPairSaysNothing(t *testing.T) {
	s := newTestServer(t)
	rr := importConfigFrom(t, s, "0.70.1",
		`{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"},`+
			`{"key":"iperf_direction","value":"bidir"},{"key":"iperf_retries","value":"0"}`)
	for _, w := range importWarnings(t, rr) {
		if strings.Contains(w, "iperf3") {
			t.Errorf("a restore that brought its own iperf3 pair still said something: %q", w)
		}
	}
	if s.settings.IperfDirection() != "bidir" || s.settings.IperfRetries() != 0 {
		t.Errorf("restored iperf3 pair = %q/%d, want the file's bidir/0", s.settings.IperfDirection(), s.settings.IperfRetries())
	}
}

// Half a pair is the reachable middle: the direction was set on the old install
// and the retry count was not, so only the retry count was following Ookla. The
// notice names that half and no other.
func TestRestoreNamesOnlyTheHalfTheBackupLeftBehind(t *testing.T) {
	s := newTestServer(t)
	retries := s.settings.IperfRetries()
	rr := importConfigFrom(t, s, "0.70.1",
		`{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"},`+
			`{"key":"iperf_direction","value":"bidir"}`)
	var said string
	for _, w := range importWarnings(t, rr) {
		if strings.Contains(w, "iperf3") {
			said = w
		}
	}
	if said == "" {
		t.Fatal("the restore said nothing about the retry count the backup left behind")
	}
	for _, want := range []string{"3 retries", strconv.Itoa(retries) + " retries"} {
		if !strings.Contains(said, want) {
			t.Errorf("the notice does not name %s: %q", want, said)
		}
	}
	// The preamble says the two engines "shared one direction and retry count",
	// so the word itself is expected; what must not appear is a direction VALUE,
	// since the backup brought that half.
	if strings.Contains(said, `direction "`) {
		t.Errorf("the notice names a direction the backup did carry: %q", said)
	}
	if s.settings.IperfDirection() != "bidir" || s.settings.IperfRetries() != retries {
		t.Errorf("iperf3 pair = %q/%d, want the file's bidir with this install's %d retries",
			s.settings.IperfDirection(), s.settings.IperfRetries(), retries)
	}
}

// A restore whose reload failed still says what the file could not bring, and
// what it says is still true: the values it names are the ones this install
// runs, a failed reload leaves them where they were, and the iperf3 rows the
// file never carried are what the next start reads. The notice reads no table,
// so there is no read to fail and nothing to guess.
func TestARestoreWhoseReloadFailedStillNamesWhatStays(t *testing.T) {
	s := newTestServer(t)
	dir, retries := s.settings.IperfDirection(), s.settings.IperfRetries()
	exhaustReconcileBudget(t)
	rr := importConfigFrom(t, s, "0.70.1", `{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"}`)
	said := iperfNotice(warningsOf(t, rr))
	if said == "" {
		t.Fatalf("a restore whose reload failed said nothing about the iperf3 pair (HTTP %d %s)",
			rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	// The restart: what the notice said stays must be what runs.
	if err := s.settings.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s.settings.IperfDirection() != dir || s.settings.IperfRetries() != retries {
		t.Errorf("after a restart iperf3 = %q/%d, want the untouched %q/%d",
			s.settings.IperfDirection(), s.settings.IperfRetries(), dir, retries)
	}
	for _, want := range []string{strconv.Quote(dir), strconv.Itoa(retries) + " retries"} {
		if !strings.Contains(said, want) {
			t.Errorf("after a restart iperf3 runs %s, and the notice does not say so: %q", want, said)
		}
	}
}

// The notice is read by whoever just restored, so it has to name the right half
// on both machines and count in English: one retry is not "1 retries".
func TestTheSharedPairNoticeNamesTheHalfAndCountsInEnglish(t *testing.T) {
	for _, tc := range []struct {
		name              string
		noDir, noRetries  bool
		dir               string
		retries           int
		hereDir           string
		hereRetries       int
		want, wantMissing []string
	}{
		{"both halves", true, true, "up", 3, "both", 1,
			[]string{`set to direction "up" and 3 retries.`, "keeps those only", "put them;", `this install's direction "both" and 1 retry.`},
			[]string{"1 retries", "3 retry."}},
		{"both halves, one retry there", true, true, "down", 1, "both", 3,
			[]string{`set to direction "down" and 1 retry.`, `this install's direction "both" and 3 retries.`},
			[]string{"1 retries", "3 retry."}},
		{"the direction only", true, false, "up", 3, "bidir", 2,
			[]string{`set to direction "up".`, "keeps that only", "put it;", `this install's direction "bidir".`},
			[]string{"3 retries", "2 retries"}},
		{"the retry count only", false, true, "up", 3, "bidir", 0,
			[]string{"set to 3 retries.", "keeps that only", "put it;", "this install's 0 retries."},
			[]string{`direction "`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sharedIperfPairWarning("0.70.1", tc.noDir, tc.noRetries, tc.dir, tc.retries, tc.hereDir, tc.hereRetries)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("notice does not contain %q: %s", want, got)
				}
			}
			for _, missing := range tc.wantMissing {
				if strings.Contains(got, missing) {
					t.Errorf("notice should not contain %q: %s", missing, got)
				}
			}
		})
	}
}

// iperfNotice is the restore's shared-pair notice among its warnings, or "".
func iperfNotice(warnings []string) string {
	for _, w := range warnings {
		if strings.Contains(w, "iperf3 tab") {
			return w
		}
	}
	return ""
}

// setHere gives this install values of its own, the way a save from its
// settings drawer does, before anything is restored onto it.
func setHere(t *testing.T, s *Server, p settings.Patch) {
	t.Helper()
	if _, err := s.settings.Update(context.Background(), p); err != nil {
		t.Fatalf("set this install's own values: %v", err)
	}
}

// A backup from a release that shared the pair can still carry none of it: its
// operator never moved Ookla off the defaults, so iperf3 there ran the defaults
// too, and the file holds only what they did change. Restored onto an install
// that set its OWN Ookla pair, the Ookla rows in the table afterwards are this
// install's, not the backup's - there is nothing the file failed to bring, and
// a notice saying the restore landed Ookla's pair would be untrue. Onto an
// install whose own iperf3 differs from those defaults it is the same answer:
// the file carried nothing iperf3 ran on.
func TestRestoreOfABackupWithNoOoklaPairSaysNothingAboutTheIperfPair(t *testing.T) {
	const rows = `{"key":"latency_interval_s","value":"60"},{"key":"latency_enabled","value":"0"}`

	s := newTestServer(t)
	up, three := "up", 3
	setHere(t, s, settings.Patch{SpeedDirection: &up, SpeedRetries: &three})
	dir, retries := s.settings.IperfDirection(), s.settings.IperfRetries()
	rr := importConfigFrom(t, s, "0.70.1", rows)
	if said := iperfNotice(importWarnings(t, rr)); said != "" {
		t.Errorf("a backup that carried no Ookla direction or retries was said to have left the iperf3 pair behind: %q", said)
	}
	if s.settings.SpeedDirection() != "up" || s.settings.SpeedRetries() != 3 {
		t.Errorf("this install's own Ookla pair = %q/%d after the restore, want the up/3 it set",
			s.settings.SpeedDirection(), s.settings.SpeedRetries())
	}
	if s.settings.IperfDirection() != dir || s.settings.IperfRetries() != retries {
		t.Errorf("iperf3 pair = %q/%d after the restore, want the untouched %q/%d",
			s.settings.IperfDirection(), s.settings.IperfRetries(), dir, retries)
	}

	own := newTestServer(t)
	bidir, two := "bidir", 2
	setHere(t, own, settings.Patch{IperfDirection: &bidir, IperfRetries: &two})
	rr = importConfigFrom(t, own, "0.70.1", rows)
	if said := iperfNotice(importWarnings(t, rr)); said != "" {
		t.Errorf("a backup that carried no Ookla direction or retries, restored over an iperf3 pair of this install's own, still spoke: %q", said)
	}
}

// The same misreading one half at a time: this install set its own Ookla
// direction, and the backup carries a retry count and nothing else. The
// direction in the table afterwards is this install's, so the notice speaks of
// the retry count alone - what the machine it came from ran, and what stays.
func TestRestoreNamesOnlyTheHalfTheBackupCarried(t *testing.T) {
	s := newTestServer(t)
	up := "up"
	setHere(t, s, settings.Patch{SpeedDirection: &up})
	retries := s.settings.IperfRetries()
	rr := importConfigFrom(t, s, "0.70.1", `{"key":"speed_retries","value":"3"}`)
	said := iperfNotice(importWarnings(t, rr))
	if said == "" {
		t.Fatal("the retry count the backup carried only as Ookla's was not mentioned")
	}
	for _, want := range []string{"3 retries", strconv.Itoa(retries) + " retries"} {
		if !strings.Contains(said, want) {
			t.Errorf("the notice does not name %s: %q", want, said)
		}
	}
	if strings.Contains(said, `direction "`) {
		t.Errorf("the notice names a direction, and the backup carried none - the one in the table is this install's own: %q", said)
	}
}

// A shared-pair backup whose Ookla values are what iperf3 already runs here has
// nothing to report: iperf3 on the machine it came from ran exactly what it
// runs on this one, and "its runs would otherwise change shape here" would send
// an operator looking for a difference that is not there. Where one half
// matches and the other does not, only the one that differs is named.
func TestRestoreSaysNothingOfAHalfThatAlreadyMatches(t *testing.T) {
	s := newTestServer(t)
	dir, retries := s.settings.IperfDirection(), s.settings.IperfRetries()
	rr := importConfigFrom(t, s, "0.70.1",
		`{"key":"speed_direction","value":`+strconv.Quote(dir)+`},{"key":"speed_retries","value":"`+strconv.Itoa(retries)+`"}`)
	if said := iperfNotice(importWarnings(t, rr)); said != "" {
		t.Errorf("a backup whose shared pair is exactly what iperf3 runs here (%s/%d) was said to change its runs: %q", dir, retries, said)
	}
	rr = importConfigFrom(t, s, "0.70.1",
		`{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"`+strconv.Itoa(retries)+`"}`)
	said := iperfNotice(importWarnings(t, rr))
	if said == "" {
		t.Fatal("the direction that differs was not mentioned")
	}
	if strings.Contains(said, strconv.Itoa(retries)+" retr") {
		t.Errorf("the notice names the retry count, which already matched: %q", said)
	}

	// An install whose OWN iperf3 already runs the backup's values - rows of its
	// own, not defaults - is the same answer.
	own := newTestServer(t)
	up, three := "up", 3
	setHere(t, own, settings.Patch{IperfDirection: &up, IperfRetries: &three})
	rr = importConfigFrom(t, own, "0.70.1", `{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"}`)
	if said := iperfNotice(importWarnings(t, rr)); said != "" {
		t.Errorf("an install already running iperf3 at the backup's up/3 was told its runs would change shape: %q", said)
	}
}

// The mirror case: this install set iperf3 itself, and the machine the backup
// came from ran iperf3 on its Ookla pair. The file cannot bring that pair here
// either - this install's own rows stay, as they must - and an operator who
// restores to reproduce that machine is owed the same word as one restoring
// over the defaults. The notice names the values that stay.
func TestRestoreOfASharedPairBackupSpeaksOverAnIperfPairOfThisInstallsOwn(t *testing.T) {
	s := newTestServer(t)
	bidir, two := "bidir", 2
	setHere(t, s, settings.Patch{IperfDirection: &bidir, IperfRetries: &two})
	rr := importConfigFrom(t, s, "0.70.1", `{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"}`)
	warnings := importWarnings(t, rr)
	said := iperfNotice(warnings)
	if said == "" {
		t.Fatalf("a restore that could not bring the backup's iperf3 pair onto an install with a pair of its own said nothing; warnings = %q", warnings)
	}
	for _, want := range []string{"0.70.1", `"up"`, "3 retries", `"bidir"`, "2 retries", "iperf3 tab"} {
		if !strings.Contains(said, want) {
			t.Errorf("the notice does not name %s: %q", want, said)
		}
	}
	if s.settings.IperfDirection() != "bidir" || s.settings.IperfRetries() != 2 {
		t.Errorf("this install's own iperf3 pair = %q/%d after the restore, want the bidir/2 it set",
			s.settings.IperfDirection(), s.settings.IperfRetries())
	}

	// A backup with only a retry count names only that half here too: the
	// direction this install set was never in question.
	half := newTestServer(t)
	setHere(t, half, settings.Patch{IperfDirection: &bidir, IperfRetries: &two})
	rr = importConfigFrom(t, half, "0.70.1", `{"key":"speed_retries","value":"3"}`)
	said = iperfNotice(importWarnings(t, rr))
	if said == "" || !strings.Contains(said, "3 retries") || !strings.Contains(said, "2 retries") {
		t.Errorf("the retry count the backup carried only as Ookla's was not named with the one that stays: %q", said)
	}
	if strings.Contains(said, `direction "`) {
		t.Errorf("the notice names a direction, and the backup carried none: %q", said)
	}
}

// The version stamp is read only when it is what our exporter writes - a plain
// string - and the rest of the envelope is walked past exactly as before: a
// stamp inside an array or an object, a number, or a version-shaped string
// under some other key says nothing, and the restore itself goes through.
func TestOnlyAPlainVersionStampIsRead(t *testing.T) {
	for _, envelope := range []string{
		`"producer_version":["0.70.1"]`,
		`"producer_version":{"version":"0.70.1"}`,
		`"producer_version":70`,
		`"producer_version":"0.100.0","note":"0.70.1"`,
		`"note":"0.70.1"`,
	} {
		t.Run(envelope, func(t *testing.T) {
			s := newTestServer(t)
			body := `{"pingularity_export":1,` + envelope + `,"categories":["config"],"config":[` +
				`{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"}]}`
			rr := postImportBody(t, s, "config=1", strings.NewReader(body))
			if said := iperfNotice(importWarnings(t, rr)); said != "" {
				t.Errorf("a stamp that is not a plain version string was read: %q", said)
			}
			if s.settings.SpeedDirection() != "up" {
				t.Errorf("the restore did not land: Ookla direction = %q, want up", s.settings.SpeedDirection())
			}
		})
	}
}

// The file's pair is read as the release that wrote it read it, whatever else
// the file carries. A hand-edited or crafted backup can list the split marker
// among its rows; the store refuses it on the way in, and it may not make the
// file's own pair read as already split either.
func TestRestoreReadsTheBackupsPairAsItsReleaseDid(t *testing.T) {
	s := newTestServer(t)
	rr := importConfigFrom(t, s, "0.70.1",
		`{"key":"engine_split_done","value":"1"},{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"}`)
	said := iperfNotice(importWarnings(t, rr))
	if said == "" {
		t.Fatal("a backup listing the split marker among its rows had its shared pair read as split")
	}
	for _, want := range []string{`"up"`, "3 retries"} {
		if !strings.Contains(said, want) {
			t.Errorf("the notice does not name %s: %q", want, said)
		}
	}
}

// Rows that never landed restored nothing. A config category the store refuses
// - a row with a column this build does not know - is a failed restore with its
// own error, and the notice may not tell that operator Ookla's pair came back.
func TestARestoreWhoseConfigDidNotLandSaysNothingAboutTheIperfPair(t *testing.T) {
	s := newTestServer(t)
	rr := importConfigFrom(t, s, "0.70.1",
		`{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"},{"key":"latency_enabled","value":"0","since":1}`)
	if rr.Code == http.StatusOK {
		t.Fatalf("precondition: a config row with an unknown column must fail the restore, got 200: %s", rr.Body.String())
	}
	if s.settings.SpeedDirection() == "up" {
		t.Fatal("precondition: none of the refused category's rows may land, yet the Ookla direction is up")
	}
	if said := iperfNotice(warningsOf(t, rr)); said != "" {
		t.Errorf("a restore whose config never landed said Ookla's pair was restored: %q", said)
	}
}

// The stamps a build of one of those releases carries are version strings with
// letters, dashes and a plus in them - git describe off the tag, a snapshot
// suffix, a dirty tree - up to the longest a stamp is kept at. Each is read, so
// the notice speaks for it; a stamp one character past that is dropped (see
// TestRestoreOfAModernOrUnstampedBackupSaysNothingAboutTheIperfPair).
func TestRestoreReadsTheStampsAReleaseBuildCarries(t *testing.T) {
	longest := "0.70.1-" + strings.Repeat("a", 57)
	for _, version := range []string{"v0.70.1-3-gabc1234", "0.70.2-SNAPSHOT-e3778ad", "0.70.1+dirty", longest} {
		t.Run(version, func(t *testing.T) {
			s := newTestServer(t)
			rr := importConfigFrom(t, s, version, `{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"}`)
			if said := iperfNotice(importWarnings(t, rr)); !strings.Contains(said, "Pingularity "+version+",") {
				t.Errorf("a backup stamped %q was not read as the release it names: %q", version, said)
			}
		})
	}
}

// Our exporters write every value as a string, and so did every release before
// 0.100, but a hand-edited backup can carry a number or a boolean and the store
// lands it as text all the same: a whole number as its digits, true as 1, a
// fraction as text no retry count reads. The release that wrote the file
// restored it that way too and ran on what landed, so the notice reads each row
// as it lands. A count the file carried as iperf3's own is not one it "keeps
// only in Ookla's settings", and it is not this install's either.
func TestRestoreReadsAValueAsTheStoreLandsIt(t *testing.T) {
	for _, tc := range []struct {
		name, rows string
		landed     int    // iperf3 retries here after the restore
		direction  bool   // the old machine's direction "up" is named as left behind
		count      string // the old machine's count as the notice names it; "" = not named
	}{
		{"a whole number as iperf3's count", `{"key":"speed_retries","value":"3"},{"key":"iperf_retries","value":2}`, 2, true, ""},
		{"a large whole number as iperf3's count", `{"key":"speed_retries","value":"1"},{"key":"iperf_retries","value":1000000}`, 3, true, ""},
		{"a whole number past int64 as iperf3's count", `{"key":"speed_retries","value":"3"},{"key":"iperf_retries","value":1e300}`, 0, true, "3 retries"},
		{"a whole number below int64 as iperf3's count", `{"key":"speed_retries","value":"3"},{"key":"iperf_retries","value":-1e300}`, 0, true, "3 retries"},
		{"two to the sixty-third as iperf3's count", `{"key":"speed_retries","value":"3"},{"key":"iperf_retries","value":9223372036854775808}`, 0, true, "3 retries"},
		{"true as iperf3's count", `{"key":"speed_retries","value":"3"},{"key":"iperf_retries","value":true}`, 1, true, ""},
		{"a fraction as iperf3's count", `{"key":"speed_retries","value":"3"},{"key":"iperf_retries","value":2.5}`, 0, true, "3 retries"},
		{"a whole number as Ookla's count", `{"key":"speed_retries","value":3}`, 0, true, "3 retries"},
		{"true as Ookla's count", `{"key":"speed_retries","value":true}`, 0, true, "1 retry"},
		{"false as Ookla's count", `{"key":"speed_retries","value":false}`, 0, true, ""},
		{"null as iperf3's direction", `{"key":"speed_retries","value":"3"},{"key":"iperf_direction","value":null}`, 0, true, "3 retries"},
		{"a number as iperf3's direction", `{"key":"speed_retries","value":"3"},{"key":"iperf_direction","value":2.5}`, 0, false, "3 retries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			if got := s.settings.IperfRetries(); got != 0 {
				t.Fatalf("precondition: this install's iperf3 runs %d retries, want 0", got)
			}
			rr := importConfigFrom(t, s, "0.70.1", `{"key":"speed_direction","value":"up"},`+tc.rows)
			said := iperfNotice(importWarnings(t, rr))
			if got := s.settings.IperfRetries(); got != tc.landed {
				t.Fatalf("precondition: iperf3 retries after the restore = %d, want the %d that landed", got, tc.landed)
			}
			var want string
			switch {
			case tc.direction && tc.count != "":
				want = `set to direction "up" and ` + tc.count + "."
			case tc.direction:
				want = `set to direction "up".`
			case tc.count != "":
				want = "set to " + tc.count + "."
			}
			if want == "" && said != "" {
				t.Errorf("nothing was left behind, yet the restore said: %q", said)
			}
			if want != "" && !strings.Contains(said, want) {
				t.Errorf("the notice does not say %q: %q", want, said)
			}
		})
	}
}

// A row that is there but does not read as a count is read the way the release
// that wrote the file read it: that machine fell back to Ookla's count, and here
// the row lands as one this install reads straight past to its own - so that
// count was left behind as surely as a missing row's, and the notice says so.
func TestRestoreNamesARetryCountItsBackupHeldBesideARowNoBuildReads(t *testing.T) {
	s := newTestServer(t)
	retries := s.settings.IperfRetries()
	rr := importConfigFrom(t, s, "0.70.1",
		`{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"},{"key":"iperf_retries","value":""}`)
	said := iperfNotice(importWarnings(t, rr))
	for _, want := range []string{"3 retries", strconv.Itoa(retries) + " retries"} {
		if !strings.Contains(said, want) {
			t.Errorf("the notice does not name %s: %q", want, said)
		}
	}
	if s.settings.IperfRetries() != retries {
		t.Errorf("iperf3 retries after the restore = %d, want this install's %d", s.settings.IperfRetries(), retries)
	}
}

// A refusal takes only the rows that never landed out of what the restore
// says. Our exporters write config last, so the category that fails is config
// itself or nothing after it; a file put together by hand can carry config
// first, and when a later category is refused that config has already landed
// and the reply says so - partial, with what committed. What the landed config
// could not bring belongs in that reply as much as in a clean one.
func TestAPartialRestoreStillNamesWhatItsLandedConfigCouldNotBring(t *testing.T) {
	s := newTestServer(t)
	body := `{"pingularity_export":1,"producer_version":"0.70.1","categories":["config","downtime"],` +
		`"config":[{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"}],` +
		`"downtime":[{"ts":1000,"type":"down","duration_s":60,"bogus":1}]}`
	rr := postImportBody(t, s, "config=1&downtime=1", strings.NewReader(body))
	var reply struct {
		Partial  bool     `json:"partial"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &reply); err != nil || !reply.Partial {
		t.Fatalf("precondition: a refused later category makes a partial restore; HTTP %d %s", rr.Code, rr.Body.String())
	}
	if s.settings.SpeedDirection() != "up" || s.settings.SpeedRetries() != 3 {
		t.Fatalf("precondition: the config ahead of it landed; Ookla pair = %q/%d", s.settings.SpeedDirection(), s.settings.SpeedRetries())
	}
	if said := iperfNotice(reply.Warnings); said == "" {
		t.Errorf("the config landed without its iperf3 pair, and the partial reply said nothing of it: %s", rr.Body.String())
	}
}

// The same inside one category. A restore commits as it goes, a batch at a time
// (importArray), so a config category long enough to commit its first batch
// before a later row is refused has landed everything that batch held - the
// Ookla pair included, in force from that moment and after any restart. Only
// the batch the store refused takes its rows' word with it.
func TestARestoreRefusedPastItsFirstCommittedBatchStillNamesWhatLanded(t *testing.T) {
	s := newTestServer(t)
	var body strings.Builder
	body.WriteString(`{"pingularity_export":1,"producer_version":"0.70.1","categories":["config"],"config":[` +
		`{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"}`)
	for i := 0; i < 4998; i++ { // with the pair, one whole batch
		body.WriteString(`,{"key":"filler_` + strconv.Itoa(i) + `","value":"1"}`)
	}
	body.WriteString(`,{"key":"latency_enabled","value":"0","since":1}]}`)
	rr := postImportBody(t, s, "config=1", strings.NewReader(body.String()))
	var reply struct {
		Partial   bool           `json:"partial"`
		Committed map[string]int `json:"committed"`
		Warnings  []string       `json:"warnings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &reply); err != nil || !reply.Partial || reply.Committed["config"] != 5000 {
		t.Fatalf("precondition: the first batch commits and a later row is refused; HTTP %d %s", rr.Code, rr.Body.String())
	}
	if s.settings.SpeedDirection() != "up" || s.settings.SpeedRetries() != 3 {
		t.Fatalf("precondition: the committed batch's Ookla pair is in force; Ookla pair = %q/%d", s.settings.SpeedDirection(), s.settings.SpeedRetries())
	}
	if said := iperfNotice(reply.Warnings); said == "" {
		t.Errorf("a committed batch landed the Ookla pair without its iperf3 half, and the partial reply said nothing of it: %s",
			strings.TrimSpace(rr.Body.String()))
	}
}

// A key the file carries twice, in two batches that both commit, is stored as
// its second value - the store replaces a settings row by key - so that is the
// one the notice names.
func TestARestoreNamesTheValueItsLaterBatchLanded(t *testing.T) {
	s := newTestServer(t)
	var body strings.Builder
	body.WriteString(`{"pingularity_export":1,"producer_version":"0.70.1","categories":["config"],"config":[` +
		`{"key":"speed_direction","value":"up"},{"key":"speed_retries","value":"3"}`)
	for i := 0; i < 4998; i++ { // with the pair, one whole batch
		body.WriteString(`,{"key":"filler_` + strconv.Itoa(i) + `","value":"1"}`)
	}
	body.WriteString(`,{"key":"speed_direction","value":"down"}]}`)
	rr := postImportBody(t, s, "config=1", strings.NewReader(body.String()))
	if rr.Code != http.StatusOK || s.settings.SpeedDirection() != "down" {
		t.Fatalf("precondition: both batches commit and the later direction is in force; HTTP %d, Ookla direction %q: %s",
			rr.Code, s.settings.SpeedDirection(), strings.TrimSpace(rr.Body.String()))
	}
	said := iperfNotice(importWarnings(t, rr))
	if !strings.Contains(said, `set to direction "down"`) {
		t.Errorf("the store landed direction down, and the notice names another: %q", said)
	}
}

// skipJSONValue hands its callback every token it consumes, in order and closing
// delimiters included - each one renews the read allowance, and the version
// stamp is kept off the first - and stops at the end of that one value, leaving
// the decoder on whatever follows.
func TestSkipJSONValueHandsOverEveryTokenItConsumes(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"a":[1,{"b":"x"}],"c":null} "next"`))
	var got []string
	if err := skipJSONValue(dec, func(tok json.Token) {
		switch v := tok.(type) {
		case json.Delim:
			got = append(got, v.String())
		case string:
			got = append(got, strconv.Quote(v))
		case float64:
			got = append(got, strconv.FormatFloat(v, 'g', -1, 64))
		case nil:
			got = append(got, "null")
		default:
			t.Fatalf("unexpected token %#v", tok)
		}
	}); err != nil {
		t.Fatalf("skipJSONValue: %v", err)
	}
	want := []string{"{", `"a"`, "[", "1", "{", `"b"`, `"x"`, "}", "]", `"c"`, "null", "}"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("tokens handed over = %v, want %v", got, want)
	}
	if tok, err := dec.Token(); err != nil || tok != "next" {
		t.Errorf("after the value the decoder reads %v (%v), want the \"next\" that follows it", tok, err)
	}
}
