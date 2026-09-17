# The dashboard

Each panel and each settings tab in detail, every outside service the daemon talks to and when, running behind a proxy, and notification recipes. The [docs site](https://docs.pingularity.dev) covers the same ground in plainer language.


The dashboard is built into the binary (no extra services, no CDN - the UI, web
font, and favicon are all embedded) and served at the `-listen` address. The top
bar carries the live status bubbles - per-family (IPv4/IPv6) latency, process
runtime, 24h/7d uptime, and cumulative speedtest data used (click it for a
breakdown by window). That figure is the transfer payload each run recorded,
including the runs that failed partway or that you cancelled - they still cost
you the traffic, so on a metered link the number has to include them. What it is
not is a wire total: protocol framing, retransmits, the warm-up seconds an engine
throws away, and the UDP loss probe all move bytes that nothing counts, which
makes this a measured lower bound rather than a bill (the exclusions are listed
in full under [Metrics](metrics.md)). On a default install - the Ookla
engine, with its five-second packet-loss probe - the uncounted part is overhead
plus that one short probe, so the figure runs a few percent light and no more.
It only becomes worth budgeting around on an install deliberately pointed at
iperf3, where every direction discards a warm-up second before the count starts
and the UDP pass moves megabytes of its own - or gigabytes, if you raise that
probe's rate cap by hand (the daemon warns about the uncounted usage only from
1 Gbps up, so a smaller raise is silent). Those
attempts are counted but never shown as measurements: they appear in no chart,
table, average or CSV, because nothing was measured.
The latency and DNS dots are the theme accent at varying
**intensity** - full = healthy, fading as latency or DNS gets worse - so the
bar stays calm at a glance and only the dot that needs attention dims. The
uptime, runtime, and data bubbles use plain icons (a pulse line, a clock, and
up/down arrows); their numbers carry the state.

Below that:

- **Connection** - public IP (v4·v6), ISP + geolocation, the actual upstream
  DNS resolver (provider + location), the **internet exit**: where traffic
  leaves the ISP's network - the exit router and peering handoff found by a
  built-in traceroute walked to the AS boundary (per-hop RTTs + city; on Linux
  this needs root, `CAP_NET_RAW`, or a suitable `ping_group_range` and is
  silently omitted otherwise -
  Windows and macOS need no privileges), plus the Cloudflare PoP serving the
  connection. A refresh button (top-right) re-runs all of it on demand. A
  dual-stack host that loses IPv4 for 15+ minutes while IPv6 still works is
  treated as IPv6-only (identity switches to the IPv6 side) until IPv4 returns.
- **Speed** - Download / Upload / Ping / Jitter / Packet-loss cards plus
  **bufferbloat** (idle vs loaded latency); three stacked history charts (speed
  with plan-threshold lines, a ping/jitter quality band, and bufferbloat) with
  per-chart **Speed / Quality / Bufferbloat** show-hide toggles, over a window of
  1d / 7d / 30d / 1y, a **custom** duration, or a typed **date range** - the custom
  box takes plain language (`jul 1 to jul 8`, `2026-07-01 to 2026-07-08`,
  `since jul 1`, `yesterday`, `2026`, `9am to 5pm`, `3d ago to now`) and echoes
  back the span it read. An end date includes that whole day, and a bare
  four-digit year means that year. A typed range reaches back at most 366 days
  from today: an older start is quietly raised to that floor, so a span lying
  entirely further back comes up empty, and the chart can say only that there is
  nothing in the range, not why. Nothing out of the box gets there - latency
  samples are kept 30 days by default, speed runs and outages a year - so it
  takes raising a retention window past a year (or to `0`, keep forever) and then
  accumulating that much history, or restoring a backup that already holds it.
  The charts fit whatever data the span actually holds, so picking a wide range
  with only a little data in it zooms to the data rather than drawing empty
  margins; a span with no runs in reach says so.
  While a fixed range is pinned the stat cards follow it rather than the newest
  run, and a **Live** button returns to the rolling window, and an expandable **all-runs** table
  (paginated, with **CSV export** and a per-run health badge).
- **Latency** over time - the lowest round-trip across your anchors, plus a
  separate **DNS-resolution** line. Each round resolves a random throwaway name
  through the host's own *system* resolver (the random label dodges caches, so it
  times the real lookup path your apps use; "no such name" is a healthy answer -
  the resolver replied, which is what is being timed). The name is **fully
  qualified**, so it is looked up exactly as written rather than being tried
  against your search domains first. That matters if you are comparing against
  readings from **0.61 or earlier**, which looked it up unqualified: on a host
  with a search domain those readings timed an extra doomed lookup and read
  high, while on one whose search domain answers wildcards they timed a fast
  local hit for a different name and read low. Either way the two are not
  comparable, and the change can move the number in either direction - re-baseline
  any DNS alert thresholds rather than assuming which way it went. Readings from
  **0.70.1 and earlier** also sit a few ms above later versions and spike
  harder: every version through 0.70.1 asked an IPv4/IPv6 question *pair*
  and timed the slower answer, and on the Linux binaries - which use Go's
  built-in resolver - one lost reply pinned a "healthy" reading at the full 3s
  budget (the macOS and Windows binaries resolve through the system resolver,
  which already reported that case as a failure; on 0.61 and earlier the pair
  stacks on top of the search-domain effect above). Later versions ask a
  **single IPv4 question**, and a lookup that eats its whole budget now always
  counts as a failure - after upgrading expect the DNS line slightly lower and
  calmer, and re-baseline DNS thresholds one more time.
  A round skips its lookup while the previous one is still in
  flight, so a hung resolver cannot pile lookups up behind it; a lookup gives up
  after 3s, so in practice that needs a probe interval shorter than that. The DNS
  line **gaps wherever a bucket held no successful lookup** (timeout / SERVFAIL / no
  resolver), so a DNS gap with the latency line intact means "online, but DNS was
  struggling." On the wider windows one plotted point averages many lookups, and
  that average counts the successful ones only - a failure neither plots nor
  shifts the value, so a bucket gaps only when none of its lookups succeeded.
  Narrow the window to see them one by one. Selectable window (5m / 1h / 6h / 1d
  / 7d, a **custom** duration, or a typed **date range** exactly like the Speed
  panel; rolling windows are capped at the relevant retention), with red bands
  marking **rounds that failed their checks**. Those come from the latency samples
  themselves, not from the debounced outage log below - so a blip too short to
  become an outage event still shows a band, and deleting an outage does not
  erase the bands underneath it. Hover either chart to read the exact point.
- **Downtime heatmap** - a GitHub-style year of daily outages. A cell's shade is
  **how many outages that day**, not how long they lasted: one 23-hour outage and
  one 1-second blip are both a single event and shade identically, while three
  blips shade darker than either. Hover a cell for the figure that answers "how
  bad was it" - the actual downtime, and how much of the day was observed. A day
  that was watched end to end with nothing to report leaves no record of its own
  behind, so its cell carries no figures and says "no outages recorded" instead -
  a claim about what is on file rather than about the day, because a day whose
  outage history you deleted looks exactly the same from here.
- **Recent outages** - the debounced up/down event log. Each resolved outage has a
  trash button to delete it (removes it from the log, heatmap, and uptime stats -
  handy after planned maintenance you don't want counted). The durations here are
  **observed** time, the same rule uptime and the heatmap use: any stretch of an
  outage that monitoring didn't watch - paused with the power button, outside a
  latency schedule window, or with the host asleep - is subtracted, so a row can be
  much shorter than the wall time it spans. A restart mid-outage is different: it
  splits the log into two rows, the first with no duration at all.

![The Downtime panel: a GitHub-style calendar heatmap of the past year, each cell a day shaded by how many outages it saw, above a Show recent outages button](https://raw.githubusercontent.com/pingular/pingularity/main/docs/downtime-heatmap.png)

> **Who does Pingularity talk to?** Every service it picks for you is keyless
> public infrastructure - no API token, no signup - and nothing is ever *pushed*
> anywhere you did not configure yourself. The complete list of outbound calls,
> so you can audit or firewall them:
>
> | Service | What it receives | When |
> |---|---|---|
> | anchors (`1.1.1.1`, `8.8.8.8`, `9.9.9.9` + v6) | a TCP handshake, no payload | every probe round |
> | your own DNS resolver | one random throwaway lookup; and, for a LAN resolver only, one CHAOS `version.bind` query to name its software | every probe round (DNS line); the `version.bind` query on the refresh that first labels the resolver set, again if that set changes, and again on any refresh while a resolver in the set is still labelled by a bare address that is not private, link-local or loopback (its naming lookup came back empty), because that retry relabels the whole set |
> | **ipify** | a "what's my IP" request | connection refresh |
> | **whoami.akamai.net** (DNS) | a fixed lookup whose answer is your *resolver's* egress address, not yours | connection refresh |
> | every router on the way to the **exit target** (`1.1.1.1` unless you change it) | one ICMP echo per hop, carrying nothing about you | exit discovery |
> | **Team Cymru** (DNS) | your public IPv4/IPv6, your resolver's egress address, the resolver addresses your host is configured with, and every traceroute hop in public address space - to name the network each one belongs to; hops that are private, carrier-NAT (`100.64/10`), link-local or loopback are skipped | connection refresh + exit discovery |
> | **`1.1.1.1:53`, then `9.9.9.9:53`** (direct DNS, the fallback for the row above) | the same Team Cymru query names, sent to that resolver directly rather than through yours | only after your own resolver fails one of those lookups (any error - a timeout, SERVFAIL, refused, unreachable - but not a "no such name" answer, which counts as answered) - then first for the following minute, your resolver last, until it answers again. The `netinfo.cymru_fallback` counter climbs once per lookup a public resolver answers, including every lookup during that minute |
> | **RIPE IPmap** | the two boundary router IPs the traceroute settles on, and your resolver's egress address, for geolocation | connection refresh + exit discovery |
> | **ipwho.is**, then **geojs.io** | your public IP, for the ISP/geo line | connection refresh |
> | **Cloudflare** (`/cdn-cgi/trace`) | a plain fetch, to learn the serving PoP | connection refresh |
> | **one.one.one.one:443** (Cloudflare) | bare TCP handshakes, no payload - the fixed target the bufferbloat idle and loaded samples are measured against. Resolved through your own resolver, so it reaches whichever of `1.1.1.1`/`1.0.0.1` (or their v6 pair) that answer names | every speedtest that samples bufferbloat |
> | reverse DNS | router/host IPs, for names | connection refresh |
> | **Ookla** servers | the speedtest traffic itself, plus a server-list lookup and a small probe of each listed server's upload endpoint (remembered, so a repeat does not send it again); opening the Ookla tab can also cost one by-ID lookup and one name search first, to centre the list on the server your last automatic run used; for the picker's Auto button, the same selection a run performs - one list fetch per candidate city, a round of pings at every racer, then a round at the rest of the winning city's field (up to twelve), no transfer; for a server ID typed in Find, and for a saved pin the drawer has not yet checked this page load (at most twice per server), one by-ID lookup plus one small POST at that server's upload endpoint to learn whether it can still run a test, no transfer; and for the saved list's refresh button, one by-ID lookup, one endpoint probe and a round of pings at each kept server (up to twelve), no transfer | when a speedtest runs, when the Ookla settings tab is opened or a city is searched, when a server ID is typed in Find or a saved pin is first shown, on every Auto click, and on every refresh click in the saved list |
> | your own **iperf3 server** (opt-in) | the test traffic itself - the TCP transfers, plus a short UDP pass for loss and jitter; or, for the status light in the settings drawer, one bare TCP handshake and nothing else | when an iperf3 speedtest runs, and when the drawer checks a saved server's status light - once per address while the drawer is open with iperf3 selected, plus whenever you click a server's light or change its address |
> | **nominatim.openstreetmap.org** | the city text you type | only when you search a city for a server |
> | **update.pingularity.dev** | a version-check fetch (no identifiers) | daily, if the update check is on (until the first check succeeds: retried at 1m/5m/15m, then hourly) |
> | your **alert webhook** (opt-in) | the alert text and its fields, to the URL you set | on an outage, a speed-threshold breach, a digest, or the Send test button |
> | your **heartbeat URL** (opt-in) | a bare `GET`, no body | every minute while monitoring is live |
>
> The **connection refresh** and **exit discovery** rows are the ones that carry
> your public IP, the `whoami.akamai.net` line excepted - that lookup's whole
> point is that it carries nothing of yours, since your own resolver asks it for
> you and the answer describes the resolver. Rows marked (DNS) are questions
> handed to that resolver rather than connections the daemon makes itself, so
> what the service at the far end sees is your resolver arriving with an address
> in the query - except the direct fallback row, where the daemon asks the named
> public resolver itself and that resolver sees your address asking. "Connection refresh" means: once an hour on its own (every 5
> minutes while a lookup is failing, or while exit discovery has yet to
> succeed), once after a reconnect at most every 5 minutes, and once after
> every speedtest - so turning speedtests on multiplies these too. Exit
> discovery rides those refreshes and re-traces at most every 10 minutes once
> an exit is known; until one is, a failed trace retries after a minute, and
> three straight failures stand it down to the slow cadence. They stop when
> monitoring is paused, and the
> **Connection info** toggle (Latency tab) stops them for good. Both cover the
> automatic lookups only - the Connection panel's refresh button still fetches
> on demand, and the panel says when it is no longer refreshing itself.
>
> Everything else - dashboard, charts, history, alerts evaluation - is fully
> local. Turn speedtests, the update check, or the DNS probe off and those rows
> stop firing on their own, bar two halves that answer to a different switch.
> The Ookla server list and the iperf3 status light are the settings drawer
> reaching out, so they follow the drawer rather than the speedtest toggle; the
> `version.bind` query rides the connection refresh, so it follows **Connection
> info** rather than the DNS probe. (Alert webhooks and the heartbeat post only
> to URLs you configure yourself.)
>
> **Behind a proxy?** Ookla speedtests use `HTTP_PROXY` / `HTTPS_PROXY` /
> `NO_PROXY` from the daemon's environment (lower-case spellings too), written as
> `http://`, `https://`, `socks5://`, `socks5h://`, or a bare `host:port`;
> `ALL_PROXY` is not read, because Go's HTTP client never routes a request through
> it. A value the daemon cannot use - an unsupported scheme, or one that names no
> host - **fails** the requests that would have ridden it, quoting the value,
> rather than quietly connecting direct: traffic leaving by a route you did not
> choose is the outcome worth refusing. And note a proxied run measures the path
> through the proxy, not your direct link. Alert webhooks, the heartbeat, and the
> update check deliberately ignore these variables and always dial direct - the two
> that dial a URL *you* configure are vetted by the IP they actually resolve to,
> which a proxy hop would hide, and the update check goes to one fixed HTTPS
> address that must not be silently intercepted - so on a
> network with no direct egress those three won't get out. iperf3 speaks its own
> TCP protocol and is never proxied. One caveat on a proxy-only network: before
> letting a proxied request name a speedtest server, the daemon resolves that name
> locally to check the proxy isn't being pointed at something internal, so with no
> local resolver every server is refused - and each refusal is logged only at debug
> level, so the reason is invisible until you raise it. The fix is to give the
> daemon a working local resolver. `NO_PROXY` is not one: the name is resolved
> before the routing decision is made, so listing a destination there does not
> skip the check that just failed - and a direct connection would need that same
> lookup anyway. Clearing `HTTP_PROXY`/`HTTPS_PROXY` altogether does stand the
> check down, since it is inert when no proxy is configured, but that only helps
> if the daemon has direct egress. Where DNS genuinely lives only at the proxy (a
> `socks5h` setup), there is no way round it and Ookla speedtests stop rather than
> run unvetted. Full reasoning in
> [docs/security-model.md](https://github.com/pingular/pingularity/blob/main/docs/security-model.md).

The **logo** (top-right) opens a tabbed settings drawer; the **power** toggle
beside it, in the top bar, starts/stops all monitoring. Changes apply **live** (no restart)
and persist across restarts.

![The settings drawer, open on its Ookla tab: a row of tabs (Speedtest, Ookla, iperf3, Latency, Schedule, Data, Alerts, Access, Appearance, About) above the per-test knobs - Best of, Discard losers, Retries, Packet-loss probe, Direction and Parallel connections, each with a hover-help dot; below them the Saved pane with Auto selected, a Find box that takes a place or an Ookla server ID, and the server list with ID, ping and distance columns and a star on each row; Save and Discard sit at the bottom left, Reset to defaults and Reset tiles at the bottom right](https://raw.githubusercontent.com/pingular/pingularity/main/docs/settings-ookla.png)

- **Latency** → latency probing on/off, latency interval, probe timeout, and
  sensitivity (failures→down / successes→up, IPv6 mode auto/on/off), plus the
  **DNS resolution** probe (on by default), the **Connection info** lookups, and
  the **Exit-path target** - the host or IP the exit traceroute heads toward
  (blank = `1.1.1.1`; it must resolve to IPv4, see [How it works](architecture.md#how-it-works)).
- **Speedtest** → automatic runs on an interval, plus on-reconnect and
  when-degraded triggers, a skip-when-busy option, and **Test more often while
  failing** (off by default): while the last run is still breaching an Alerts
  threshold, the interval drops to a quarter of what you set - never longer than 5
  minutes, never shorter than 1 - so an hourly schedule tests every 5 minutes until
  a run passes. It needs a threshold set in Alerts, and it costs far more data than
  the cadence you configured - 12x the runs on that hourly default, for as long as
  the breach lasts. Changing the interval shows a live estimate (Ookla only, and
  only while automatic runs are on) of the daily/monthly data the *scheduled* tests
  will use, based on what your recent runs recorded - so it is a measured lower
  bound like the data-used figure itself - and it counts neither the extra
  triggers nor this faster cadence. It does follow a speedtest **schedule**:
  with one set, only the runs its windows leave room for are counted (plus the
  one each window opening catches up), so confining an hourly test to office
  hours shows the handful of tests you will actually get rather than all 24.
- **Ookla** → the Ookla server picker (kept servers, Find by place or ID, Auto
  to preview what a run would race - the list you were looking at comes back
  when you reopen the drawer or reload the page: a searched place fetched
  fresh, the Auto candidates as last raced while that is under ten minutes old
  and no speedtest has run since, raced again otherwise - and your kept
  servers' pings are measured again on the same ten-minute rule; Save leaves
  it all alone and Reset to defaults starts over), test direction, retries, parallel
  connections, the packet-loss probe, and **Discard losers** (on by default):
  what a Best-of round keeps. On, only the best result is recorded - one row
  per test, as always. Off, every server the round measured gets its own row
  in the runs table, the chart and the exports, on the second it finished,
  tagged *round* and pointing at the winner (`round_ts`); the winner alone
  stays the test's result - thresholds, alerts and server selection look
  only at it - and deleting the winner deletes its round with it. Each row
  then carries its own data volume (the winner's adds the round's overhead),
  so the totals are unchanged and the data estimate still counts the whole
  round. (The automatic challenger that lets a
  rival server take the seat now and then has no knob here - see *Choosing an
  Ookla server*.) The engine itself (**Ookla** or
  **iperf3**) is chosen on the Speedtest tab; iperf3's servers and per-test
  options live on their own **iperf3** tab, laid out the same way: the test
  knobs on top, the saved servers below in the same kind of list (each row
  shows the server, its IP version, whether it authenticates, and a status
  light that re-checks it when clicked). The list's last row adds one: it puts
  an empty server at the end and opens its details, where you type the address
  like every other field. Adding, editing and removing all take effect when you
  press Save, like every other setting in the drawer. **Best of** (Ookla only, default 1 =
  a single server, up to 16) is how many servers each scheduled or manual test
  measures, keeping only the best result (or every result, with **Discard
  losers** off). The round is your pinned server if
  you have one, then your starred servers fastest ping first, then the fastest
  of the rest, N in all: under a pin the rest come from *around the pin*; on
  Auto they come from the whole city race - every candidate city's pool,
  widened to N, ranked together by ping - so a Toronto server that pings well
  sits in the same round as Montréal's. A starred server the race did not
  reach is looked up and pinged for the round. It costs N times the data and
  up to N times the time of a single test (each server's turn is bounded, and
  the run's budget grows with N), so the estimate on the Speedtest tab shows
  a warning sign above 4; above 1 the automatic challenger stands down. (Upgrading from
  a version with the old on/off: on becomes 3, off becomes 1; the old setting
  is left as it was, so a downgrade reads it as before the upgrade.) It keeps
  only the best result as the test's result - handy when one server has a bad
  day and you'd rather it didn't define your history. The best result is the highest *score*: a
  capacity figure weighting download 70% and upload 30% **relative to each
  other** (not as raw Mbps, so it means the same on a symmetric line and a 20:1
  asymmetric one), discounted by ping (roughly 1% per millisecond, topping out
  at 20ms). So a server that measured a fifth of your real upload can't hide
  behind a big download number, a near-tie on speed goes to the lower-ping
  server, and a clearly faster one still wins. Ties break on ping, then jitter, then
  bufferbloat; the other runs are discarded (their
  data volume is still counted, since it was really spent) unless **Discard
  losers** is off, which records them as rows of their own. The run that is kept
  is one real test, so its **ping, jitter and bufferbloat are the winner's too**:
  when a round is decided on throughput, those columns can jump because the round
  changed hands, not because your connection did. It is not averaged across
  servers - that would describe a test that never happened - so read latency and
  jitter from the charts, which sample continuously and do not depend on who won.
  One guard runs before the comparison: when one server reports a direction far
  beyond what the rest of the round agrees on (buffer absorption at the server,
  not your line), that reading is **held to what the round agrees on** - for the
  decision and for what lands in history - so a speed you never had can't set a
  record or pass a threshold. And every round keeps its receipts: which servers
  were ranked, raced, measured, or failed, each one's numbers, and why the
  winner won - stored next to the run (`GET /api/speed/runs/servers?ts=`) and
  summarised in the logs. Each server gets 90
  seconds before it is dropped and the next is tried, so a stalled server can't
  hold up the round. Between one server and the next it pauses two seconds, so
  each turn starts on a settled link rather than into what the last transfer
  left draining. A whole round budgets 90 seconds per server, 90 seconds to
  pick them, and those pauses (a Best of 3 is about six minutes of work; the
  largest round, 16, about 26). It costs roughly **N times
  the time and data** of a normal test, so only the runs worth being thorough
  about use it: the ones on your chosen interval, and the **RUN** button. The
  quick automatic tests - at startup, after a reconnect, and the while-degraded
  one - always measure a single server. The data estimate on the Speedtest tab
  accounts for it.
- **Schedule** → optionally restrict *when* monitoring runs. **Latency** probing and
  **speedtests** are scheduled independently (each off by default = run 24/7); when on,
  each gets a list of windows, and a window is a weekday selection + a time-of-day range
  (windows may wrap past midnight). Add multiple windows for split or per-day schedules,
  with Weekdays / Weekends / Every day / 24/7 / Clear presets, and a "week at a glance"
  strip under each list shows the merged coverage. Manual "Run now" always works.
- **Data** → retention: three independent windows - **latency** samples (default
  **30** days), **speed** history (default **365** days), and **downtime**/outage
  history (the heatmap, default **365** days); `0` = keep forever - plus
  per-kind "delete data" buttons, each clearing everything its category exports:
  **latency** takes the DNS-resolution series with the ping samples, **speed**
  takes the server-selection reports with the runs, and **downtime** takes the
  pause/unobserved spans with the outage events, so clearing downtime also resets
  observation coverage. And **Export** / **Import** on the same tab: pick any of
  config / latency / speed / downtime, export them to a JSON file, and import one
  back - time-series data is **merged** (existing/newer local rows are kept, only
  missing rows are added) while **config is overwritten** and reloaded live.
  Both ends stream on the wire, but the *browser* download assembles the whole
  file before it saves, with no progress shown while it does and no size warning
  first - the export is sent as a stream, so its size is not known in advance to
  warn about. A very large backup (years of dense history) can therefore sit
  silently for a long time, and on a big enough one the tab can give up. For one
  that big, stop the service and copy the SQLite database file at the `-db` path
  together with `pingularity.key` beside it (a copy taken while it runs misses
  the `pingularity.db-wal` sidecar, which on a young install is *everything*,
  and without the key the saved iperf3 passwords and signed-in sessions do not
  survive the restore), or stream `/api/export` straight to disk with
  `curl -OJ 'http://127.0.0.1:9000/api/export?config=1&latency=1&speed=1&downtime=1'`
  (name at least one category or it is a `400`; add `-u user:pass` when a login is
  set). The import warns you when it matters: restored rows older than
  your current retention windows will be pruned within the hour (raise
  retention first to keep them), and a config restore that carried "login on"
  without a password leaves login off until you set one.
- **Alerts** → *Thresholds* (min download/upload, max ping/jitter/packet-loss, and
  max bufferbloat per direction; each run is marked healthy/unhealthy against the
  values in effect when it ran) with a **Breaches in a row** count (1-10) that
  debounces alerting - it defaults to **1**, which pages on every breaching run,
  so raise it if one blip shouldn't - and
  *Notifications* (alert-on-outage, a generic **webhook** with a Test button, an
  optional **periodic summary** posted to the webhook - off / daily / weekly, a
  "how it went" report of uptime, median speeds, and outage count/downtime; it
  always goes out on its cadence and states the span it actually observed, so a
  period spent scheduled-off or paused is reported as such rather than as a
  confident 100%. The first one lands a full period *after* you switch it on -
  enabling "daily" arms the clock rather than sending immediately - and with no
  webhook URL set nothing is sent and no period is consumed, so a webhook you
  remove and put back still gets the window that was waiting for it. An install
  that has never had a webhook has no such window to hand over: adding the URL
  arms the clock exactly like switching the summary on does, and the first report
  lands a full day or week after that. And a dead-man's-switch **heartbeat**
  URL).
- **Access** → access controls (changes here apply on **Save**).
  **Network access** decides whether other devices can reach the dashboard /
  API / `/metrics`, or only this machine - a live loopback filter, so remote
  clients get 403. It starts **off everywhere** (localhost-only until you flip
  it), containers included: the loopback filter is enforced the same way in every
  environment, and a container that must be reachable opts in explicitly with
  `-access network` (or `-e PINGULARITY_ACCESS=network`) rather than being guessed
  open. The tab shows the **reachable address(es)** with port plus a static-IP
  hint. **Require login** (off by default) gates
  everything behind a password: browsers get a login form + session cookie,
  while API clients and Prometheus use HTTP Basic with the same credentials
  (passwords are capped at 72 bytes, the bcrypt limit). Failed logins are
  recorded (with source IP) in the log and rate-limited per client. Once a
  login is active, changing **any** Access setting - password, username, the
  login toggle, or Network access - requires re-entering the **current
  password** (API callers send `current_password`), so a stolen or walked-up
  browser session cannot quietly take over the account. A login lasts **30 days**,
  and signing out revokes **every** signed-in browser rather than just the one that
  asked - which is also how you evict a lost laptop. Forgot the
  password? Run `pingularity reset-auth` on the host to clear it and disable
  auth, then `systemctl reload pingularity` (or restart the service) - a running
  daemon caches settings in memory and would keep enforcing the old password; in a
  container, run it from a one-off container sharing the volume,
  then restart the container (exact command under
  [Docker](install.md#docker)). **Local-only
  cannot block a same-host reverse proxy** (cloudflared, nginx): it delivers
  internet visitors as loopback connections, so pair any proxy with login.
- **Appearance** → nine themes - Retro (the default), Light, Dark, Amoled,
  Cyber, Slate (flat greyscale), Solarized, Parchment, and Ember - plus a
  Full-width layout toggle and a Corners toggle (Flat squares the panels and
  controls, Round keeps them rounded), content brightness and
  fade sliders, **UI colours** (recolour any of the theme's building blocks:
  backgrounds, panels, borders, text, status colours, accents - every pixel
  derives from them), **top bar** (a colour for the latency and DNS dots - each
  keeping its health shading - the power button in each state, and the two
  halves of the wordmark), and
  **chart customization** - per-series colours, line thickness and area fill, plus
  two rows of switches under **Thresholds and labels**: one to show or hide each
  threshold line, and one for the chart furniture (**Grid lines**, **Y labels**
  for the numbers up the left, **X labels** for the times along the bottom, and
  **Y title**, which prints what the axis measures sideways down the right edge -
  LATENCY (ms), SPEED (Mbps), PING (ms), BLOAT (ms)). Turning the Y labels off
  hands their space back to the chart. All preview live and
  apply on Save; each picker resets to the theme.
- **About** → version, the **daily update-check** toggle, and the **log viewer**
  (logging on/off, PII redaction, and copy/download/clear). PII redaction is **on**
  by default and is a display choice only: every line is kept in both forms, and
  stdout (`journalctl`, `docker logs`) always carries the unmasked one - so use the
  viewer's own download for anything you intend to share.

**Nine built-in themes**, every one fully recolourable (backgrounds, panels,
status colours, chart series - each picker previews live and resets to the theme):

![All nine of Pingularity's built-in themes in a three-by-three grid: Retro, Dark, Amoled across the top; Cyber, Slate, Light in the middle; Parchment, Solarized, Ember along the bottom](https://raw.githubusercontent.com/pingular/pingularity/main/docs/themes.png)

> **Notifications** post to one webhook URL, shaped per host so the common
> targets just work - JSON everywhere except ntfy. Discord → `{content}`,
> Slack → `{text}`, ntfy → the alert text as a plain-text body with
> `X-Title` / `X-Priority` / `X-Tags` headers (see the recipes below); every other
> receiver gets a rich body carrying the alert text under `text`/`content`/
> `message`/`body` plus a `title`, a `type` (`info`/`success`/`warning`/
> `failure`), and a numeric `priority` (1 low - 5 urgent). The heartbeat pings an
> external watchdog (Healthchecks.io, Uptime Kuma push, …) every minute *while
> monitoring is on*, so the watchdog can alert you if Pingularity or the whole
> host goes silent - the one failure the in-band outage alert can't deliver. It
> follows the **power button** only, not whether probing is actually running: it
> keeps pinging through a closed schedule window, with `-latency=false`, and
> through a fresh install's first-run hold. A green watchdog therefore means "the
> process is alive", not "the link is being measured" - pair it with
> `pingularity_probing_active` if you need the latter.

Notification recipes (set the **webhook URL** to):

| Target | URL | Notes |
|---|---|---|
| **Discord / Slack** | the channel's incoming-webhook URL | shaped automatically |
| **Gotify** | `https://gotify.example/message?token=APP_TOKEN` | uses `title` / `message` / `priority` |
| **ntfy** | `https://ntfy.sh/your-topic` (or self-hosted) | spoken natively: the alert text arrives as the notification body with title, priority (1-5, mapped from severity), and an emoji tag. ntfy.sh is auto-detected; for ntfy on your own domain set **Webhook format: ntfy** in the Alerts tab |
| **Apprise** → email, Telegram, Pushover, Gotify, ntfy, … | run the Apprise API server, point at `http://apprise:8000/notify/your-key` | one gateway to 100+ services; uses `title` / `body` / `type` |

For **email, Telegram, or Pushover**, the simple path is Apprise: run the Apprise
API server, add those services to an Apprise config key, and point Pingularity's
webhook at that key. Self-hosted receivers on your LAN/localhost are allowed (only
link-local/cloud-metadata addresses are blocked). For "is Pingularity even alive?"
use the separate **Heartbeat URL**, not the webhook.

Initial values can also be set via flags (`-interval`, `-timeout`, `-latency`,
`-down-after`, `-up-after`, `-speedtest`, `-speedtest-interval`,
`-speedtest-on-reconnect`, `-ipv6`, `-retain`, `-retain-speed`,
`-retain-downtime`); the UI overrides them once changed. `-ipv4` is flag-only -
it has no UI setting.
