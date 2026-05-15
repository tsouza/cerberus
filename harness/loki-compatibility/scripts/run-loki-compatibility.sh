#!/usr/bin/env bash
# LogQL compatibility harness entry point (PR 1: scaffold smoke).
#
# Brings up the docker-compose stack (reference Loki + cerberus +
# ClickHouse), runs the deterministic seeder against both targets, and
# verifies /labels is non-empty on the reference Loki side. The diff
# driver lands in PR 3 — this script's contract for PR 1 is just
# "stack comes up, seed runs, /labels is non-empty on both endpoints".
#
# Usage:
#   ./harness/loki-compatibility/scripts/run-loki-compatibility.sh   full lifecycle
#   COMPOSE_KEEP=1 ./...                                             leave stack up after run
#
# Env:
#   COMPOSE_KEEP        non-empty: leave the compose stack running after the
#                       smoke completes (useful for poking at /loki/api/v1/*
#                       and ClickHouse manually).
#
# Mirrors harness/prometheus-compliance/scripts/run-compatibility.sh's
# lifecycle: up --wait, run go seeder, tear down on every exit path.

set -eu -o pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT_DIR"

echo "==> bringing up loki-compatibility stack (compose up --wait)"
docker compose up -d --build --wait clickhouse loki cerberus

# Consolidated cleanup: tear down the compose stack on every exit path
# (success, seeder failure, set -e abort, manual SIGINT). Without this,
# a non-zero seeder exit propagated by `set -e` would leak the stack
# across re-runs — local repros, in particular, would inherit dirty CH /
# Loki state and confuse subsequent debugging.
cleanup() {
    rc=$?
    if [ -z "${COMPOSE_KEEP:-}" ]; then
        echo "==> tearing down (set COMPOSE_KEEP=1 to leave running)"
        docker compose down -v || true
    fi
    exit "$rc"
}
trap cleanup EXIT

echo "==> running seeder (go run ./cmd/seed)"
(cd "$ROOT_DIR/../.." && go run ./harness/loki-compatibility/cmd/seed/)

echo "==> smoke OK — /labels reported non-empty on both targets"
echo "    (corpus + diff driver land in PR 2+ per docs/loki-compliance-plan.md)"
