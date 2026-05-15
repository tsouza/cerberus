#!/usr/bin/env bash
# Tempo / TraceQL compatibility harness entry point.
#
# Status: PR 3 of docs/tempo-compliance-plan.md. The driver is now a
# real seeder (not a stub) — it writes a deterministic OTLP batch into
# Tempo's :4317 AND inserts the same fixture into ClickHouse so cerberus
# reads it via the same /api/traces path. The driver's own smoke
# assertion (poll /api/traces/<id> on both backends and assert non-zero
# matching span counts) gates this script's exit code.
#
# Usage:
#   ./harness/tempo-compatibility/scripts/run-tempo-compatibility.sh   full lifecycle
#   COMPOSE_KEEP=1 ./...                                               leave stack up after run
#
# Env:
#   REPORT_DIR    where the driver writes diff.json (default:
#                 harness/tempo-compatibility/reports/). PR 3's seeder
#                 doesn't write a report — that lands in PR 4.
#   COMPOSE_KEEP  non-empty: leave the compose stack running after the
#                 seeder completes (useful for poking at /api/traces and
#                 the otel_traces table manually).
#
# Exit codes (driver passthrough):
#   0  seeder + smoke succeeded (both backends returned spans for the
#      first trace ID with matching counts)
#   non-zero  stack failed to come up OR seeder/smoke failed.

set -eu -o pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT_DIR"

REPORT_DIR=${REPORT_DIR:-"$ROOT_DIR/reports"}
mkdir -p "$REPORT_DIR"

# Consolidated cleanup: tear down the compose stack on every exit path
# (success, driver failure, set -e abort, SIGINT). Without this, a
# non-zero driver exit propagated by `set -e` would leak the stack
# across re-runs — local repros, in particular, would inherit dirty
# Tempo block storage and confuse subsequent debugging. Same pattern
# as harness/prometheus-compliance/scripts/run-compatibility.sh.
cleanup() {
    rc=$?
    if [ -z "${COMPOSE_KEEP:-}" ]; then
        echo "==> tearing down (set COMPOSE_KEEP=1 to leave running)"
        docker compose down -v || true
    fi
    exit "$rc"
}
trap cleanup EXIT

echo "==> bringing up tempo-compatibility stack"
# --wait blocks until each service's healthcheck reports `healthy`.
# 5min compose-level timeout (`--wait-timeout 300`) is generous: the
# CH boot can take 30-60s on a cold runner, Tempo single-binary boots
# in <10s, cerberus is <2s. If we time out, the issue is at the
# infrastructure layer (image pull, cgroup, etc.), not the harness.
docker compose up -d --build --wait --wait-timeout 300 \
    clickhouse tempo cerberus-tempo

echo "==> running tempo-compat-driver seed"
# `docker compose run --rm` invokes the seeder and removes the
# container afterwards. The seeder pushes OTLP into Tempo, INSERTs the
# same fixture into ClickHouse, and polls /api/traces/<id> on both
# backends as the in-process smoke check.
#
# Note: NO `|| true` here. The driver's exit code is meaningful — masking
# it would let regressions land green (the same trap that bit the PromQL
# harness pre-#298). The cleanup trap still tears down the stack on a
# non-zero exit.
set +e
docker compose run --rm tempo-compat-driver seed
DRIVER_RC=$?
set -e

echo "==> driver exited with rc=$DRIVER_RC"
echo "==> reports under $REPORT_DIR (empty until PR 4 lands the differ)"

exit "$DRIVER_RC"
