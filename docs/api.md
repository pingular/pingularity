# HTTP API

Every endpoint the dashboard talks to, and the rules they share. Summarised
in the [README](../README.md#http-api).

Responses are gzip-encoded when the client sends `Accept-Encoding: gzip` and the
body is at least 1 KiB (smaller ones are sent as-is - gzip's framing can make a
short body bigger, and it already fits in one packet). Every such response
carries `Vary: Accept-Encoding`. The two streaming downloads, `/api/export` and
`/api/speed/runs.csv`, are always sent uncompressed so they keep streaming at
constant memory.

Every `POST` must carry `Content-Type: application/json`, **including the ones
with no body at all** (`/api/speedtest`, `/api/speedtest/abort`, `/api/netinfo`,
`/api/iperf/check`, `/api/speedtest/servers`, `/api/speedtest/candidates`,
`/api/auth/logout`), which answer `415` without it. It is a CSRF guard: a cross-site form cannot set that content
type without a preflight this daemon never grants. So
`curl -X POST -H 'Content-Type: application/json' http://127.0.0.1:9000/api/speedtest`,
and `-d '{…}'` where a body is listed below.

- `GET /api/status` - current status, uptime, per-family state, targets, latest
  speed, and live speedtest progress. Every uptime figure ships with its
  observation coverage (`uptime_coverage` per window, `uptime_custom_coverage`
  for `?upMins=`); a coverage of `0` means the window observed nothing and has no
  uptime to report, exactly as `pingularity_uptime_ratio` is then absent. A
  running speedtest is reported as `speedtest_running` plus `speedtest_run_id`
  (`0` when idle) - that id is what `/api/speedtest/abort` takes, so a stop can
  name the run it was decided against. A fresh install awaiting first-run consent
  reports `quick_setup_pending`, and `access_local_only` mirrors the loopback-only
  access filter (so a client can default the Quick Setup access choice to how the
  install booted); `bridged_container` is present only in a bridged container,
  where measurements describe the container network rather than the host's
- `GET /api/series?mins=…[&exclude=…]` - latency / online time series (server-side
  bucketed); `exclude` drops targets from the lowest-latency line. Also takes an
  absolute window as `?from=&to=` (unix seconds, half-open `[from, to)`; omit `to`
  for an open end), which wins over `mins`. Either form reaches at most 366 days
  back: a `from` older than that is silently raised to the floor, and a `mins`
  beyond it is ignored in favour of the endpoint's default window - so a window
  lying entirely further back than a year returns nothing even where retention
  kept the rows. The bucket width follows the part of
  the window that can hold data - `[from, min(to, now))` - so an omitted or
  future `to` buckets as if the window ended now rather than coarsening the lot
- `GET /api/events?limit=&offset=` - paginated up/down transition (outage) log.
  `limit` defaults to 10 (50 on `/api/speed/runs`) and is silently capped at 1000,
  the same ceiling `/api/logs` uses - page with `offset` and trust the `total` in
  the body rather than the length of the array you got back
- `POST /api/outages/delete` - `{ts}` delete one resolved outage (`ts` = the unix
  seconds of its closing up event); removes it from the log, heatmap, and
  uptime stats. Idempotent
- `GET /api/speed?mins=…` - speedtest history for the chart, capped at about 1500
  points. A window holding fewer runs than that is returned in full; a larger one
  is thinned by taking an even stride through the runs *by position* (not by
  time), always keeping the newest. Every point is a real recorded row, never a
  derived value - unlike `/api/series`, which buckets by time and reduces each
  bucket to one number (the lowest latency measured in it, and the mean DNS time). The stride is positional, so widening a window past 1500 runs re-picks
  from scratch: the newest run is always kept, but the other points are generally
  *different* runs rather than a superset of the narrower window's. The body stays
  a bare array; the disclosure is
  in the headers - `X-Total-Count` (runs in the window), `X-Returned-Count` and
  `X-Sampled` (`true` when thinned), so a client can tell a thinned answer from a
  complete one. For every run, use `/api/speed/runs` (paginated) or
  `/api/speed/runs.csv` - both cover the whole history rather than a window, so a
  caller that wants one window filters on `ts` itself. Also
  takes an absolute window as `?from=&to=` (unix seconds, half-open `[from, to)`;
  omit `to` for an open end), which wins over `mins` when present - with the same
  366-day reach as `/api/series`, so a window entirely older than that comes back
  empty however long retention keeps the runs
- `GET /api/speed/runs?limit=&offset=` - paginated run history (full detail).
  With **Discard losers** off, a Best-of round's other measurements are rows
  of their own here and on `/api/speed` (the chart plots them like any other
  run), each carrying `round_ts` - the winner row's `ts`; the latest-run
  endpoints and the digest never list them.
  Runs that recorded them carry `ip_family` (`4`/`6`/`mixed`, the family the
  transfer actually used) and `udp_direction` (`down`/`up`, which way the
  loss/jitter probe sampled); on runs that didn't establish one - and rows
  predating the fields - the keys are omitted rather than sent empty
- `GET /api/speed/runs.csv` - all runs as CSV. Everything added since the
  original column set is appended at the END, so consumers indexing existing
  columns by position keep working. In order, the tail is `ip_family` and
  `udp_direction` (blank = unrecorded); `round_of`, which on a row measured in
  a Best-of round that another server won (**Discard losers** off) carries the
  winner's timestamp and is blank on a test's own result; and then the same
  five the runs table's **Centre** column is built from - `win_reason`,
  `race_outcome`, `race_winner_label`, `race_winner_ms`, `race_racers`
- `POST /api/speed/runs/delete` - `{ts}` delete one speedtest run
- `GET /api/speed/runs/servers?ts=` - the server-selection report for one
  Ookla run (`ts` = the run's unix seconds) - every automatic run, challenge
  run, and pinned run writes one, not only Best-of rounds: every candidate that
  was ranked, raced, measured, or failed - each with its own numbers, the
  capacity the round believed, any direction it refused to believe, and the
  rule that made the winner win (`win_reason` on the winner row:
  `fastest_ranked`, `fallback`, `incumbent`, `on_net`, `challenger`, `challenger_won`,
  `challenger_failed`, `pinned`, and for Best-of rounds `score`, `favourite`
  (a starred server scored highest), `ping_bootstrap`, `pinned_bestof`,
  `pinned_companion` - the same value the runs table shows as the muted tag). `404` only when no such run exists; a run
  with no report (an iperf3 run, history from before the report existed, an
  old backup) answers `200` with an empty `servers` array
- `GET /api/speed/usage` - cumulative data used per window
- `POST /api/speedtest/candidates` - the field an automatic Ookla run would race
  right now: every origin's pool (exit router, ISP city, starred servers' cities,
  the last race's winning city, Ookla's placement), deduplicated and pinged, fastest first, plus the rest of
  the winning city's field pinged the same way (a Best of above 1 ranks the
  whole union instead, its pools widened to the count, so every row is then
  in the field and nothing more is pinged); `in_field` marks the rows a run
  would actually choose from, and `distance_km` is re-measured from the winning
  city for every row that has a coordinate; when Ookla's own placement wins
  (it has no coordinate to measure from) each row keeps the distance from the
  origin that listed it, so distances are then not comparable across origins;
  `409` while a speedtest is running
- `POST /api/speedtest/ping` - `{ids:[...]}` measure the ping to up to twelve
  Ookla servers by ID, the way the race pings (no transfer), probing each
  one's upload endpoint on the way - the picker's saved-pane refresh; answers
  `{pings:{id: ms|null}, health:{id: bool}}` (`health` only where the probe
  could tell, so a starred server outside every listing can earn its
  Unsupported mark); `409` while a speedtest is running
- `GET /api/speed/server-pings?ids=` - the median of each server's recent
  ranking pings from this daemon's own runs (no network); a server never
  ranked is absent
- `POST /api/speedtest/servers?city=` - list Ookla servers (near a city - `404`
  when the geocoder knows no such place, `502` when it could not be asked at
  all, a distinction the picker reports differently; `?id=<ookla
  id>` resolves one server by its Ookla ID instead - identity plus `fallback_ok`
  when its upload endpoint could be probed, no coordinate, and a `distance_km`
  of `0` that means *unknown*, because that endpoint reports the *caller's*
  position as the server's: `404` when Ookla answers and knows no such server, `502`/`504` when
  Ookla could not be asked; by default the list is centred where auto last
  tested, else near you)
- `POST /api/iperf/check?addr=` - check that an iperf3 server is reachable
- `GET /api/heatmap?days=365[&tz=Europe/Berlin]` - daily downtime, plus
  `window_s`/`observed_s` on every day it returns (how much of it was in range
  and how much was actually monitored). The response is **sparse**: a day earns a
  row only if it had an outage or was not watched end to end, so a year of clean
  monitoring comes back as a handful of rows rather than 366. An absent date
  therefore means "nothing on record", not "watched all day and clean" - history
  you deleted, or that retention pruned, is missing in exactly the same way, so a
  client must not read observation coverage out of a gap. `days` defaults to 365
  and is capped at 366; `tz` takes an IANA zone name and decides where each day
  starts (default: the server's own zone), so a client in another zone gets its
  own calendar days rather than the server's
- `GET /api/netinfo` - connection info (IP/ISP/DNS); `POST` forces a full refresh
- `GET|POST /api/settings` - read / update live settings. POST is a **partial**
  update: fields you omit keep their current value, and only the settings form is
  reachable here - monitoring (`/api/monitoring`), access/auth (`/api/access`), the
  update check (`/api/update`) and logging (`/api/logs`) have no field in it
- `GET|POST /api/access` - read / update access controls (local-only, auth, password); once auth is active, any change must carry `current_password`
- `POST /api/auth/login` / `POST /api/auth/logout` - session login / logout. The
  session cookie lasts **30 days**, and a logout revokes **every** signed-in
  browser rather than just the one that asked (the revocation is persisted, so it
  survives a restart) - which is also how you evict a lost laptop
- `POST /api/speedtest` - run a speedtest now
- `POST /api/speedtest/abort[?run=…]` - stop a speedtest in flight. A bare POST
  stops whatever is running when it arrives; `run=` (the `speedtest_run_id` from
  `/api/status`) stops only that run, and is **recommended for any client that
  might be delayed** - a stop decided seconds ago would otherwise kill a run that
  started in between. `204` = stopped, `409` = nothing matching to stop (idle, or
  that run already ended), `400` = `run` was not a run id. A best-of-N run that
  has already measured a server keeps that result; an abort before the first
  result stores nothing. `204` means the run was released, not that the traffic
  stopped: the Ookla engine's transfer does not observe cancellation, so it keeps
  moving bytes until its own 15-second capture window closes. Those bytes are still
  counted against your data usage, and a fresh run started immediately afterwards
  waits (up to ~20s) for the abandoned one to go quiet rather than measuring
  through it
- `GET|POST /api/monitoring` - read / set `{enabled}` master start/stop (the power toggle)
- `POST /api/quick-setup` - apply the first-run **Quick Setup** answer in ONE
  transaction (speedtest cadence, network access, update check, and an optional
  login) and mark it answered so the dialog never returns; `{dismiss:true}` marks
  it answered without changing anything else (once a login is active, dismissing
  requires that login too). `auth_enabled` must agree with
  whether a `password` is sent. Two things refuse a full answer with `403`: a
  login already being configured (change access under Settings then), and the
  first-run window having closed - the offer lasts 48h from first launch, and
  once it lapses this endpoint stops accepting full answers just as `/api/status`
  stops advertising it. `{dismiss:true}` keeps working past that point, because
  all it writes is the answered marker a lapsed window already implies, and a
  dialog left open on a stale tab should always be able to close itself. Fresh
  installs only; the offer is `quick_setup_pending` in `/api/status`
- `GET|POST /api/update` - update-check status / toggle the daily release poll
- `GET|POST /api/logs` - the About-tab log viewer: read recent lines (or
  `?download=1` for a text file, still the complete buffer - add `&masked=1` for
  the PII-masked form, which is the one to attach to a bug report; the raw form is
  the default here, since the redaction setting is a display choice) / set log
  level, PII redaction, or clear the buffer. A bare read returns the newest 500 lines;
  `?limit=` asks for a different window (capped at 1000, `?limit=0` for the whole
  buffer). Responses carry `limit` (the cap applied) and `buffered` (lines held),
  so a short answer can be told from a complete one, plus `epoch`, `first_seq`,
  `next_seq` and `dropped`, so a
  poller can pass `?since=<next_seq>&epoch=<epoch>` and be sent only what has
  arrived since - `since` is ignored unless `epoch` matches, because a restart
  reseeds the buffer and re-uses the same sequence numbers for different lines
- `POST /api/data/delete` - `{type: latency|speed|downtime}` clear that data
- `GET /api/export?config=1&latency=1&speed=1&downtime=1` / `POST /api/import` -
  export / import config + history. Pick at least one of those four categories (any
  non-empty value selects one); with none at all the export is a `400`.
  (JSON; export streams a single consistent snapshot with a small manifest; import
  streams in bounded batches and is **not atomic** - a mid-file error leaves earlier
  categories applied and returns `{partial:true, committed:{…}}`. Import puts no
  cap on the total request (a default install's own export outgrows any fixed one)
  and bounds the pieces instead: 8 MiB per record, 256 MiB per JSON element
  (413), 8 MiB per batch held in memory. In a file from Pingularity's own
  exporter, config is applied last, so a data failure can't half-change your
  settings; a hand-built or third-party file is applied in *its* key order, so put
  `config` last yourself)
- `POST /api/notify/test` - `{url}` send a test alert to a webhook
- `POST /api/notify/heartbeat/test` - `{url}` check in to a heartbeat URL. There is no dry run, so this counts as a real check-in and resets the watchdog's countdown

> All endpoints are **unauthenticated** by default; what protects a fresh
> install - native or container - is the **Network access** filter starting
> off (localhost only). Once you open network access for other devices or Prometheus, every
> device on the LAN can use every endpoint - the **Access** tab's **login**
> (cookie for browsers, HTTP Basic for API/Prometheus) is the fix if that LAN
> isn't fully trusted. Either way, the dashboard speaks plain HTTP: for
> exposure over untrusted networks, still front it with a TLS reverse proxy.
> The webhook test posts to a URL you supply, so treat access to the dashboard
> as access to that capability.
>
> **DNS-rebinding protection** is always on: requests whose `Host` header is a
> public domain are refused (403), which stops malicious web pages from using
> a local browser as a proxy into the API. IP addresses, `localhost`, dotless
> LAN names (`plex:9000`), and `.local`/`.lan`/`.home`/`.internal`/`.home.arpa`
> all work without configuration. Serving the dashboard behind a reverse proxy
> on a real domain? Set `-allow-host=ping.example.com` (comma-separate several)
> and have the proxy **preserve** the `Host` header - it must reach Pingularity
> as the public domain so the rebinding guard can vet it and the session cookie
> is marked `Secure`. Add `-trusted-proxy` with the proxy's address so the login
> rate limiter keys on the real client instead of the proxy.
>
> **Secrets at rest:** the database stores the login password hash and
> webhook/heartbeat URLs - so Pingularity creates its data directory `0700`
> and the database file `0600` (owner-only).
> Keep it that way if you relocate the DB with `-db`.
>
> **Legacy Docker volumes:** the database file is always owner-only, but a named
> volume created by an *older* image may have a group/world-readable directory
> root (Docker's volume copy-up loosens it). A volume first created by a current
> image is recognized as the daemon's own - its path, its owner, and a marker
> file the image plants - and re-tightened to `0700` at boot automatically. One
> created by an older image carries no marker, and Pingularity won't silently
> re-lock a directory it can't prove is its own - so it logs a one-line notice
> on start instead. The data is already private; to clear the notice, tighten
> the directory once from any container that can reach the volume (the default
> image has no shell to `docker exec` into):
> `docker run --rm -v pingularity-data:/data debian:13-slim chmod 700 /data`.
>
> The full picture - what the trust boundary is, what privilege each install
> channel runs with, what the defaults protect and how to deploy it safely - is in
> [docs/security-model.md](https://github.com/pingular/pingularity/blob/main/docs/security-model.md). To report a vulnerability,
> see [SECURITY.md](../SECURITY.md).
