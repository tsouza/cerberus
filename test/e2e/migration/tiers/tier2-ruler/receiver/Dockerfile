# syntax=docker/dockerfile:1
# Builds the Tier-2 migration substrate's dead-end webhook receiver
# (test/e2e/migration/tiers/tier2-ruler/receiver/main.go). The binary imports
# only the standard library, so building its one package pulls in none of the
# module's external dependencies.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY test/e2e/migration/tiers/tier2-ruler/receiver ./test/e2e/migration/tiers/tier2-ruler/receiver
RUN CGO_ENABLED=0 go build -o /dead-end-receiver ./test/e2e/migration/tiers/tier2-ruler/receiver

FROM gcr.io/distroless/static-debian12
COPY --from=build /dead-end-receiver /dead-end-receiver
ENTRYPOINT ["/dead-end-receiver"]
