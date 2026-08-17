# Security model

What Pingularity assumes, what it protects, and the decisions behind the
defaults. Written so you can decide how to deploy it, and so a reviewer can
tell a deliberate choice from an oversight.

Reporting: see [SECURITY.md](../SECURITY.md).

## The trust boundary

Pingularity is a single-operator tool. There are no roles, no tenants, and no
privilege levels inside the app. Anyone who can reach the dashboard and get
past the login is the operator, and the operator can do everything: read the
history, change any setting, wipe the database, and point alert webhooks at
nearly any URL. RFC1918 and loopback destinations are allowed on purpose, since
a self-hosted ntfy or gotify on the LAN is normal use, but link-local and
cloud-metadata addresses are refused at dial time and the webhook client
follows no redirects.

The boundary is therefore **the host it runs on and the network you expose it
to**. Everything below is about controlling those two things.

## What it runs as

The honest answer is a per-channel matrix, not a single "runs as root." Only two
things in the process ever want privilege, and each install channel grants the
least that covers them:

- **The data directory.** `/var/lib/pingularity` (or the platform equivalent)
  is created and locked down at startup (`0700`, owner only; on Windows a
  protected ACL granting only the service account, SYSTEM, and Administrators -
  which collapses to SYSTEM + Administrators when the service runs as
  LocalSystem). Whatever account the service runs as has to own it.
- **The exit-path traceroute.** The Connection panel traces where your traffic
  leaves your ISP. On Linux that uses a raw ICMP socket when it is permitted
  (root or `CAP_NET_RAW`), and falls back to the unprivileged ICMP datagram
  socket when it is not - which is the path a widened `ping_group_range`
  permits. The feature degrades rather than failing, but the full trace wants
  the privilege.

Nothing else needs it, so most channels drop root entirely and grant only the
raw-socket capability. Where each one lands:

| Channel | Runs as | Raw-socket privilege | Sandbox |
| --- | --- | --- | --- |
| **deb / rpm** (`apt` / `dnf` / `zypper`) | dedicated unprivileged `pingularity` user | ambient `CAP_NET_RAW`, bounded to just that | full systemd sandbox |
| **`sudo pingularity install`** (Linux self-install) | root | inherent | partial - no `User=`/`StateDirectory`, but `NoNewPrivileges`, `ProtectSystem=full`, `ProtectHome`, `PrivateTmp`, `ProtectKernelTunables`, `RestrictSUIDSGID`, `LockPersonality` |
| **macOS** (`sudo pingularity install`, launchd) | root | inherent | none |
| **Windows** (`pingularity install`, SCM) | `LocalSystem` | inherent | none |
| **Docker** | non-root, uid 65532 (distroless `nonroot`) | `cap_net_raw+ep` file capability on the binary - the effective bit makes it mandatory: without `NET_RAW` the container fails to start rather than run degraded (details below) | container isolation |

The rows in detail:

- **deb / rpm — the hardened one.** The packaged unit
  (`packaging/pingularity.service`) does not run as root. The postinstall creates
  a system `pingularity` account, and the unit runs `User=pingularity` with
  `AmbientCapabilities=CAP_NET_RAW` — the ambient set is what an unprivileged
  process actually keeps effective, unlike a plain `--cap-add` — bounded by
  `CapabilityBoundingSet=CAP_NET_RAW` and with `NoNewPrivileges=true` forbidding
  it from regaining anything more. On top of that it sets `ProtectSystem=strict`,
  `ProtectHome=true`, `PrivateTmp=true`, `ProtectKernelTunables=true`,
  `ProtectControlGroups=true`, `RestrictSUIDSGID=true`, and `LockPersonality=true`;
  `StateDirectory=pingularity` (mode `0700`) keeps the data dir writable through
  the read-only system tree. So a package install keeps the full traceroute
  without ever being root.
- **`sudo pingularity install` (Linux) — root, but not unsandboxed.** The
  self-install path registers a systemd service with no `User=`, so it runs as
  root and keeps neither the de-rooting nor `StateDirectory`. It does carry the
  directives that still bite under root: `NoNewPrivileges=yes`,
  `ProtectSystem=full` (not `=strict`, which would make the DB's own directory
  read-only for a root daemon writing under `/var`), `ProtectHome=yes`,
  `PrivateTmp=yes`, `ProtectKernelTunables=yes`, `RestrictSUIDSGID=yes` and
  `LockPersonality=yes` (svcopts_other.go). It matches the packaged unit in every other respect
  (`$PINGULARITY_OPTS` from `/etc/default/pingularity` - a native-systemd
  mechanism; the container images ignore that variable - a 5s restart, a bounded
  restart loop, `ExecReload` for the SIGHUP settings reload) but not the
  de-rooting — aligning it with the packaged unit is planned but not yet
  shipped, so today treat a self-install as root, though not unprotected. If you want
  the hardening now, a systemd drop-in that adds the `User=` / capability /
  sandbox lines above is the right place, and nothing in the app depends on
  running as root beyond the two items above.
- **macOS and Windows — root / `LocalSystem`.** The launchd daemon and the
  Windows service run at the platform's default system privilege. Dropping to an
  unprivileged account is clean on Linux but awkward here: on Windows the service
  manager maps a user account to a logon account that needs a password, and
  neither platform has the tidy `AmbientCapabilities` equivalent that would keep
  the traceroute working after the drop. Both use the OS's own privileged ICMP
  path instead (`IcmpSendEcho` on Windows), so nothing degrades.
- **Docker — non-root.** The image is distroless `nonroot` and runs as uid
  65532, never root. The raw-socket path is granted to the binary rather than the
  user: a build stage stamps `cap_net_raw+ep` onto it with `setcap`, so the
  capability is raised on exec even for the unprivileged process. `NET_RAW` is in
  Docker's default capability set, so the trace works with no `--cap-add`, and
  you can drop everything else (`--cap-drop=ALL --cap-add=NET_RAW`) and it still
  runs. Two hardening knobs interact with that file capability differently,
  and the difference matters. Removing `NET_RAW` itself (`--cap-drop=ALL`
  without adding it back, Podman 4+'s default capability set, a Kubernetes
  `drop: [ALL]`) does **not** yield a degraded trace: the capability's
  effective bit makes it mandatory at exec, so the kernel refuses to start
  the binary and the container exits immediately with "operation not
  permitted". Refusing to start is the honest outcome - it is loud, and the
  fix is the documented `--cap-add=NET_RAW`. `no-new-privileges`
  (`--security-opt no-new-privileges`, Kubernetes
  `allowPrivilegeEscalation: false`) is the opposite: the container starts,
  but a file capability cannot raise privileges under it even when `NET_RAW`
  is granted, so the daemon runs without the capability and only the trace
  degrades - the Exit row shows unavailable, everything else works. (The
  unprivileged ICMP fallback does not normally rescue it in a container - a
  fresh network namespace's `ping_group_range` admits no group - but opening
  that sysctl to gid 65532 restores the trace without any capability.) The
  graceful degradation native installs get exists because their binaries
  carry no file capability at all: the deb/rpm unit grants an ambient
  `CAP_NET_RAW` instead, and an unprivileged tarball binary simply loses the
  trace. That file capability stays on the `pingularity` binary alone: the
  `-iperf` image's bundled `iperf3` carries none, and a file capability does not
  survive the exec into it, so the iperf3 child runs with no privilege at all —
  deliberate, since the engine needs none. The one visible consequence is on
  kernels older than 5.7, where `SO_BINDTODEVICE` still demands `CAP_NET_RAW`:
  there, an iperf3 **Bind source** naming an interface (`--bind-dev`) fails in
  the container, while a deb/rpm install is unaffected because its *ambient*
  `CAP_NET_RAW` does carry into the child.

Independently of the account, the only path that runs another program on the
operator's behalf is the **iperf3 client**, which is off unless you select the
iperf3 engine. It runs a fixed `iperf3` binary with arguments built from your own
settings. It is not a general command path. In a container it can only run in the
`-iperf` image variant, which bundles `iperf3`; the default distroless image ships
no `iperf3` and no package manager to add one, so there the engine falls back to
Ookla. (On macOS the daemon also runs
`scutil --dns` to discover the system resolver. Same shape: a fixed program name
compiled into the binary, never read from settings, a request, or the database.)

## What it listens on

The default is `-listen :9000`, which binds every interface. On its own that
would put an unauthenticated dashboard on your LAN, so there are three layers
in front of it:

1. **The access filter, on by default everywhere.** A fresh install starts
   local-only - native or in a container - and every request from a non-loopback
   peer is refused with a 403 - bar `/healthz` and `/readyz`, which answer ahead
   of this filter so an off-box load balancer can probe the port, and which
   carry an ok/not-ready verdict and nothing else (see "What needs a login and
   what does not"). LAN access is something you turn on deliberately:
   `-access network`, `-e PINGULARITY_ACCESS=network`, or the Access tab.
2. **The login.** Optional, off until you set a password. Passwords are stored
   as bcrypt hashes, and password checks are rate limited per client: five
   consecutive failures buy a 30-second block, keyed on the peer address (an
   IPv6 /64, or the real client behind a `-trusted-proxy`). One container
   caveat: in a bridged container remote clients can arrive NAT'd from the
   container gateway's address (Docker Desktop, rootless Docker, and IPv6
   connections proxied to an IPv4-only container all do this; rootful Linux
   Engine's port DNAT keeps the real source), and naming that gateway in
   `-trusted-proxy` would trust the spoofable `X-Forwarded-For` header from
   **every** peer - any visitor could then choose its own rate-limit
   identity. Reserve
   `-trusted-proxy` for a real reverse proxy that overwrites the header;
   where that NAT applies and there is no such proxy, all peers share one
   bucket, which is the safer failure. The limit covers
   HTTP Basic on any gated route, not just the login endpoint. Separately,
   credentials that have already passed a bcrypt check are cached as a
   fingerprint keyed on the current hash, so a valid login is never locked out
   by someone else's failures. Changing the password invalidates that cache, and
   every outstanding session token with it. Note it does **not** clear an active
   block: only the cool-down does that.
3. **A startup warning.** If the listen address is non-loopback *and* the
   access filter is off *and* no password is set, the daemon prints a warning
   to stderr as well as the log, because the default install runs with logging
   off and that warning is aimed at exactly that install.

Two things worth knowing about the filter:

- **In a container it is on, and fail-closed.** Access is an explicit setting,
  never guessed from the network layout, so a container starts local-only like
  everything else. A bridged container NATs external traffic through the gateway,
  so even its own published port (`-p`) reaches the daemon as a non-loopback peer
  and gets a 403 until you opt in with `-access network` (or `-e
  PINGULARITY_ACCESS=network`) - set a login at the same time. A `--network=host`
  container sees real peer addresses, so local-only there behaves exactly as it
  does natively. Pingularity still detects a bridged container, but only to
  inform - the dashboard's container notice and the iperf3 container hints -
  never to decide access. An explicitly passed `-access` or `PINGULARITY_ACCESS`
  is authoritative at every start: if it disagrees with the stored setting, the
  daemon updates the stored value to match (in either direction) and logs the
  change - which also makes `-e PINGULARITY_ACCESS=network` the recovery path
  for an install whose saved local-only would otherwise 403 its own published
  port. There is **no upgrade exception**: a container carried over from 0.61
  or earlier - where the filter defaulted off - starts local-only too, and its
  published port answers 403 until the operator opts in. An earlier, unreleased
  build did make one, persisting network access for any store that *looked*
  like an upgrade (established, no birth marker, no stored access choice).
  That inference was unsound: the fail-closed default landed several commits
  before the birth marker existed, so a container born private under one of
  those pre-marker builds carries no marker and is byte-identical on disk to a
  genuine install from 0.61 or earlier - as is any pre-marker database copied
  into a container. On that whole population the migration silently opened an
  unauthenticated dashboard to the LAN, so it was removed: **provenance is
  never inferred for an access decision.** Date heuristics fail the same way
  (a restored backup re-opens the hole), so the ambiguity fails closed and the
  daemon only *explains* it - one warning naming the local-only state, why the
  shape is ambiguous, and the way out. A backup cannot make the decision
  either: `access_local_only` is denied in **both** directions, so it never
  leaves in an export and is never accepted from one. The same reasoning
  applies - the daemon will not persist an open posture on evidence far
  stronger than a file, and an export is something an attacker may hold or
  craft. Restoring moves the data; the destination decides who may see it. The cost is bounded and self-announcing: the 403
  body names the same fix, and an explicit `-access`/`PINGULARITY_ACCESS`
  restores the container at the next start without a shell. A heuristic may
  advise; it may never persist an access decision.
- **A same-host reverse proxy passes it.** Traffic arriving through a proxy on
  the same machine looks local, because it is. The filter cannot block a remote
  visitor arriving that way, and the daemon warns once when it detects the
  pattern. If you front the dashboard with a proxy, the login is your access
  control, not the filter.

We have deliberately not changed the bind default. It would break existing
installs and the one-click "enable Network access" flow, and the layers above
address the exposure that matters.

## TLS and the reverse proxy

**The binary never terminates TLS.** There is no HTTPS listener and no
certificate handling anywhere in it. If the dashboard needs to be reachable
beyond the machine it runs on, put a reverse proxy in front and let that
terminate TLS.

The session cookie is marked `Secure` when the deployment tells the daemon it
is behind such a proxy. It does not read `r.TLS`, which is always empty when
TLS is terminated upstream. Instead:

- With no `-allow-host` set, the cookie is not `Secure`. That is the plain
  local or LAN case, where the traffic is plain HTTP and marking it `Secure`
  would stop the cookie working at all.
- With `-allow-host` set, an `X-Forwarded-Proto` header decides it — but only
  when the TCP peer is in the `-trusted-proxy` set. The header is spoofable, so
  from an untrusted peer it is ignored entirely and the Host decides instead; a
  direct client cannot steer the cookie's Secure flag with a forged header. The
  leftmost value is used, so a multi-proxy chain reports the original client scheme.
- With `-allow-host` set and no such header, it infers from the `Host`: a host
  that needed allow-listing is a real public domain, so the cookie is marked
  `Secure`.

So the practical rule is: **if you front it with a TLS proxy, set
`-allow-host` to the public domain and have the proxy preserve the `Host`
header.** (In a container that flag goes after the image name - compose:
under `command:`; `PINGULARITY_OPTS` is expanded only by the native systemd
units and the images ignore it.) That one flag is what turns on the `Secure` cookie, and it is also
what tells the rebinding guard below to admit your public domain. Add
`-trusted-proxy` with the proxy's address so the login rate limiter keys on the
real client rather than on the proxy.

## DNS-rebinding protection

Always on, no configuration. A request whose `Host` header is a public domain
is refused with a 403 - again except `/healthz` and `/readyz`, which answer
ahead of this check too. This stops a malicious web page from using a visitor's
browser as a proxy into a dashboard on their own network, which is otherwise a
real attack against any local service.

What passes without configuration: IP addresses, `localhost`, dotless LAN names
such as `plex:9000`, and the `.local`, `.lan`, `.home`, `.internal` and
`.home.arpa` suffixes. Serving it on a real domain is exactly the case that
needs `-allow-host`.

## Speedtest servers are untrusted input

Speedtest destinations come from third-party catalogue data, so the daemon
treats them as hostile until proven otherwise. Every measurement connection -
ranking pings, health probes, uploads, downloads, the packet-loss probe - is
checked at dial time and refused if it points somewhere internal: loopback,
RFC1918, link-local (including cloud metadata), CGNAT (RFC 6598), the
documentation and benchmark ranges (RFC 5737 TEST-NET-1/2/3, RFC 2544), IETF
protocol assignments (192.0.0.0/24), and class E space (240.0.0.0/4). A server
that redirects a probe is re-checked against the same rules before the new
address is adopted.

Configuring an HTTP(S) or SOCKS proxy through the standard
`HTTP_PROXY`/`HTTPS_PROXY` variables (a `socks5://` value works in either)
changes the shape of the problem: the dial the guard sees is to your proxy,
and the real destination is only named inside the request. Which requests
travel the proxy follows the rules Go's HTTP stack applies - `HTTP_PROXY` for
http, `HTTPS_PROXY` for https, `NO_PROXY` exclusions honoured,
loopback/localhost destinations never proxied - and the endpoint probes take
the same route the transfers do. It is not a byte-for-byte match, though, and
the two known differences point opposite ways. `NO_PROXY` matching is not
punycoded here, so a Unicode entry and a punycode host (or the reverse) do not
cancel out: a request Go would send direct rides the proxy instead - where it
is still destination-vetted, which is why that one is left alone rather than
taking on an IDNA dependency for a mixed-spelling case. In the other
direction, a proxy value this daemon cannot use - a scheme it cannot speak
(`ftp://…`), or a value that does not parse as one - refuses the request
outright and names the offending value. Go's stack does none of that: it takes
an unusable scheme as that request's proxy without complaint, retries a value
that will not parse as `http://` plus the whole string - which can invent a
reachable endpoint nobody configured - and only when even that fails discards
the error and connects direct. Traffic leaving by a route the operator did not
choose is the outcome worth refusing, and nothing from a refused value enters
the dial-guard exemption below. `ALL_PROXY` is deliberately not
consulted: Go's HTTP client never reads it, so no request could ever ride an
endpoint named only there - and it earns no exemption from the dial guard,
which treats such an address like any other destination. The
addresses exempted from the dial guard are therefore exactly the configured
`HTTP_PROXY`/`HTTPS_PROXY` endpoints - your own configuration, and no wider
than what a request can actually traverse. One narrowing goes with that: in a
CGI environment (`REQUEST_METHOD` set) `HTTP_PROXY` is not consulted for http
requests and earns no exemption either, matching Go's own refusal to trust a
variable a client can set through a request header. Pingularity is a daemon,
so this is theory rather than practice - but the exemption follows what the
request can actually use, in both directions. The daemon then separately
validates the logical destination of every proxied request before the proxy
is asked to connect - per request and per redirect hop, so a server that
answers a proxied request with a redirect gets the new destination vetted
before the next hop is carried: internal IP addresses are refused outright,
hostnames are resolved and checked first, and hostnames that do not resolve
are refused rather than trusted. The *check* is per request, but a
hostname's resolved answer is reused for up to 30 seconds rather than looked
up again every time: a proxied transfer issues thousands of requests to one
name, and a resolver round-trip inline with each would land inside the
throughput being measured. Refusals are reused exactly like allowances, so
that reuse can never turn a "no" into a "yes" partway through a window.
Every discovery path passes the same filter, including the automatic city
race, which vets both a catalogue entry's host and its URL. One caveat
remains: the daemon's DNS lookup and your proxy's are two separate
resolutions, so a hostile hostname that answers differently to each (DNS
rebinding) could still direct proxied traffic at an address the daemon would
have refused - and the reuse window above is part of that same residual,
which is why it is a name's answer that is reused and never an IP literal's
verdict. Internal IP-literal entries - the realistic hostile shape - have no
such window: they are checked on the spot, every request, and never
memoized.

## What needs a login and what does not

When a password is set:

- Every `/api/*` endpoint requires it, and so does **`/metrics`** - which is not
  an `/api/` path and stays gated only because the exemption table names it
  explicitly. `/metrics` is also the one route that takes a second credential:
  with `-metrics-token` set, a scraper may present that token instead of the
  admin login (`Authorization: Bearer <token>`, or as the HTTP Basic password
  with any username), so Prometheus never holds the account that can change
  settings. It is consulted only while a login is active, and a wrong or absent
  token just falls through to the normal check, so the admin login still works
  there too.
- Four things are exempt from auth: login, logout, a `GET /api/access` so the UI
  can tell you what state it is in before you have signed in, and
  `/api/quick-setup`, the first-run dialog's endpoint. The access entry is the
  only one the exemption table scopes by method - `POST /api/access`, which
  changes who may reach the dashboard and what the password is, is gated like
  every other write. The other three name a path and nothing else, so every
  method reaches the handler; each handler then answers `POST` alone and returns
  405 to the rest.
- **Quick Setup is exempt from the guard, not open.** The exemption is what
  keeps a retry harmless: a browser whose first answer committed but whose
  response was lost would otherwise be refused by the very login that answer
  had just enabled. The handler gates itself on state instead of on a session.
  Once the answer is recorded, every later call is an "already done" 200 that
  writes nothing. Before that, a full answer - the one that would set access and
  a password - is refused with a 403, and which 403 depends on the 48-hour
  first-run window: while the window is open, an install already secured out of
  band answers "a login is already configured" and points at Settings > Access;
  once the window closes, the window gate refuses ahead of that check and
  answers "Quick Setup is no longer offered", on any install, secured or not.
  Neither writes anything. The bare "dismiss" marker takes neither path - it is
  deliberately still accepted after the window has closed, so a stale dialog can
  always close itself - but because it releases the boot monitoring hold, and is
  therefore a real write, it demands the same credentials as any gated endpoint.
  With the login active, no unauthenticated call to this endpoint writes
  anything: the only success it can reach is that "already done" no-op, and
  every other path is a refusal - 405 for a non-`POST`, 415/400 for a body it
  cannot read, 401 for `dismiss`, 403 for a full answer. The
  DNS-rebinding check and the access filter run ahead of the exemption table in
  any case, so nothing here widens who can reach the port.
- `/healthz` and `/readyz` are exempt from more than auth: they answer before
  the DNS-rebinding check and the local-only filter as well, so a load balancer
  hitting a bare IP gets an answer. They expose no data — an ok/not-ready
  verdict and nothing else.
- Static assets are exempt: the HTML shell, the font, the favicon. They contain
  no data.

So the UI shell loads without a login, and every byte of your history behind it
does not.

Forgot the password: `pingularity reset-auth` clears it and disables auth. That
is a local command, so it is gated by access to the machine, which is the same
boundary as the data itself.

## Data and secrets at rest

- **History** lives in one SQLite file in the data directory. It is not
  encrypted. The protection is filesystem permissions: the data directory is
  `0700` and the database `0600`, owner only, and on Windows a protected ACL
  granting SYSTEM and Administrators. The database and its `-wal`/`-shm`
  sidecars are re-tightened on every start, so a file restored or copied in from
  elsewhere is fixed up; the directory's mode is set when Pingularity creates it,
  and an existing directory is left as it is (a warning is logged if it is
  group- or world-readable). One carve-out to that leave-it rule: inside a
  container, at exactly the image's own data path (`/var/lib/pingularity`),
  owned by the daemon's user and carrying the marker file the image plants,
  the directory is re-tightened to `0700` at boot - Docker's volume copy-up
  loosens a fresh named volume's root, and that directory is Pingularity's
  own by construction, so the never-repermission rule protects nothing there.
  Keep it that way if you relocate the database with `-db`.
- **Secrets** you enter, such as iperf3 passwords, are sealed with a key stored
  as `pingularity.key` beside the database. That protects a settings export or
  a database copy, not someone who already has both files and the ability to
  read them.
- **The saved log snapshot** (`logs.txt`, beside the database) holds the raw
  log lines — unmasked IPs and hostnames — so it gets the same owner-only
  treatment as the database and key: `0600`, and on Windows a protected ACL of
  its own rather than the ACEs it would inherit from its directory, applied
  before the file ever appears under its final name.
- **Webhook URLs are bearer secrets.** A Discord or Slack webhook URL is enough
  to post as you, so treat it like a password. Delivery errors have the URL
  path and token stripped before they are logged. Be aware the wrapped cause
  can still name the **host**, for example in a DNS failure. The secret part is
  removed; the destination is not always.

## Things that are intentional and look odd

Collected here so you do not have to work them out from the source.

- **Import validates what it can, and trusts the operator for the rest.**
  `/api/import` is behind the auth guard, and what you are importing is a file
  this same tool exported — operator input, not attacker input. Even so, the
  rows a bad file can carry are checked for meaning rather than only for type:
  an outage's `type` must be the string `down` or `up`, an integer column must
  hold an integer, a pause span must be positive, start after the project
  existed and not run longer than the retention ceiling, and a `ts + duration`
  must not overflow. Those exist because a hand-edited or corrupt backup could
  otherwise rewrite every uptime figure, not because the endpoint is exposed.
- **The iperf3 congestion-algorithm setting is not checked against a list of
  valid values.** It cannot be. `-C` applies on the sending side, so for a
  download the valid set belongs to the remote server's kernel, which this host
  cannot enumerate. The setting is sanitised for shape, which is the only
  boundary that can be enforced honestly. A wrong value fails that test and
  nothing else.
- **Two things can leave the machine without you asking, and both can be turned
  off.**
  - **Connection info.** Fills the Connection panel (public IP, ISP, geo, DNS
    upstream, exit-router discovery) by asking the enrichment services listed
    in the README's outbound table, so your public IP does leave the machine.
    It runs at startup and then hourly. **Pausing monitoring stops it**, and
    the **Connection info** toggle in the Latency tab stops it for good. Both
    stop the AUTOMATIC lookups only: the panel's refresh button still works,
    because an explicit click is a request you made rather than something the
    daemon did on its own. With automatic lookups off the panel says so, and
    the figures it shows are whatever was last seen.
  - **The update check.** Daily, carries no identifiers, and has its own toggle
    in the About tab. It deliberately keeps running while monitoring is paused:
    a pause is a transient action, and it should not quietly stop you hearing
    about a security fix weeks later. Turn it off with its own toggle if you
    want silence.
- **The daemon collects no telemetry.** Nothing about your network, your
  measurements, or how you use the app is sent anywhere. There is no analytics
  SDK, no usage beacon, and no payload in any request that we send back to
  ourselves. The only request the daemon makes to us is the daily **update
  check** - a plain GET to `update.pingularity.dev` for the current version number.
  It carries no identifier, no version, and no data about your install: the
  request has an empty body and no query string, and the newer-or-not comparison
  happens entirely on your machine. Like any HTTP fetch it reveals the
  connecting IP to the server it talks to, as every web request does, but
  nothing about your install rides along with it. Turn the check off in the
  About tab and the daemon makes no outbound request to us at all.
- Everything else that leaves the machine is a measurement you asked for: the
  probes, the speedtests, the alert webhooks and heartbeat you configure, and
  the city geocode when you search for a speedtest server.

## Deploying it safely

Shortest version:

- **On your own machine only**: the defaults are already right. Leave the
  access filter on.
- **On your LAN**: set a password first, then enable Network access.
- **Reachable from the internet**: set a password, and put it behind a reverse
  proxy that terminates TLS. Do not expose `:9000` directly.
- **In a container**: local-only is enforced there too - a published port
  returns 403 until you opt in with `-access network`. Do that deliberately,
  and set a password at the same time. An upgrade from 0.61 or earlier is no
  exception: it needs the same explicit opt-in as a new install.
