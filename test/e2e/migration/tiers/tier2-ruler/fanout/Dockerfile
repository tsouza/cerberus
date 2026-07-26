# syntax=docker/dockerfile:1
# Builds the Tier-2 migration substrate's query/write fanout
# (test/e2e/migration/tiers/tier2-ruler/fanout/main.go). The binary imports
# only the standard library, so building its one package pulls in none of the
# module's external dependencies.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY test/e2e/migration/tiers/tier2-ruler/fanout ./test/e2e/migration/tiers/tier2-ruler/fanout
RUN CGO_ENABLED=0 go build -o /query-write-fanout ./test/e2e/migration/tiers/tier2-ruler/fanout

FROM gcr.io/distroless/static-debian12
COPY --from=build /query-write-fanout /query-write-fanout
ENTRYPOINT ["/query-write-fanout"]
