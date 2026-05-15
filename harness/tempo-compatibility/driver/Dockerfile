# syntax=docker/dockerfile:1
# Multi-stage build for the Tempo / TraceQL compatibility driver.
#
# Status: PR 2 of docs/tempo-compliance-plan.md. The driver is a stub
# (main.go uses stdlib only), so the build context is tiny — no
# vendor dir, no module-path tricks. PRs 3-4 will likely grow this
# to pull in harness/tempo-compatibility/upstream/pkg/httpclient and
# the OTLP gRPC SDK; when that happens, the build context will need
# to widen to the repo root for module-aware compilation, and this
# Dockerfile will need a corresponding rewrite.
#
# Build:
#   docker compose build tempo-compat-driver
# Run:
#   docker compose run --rm tempo-compat-driver

FROM golang:1.26 AS build
WORKDIR /src

# Stub-only build: copy the driver source and compile it as a
# standalone main package. The driver lives inside the cerberus
# module on disk (so `go build ./...` from the repo root also
# compiles it), but inside this image we build it in isolation with
# a synthetic `go.mod` — that keeps the Docker build context small
# and decoupled from cerberus's go.sum. The `-trimpath` flag is kept
# for parity with cerberus's release builds.
#
# When PRs 3-4 grow the driver beyond stdlib, this image will need
# to switch to a repo-root build context (like Dockerfile.local) so
# the real go.mod is in scope.
COPY main.go ./
RUN go mod init tempo-compat-driver \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/driver .

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="cerberus-tempo-compat-driver"
LABEL org.opencontainers.image.description="Tempo / TraceQL compatibility diff driver for cerberus (stub in PR 2; real differ lands in PRs 3-4)"
LABEL org.opencontainers.image.licenses="MIT"

COPY --from=build /out/driver /usr/local/bin/driver

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/driver"]
