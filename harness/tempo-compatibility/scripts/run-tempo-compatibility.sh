#!/usr/bin/env bash
# Tempo / TraceQL compatibility harness entry point.
#
# Status: PR 2 of docs/tempo-compliance-plan.md. Brings up the
# docker-compose stack (reference Tempo + cerberus + ClickHouse +
# STUB driver), invokes the stub, and tears down. The real seeder +
# diff driver land in PRs 3-4; the stub already accepts the full flag
# surface so this script's contract is stable across the rollout.
#
# Usage:
#   ./harness/tempo-compatibility/scripts/run-tempo-compatibility.sh        full lifecycle
#   COMPOSE_KEEP=1 ./...                                                    leave stack up after run
#
# Env:
#   REPORT_DIR    where the driver writes diff.json (default:
#                 harness/tempo-compatibility/reports/). The directory
#                 is created on first run.
#
# Exit codes (driver passthrough):
#   0  driver exited 0  (in PR 2: stub always exits 0)
#   non-zero  stack failed to come up OR driver exited non-zero

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

echo "==> running tempo-compat-driver (stub in PR 2)"
# `docker compose run --rm` invokes the driver and removes the
# container afterwards. The stub prints a banner and exits 0; PRs 3-4
# will replace it with the real seed → query → diff pipeline.
#
# Note: NO `|| true` here. The driver's exit code is meaningful —
# masking it would let regressions land green (this is the same trap
# that bit the PromQL harness pre-#298). The cleanup trap still tears
# down the stack on a non-zero exit.
set +e
docker compose run --rm tempo-compat-driver
DRIVER_RC=$?
set -e

echo "==> driver exited with rc=$DRIVER_RC"
echo "==> reports under $REPORT_DIR (empty until PR 4 lands the differ)"

exit "$DRIVER_RC"
