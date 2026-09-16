# Changelog

What a release does to an install that is already running: what to do before
upgrading, what changes without anyone asking for it, and what has been taken
away. New panels and fixes are in the release's own notes and in the commit
log - this file is for the things that reach a machine on their own.

Releases before v0.100.4 have no section here. The file starts where it was
first needed; nothing has been reconstructed after the fact.

## v0.100.4

Upgrading from v0.70.1. On a healthy database this is an in-place upgrade:
start the new binary on the same file. Every metric, endpoint and flag v0.70.1
served is still served, and nobody is logged out.

### Before you upgrade

- **Read the database once before you start the new binary on it.**
  `sqlite3 <your -db file> 'PRAGMA quick_check;'` should print `ok`. If it
  prints anything else, do not upgrade that file yet. This build reads tables
  v0.70.1 never read at startup - it builds new indexes across them - so damage
  that sat unnoticed under v0.70.1 can surface on the first start. When it does,
  the daemon now stops instead of replacing the file: it names the database,
  prints the `sqlite3 <db> .recover | sqlite3 <db>.recovered` command that tries
  to get the data back and the `sqlite3 <db>.recovered "SELECT key FROM settings"`
  command that checks the copy, and leaves the file where it is, still readable
  by v0.70.1 when the damage is in a table v0.70.1 does not read at startup.
  How much `.recover` returns depends on where the damage is: nearly everything
  for a torn table page; for a torn first page, anything from all of it to
  nothing - it can come back as a single `lost_and_found` table, with the
  settings rows loose inside it and no settings table to list.
  If the check lists your settings, put the copy in place and start again; if
  it lists nothing, do not put it in place - a copy with no settings in it is a
  brand-new install with no login - and use `-on-corrupt rebuild` instead. Under
  a service manager set to restart forever this means restarting until someone
  acts. This is a change, not a return to older behaviour: v0.70.1 replaced a
  database it could not open too, and only ever met the damage its own startup
  happened to read.
- **Take a backup and keep it somewhere the next one will not overwrite.**
  Backup & restore in the settings drawer, or `GET /api/export`. A backup
  carries the oldest version that can read every row in it, and this release
  added columns
  to the speed table: the moment one speedtest records them, the export stamps
  a version v0.70.1 will not read (it accepts up to 5, this build writes up to
  7). The refusal is clean - it happens before a single row is restored - but
  the backup that can take you back has to predate the first new speedtest, so
  the moment to take it is now.
- **macOS 13 Ventura or newer.** The binaries are built with Go 1.27, whose
  floor is macOS 13; v0.70.1 ran on macOS 12 Monterey. `brew upgrade` refuses
  on Monterey rather than installing a binary that cannot start, but the
  release archive and the raw binary carry no such guard: there the daemon
  simply never comes back. On macOS 12, stay on v0.70.1.
- **Look at what `-db` points at.** The path and the `-wal` and `-shm` files
  beside it are now inspected before the database is opened, where v0.70.1
  looked at none of them. A directory - the easy slip, the folder typed for the
  file inside it - a device, and a link that leads nowhere are refused by name,
  and nothing is opened, tightened or renamed until the path makes sense. (One
  link to nothing is not refused: the one a rebuild cut short between its two
  renames leaves, with the store it finished beside the far end - the next
  start puts that store in place.) A link to a real file is followed once, and
  the database is then the file the link names: that file is what is opened,
  what has its permissions tightened, what the refusal of a damaged database
  names - so the recovered copy goes back in its place and the link goes on
  naming it - and what `-on-corrupt rebuild` sets aside. The link itself is never
  renamed or replaced, and `pingularity.key` and `logs.txt` stay beside the
  link, where v0.70.1 kept them - so leave `-db` naming the link. Pointed at the
  file the link names instead, the daemon finds no key there and makes a new
  one, and the iperf3 passwords saved under the old one stop decrypting until
  `pingularity.key` is copied across. A link where a `-wal` or `-shm` sidecar
  should be is refused, because SQLite opens those beside the database directly
  and cannot follow one.

### Changed

- **Automatic Ookla server selection was reworked, and the "Auto location"
  setting is gone.** An automatic run now uses the server you pinned, or finds
  one itself: it races the cities it knows about, keeps the server it measured
  last time while that server stays within a hair of the fastest, and gives the
  strongest rival a turn every twelfth run. If you had pinned a city, nothing
  steers selection to it any more and your speed history will step at the
  upgrade - star the servers you want measured, or pin one outright. Every run
  now records the city it raced and why its server was the one measured, and
  the runs table shows both.
- **Each speedtest engine keeps its own direction and retry count.** A value
  you never set for iperf3 used to be filled in from Ookla's at every start; it
  now means the shipped default. An upgraded database keeps the iperf3
  direction and retries it had: the first start writes what iperf3 had been
  taking from Ookla under iperf3's own names, once, and says so in a line on
  stderr (a start that cannot write to the database just then runs with those
  values anyway, warns that it could not record them, and tries again at the
  next settings load), so your runs do not change and v0.70.1 reads the same
  values back if you step down. A value that was following an Ookla setting nobody had saved -
  the shipped default, `both` and one retry - has nothing to carry and stays
  unwritten, and there the two releases part company once Ookla's direction or
  retries change on either of them: v0.70.1 hands that value Ookla's at every
  start, and this release keeps the default. If you step down to v0.70.1 and
  back, check iperf3's direction and retries after each step. What the carry
  cannot reach is a database made by v0.70 or later that has already been
  started by v0.100.0-rc.1: the release candidate recorded the split without
  carrying the pair across, so an iperf3 direction or retry count that was
  still following Ookla's sits there at the shipped default, `both` and one
  retry, whatever Ookla is set to, and nothing on disk tells that apart from a
  choice made since. If you ran the release candidate and test with iperf3
  over a metered link, set iperf3's direction and retries yourself: `both`
  measures upload as well as download, so a run moves about twice the data
  that `down` alone does. A settings backup taken by v0.70.1 or earlier holds
  an iperf3 value that was still following Ookla's only as Ookla's: restoring
  it sets Ookla's, leaves iperf3 as this install has it, and the restore's
  reply names each value iperf3 was set to on the old machine that differs
  from what it runs here. An iperf3 value the backup holds of its own restores
  like any other setting.
- **A schedule that is on with no active day is named at boot.** A window that
  selects no weekday is saved as written and still means what it has always
  meant: it can never open, so the schedule parks what it gates - latency
  probing stops, no automatic speedtest fires. What changes is the silence.
  An install measuring nothing looks exactly like an install with nothing to
  report, so the daemon now prints one line per parked feature on stdout at
  every start, in the words the dashboard uses when it refuses to save the same
  schedule. The dashboard has always refused to save it; such a schedule
  reaches a database through the settings API, a restored backup, or a hand
  edit.
- **A `.deb` or `.rpm` upgrade that leaves the daemon down says so.** The
  package now watches the unit for a few seconds after handing over to the new
  binary, and prints `Pingularity upgraded but is not running; check: systemctl
  status pingularity` when it is not there. apt and dnf report a clean success
  either way - the unit is `Type=simple`, so a binary that execs and then exits
  is a start that succeeded - so that line is the only sign there is. A unit
  that was stopped before the upgrade is not restarted and says nothing, as
  before. One that was disabled but still running is restarted onto the new
  binary and watched like any other: disabling a unit keeps it from starting at
  boot, not from running now.
- **A database the daemon cannot open is no longer replaced unless you ask.**
  v0.70.1 renamed such a file to `<db>.<UTC>.corrupt` and started over on an
  empty store; this build does that only under `-on-corrupt rebuild`, and
  otherwise refuses to start (see *Before you upgrade*). Asked, it catches the
  damage at any stage of opening, not only at the first read, carries on
  monitoring, and the log line names what went with the old file: the history
  and every saved setting, the login and the network scope included. Unless the
  old file had not got past its first run - then the rebuilt one starts over as
  one, measures nothing until Quick Setup is answered or its 48h grace runs out,
  and the log line says that instead. The empty store is built whole beside the
  damaged file before that file moves, so a full disk or a crash part-way
  leaves the damaged file where it was, and a start that dies between the two
  renames is finished by the next. A rebuilt store has no password, so:
  - it answers loopback only whatever `-access` or `PINGULARITY_ACCESS` says -
    at the start that rebuilt it and at every start after - until a password is
    set on it from the machine itself and the daemon is restarted or reloaded,
    network access is switched on there by hand (the Access tab, or Quick Setup
    on a store still on its first run), or `pingularity reset-auth` releases the
    hold. The log says why at every start the hold applies, and a network visitor
    refused by it is told what ends it.
    A bridged container, which cannot reach its own loopback address and has no
    shell, runs `reset-auth` against the volume; its next start then answers the
    network with no login, like a fresh install, so claim it straight away.
  - a network setting found stored under the hold is not honoured, and the log
    says so. An older release the store is rolled back to stores one on any
    ordinary Save; the Access tab shows the local-only scope in force, and the
    next Save there stores it.
  - `/readyz` answers `503` for as long as the start that rebuilt it runs, and
    at every later start for as long as the hold keeps the network out, while
    `/healthz` stays `200`. A start that cannot read whether its store carries
    the hold keeps the network out and answers `503` too, without claiming a
    rebuild, until a settings load reads it.
  - rolling back to v0.70.1 lifts the hold for as long as v0.70.1 runs: it does
    not know it, and with `-access network` it answers the network with no
    password. Set a password before you roll such a store back. Upgrading again
    holds the network again unless a login was set in the meantime - by anyone
    who reached the rolled-back dashboard - and the log says when a login found
    on the store ends the hold. A backup v0.70.1 exports from such a store
    carries the hold with it: restored under v0.70.1 into another install, it
    holds that install's network once it upgrades - with a warning naming a
    rebuild that install never had - until `reset-auth` or the Access tab
    releases it.
- **Moving from Go 1.25 to Go 1.27 unpins five standard-library behaviors**
  that v0.70.1's binary held at their older settings. Two can reach you:
  - `SSL_CERT_FILE` and `SSL_CERT_DIR`, if either is set in the daemon's
    environment on macOS or Windows, now steer its outbound TLS instead of
    being ignored - a stale file there breaks webhooks, speedtests and the
    update check with certificate errors while the dashboard stays green.
    `GODEBUG=x509sslcertoverrideplatform=0` restores the old behavior.
  - A URL whose host carries colons that are not a port is now refused by the
    parser, so a webhook or heartbeat address written as `http://fd00::1/hook`
    fails with `invalid port "::1" after host`. Bracket the address -
    `http://[fd00::1]/hook`, which older builds take too - and it is delivered.
    `GODEBUG=urlstrictcolons=0` restores the old parsing for any other URL the
    stricter parser now refuses, but it does not deliver this one: the old
    parser read that address as the host `fd00:` and the port `1`, and looked
    up `fd00:` as a name, which is why the webhook never arrived on v0.70.1
    either. The same address with a port is the one that stops working:
    `http://fd00::1:8080/hook` was delivered by v0.70.1, whose parser took the
    last colon for the port, and is refused now. For that one both remedies
    work: `http://[fd00::1]:8080/hook`, or `GODEBUG=urlstrictcolons=0`.

  The other three never surface here: TLS offers two more post-quantum key
  exchange groups, which is between this build and the machines it talks to;
  `crypto/...` ignores a custom random source, which nothing here passes; and a
  crash traceback carries the goroutine labels a program sets, which nothing
  here sets.

### Removed

- The `speed_auto_loc` and `speed_auto_label` settings, with the "Auto
  location" picker they belonged to. They are gone from `/api/settings` and
  from its `defaults` block, and `speedtest_auto_label` is gone from
  `/api/status`. A value v0.70.1 stored stays in the database - nothing in this
  build sets it or removes it, a restored backup that carries one lands it with
  the rest of its rows, and a roll-back still finds it - and this build reads
  it once at each start for one purpose: an install that carries a city, runs
  its speedtests on Ookla, and has pinned no server and starred none gets one
  line on stdout saying that the city no longer chooses anything, and what
  does. An install whose speedtests run on iperf3 is not told, because the city
  never chose its server. Writing one is refused rather than accepted: a
  non-empty `speed_auto_loc` or `speed_auto_label` in a `POST /api/settings`
  body comes back `400` naming what decides selection now, and the rest of that
  body is refused with it, so a provisioning run that pinned its scope that way
  is told instead of being answered `200` forever - even when it sends the city
  this install already has. Sending either of them blank is still `200` - that
  asks for the state the daemon is in. The v0.70.1 dashboard sends the stored
  city back with every Save, so on an install that had one, a settings drawer
  opened before the upgrade and saved after it is refused the same way,
  together with everything else that Save sent to `/api/settings`: reload the
  page and save again. What replaced the setting is the starred-server list
  (`speed_servers`) and the pinned server (`speed_server_id`);
  `POST /api/speedtest/candidates` shows the field an automatic run would race
  right now.

### Added

- `POST /api/speedtest/candidates`, `POST /api/speedtest/ping`,
  `GET /api/speed/server-pings` and `POST /api/notify/heartbeat/test`. The
  first three serve the reworked Ookla server picker; the last is the Test
  button for the heartbeat URL.
- The settings behind that picker: `speed_servers` (the servers you starred),
  `speed_best_of_count`, `speed_challenge_every` and `speed_discard_losers`.
- `-on-corrupt refuse|rebuild` (default `refuse`): what a start does with a
  database it cannot open. Add `-on-corrupt rebuild` to a unit or compose file
  only if you want the old replace-and-carry-on behaviour, and take it out
  again before rolling back: v0.70.1 answers `flag provided but not defined:
  -on-corrupt` and does not start.
- `pingularity reset-auth` also releases the hold a store rebuilt with
  `-on-corrupt rebuild` keeps on network access, and says so when there was one.
