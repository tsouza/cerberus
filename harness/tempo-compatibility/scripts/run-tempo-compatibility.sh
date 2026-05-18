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
# The seeder and differ run as Go binaries on the CI runner (host),
# connecting to Docker-published ports on localhost. This avoids Docker
# DNS resolution failures ("lookup tempo on 127.0.0.11:53: server
# misbehaving") that occur when the driver runs inside Docker on some
# CI runner configurations. Matches the pattern used by the sibling
# Loki harness (harness/loki-compatibility/).
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
REPO_ROOT=$(cd "$ROOT_DIR/../.." && pwd)
cd "$ROOT_DIR"

REPORT_DIR=${REPORT_DIR:-"$ROOT_DIR/reports"}
mkdir -p "$REPORT_DIR"
chmod a+w "$REPORT_DIR"

DRIVER_BIN=$(mktemp -t cerberus-tempo-compat-driver.XXXXXX)

# Consolidated cleanup: tear down the compose stack and drop the
# throwaway driver binary on every exit path (success, driver failure,
# set -e abort, SIGINT) so a non-zero exit can't leak compose state
# across re-runs. Same pattern as harness/loki-compatibility/scripts/
# run-loki-compatibility.sh.
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

# The seeder and differ run as Go binaries on the host, connecting
# to Docker-published ports on localhost. Port mapping from
# docker-compose.yml:
#   Tempo HTTP:    23200:3200   → localhost:23200
#   Tempo OTLP:    24317:4317   → localhost:24317
#   cerberus:      29092:29092  → localhost:29092
#   ClickHouse:    29100:9000   → localhost:29100
#
# The Go flag defaults match these host-accessible endpoints; the
# docker-compose.yml env vars set Docker-internal hostnames for
# optional in-Docker debugging (`docker compose run --rm
# tempo-compat-driver`).

echo "==> running seeder (go run ./harness/tempo-compatibility/driver/ seed)"
# Note: NO `|| true` here. The driver's exit code is meaningful — masking
# it would let regressions land green (the same trap that bit the PromQL
# harness pre-#298). The cleanup trap still tears down the stack on a
# non-zero exit.
set +e
(cd "$REPO_ROOT" && go run ./harness/tempo-compatibility/driver/ seed)
SEED_RC=$?
set -e

echo "==> seeder exited with rc=$SEED_RC"
if [ "$SEED_RC" -ne 0 ]; then
    echo "==> seeder failed — skipping diff"
    exit "$SEED_RC"
fi

# Build the diff driver. The driver imports internal/schema/ddl and
# OTLP gRPC client types via cerberus's go.mod, so it compiles from
# the repo root like the cerberus binary.
echo "==> building diff driver"
(cd "$REPO_ROOT" && go build -o "$DRIVER_BIN" ./harness/tempo-compatibility/driver/)

# After seed, run the differ against the same backends. The differ
# emits a markdown report under $REPORT_DIR; in informational mode
# (default) it returns 0 even if the report contains diffs so the
# workflow stays a baseline. Set FAIL_ON_DIFF=1 to bubble regressions
# up to a non-zero rc — useful for `just`-style local repro and for
# the eventual PR 7 promotion to a required check.
DIFF_FLAGS=""
if [ -n "${FAIL_ON_DIFF:-}" ]; then
    DIFF_FLAGS="--fail-on-diff"
fi
DIFF_FLAGS="$DIFF_FLAGS --expected-failures $ROOT_DIR/expected-failures.json"

echo "==> running diff driver (writing report to $REPORT_DIR/diff.md)"
echo "    --tempo-http=http://localhost:23200  (reference Tempo)"
echo "    --cerberus=http://localhost:29092  (cerberus)"
set +e
"$DRIVER_BIN" diff \
    --tempo-http=http://localhost:23200 \
    --cerberus=http://localhost:29092 \
    --corpus="$ROOT_DIR/driver/corpus/smoke.txtar" \
    --report="$REPORT_DIR/diff.md" \
    $DIFF_FLAGS
DIFF_RC=$?
set -e

echo "==> differ exited with rc=$DIFF_RC"
echo "==> report at $REPORT_DIR/diff.md"

exit "$DIFF_RC"
