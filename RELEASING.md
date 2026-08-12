# Releasing Pingularity

Maintainer runbook for cutting a public release. Cutting a release and
*announcing* it are deliberately two separate steps: pushing a tag publishes
artifacts, while the in-app update badge only lights up when you bump the
`latest.json` feed by hand. That split lets you ship, smoke-test, and only then
tell running installs a new version exists.

## How a release happens

A release is driven entirely by a version tag. GoReleaser (invoked from GitHub
Actions on the tag) does the rest.

```bash
git tag v1.2.3          # semver, leading v
git push origin v1.2.3
```

The `-X main.version=` ldflag is set from the tag, so `pingularity version`
reports `1.2.3`. Use a valid SemVer tag with a leading `v`
(`vMAJOR.MINOR.PATCH`, optionally `-prerelease`). Do **not** append `+build`
metadata: SemVer allows it, but the workflow rejects it, because the hyphen in
a value like `+build-1` makes the stable-vs-prerelease shell test misfire (a
stable tag would skip the immutable-release guard) and Docker tags cannot carry
`+` at all. The release workflow guards the tag with a SemVer regex and refuses
the obviously malformed shapes (bare `v`, `v1.2`, leading-zero identifiers, and
any `+build` metadata) before it does anything - but that
regex is *close to*, not exactly, SemVer, so a pathological tag can still slip
past it and is then rejected by GoReleaser itself. Either way, a tag that is not
valid SemVer would never register as "newer" with running installs, because the
update feed compares SemVer.

On that tag push, the release workflow runs GoReleaser, which builds the full
matrix (linux/windows/darwin × amd64/arm64, `CGO_ENABLED=0`) and publishes:

- **GitHub Releases** - the `.tar.gz` archives (`.zip` on Windows), the `.deb`
  and `.rpm` packages, and a `checksums.txt` covering all of them. Each artifact
  gets a signed build-provenance attestation (keyless, via the workflow's OIDC
  token), so a download can be verified with
  `gh attestation verify <file> --repo pingular/pingularity`.
- **GHCR** - the multi-arch image `ghcr.io/pingular/pingularity` (amd64 + arm64
  under one tag), tagged with the version and `latest`.
- **Homebrew tap** - a cask commit to `pingular/homebrew-tap` so
  `brew install pingular/tap/pingularity` resolves to the new version. (It is a
  cask, not a formula - casks are macOS-only; Linux users take the deb/rpm
  packages or the container image.)
- **winget** - GoReleaser pushes a manifest branch
  (`pingularity-<version>`) to the `pingular/winget-pkgs` fork. It does NOT
  open the upstream PR: the maintainer opens (and later merges) the PR from
  that branch against `microsoft/winget-pkgs` by hand. That community listing
  is a parallel, best-effort track: what users actually install from is the
  SELF-HOSTED winget source at `winget.pingularity.dev` (the `dl/` Worker in
  the private pingularity.dev repo), which serves the same fork-branch data
  and is updated at announce time - see "Update notifications" below.

**Prerelease tags** (`v1.2.3-rc1`, `-beta`) exercise the pipeline without
touching users: the GitHub release is marked pre-release, the brew cask and
winget manifest are skipped (`skip_upload: auto`), and the GHCR `latest` tag is
not moved - only the version-tagged image is pushed. Re-pushing the same rc tag
(the re-cut-on-amend loop) republishes into the existing GitHub release;
`replace_existing_artifacts: true` makes that overwrite the old assets instead
of failing with `422 already_exists`. That overwrite is confined to prerelease
tags: a **stable** release is immutable, so the workflow refuses to re-cut a
stable tag whose release already exists (its artifacts are provenance-attested,
and overwriting them would break that). To re-issue a stable build, bump the
version.

None of this touches `latest.json` or `internal/update` - the running fleet is
not notified yet (see [Update notifications](#update-notifications)).

## Prerequisites (secrets & repos)

Configure these once on the `pingular/pingularity` repo. A dry run against the
private repo (`goreleaser release --snapshot --clean`, which builds everything
locally without publishing) validates the config without needing any of them.

| Requirement | What it is | Notes |
| --- | --- | --- |
| `GITHUB_TOKEN` | Auto-provided by Actions | Uploads the release assets and pushes the GHCR image. Needs `contents: write` and `packages: write` on the workflow. |
| `TAP_GITHUB_TOKEN` (PAT) | One PAT with write access to **both** `pingular/homebrew-tap` and `pingular/winget-pkgs` | The single cross-repo secret the pipeline reads (`release.yml` passes it; `.goreleaser.yaml` uses it for the brew cask commit and the winget manifest branch). Store as a repo secret under exactly this name. |
| GHCR package | The `ghcr.io/pingular/pingularity` package | Created on first push by `GITHUB_TOKEN`; set its visibility to **public** when you go public. |

`TAP_GITHUB_TOKEN` needs `repo`/`contents` scope on those two repos and
nothing more. Keep it as a repo (or org) Actions secret, never in the
workflow file.

## Update notifications

The in-app badge polls `https://update.pingularity.dev/latest.json` (see
`internal/update/update.go`). It is **notify-only** and completely decoupled
from the GoReleaser run - a release does **not** bump it.

The feed is served by the `dl/` Cloudflare Worker in the private
`pingular/pingularity.dev` repo, which also records privacy-preserving visitor
analytics (unique installs per day, by country - see that repo's README). To
announce a release, add the release's winget entry, bump `LATEST_VERSION` in
its `wrangler.toml`, and `wrangler deploy` (all from that repo's `dl/`; full
runbook in its README):

```bash
node update-winget-versions.mjs 1.2.3   # paste output into winget-versions.mjs
# edit wrangler.toml: LATEST_VERSION = "1.2.3"
node ../test/winget.test.mjs
wrangler deploy
```

The same deploy announces to BOTH channels: the update badge (latest.json) and
the self-hosted winget source only ever advertise versions `<= LATEST_VERSION`,
so ship-quietly and rollback-by-revert cover winget too.

Installs pick it up within their daily poll and show the update badge; the
button links to `install.pingularity.dev` - an owned indirection (served by
the same Worker as the feed) that serves the install page today, with the
GitHub Releases 302 only as a no-page fallback; repointable without stranding
the link baked into old binaries. Because publishing and announcing are
separate:

- **Ship quietly:** tag and release, verify the artifacts, and only then bump
  `latest.json`.
- **Roll back a bad release:** set `latest.json` back to the previous good
  version. That silences the badge immediately - no install auto-updated, so
  nothing has to be un-done on the client side. (Optionally mark the GitHub
  release as a pre-release or delete it so fresh downloads don't grab it.)

`latest.json` must stay **public**, or clients can't reach it. It is already
live and public via the Worker (verified serving 200 with a 5-minute edge
cache); the Worker project is the single place it is maintained.
