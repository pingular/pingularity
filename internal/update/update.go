// Package update polls a small public endpoint for the latest released version
// and reports whether the running build is behind it.
//
// Best-effort and fail-safe: the check runs only in a background loop that
// caches the result, so Status() is a non-blocking read and a dead/slow/garbage
// endpoint can never stall the dashboard. Any failure - unreachable, timeout,
// non-200, bad body, bogus version - is discarded, keeping the last-known-good
// result, and retried next cycle. A "dev" (unstamped) build has no semver, so it
// never reports an update. An update is flagged only on a release strictly newer
// than the running build; uncertainty always resolves to "no update".
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// latestURL is the public JSON endpoint serving {"version":"X.Y.Z", ...}.
// CHANGE ME to wherever you publish the latest version. It MUST be public (not
// behind Cloudflare Access) and should be CDN-cached. See the deploy notes.
const latestURL = "https://update.pingularity.dev/latest.json"

// releasesURL is where the "View release" button points. Hardcoded, not taken
// from the endpoint response, so a tampered file can't redirect the link. An
// OWNED host, because this link ships baked into every binary forever: it
// currently forwards to the GitHub Releases page and can be repointed (e.g.
// at the website's install page) without stranding old installs.
const releasesURL = "https://install.pingularity.dev"

const (
	checkInterval = 24 * time.Hour
	checkTimeout  = 10 * time.Second
	startupDelay  = 5 * time.Second // keep boot quiet; first real check lands a few seconds in
	maxBody       = 64 << 10        // the real file is <1KB; cap so a garbage body can't bloat memory
)

// firstPollRetry schedules the checks before the FIRST success. A fresh
// install's poll is what makes it visible on the fleet dashboard, and the
// daily cadence turned one unlucky boot - the feed briefly unreachable in the
// seconds after startup - into a whole day of invisibility. Retry on a short
// ladder instead, settling at hourly until the first success (an install
// during an outage appears when the line comes back); after that the daily
// cadence owns the schedule. Bounded and tiny: one <1KB GET per step.
var firstPollRetry = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// semverRE matches a leading MAJOR.MINOR.PATCH, ignoring an optional "v" prefix
// and any pre-release/build suffix. Used both to validate and to parse.
var semverRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// Status is the snapshot the web layer serves on /api/status. Available is true
// only when checking is enabled AND a valid release strictly newer than the
// running build was seen - so dev builds, disabled checks, and failures all
// resolve to Available=false.
type Status struct {
	Current     string `json:"current"`
	Latest      string `json:"latest,omitempty"`
	Available   bool   `json:"available"`
	URL         string `json:"url"`
	Enabled     bool   `json:"enabled"`
	CheckedUnix int64  `json:"checked_unix,omitempty"` // last SUCCESSFUL check; 0 = never
}

// Checker polls latestURL on an interval and caches the latest known version.
type Checker struct {
	current   string
	url       string
	enabledFn func() bool
	client    *http.Client
	log       *slog.Logger

	mu      sync.RWMutex
	latest  string    // last successfully fetched release version (no "v"); "" until first success
	checked time.Time // time of last successful fetch
	kick    chan struct{}
}

// New builds a Checker for the running version. enabledFn reads the live toggle
// (nil = always enabled). It does not start anything; call Loop in a goroutine.
func New(current string, enabledFn func() bool, log *slog.Logger) *Checker {
	if log == nil {
		log = slog.Default()
	}
	// The check fetches one fixed HTTPS URL that returns JSON directly - no redirect
	// is ever expected. Don't follow 3xx: a redirect (from a DNS hijack or a tampered
	// edge) could pivot the fetch to an internal address, so surface it as a non-200
	// failed check instead of chasing it. And ignore any HTTP(S)_PROXY env var so the
	// notify-only check can't be silently routed through an interceptor.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &Checker{
		current:   current,
		url:       latestURL,
		enabledFn: enabledFn,
		client: &http.Client{
			Timeout:       checkTimeout,
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log:  log,
		kick: make(chan struct{}, 1),
	}
}

func (c *Checker) enabled() bool { return c.enabledFn == nil || c.enabledFn() }

// Status returns the cached snapshot. It never touches the network.
func (c *Checker) Status() Status {
	c.mu.RLock()
	latest, checked := c.latest, c.checked
	c.mu.RUnlock()
	en := c.enabled()
	st := Status{Current: c.current, Latest: latest, URL: releasesURL, Enabled: en}
	if !checked.IsZero() {
		st.CheckedUnix = checked.Unix()
	}
	st.Available = en && latest != "" && newer(c.current, latest)
	return st
}

// CheckNow asks the loop to poll promptly (e.g. just after the toggle is turned
// on) instead of waiting for the next daily tick. Non-blocking.
func (c *Checker) CheckNow() {
	select {
	case c.kick <- struct{}{}:
	default: // a check is already queued; one is enough
	}
}

// Loop runs the periodic check until ctx is cancelled.
func (c *Checker) Loop(ctx context.Context) {
	t := time.NewTimer(startupDelay)
	defer t.Stop()
	attempts := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
			// The kick re-arms the schedule too. It used to only checkOnce:
			// enable-after-disabled with a failing feed then sat out whatever
			// the timer held from the DISABLED era - up to the full daily
			// interval - instead of the firstPollRetry ladder.
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(c.checkAndNext(ctx, &attempts))
		case <-t.C:
			t.Reset(c.checkAndNext(ctx, &attempts))
		}
	}
}

// checkAndNext runs one check and picks the delay before the next scheduled
// one - the single re-arm path shared by the timer tick and the CheckNow kick.
func (c *Checker) checkAndNext(ctx context.Context, attempts *int) time.Duration {
	c.checkOnce(ctx)
	return c.nextInterval(attempts)
}

// nextInterval picks the delay before the next scheduled check: the
// firstPollRetry ladder while a release build with checking enabled has never
// succeeded, the daily cadence otherwise. A disabled toggle or a dev build
// never fetches, so retrying fast there would spin for nothing - they wait the
// daily tick (and the toggle's kick re-checks immediately on enable anyway).
func (c *Checker) nextInterval(attempts *int) time.Duration {
	if !c.enabled() || !isRelease(c.current) {
		return checkInterval
	}
	c.mu.RLock()
	succeeded := !c.checked.IsZero()
	c.mu.RUnlock()
	if succeeded {
		return checkInterval
	}
	d := firstPollRetry[min(*attempts, len(firstPollRetry)-1)]
	*attempts++
	return d
}

// checkOnce performs one best-effort poll. Any failure leaves the cached
// last-known-good result untouched and returns; the next cycle retries.
func (c *Checker) checkOnce(ctx context.Context) {
	if !c.enabled() {
		return // toggle off: don't hit the endpoint
	}
	if !isRelease(c.current) {
		return // dev/unstamped build: nothing to compare against
	}
	v, err := c.fetch(ctx)
	if err != nil {
		// Quiet by design: this runs during outages, so don't spam the log.
		c.log.Debug("update check failed", "err", err)
		return
	}
	c.mu.Lock()
	c.latest, c.checked = v, time.Now()
	c.mu.Unlock()
	c.log.Debug("update check ok", "latest", v)
}

// fetch GETs the endpoint and returns a validated release version (no "v").
func (c *Checker) fetch(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var body struct {
		Version string `json:"version"`
	}
	// LimitReader caps an oversized body; a truncated/non-JSON payload then fails
	// the decode and counts as a failed check (caller keeps last-known).
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return "", err
	}
	if !isRelease(body.Version) {
		return "", fmt.Errorf("bogus version %q", body.Version)
	}
	// Return only the matched MAJOR.MINOR.PATCH, never the raw field: the regex
	// is unanchored at the end, so a tampered file could otherwise smuggle up to
	// maxBody of junk after a valid prefix into the cached, served version.
	return strings.TrimPrefix(semverRE.FindString(body.Version), "v"), nil
}

// isRelease reports whether s looks like a real semver, excluding "dev" and junk
// on both ends (our build and the endpoint's claim).
func isRelease(s string) bool { return semverRE.MatchString(s) }

// newer reports whether latest is strictly greater than current as a semver.
// The numeric MAJOR.MINOR.PATCH decides it; on a tie a PRERELEASE current (e.g.
// "1.0.0-rc.1") ranks below the same-numbered final ("1.0.0"), per SemVer
// precedence - so an rc tester is offered the stable they were testing without
// anyone having to inflate the version to force the badge. latest arrives
// stripped to its numeric core (see fetch), so it never carries a prerelease;
// the tie-break is written symmetrically anyway. Unparseable input returns
// false - fail toward "no update" rather than nag on garbage.
func newer(current, latest string) bool {
	c, ok1 := parse(current)
	l, ok2 := parse(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	// Equal core: a final release outranks its own prerelease, and nothing else
	// is an upgrade (a stable never nags toward its own rc, two finals are equal).
	return isPrerelease(current) && !isPrerelease(latest)
}

// isPrerelease reports whether s carries a SemVer prerelease suffix - a hyphen
// immediately after the MAJOR.MINOR.PATCH core, e.g. "1.0.0-rc.1". Build
// metadata ("1.0.0+build") is not a prerelease, and non-semver input never is.
func isPrerelease(s string) bool {
	loc := semverRE.FindStringIndex(s)
	if loc == nil {
		return false
	}
	return strings.HasPrefix(s[loc[1]:], "-")
}

func parse(s string) ([3]int, bool) {
	m := semverRE.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, true
}
