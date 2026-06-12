#!/usr/bin/env bash
# Compatibility harness entry point.
#
# Brings up the docker-compose stack (reference Prometheus + cerberus +
# ClickHouse + seeder), builds the upstream promql-compliance-tester
# binary, runs it pointed at the two endpoints, writes the JSON report
# to compatibility/prometheus/report.json, and emits a shields.io
# endpoint-badge compat-score JSON next to it.
#
# Per task #68 ("compat is informational" workstream), this harness is
# report-only: per-case parity diffs no longer fail the run. The
# upstream tester still exits non-zero when any case diff'd (it has
# no "report-only" flag), but the wrapping script ignores that exit
# as long as it can read + score the report. Only hard infrastructure
# failures (compose-up, build, missing report.json, scorer failure)
# escalate to a non-zero rc.
#
# Tests are RUN_ONCE here. There is no expected-failures allow-list:
# every diff against reference Prometheus is a real bug to surface and
# fix at the source.
#
# Usage:
#   ./compatibility/prometheus/scripts/run-compatibility.sh        full lifecycle
#   COMPOSE_KEEP=1 ./...                                 leave stack up after run
#
# Env:
#   TESTER_OUTPUT     report file path (default: compatibility/prometheus/report.json)
#   TESTER_SCORE      compat-score.json output path (default:
#                     compatibility/prometheus/compat-score.json).
#                     Shields.io endpoint-badge contract — see
#                     compatibility/internal/score for the schema.
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
# Score JSON lives next to report.json so today's workflow (which
# uploads the single report.json file as an artifact) can be widened
# in task #69 to include the score with a single path-glob change.
# A dedicated reports/ sub-dir (like compatibility/{loki,tempo}/
# reports/) would have required the workflow change up-front.
SCORE=${TESTER_SCORE:-"$ROOT_DIR/compat-score.json"}
QUERIES=${TESTER_QUERIES:-"$ROOT_DIR/cerberus-test-queries.yml"}
END_TIME=${TESTER_END_TIME:-"2026-05-11T01:00:00Z"}
RANGE=${TESTER_RANGE:-3600}

echo "==> bringing up compatibility stack"
docker compose up -d --build --wait clickhouse prometheus cerberus
echo "==> running seeder (go run ./cmd/seed)"
(cd "$ROOT_DIR/../.." && go run ./compatibility/prometheus/cmd/seed/)

# Patch an upstream tester bug before building: the comparer
# (upstream/promql/comparer/comparer.go) sorts ONLY the test backend's
# matrix — `sort.Sort(testResult.(model.Matrix))` — and then diffs it
# against the reference matrix in the reference's NATIVE return order.
# Those two orders are NOT the same: `model.Matrix.Less` (prometheus/
# common) orders series by LABEL COUNT first (LabelSet.Before:
# `if len(ls) < len(o) { return true }`), then lexicographically;
# reference Prometheus returns series in pure lexicographic
# (labels.Compare) order. For a selector whose matched series have
# DIFFERENT label counts — e.g. `{job="demo", __name__!~"..."}` mixes
# `up{instance,job}` (2 labels) with `demo_disk_total_bytes{device,
# instance,job}` (3 labels) — the count-first test sort and the
# lexicographic reference order diverge, and the order-sensitive
# `cmp.Diff` reports a spurious mismatch even though both backends
# returned byte-identical series + samples (verified: sorting BOTH
# matrices yields zero diff). The one-line fix sorts the reference
# matrix with the same `model.Matrix.Sort` so the two sides are ordered
# identically before the diff. Idempotent (the grep guard skips a
# re-patch); applied to the vendored source before the build because the
# submodule pins upstream `prometheus/compliance` and carrying a fork
# just for this is heavier than a build-time patch. Upstream bug; a PR
# to prometheus/compliance to sort both sides would retire this.
echo "==> patching promql-compliance-tester comparer (symmetric matrix sort)"
COMPARER="$ROOT_DIR/upstream/promql/comparer/comparer.go"
if ! grep -q 'sort.Sort(refResult.(model.Matrix))' "$COMPARER"; then
    # Insert the reference sort immediately after the existing test sort.
    perl -0pi -e 's/(\tsort\.Sort\(testResult\.\(model\.Matrix\)\)\n)/$1\tsort.Sort(refResult.(model.Matrix))\n/' "$COMPARER"
    if ! grep -q 'sort.Sort(refResult.(model.Matrix))' "$COMPARER"; then
        echo "ERROR: failed to patch comparer.go symmetric sort (upstream layout changed?)" >&2
        exit 2
    fi
fi

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
SCORER_BIN=$(mktemp -t cerberus-prom-scorer.XXXXXX)

# Consolidated cleanup: remove the overlay + scorer binary AND tear
# down the compose stack on every exit path (success, tester failure,
# set -e abort, manual SIGINT). Without this, a non-zero tester exit
# propagated by `set -e` would leak the stack across re-runs — local
# repros, in particular, would inherit dirty CH state and confuse
# subsequent debugging.
cleanup() {
    rc=$?
    rm -f "$OVERLAY" "$SCORER_BIN"
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
# The upstream tester's exit code:
#   - 0 → every test passed (no diffs, no unexpected failures)
#   - 1 → at least one diff / unexpected failure (allSuccess=false in
#         the tester's main.go), OR log.Fatalf on a corpus-vs-tester
#         mismatch (e.g. should_fail entry that no longer fails)
#   - 2 → flag parse / config load / build error
#
# Per task #68, RC=1 (parity drift) is no longer a harness failure:
# the report.json captures the drift and the scorer below renders it
# into compat-score.json. RC=2 (config / flag / build error) is still
# a hard failure because there's no usable report to score. We
# distinguish the two by checking whether report.json was produced
# AND parses as JSON with a non-null results array — if so, RC=1 is
# parity drift; otherwise it's hard.
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

# Hard-error gate: if the tester didn't even produce a parseable JSON
# report, the run is infrastructure-broken. Bail with the tester's
# rc so the workflow turns red.
if ! jq -e '.results | type == "array"' "$OUTPUT" >/dev/null 2>&1; then
    echo "==> report not parseable as JSON with .results array; treating as hard failure"
    exit "$TESTER_RC"
fi

# Rejection-parity pass: every deliberate 422 in internal/promql must
# also be rejected by reference Prometheus (status-class comparison,
# never message text). The corpus is the rejection catalogue itself —
# see test/rejection-parity/ and docs/compatibility.md. Report-only:
# wrong_rejection verdicts land in the JSON report; only driver-level
# infrastructure failures propagate (set -e).
echo "==> running rejection-parity driver (promql)"
(cd "$ROOT_DIR/../.." && go run ./compatibility/cmd/rejection-parity \
    -head promql \
    -catalogue test/rejection-parity/catalogue.json \
    -ref http://localhost:29090 \
    -cerberus http://localhost:29091 \
    -report "$ROOT_DIR/rejection-parity.json")
echo "==> rejection-parity report written to $ROOT_DIR/rejection-parity.json"

# Build + run the in-tree scorer. The scorer reads report.json and
# writes the shields.io endpoint-badge compat-score JSON to $SCORE.
# Its own exit is propagated — a scorer failure means the harness
# can't produce the downstream artefact, which IS a hard failure.
echo "==> building prometheus-compat-scorer"
(cd "$ROOT_DIR/../.." && go build -o "$SCORER_BIN" ./compatibility/prometheus/cmd/scorer)

mkdir -p "$(dirname "$SCORE")"
"$SCORER_BIN" -report "$OUTPUT" -score "$SCORE"

echo "==> score written to $SCORE"
if [ "$TESTER_RC" -ne 0 ]; then
    echo "==> tester rc=$TESTER_RC was parity drift (report.json present); exiting 0"
fi
exit 0
