# Install, upgrade and run as a service

The full detail behind the [README](../README.md)'s install section: every channel, what the Docker flags are for, updating and rolling back, and running the daemon as a service - where its data lives, the key file, backups, and what happens when the database is damaged.


Prebuilt binaries and packages for Linux, macOS, and Windows (amd64 + arm64) are
published on every tagged release. Pick the channel that fits your OS - each one
lands the same single static binary. The `.deb`/`.rpm` packages also register
and start the background service for you; Homebrew and winget install the binary
and leave `pingularity install` to you (each section below says which).

## Linux

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

## macOS

> Requires macOS 13 Ventura or newer (the Go 1.27 toolchain's floor).

```bash
brew install pingular/tap/pingularity
sudo pingularity install    # registers the launchd service and starts it
```

`brew upgrade` later pulls new versions, and `sudo pingularity restart` switches the
running service to one; `sudo pingularity uninstall` removes the service (data
untouched).

## Windows

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

> **A webhook or heartbeat URL with a bare IPv6 address?** The same toolchain
> change made the URL parser refuse a host whose colons are not a port, so
> `http://fd00::1/hook` now fails with `invalid port "::1" after host`. Bracket
> the address - `http://[fd00::1]/hook`, which older builds take too - and it is
> delivered. `GODEBUG=urlstrictcolons=0` restores the old parsing for any other
> URL the stricter parser now refuses, but it does not deliver this one: the old
> parser read that address as the host `fd00:` and the port `1`, and looked up
> `fd00:` as a name, which is why it never arrived on the old build either. The
> same address with a port is the one that stops working:
> `http://fd00::1:8080/hook` was delivered by older builds, whose parser took the
> last colon for the port, and is refused now. For that one both remedies work:
> `http://[fd00::1]:8080/hook`, or `GODEBUG=urlstrictcolons=0`.

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

## Docker

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
> local-only would otherwise lock its published port out. One store does not
> honour it: a database the daemon rebuilt because it was damaged and it was
> asked to (`-on-corrupt rebuild`) has no password - that went with the file it
> set aside - and the flag was only ever safe with one behind it. That store
> answers loopback-only at every start, not just the first, until a password is
> set on it from the machine itself and the daemon is restarted or reloaded,
> network access is switched on there by hand, or `pingularity reset-auth`
> releases the hold (the way back for a bridged container; see the corruption
> notes under
> [the service section](#run-in-the-background-systemd--launchd--windows-service)).
> The flip side: while the flag or env stays pinned in your unit/compose file,
> changing **Network access** in the UI is overridden again at the next restart -
> drop the flag to let the UI choice stick.

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
> immediately, at that start and every later one - bar a store rebuilt from a
> damaged database on request, which has no password and holds itself to
> loopback until one is set on it and the daemon restarted or reloaded, network
> access is switched on at the machine, or `reset-auth` releases it
> ([`-on-corrupt`](cli.md)). It does not write
> the choice into the database, though: keep the env var in your `docker run` / compose
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
[iperf3 in a container](speedtests.md#iperf3-in-a-container) first: several iperf3 settings
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
the volume (the image's entrypoint pins the `run` subcommand, so override it).
It opens the database as it is: a path that is missing, empty or not a
database is refused, and it never creates a database or sets one aside. It is
also the way back onto the network for a container whose database was rebuilt
with `-on-corrupt rebuild`: that store has no password and holds network access
to loopback until one is set and the daemon restarted or reloaded, which a
bridged container cannot do through its own published port. `reset-auth` releases the hold and says so, and the next
start answers the network with no login - so claim it straight away.

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

## Updating

- **apt / dnf** - download the newer `.deb`/`.rpm` and reinstall it the same way
  (`sudo apt install ./pingularity_*.deb` / `sudo dnf install ./pingularity_*.rpm`);
  your data and env file are preserved, and the running service is restarted
  onto the new binary automatically. If it does not come back, the install says
  so and points you at `systemctl status pingularity`; apt and dnf themselves
  report a clean success either way.
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
touches your install. See [RELEASING.md](../RELEASING.md) for how that feed is
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
pingularity status          # running | stopped | not installed (macOS: with sudo, see below)
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
changing a flag means `uninstall` first, then `install` with the new set. Whatever
you pass reaches the daemon exactly as typed: on systemd the unit escapes a `%`, a
`$` or a backslash in a value, which systemd would otherwise expand or choke on.
Manage with `pingularity start | stop | restart | status | uninstall`. On macOS
`status` needs `sudo` like the rest: launchd shows a system daemon only to root,
so run unelevated it reports the state as `unknown` and points at
`pingularity healthz`, which asks the daemon itself. On Linux and Windows
`status` answers any user. On Linux and macOS a reload signal (`sudo systemctl
reload pingularity`, or `kill -HUP <pid>`) re-reads settings from the database
without restarting - how you pick up an out-of-band change like `reset-auth`,
and the way back from the `503` a daemon serves when it couldn't load its
settings at all. Windows has no reload signal;
restart the service there.

Beside the `-db` path - beside the link, when that path is a symlink to the
database - sit `logs.txt` (the log viewer's ring, so it survives a restart) and
**`pingularity.key`** (0600) - the key that encrypts the secret
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
- **A database that won't open is left where it is.** A torn file - typically a
  hard power-off mid-write - stops the daemon starting, and the message names the
  file, the command that tries to get the data back
  (`sqlite3 pingularity.db .recover | sqlite3 pingularity.db.recovered`), and the
  flag below. The file is not moved, replaced or repaired, because a refusal can
  be undone and a replacement cannot: recover it, check the copy kept your
  settings (`sqlite3 pingularity.db.recovered "SELECT key FROM settings"` lists
  them), put it back, start again. How much `.recover` gets back depends on where
  the damage is - most of the file for a torn table page; for a torn first page,
  anything from all of it to nothing, sometimes only a `lost_and_found` table with
  no settings table in it - and a copy with no settings in it is a
  brand-new install with no login: do not put that one back, take the flag below
  instead, which holds the network closed.
  Under a service manager set to restart forever this does mean restarting
  forever until you act - which is the point. It is loud, and it is the failure a
  periodic **Export** exists for.
  - **`-on-corrupt rebuild`** takes the other road, for an unattended box where
    monitoring matters more than the history: the daemon renames the file and its
    `-wal`/`-shm` sidecars to `pingularity.db.<UTC timestamp>.corrupt`, starts
    again on an empty store, and logs which file it moved - wherever in the file
    the damage sits, not only in the page the first statement reads, and whether
    or not enough of it survives to still look like a database. Monitoring
    carries on: the daemon knows this is no first run (it just moved your database
    aside), so it does not ask Quick Setup again or hold measuring for the 48h
    consent grace. The exception is an install torn while it was still *on* its
    first run - one that had never answered Quick Setup - and only when the old
    file can still say so: that comes back held, and offers the dialog again,
    because it never consented to anything. Nothing is deleted, so the old data is
    still there to inspect or hand to a recovery tool - but the dashboard comes
    back blank, every saved setting (login, network access, thresholds,
    notifications) is back at its default, and the quarantined copy keeps taking
    up its space until you remove it. Because the password went with the old file,
    the rebuilt store stays **loopback-only even if you passed `-access network`** -
    not for one start but for every start until a login stands behind the network
    again: set a new password from the machine itself, then restart or reload to
    get network access back. Switching **Network access** on there yourself releases the hold as
    well, since that is you deciding at the machine; a network setting stored by
    anything else does not - an older release the store is rolled back to stores one
    on any ordinary Save, and this build does not honour it and says why. A bridged container has no shell and its published
    port is not loopback, so there `pingularity reset-auth` (the one-off container under
    [Forgot the password?](#docker)) releases the hold instead - after which the
    next start answers the network with no login at all, like a fresh install, so
    publish the port to `127.0.0.1` and set the password straight away (see
    [claiming a container](security-model.md#claiming-a-container-before-someone-else-does)).
    The start that rebuilt the store answers
    [`/readyz` with 503](metrics.md#health-endpoints) for as long as it runs,
    and every later start does too while the hold keeps the network out, so a
    monitoring system hears about it instead of reading a healthy daemon over an
    empty dashboard.

  What the daemon will *not* do is touch a `-db` path that
  is not a file at all: a directory (the easy slip - `-db /var/lib/pingularity`
  for the file inside it), a device or a link that leads nowhere is refused with
  an error naming what it found, rather than re-permissioned or renamed. (One
  link to nothing is not refused: the one a rebuild cut short between its two
  renames leaves, with the store it finished beside the far end - the next
  start puts that store in place.) A
  symlink to a real file is fine - the database moved to a bigger disk with a
  link left behind is an ordinary arrangement - and the database is then the
  file the link names: that file is what is opened and tightened, what the
  refusal above names - so the recovered copy goes back in its place and the
  link goes on naming it - and what `-on-corrupt rebuild` sets aside. The link
  itself is never renamed or replaced, and
  `pingularity.key` and `logs.txt` stay beside the link, where they have always
  been - so keep `-db` pointing at the link. Pointed at the file it names
  instead, the daemon finds no key beside it and makes a new one, and saved
  iperf3 passwords stop decrypting until the old `pingularity.key` is copied
  across. `reset-auth` never sets a file aside either - it opens the database as
  it is, or refuses.
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
