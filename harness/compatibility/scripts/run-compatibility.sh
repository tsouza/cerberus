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

set -eu -o pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT_DIR"

OUTPUT=${TESTER_OUTPUT:-"$ROOT_DIR/report.json"}
QUERIES=${TESTER_QUERIES:-"$ROOT_DIR/upstream/promql/promql-test-queries.yml"}
END_TIME=${TESTER_END_TIME:-"2026-05-11T01:00:00Z"}
RANGE=${TESTER_RANGE:-3600}

echo "==> bringing up compatibility stack"
docker compose up -d --build --wait clickhouse prometheus cerberus
echo "==> running seeder"
docker compose up --no-log-prefix seeder

echo "==> building promql-compliance-tester"
TESTER_DIR="$ROOT_DIR/upstream/promql/cmd/promql-compliance-tester"
TESTER_BIN="$ROOT_DIR/upstream/promql/cmd/promql-compliance-tester/promql-compliance-tester"
(cd "$TESTER_DIR" && go build -o promql-compliance-tester .)

echo "==> running tester"
"$TESTER_BIN" \
    -config-file "$ROOT_DIR/test-cerberus.yml" \
    -query-file "$QUERIES" \
    -end "$END_TIME" \
    -range "${RANGE}s" \
    -output-format json > "$OUTPUT" || true

echo "==> report written to $OUTPUT"
echo "==> summary:"
jq '{total: (.totalResults // (.results | length)), passed: ([.results[]? | select(.unexpectedFailure == null and .diff == null)] | length)}' "$OUTPUT" 2>/dev/null || echo "(install jq for summary)"

if [ -z "${COMPOSE_KEEP:-}" ]; then
    echo "==> tearing down (set COMPOSE_KEEP=1 to leave running)"
    docker compose down -v
fi
