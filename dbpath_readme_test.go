package main

// The README's corrupt-database bullet is where an operator reads what the
// daemon will and will not do with a `-db` path that is not an ordinary file,
// and it is the sentence someone checks before swapping a binary. It said the
// path is never followed through a link. Hold it to what Open actually does, in
// both directions, so neither side can drift alone.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/secret"
	"github.com/pingular/pingularity/internal/store"
)

// The sentence the -db path-shape claim hangs off.
const dbPathBulletMarker = "What the daemon will *not* do is touch a "

// dbLinkOpened is the README's claim that a link to a real file is taken rather
// than refused. dbLinkKeyStays is the clause - in that bullet and in
// CHANGELOG.md's `-db` entry alike - that says where the key and the log go
// when it is.
const (
	dbLinkOpened   = "A symlink to a real file is fine"
	dbLinkKeyStays = "`pingularity.key` and `logs.txt` stay beside the link"
)

func TestREADMEDBPathClaimMatchesWhatOpenAccepts(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.db")
	st, err := store.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	link := filepath.Join(dir, "pingularity.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(link)
	follows := err == nil
	if st != nil {
		st.Close()
	}

	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(b), dbPathBulletMarker)
	if i < 0 {
		t.Fatalf("the -db path-shape bullet is gone from README.md (marker %q)", dbPathBulletMarker)
	}
	bullet := string(b)[i:]
	if j := strings.Index(bullet, "\n- "); j > 0 {
		bullet = bullet[:j]
	}
	if !strings.Contains(bullet, "symlink") {
		t.Fatal("the -db path-shape bullet no longer says anything about a symlink")
	}
	saysItNeverFollows := strings.Contains(bullet, "never followed through")
	if follows == saysItNeverFollows {
		t.Fatalf("README and Open disagree: Open follows a symlink to a real database = %v, but the bullet says the path is never followed through a link = %v\n\n%s",
			follows, saysItNeverFollows, bullet)
	}
	// Not saying "never followed" is only half of it: a bullet rewritten to say a
	// link is refused the same way keeps the word and drops that phrase, and
	// tells an operator whose database sits behind a link that the upgrade will
	// not start. So the claim that the link is taken is held as well.
	if saysItOpens := strings.Contains(unwrapped(bullet), dbLinkOpened); follows != saysItOpens {
		t.Fatalf("README and Open disagree: Open follows a symlink to a real database = %v, but the bullet says %q = %v\n\n%s",
			follows, dbLinkOpened, saysItOpens, bullet)
	}
}

// linkedDatabase lays out the arrangement the -db docs are written for: a
// database moved to another directory, with a link left where -db still
// points. It returns the link's directory, the real file's directory, the link
// and the file.
func linkedDatabase(t *testing.T) (etc, moved, link, target string) {
	t.Helper()
	root := t.TempDir()
	etc, moved = filepath.Join(root, "etc"), filepath.Join(root, "moved")
	for _, d := range []string{etc, moved} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	target = filepath.Join(moved, "pingularity.db")
	st, err := store.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	link = filepath.Join(etc, "pingularity.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this filesystem will not make a symlink: %v", err)
	}
	return etc, moved, link, target
}

// keyAndLogFollowTheDBPathAsTyped reports whether pingularity.key and logs.txt
// land beside a -db link rather than beside the file it names. It asks the two
// functions run() takes their places from, secret.New and ringPath, about the
// -db path exactly as it was passed - which is what main hands them.
func keyAndLogFollowTheDBPathAsTyped(t *testing.T) bool {
	t.Helper()
	etc, moved, link, _ := linkedDatabase(t)
	if _, err := secret.New(link); err != nil {
		t.Fatalf("secret.New through the link: %v", err)
	}
	_, errByLink := os.Stat(filepath.Join(etc, "pingularity.key"))
	_, errByFile := os.Stat(filepath.Join(moved, "pingularity.key"))
	keyByLink := errByLink == nil && errByFile != nil
	logByLink := ringPath(link) == filepath.Join(etc, "logs.txt")
	if keyByLink != logByLink {
		t.Fatalf("the key and the log no longer live together behind a -db link (key beside the link = %v, log at %s); every doc that places them names them as one pair, and has to be rewritten with the code",
			keyByLink, ringPath(link))
	}
	return keyByLink
}

// pointingAtTheFileLosesTheKey reports what an operator gets who points -db at
// the file behind a link instead of at the link: whether the key made beside
// that file is a new one that cannot open what the key beside the link sealed,
// and whether copying the old key across brings those secrets back.
func pointingAtTheFileLosesTheKey(t *testing.T) (newKeyCannotOpen, copyRestores bool) {
	t.Helper()
	etc, moved, link, target := linkedDatabase(t)
	const password = "s3cr3t-lab-pw"
	beside, err := secret.New(link)
	if err != nil {
		t.Fatalf("secret.New through the link: %v", err)
	}
	sealed, err := beside.Seal(password)
	if err != nil {
		t.Fatalf("seal under the key beside the link: %v", err)
	}
	pointedAtFile, err := secret.New(target)
	if err != nil {
		t.Fatalf("secret.New on the file behind the link: %v", err)
	}
	_, openErr := pointedAtFile.Unseal(sealed)
	newKeyCannotOpen = openErr != nil
	key, err := os.ReadFile(filepath.Join(etc, "pingularity.key"))
	if err != nil {
		t.Fatalf("read the key beside the link: %v", err)
	}
	if err := os.WriteFile(filepath.Join(moved, "pingularity.key"), key, 0o600); err != nil {
		t.Fatalf("copy the key across: %v", err)
	}
	copied, err := secret.New(target)
	if err != nil {
		t.Fatalf("secret.New on the file behind the link, key copied across: %v", err)
	}
	plain, err := copied.Unseal(sealed)
	copyRestores = err == nil && plain == password
	return newKeyCannotOpen, copyRestores
}

// Where pingularity.key and logs.txt live is what an operator backs up, restores
// and carries to a bigger disk, and behind a -db link it is not where the
// database is: both stay beside the link, as they did before a link was ever
// resolved. A sentence saying a linked -db "behaves exactly as if you had typed
// the file the link names" sent operators to point -db at that file instead,
// where the daemon finds no key, makes a new one, and every saved iperf3
// password stops decrypting. Hold each place the docs say where the pair lives,
// and what pointing -db at the file costs, to what the build does.
func TestDocsSayWhereTheKeyAndTheLogLiveBehindADBLink(t *testing.T) {
	if !keyAndLogFollowTheDBPathAsTyped(t) {
		t.Fatalf("pingularity.key and logs.txt now follow a -db link to the file it names; README.md, docs/security-model.md and %s all say they stay beside the -db path as typed - rewrite them with the change", changelogPath)
	}
	readme := unwrapped(mustReadRepoFile(t, "README.md"))
	if !strings.Contains(readme, dbLinkKeyStays) {
		t.Errorf("README.md's -db bullet never says %q. It is the sentence that stops an operator pointing -db at the file behind the link, where the daemon makes a new key and the saved iperf3 passwords stop decrypting.", dbLinkKeyStays)
	}
	const introduced = "**`pingularity.key`** (0600)"
	if at := strings.Index(readme, introduced); at < 0 {
		t.Errorf("README.md no longer introduces %s; this test's anchor has rotted", introduced)
	} else if lead := readme[max(0, at-200):at]; !strings.Contains(lead, "Beside the `-db` path") {
		t.Errorf("README.md introduces the key and the log snapshot without saying they sit beside the `-db` path - behind a link, that is not beside the database:\n%s", lead)
	}
	model := unwrapped(mustReadRepoFile(t, "docs/security-model.md"))
	for _, want := range []string{"`pingularity.key` beside the `-db` path", "(`logs.txt`, beside the `-db` path)"} {
		if !strings.Contains(model, want) {
			t.Errorf("docs/security-model.md never says %q; behind a -db link the key and the log are not beside the database, and that page is where someone deciding what to protect reads where they are", want)
		}
	}

	// And what pointing -db at the file costs, which is the half of the sentence
	// that makes it worth reading: a new key, the passwords sealed under the old
	// one unreadable, and the way back. Each clause is held to what secret.New
	// does with the two paths, in both documents that say it.
	newKey, copyBack := pointingAtTheFileLosesTheKey(t)
	changelog := unwrapped(mustReadRepoFile(t, changelogPath))
	for _, doc := range []struct{ name, body string }{{"README.md", readme}, {changelogPath, changelog}} {
		for _, c := range []struct {
			holds  bool
			clause string
		}{
			{newKey, "makes a new one"},
			{newKey, "stop decrypting"},
			{copyBack, "is copied across"},
		} {
			if says := strings.Contains(doc.body, c.clause); says != c.holds {
				t.Errorf("pointing -db at the file behind a link does what %q describes = %v, and %s says it = %v", c.clause, c.holds, doc.name, says)
			}
		}
	}
}
