# === Compatibility (prometheus/compliance differential harness) ===

# Run the PromQL compatibility suite end-to-end. Slow; expect minutes.
# Sets up the Docker Compose stack (reference Prom + cerberus + CH + seeder),
# runs the upstream tester, writes compatibility/prometheus/report.json.

# Run the PromQL compatibility suite end-to-end. Slow; expect minutes.
compat-promql:
    ./compatibility/prometheus/scripts/run-prometheus-compatibility.sh

# Keep the compatibility stack running after the tester finishes (for debugging).
compat-promql-keep:
    COMPOSE_KEEP=1 ./compatibility/prometheus/scripts/run-prometheus-compatibility.sh

# Tear down the compatibility stack manually.
compat-promql-down:
    cd compatibility/prometheus && docker compose down -v

# === Compatibility (LogQL — Loki compatibility harness) ===

# Run the LogQL compatibility harness end-to-end. Brings up reference
# Loki + cerberus + ClickHouse, seeds both, builds the diff driver from
# the vendored upstream/loki-bench corpus, runs TestRemoteStorageEquality
# against both endpoints, writes compatibility/loki/reports/diff.json.
# See compatibility/loki/README.md for the harness layout.

# Run the LogQL compatibility harness end-to-end. Slow; expect minutes.
compat-logql:
    ./compatibility/loki/scripts/run-loki-compatibility.sh

# Run the smoke (compose + seed + /labels assertion) without the diff
# driver. Useful when the seeder is the bisect target.

# Run the LogQL compose + seed + /labels smoke without the diff driver.
compat-logql-smoke:
    DRIVER_SKIP=1 ./compatibility/loki/scripts/run-loki-compatibility.sh

# Keep the Loki compatibility stack running after the run finishes
# (for debugging /loki/api/v1/* + ClickHouse manually).

# Keep the Loki compatibility stack running after the run finishes (for debugging).
compat-logql-keep:
    COMPOSE_KEEP=1 ./compatibility/loki/scripts/run-loki-compatibility.sh

# Tear down the Loki compatibility stack manually.
compat-logql-down:
    cd compatibility/loki && docker compose down -v
# === Tempo / TraceQL compatibility harness ===

# Run the Tempo / TraceQL compatibility harness end-to-end. Slow; expect
# minutes. Sets up the Docker Compose stack (reference Tempo +
# cerberus + CH + seeder driver), runs the seeder which pushes a
# deterministic OTLP batch to Tempo and an equivalent INSERT into CH
# for cerberus, then runs the differ over the TXTAR corpus.

# Run the Tempo / TraceQL compatibility harness end-to-end. Slow; expect minutes.
compat-traceql:
    ./compatibility/tempo/scripts/run-tempo-compatibility.sh

# Keep the tempo-compatibility stack running after the driver finishes (for debugging).
compat-traceql-keep:
    COMPOSE_KEEP=1 ./compatibility/tempo/scripts/run-tempo-compatibility.sh

# Tear down the tempo-compatibility stack manually.
compat-traceql-down:
    cd compatibility/tempo && docker compose down -v

# === Compatibility — all three heads ===

# Run all three compatibility harnesses sequentially: PromQL
# (prometheus/compliance), LogQL (vendored grafana/loki:pkg/logql/bench
# + cerberus-owned tester), TraceQL (TXTAR corpus + cerberus-owned
# differ over /api/search + tags + metrics endpoints). Each sub-recipe
# tears its own compose stack down on every exit path.
# Slow — expect tens of minutes. Use the per-head recipes for iteration.
# Exit semantics: fails fast on the first non-zero recipe; the report
# files for each head land under compatibility/*/reports/ regardless.

# Run all three compatibility harnesses in sequence. Slow — expect tens of minutes.
compat-all: compat-promql compat-logql compat-traceql

