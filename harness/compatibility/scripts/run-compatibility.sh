#!/usr/bin/env bash
# Compatibility harness entry point.
#
# Brings up the docker-compose stack (reference Prometheus + cerberus +
# ClickHouse + seeder), builds the upstream promql-compliance-tester
# binary, runs it pointed at the two endpoints, and writes the JSON
# report to harness/compatibility/report.json.
#
# Tests are RUN_ONCE here; reconciliation against expected-failures.json
# is done by a follow-up Go script (lands in M1.x — for the seed we just
# capture the raw report and let the user inspect).
#
# Usage:
#   ./harness/compatibility/scripts/run-compatibility.sh        full lifecycle
#   COMPOSE_KEEP=1 ./...                                 leave stack up after run
#
# Env:
#   TESTER_OUTPUT     report file path (default: harness/compatibility/report.json)
#   TESTER_QUERIES    queries yaml (default: upstream/promql/promql-test-queries.yml)
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
QUERIES=${TESTER_QUERIES:-"$ROOT_DIR/upstream/promql/promql-test-queries.yml"}
END_TIME=${TESTER_END_TIME:-"2026-05-11T01:00:00Z"}
RANGE=${TESTER_RANGE:-3600}

echo "==> bringing up compatibility stack"
docker compose up -d --build --wait clickhouse prometheus cerberus
echo "==> running seeder (go run ./cmd/seed)"
(cd "$ROOT_DIR/../.." && go run ./harness/compatibility/cmd/seed/)

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
trap 'rm -f "$OVERLAY"' EXIT
cat > "$OVERLAY" <<EOF
query_time_parameters:
  end_time: '${END_TIME}'
  range_in_seconds: ${RANGE}
EOF

echo "==> running tester"
# Note: NO `|| true` here. A non-zero exit from the tester is meaningful:
#   - flag parsing / config errors (exit 2) → real harness bug
#   - per-query divergences are emitted INSIDE the JSON report, not via
#     exit code — the tester exits 0 even when individual queries
#     diverge, so we lose no signal by failing-fast on a non-zero exit.
# Stdout (the JSON report) and stderr (operator-readable log lines from
# the tester) are split so the report file stays parseable while errors
# remain visible in CI logs.
"$TESTER_BIN" \
    -config-file "$ROOT_DIR/test-cerberus.yml" \
    -config-file "$QUERIES" \
    -config-file "$OVERLAY" \
    -output-format json > "$OUTPUT"

echo "==> report written to $OUTPUT"
echo "==> summary:"
jq '{total: (.totalResults // (.results | length)), passed: ([.results[]? | select(.unexpectedFailure == null and .diff == null)] | length)}' "$OUTPUT" 2>/dev/null || echo "(install jq for summary)"

if [ -z "${COMPOSE_KEEP:-}" ]; then
    echo "==> tearing down (set COMPOSE_KEEP=1 to leave running)"
    docker compose down -v
fi
