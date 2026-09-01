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
FROM --platform=$BUILDPLATFORM debian:13-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258 AS setcap
RUN apt-get update \
    && apt-get install -y --no-install-recommends libcap2-bin \
    && rm -rf /var/lib/apt/lists/*
# TARGETPLATFORM (set by buildx for each target arch) selects the matching prebuilt
# binary from goreleaser's per-platform context. This stage stays pinned to
# $BUILDPLATFORM above, so apt + setcap run natively without QEMU.
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/pingularity /pingularity
# +ep: effective+permitted, so the cap is raised automatically on exec even for
# a non-root user. `mkdir /seed/pingularity` seeds the data dir the final stage
# copies; it is made 0700 here so the copy carries that mode across.
# The .pingularity-image-dir marker is a volume-lineage HEURISTIC (not a proof
# of volume type): Docker's copy-up carries it into a fresh named volume, so the
# daemon's container carve-out (store.go) can tell content that came from OUR
# image from an empty PVC or a plain bind-mounted host directory, which never
# carry it. A bind mount restored FROM a marked volume would carry it too - an
# accepted edge, since the content is genuinely ours.
RUN setcap cap_net_raw+ep /pingularity \
    && mkdir -p /seed/pingularity \
    && touch /seed/pingularity/.pingularity-image-dir \
    && chmod 0700 /seed/pingularity

# --- final image: distroless nonroot, carrying the capped binary ---
FROM gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
# Static attribution; version/revision are stamped per build by goreleaser
# (dockers_v2 labels/annotations in .goreleaser.yaml), not hardcoded here.
LABEL org.opencontainers.image.title="pingularity" \
      org.opencontainers.image.description="Single-binary internet-connectivity monitor." \
      org.opencontainers.image.source="https://github.com/pingular/pingularity" \
      org.opencontainers.image.licenses="MIT"
COPY --from=setcap /pingularity /pingularity
# The licences the binary is distributed under. Every other channel carries these
# - the archives ship them beside the binary (.goreleaser.yaml archives.files) and
# the deb/rpm install them under /usr/share/doc/pingularity - but an image is a
# binary distribution too, and the statically-linked binary carries BSD-3 code
# (golang.org/x, speedtest-go, the modernc/musl-derived libc) plus an embedded
# OFL font, all of which ask for their notice to travel with it. goreleaser stages
# only the built binaries into the docker context, so both files are named in that
# builder's extra_files; without them this COPY fails the build rather than
# silently shipping an image with no notices.
COPY LICENSE THIRD-PARTY-NOTICES.md /usr/share/doc/pingularity/
# The data dir must exist owned by nonroot (65532) so a freshly created named
# volume inherits that ownership and the unprivileged process can write to it.
# It is copied as an entry INSIDE /seed rather than being the destination itself:
# a destination directory COPY creates implicitly is 0755 and --chmod does not
# reach it, so the image shipped a group/world-readable data directory. As a
# copied entry it carries its own mode, set in the builder stage and again here.
# 0700 is the mode pingularity gives a data directory it creates itself; its
# start-up check refuses to re-permission a directory it did not create, so the
# mode has to be right as shipped or every container start warns that the
# directory is too open. This stage is distroless, so unlike Dockerfile.iperf
# there is no shell to correct the mode afterwards.
COPY --from=setcap --chown=65532:65532 --chmod=0700 /seed/ /var/lib/
EXPOSE 9000
VOLUME /var/lib/pingularity
USER nonroot
# Exec form is mandatory: distroless has no shell for the CMD-string form. The
# subcommand probes http://127.0.0.1:9000/healthz; operators who change -listen
# must override this healthcheck (or disable it), or the container reports
# unhealthy while the daemon is fine.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD ["/pingularity", "healthz"]
# Pin the database onto the volume. Without this the path is chosen from the
# effective uid, and a --user override (or this non-root default) would fall
# back to a temp dir inside the writable layer, so the mounted volume stays
# empty and updates silently discard all history plus pingularity.key. The key
# lives beside the database, so pinning -db pins both.
ENTRYPOINT ["/pingularity", "run", "-db", "/var/lib/pingularity/pingularity.db"]
