# syntax=docker/dockerfile:1
# This Dockerfile is consumed by goreleaser (see .goreleaser.yml); the binary
# is built outside Docker by goreleaser's `builds` stage and dropped into the
# build context as `cerberus`. For local Docker builds without goreleaser,
# see `Dockerfile.local` (PR8) once it lands.

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="cerberus"
LABEL org.opencontainers.image.description="Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse"
LABEL org.opencontainers.image.url="https://github.com/tsouza/cerberus"
LABEL org.opencontainers.image.source="https://github.com/tsouza/cerberus"
LABEL org.opencontainers.image.licenses="MIT"

COPY cerberus /usr/local/bin/cerberus

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/cerberus"]
