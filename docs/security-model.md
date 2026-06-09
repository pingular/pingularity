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
  protected ACL granting only SYSTEM and Administrators). Whatever account the
  service runs as has to own it.
- **The exit-path traceroute.** The Connection panel traces where your traffic
  leaves your ISP. On Linux that uses a raw ICMP socket when it is permitted
  (root, `CAP_NET_RAW`, or a suitable `ping_group_range`), and falls back to an
  unprivileged ICMP datagram socket when it is not. The feature degrades rather
  than failing, but the full trace wants the privilege.

Nothing else needs it, so most channels drop root entirely and grant only the
raw-socket capability. Where each one lands:

| Channel | Runs as | Raw-socket privilege | Sandbox |
| --- | --- | --- | --- |
| **deb / rpm** (`apt` / `dnf` / `zypper`) | dedicated unprivileged `pingularity` user | ambient `CAP_NET_RAW`, bounded to just that | full systemd sandbox |
| **`sudo pingularity install`** (Linux self-install) | root | inherent | none yet |
| **macOS** (`sudo pingularity install`, launchd) | root | inherent | none |
| **Windows** (`pingularity install`, SCM) | `LocalSystem` | inherent | none |
| **Docker** | non-root, uid 65532 (distroless `nonroot`) | `cap_net_raw+ep` file capability on the binary | container isolation |

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
- **`sudo pingularity install` (Linux) — still root.** The self-install path
  registers a root systemd service with no `User=` and none of the sandbox
  directives above. It matches the packaged unit in every other respect
  (`$PINGULARITY_OPTS` from `/etc/default/pingularity`, a 5s restart, a bounded
  restart loop, `ExecReload` for the SIGHUP settings reload) but not the
  de-rooting — aligning it with the packaged unit is planned but not yet
  shipped, so today treat a self-install as root-and-unsandboxed. If you want
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
  runs.

Independently of the account, the only path that runs another program on the
operator's behalf is the **iperf3 client**, which is off unless you select the
iperf3 engine. It runs a fixed `iperf3` binary with arguments built from your own
settings. It is not a general command path. (On macOS the daemon also runs
`scutil --dns` to discover the system resolver. Same shape: a fixed program name
compiled into the binary, never read from settings, a request, or the database.)

## What it listens on

The default is `-listen :9000`, which binds every interface. On its own that
would put an unauthenticated dashboard on your LAN, so there are three layers
in front of it:

1. **The access filter, on by default for native installs.** A fresh install
   sets local-only access, and every request from a non-loopback peer is
   refused with a 403. LAN access is something you turn on deliberately in the
   Access tab.
2. **The login.** Optional, off until you set a password. Passwords are stored
   as bcrypt hashes, and password checks are rate limited per client: five
   consecutive failures buy a 30-second block, keyed on the peer address (an
   IPv6 /64, or the real client behind a `-trusted-proxy`). The limit covers
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

- **In a container it is off, because it cannot work.** A bridged container
  NATs every external request to the gateway, so the loopback test cannot tell
  a local user from a LAN one, and enforcing it would lock you out of your own
  dashboard. So it defaults off there, and **if you turn it on anyway it is
  ignored rather than enforced**: the Access tab still shows it as on, but no
  request is refused. Do not rely on it inside a container. The real boundary
  there is how you publish the port (`-p`) and the login password. The daemon
  says so in the log.
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
- With `-allow-host` set, an `X-Forwarded-Proto` header decides it, using the
  leftmost value so a multi-proxy chain reports the original client scheme.
- With `-allow-host` set and no such header, it infers from the `Host`: a host
  that needed allow-listing is a real public domain, so the cookie is marked
  `Secure`.

So the practical rule is: **if you front it with a TLS proxy, set
`-allow-host` to the public domain and have the proxy preserve the `Host`
header.** That one flag is what turns on the `Secure` cookie, and it is also
what tells the rebinding guard below to admit your public domain. Add
`-trusted-proxy` with the proxy's address so the login rate limiter keys on the
real client rather than on the proxy.

## DNS-rebinding protection

Always on, no configuration. A request whose `Host` header is a public domain
is refused with a 403. This stops a malicious web page from using a visitor's
browser as a proxy into a dashboard on their own network, which is otherwise a
real attack against any local service.

What passes without configuration: IP addresses, `localhost`, dotless LAN names
such as `plex:9000`, and the `.local`, `.lan`, `.home`, `.internal` and
`.home.arpa` suffixes. Serving it on a real domain is exactly the case that
needs `-allow-host`.

## What needs a login and what does not

When a password is set:

- Every `/api/*` endpoint requires it, **including `/metrics`**.
- Three things are exempt: login, logout, and a `GET /api/access` so the UI can
  tell you what state it is in before you have signed in.
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
  granting SYSTEM and Administrators. These are applied on every start rather
  than only at creation, so a database restored or copied in from elsewhere is
  tightened too. Keep it that way if you relocate the database with `-db`.
- **Secrets** you enter, such as iperf3 passwords, are sealed with a key stored
  as `pingularity.key` beside the database. That protects a settings export or
  a database copy, not someone who already has both files and the ability to
  read them.
- **Webhook URLs are bearer secrets.** A Discord or Slack webhook URL is enough
  to post as you, so treat it like a password. Delivery errors have the URL
  path and token stripped before they are logged. Be aware the wrapped cause
  can still name the **host**, for example in a DNS failure. The secret part is
  removed; the destination is not always.

## Things that are intentional and look odd

Collected here so you do not have to work them out from the source.

- **Import accepts opaque event types.** `/api/import` is behind the auth
  guard, and what you are importing is a file this same tool exported. It is
  operator input, not attacker input.
- **The iperf3 congestion-algorithm setting is not checked against a list of
  valid values.** It cannot be. `-C` applies on the sending side, so for a
  download the valid set belongs to the remote server's kernel, which this host
  cannot enumerate. The setting is sanitised for shape, which is the only
  boundary that can be enforced honestly. A wrong value fails that test and
  nothing else.
- **A saved log snapshot relies on its directory's permissions on Windows,**
  not on its own ACL. The snapshot is always written into the data directory,
  which is secured before the snapshot goroutine starts.
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
  ourselves. The one nuance, so the claim stays honest, is the daily **update
  check**: like any HTTP fetch it reveals the connecting IP to the server it
  talks to, and that server is a maintainer-run endpoint
  (`dl.pingularity.dev`). The request carries no identifier and no data about
  your install, but from the connecting IP the endpoint does keep a
  privacy-preserving tally - unique installs per day, bucketed by country. That
  is the only sense in which anything reaches us, it is derived from the request
  source alone, and the About-tab toggle turns the check (and the tally with it)
  off. See [RELEASING.md](../RELEASING.md) for how that endpoint and its count
  are run.
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
- **In a container**: the access filter cannot help you. Publish the port
  carefully and set a password.
