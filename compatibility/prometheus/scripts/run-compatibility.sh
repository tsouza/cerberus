#!/usr/bin/env bash
# Compatibility harness entry point.
#
# Brings up the docker-compose stack (reference Prometheus + cerberus +
# ClickHouse + seeder), builds the upstream promql-compliance-tester
# binary, runs it pointed at the two endpoints, and writes the JSON
# report to compatibility/prometheus/report.json.
#
# Tests are RUN_ONCE here; reconciliation against expected-failures.json
# is done by a follow-up Go script (lands in M1.x — for the seed we just
# capture the raw report and let the user inspect).
#
# Usage:
#   ./compatibility/prometheus/scripts/run-compatibility.sh        full lifecycle
#   COMPOSE_KEEP=1 ./...                                 leave stack up after run
#
# Env:
#   TESTER_OUTPUT     report file path (default: compatibility/prometheus/report.json)
#   TESTER_QUERIES    queries yaml (default: compatibility/prometheus/cerberus-test-queries.yml,
#                     a curated copy of upstream/promql/promql-test-queries.yml
#                     with corpus-incompatible should_fail entries removed —
#                     see the file header for the policy)
#   TESTER_END_TIME   compatibility end timestamp (default: 2026-05-11T01:00:00Z)
#   TESTER_RANGE      range in seconds (default: 3600 = 1h, matches seed window)
#
# Upstream tester invocation (post-PR #298 / #300 audit):
#   The upstream `promql-compliance-tester` only accepts `-config-file`
#   (repeatable), `-output-format`, `-output-html-template`,
#   `-output-passing`, `-query-parallelism`. Older legacy flags
#   `-query-file`, `-end`, `-range` were silently rejected with
#   "flag provided but not defined" — the script previously masked
#   that with `|| true`, so the suite produced a zero-byte report and
#   the workflow reported success despite never running a single query.
#   The flags now flow through repeated `-config-file` args; time
#   parameters are injected via a generated overlay yaml so this stays
#   driven from the workflow env.

set -eu -o pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT_DIR"

OUTPUT=${TESTER_OUTPUT:-"$ROOT_DIR/report.json"}
QUERIES=${TESTER_QUERIES:-"$ROOT_DIR/cerberus-test-queries.yml"}
END_TIME=${TESTER_END_TIME:-"2026-05-11T01:00:00Z"}
RANGE=${TESTER_RANGE:-3600}

echo "==> bringing up compatibility stack"
docker compose up -d --build --wait clickhouse prometheus cerberus
echo "==> running seeder (go run ./cmd/seed)"
(cd "$ROOT_DIR/../.." && go run ./compatibility/prometheus/cmd/seed/)

echo "==> building promql-compliance-tester"
TESTER_DIR="$ROOT_DIR/upstream/promql/cmd/promql-compliance-tester"
TESTER_BIN="$ROOT_DIR/upstream/promql/cmd/promql-compliance-tester/promql-compliance-tester"
(cd "$TESTER_DIR" && go build -o promql-compliance-tester .)

# Materialise the time-window overlay. The upstream tester reads
# `query_time_parameters.end_time` + `range_in_seconds` from one of
# its `-config-file` inputs (it concatenates all of them before YAML
# parsing). Keeping the overlay ephemeral keeps test-cerberus.yml
# clean — that file declares stable wiring (URLs + tweaks); the
# overlay carries CI-volatile values (END_TIME / RANGE) which are
# expected to differ per local invocation and per CI dispatch.
OVERLAY=$(mktemp -t cerberus-compat-overlay.XXXXXX.yml)

# Consolidated cleanup: remove the overlay AND tear down the compose
# stack on every exit path (success, tester failure, set -e abort,
# manual SIGINT). Without this, a non-zero tester exit propagated by
# `set -e` would leak the stack across re-runs — local repros, in
# particular, would inherit dirty CH state and confuse subsequent
# debugging.
cleanup() {
    rc=$?
    rm -f "$OVERLAY"
    if [ -z "${COMPOSE_KEEP:-}" ]; then
        echo "==> tearing down (set COMPOSE_KEEP=1 to leave running)"
        docker compose down -v || true
    fi
    exit "$rc"
}
trap cleanup EXIT

cat > "$OVERLAY" <<EOF
query_time_parameters:
  end_time: '${END_TIME}'
  range_in_seconds: ${RANGE}
EOF

echo "==> running tester"
# Note: NO `|| true` here. The tester's exit code is meaningful:
#   - 0 → every test passed (no diffs, no unexpected failures)
#   - 1 → at least one diff / unexpected failure (allSuccess=false in
#         the tester's main.go), OR log.Fatalf on a corpus-vs-tester
#         mismatch (e.g. should_fail entry that no longer fails)
#   - 2 → flag parse / config load / build error
# The `set +e` window is just wide enough to capture the exit code so
# we can emit the report summary before propagating the failure. The
# cleanup trap tears down the compose stack on every exit path.
set +e
"$TESTER_BIN" \
    -config-file "$ROOT_DIR/test-cerberus.yml" \
    -config-file "$QUERIES" \
    -config-file "$OVERLAY" \
    -output-format json > "$OUTPUT"
TESTER_RC=$?
set -e

echo "==> report written to $OUTPUT"
echo "==> summary:"
jq '{total: ([.results[]?] | length), passed: ([.results[]? | select((.unexpectedFailure // "") == "" and (.diff // "") == "")] | length), diffs: ([.results[]? | select((.diff // "") != "")] | length), unexpected_failures: ([.results[]? | select((.unexpectedFailure // "") != "")] | length)}' "$OUTPUT" 2>/dev/null || echo "(install jq for summary)"

exit "$TESTER_RC"
