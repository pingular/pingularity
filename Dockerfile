# Minimal image around the prebuilt static binary.
# goreleaser's docker build passes the compiled `pingularity` into the context.
#
# Accurate host monitoring (raw-socket traceroute, real network view) needs:
#   docker run --network=host --cap-add=NET_RAW \
#     -v pingularity-data:/var/lib/pingularity ghcr.io/pingular/pingularity
FROM gcr.io/distroless/static-debian12
COPY pingularity /pingularity
EXPOSE 9000
VOLUME /var/lib/pingularity
# Pin the database onto the volume. Without this the path is chosen from the
# effective uid: root lands on the volume, but running with --user (a common
# hardening flag, mandatory under runAsNonRoot) makes the code fall back to a
# temp dir inside the writable layer, so the mounted volume stays empty and
# updates silently discard all history plus pingularity.key. The key lives
# beside the database, so pinning -db pins both.
ENTRYPOINT ["/pingularity", "run", "-db", "/var/lib/pingularity/pingularity.db"]
