#!/usr/bin/env bash
# Tempo / TraceQL compatibility harness entry point.
#
# Status: PR 4 of docs/tempo-compliance-plan.md. The driver runs in two
# phases sequentially:
#
#   1. seed — push a deterministic OTLP batch to Tempo's :4317 AND
#             insert the same fixture into ClickHouse so cerberus reads
#             it via /api/traces. In-process smoke confirms both
#             backends resolve the first trace ID with non-zero spans.
#   2. diff — read the TXTAR corpus, run every TraceQL query through
#             both backends, write a markdown diff report to
#             $REPORT_DIR/diff.md. Default: informational (script
#             returns 0 even if the corpus diffs); set FAIL_ON_DIFF=1
#             to bubble per-case regressions up to a non-zero rc.
#
# Usage:
#   ./harness/tempo-compatibility/scripts/run-tempo-compatibility.sh   full lifecycle (seed → diff)
#   COMPOSE_KEEP=1 ./...                                               leave stack up after run
#   FAIL_ON_DIFF=1 ./...                                               exit non-zero on diffs
#
# Env:
#   REPORT_DIR     where the driver writes diff.md (default:
#                  harness/tempo-compatibility/reports/).
#   COMPOSE_KEEP   non-empty: leave the compose stack running after the
#                  differ completes (useful for poking at /api/traces
#                  and the otel_traces table manually).
#   FAIL_ON_DIFF   non-empty: forward --fail-on-diff to the differ so
#                  the script exits non-zero on any case that reported
#                  a structural diff / assertion / hard error.
#
# Exit codes:
#   0  seed + diff completed (informational mode) OR with FAIL_ON_DIFF
#      set, every corpus case passed.
#   non-zero  stack failed to come up OR seeder failed OR (with
#             FAIL_ON_DIFF set) the differ reported a regression.

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
# Step 1: start tempo (no compose-level healthcheck — distroless image,
# see compose.yml). The driver polls /ready before pushing.
docker compose up -d --build tempo

# Step 2: `--wait` block on the healthchecked services. 5min compose-
# level timeout is generous: CH boot can take 30-60s on a cold runner,
# cerberus is <2s. If we time out, it's an infra-layer issue (image
# pull, cgroup, etc.), not a harness bug.
docker compose up -d --build --wait --wait-timeout 300 \
    clickhouse cerberus-tempo

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
SEED_RC=$?
set -e

echo "==> seeder exited with rc=$SEED_RC"
if [ "$SEED_RC" -ne 0 ]; then
    echo "==> seeder failed — skipping diff"
    exit "$SEED_RC"
fi

# PR 4: after seed, run the differ against the same backends. The
# differ emits a markdown report under $REPORT_DIR; in informational
# mode (default) it returns 0 even if the report contains diffs so
# the workflow stays a baseline. Set FAIL_ON_DIFF=1 to bubble
# regressions up to a non-zero rc — useful for `just`-style local
# repro and for the eventual PR 7 promotion to a required check.
DIFF_FLAGS=""
if [ -n "${FAIL_ON_DIFF:-}" ]; then
    DIFF_FLAGS="--fail-on-diff"
fi

echo "==> running tempo-compat-driver diff"
set +e
docker compose run --rm tempo-compat-driver diff $DIFF_FLAGS
DIFF_RC=$?
set -e

echo "==> differ exited with rc=$DIFF_RC"
echo "==> report at $REPORT_DIR/diff.md"

exit "$DIFF_RC"
