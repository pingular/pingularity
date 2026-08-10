# Minimal image around the prebuilt static binary. goreleaser (dockers_v2) stages
# the compiled binaries per platform in the build context - linux/amd64/pingularity,
# linux/arm64/pingularity - and buildx selects the target-arch one via $TARGETPLATFORM.
#
# Both FROM lines are pinned by digest (alongside the human-readable tag) so
# Dependabot's docker ecosystem can bump them; see .github/dependabot.yml.
#
# The image runs as a NON-ROOT user (distroless `nonroot`, uid 65532). Accurate
# host monitoring needs raw sockets (traceroute); a non-root process gets that
# only via a file capability, so a throwaway stage stamps CAP_NET_RAW onto the
# binary with setcap. NET_RAW is in Docker's default capability set, so:
#   docker run --network=host \
#     -v pingularity-data:/var/lib/pingularity ghcr.io/pingular/pingularity
# works with no --cap-add. To harden further, drop everything but NET_RAW:
#   docker run --network=host --cap-drop=ALL --cap-add=NET_RAW \
#     -v pingularity-data:/var/lib/pingularity ghcr.io/pingular/pingularity
#
# The setcap stage runs on the BUILD platform (native, no emulation) and only
# writes an xattr onto the target-arch binary; BuildKit preserves that xattr
# across the COPY into the final image. Needs a BuildKit builder - the release
# workflow already sets up buildx.

# --- stamp CAP_NET_RAW onto the binary (runs natively on the build host) ---
FROM --platform=$BUILDPLATFORM debian:13-slim@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd AS setcap
RUN apt-get update \
    && apt-get install -y --no-install-recommends libcap2-bin \
    && rm -rf /var/lib/apt/lists/*
# TARGETPLATFORM (set by buildx for each target arch) selects the matching prebuilt
# binary from goreleaser's per-platform context. This stage stays pinned to
# $BUILDPLATFORM above, so apt + setcap run natively without QEMU.
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/pingularity /pingularity
# +ep: effective+permitted, so the cap is raised automatically on exec even for
# a non-root user. `mkdir /data` seeds the data dir the final stage chowns.
RUN setcap cap_net_raw+ep /pingularity && mkdir -p /data

# --- final image: distroless nonroot, carrying the capped binary ---
FROM gcr.io/distroless/static-debian13:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6
COPY --from=setcap /pingularity /pingularity
# The data dir must exist owned by nonroot (65532) so a freshly created named
# volume inherits that ownership and the unprivileged process can write to it.
# --chmod=0700 matches the mode pingularity gives a data directory it creates
# itself. Without it COPY makes the destination directory 0755 (the source mode is
# not carried across), and the image shipped a group/world-readable data directory
# - which tripped pingularity's own start-up check on every container start,
# telling the operator the directory was too open and to "consider a dedicated -db
# directory" when this IS the dedicated directory and the image had made it that
# way. The check rightly refuses to re-permission a directory it did not create,
# so the mode has to be correct as shipped.
COPY --from=setcap --chown=65532:65532 --chmod=0700 /data /var/lib/pingularity
EXPOSE 9000
VOLUME /var/lib/pingularity
USER nonroot
# Pin the database onto the volume. Without this the path is chosen from the
# effective uid, and a --user override (or this non-root default) would fall
# back to a temp dir inside the writable layer, so the mounted volume stays
# empty and updates silently discard all history plus pingularity.key. The key
# lives beside the database, so pinning -db pins both.
ENTRYPOINT ["/pingularity", "run", "-db", "/var/lib/pingularity/pingularity.db"]
