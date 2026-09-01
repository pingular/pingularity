# Metrics

The optional Prometheus endpoint: every metric Pingularity exports, the
health endpoints beside it, and how to scrape and alert on them. Summarised
in the [README](../README.md#metrics).

> **Grafana users:** there is an official importable dashboard (latency
> heatmap, speed/bufferbloat history, outage annotations, a multi-instance
> fleet view) and a ready-made alert-rules file - see
> [docs.pingularity.dev/grafana](https://docs.pingularity.dev/grafana/).

A Prometheus endpoint is exposed at `GET /metrics` if you already run a
Prometheus/Grafana stack and want to scrape Pingularity - but nothing external is
required; the built-in dashboard is fully standalone. `/metrics` is a passive
**pull** endpoint - nothing scrapes it for you, and it hands data out only in
answer to a scrape. It is not the whole story of what leaves the box, though:
measurements *are* pushed on the paths you configure yourself - alerts and the
periodic digest carry figures to your alert webhook, and the heartbeat pings its
URL (see the outbound-calls table in the [README](../README.md#dashboard)).

`GET /metrics` exposes (every gauge has a `# HELP` line in the output, so it's
self-describing):

- `pingularity_build_info{version,goversion}` - constant 1, build version and Go
  toolchain in the labels
- `pingularity_runtime_seconds` - process uptime
- `pingularity_up` - overall connectivity (1/0)
- `pingularity_latency_seconds` - headline latency: lowest across the anchors that
  answered (your base internet latency); absent when nothing answered
- `pingularity_monitoring_paused` - 1 while monitoring is not running: stopped via
  the power button, or a fresh install still holding for its first-run Quick Setup
  answer (`quick_setup_pending` in `/api/status` tells those two apart). Stored
  gauges freeze and the live per-family/DNS series go absent while paused
- `pingularity_probing_active` - 1 while probe rounds are actually running:
  the "can I trust the data" signal. It goes 0 for every way rounds can stop -
  the power button, the latency toggle, a closed schedule window, all
  address families switched off, or that same first-run hold - while
  `pingularity_up` and
  `_state_since_timestamp_seconds` hold their last values
- `pingularity_state_since_timestamp_seconds` - when the current up/down state began
- `pingularity_current_outage_seconds` - length of the outage in progress; absent
  while online, and absent while probing is paused (paused time is excluded from the
  outage the monitor finally records, so the live value would otherwise run ahead of
  history). Use `_state_since_timestamp_seconds` to see when the outage began
- `pingularity_family_up{family}` / `pingularity_family_latency_seconds{family}` /
  `pingularity_family_state_since_timestamp_seconds{family}` - per-address-family
  connectivity, latency (only while that family is up), and when that family's
  current state began - so "how long has IPv6 alone been down" is answerable.
  All three go absent (not frozen) while probing isn't running
- `pingularity_target_latency_seconds{target}` / `pingularity_target_up{target}` -
  per anchor; the latency line appears only for a successful probe (a down target
  has no reading, not a misleading 0)
- `pingularity_target_last_probe_timestamp_seconds{target}` /
  `pingularity_probe_last_round_timestamp_seconds` - per-target and overall probe
  freshness. `target_up` deliberately holds its last value while paused, so a
  timestamp that stops advancing is how you tell a frozen reading from a live one
- `pingularity_probe_latency_seconds` / `pingularity_dns_latency_seconds` /
  `pingularity_series_query_seconds` - **histograms** (`_bucket{le}` + `_sum` +
  `_count`) of anchor RTT, DNS resolve-time, and how long a chart aggregate took,
  so `histogram_quantile()` gives real p95/p99 and catches spikes that fall
  between scrapes - which the last-value latency gauges lose. Each appears
  once it has recorded something, so the chart-query one is absent until a
  dashboard or an `/api/series` call has run a query. Its buckets are deliberately
  wider than the two latency ones - out to a minute rather than five seconds -
  because a re-scan of the samples table on slow hardware runs well past where
  latency stops being interesting
- `pingularity_dns_up` / `pingularity_dns_resolve_seconds` - the DNS-resolution
  probe (the chart's second line): whether a cache-busted lookup succeeded and how
  long it took, via the host's own resolver. Present only while the probe is
  actually running and has produced a result (in short: while
  `pingularity_probing_active` is 1, the DNS toggle is on, and the first lookup
  has answered) - a probe that isn't running, or hasn't resolved yet, reads as
  absent, not a fake 0
- `pingularity_uptime_ratio{window}` - up-fraction over `6h`, `24h`, `7d`, `30d`,
  `1y`, and `all`. This is **observed** downtime / **observed** time: paused,
  scheduled-off, families-off, and process-down wall time is excluded from the
  denominator (it's neither up nor down), so it can't inflate uptime. Two small
  gaps are deliberately *not* booked, and so stay in the denominator (and are
  normally credited as up): a restart that took **2 minutes or less**, and a
  suspend/freeze shorter than **one probe interval plus 10 minutes** - below
  that, a gap can't be told apart from ordinary scheduler overshoot, and the
  error is bounded and self-limiting where a spurious unobserved row would not
  be. A window that
  observed **nothing** is omitted entirely rather than published as a misleading
  100%. Each window is also clamped to the outage-retention horizon, so it can't
  reach past where the downtime events behind it were pruned.
- `pingularity_uptime_coverage_ratio{window}` - the fraction of each window that was
  actually observed (0..1). A low value means the window was mostly paused/unobserved
  and its `uptime_ratio` is thin evidence; `0` means the ratio is absent.
- `pingularity_uptime_since_timestamp_seconds` - the earliest time the uptime figures
  can vouch for (later of first observation and the retention horizon); the `all`
  window reaches back only to here
- `pingularity_speed_last_run_timestamp_seconds` - freshness anchor for the speed
  gauges; `pingularity_speed_info{engine}` names the backend (ookla / iperf3)
- `pingularity_speed_next_run_timestamp_seconds` - when the next scheduled
  speedtest is due (absent when scheduled tests are off); pairs with the last-run
  timestamp to catch a wedged scheduler before the next run would even land
- `pingularity_speed_download_mbps` / `_upload_mbps` / `_ping_ms` / `_jitter_ms` /
  `_packet_loss_percent` - the last run (loss only when measured). These use the
  dashboard's human units (ms, Mbit/s, %) so the numbers match the UI and speedtest
  sites. For Prometheus base-unit conventions, the same values are also emitted as
  `pingularity_speed_download_bytes_per_second` / `_upload_bytes_per_second` /
  `pingularity_speed_ping_seconds` / `pingularity_speed_packet_loss_ratio` (0..1) -
  use whichever your dashboards expect, but don't mix the two unit systems in one
  expression
- `pingularity_speed_ping_best_ms` - on **Ookla** runs, the **fastest** of the
  ping samples `_ping_ms` averages. There the engine reports a mean over ten
  samples, so one stalled handshake moves it several-fold; this is the floor
  beneath it. Alert on this one to mean "the link really is far", and watch the
  **gap** between the two to spot a lossy path. Absent on iperf3 runs, which do
  sample the server themselves - up to five bare TCP handshakes - but report the
  **median** of those as `_ping_ms` and record no floor beside it
- `pingularity_speed_healthy` - 1/0, did the last run pass your configured
  thresholds (lets alerting reuse the in-app verdict instead of re-encoding it);
  **absent** when no thresholds are configured *or* when the run couldn't measure
  something a threshold covers. A check that never ran is not a check that
  passed, so those runs get no verdict rather than a green one - alert on
  `absent()` if a silently unjudged run matters to you
- `pingularity_speed_idle_latency_ms` / `pingularity_speed_loaded_latency_ms{direction}`
  (+ `_p95_ms`) - latency idle vs under load; **loaded minus idle is bufferbloat**.
  Present only when the engine measured them
- `pingularity_speed_data_used_bytes` (total within retention),
  `pingularity_speed_data_used_window_bytes{window}` (per `6h`/`24h`/`7d`/`30d`/`1y`
  window - the total is non-monotonic under pruning, so metered-link budgets
  should use these), `pingularity_speed_last_run_bytes{direction}` (what the
  last run itself consumed), and `pingularity_speed_avg_run_bytes{direction}`.
  Treat all of these as a **measured lower bound on wire usage, not a bill**:
  they count the payload the engine reports moving, so they exclude warm-up
  traffic, the UDP loss/jitter probe, TCP/TLS/IP overhead, and retransmits. A
  run that failed or was aborted partway still contributes the bytes its
  engine had counted by then, but bytes an engine never got to count - and a
  run cut short by daemon shutdown - are lost. On a metered link, budget with
  headroom above these numbers rather than against them. That failed run is
  kept as an accounting row, **flagged** as one: the totals and windows above
  count its bytes, while every view that means "a measurement" filters it out -
  it is not in the runs table, the charts, or `latest`, so it can't become the
  last run, and it gets no healthy/unhealthy verdict. `avg_run_bytes` skips it
  too, on purpose: that average projects what the *next* run will cost, and a
  run that died partway spent a fraction of a full one, so counting it would
  predict a bill no schedule produces
- `pingularity_process_start_time_seconds` - process start (the Prometheus-conventional
  form; `pingularity_runtime_seconds` kept for compatibility)
- `pingularity_goroutines` / `pingularity_memory_heap_bytes` / `_memory_sys_bytes` /
  `pingularity_gc_cycles_total` / `pingularity_gomaxprocs` / `pingularity_open_fds`
  (Unix) - process self-health: leak and GC trends, and an FD-leak early warning
- `pingularity_db_bytes` - on-disk database size incl. WAL/SHM (watch your retention)
- `pingularity_disk_free_bytes` - free space on the filesystem holding the
  database, where the platform supports it - an early disk-full warning long
  before writes start failing
- `pingularity_update_available` - 1 when a newer release has been seen, 0 when
  not; `pingularity_update_check_timestamp_seconds` - when the release feed was
  last polled **successfully**. Both are absent unless the daily update check is
  on, and the timestamp stays absent until the first poll succeeds (a `0` would
  read as 1970-stale). So `time() - pingularity_update_check_timestamp_seconds >
  172800` is "this box can't reach the feed" - the firewalled-install signature -
  and a week of `pingularity_update_available == 1` is an upgrade nobody noticed
- `pingularity_worker_up{worker}` / `pingularity_worker_restarts_total{worker}` -
  per background worker (`scheduler`, `pruner`, `netinfo`, `update-check`,
  `heartbeat`, `digest`, and `settings-retry` when a failed settings load armed
  it): `up` is 1 while its loop runs and 0 once it dies - gives up after
  repeated panics, or the process shuts down. A one-shot worker that COMPLETES
  its job (settings-retry succeeding) removes its series instead of reporting
  0, so `worker_up == 0` alerts match only real deaths; `restarts_total`
  climbing means it's thrashing
- `pingularity_stat{stat="monitor.pending_events"}` (a gauge) /
  `pingularity_stat_total{stat="monitor.event_dropped"}` - the outage-persistence
  retry queue's depth (0 = healthy) and a counter of transitions dropped for good
  when the DB stayed unwritable past the buffer cap (each drop leaves a gap in
  uptime history). `pending_events` is a depth, not a counter, so it is not seeded
  at startup: the series appears the first time an event has to be queued
- `pingularity_metrics_data_valid` - **1 only when every store read on this scrape
  succeeded**; 0 when any failed (so a DB outage that would otherwise be a silent
  `200` with missing/stale series is directly alertable). Paired with
  `pingularity_metrics_collector_success{collector}` / `_errors_total{collector}` /
  `_duration_seconds{collector}` / `_last_success_timestamp_seconds{collector}` for
  the `targets` / `aggregates` / `speed` / `uptime_floor` reads (`aggregates`
  tracks the LAST refresh attempt, so a store that fails after the cache once
  warmed still reads 0)
- **Well-named families** (Prometheus-conventional, one quantity + labels each,
  emitted alongside the generic `stat_total` below): `pingularity_probe_rounds_total`,
  `pingularity_probe_failures_total{reason}`, `pingularity_dns_attempts_total`,
  `pingularity_dns_failures_total{reason}`, `pingularity_outages_total`,
  `pingularity_outage_duration_seconds_total`, `pingularity_speed_runs_total{trigger}`,
  `pingularity_speed_failures_total{stage}`, `pingularity_notification_deliveries_total{destination}`
  / `_failures_total{destination}` / `_blocked_total{destination}` /
  `pingularity_notification_delivery_duration_seconds{destination}` (a
  `_sum`/`_count` summary in seconds, so `rate(_sum)/rate(_count)` is "the webhook
  got slow"),
  `pingularity_database_errors_total{reason}`, `pingularity_database_prunes_total`,
  `pingularity_database_prune_duration_seconds_total`,
  `pingularity_speed_run_duration_seconds` (a `_sum`/`_count` summary),
  `pingularity_probe_blips_total`, `pingularity_login_failures_total`,
  `pingularity_rate_limit_trips_total`, and the chart-aggregate cache accounting:
  `pingularity_series_cache_hits_total`, `pingularity_series_cache_expired_total`,
  `pingularity_series_cache_new_total`, `pingularity_series_cache_empty_total`,
  `pingularity_series_bypass_total` and `pingularity_series_queries_total`. Every
  chart request books exactly one cache outcome - hit, expired, new, empty or
  bypass - and every one of them but the hit goes on to run an aggregate, so
  `queries = new + empty + expired + bypass` is an identity you can check on the
  wire. A bypass is a sub-minute bucket, which skips the cache entirely. None of
  the six carries a window or bucket-width label, on purpose: one series each,
  rather than one per range the dashboard offers. They were readable all along as
  `pingularity_stat_total{stat="series.…"}` - the named families just make them
  findable
- `pingularity_stat_total{stat}` / `pingularity_stat{stat}` - the internal
  **operational** registry, keyed by a `stat` label. These are two families: the
  **counters** `pingularity_stat_total{stat}` (monotonic totals + float sums;
  query with `rate()` / `increase()`) and the **gauges** `pingularity_stat{stat}`
  (point-in-time values: high-water marks like `monitor.blip_streak_max`, the
  probe/DNS freshness timestamps, and the per-worker `worker.<name>.up` flags -
  so the family is present on every install from boot). Together they
  cover probe rounds (`monitor.rounds` - the liveness denominator - and
  `monitor.bad_rounds`), the probe-failure taxonomy (`probe.fail.<class>` -
  timeout/refused/dns/…), the DNS-resolve failure taxonomy (`dns.fail.<class>`),
  family flaps, IPv4-only vs IPv6-only downtime (`monitor.v4_only_down_s` /
  `monitor.v6_only_down_s`), brownouts (`monitor.degraded_episodes`), pause
  accounting (`monitor.pauses` / `monitor.paused_s` - why gauges froze),
  speedtest **runs by trigger** (`speed.run.<trigger>`) and **failures by
  stage** (`speed.fail.<stage>` - server_fetch/ping/download/…), exit-discovery
  traces and geo lookups (`netinfo.trace_ok` / `netinfo.trace_fail` /
  `netinfo.ipmap_*`, and `netinfo.cymru_fallback` - a Team Cymru lookup answered
  by a public resolver rather than your own; it climbs for every lookup during
  the minute your resolver is being bypassed after one failure, not once per
  failure), the auto city race
  (`speed.cityrace_decided` / `_silent` / `_unanchored`, and
  `speed.cityrace_field_reused` - runs that ranked the race's own list and
  pings instead of fetching again; every decided race should), the auto-select
  challenger (`speed.challenge` - every challenge attempt, whether or not the
  rival could be measured; `speed.challenge_won` - attempts where the rival
  took the seat; `speed.challenge_failed` - attempts where the rival could not
  be measured and the incumbent was measured instead; `speed.head_failed` -
  runs where the server the run led with could not be measured and the next
  ranked one was measured instead), webhook delivery (`.ok` / `.fail` / `.blocked` per
  destination), DB health (`db.*`), import/restore repairs (`import.*` - rows a
restore refused rather than silently dropped), the /metrics self-disclosures
(`web.metrics_targets_capped`, `web.metrics_label_collisions` - the operator's
sign that the target-series view was truncated or a normalized label collided),
notification-queue loss (`notify.outage_dropped`), and security signals
(`web.login_fail`, `web.stepup_fail`,
  `web.limiter_trips`). Always-on and monotonic. Product-usage counters (which
  settings change, dashboard loads) are **not recorded at all** - those
  emitters were removed; the `promStat` allowlist stays only as a guard so a
  future product counter can't leak onto `/metrics`.

## Health endpoints

Two unauthenticated liveness/readiness probes for a load balancer or orchestrator
(they expose no data, just a verdict, and bypass the DNS-rebinding guard, the
local-only filter, and auth so a bare-IP health check from an LB reaches them):

- `GET /healthz` - liveness: `200 ok` while the process serves. No dependency
  checks, so a transient DB hiccup can't trigger a restart loop.
- `GET /readyz` - readiness: `200 ready` once the store answers and the first status
  aggregate is warm; `503` otherwise, so an LB holds traffic until the daemon is warm.
  It also reports `503` when the daemon could not read its settings at startup - in
  that state it refuses every other route, `/metrics` and the dashboard included,
  rather than serve with access control it can't apply. `/healthz` keeps answering
  `200` throughout, so the container images' baked-in health check still reads
  `(healthy)` there: a whole-instance `503` beside a healthy `/healthz` means "check
  the log, then reload or restart", not "still warming up".

`pingularity healthz [-addr host:port]` probes `/healthz` from the command
line and reports by exit code (0 = answered `200`; anything else prints a
one-line reason). It exists for environments with no curl - it is what the
container images' baked-in `HEALTHCHECK` runs (see [Docker](../README.md#docker)).

## Scraping it

**Step zero for a remote Prometheus:** flip **Network access** on first - it
starts off on every install that never chose otherwise, containers included,
and until then every scrape from another machine gets `403`. Use
the Access tab, or start with `-access network` / `-e
PINGULARITY_ACCESS=network`. (A Prometheus on the same host scraping
`127.0.0.1:9000` needs nothing. A container upgraded from 0.61 or earlier needs
the same opt-in as any other install - it is not grandfathered into network
access, see [Docker](../README.md#docker).)

A minimal job (scrape by IP so the DNS-rebinding guard doesn't get in the way -
see the gotchas below):

```yaml
scrape_configs:
  - job_name: pingularity
    scrape_interval: 30s
    static_configs:
      - targets: ["192.168.1.10:9000"]   # the host running Pingularity
```

Three access controls can turn a scrape into a `401`/`403`:

- **Network access still off?** (the default on every fresh install) - remote
  scrapes get `403`. Enable it in the Access tab, or run the scraper on the
  same host.

- **Login enabled?** `/metrics` then sits behind auth, and scrapes get `401`.
  Either add the admin credentials as HTTP Basic, or (better) start Pingularity with
  `-metrics-token=<token>` and give the scraper a read-only token that can't change
  settings:

  ```yaml
      # admin credentials …
      basic_auth:
        username: admin
        password: your-password
      # … or a read-only token (with -metrics-token):
      authorization:
        credentials: your-metrics-token
  ```

- **Scraping by a public hostname?** The always-on DNS-rebinding guard rejects a
  `Host` header that's a public domain with `403`. IP targets and `*.local`/
  `*.lan`/etc. pass automatically; for a real domain, start Pingularity with
  `-allow-host=pinger.example.com`.

Starter queries and alerts against the operational series:

```promql
# Link down right now. Put for: 2m on the alert rule rather than widening the
# query: min_over_time(pingularity_up[2m]) == 0 only says "some sample in the last
# 2m was down", so one missed round pages as a two-minute outage. The
# probing_active conjunct keeps a deliberate pause quiet - pingularity_up holds its
# last value whenever rounds stop, so a bare == 0 would page forever.
pingularity_up == 0 and pingularity_probing_active == 1

# Outage count / downtime seconds over a day - robust even when an outage is
# shorter than the scrape interval (counts confirmed transitions, not samples)
increase(pingularity_outages_total[24h])
increase(pingularity_outage_duration_seconds_total[24h])

# The probe loop itself stopped (wedged/crashed prober, NOT a quiet link):
# no completed rounds for 5m while probing should be running.
rate(pingularity_probe_rounds_total[5m]) == 0 and pingularity_probing_active == 1

# 30-day uptime under 99.9% - but only trust it where the window was actually
# observed (coverage guards against a mostly-paused window reading falsely high)
pingularity_uptime_ratio{window="30d"} < 0.999
  and pingularity_uptime_coverage_ratio{window="30d"} > 0.95

# p95 anchor latency over 5m (from the histogram)
histogram_quantile(0.95, rate(pingularity_probe_latency_seconds_bucket[5m]))

# A background worker died, or the scrape returned incomplete data (DB failing).
# A worker that FINISHED its job (the one-shot settings-retry succeeding) removes
# its series instead of reporting 0, so this matches only real deaths.
pingularity_worker_up == 0
pingularity_metrics_data_valid == 0

# Speedtests have stopped landing (wedged scheduler, or every run failing). Not a
# plain time() subtraction: the last-run series appears only once a run has
# SUCCEEDED, so an install whose every speedtest fails has no series to go stale -
# and a bare subtraction keeps firing once scheduled tests are deliberately turned
# off. Gating on next_run (present only while the schedule is on) and treating an
# absent last run as a stale one covers both. Raise 7200 (2x the default 1h
# interval) above the longest gap a Speedtest schedule window leaves - a nightly
# window means ~22h of honestly stale last_run - and expect this to be true on a
# new install until the first run lands.
pingularity_speed_next_run_timestamp_seconds
  unless on(instance, job) (pingularity_speed_last_run_timestamp_seconds > time() - 7200)

# How overdue the next scheduled speedtest is (present only while scheduled tests
# are on). Read this one, don't alert on it: a next_run in the past is normal while
# a closed window or a busy link is holding the run back, because that deferral
# deliberately leaves the schedule anchor where it is. Nothing on /metrics tells a
# deferral apart from a wedge, so alert with the query above and use this one to
# answer "why hasn't it run" (the log says "speedtest deferred" too).
time() - pingularity_speed_next_run_timestamp_seconds

# Download below 100 Mbit/s on the last speedtest
pingularity_speed_download_mbps < 100

# DNS resolution failing while the link itself is up (name resolution broke)
pingularity_dns_up == 0 and pingularity_up == 1

# Last speedtest failed its configured thresholds, or a long current outage
pingularity_speed_healthy == 0
pingularity_current_outage_seconds > 300

# Speedtests failing by stage, per hour (server_fetch / ping / download / …).
# sum by (stage), not a bare sum: this is a labelled family with one series per
# stage, and summing without the label collapses all nine into a single number that
# answers the opposite of the question.
sum by (stage) (rate(pingularity_speed_failures_total[1h])) * 3600

# Webhook deliveries failing or SSRF-blocked - series exist at 0 from startup,
# so the first event is a visible 0->1 step for rate()/increase()
rate(pingularity_notification_failures_total[15m]) > 0
rate(pingularity_notification_blocked_total[15m]) > 0

# Average speedtest duration over 6h (the _sum/_n summary pair)
rate(pingularity_stat_total{stat="speed.duration_s_sum"}[6h])
  / rate(pingularity_stat_total{stat="speed.duration_n"}[6h])
```
