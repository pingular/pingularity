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
FROM --platform=$BUILDPLATFORM debian:12-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS setcap
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
FROM gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35
COPY --from=setcap /pingularity /pingularity
# The data dir must exist owned by nonroot (65532) so a freshly created named
# volume inherits that ownership and the unprivileged process can write to it.
COPY --from=setcap --chown=65532:65532 /data /var/lib/pingularity
EXPOSE 9000
VOLUME /var/lib/pingularity
USER nonroot
# Pin the database onto the volume. Without this the path is chosen from the
# effective uid, and a --user override (or this non-root default) would fall
# back to a temp dir inside the writable layer, so the mounted volume stays
# empty and updates silently discard all history plus pingularity.key. The key
# lives beside the database, so pinning -db pins both.
ENTRYPOINT ["/pingularity", "run", "-db", "/var/lib/pingularity/pingularity.db"]
