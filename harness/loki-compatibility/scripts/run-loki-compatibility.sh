#!/usr/bin/env bash
# LogQL compatibility harness entry point.
#
# Lifecycle (mirrors harness/prometheus-compliance/scripts/run-compatibility.sh):
#
#   1. `docker compose up --wait` brings reference Loki + cerberus + CH up.
#   2. The Go seeder pushes a deterministic fixture to both targets and
#      asserts /labels is non-empty (the original PR 1 smoke).
#   3. `go test -tags=remote_correctness -c` builds the diff driver from
#      the vendored upstream/loki-bench/ corpus (the binary contains the
#      single TestRemoteStorageEquality test).
#   4. The driver runs against -addr-1 (reference Loki :23100) and
#      -addr-2 (cerberus :29092); its JSON-shaped go-test output is
#      captured to reports/diff.json.
#   5. The compose stack is torn down on every exit path (success,
#      driver failure, set -e abort, manual SIGINT) via a cleanup trap.
#
# Exit semantics:
#
#   - 0  → smoke + driver passed (no diffs reported on any query case).
#   - 1  → driver reported at least one diff or run-time failure (the
#          informational CI lane in PR 4 treats this as non-blocking; a
#          required-check upgrade is planned per docs/loki-compliance-plan.md
#          PR 5).
#   - 2+ → harness itself failed (compose up, seed, build, docker missing,
#          …). Inspect script output; the report file may be empty.
#
# Usage:
#   ./harness/loki-compatibility/scripts/run-loki-compatibility.sh   full lifecycle
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
#                       NOTE: the PR 3 driver emits `go test -v` text;
#                       the .json suffix is forward-looking — PR 5's
#                       cerberus-owned driver will switch this to a
#                       structured JSON report matching the Prom
#                       harness's report.json shape. Existing tooling
#                       that consumes `reports/*.json` should pin the
#                       output format expectation rather than the path.
#   DRIVER_TIMEOUT      go-test -timeout flag (default: 10m).
#   DRIVER_TOLERANCE    -tolerance flag passed through to the driver
#                       (default: 1e-5; matches upstream remote_test.go).
#   DRIVER_RANGE_TYPE   -remote-range-type flag (default: range).
#                       Set "instant" to exercise instant-query mode.

set -eu -o pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
REPO_ROOT=$(cd "$ROOT_DIR/../.." && pwd)
cd "$ROOT_DIR"

REPORT=${DRIVER_REPORT:-"$ROOT_DIR/reports/diff.json"}
TIMEOUT=${DRIVER_TIMEOUT:-10m}
TOLERANCE=${DRIVER_TOLERANCE:-1e-5}
RANGE_TYPE=${DRIVER_RANGE_TYPE:-range}

TEST_BIN=$(mktemp -t cerberus-loki-equality.XXXXXX.test)

# Consolidated cleanup: tear down the compose stack, restore the root
# go.mod / go.sum (the `-mod=mod` build temporarily promotes the
# vendored Loki-client + transitive deps to direct entries — we revert
# so the working tree stays clean), and drop the throwaway test binary.
# Runs on every exit path (success, driver failure, set -e abort, SIGINT)
# so a non-zero exit can't leak compose state or a dirty go.mod across
# re-runs.
cleanup() {
    rc=$?
    rm -f "$TEST_BIN"
    if [ -d "$REPO_ROOT/.git" ]; then
        (cd "$REPO_ROOT" && git checkout -- go.mod go.sum 2>/dev/null) || true
    fi
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
(cd "$REPO_ROOT" && go run ./harness/loki-compatibility/cmd/seed/)

if [ -n "${DRIVER_SKIP:-}" ]; then
    echo "==> DRIVER_SKIP set — finishing after smoke"
    exit 0
fi

# Build the diff driver from the vendored corpus. The root go.mod pins
# `ignore ./harness/loki-compatibility/upstream` so default toolchain
# operations skip this path; an explicit build target requires it. We
# pass -mod=mod so the test-only Loki-client + transitive deps
# (aws-sdk-go-v2, azure-sdk, openapi, …) can resolve without
# permanently polluting the root go.mod — the cleanup trap reverts
# go.mod/go.sum on every exit path.
echo "==> building diff driver (go test -tags=remote_correctness -c)"
(cd "$REPO_ROOT" && \
    GOFLAGS=-mod=mod go test \
        -tags=remote_correctness \
        -c \
        -o "$TEST_BIN" \
        ./harness/loki-compatibility/upstream/loki-bench/)

# The driver's NewQueryRegistry("./queries") loader is relative — cwd
# must contain the corpus subtree. Running from the vendor dir keeps
# the test binary unaltered (no patches against upstream).
echo "==> running diff driver (writing report to $REPORT)"
echo "    -addr-1=http://localhost:23100  (reference Loki)"
echo "    -addr-2=http://localhost:29092  (cerberus)"
echo "    -tolerance=$TOLERANCE -remote-range-type=$RANGE_TYPE -timeout=$TIMEOUT"

# Capture the driver's full stdout/stderr (one "--- PASS"/"--- FAIL"
# line per query subtest plus the standard `go test -v` framing). PR 5
# will swap this to the JSON shape the Prom harness uses; for now the
# raw stream is sufficient for the informational lane. The set +e
# window is just wide enough to capture the driver's exit code so the
# report write completes before the script propagates the failure. The
# cleanup trap still runs.
set +e
(cd "$ROOT_DIR/upstream/loki-bench" && \
    "$TEST_BIN" \
        -test.v \
        -test.timeout="$TIMEOUT" \
        -test.run=TestRemoteStorageEquality \
        -addr-1=http://localhost:23100 \
        -addr-2=http://localhost:29092 \
        -metadata-dir="$ROOT_DIR" \
        -tolerance="$TOLERANCE" \
        -remote-range-type="$RANGE_TYPE" 2>&1) | tee "$REPORT"
DRIVER_RC=${PIPESTATUS[0]}
set -e

echo "==> report written to $REPORT"
echo "==> summary:"
# go-test -v emits human-readable "--- PASS"/"--- FAIL" markers per
# subtest. The driver registers each query case as a subtest, so a
# grep against those is the cheapest summary surface; the report
# captures the full stream for downstream analysis.
PASS_COUNT=$(grep -c '^    --- PASS' "$REPORT" 2>/dev/null || echo 0)
FAIL_COUNT=$(grep -c '^    --- FAIL' "$REPORT" 2>/dev/null || echo 0)
SKIP_COUNT=$(grep -c '^    --- SKIP' "$REPORT" 2>/dev/null || echo 0)
printf '    passed=%s failed=%s skipped=%s exit_code=%s\n' \
    "$PASS_COUNT" "$FAIL_COUNT" "$SKIP_COUNT" "$DRIVER_RC"

exit "$DRIVER_RC"
