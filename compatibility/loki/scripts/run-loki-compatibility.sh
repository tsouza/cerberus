#!/usr/bin/env bash
# LogQL compatibility harness entry point.
#
# Lifecycle (mirrors compatibility/prometheus/scripts/run-compatibility.sh):
#
#   1. `docker compose up --wait` brings reference Loki + cerberus + CH up.
#   2. The Go seeder pushes a deterministic fixture to both targets and
#      asserts /labels is non-empty (the original PR 1 smoke).
#   3. Builds the cerberus-owned diff driver (cmd/loki-compliance-tester);
#      the binary imports the vendored upstream/loki-bench/ corpus loader,
#      so a `-mod=mod` build is required.
#   4. The driver runs against -addr-1 (reference Loki :23100) and
#      -addr-2 (cerberus :29092); it emits a structured JSON report
#      matching the prometheus-compliance harness's shape into
#      reports/diff.json.
#   5. The compose stack is torn down on every exit path (success,
#      driver failure, set -e abort, manual SIGINT) via a cleanup trap.
#
# PR 5 of docs/loki-compliance-plan.md swapped the upstream `go test -c`
# driver for the cerberus-owned `cmd/loki-compliance-tester` so the
# report shape matches the Prom harness; the build / teardown lifecycle
# is otherwise unchanged.
#
# Exit semantics:
#
#   - 0  → smoke + driver passed (no diffs reported on any query case).
#   - 1  → driver reported at least one diff or run-time failure (the
#          informational CI lane treats this as non-blocking; a
#          required-check upgrade is planned per docs/loki-compliance-plan.md
#          PR 6).
#   - 2+ → harness itself failed (compose up, seed, build, docker missing,
#          …). Inspect script output; the report file may be empty.
#
# Usage:
#   ./compatibility/loki/scripts/run-loki-compatibility.sh   full lifecycle
#   COMPOSE_KEEP=1 ./...                                             leave stack up after run
#   DRIVER_SKIP=1 ./...                                              run smoke only (skip diff)
#
# Env:
#   COMPOSE_KEEP        non-empty: leave the compose stack running after
#                       the run completes (useful for poking at
#                       /loki/api/v1/* and ClickHouse manually).
#   DRIVER_SKIP         non-empty: skip the diff driver entirely. Useful
#                       when the seeder is the bisect target.
#   DRIVER_REPORT       report file path (default: reports/diff.json).
#                       The driver emits a JSON envelope matching the
#                       Prom harness's report shape; existing tooling
#                       can consume both via the same schema.
#   DRIVER_TIMEOUT      Per-request HTTP timeout (default: 30s).
#   DRIVER_TOLERANCE    -tolerance flag (default: 1e-5; matches upstream).
#   DRIVER_RANGE_TYPE   -range-type flag (default: range; 'instant' also valid).
#   DRIVER_PARALLELISM  -parallelism flag (default: 8).
#   DRIVER_OVERLAY      Path to cerberus-test-queries.yml overlay; default
#                       resolves the in-tree file beside this script.

set -eu -o pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
REPO_ROOT=$(cd "$ROOT_DIR/../.." && pwd)
cd "$ROOT_DIR"

REPORT=${DRIVER_REPORT:-"$ROOT_DIR/reports/diff.json"}
TIMEOUT=${DRIVER_TIMEOUT:-30s}
TOLERANCE=${DRIVER_TOLERANCE:-1e-5}
RANGE_TYPE=${DRIVER_RANGE_TYPE:-range}
PARALLELISM=${DRIVER_PARALLELISM:-8}
OVERLAY=${DRIVER_OVERLAY:-"$ROOT_DIR/cerberus-test-queries.yml"}

DRIVER_BIN=$(mktemp -t cerberus-loki-tester.XXXXXX)

# Consolidated cleanup: tear down the compose stack and drop the
# throwaway driver binary on every exit path (success, driver failure,
# set -e abort, SIGINT) so a non-zero exit can't leak compose state
# across re-runs.
#
# Unlike the PR 3 `go test -c` approach, the cerberus-owned driver's
# import surface (bench-package corpus loader + cerberus's existing
# Loki / yaml deps) resolves through the root go.mod without the
# `-mod=mod` promotion, so no go.mod / go.sum revert is needed.
cleanup() {
    rc=$?
    rm -f "$DRIVER_BIN"
    if [ -z "${COMPOSE_KEEP:-}" ]; then
        echo "==> tearing down (set COMPOSE_KEEP=1 to leave running)"
        docker compose down -v || true
    fi
    exit "$rc"
}
trap cleanup EXIT

mkdir -p "$(dirname "$REPORT")"

echo "==> bringing up loki-compatibility stack (compose up --wait)"
docker compose up -d --build --wait clickhouse loki cerberus

echo "==> running seeder (go run ./cmd/seed)"
(cd "$REPO_ROOT" && go run ./compatibility/loki/cmd/seed/)

if [ -n "${DRIVER_SKIP:-}" ]; then
    echo "==> DRIVER_SKIP set — finishing after smoke"
    exit 0
fi

# Build the cerberus-owned diff driver. The driver imports the
# vendored bench package for corpus loading + cerberus's existing
# Loki / yaml deps for the HTTP + decode path. The root go.mod marks
# `ignore ./compatibility/loki/upstream`, which keeps
# `go build ./...` from walking the bench tree as a build target;
# importing the package by path is still permitted because every
# transitive dep is already a direct entry in go.mod.
echo "==> building diff driver (cmd/loki-compliance-tester)"
(cd "$REPO_ROOT" && \
    go build \
        -o "$DRIVER_BIN" \
        ./compatibility/loki/cmd/loki-compliance-tester/)

echo "==> running diff driver (writing report to $REPORT)"
echo "    -addr-1=http://localhost:23100  (reference Loki)"
echo "    -addr-2=http://localhost:29092  (cerberus)"
echo "    -tolerance=$TOLERANCE -range-type=$RANGE_TYPE -timeout=$TIMEOUT -parallelism=$PARALLELISM"

# The driver writes structured JSON to `-report` and a single-line
# summary to stderr. We capture only the summary on console (via tee
# /dev/null to keep the trap-friendly exit-code propagation); the JSON
# report lives entirely at the report path. The set +e window is just
# wide enough to capture the exit code before the script propagates
# any failure.
set +e
"$DRIVER_BIN" \
    -addr-1=http://localhost:23100 \
    -addr-2=http://localhost:29092 \
    -corpus="$ROOT_DIR/upstream/loki-bench/queries" \
    -metadata-dir="$ROOT_DIR" \
    -overlay="$OVERLAY" \
    -report="$REPORT" \
    -tolerance="$TOLERANCE" \
    -range-type="$RANGE_TYPE" \
    -parallelism="$PARALLELISM" \
    -timeout="$TIMEOUT"
DRIVER_RC=$?
set -e

echo "==> report written to $REPORT"
echo "==> summary:"
if command -v jq >/dev/null 2>&1; then
    jq '{total: .totalResults, passed: ([.results[]? | select((.unexpectedFailure // "") == "" and (.diff // "") == "" and (.unexpectedSuccess // false) == false)] | length), diffs: ([.results[]? | select((.diff // "") != "")] | length), unexpected_failures: ([.results[]? | select((.unexpectedFailure // "") != "")] | length), unsupported: ([.results[]? | select((.unsupported // false) == true)] | length), skipped: ([.results[]? | select((.skipReason // "") != "")] | length)}' "$REPORT" 2>/dev/null || echo "    (jq failed to parse $REPORT)"
else
    echo "    (install jq for per-bucket summary)"
fi

exit "$DRIVER_RC"
