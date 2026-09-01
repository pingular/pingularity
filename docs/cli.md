# Commands & flags

Every subcommand and flag `pingularity` accepts. Summarised in the
[README](../README.md#commands--flags).

```
pingularity [run] [flags]    monitor + serve the UI (default)
pingularity install [flags]  install as a service and start it (flags are persisted)
pingularity start|stop       start / stop the installed service
pingularity restart|status   restart / show status
pingularity uninstall [-y]   remove the service (data untouched)
pingularity reset-auth       clear the password + disable auth (recovery)
pingularity healthz          probe a running instance's /healthz; exit 0 = healthy
                             (-addr host:port, default 127.0.0.1:9000)
pingularity version          print version
```

Flags only **seed** the initial values - almost everything is adjustable live in
the settings drawer afterward and persists across restarts. A value you **save**
in the UI is persisted even when it equals what a flag currently supplies, and
wins from then on - removing the flag later keeps what you saved. Fields you
never save keep following the flag (or the shipped default) - but note that
**Save writes the whole drawer**: the settings form submits every field it holds,
so the first save from the UI pins every one of them a flag had moved off the
shipped default, not just the field you edited. Access, logging, the update check
and the power toggle aren't part of that form and are unaffected.

| Flag | Default | Purpose |
| --- | --- | --- |
| `-listen` | `:9000` | UI + metrics address (`127.0.0.1:9000` = local-only at the socket). Port `0` is refused: it asks the OS for a random port, which nothing can then find - not a bookmark, not a scrape target, not the container health check, which runs as its own process and cannot discover it |
| `-access` | `local` | who may open the dashboard: `local` (loopback only) or `network` (reachable from the LAN - set a login). A container that publishes a port needs `network` (or `PINGULARITY_ACCESS=network`), or the published port returns 403. Also settable in the UI - but unlike every other flag here, an explicitly passed `-access` / `PINGULARITY_ACCESS` re-asserts itself at **every** start, overwriting a disagreeing saved choice in either direction (and logging that it did), so while it stays in your unit or compose file a change made in the UI is undone at the next restart. Drop it to let the UI choice stick |
| `-db` | per-OS ([details](../README.md#run-in-the-background-systemd--launchd--windows-service)) | SQLite path (dir auto-created) |
| `-interval` | `5s` | time between probe rounds, `1s`-`1h` (a value saved in the UI takes precedence) |
| `-timeout` | `3s` | per-target dial timeout, `1s`-`30s` (a value saved in the UI takes precedence) |
| `-down-after` / `-up-after` | `2` / `1` | consecutive rounds to confirm down / up (1-10) |
| `-latency` | `true` | probe latency/connectivity at all (`-latency=false` = speedtest-only mode: no probe rounds, so no outage detection, no outage alerts, no DNS line, and no reconnect or while-degraded speedtests - the probe round is what triggers those) |
| `-ipv4` | `auto` | IPv4 probing: `auto` \| `on` \| `off` (`auto` = only while the host has an IPv4 address) |
| `-ipv6` | `auto` | IPv6 probing: `auto` \| `on` \| `off` (live) |
| `-speedtest` | `false` | run scheduled speedtests (startup + interval); opt-in. On-reconnect tests are governed separately by `-speedtest-on-reconnect`, the while-degraded trigger by its own UI toggle |
| `-speedtest-interval` | `1h` | time between scheduled speedtests, `1m`-`24h` |
| `-speedtest-on-reconnect` | `true` | speedtest after a reconnect (at most one per `-speedtest-interval`, and never more often than once per 15m) |
| `-retain` / `-retain-speed` / `-retain-downtime` | `720h` (30 days) / `8760h` (1 year) / `8760h` | prune windows in Go duration units (`0` = keep forever) |
| `-allow-host` | *(none)* | extra `Host` header values the DNS-rebinding guard accepts - only needed behind a reverse proxy on a public domain |
| `-trusted-proxy` | *(none)* | proxy IPs/CIDRs whose `X-Forwarded-For` identifies the real client, so one visitor's failed logins can't rate-limit everyone behind the proxy |
| `-metrics-token` | *(none)* | optional read-only token a scraper presents to `/metrics` (Bearer or Basic password) instead of the admin login, so Prometheus needn't hold an account that can change settings; only consulted when Require login is on |
| `-quick-setup` | `prompt` | headless first-run: `skip` starts monitoring immediately and never shows the browser Quick Setup dialog; `prompt` leaves it for a first visit. Passed **explicitly**, `prompt` is authoritative: monitoring flags on the same command line then only configure values and no longer count as consent, so the dialog still gates monitoring |

Out-of-range numeric flags are rejected at startup (and at `pingularity
install`) rather than silently adjusted - as are a fractional duration
(`-interval`, `-timeout`, `-speedtest-interval` and the `-retain*` windows take
whole seconds), a retention window over `87600h` (10 years; use `0` for forever),
an unrecognised value for `-ipv4` / `-ipv6` / `-access` / `-quick-setup`, and a
stray positional argument (`pingularity install run -listen :9000` fails rather
than quietly dropping the flags).

> **Headless installs:** a genuinely fresh install waits (monitoring paused) for
> a first-run consent - either the browser **Quick Setup** dialog or an explicit
> flag - so it does not start probing until someone has said to, or until the
> offer lapses: left unanswered, the hold releases 48h after first launch and
> monitoring starts with the defaults in the table above (which include
> `-speedtest-on-reconnect`, on by default, so a later link flap can run a full
> speedtest). The daemon prints that fallback on stdout at boot, so a headless
> install has it in its own log. Passing any monitoring flag (`-speedtest`,
> `-speedtest-interval`, `-latency`, `-interval`) counts as that consent -
> unless you explicitly pass `-quick-setup=prompt` alongside them, which keeps
> the dialog in charge and makes those flags configure values only. If you only
> tune other knobs (say `-timeout` or `-ipv6`) pass `-quick-setup=skip` so the
> service starts monitoring at boot instead of holding for the dialog.
