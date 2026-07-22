// Package notify delivers Pingularity alerts to a user-configured webhook and
// pings a dead-man's-switch URL. It is intentionally dependency-free: a single
// generic webhook covers Discord, Slack, Gotify, Apprise (and thus email,
// Telegram, Pushover, … via Apprise), ntfy, and any receiver that accepts an
// HTTP POST. The body is shaped per well-known host; the generic shape carries
// the alert text under several common keys plus title/type/priority so the usual
// targets render a headline and severity.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/util"
)

// Notifier posts alerts to the webhook returned by URLFn (read live, so the URL
// can change at runtime). A nil/empty URL disables delivery.
type Notifier struct {
	URLFn    func() string
	log      *slog.Logger
	client   *http.Client
	outageMu sync.Mutex // serializes Outage delivery so a flap's down/up can't arrive out of order
}

// New builds a Notifier.
func New(urlFn func() string, log *slog.Logger) *Notifier {
	return &Notifier{URLFn: urlFn, log: log, client: NewClient()}
}

// NewClient builds the HTTP client for outbound notifications. Two SSRF
// defenses:
//
//   - No redirects, so a hostile URL can't bounce into an internal address it
//     couldn't name directly.
//   - dialGuard on every dial checks the resolved remote IP, catching
//     link-local/metadata targets reached via DNS name, odd IP encodings, or
//     DNS rebinding - cases the literal-only ssrfBlocked check misses.
func NewClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dialer.Control = dialGuard
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = dialer.DialContext
	// Drop the inherited ProxyFromEnvironment: an HTTP(S)_PROXY in the daemon's
	// environment would route the POST through a CONNECT/absolute-URI to the proxy,
	// so the connection dialGuard actually inspects is the proxy's address - not the
	// webhook's resolved IP. That lets an attacker-influenced webhook host reach a
	// link-local/metadata target the guard is meant to block. No proxy => dialGuard
	// vets the real destination every dial.
	tr.Proxy = nil
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// NewHeartbeatClient builds the HTTP client for dead-man's-switch pings. Same
// dialGuard and timeout as NewClient, but it FOLLOWS redirects: a heartbeat GET
// (e.g. Healthchecks.io) legitimately bounces (http->https, a 302), and
// rejecting that would mis-flag a healthy ping as down. CheckRedirect re-runs
// ssrfBlocked on each hop so the literal-IP guard covers redirect pivots too.
func NewHeartbeatClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dialer.Control = dialGuard
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = dialer.DialContext
	// No env proxy: a proxied request would be dialed to the proxy's address, so
	// dialGuard would vet the proxy instead of the heartbeat's real destination -
	// letting a crafted host slip a link-local/metadata target past the guard (see
	// NewClient). ssrfBlocked still covers redirect hops.
	tr.Proxy = nil
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Installing a custom CheckRedirect drops the stdlib's hop cap, so
			// re-impose it - a redirect loop must not spin until the timeout.
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if ssrfBlocked(req.URL.String()) {
				return fmt.Errorf("blocked redirect to link-local/metadata destination")
			}
			return nil
		},
	}
}

// awsMetaV6 is AWS's well-known IPv6 metadata prefix (fd00:ec2::/32, holding
// fd00:ec2::254). It is unique-local, not link-local, so the link-local check
// below misses it.
var awsMetaV6 = &net.IPNet{IP: net.ParseIP("fd00:ec2::"), Mask: net.CIDRMask(32, 128)}

// metaLiterals are cloud-metadata endpoints that are neither link-local nor
// RFC1918, so they slip past the link-local check and the intentional LAN
// allowance: Alibaba Cloud (100.100.100.200, in CGNAT space) and Oracle Cloud's
// legacy endpoint (192.0.0.192). Blocked as literals since there's no prefix to
// key off.
var metaLiterals = map[string]bool{
	"100.100.100.200": true, // Alibaba Cloud metadata
	"192.0.0.192":     true, // Oracle Cloud legacy metadata
}

// dialGuard refuses any dial whose resolved remote IP is link-local
// (169.254.0.0/16, fe80::/10 - includes the 169.254.169.254 cloud-metadata
// endpoint) or in the IPv6 cloud-metadata prefix. Running post-DNS on the
// actual address dialed, it catches what the literal-IP ssrfBlocked check
// can't: DNS names resolving to link-local, alternate IP encodings, and DNS
// rebinding (re-checked every dial).
func dialGuard(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("blocked dial to unresolved address %q", address)
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4 // normalize IPv4-mapped IPv6 so the checks below apply
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("blocked link-local destination %s", ip)
	}
	if awsMetaV6.Contains(ip) || metaLiterals[ip.String()] {
		return fmt.Errorf("blocked cloud-metadata destination %s", ip)
	}
	return nil
}

// ssrfBlocked reports whether a destination URL is link-local/cloud-metadata -
// never a real webhook, and the top SSRF target. RFC1918/loopback are NOT
// blocked on purpose: a self-hosted notifier (ntfy/gotify/Home Assistant) on
// the LAN or localhost is normal use, and the threat model already equates
// dashboard access with this capability.
func ssrfBlocked(rawurl string) bool {
	u, err := url.Parse(rawurl)
	if err != nil {
		return false // let the request layer surface the parse error
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLinkLocalUnicast() // 169.254.0.0/16, fe80::/10 (incl. 169.254.169.254)
}

func (n *Notifier) url() string {
	if n.URLFn == nil {
		return ""
	}
	return strings.TrimSpace(n.URLFn())
}

// outageRetries is the bounded backoff for a transition alert. A link_up/
// link_down POST is a one-shot event, so a transient webhook failure (5xx,
// rate-limit, or the brief post-reconnect resolver hiccup when DNS/routing lags
// the link coming back) would otherwise drop the alert for good. We retry a few
// times; the call already runs on its own goroutine, so blocking here can't
// stall the monitor, and ctx cancellation (shutdown) aborts the wait.
var outageRetries = []time.Duration{2 * time.Second, 5 * time.Second}

// errPermanent marks a delivery failure that will recur identically on retry: an
// SSRF-blocked destination (a pure function of the URL, re-read unchanged each
// attempt) or a non-transient 4xx the server won't reconsider. Outage checks for
// it and skips the backoff loop, so one link transition emits a single blocked/
// fail record and never holds outageMu across the full backoff for a decision the
// code already knows is final.
var errPermanent = errors.New("notify: permanent delivery failure")

// permanentStatus reports whether an HTTP status is a settled reject not worth
// retrying: a 4xx other than the transient ones (408 timeout, 425 too-early, 429
// rate-limit). 5xx and 3xx stay retryable.
func permanentStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	return code >= 400 && code < 500
}

// Outage notifies that the link went down or came back up. durationS is the
// length of the outage that just ended (only meaningful when online is true).
// It retries a transient delivery failure so a webhook hiccup at the exact
// moment of the transition doesn't silently lose the alert.
func (n *Notifier) Outage(ctx context.Context, online bool, durationS int) {
	// Each transition is dispatched on its own goroutine, so a down-alert stuck
	// in delivery retries could otherwise be overtaken by the following up-alert.
	// Serialize delivery: transitions are inherently ordered (down before up),
	// so holding the lock across retries preserves that order on the wire.
	n.outageMu.Lock()
	defer n.outageMu.Unlock()
	msg := "🔴 Internet connection went down"
	fields := map[string]any{"event": "link_down"}
	if online {
		msg = fmt.Sprintf("✅ Internet is back (down for %s)", util.HumanDur(durationS))
		fields = map[string]any{"event": "link_up", "downtime_s": durationS}
	}
	// A permanent failure (blocked destination, non-transient 4xx) will fail the
	// same way every attempt, so return on it instead of retrying: retrying would
	// triple-count the fail/blocked counter, re-emit the same WARN into the log
	// ring, and hold outageMu across the full backoff for nothing.
	if err := n.Send(ctx, msg, fields); err == nil || errors.Is(err, errPermanent) {
		return
	}
	for _, d := range outageRetries {
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
		if err := n.Send(ctx, msg, fields); err == nil || errors.Is(err, errPermanent) {
			return
		}
	}
}

// SpeedThreshold notifies that a speedtest failed one or more thresholds.
func (n *Notifier) SpeedThreshold(ctx context.Context, sp store.SpeedSample, failures []string) {
	jit := ""
	if sp.JitterMS != nil {
		jit = fmt.Sprintf(" · %.0f ms jitter", *sp.JitterMS)
	}
	msg := fmt.Sprintf("⚠️ Speedtest below threshold: %s\n↓ %.1f Mbps · ↑ %.1f Mbps · %.0f ms ping%s",
		strings.Join(failures, "; "), sp.DownMbps, sp.UpMbps, sp.PingMS, jit)
	fields := map[string]any{
		"event":     "speedtest_threshold_failed",
		"failures":  failures,
		"down_mbps": sp.DownMbps,
		"up_mbps":   sp.UpMbps,
		"ping_ms":   sp.PingMS,
		"server":    sp.Server,
	}
	if sp.JitterMS != nil {
		fields["jitter_ms"] = *sp.JitterMS
	}
	n.Send(ctx, msg, fields)
}

// Send delivers a message to the configured webhook, shaping the JSON body for
// the destination host and merging extra fields into generic payloads. Returns
// nil when no URL is configured (a deliberate no-op), and a non-nil error on a
// real delivery failure (blocked, build error, transport error, non-2xx) so a
// retrying caller (the digest) can tell delivered from dropped. Outage and
// SpeedThreshold ignore the return.
func (n *Notifier) Send(ctx context.Context, message string, fields map[string]any) error {
	url := n.url()
	if url == "" {
		// Common "my alert never arrived" cause: no webhook set. Visible at debug so
		// it's distinguishable from a configured-but-failing webhook.
		n.log.Debug("notify skipped: no webhook configured", "event", fields["event"])
		return nil // not configured: a deliberate no-op, not a failure
	}
	if ssrfBlocked(url) {
		stats.Inc("notify." + classifyDest(url) + ".blocked")
		n.log.Warn("notify blocked", "dest", classifyDest(url), "reason", "link-local/metadata destination")
		return fmt.Errorf("notify: destination blocked (link-local/metadata): %w", errPermanent)
	}
	body := payloadFor(url, message, fields)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// Scrub: webhook URLs are bearer secrets and *url.Error prints the full URL.
		err = scrubURLErr(err)
		n.log.Error("notify build request", "err", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Per-destination-class delivery accounting (never the URL/host itself).
	class := classifyDest(url)
	start := time.Now()
	resp, err := n.client.Do(req)
	stats.AddF("notify."+class+".lat_ms_sum", util.DurMS(time.Since(start)))
	stats.Inc("notify." + class + ".lat_n")
	if err != nil {
		stats.Inc("notify." + class + ".fail")
		err = scrubURLErr(err)
		n.log.Error("notify send", "dest", class, "err", err)
		return err
	}
	// Drain (bounded) so the connection can be pooled for keep-alive reuse.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		stats.Inc("notify." + class + ".fail")
		n.log.Warn("notify non-2xx", "dest", class, "status", resp.StatusCode)
		if permanentStatus(resp.StatusCode) {
			return fmt.Errorf("notify: %s returned status %d: %w", class, resp.StatusCode, errPermanent)
		}
		return fmt.Errorf("notify: %s returned status %d", class, resp.StatusCode)
	}
	stats.Inc("notify." + class + ".ok")
	n.log.Info("alert sent", "event", fields["event"])
	return nil
}

// scrubURLErr strips the URL from a *url.Error before it hits a log line:
// webhook URLs are bearer secrets (Discord/Slack put the token in the path),
// and url.Error.Error() prints the full URL. The underlying cause is kept.
func scrubURLErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s webhook: %w", ue.Op, ue.Err)
	}
	return err
}

// classifyDest buckets a webhook URL into a fixed destination class for the
// delivery counters. Only these compile-time labels reach a counter name; the
// URL/host must not, since counter names take no user/network input.
func classifyDest(rawurl string) string {
	host := strings.ToLower(hostOf(rawurl))
	switch {
	case strings.Contains(host, "discord"):
		return "discord"
	case strings.Contains(host, "slack"):
		return "slack"
	case strings.Contains(host, "hc-ping"), strings.Contains(host, "healthchecks"):
		return "healthchecks"
	}
	return "generic"
}

// payloadFor builds a JSON body suited to the destination. Discord wants
// {"content"}, Slack wants {"text"}; everything else gets a rich payload.
func payloadFor(url, message string, fields map[string]any) []byte {
	host := hostOf(url)
	var v map[string]any
	switch {
	case strings.Contains(host, "discord.com") || strings.Contains(host, "discordapp.com"):
		v = map[string]any{"content": message}
	case strings.Contains(host, "hooks.slack.com"):
		v = map[string]any{"text": message}
	default:
		title, typ, prio := alertMeta(fields)
		// Carry the alert text under every common key (Slack/generic: text,
		// Discord-style: content, Gotify: message, Apprise: body) plus title/type/
		// priority, so Gotify and Apprise render a headline and severity. Receivers
		// ignore the keys they don't use. Merged fields follow for structured data.
		v = map[string]any{
			"text": message, "content": message, "message": message, "body": message,
			"title": title, "type": typ, "priority": prio, "app": "pingularity",
		}
		for k, val := range fields {
			v[k] = val
		}
	}
	b, _ := json.Marshal(v)
	return b
}

// alertMeta derives a headline, an Apprise notification type (info/success/
// warning/failure), and a 1 (low) - 5 (urgent) priority from the alert event,
// for receivers that render them (Apprise, Gotify, ntfy's JSON publish). An
// unknown/absent event gets a neutral default.
func alertMeta(fields map[string]any) (title, typ string, priority int) {
	switch fields["event"] {
	case "link_down":
		return "Internet down", "failure", 5
	case "link_up":
		return "Internet restored", "success", 3
	case "speedtest_threshold_failed":
		return "Speedtest below threshold", "warning", 4
	case "digest":
		return "Connectivity digest", "info", 2
	}
	return "Pingularity", "info", 3
}

func hostOf(rawurl string) string {
	s := rawurl
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

// Heartbeat pings a dead-man's-switch URL (Healthchecks.io, Uptime Kuma push,
// …) so the external service can alert if Pingularity or the host goes silent.
// Errors are ignored - a missed ping is itself the signal the watchdog wants.
func Heartbeat(ctx context.Context, client *http.Client, url string, log *slog.Logger) {
	url = strings.TrimSpace(url)
	if url == "" {
		return
	}
	if ssrfBlocked(url) {
		stats.Inc("notify.heartbeat.blocked")
		// A misconfiguration: the watchdog will never be pinged. Worth surfacing.
		log.Warn("heartbeat blocked", "reason", "link-local/metadata destination")
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Debug("heartbeat build request", "err", scrubURLErr(err))
		return
	}
	req.Header.Set("User-Agent", "pingularity")
	// Same accounting as Send, under a fixed "heartbeat" class regardless of host.
	start := time.Now()
	resp, err := client.Do(req)
	stats.AddF("notify.heartbeat.lat_ms_sum", util.DurMS(time.Since(start)))
	stats.Inc("notify.heartbeat.lat_n")
	// The alerting backstop is otherwise invisible; log each outcome at debug (a
	// per-minute line would be too noisy at info). A failing ping is expected during
	// a real outage - that's exactly when the watchdog should fire.
	if err != nil {
		stats.Inc("notify.heartbeat.fail")
		log.Debug("heartbeat fail", "err", scrubURLErr(err))
		return
	}
	if resp.StatusCode >= 300 {
		stats.Inc("notify.heartbeat.fail")
		log.Debug("heartbeat fail", "status", resp.StatusCode)
	} else {
		stats.Inc("notify.heartbeat.ok")
		log.Debug("heartbeat ok")
	}
	// Drain (bounded) so the connection can be pooled for keep-alive reuse.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
