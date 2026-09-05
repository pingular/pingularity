# Pingularity

**[pingularity.dev](https://pingularity.dev)** · **[live demo](https://demo.pingularity.dev)**

A single-binary internet connectivity monitor with a built-in web dashboard,
native speedtests, and a Prometheus `/metrics` endpoint. It continuously checks
your connection by pinging several always-on internet landmarks at once and going
by majority vote (a *quorum* across multiple *anchors*), with anti-flapping
(*debounce*) so a single blip isn't mistaken for a real outage. It records
latency, uptime, and speed to SQLite and shows it all in a live UI - no runtime
to install.

This is the dashboard on a real install. The [live demo](https://demo.pingularity.dev)
is the same thing on synthetic data, with a badge to say so:

![The Pingularity dashboard: top-bar status bubbles, the Connection panel (IP / ISP / DNS / internet exit), the Speed panel - seven stat tiles over three stacked charts - the "Lowest round-trip across anchors" latency chart with its DNS line and clickable anchor pills, and a year-long downtime heatmap](https://raw.githubusercontent.com/pingular/pingularity/main/docs/dashboard.png)

## Contents

- [Quick start](#quick-start)
- [Install](#install)
  - [Linux](#linux)
  - [macOS](#macos)
  - [Windows](#windows)
  - [Docker](#docker)
  - [Updating](#updating)
- [Run in the background (systemd / launchd / Windows service)](#run-in-the-background-systemd--launchd--windows-service)
- [Architecture](#architecture)
- [Speedtests](#speedtests)
  - [iperf3 in a container](#iperf3-in-a-container)
  - [Scheduling and triggers](#scheduling-and-triggers)
  - [Choosing an Ookla server](#choosing-an-ookla-server)
- [Dashboard](#dashboard)
- [Commands & flags](#commands--flags)
- [Metrics](#metrics)
- [HTTP API](#http-api)
- [How it works](#how-it-works)
- [Design notes](#design-notes)

The last four are summarised here and written out in full beside the code, in
[`docs/`](docs/):

- [Commands & flags](docs/cli.md) - every subcommand, flag and environment variable
- [Metrics](docs/metrics.md) - every metric, the health endpoints, scraping and alert rules
- [HTTP API](docs/api.md) - every endpoint and the rules they share
- [Security model](docs/security-model.md) - the trust boundary and the reasoning behind the defaults

## Quick start

```bash
go build -o pingularity .   # requires Go 1.27.0+ (go.mod); pure Go, no cgo
./pingularity               # UI on http://localhost:9000
```

No flags needed - but a fresh install measures nothing until you open the
dashboard and answer **Quick Setup** (headless: start with `-quick-setup=skip`);
left unanswered it starts on its own 48h after first launch. Once started it
probes every 5s. The UI binds `:9000` by default, but every install starts
**private**: a built-in filter answers only the machine it runs on, and other
devices get `403` until you flip **Network access** on in the settings drawer's
Access tab (flip it on and hit Save; the tab shows the address to use), or start
with `-access network`. This is true in a container too - a published port
returns `403` until you set `-access network` (or `-e PINGULARITY_ACCESS=network`),
so a container is never accidentally exposed. `-listen 127.0.0.1:9000` hard-pins
it to local-only at the socket level regardless.

Connectivity is probed over **both IPv4 and IPv6** (each as an independent
quorum of three anycast anchors). IPv6 is auto-detected - skipped on IPv4-only
hosts - and the families are tracked separately, so an IPv6-only outage is
visible without falsely reporting the whole link down. Overall status is
"online" when *either* family has connectivity. Be precise about where that
shows up: a single-family outage appears in the live status bubbles, the raw
latency samples, and the `monitor.v4_only_down_s` / `monitor.v6_only_down_s`
counters - but **outage events, the downtime heatmap, and the uptime ratios are
driven by the overall state**, so a loss of just one family is not recorded as
downtime there. That is the intended reading of "either family": the link still
carried traffic.

## Install

Prebuilt binaries and packages for Linux, macOS, and Windows (amd64 + arm64) are
published on every tagged release. Pick the channel that fits your OS - each one
lands the same single static binary. The `.deb`/`.rpm` packages also register
and start the background service for you; Homebrew and winget install the binary
and leave `pingularity install` to you (each section below says which).

### Linux

Fastest path is a native package (sets up and starts the systemd service, drops
an `EnvironmentFile` for flags, and runs the daemon as a dedicated unprivileged
`pingularity` user granted just an ambient `CAP_NET_RAW` - enough for the
raw-socket traceroute behind the **Exit** panel - inside a systemd sandbox, so
it is never root):

```bash
# Debian / Ubuntu (.deb)
sudo apt install ./pingularity_*.deb

# Fedora / RHEL (.rpm)
sudo dnf install ./pingularity_*.rpm

# openSUSE (.rpm) - same package, openSUSE's own package manager
sudo zypper install ./pingularity_*.rpm
```

Both start `pingularity.service` immediately (`systemctl status pingularity`),
put the database in `/var/lib/pingularity`, and read flags from
`/etc/default/pingularity` (`EnvironmentFile`) - edit that and
`systemctl restart pingularity` to change them. Two kinds of flag won't work
there, because that unit is sandboxed: `/var/lib/pingularity` is its only
writable path, so a `-db` pointing anywhere else fails on a read-only
filesystem, and `CAP_NET_RAW` is its only capability, so a `-listen` port below
1024 can't be bound. Keep the database on the state directory and the dashboard
on a high port, or put a reverse proxy in front.

Prefer no package manager? Grab the `.tar.gz` for your arch from the
[Releases page](https://install.pingularity.dev), extract it,
and use the binary's own installer:

```bash
tar xzf pingularity_*_linux_amd64.tar.gz
sudo cp pingularity /usr/local/bin/
sudo pingularity install    # registers, configures, and starts the systemd service
```

Or run it as a container (see [Docker](#docker) for why the flags matter):

```bash
docker run -d --name pingularity --restart unless-stopped \
  --network=host --cap-add=NET_RAW \
  -v pingularity-data:/var/lib/pingularity \
  ghcr.io/pingular/pingularity
```

### macOS

> Requires macOS 13 Ventura or newer (the Go 1.27 toolchain's floor).

```bash
brew install pingular/tap/pingularity
sudo pingularity install    # registers the launchd service and starts it
```

`brew upgrade` later pulls new versions; `sudo pingularity uninstall` removes the
service (data untouched).

### Windows

```powershell
irm https://install.pingularity.dev/winget.ps1 | iex
```

One paste from **any** PowerShell. The script elevates itself (one UAC click),
updates a too-old winget first (fresh Windows images ship one that fails zip
installs silently), points winget at the maintainer-hosted package source
(`winget.pingularity.dev` - so installs and upgrades never wait on app-store
moderation), installs under `Program Files`, and registers + starts the
Windows service. Re-running the same paste later **updates**: it stops the
service, upgrades, and starts it again. Prefer to run the steps yourself?
This is what it does, from an **elevated** PowerShell:

```powershell
winget source add -n pingularity -a https://winget.pingularity.dev -t Microsoft.Rest --accept-source-agreements
winget install pingular.pingularity -s pingularity --scope machine
& "$env:ProgramFiles\WinGet\Links\pingularity.exe" install    # registers the Windows service and starts it
```

The `source add` is one-time (re-running it just reports the source already
exists). `--scope machine` matters - it installs under `Program Files`, where
the Windows service expects its binary; keep it on upgrades too. The last line
spells out the exe's path because winget adds `pingularity` to the PATH of
*new* shells only - from your next terminal onward, plain `pingularity` works.
If the install fails silently right after "Successfully verified installer
hash", your winget is outdated: update **App Installer** in the Microsoft
Store and re-run.

> **Custom certificate stores (macOS, Windows):** if `SSL_CERT_FILE` or
> `SSL_CERT_DIR` is set in the daemon's environment, its outbound TLS
> (webhooks, speedtests, the update check) now trusts the roots in those files
> instead of the operating system's keychain - a Go 1.27 behavior change. A
> stale or truncated file there breaks every TLS connection with certificate
> errors; unset the variable, or start with
> `GODEBUG=x509sslcertoverrideplatform=0` to restore the old behavior.

> **Downloaded a raw binary in a browser?** macOS Gatekeeper or Windows
> SmartScreen may block it as "unidentified". Clear the quarantine flag once and
> it runs:
> ```bash
> xattr -d com.apple.quarantine ./pingularity      # macOS
> ```
> ```powershell
> Unblock-File .\pingularity.exe                    # Windows
> ```
> Installs via **brew**, **winget**, **apt/dnf**, and **docker** don't trip this
> at all - the prompt only appears for a file you fetched directly with a browser.

### Docker

```bash
docker run -d --name pingularity --restart unless-stopped \
  --network=host --cap-add=NET_RAW \
  -v pingularity-data:/var/lib/pingularity \
  ghcr.io/pingular/pingularity
```

The image is multi-arch (amd64 + arm64). `--restart unless-stopped` is there
because a connectivity monitor that stays down after a reboot is silently
useless - Docker brings it back with the daemon unless you stopped it
yourself. Two more flags matter:

- **`--network=host`** (load-bearing) - Pingularity measures *your host's*
  internet path. Behind Docker's default bridge network you'd instead measure
  the container's NAT'd view (extra hop, wrong latency, and a traceroute that
  dead-ends at the Docker gateway). Host networking also means the UI is
  reachable on the host's `:9000` directly - no `-p` needed.
  **Docker Desktop (macOS, Windows) can't reproduce this the same way.** Desktop
  runs the container inside its own Linux VM, so `--network=host` attaches to
  *that VM's* network namespace, not your Mac's or PC's interfaces. Docker
  Desktop 4.34+ does add opt-in host networking (enable it under **Settings ->
  Resources -> Network**), but it only bridges **TCP and UDP** flows between the
  host and the VM - it still does not expose the host's own interfaces or
  anything below L4, so the raw-ICMP traceroute behind the **Exit** panel and
  true native-host parity remain unavailable. Even with it turned on, the
  readings describe the VM's path rather than your machine's. Speedtest numbers
  are capped the same way: the VM's traffic leaves through a user-space network
  proxy that terminates every connection and re-opens it from the host, so on a
  fast link the measured throughput can sit well below what the machine gets
  natively. A small VM compounds it - the Ookla engine sizes its parallel
  streams from the CPUs it can see, and VM defaults are often just 2. Giving
  the VM more cores and memory (Docker Desktop: **Settings -> Resources**;
  colima: `colima start --cpu 6 --memory 8`) wins back the streams and some
  headroom, but the proxy ceiling stays. All of
  this applies to every VM-backed runtime, not only Docker Desktop: colima,
  OrbStack, Rancher Desktop, and Podman machine share the design. Run Pingularity
  natively on macOS or Windows if you want the measurements to match the host.
  (The dashboard's container notice is raised for a *bridged* container, which is
  the case it can detect; it does not detect Docker Desktop's host-networking
  mode specifically.)
- **`--cap-add=NET_RAW`** (keep it - without it the container won't start) -
  the **Exit** panel walks a raw-socket ICMP traceroute to find where traffic
  leaves your ISP. The images grant that privilege through a file capability
  stamped on the binary (`cap_net_raw+ep`), and the effective (`+e`) bit makes
  it mandatory: when `NET_RAW` is missing from the container's capability set,
  the kernel refuses to execute the binary at all, so the container **exits
  immediately with "operation not permitted"** - it does not come up with a
  degraded trace. Stock Docker still grants `NET_RAW` by default, so plain
  `docker run` works without the flag today - but Podman 4+ dropped it from its
  default set, and `--cap-drop=ALL` / a Kubernetes `capabilities: {drop:
  [ALL]}` remove it too, so all of those need it added back
  (`--cap-add=NET_RAW`; Kubernetes `add: [NET_RAW]`) for the container to
  start. Spelling it out keeps the command correct everywhere. Two related
  setups behave differently, and the difference is that file capability:
  - **`--security-opt no-new-privileges`** (Kubernetes
    `allowPrivilegeEscalation: false`) does not stop the start: it blocks a
    file capability from raising privileges at exec even when `NET_RAW` is
    granted, so the daemon runs *without* the capability and only the trace
    degrades - the Exit row shows as unavailable, everything else works. The
    unprivileged ICMP fallback that can save the trace natively is normally
    closed in a container (a fresh network namespace's `ping_group_range`
    admits no group), but you can open it and win the trace back without the
    capability: `--sysctl net.ipv4.ping_group_range="65532 65532"` on a
    bridged container (the same sysctl via `securityContext.sysctls` on
    Kubernetes). Under `--network=host` the namespace is the host's, so
    Docker refuses `--sysctl` there - widen the host's own
    `ping_group_range` instead.
  - A **native** binary without the privilege degrades gracefully, because
    the release binaries carry no file capability - the deb/rpm unit grants
    an ambient `CAP_NET_RAW` instead, and a tarball binary run unprivileged
    just loses the trace. Only the container images make the capability a
    start condition.

> **Reaching the dashboard from other devices.** Every install starts
> loopback-only, containers included - it is never guessed open from the network
> setup. With `--network=host` the dashboard answers on the *host's*
> `localhost:9000`, but other devices on your LAN get `403` until you opt in with
> `-access network` (or `-e PINGULARITY_ACCESS=network`) - set a login at the same
> time. A bridged container that publishes a port with `-p` needs the same flag,
> or the published port returns `403`. An explicitly passed `-access` /
> `PINGULARITY_ACCESS` is authoritative at every start: it updates a disagreeing
> saved setting (in either direction) and logs the change, so
> `-e PINGULARITY_ACCESS=network` also recovers an install whose saved
> local-only would otherwise lock its published port out. The flip side: while
> the flag or env stays pinned in your unit/compose file, changing **Network
> access** in the UI is overridden again at the next restart - drop the flag to
> let the UI choice stick.

> **Upgrading a container from 0.61 or earlier?** Up to 0.61 a container
> answered the network by default; every install now starts private -
> **upgrades included**. An existing container install is *not* kept
> network-reachable across the upgrade: it starts local-only like everything
> else, so its published port answers `403` until you opt in, and that `403`
> body names the setting that refused you and both ways out (the **Access** tab
> from the machine itself, or `-access network` / `-e
> PINGULARITY_ACCESS=network` at start). The one-step fix is the env var: add
> `-e PINGULARITY_ACCESS=network` and recreate the container - an explicitly
> passed access mode is authoritative at every start, so it opens the port
> immediately, at that start and every later one. It does not write the choice
> into the database, though: keep the env var in your `docker run` / compose
> file, or - to make it stick without one - open the **Access** tab while the
> port is open, leave **Network access** on, enter your **current password**
> and hit **Save**, which does store it (a saved value persists even when it
> matches what the env var supplies), then drop the env var. Set a password at
> the same time. Your volume, database and history are untouched either way.
>
> The current password is the point of that step, not a formality: storing
> "network" is what makes the open port outlive the variable, so it is a real
> access change and is priced like one. Save it without the password and the
> setting still applies - nothing breaks, and every other setting on the page
> saves normally - but the choice is not written down, so dropping the env var
> returns the install to local-only. If a login is not configured yet, there is
> no password to enter and Save stores it as before.
>
> Why you have to say it, rather than the daemon working it out: what it can
> see is an established database that never stored an access choice, and more
> than one kind of install looks exactly like that. A container upgraded from
> 0.61 or earlier is one - its dashboard did answer the network. But so is any
> database that simply lacks the birth marker stamped on new installs,
> including one whose marker could not be written when it was created. The
> first wants opening; the rest were private all along, and guessing "open"
> for them would put an unauthenticated dashboard on the LAN. So the ambiguity
> fails closed. (A container carrying the marker is not ambiguous and is never
> warned about.) The daemon does say so when it sees the
> ambiguous shape: one warning in the log, `container install with no recorded
> access choice: access stays LOCAL-ONLY, so a published port answers 403 until
> you opt in`, with the same fix attached. It only explains - it never changes
> access on its own.

The **`-v pingularity-data:/var/lib/pingularity`** volume is what makes updates
safe: the SQLite database *and* `pingularity.key` (which encrypts saved iperf3
passwords) live there. Skip the volume and a `docker pull` + recreate throws
away your history and key. Pass flags as arguments after the image name, e.g.
`ghcr.io/pingular/pingularity -speedtest-interval 30m` - in compose, that means
under `command:`, e.g. `command: ["-allow-host=your.domain"]`. Pingularity does
not interpret `PINGULARITY_OPTS` as flags: that variable is expanded by the
native Linux systemd units from `/etc/default/pingularity`, and the container
images ignore it.

A **named volume** is the happy path: on first use Docker copies the image's
data directory into it - owner `65532:65532`, mode `0700`, plus a marker file
the daemon uses to recognize its own directory - so an unprivileged container
just works. (Some Docker engines loosen a fresh volume's root during that
copy; at boot the daemon re-tightens exactly that directory - its own path,
its own owner, the image's marker - back to `0700` and logs that it did.) A
**bind mount** is whatever host directory you point at it, and the image's
user won't own it: `chown 65532:65532` it first (and `chmod 700`), or run
with `--user <uid>:<gid>` matching the directory's owner. Either way the data
lands on the mount - the image's entrypoint pins
`-db /var/lib/pingularity/pingularity.db`, so a `--user` override changes who
writes, never where. On **Kubernetes**, mount the PVC at
`/var/lib/pingularity` and set `securityContext: fsGroup: 65532` so the
kubelet makes the volume writable for the pod (add `fsGroupChangePolicy:
OnRootMismatch` to skip the re-chown on every mount). The daemon notices the
resulting group-writable volume root at each boot and says exactly that: the
shape is how an fsGroup pod writes at all, so it explains it and leaves it
alone, and the database file itself stays owner-only. To make the directory
owner-only and end the notice, chown the volume root to uid 65532 once and
drop fsGroup.

Two image variants ship to the same repo. The default
`ghcr.io/pingular/pingularity` is a lean distroless image and deliberately ships
**no iperf3** - its base has no package manager, so the opt-in iperf3 speedtest
engine can't run there and speedtests fall back to Ookla. If you use the iperf3
engine, pull the `-iperf` variant instead
(`ghcr.io/pingular/pingularity:latest-iperf`), a debian-slim image that bundles a
working `iperf3` and is otherwise identical - same non-root uid 65532, same
`CAP_NET_RAW` binary, same volume layout, so every flag above carries over. If
you run it bridged rather than with `--network=host`, read
[iperf3 in a container](#iperf3-in-a-container) first: several iperf3 settings
name things only the host has, and the compose file there maps
`host.docker.internal` so a host-side `iperf3 -s` stays reachable.

**What `docker logs` shows.** Logging is off by default, but the daemon always
prints one startup line to stdout - version, listen address, access mode, and
dashboard URL, e.g. `pingularity 0.70.0: listening on :9000, access
local-only, dashboard at http://localhost:9000` - so a healthy container is
distinguishable from a hung one. A genuinely fresh install prints a second line
beside it - `first run: monitoring is on hold - nothing is being measured yet` -
naming the dashboard URL and `-quick-setup=skip`, because that is the state
where `docker ps` reads `(healthy)` and nothing is being recorded. Warnings and
errors (the ambiguous-access warning above, security warnings) still surface at
the default level; routine detail needs the log level raised in the About tab.

**Health check.** Both images bake in a
`HEALTHCHECK ["/pingularity", "healthz"]` (every 30s, 5s timeout, 10s start
period): the `healthz` subcommand fetches `http://127.0.0.1:9000/healthz`
from inside the container and exits 0 on a `200`, so `docker ps` reports
`(healthy)`/`(unhealthy)` with no curl or shell in the image. If you change
`-listen`'s **port**, or bind it to an address that excludes `127.0.0.1`, the
baked-in probe misses the daemon and the container reads unhealthy while it is
fine: in compose, override it with the exec form -
`healthcheck: { test: ["CMD", "/pingularity", "healthz", "-addr",
"127.0.0.1:8080"] }` - and with plain `docker run`, pass `--no-healthcheck`
on the default image (`--health-cmd` needs a shell, which distroless does not
have; the `-iperf` image has one). A bind that keeps port 9000 and still answers
on loopback - `:9000`, `0.0.0.0:9000`, `127.0.0.1:9000` - needs no override at
all.

**Read-only root filesystem.** The default image runs under
`docker run --read-only`: everything the daemon writes - the database and its
`-wal`/`-shm` sidecars, `pingularity.key`, and the `logs.txt` log snapshot -
lives beside the pinned `-db` path, on the volume. The `-iperf` variant wants
one addition, `--tmpfs /tmp`, if you use iperf3 **RSA auth**: the server's
public key is staged as a temp file for the iperf3 child, and with nowhere to
write it those runs fail with a clear `iperf3 auth: temp key` error (nothing
else is affected).

**Forgot the password?** `pingularity reset-auth` needs the database, and in
a container that means the volume - run it from a one-off container sharing
the volume (the image's entrypoint pins the `run` subcommand, so override it):

```bash
docker run --rm --entrypoint /pingularity \
  -v pingularity-data:/var/lib/pingularity \
  ghcr.io/pingular/pingularity:<tag> \
  reset-auth -db /var/lib/pingularity/pingularity.db
docker restart pingularity   # the running daemon caches settings in memory
```

Use the tag your container runs (`docker inspect pingularity --format
'{{.Config.Image}}'`), so the one-off binary matches the database it opens.

**Locked out of a published port?** A restore that forces access to
local-only (see [the restore notes](#run-in-the-background-systemd--launchd--windows-service))
leaves a bridged container's published port answering `403` - including to
the browser that ran the restore. Recreate or restart the container with
`-e PINGULARITY_ACCESS=network`: an explicit access choice at start overrides
the stored setting. Then set things right in the Access tab.

### Updating

- **apt / dnf** - download the newer `.deb`/`.rpm` and reinstall it the same way
  (`sudo apt install ./pingularity_*.deb` / `sudo dnf install ./pingularity_*.rpm`);
  your data and env file are preserved, and the running service is restarted
  onto the new binary automatically.
- **Homebrew** - `brew upgrade pingularity`, then `sudo pingularity restart`.
- **winget** - re-run the one-shot
  (`irm https://install.pingularity.dev/winget.ps1 | iex`), or by hand from an
  elevated PowerShell, in this order: `pingularity stop`, then
  `winget upgrade pingular.pingularity -s pingularity --scope machine`, then
  `pingularity start`. Stop first - winget cannot replace a running
  service's binary - and keep `--scope machine`, or winget reinstalls into
  your user profile and the service loses its program.
- **Docker** - `docker pull ghcr.io/pingular/pingularity`, then `docker rm -f
  pingularity` and re-run it (the named volume carries your data across).
  Coming from **0.61 or earlier** and reaching the dashboard from other
  devices? Add `-e PINGULARITY_ACCESS=network` to that re-run: every install
  starts private and the upgrade is not grandfathered, so without it the
  published port answers `403` (see [Docker](#docker)).
- **tarball** - Linux won't let you overwrite a *running* program file
  ("text file busy"), so either stop the service first, or copy alongside and
  rename over it (rename always works):
  ```bash
  sudo cp pingularity /usr/local/bin/pingularity.new
  sudo mv -f /usr/local/bin/pingularity.new /usr/local/bin/pingularity
  sudo pingularity restart
  ```

The in-app **update badge** notifies you when a newer release exists - it's a
poll of a maintainer-controlled feed (`latest.json`), notify-only, and never
touches your install. See [RELEASING.md](RELEASING.md) for how that feed is
published.

> **Rolling back to an older release?** Stepping the binary back (and forward
> again) is fine on its own - a downgrade does not rewrite your history. The one
> boundary that matters is **0.70**. That release added a marker on the
> bookkeeping rows a failed or partly-retried speedtest leaves behind so their
> bytes still count toward **Speedtest data used** without being shown as runs;
> builds older than 0.70 don't know the column is there. Point one at a database
> a 0.70-or-newer build has written and it reads those rows as real runs: a
> `0 Mbps / 0 ms` speedtest at the top of the dashboard, folded into the run
> averages, and published on `/metrics` as `pingularity_speed_download_mbps 0` -
> enough to fire a "download below X" alert. *Reading* is safe: nothing is
> damaged, the marker is left untouched, and coming back up on 0.70+ hides those
> rows again. *Deleting* is not: the older build removes only the row you
> clicked. Delete the bogus `0 Mbps` row and you have deleted an accounting row
> 0.70+ keeps on purpose, so its bytes leave **Speedtest data used** for good.
> Delete the real run it was billing for and that row is stranded instead -
> hidden again on 0.70+, still counting, with no run left to delete it by until
> retention prunes it. Do your deleting before you step down, or after you come
> back up. What does *not* heal is a **backup taken by the older build**: its
> export has no marker to carry, so restoring that file onto an install that
> does not already hold those rows - a fresh box, a rebuilt volume - brings them
> back as permanent 0 Mbps runs. Take the backup with the newer build, before
> you step down - though that file is your way back *up*, not a rescue while you
> are down: any run carrying the new columns stamps the export for the release
> that introduced them - 0.70 for the failed-run marker, higher again for the
> city-race verdict every Ookla run has recorded since - and an older build
> refuses a newer stamp outright rather than restoring half of it. One rung is
> yours to trigger: with **Discard losers** off a round's other servers are kept
> as rows of their own, which stamps the export a rung higher again, so take the
> backup before you turn it off (or turn it back on first) if the file has to
> restore onto an older release.

## Run in the background (systemd / launchd / Windows service)

The `.deb`/`.rpm` packages already do this for you; the steps below are for the
tarball, a `go build`, or a fresh binary you dropped in yourself.

```bash
sudo cp pingularity /usr/local/bin/
sudo pingularity install    # no flags - DB goes to /var/lib/pingularity, UI on :9000 (loopback-only until you turn Network access on); starts the service
pingularity status          # running | stopped | not installed
```

The database path and its directory are chosen and created automatically. On
Windows that is always `%ProgramData%\pingularity\` - service or not - so the
installed service and an admin prompt (`reset-auth`) find the same database.
On Linux and macOS what decides is root, not the service: as root (which is how
`install`'s service runs) it is `/var/lib/pingularity/` on Linux and
`/Library/Application Support/pingularity/` on macOS; as a regular user it is a
per-user data dir - `~/.config/pingularity/` on Linux (`$XDG_CONFIG_HOME`
honoured), `~/Library/Application Support/pingularity/` on macOS - with a
temp-dir path as the last resort when there is no home directory at all.
Any flags you *do* pass to `install` are persisted into the service definition.
On systemd you can change them later without re-installing: the unit that
`install` writes also reads `/etc/default/pingularity`, if you create one, so
`PINGULARITY_OPTS="-speedtest -listen 127.0.0.1:9000"` plus
`sudo systemctl restart pingularity` is enough. macOS and Windows have no
equivalent, and re-running `install` is not one: over a service that already
exists it fails with an "already exists" error and leaves the flags it was
installed with exactly as they were - on macOS, on Windows, and on a systemd
unit this CLI installed alike. Wherever `/etc/default` isn't an option, then,
changing a flag means `uninstall` first, then `install` with the new set. Manage
with `pingularity start | stop | restart | status | uninstall`. On Linux and macOS a
reload signal (`sudo systemctl reload pingularity`, or `kill -HUP <pid>`)
re-reads settings from the database without restarting - how you pick up an
out-of-band change like `reset-auth`, and the way back from the `503` a daemon
serves when it couldn't load its settings at all. Windows has no reload signal;
restart the service there.

Alongside the database sit `logs.txt` (the log viewer's ring, so it survives a
restart) and **`pingularity.key`** (0600) - the key that encrypts the secret
that has to be kept recoverable: each saved iperf3 server's password. iperf3 needs
it in the clear at test time (it encrypts it with the server's RSA key itself), so
unlike your dashboard login it can't be hashed. Two things follow:

- **Back up the key with the database** if you want those passwords to survive a
  restore. Delete or lose the key and you re-enter them - the daemon mints a fresh
  one at the next start. That also signs out every logged-in browser, because
  session cookies are signed with a second secret derived from this same file;
  which is the point, since it means a database travelling *without* its key can't
  mint a valid session for whoever picks it up. Your password, settings and history
  are untouched. (A key file that is present but not 32 bytes - a truncated copy, a
  half-restored backup - is refused rather than used: the daemon still starts and
  still monitors, but stores iperf3 passwords in the clear and says so on stderr
  until you move the file aside and let it make a new one.)
- The key lives next to the database, so this does **not** protect you from someone
  who can read the host. What it does protect is the database travelling on its own -
  a backup, a snapshot, a stray copy - which now carries ciphertext, not your password.

Backup exports never contain passwords at all (neither the login nor the iperf3
ones). **They are still sensitive files.** A config export deliberately carries
your **webhook URL** and **heartbeat URL** so that a restore is complete, and for
both of those the URL *is* the credential - there is no separate token to
withhold. Anyone holding an export file can post to your alert channel or tick
your dead-man's-switch (masking a real outage), so store and share it like a
secret, and rotate both URLs at their provider if one leaks. Two more
consequences worth knowing before you need them:

- **Copying the database files by hand?** Stop the service first. While it runs,
  recent rows live in a sidecar file (`pingularity.db-wal`) that a copy of just
  `pingularity.db` misses - on a young install that can be *everything*. A clean
  stop folds the sidecar back into the main file. (The Data tab's **Export** is
  the safe way to back up a *running* instance - it streams a single consistent
  read-snapshot, so categories can't skew across it. **A full-retention export
  round-trips**, however large - import puts no ceiling on the total file, only on
  a single record (8 MiB) or a single JSON element (256 MiB), which no real backup
  reaches.)
- **A database that won't open is set aside, not repaired.** A torn file - typically
  a hard power-off mid-write - would otherwise crash-loop the service forever, so
  instead the daemon renames it and its `-wal`/`-shm` sidecars to
  `pingularity.db.<UTC timestamp>.corrupt`, starts again on an empty store, and logs
  which file it moved. Nothing is deleted, so the old data is still there to inspect
  or hand to a recovery tool - but the dashboard comes back blank, and the
  quarantined copy keeps taking up its space until you remove it. This is the
  failure a periodic **Export** exists for.
- **Restoring a backup where login was enabled?** The export carries the
  "login on" preference but never the password, so on a machine that doesn't
  already have one the restore leaves login **off**, **forces access to
  local-only** so it can't fall open to the LAN, and tells you so - set a
  new password in the Access tab, then re-enable Network access. (Restoring onto
  the same machine, where the password still exists, keeps login working untouched
  and access unchanged.) Restoring onto a machine that already has its *own*
  password works the other way round: the backup's login **name** is ignored and
  yours is kept, and a backup that turns login **off** does not disable it. The
  import says so in both cases - the password never travels, so a foreign name
  paired with your hash would lock you out of the only page that can fix it.
  In a container, the local-only forced by that FIRST case - a backup with login
  on, restored where no password exists - can lock you out
  of a published port - `403`, including for the browser that ran the restore -
  and the import response says so; the way back in is restarting the container
  with `-e PINGULARITY_ACCESS=network`, then setting things right in the
  Access tab.
- **Restoring onto a *different* machine?** A backup never carries the source's
  install date, so the destination keeps its own answer to "monitoring since".
  That date is the denominator behind every uptime figure, and importing it would
  have this box reporting uptime over a stretch it did not watch. Restore the
  **history** too and the date moves anyway - derived from the earliest row that
  actually arrived, which is a claim the restored data backs up. A backup does
  not carry **who can reach the dashboard** either: whether a machine answers
  only itself or the whole network is a decision about *that* machine, so a
  restore never opens a dashboard to the network, and closes one only in the
  fail-closed case above - a backup with login on, restored where no password
  exists. Set it in the Access tab on the destination.
- **Restoring on an *older* version?** It will refuse the file rather than
  restore half of it. A backup is stamped with the oldest version that can read
  it, and that stamp is worked out from what the file actually contains - so a
  backup whose runs use nothing new still restores on an older build, and one
  that doesn't is turned away up front, before anything is written.

## Architecture

One static binary runs a handful of independent goroutine loops that share a
SQLite store and a live settings controller. Nothing else is required - the UI,
web font, and favicon are embedded; outbound calls are the probes themselves,
optional enrichment (geo/ISP/exit), speedtests (Ookla, or the iperf3 server you
point it at), the update check, and the alert webhook and heartbeat if you
configure them - the full inventory is the outbound-calls table below.

```mermaid
flowchart TB
  browser["Browser / Prometheus / curl"]

  subgraph bin["pingularity - single binary"]
    web["web<br/>UI · JSON API · /metrics<br/>(loopback filter + auth guard)"]
    monitor["monitor<br/>probe loop + debounce FSM"]
    prober["prober<br/>concurrent quorum dialer"]
    sched["speedtest scheduler<br/>(single-flight)"]
    netinfo["netinfo<br/>IP · ISP · DNS · exit node"]
    notify["notify<br/>webhook + heartbeat"]
    settings["settings<br/>live + persisted"]
    store[("store - SQLite/WAL<br/>samples · events · speed · settings")]
  end

  anchors["anycast anchors<br/>1.1.1.1 · 8.8.8.8 · 9.9.9.9 (v4+v6)"]
  ext["Ookla · ipify · RIPE IPmap<br/>Team Cymru · Cloudflare"]
  watchdog["external watchdog / chat webhook"]

  browser -->|HTTP| web
  web --> store
  web --> settings
  monitor --> prober --> anchors
  monitor --> store
  sched --> store
  sched --> ext
  netinfo --> ext
  monitor -. "settings read live" .-> settings
  sched -. "settings read live" .-> settings
  monitor -->|outage| notify
  sched -->|threshold| notify
  notify --> watchdog
```

| Package | Responsibility |
| --- | --- |
| `main` | CLI, OS-service lifecycle (`kardianos/service`), wiring |
| `config` | flags, defaults, the anchor target list |
| `prober` | concurrent IPv4/IPv6 quorum dialer |
| `monitor` | probe loop + debounced up/down state machine |
| `store` | SQLite persistence + the uptime/aggregate queries |
| `settings` | runtime-adjustable values, persisted and broadcast live |
| `speedtest` | Ookla + iperf3 testers, single-flight scheduler |
| `netinfo` | public IP/ISP/DNS + exit-node discovery (traceroute) |
| `notify` | webhook alerts + dead-man's-switch heartbeat |
| `web` | embedded UI, JSON API, `/metrics`, access/auth guard |

## Speedtests

A run records download, upload, ping, **jitter**, (best-effort) **packet loss**,
and **bufferbloat**, plus the bytes used and the connection it ran on (public IP,
ISP, DNS resolver). A run also records two facts that used to be
invisible: the **address family** the transfer actually used (IPv4, IPv6, or
`mixed` when one run genuinely used both) - read back from the run's own
connections, never guessed - and **which direction** its
loss/jitter probe sampled. Not every field is present on every run: a download-only
or upload-only run has no figures for the direction it skipped, packet loss is
optional and not always measurable, family and probe direction are recorded
only when the run really established them (the engine notes below say when
that is), and bufferbloat is absent when a transfer
phase was too short to sample, returned too few samples, the latency target
was unreachable, or too few idle probes survived the retransmit filter to leave a
baseline. That last case drops the figure on purpose: a single retransmit in the
idle number is worth about a second, enough on its own to cancel real bloat down
to zero, so a polluted baseline would report a clean link instead of an
unmeasurable one. Missing is stored as missing rather than as a zero, so charts
and thresholds can tell "not measured" from "measured, and it was bad". A run
that failed outright isn't a measurement at all: it is kept only as a flagged
data-usage row, which every measurement view filters out (see the data-usage
bullet under [Metrics](docs/metrics.md)).

The **ping** shown is the engine's own number, a mean over ten samples, so it
keeps matching what speedtest.net would report. A mean has no defence against an
outlier, though: one stalled handshake among nine fast ones reports several times
the real latency (and lands in jitter, which is their standard deviation, as a
much larger distortion still). So the run also keeps the **fastest** of those
same samples - no extra probes - and everything that *decides* on latency uses
that floor instead: which city wins the race for the centre, which server every
automatic run ranks first (and whether last run's server keeps its seat within
the max(2 ms, 15 %) band), which server wins a best-of round, which server the
very first run picks, and whether your ping threshold breached. A pothole shouldn't
pick your server or page you, but a genuinely distant link has a high floor too
and still breaches. iperf3 exposes no per-sample values of its own, so a run
measures the latency itself: five bare TCP handshakes to the server before the
transfer, reported as their **median** (if none of them land, iperf3's own
`min_rtt`, and failing that the idle baseline). There is no separate fastest
figure on those runs, so that median is both what is shown and what decides.

![The Speed panel: a header row with the RUN button, the server this test used and when the next one is due, and a window picker; below it a row of stat tiles (download, upload, ping, jitter, packet loss, and bufferbloat both directions) above three stacked time charts for speed, ping and bufferbloat, with window averages for download, upload and ping below them, per-chart show/hide toggles, a Show all runs button, and a save-as-image button](https://raw.githubusercontent.com/pingular/pingularity/main/docs/speed-panel.png)

**Bufferbloat** is the extra lag that appears only while the line is busy - the
reason a video call breaks up the moment a big download starts. Pingularity
measures it by pinging before the test (idle) and during it (loaded) - both
against a fixed target of its own (`one.one.one.one`, Cloudflare) rather than
the speedtest server, so only the gap between them is meaningful, and the idle figure will not match the **ping**
recorded above:

```mermaid
flowchart LR
  idle["idle link<br/>ping 24 ms"] --> load["speedtest saturates<br/>the connection"]
  load --> queued["your packets now wait in the<br/>modem's queue: ping 190 ms"]
  queued --> bloat["bufferbloat = 190 - 24<br/>= +166 ms under load"]
```

Both figures are **medians** of their probes - the idle one over only the probes
that survive a retransmit filter, since on an unloaded link a sample more than
500 ms above that burst's own minimum is an OS retry rather than latency. A burst
whose own fastest probe is already at or above a second holds no honest sample to
measure the rest against, and yields no baseline at all. The loaded phases keep
theirs, where a near-second sample is the bloat itself. The headline bufferbloat
number - the one the tiles show and the one your **max bufferbloat** threshold is
compared against - is `median(loaded) - median(idle)`. The chart also plots a
**p95** per direction, the sustained bad end of the distribution. p95 is
deliberately not the maximum: these are TCP-connect probes, and a single worst
sample on one is usually a SYN retransmission (a fixed ~1000 ms OS retry, and
~2000 ms for a second one) rather than queue delay, so a max-based number
reports round figures that say more about packet loss than about buffering. The
probes go to a fixed dual-stack name, and the address family that wins their
connection race is not taken on trust - a path that drops half its handshakes
still wins races constantly, and its retries would land in the baseline. So the
winner is graded with a short burst first, and only if that burst comes back
lossy is the other family resolved and measured, taking the job only if it grades
cleaner. A host reachable in just one family keeps it however lossy: lossy data
beats none.

There are two engines, picked in the settings drawer:

- **Ookla (speedtest.net)** - the default. Numbers match speedtest.net; no setup.
  Its own knobs: parallel connections (`0` = auto, which is one per logical CPU
  for downloads and at most 8 for uploads; a value you set instead applies to
  both directions, up to 16 - worth raising on a fast or high-latency link, or in
  a small VM that can only see two cores) and the packet-loss probe.
- **iperf3** - opt-in, run against your own `iperf3 -s` box (LAN, homelab, or
  VPS). It measures what Ookla can't: internal/LAN links and honest upload. Used
  only when the `iperf3` binary is installed (otherwise it falls back to Ookla) -
  present on a native install once you've installed iperf3, and in the container
  only in the `-iperf` image variant, not the default image.
  Its own knobs: parallel streams, duration, warm-up, TCP window, congestion
  control, MSS, DSCP, the loss/jitter UDP pass, and - per server - IP version,
  bind source, and optional RSA auth. In a bridged container several of those
  knobs point at things only the host has - see
  [iperf3 in a container](#iperf3-in-a-container) below.

**Direction** (both / download / upload, plus iperf3's simultaneous `--bidir`) and
**retries** (default `1`, at most `3`) are kept **per engine**: the Ookla and iperf3
tabs each carry their own pair, so tuning one never disturbs the other, and
switching engines switches which pair is in force. On Ookla, retries are also what
let a very slow uplink finish at all: when parallel upload streams are too slow for
any of them to complete inside the capture window, the retry falls back to a single
stream. Set Ookla's retries to `0` and that fallback cannot run, so on a link that
slow the upload always fails and records nothing. The run's download half is kept
either way - a "both" run that loses only its upload stores its download, ping and
jitter as a partial result, with the upload shown as unmeasured (the same contract
iperf3 has always had) - and the warning in the log says why and names the setting.

That UDP pass needs the iperf3 port open for **UDP as well as TCP** - the same
port, both protocols (`ufw allow 5201/tcp` and `ufw allow 5201/udp`, or the
equivalent security-group rules). Allowing only TCP is the usual reason a server
reports throughput perfectly while loss and jitter stay blank forever: the
control connection and both transfers are TCP and connect fine, and the UDP
datagrams are dropped without a refusal, so the pass waits out its window and
gives up. The daemon logs `iperf3 udp pass failed, loss and jitter unrecorded`
each time, and when the run's TCP transfers succeeded it names the firewall as
the likely cause. Nothing is retried later, so runs taken while the port was
closed have no loss or jitter to recover.

For iperf3, the separate UDP loss/jitter pass probes the same direction you
test: downstream normally, upstream for an upload-only run - so a one-direction
test on an asymmetric line reports loss for the direction you asked about. That
also means loss and jitter describe **one direction per run**, never both, and
loss on an asymmetric path genuinely differs by direction - so each sample now
records which way its probe ran. The loss and jitter readouts name the path on
hover, the run tooltip carries it in its Quality line, and it's exported as
`udp_direction` (`down`/`up`) in the API and CSV. An Ookla run records a
direction too: its packet-loss probe sends the datagrams from the client to
the server, so a probe that succeeded is recorded as `up`. Runs that never
measured loss/jitter - either engine's - and rows recorded before the field
carry no direction and are shown unlabeled rather than guessed at.

### iperf3 in a container

A bridged container (the default `docker run`/compose network) has its own
network namespace: its own `localhost`, its own interfaces and addresses, its
own `/etc/hosts`, and NAT between it and everything else. Several iperf3
settings are **host-referential** - they name things that exist on the host but
not inside that namespace. All of them fail loudly rather than mismeasure
quietly, and when the daemon knows it runs in a container, most of the failures
carry a container-specific explanation in the error itself (natively the same
errors mean exactly what they say, and get no such note):

- **A loopback server address** (`localhost`, `127.0.0.1`, `::1`) - inside a
  bridged container that is the *container*, so the connection is refused by
  the container's own (empty) loopback before it ever reaches the `iperf3 -s`
  on the host. The settings drawer warns as soon as a saved
  server points at loopback while the daemon runs bridged (a host-network
  container's loopback *is* the host, so it never warns there; it never blocks
  saving either - the operator may really mean the container), and a failed run's
  error explains the same thing. Use `host.docker.internal` (see the compose
  file below) or the host's LAN IP.
- **Server names the host resolves privately** - entries in the host's
  `/etc/hosts` and mDNS `.local` names resolve natively but not in a bridged
  container, which has its own hosts file and no mDNS responder. The run fails
  with iperf3's own name-resolution error (no container-specific hint for this
  one - the daemon can't tell a host-private name from a typo). Use an IP, or
  a name the container's DNS resolves.
- **Bind source = a host IP** (`--bind`) - the address doesn't exist in the
  container's namespace, so the bind fails ("cannot assign requested
  address"), and the error says so. Bind a container address instead, or use
  host networking.
- **Bind source = a host interface name** (`--bind-dev`) - interface names
  don't cross network namespaces, so it fails ("no such device"), and the
  error says so. Separately, on kernels older than 5.7 `SO_BINDTODEVICE`
  needs `CAP_NET_RAW`, which the `-iperf` image's `iperf3` deliberately does
  not have (the capability is stamped on the `pingularity` binary alone - see
  [docs/security-model.md](https://github.com/pingular/pingularity/blob/main/docs/security-model.md)) - so on those kernels
  `--bind-dev` fails in the container even for an interface that does exist
  inside it. Native installs are unaffected: the deb/rpm unit's *ambient*
  `CAP_NET_RAW` carries into the iperf3 child.
- **IP version = IPv6** - the default Docker bridge carries no IPv6 (unless
  you've enabled it in the daemon config), so a forced IPv6 run fails outright
  ("network unreachable"), and the error says why. The quieter half of the
  same problem is **Auto**: it doesn't fail, it silently measures IPv4 where a
  dual-stack native install would measure IPv6. That is why a run records the
  family its transfers actually used - shown beside the server in the runs
  table and run tooltip, exported as `ip_family` (`4`/`6`/`mixed`) in the API
  and CSV. iperf3 reads it back from each direction's own connection report,
  and `mixed` means the download and upload really landed on different
  families (dual-stack DNS can do that) - labeled `IPv4+IPv6` in the UI
  rather than picking a side. Ookla runs record it from the transfer's real
  connections; a run with no recordable connection - for example one carried
  entirely through an operator's proxy, where only the hop to the proxy is
  visible - stays empty, never guessed, like rows recorded before the field
  existed. An Ookla `mixed` claims less than an iperf3 one: a single recorder
  spans both directions *and* every retry there, so it means both families
  showed up somewhere in the run's transfers - a retried attempt landing on the
  other family is enough, and the two directions need not have differed.

Two more things a bridged container changes without any error at all:

- **MTU.** The Docker bridge defaults to an MTU of 1500 no matter what the
  uplink uses, so over a tunnel or PPPoE uplink with a smaller effective MTU,
  full-size packets fragment along the way. The UDP loss/jitter probe now
  sends **1200-byte datagrams** (1200 + 8 UDP + 40 IPv6 = 1248, under the
  1280-byte IPv6 minimum MTU), so the probe itself can't fragment on any sane
  path - container or not - and its loss figure can't be fabricated by dropped
  or late fragments. An oversized **MSS** setting doesn't error here either:
  the kernel silently clamps it to what the interface takes.
- **LAN line rate.** Bridged traffic crosses a veth pair and conntrack NAT,
  which costs real CPU per packet - against a fast LAN server the measured TCP
  rate can sit measurably below native line rate, most visibly at
  multi-gigabit speeds. The number honestly describes the container's network
  path; it just isn't the host's.

And one setting empties rather than fails: the **congestion control**
dropdown's suggestions come from
`/proc/sys/net/ipv4/tcp_allowed_congestion_control`, which exists only in the
host's initial network namespace - a bridged container can't see it, so the
dropdown arrives with no suggestions, and the UI now says why instead of
letting the empty list read as "this host supports no algorithms". An
algorithm you type or import is still passed to iperf3 unchanged.

**`--network=host` makes nearly all of this go away** on Linux Docker Engine:
the container shares the host's namespace, so loopback is the host, host IPs
bind, IPv6 works, multicast reaches the wire, and LAN tests measure at native
line rate. Two caveats survive it: `.local` *name resolution* still depends on
the image's own resolver (debian-slim has no mDNS module - prefer an IP or
`host.docker.internal`), and on kernels older than 5.7 an interface-name bind
still fails in the container (the capability note above). It is already the recommended way to run the container (see
[Docker](#docker) - including why Docker Desktop can't provide it).

The canonical compose file for the iperf3-enabled image lives at
[install.pingularity.dev/compose-iperf.yaml](https://install.pingularity.dev/compose-iperf.yaml) -
fetch it from there rather than copying a block from this README: the served
file is pinned to the image version it was published with and carries the
current comments, so it cannot drift from the daemon the way an inline
snapshot here could. Three of its pieces are the ones this section is about:

- It defaults to **`network_mode: host`**, for all the reasons above.
- Its `extra_hosts: ["host.docker.internal:host-gateway"]` maps
  `host.docker.internal` to the host's gateway address, so an `iperf3 -s`
  running on the host is reachable from the container by that name on Linux
  Docker Engine too (Docker Desktop resolves the name on its own).
- If you must stay bridged (published ports, Docker Desktop without host
  networking, an orchestrator that owns the network), it keeps the fallback
  as a commented **pair** - `ports: ["9000:9000"]` together with
  `environment: ["PINGULARITY_ACCESS=network"]`. Uncomment both or neither: a
  published port alone answers `403`, because every install starts private
  (see [Docker](#docker)) - and set a login when you opt in.

One honest caveat either way: a test against an `iperf3 -s` on the **same
machine** measures the container-to-host virtual path (or loopback, under
host networking), not any real network - fine as a smoke test, useless as a
line measurement.

### Scheduling and triggers

Scheduled speedtests are **off by default** - turn them on in the Speedtest
settings (or with `-speedtest`). Once enabled, they run on startup and on a
schedule (`-speedtest-interval`, default `1h`). Two extra triggers are governed
separately: a test runs **after a reconnect** (on by default;
`-speedtest-on-reconnect=false` to disable) - spaced out so a flapping line
cannot fire tests back to back: at most one reconnect test per
`-speedtest-interval`, or per 15 minutes when that interval is shorter. There is
also an optional **while degraded**
toggle in the Speedtest settings (off by default, needs scheduled tests on) that
fires a test when latency stays high without the link fully dropping - above
**Degraded above** (default `150` ms, `0` = off) for two probe rounds in a row,
re-arming once latency recovers. **Run now** (or `POST /api/speedtest`) always
works.

Only one speedtest runs at a time, and the triggers do not queue behind each
other. If a **scheduled** slot comes due while any other test is already
running, that slot is **skipped** and the schedule advances to the next one - it
is not retried, and not run late (the counter
`pingularity_stat_total{stat="speed.scheduled_skipped"}` records it). A slot
held back by a **closed window** or a **busy link** behaves the opposite way:
nothing was measured, so it keeps polling and fires as soon as the condition
clears. "Busy" is traffic on the busiest interface above **Busy above** (default
`5` Mbps) - and unlike the alert thresholds, `0` is not "off" here: it makes any
measurable traffic count as busy, so scheduled tests stop firing. Only scheduled
runs consult it; reconnect, degraded and **Run now** go regardless.

### Choosing an Ookla server

For Ookla, choose a server (Find by place or Ookla ID, then pick its row) or
leave **Auto - fastest near you**. A row badged **Unsupported** cannot be
chosen: that server has no HTTP speedtest endpoint (Ookla's legacy upload
path), so every test against it would fail - clicking it says so in the
footer instead of selecting it, and typing its ID into Find lists it with the
badge rather than pinning it (hover the badge for the reason). Such a server
can still be starred, and a server that is already chosen keeps its radio
even if it later earns the badge, so the picker never hides what the next run
will use. That badge comes from a cheap check - fetching the server's latency
file - which a host whose *upload* endpoint refuses everything still passes.
Those only reveal themselves when a run tries them, so when one refuses every
upload the daemon stops offering it to automatic selection for twelve hours,
and remembers that across a restart. A server that comes back and refuses
everything again earns a longer rest each time - twelve hours, then a day,
then three - because re-admitting a still-broken server costs a whole
measurement turn to rediscover. It is never permanent: a repaired server is
back within three days on its own, and one that has behaved for a week starts
over at twelve hours. Auto isn't just "nearest": every server that's effectively equidistant gets to race
(in a big city, a dozen providers all sit "0 km away" - one of each is pinged
rather than an arbitrary few, and the same rule seeds each candidate city's
six in the city race that picks the centre, with distance ties broken by the
echo the server list itself came back with) and the lowest latency wins -
judged on the **floor** of each server's ten probes, not their mean, because one
stalled probe among nine fast ones moves a mean by 20 ms and a floor by nothing
(the city race and the Best-of verdict use the same floor, so the three
decisions agree). Your own ISP's
server, when Ookla lists one nearby **and the sponsor name can be matched to
your ISP**, is guaranteed a place in the race - traffic to it never leaves your
provider's network, so it's the most likely winner - but it still has to win on
ping like everyone else. (That match is a name heuristic: if your ISP is unknown
or trades under a different name than it sponsors servers under, its server
simply competes on distance like any other.) Two ties are broken deliberately
rather than by jitter: a run **keeps the server the last automatic run
measured** while it is still among the servers this run pings (the winning
city's list, seeded as above) and still pings within max(2 ms, 15 %) of the
fastest (win reason `incumbent` when that kept it ahead of a faster server;
plain `fastest_ranked` when it was the fastest anyway), and failing that prefers your
ISP's own server inside the same band (`on_net`) - so the history compares
like with like instead of flipping between equivalent servers, while a server
that has gone bad loses its seat the run it goes bad, because the seat is
re-pinged every run rather than remembered - and an incumbent the winning
city's list does not carry is not pinged at all, and loses the seat the same
way. A server that still *pings* well but can no longer be **measured** - it
answers, then moves no bytes - is the one case pinging cannot see, and a
failed run records no winner for the next run to learn from, so it would hold
its place indefinitely, hourly, with nothing to alert you: the next ranked
candidate rides behind whichever server an automatic run leads with, is
measured when that one cannot be, and takes the place itself (win reason
`fallback`, counter `speed.head_failed`). The run after it leads with the
server that answered, so the failure usually costs one wasted attempt and no
more - though only while that server pings within the same hair of the fastest
that the paragraph above describes. Further out it cannot be preferred over a
faster-pinging server, so the wasted attempt repeats each run until the broken
one recovers or slows down; either way a real measurement is now recorded every
time, where before there was none. A pinned server has no fallback: the pin is
your answer to this question. Ping alone
never learns whether a rival is *faster* to transfer, so after every twelve
automatic tests (any unpinned Ookla run counts; the challenge itself lands on
the next *scheduled* single-server run, and with **Best of** above 1 it never
does, because every round already measures rivals) the run measures the
incumbent's strongest rival instead - one server, no extra data - and the
rival takes the seat only if its one score clears a bar set by the
incumbent's own last dozen same-direction runs: their median plus 15 %, or
their second-best hour if that is higher. So on a steady wired line the rival
needs a clear 15 % win; on a link whose runs swing by a fifth it has to beat
what the incumbent itself reaches on a good hour, or one lucky hour would
steal the seat and the next challenge would steal it back - nobody has to
know their link's noise as a number, the record already says it. A score that
lands more than three times the incumbent's good hour is not treated as a win
at all: one reading has no round to be disbelieved against, and a server that
buffers and acknowledges an upload without delivering it can report several
times the line - so it is judged as the record's median instead and keeps the
seat where it is. The reading itself is still recorded as the test's result;
what the daemon declines to do is hand a seat to a number the line cannot
carry. A fresh seat
needs three runs of record before it is challenged at all, and changing the
test direction starts that record afresh. There is nothing to set: the
cadence is `speed_challenge_every` on the settings API (default 12; 0 turns
the challenger off) and is deliberately not in the drawer. If the rival cannot be measured the incumbent is measured as the
fallback and the attempt still counts. Win reasons `challenger` (tried,
lost), `challenger_won` and `challenger_failed` record it; the
`speed.challenge` / `speed.challenge_won` / `speed.challenge_failed` counters
count it. The **centre** of that search is
measured, not guessed: the candidate cities your connection names - your
**ISP's exit-router city** (found by traceroute), the city your **IP's
geolocation** puts you in, the cities of any servers you have **starred**, the
city that **won the last race** (so a lookup going dark cannot make Auto
forget where its servers are - it is a candidate like the others, and still
has to win), and the one **speedtest.net itself** places you in -
each enter six of their nearest servers (at most five of the cities with a
coordinate of their own are fetched, in the order listed - with the exit and
ISP cities both known that leaves room for stars in three cities, and past it
the last race's city is dropped first, then further starred cities;
speedtest.net's own placement is never displaced) - seeded the way a run's own ranking
is, so a metro whose servers all sit "0 km" apart contributes one per provider
and your ISP's box rather than six by chance - into one deduplicated ping race, and the
city whose server answers fastest becomes the centre. (Ookla returns the
servers *around* a coordinate, so a different centre yields a genuinely
different list rather than the same one reordered - picking the wrong city can
hide the fast servers entirely, which is why it's raced. The lists usually
overlap, and where two candidate cities are close enough to be
interchangeable they collapse, so a server never gets to race twice.) The race
does the run's homework as it goes: the winning city's list and the pings the
race already took are what the run ranks, so it fetches nothing twice and only
pings the servers the race did not reach. Its verdict - which cities raced,
each one's fastest answer, which won and why - is recorded on the run (the
**Centre** column of the runs table; hover it for every city) so a surprising
city is explainable afterwards - and the muted tag after the server name
(`incumbent`, `challenger won`, `pinned`, …) says why that server was the one
measured; hover it for the rule. Searching a place in the picker only moves
the list you are looking at; nothing but a pinned server overrides the race.

## Dashboard

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
in full under [Metrics](docs/metrics.md)). On a default install - the Ookla
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
  **0.80.0-rc.1 and earlier** also sit a few ms above later versions and spike
  harder: every version through 0.80.0-rc.1 asked an IPv4/IPv6 question *pair*
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
  (blank = `1.1.1.1`; it must resolve to IPv4, see [How it works](#how-it-works)).
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
  the run's budget grows with N), so the estimate on the Speedtest tab turns
  amber above 4; above 1 the automatic challenger stands down. (Upgrading from
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
  [Docker](#docker)). **Local-only
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

## Commands & flags

`pingularity` runs the monitor and serves the UI by default; the other
subcommands install and control it as a service, print the version, or probe a
running instance's health. Flags cover the listen address and access mode, the
database path, probe interval and sensitivity, speedtest scheduling, and how long
each kind of history is kept. Only `-access` has an environment-variable form
(`PINGULARITY_ACCESS`), which is how a container sets it - `PINGULARITY_OPTS` is
expanded by the systemd unit and is ignored everywhere else.

**→ [docs/cli.md](docs/cli.md)** - every subcommand and flag, with its
default and what changing it costs.

## Metrics

Pingularity serves a Prometheus endpoint at `GET /metrics`, for people who
already run a Prometheus/Grafana stack - nothing external is required, and the
dashboard is fully standalone. It is a passive **pull** endpoint, behind the same
access guard as the dashboard, with an optional read-only token
(`-metrics-token`) for scrapers. It publishes the current link state, latency
histograms, the last speedtest's numbers, outage counters, speedtest failures by
stage, and worker health - enough to alert on a wedged prober, a failing
schedule, or a link that is up but slow. Beside it sit `/healthz` and `/readyz`,
answered ahead of the access guard so a container probe needs no credentials.

**→ [docs/metrics.md](docs/metrics.md)** - every metric and its labels, the
health endpoints, how to scrape it, worked alert rules with the reasoning behind
each query, and the importable Grafana dashboard.

## HTTP API

Everything the dashboard does, it does over this API, so anything the UI can do
is scriptable. Reads return JSON and are gzip-encoded when worth it; writes are
POSTs guarded by the same access rules as the dashboard. Two streaming
downloads - a full database export and the speedtest run history as CSV - are
always sent uncompressed so they keep streaming.

**→ [docs/api.md](docs/api.md)** - every endpoint, its shape, and the rules
they share.

## How it works

**Connectivity is a debounced state machine.** Every round, the prober dials all
anchors concurrently; each address family is "up" on a strict majority of its
targets, and overall is up when *either* family is. A confirmed flip needs
`down-after` / `up-after` consecutive rounds, which is what suppresses flapping.

```mermaid
stateDiagram-v2
  [*] --> Online: starts optimistic
  Online --> Offline: down-after consecutive failed rounds<br/>→ write 'down' event + alert
  Offline --> Online: up-after consecutive ok rounds<br/>→ write 'up' (with duration) + speedtest + alert
```

Any round that does not meet the threshold leaves the state where it is: a single
bad round while Online, or a run of successes shorter than `-up-after` while
Offline, changes nothing and writes nothing.

**Each round fans out into the raw series and the derived records.** Outage
*events* - not per-probe success - drive uptime and the heatmap, so those views
all agree.

```mermaid
flowchart LR
  round["probe round"] --> quorum{"per-family<br/>quorum"}
  quorum --> samples[("samples")]
  quorum --> fsm["debounce FSM"]
  fsm -->|confirmed flip| events[("events")]
  samples --> chart["latency chart"]
  events --> uptime["uptime % (24h / 7d)"]
  events --> heatmap["downtime heatmap"]
  events --> log["recent outages"]
```

**The store is seven independent time-series tables** (plus a key/value settings
table), tuned for a constant writer with WAL + `synchronous=NORMAL`.

| table | columns |
| --- | --- |
| `samples` | `ts` int · `target` text · `latency_ms` real · `success` int · `family` text |
| `dns` | `ts` int · `latency_ms` real (NULL when the lookup failed) · `success` int |
| `events` | `ts` int · `type` text (`up` \| `down`) · `duration_s` int |
| `pauses` | `ts` int · `duration_s` int - an unobserved span: paused, scheduled-off, or process-down |
| `pauses_quarantine` | `ts` int · `duration_s` int - pause rows held aside by clock repair, returned if the clock corrects |
| `speed` | `ts` int · `down_mbps` `up_mbps` `ping_ms` `jitter_ms` `packet_loss` real · `healthy` int · `server` text · `race_outcome` text (how the centre was chosen: `decided` \| `silent` \| `unanchored` \| `failed` \| `skipped` \| `bypassed_pin`) · `race_origins` text (every city that raced, with its fastest answer) · `race_winner_label` text · `race_winner_ms` real |
| `speed_servers` | `run_ts` int - joins `speed.ts`, one row per candidate in that run's server-selection report · `server_id` text · `rank_ping_ms` real · `score` real · `winner` int · `win_reason` text |
| `settings` | `key` text · `value` text - the key/value table, not a time series |

**Exit-node discovery** traces toward `1.1.1.1`, attributes each hop to an ASN,
and finds the ISP boundary - then geolocates the two boundary hops. The trace is
IPv4-only: on an IPv6-only host the Exit row shows as unavailable, and an
exit-path target that doesn't resolve to an IPv4 address falls back to tracing
the default `1.1.1.1` path (flagged in the UI).

```mermaid
flowchart TB
  refresh["netinfo refresh"] --> trace["ICMP traceroute → 1.1.1.1<br/>(native per OS: raw/ping socket on Linux,<br/>ICMP socket on macOS, IcmpSendEcho on Windows)"]
  trace --> asn["per-hop ASN<br/>(Team Cymru DNS, via your resolver;<br/>1.1.1.1 / 9.9.9.9 directly when it fails)"]
  asn --> boundary{"walk to the AS boundary"}
  boundary --> exit["exit router<br/>(last hop in the ISP)"]
  boundary --> handoff["handoff<br/>(first hop beyond)"]
  exit --> geo["geolocate: RIPE IPmap,<br/>then rDNS city fallback"]
  handoff --> geo
  refresh --> colo["Cloudflare PoP<br/>(/cdn-cgi/trace)"]
  geo --> panel["Connection panel · Exit"]
  colo --> panel
```

**Every request passes the access guard** before any handler runs, with two
deliberate exceptions: the [`/healthz` and `/readyz` probes](docs/metrics.md#health-endpoints)
are answered ahead of it, so a load balancer hitting a bare IP with no
credentials still gets its verdict (they carry no data to protect). Everything
else meets the DNS-rebinding `Host` check first, then the loopback filter
(judged on the real TCP peer, never the spoofable `X-Forwarded-For`), then
authentication - so a `403` on a public hostname is the rebinding guard talking,
not the filter.

```mermaid
flowchart TB
  req["request"] --> hz{"/healthz<br/>or /readyz?"}
  hz -->|yes| handler["handler runs"]
  hz -->|no| rb{"Host header a<br/>public domain<br/>not in -allow-host?"}
  rb -->|yes| d403h["403 (rebinding guard)"]
  rb -->|no| lo{"network access off<br/>AND peer not loopback?"}
  lo -->|yes| d403["403"]
  lo -->|no| au{"login required<br/>AND path gated<br/>AND not authenticated?"}
  au -->|no| handler
  au -->|yes| d401["401 (+ log failed attempt)"]
  handler --> resp["response"]
```

## Design notes

- **Quorum + debounce.** Each round dials several independent anycast anchors and
  applies a majority rule, and a confirmed up/down flip needs `down-after` /
  `up-after` consecutive rounds - so one flapping anchor or a single dropped
  packet can't manufacture a false outage.
- **Address families are independent.** IPv4 and IPv6 are each their own quorum;
  overall status is online when *either* is up, so an IPv6-only outage is
  recorded and shown without falsely reporting the whole link down. (IPv6 is
  skipped entirely on hosts without working IPv6.)
- **Uptime is real downtime, not a probe success rate.** The 24h/7d figures are
  derived from the debounced outage events (so they match the heatmap and outage
  log), clamped to the period actually observed - not the fraction of individual
  probes that succeeded, which would dip whenever a single family flapped.
- **Self-contained on purpose.** Pure-Go SQLite (no cgo) plus an embedded UI, web
  font, and favicon mean a single static binary with no runtime, no CDN, and no
  external database - install and run.
- **SQLite is tuned for a 24/7 writer.** WAL + `synchronous=NORMAL` keep the
  constant probe-write load cheap, a small connection pool lets dashboard reads
  proceed without blocking the writer, and the expensive uptime aggregation is
  cached briefly so the 3-second status poll stays light.
