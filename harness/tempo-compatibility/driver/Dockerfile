# syntax=docker/dockerfile:1
# Multi-stage build for the Tempo / TraceQL compatibility driver.
#
# Status: PR 3 of docs/tempo-compliance-plan.md. The driver grew from
# stdlib-only stub (PR 2) into a real seeder that imports
# `internal/schema/ddl` and `go.opentelemetry.io/proto/otlp/...`. To
# compile against cerberus's go.mod we must build with a repo-root
# context (same shape as Dockerfile.local for the cerberus binary).
#
# Build:
#   docker compose build tempo-compat-driver
# Run:
#   docker compose run --rm tempo-compat-driver seed
#
# Subcommands (see driver/main.go):
#   seed   PR 3 — push OTLP to Tempo + INSERT into ClickHouse +
#                 smoke-poll /api/traces on both backends.
#   diff   PR 4 — corpus + markdown report (not implemented yet).

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
    ./harness/tempo-compatibility/driver

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="cerberus-tempo-compat-driver"
LABEL org.opencontainers.image.description="Tempo / TraceQL compatibility driver (seeder in PR 3; differ lands in PR 4)"
LABEL org.opencontainers.image.licenses="MIT"

COPY --from=build /out/driver /usr/local/bin/driver

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/driver"]
