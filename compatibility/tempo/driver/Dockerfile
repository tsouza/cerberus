# syntax=docker/dockerfile:1
# Multi-stage build for the Tempo / TraceQL compatibility driver.
#
# The driver exposes `seed` + `diff` subcommands; both compile from the
# same binary so the compose image is stable. The repo-root build
# context lets the driver import `internal/schema/ddl` and the OTLP
# gRPC client types via cerberus's go.mod (same shape as
# Dockerfile.local for the cerberus binary).
#
# Build:
#   docker compose build tempo-compat-driver
# Run:
#   docker compose run --rm tempo-compat-driver seed
#   docker compose run --rm tempo-compat-driver diff
#
# Subcommands (see driver/main.go):
#   seed   PR 3 — push OTLP to Tempo + INSERT into ClickHouse +
#                 smoke-poll /api/traces on both backends.
#   diff   PR 4 — read TXTAR corpus, run TraceQL through both backends
#                 via /api/search + /api/traces, write markdown report
#                 + shields.io endpoint-badge score JSON. Report-only;
#                 parity drift does not change the exit code.

FROM golang:1.26 AS build
WORKDIR /src

# Build context is the cerberus repo root (set in docker-compose.yml's
# `context: ../..`). We copy go.mod / go.sum first so module download
# caches across iterative builds, then the rest of the tree, then
# compile only the driver package — fast and self-contained.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build \
    -trimpath -ldflags="-s -w" \
    -o /out/driver \
    ./compatibility/tempo/driver

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="cerberus-tempo-compat-driver"
LABEL org.opencontainers.image.description="Tempo / TraceQL compatibility driver (seed + diff subcommands)"
LABEL org.opencontainers.image.licenses="MIT"

COPY --from=build /out/driver /usr/local/bin/driver

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/driver"]
