#!/usr/bin/env bash
# Tempo / TraceQL compatibility harness entry point.
#
# The driver runs in two phases sequentially:
#
#   1. seed — push a deterministic OTLP batch to Tempo's :4317 AND
#             insert the same fixture into ClickHouse so cerberus reads
#             it via /api/traces. In-process smoke confirms both
#             backends resolve the first trace ID with non-zero spans.
#   2. diff — read the TXTAR corpus, run every TraceQL query through
#             both backends, write a markdown diff report to
#             $REPORT_DIR/diff.md AND a shields.io endpoint-badge score
#             JSON to $REPORT_DIR/compat-score.json. Report-only: the
#             differ exits 0 even when parity diffs are present; only
#             driver-wide hard errors (compose up failure, seed failure,
#             corpus load failure) escalate to a non-zero rc.
#
# The seeder and differ run as Go binaries on the CI runner (host),
# connecting to Docker-published ports on localhost. This avoids Docker
# DNS resolution failures ("lookup tempo on 127.0.0.11:53: server
# misbehaving") that occur when the driver runs inside Docker on some
# CI runner configurations. Matches the pattern used by the sibling
# Loki harness (compatibility/loki/).
#
# Usage:
#   ./compatibility/tempo/scripts/run-tempo-compatibility.sh   full lifecycle (seed → diff)
#   COMPOSE_KEEP=1 ./...                                               leave stack up after run
#
# Env:
#   REPORT_DIR     where the driver writes diff.md and compat-score.json
#                  (default: compatibility/tempo/reports/).
#   COMPOSE_KEEP   non-empty: leave the compose stack running after the
#                  differ completes (useful for poking at /api/traces
#                  and the otel_traces table manually).
#
# Exit codes:
#   0         seed + diff completed; parity drift is captured in
#             compat-score.json, not in the exit code.
#   non-zero  stack failed to come up OR seeder failed OR differ hit
#             a hard error (corpus load, report write, etc.). Parity
#             drift never reaches this branch.

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
# across re-runs. Same pattern as compatibility/loki/scripts/
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

echo "==> running seeder (go run ./compatibility/tempo/driver/ seed)"
# Note: NO `|| true` here. The driver's exit code is meaningful — masking
# it would let regressions land green (the same trap that bit the PromQL
# harness pre-#298). The cleanup trap still tears down the stack on a
# non-zero exit.
set +e
(cd "$REPO_ROOT" && go run ./compatibility/tempo/driver/ seed)
SEED_RC=$?
set -e

echo "==> seeder exited with rc=$SEED_RC"
if [ "$SEED_RC" -ne 0 ]; then
    echo "==> seeder failed — skipping diff"
    exit "$SEED_RC"
fi

# Tempo v3's /api/search endpoint only queries completed blocks, not the
# live store. Even with complete_block_timeout: 5s, the block builder needs
# time to flush, complete, and index each block before /api/search returns
# results. On CI runners this can take 10-30s after the seeder finishes.
# The 30s sleep is a conservative upper bound; if Tempo has already
# indexed by then, the differ proceeds immediately.
echo "==> waiting 30s for Tempo to flush and index blocks..."
sleep 30

# Build the diff driver. The driver imports internal/schema/ddl and
# OTLP gRPC client types via cerberus's go.mod, so it compiles from
# the repo root like the cerberus binary.
echo "==> building diff driver"
(cd "$REPO_ROOT" && go build -o "$DRIVER_BIN" ./compatibility/tempo/driver/)

# After seed, run the differ against the same backends. Report-only:
# parity drift is captured in compat-score.json (shields.io endpoint
# badge JSON) and the markdown diff report; the driver returns 0 even
# when cases diverge. Only driver-wide hard errors (corpus load failure,
# report write failure) escalate to a non-zero rc.

echo "==> running diff driver (writing report to $REPORT_DIR/diff.md, score to $REPORT_DIR/compat-score.json)"
echo "    --tempo-http=http://localhost:23200  (reference Tempo)"
echo "    --cerberus=http://localhost:29092  (cerberus)"
set +e
"$DRIVER_BIN" diff \
    --tempo-http=http://localhost:23200 \
    --cerberus=http://localhost:29092 \
    --corpus="$ROOT_DIR/driver/corpus/smoke.txtar" \
    --report="$REPORT_DIR/diff.md" \
    --score="$REPORT_DIR/compat-score.json"
DIFF_RC=$?
set -e

# Rejection-parity pass: every deliberate 422 in internal/traceql must
# also be rejected by reference Tempo (status-class comparison, never
# message text). The corpus is the rejection catalogue itself — see
# test/rejection-parity/ and docs/compatibility.md. Report-only:
# wrong_rejection verdicts land in the JSON report; only driver-level
# infrastructure failures propagate (set -e).
echo "==> running rejection-parity driver (traceql)"
(cd "$REPO_ROOT" && go run ./compatibility/cmd/rejection-parity \
    -head traceql \
    -catalogue test/rejection-parity/catalogue.json \
    -ref http://localhost:23200 \
    -cerberus http://localhost:29092 \
    -report "$REPORT_DIR/rejection-parity.json")
echo "==> rejection-parity report written to $REPORT_DIR/rejection-parity.json"

echo "==> differ exited with rc=$DIFF_RC"
echo "==> report at $REPORT_DIR/diff.md"
echo "==> score at $REPORT_DIR/compat-score.json"

exit "$DIFF_RC"
