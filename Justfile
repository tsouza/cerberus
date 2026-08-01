# Cerberus task runner. All commands go through `just`.
# Run `just` for the full recipe list.

set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

GOLANGCI_LINT_VERSION := "v2.12.2"
GOFUMPT_VERSION := "v0.7.0"
GOIMPORTS_VERSION := "latest"
GREMLINS_VERSION := "v0.6.0"
MARKDOWNLINT_VERSION := "v0.18.1"
ACTIONLINT_VERSION := "v1.7.12"
MODULE := "github.com/tsouza/cerberus"

# Per-checkout compose project suffix, exported into every recipe so no compose
# call site has to remember it: each docker-compose file spells its project name
# `<stable-base>${COMPOSE_PROJECT_SUFFIX:-}`, and a compose invocation from a
# recipe, from a harness script a recipe runs, or from a Go test a recipe runs
# all inherit the same value. Every image tag this tree builds into the local
# daemon carries it too, because a tag is namespaced by the daemon rather than
# by the project. Empty in a primary checkout and in CI — project, container,
# network, volume and image names are then exactly what they are without this
# mechanism — and a short path-derived hash in a linked worktree, so each agent's
# stack is a distinct set of objects rather than a shared one. Host ports stay
# fixed, so two worktrees still cannot run the same stack at once; that now fails
# on a port bind instead of silently adopting the other checkout's containers.
# See scripts/compose-project-suffix.sh.
#
# `just` evaluates this before running ANY recipe, so the derivation has to hold
# for a checkout with no git in sight (it prints nothing, and the bare project
# names apply) and for an invocation whose working directory is anywhere at all
# (hence the justfile-relative path). What it does not paper over is a checkout
# missing the script: that fails loudly, naming the absolute path, rather than
# defaulting to a suffix that would silently share stacks between worktrees.
export COMPOSE_PROJECT_SUFFIX := shell('exec "$1"', justfile_directory() / "scripts/compose-project-suffix.sh")

# Default: list recipes.
default:
    @just --list

# === Tools ===

# Install dev tools into $GOBIN (one-time).
install-tools:
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@{{GOLANGCI_LINT_VERSION}}
    go install mvdan.cc/gofumpt@{{GOFUMPT_VERSION}}
    go install golang.org/x/tools/cmd/goimports@{{GOIMPORTS_VERSION}}
    go install github.com/go-gremlins/gremlins/cmd/gremlins@{{GREMLINS_VERSION}}
    go install github.com/rhysd/actionlint/cmd/actionlint@{{ACTIONLINT_VERSION}}

# Install lefthook + activate git hooks. Idempotent; run once after clone.
# Hooks defined in lefthook.yml run gofumpt / goimports / markdownlint-cli2 --fix
# on staged files at commit time, and commitlint on the commit message.
# Heavy validation (go test / golangci-lint / go build) is NOT in the hook —
# CI owns that. See CLAUDE.md § "No local validation; lefthook + CI own it."
hooks-install:
    go install github.com/evilmartians/lefthook@latest
    lefthook install

# === Build ===

# Build cerberus into ./bin.
build:
    go build -trimpath -o bin/cerberus ./cmd/cerberus

# Install cerberus into $GOBIN.
install:
    go install -trimpath ./cmd/cerberus

# Remove build outputs.
clean:
    rm -rf bin/ dist/

# === Generate ===

# Regenerate the structural feature table (id / minVersion / stability) in
# docs/clickhouse-optimizations.md from internal/chopt/registry.go, the single
# source of truth. Adding a feature to the registry lands it in the doc here;
# CI fails any PR whose generated block drifts (git diff --exit-code). The rich
# hand-authored columns (experimental setting, effect prose) stay outside the
# markers and are untouched. Same as `go generate ./internal/chopt/`.
gen-opt-docs:
    go run ./cmd/cerberus optdocs -doc docs/clickhouse-optimizations.md

# Run the router-rules catalog against a corpus (offline analysis). Mines the
# cerberus_router_corpus table (or its per-pod JSONL fallback) and prints
# findings where the recorded A/B route is paying a cost the corpus shows the
# other route would avoid. Pass flags through, e.g.:
#   just route-rules --source jsonl --corpus-path /var/lib/cerberus/router-corpus
#   just route-rules --validate-only
route-rules *ARGS:
    go run ./cmd/cerberus route-rules {{ARGS}}

# Pre-cutover migration preview. Renders the ClickHouse schema cerberus expects
# from the current CERBERUS_* environment — offline, no database connection —
# so you can review it before provisioning. Pipeable into clickhouse-client:
#   just migrate schema | clickhouse-client -h ...
migrate *ARGS:
    go run ./cmd/cerberus migrate {{ARGS}}

# === Test ===

# Run unit + spec tests with race detector, then type-check the build-tagged
# lanes no other recipe compiles. This is the whole `check` gate in one
# command, which is what you want locally; CI splits it across two concurrent
# jobs via the two recipes below, so keep this one a pure composition of them
# rather than a third copy of the commands.
test: test-unit vet-tagged

# The race-detected unit + spec suite. CI's `check-test` job runs exactly this
# and nothing else: it is the long pole of the gate, and pairing it with the
# quick type-check/build work would serialise two things that have no reason to
# wait on each other.
test-unit:
    go test -race ./...

# Type-check the build-tagged migration lanes no other recipe compiles. `go vet`
# typechecks, so a rename in an untagged package that breaks a `migration_tier1`
# or `migration_tier2` assertion fails HERE, in the required `check` gate,
# instead of surfacing in the scheduled `migration` workflow — the only lane
# that executes those files. Two separate invocations, not one
# `-tags=migration_tier1,migration_tier2`: the two tiers run against different
# compose stacks and never share a process, and tier2_ruler_test.go's helpers
# are named to avoid colliding with tier1_stack_test.go's only because both live
# in package migration — a single combined vet call would silently start
# requiring that to keep holding.
vet-tagged:
    go vet -tags=migration_tier1 ./test/e2e/migration/...
    go vet -tags=migration_tier2 ./test/e2e/migration/...

# Run the internal/schema/ddl integration tests against a real ClickHouse
# container (spun up via testcontainers-go). Requires Docker. Gated behind
# the `integration` build tag so regular `just test` doesn't pull in
# Docker.
schema-ddl-test:
    @just _pull-retry {{CH_TEST_IMAGE}} {{CH_TEST_IMAGE_PRIOR}}
    go test -race -tags=integration ./internal/schema/ddl/...

# Run the TXTAR spec suite with the chDB-backed round-trip assertion
# layer enabled. Requires libchdb.so (see `just chdb-install`). The
# default `just test` lane stays CGO_ENABLED=0 and never compiles the
# chdb-go driver. Only fixtures that declare both `seed:` and
# `expected_rows:` are executed against chDB; everything else still
# runs through the text-equality golden path.
spec-chdb:
    go test -tags chdb -count=1 ./test/spec/...

# Run the chDB-tagged handler tests under internal/api/... plus the
# chclienttest package itself and the consumer-corpus replay lane
# (test/consumer-corpus — captured Grafana request shapes executed
# through chDB seeds). Same prerequisite as spec-chdb (libchdb at the
# default install path). Mirrors the `chdb` CI job.
# Includes ./internal/routerrules/... so the chDB cross-backend parity test
# (parity_chdb_test.go) actually RUNS, not just compiles.
# Includes ./internal/schema/ddl/... so the chDB-backed trace_id cross-table
# index probe (trace_id_index_probe_chdb_test.go) actually RUNS under a
# pass/fail CI gate, rather than only ever compiling under `chdb-build`'s
# build+vet-only check or executing inside `just coverage`'s `|| true`-
# shielded, non-required lane. Includes ./internal/solver/... so the A-vs-B
# route memo chDB differential lane (avb_chdb_lane_test.go) actually RUNS the
# parity proof against a real ClickHouse engine instead of only typechecking
# under `go vet -tags chdb`. Includes ./internal/optcorpus/... so the
# query_log.type Enum8 resolution probe (querylogenum_chdb_test.go) actually
# RUNS: the corpus reconciler's terminal-row predicate is only correct because
# of how a real engine resolves a name against an Enum8, and no shape assertion
# over the emitted SQL can prove that.
test-chdb:
    go test -tags chdb -count=1 ./internal/chclienttest/... ./internal/api/... ./internal/optcorpus/... ./internal/routerrules/... ./internal/schema/ddl/... ./internal/solver/... ./test/consumer-corpus/...

# Run the chDB-tagged property tests (rapid + from-scratch oracle).
# Requires libchdb.so (see `just chdb-install`). Local default is rapid's
# 100 iterations; the nightly `property` CI workflow overrides to 500.
property:
    go test -tags chdb -count=1 ./test/property/...

# Run the chDB-tagged perf regression guards (test/perf). These are
# deterministic ASSERTION pins — not wall-clock benchmarks — that bite a
# regression of the landed perf wins: the metrics-table MetricName-first
# ORDER BY granule prune (EXPLAIN indexes=1 ratio floor) and the /series
# fan-out round-trip baseline. Requires libchdb.so (see `just chdb-install`).
# Mirrors the `perf-guards` CI job in chdb.yml. Distinct from the
# informational `perf-benchmark.yml` lane, which only reports benchstat
# deltas and never gates.
perf-chdb:
    go test -tags chdb -count=1 ./test/perf/...

# Profile the WHOLE TXTAR corpus for compute fan-out (perf-assessment
# Component B). Walks every executable fixture under test/spec/** (those
# with `seed:` + `expected_rows:` + `sql:`) and, per fixture, runs
# EXPLAIN PLAN actions=1 (CROSS JOIN / ARRAY JOIN / recursive-CTE
# detection) + a per-subquery-level count() decomposition (peak
# intermediate cardinality vs leaf scan rows) in-process via chDB.
# Writes a JSON profile array and prints the top fan_factor fixtures.
# Requires libchdb.so (see `just chdb-install`). Drives the nightly
# perf-profile.yml lane — NOT a per-PR gate (corpus-wide breadth over
# ~640 fixtures is too heavy for every PR). Override OUT / TOP to taste.
perf-profile OUT="perf-profile.json" TOP="40":
    go run -tags chdb ./cmd/perf-profile -spec test/spec -out {{OUT}} -top {{TOP}}

# Run the chclient testcontainers integration tests against a real
# ClickHouse container. Requires Docker. Gated behind the `integration`
# build tag so regular `just test` doesn't pull in Docker.
chclient-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -race -tags=integration ./internal/chclient/...

# Run the strict-scan differential: execute the matrix-shaped spec golden
# SQL corpus against a REAL ClickHouse (testcontainers-go) through the
# production cursor's strict positional scan, failing on any type-coercion
# error. This catches the prod-vs-chDB divergence the chdb-tagged goldens
# are structurally blind to (chDB leniently coerces e.g. UInt8 -> *float64;
# clickhouse-go strict-scans and 502s). Requires Docker; gated behind the
# `integration` build tag. See test/spec/strictscan_integration_test.go and
# .github/workflows/strict-scan.yml.
strict-scan-test:
    @just _pull-retry {{CH_STRICT_SCAN_IMAGE}}
    go test -tags=integration -count=1 -run TestStrictScanDifferential ./test/spec/...

# Run the router-corpus real-CH integration tests: the offline corpus WRITE
# path (optcorpus.CHTableSink: real CREATE DDL + columnar Enum8 batch) and READ
# path (routerrules.chCorpusSource: strict-scanned aggregates) executed against
# a REAL ClickHouse (testcontainers-go) through clickhouse-go/v2. These offline
# paths never touch the data plane, so compose-smoke / e2e never exercise them,
# and their only other tests use a fake batch / chDB (which leniently coerces
# integer columns into *float64 — exactly the #1064 strict-scan blind spot).
# Requires Docker; gated behind the `integration` build tag. See
# internal/routerrules/realch_integration_test.go and strict-scan.yml.
router-corpus-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -tags=integration -count=1 -run 'RealClickHouse' ./internal/routerrules/... ./internal/optcorpus/...

# Run the TraceQL spans-scan resource-bound real-CH guard (PR #1154): lowers +
# emits the Grafana Traces Drilldown Structure + Comparison queries through the
# real cerberus path and executes them against a REAL ClickHouse
# (testcontainers-go) seeded with a multi-partition otel_traces. Proves the
# bounded recursive-CTE drilldown SQL runs WITHOUT error 49 (the seam chDB
# cannot validate), that partition pruning fires at runtime, and that the
# compare matrix scan fails closed on an absent window. Requires Docker; gated
# behind the `integration` build tag. See
# test/spec/traces_scan_resource_bound_integration_test.go and strict-scan.yml.
traces-scan-bound-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -tags=integration -count=1 -run TestTracesScanResourceBoundRealCH ./test/spec/...

# Run the solver's mandatory per-shard memory-apportionment real-CH guard:
# proves Executor.runShard's max_memory_usage WithQuerySetting override is
# actually honored by a real ClickHouse over clickhouse-go/v2, not just
# accepted and silently ignored (chDB is known-lenient about settings, the
# same class of blind spot the strict-scan lane documents for type
# coercion). A deliberately tiny per-shard budget must fail with CH's own
# memory-limit error; a generous one on the identical query must succeed.
# Requires Docker; gated behind the `integration` build tag. See
# internal/solver/executor_realch_integration_test.go and strict-scan.yml.
solver-memory-apportion-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -tags=integration -count=1 -run TestExecutor_PerShardMaxMemoryUsage_RealClickHouse ./internal/solver/...

# Run the FuzzParse target for one parser head for a bounded duration.
# Usage: `just fuzz QL=promql DURATION=60s` (defaults).
fuzz QL="promql" DURATION="60s":
    go test -run='^$' -fuzz=FuzzParse -fuzztime={{DURATION}} ./internal/{{QL}}/...

# Run all Go benchmarks (no tests). Short benchtime for local use.
bench:
    go test -bench=. -benchmem -benchtime=5x -run='^$' ./...

# Generate the GA-prep coverage baseline (default-tag + chdb-tagged
# lanes, merged via in-line awk because gocovmerge can't reconcile
# block-boundary drift between the two compilations). Writes
# cover.out, cover-chdb.out, and cover-merged.out, then prints the
# total + a per-package summary sorted by coverage.
#
# Requires chDB for the second lane (`just chdb-install`). If
# libchdb.so isn't present, the recipe still emits cover.out and
# treats cover-merged.out as cover.out (default-tag baseline only).
coverage:
    @echo "==> default-tag coverage"
    # `|| true` tolerates partial failures (e.g. `main` packages that
    # require the `covdata` tool on toolchains that ship without it).
    # The cover.out profile is still written for every package that
    # compiled, which is all production code in internal/**.
    go test -coverprofile=cover.out ./... || true
    @test -s cover.out
    @if [ -e /usr/local/lib/libchdb.so ]; then \
        echo "==> chdb-tagged coverage"; \
        go test -tags chdb -coverprofile=cover-chdb.out ./... || true; \
        echo "==> merging profiles"; \
        { echo "mode: set"; \
          awk 'FNR==1{next} { k=$1" "$2; if (!(k in m) || $3>m[k]) m[k]=$3 } END { for (k in m) print k, m[k] }' cover.out cover-chdb.out | sort; \
        } > cover-merged.out; \
    else \
        echo "==> libchdb.so not found, skipping chdb lane"; \
        cp cover.out cover-merged.out; \
    fi
    @echo
    @echo "==> Total"
    @go tool cover -func=cover-merged.out | tail -1
    @echo
    @echo "==> Per-package (sorted by coverage)"
    @awk -F'[: ,]' 'NR > 1 { \
        n = split($0, w, " "); stmts = w[n-1]; hits = w[n]; \
        split($0, a, ":"); fp = a[1]; \
        sub(/^github\.com\/tsouza\/cerberus\//, "", fp); \
        k = fp; sub(/\/[^\/]+$/, "", k); \
        total[k] += stmts; \
        if (hits != 0) covered[k] += stmts; \
      } END { \
        for (p in total) { \
          pct = (total[p] > 0) ? 100.0*covered[p]/total[p] : 0; \
          printf "%6.2f%%  %5d / %-5d  %s\n", pct, covered[p], total[p], p; \
        } \
      }' cover-merged.out | sort -rn

# Regenerate TXTAR golden sections in test/spec/**/*.txtar from current output.
# Two lanes: the default-tag pass rewrites `-- sql --` / `-- chplan --`
# text goldens, then a chdb-tagged pass (mirroring `just spec-chdb`)
# rewrites the `-- expected_rows --` round-trip cells, which only execute
# under that build tag. Requires libchdb.so (`just chdb-install`) — the
# recipe fails fast without it rather than leaving stale expected_rows
# behind (the PR #758 failure mode).
#
# It also chains the two perf-assessment ratchet baselines so a SINGLE
# `just update-golden` records every fixture-derived artefact in one shot:
# the solver routing-DECISION baseline, the text/expected_rows goldens (this
# body), and the cardinality fan-factor baseline. This closes the recurring
# miss where a new TXTAR fixture regenerated its goldens but left
# `cardinality-baseline.json` unrecorded, turning
# `perf-guards` TestCardinalityRatchet red on main after merge (hit by #1096
# native_resample_offset and #1098 increase_left_edge_scan_bound).
#
# The two baselines sit on OPPOSITE sides of the body, because they read
# opposite ends of a fixture:
#
# - `update-solver-decision-baseline` is a PRIOR dep. It re-derives every
#   decision from the fixture's `-- query.promql --` INPUT (parse -> lower ->
#   optimize -> classify), so the golden rewrite cannot change what it
#   records. It must run FIRST because the body's default-tag
#   `GOLDEN_UPDATE=1 go test ./...` lane runs TestSolverDecisionRatchet in
#   ASSERT mode, which fails on a brand-new fixture not yet in the routing
#   baseline — aborting before the body could finish.
# - `update-cardinality-baseline` is a SUBSEQUENT dep (`&&`). It profiles the
#   fixture's RECORDED `-- sql --` section verbatim: spec.PrepareRoundTrip
#   rewrites that literal text and hands it to profile.ProfileFixture. Run
#   BEFORE the body it profiles the very SQL this recipe is about to
#   overwrite — and on a brand-new fixture `-- sql --` is still EMPTY, so
#   PrepareRoundTrip returns ok=false, the fixture is not discovered as
#   executable at all, and the row the chaining exists to record is silently
#   skipped. It must run AFTER, which is safe: the body's chdb-tagged
#   asserting lane scopes to ./test/spec/..., never ./test/perf/.
#
# All three are no-ops on a corpus already in sync (zero diff).
# Review `git diff test/spec/ test/perf/*-baseline.json` before committing.
update-golden: update-solver-decision-baseline && update-cardinality-baseline
    @test -f "{{CHDB_INSTALL_PATH}}" || { echo "error: {{CHDB_INSTALL_PATH}} not found — run 'just chdb-install' first; without it the chdb-tagged -- expected_rows -- sections (and the cardinality baseline) cannot regenerate and go stale" >&2; exit 1; }
    GOLDEN_UPDATE=1 go test ./...
    GOLDEN_UPDATE=1 go test -tags chdb -count=1 ./test/spec/...
    @echo
    @echo "Diff of regenerated fixtures:"
    @git --no-pager diff --stat test/spec/ || true

# Regenerate the cardinality/fan-factor ratchet baseline (perf-assessment
# Component C) from the current corpus profile. Re-profiles every executable
# TXTAR fixture under test/spec/** in-process via chDB and rewrites
# test/perf/cardinality-baseline.json (deterministic, sorted by fixture).
# Requires libchdb.so (`just chdb-install`). Run this — and review the diff —
# whenever the ratchet test reports a NEW/REMOVED fixture or a deliberately
# intended fan_factor change; the diff is the built-in cost review (it shows
# each construct's absolute fan_factor). The gating assertion is
# TestCardinalityRatchet in the already-required `perf-guards` job.
update-cardinality-baseline:
    @test -f "{{CHDB_INSTALL_PATH}}" || { echo "error: {{CHDB_INSTALL_PATH}} not found — run 'just chdb-install' first; the ratchet baseline is generated by an in-process chDB profile pass" >&2; exit 1; }
    UPDATE_CARDINALITY_BASELINE=1 go test -tags chdb -count=1 -run TestCardinalityRatchet ./test/perf/
    @echo
    @echo "Diff of regenerated baseline:"
    @git --no-pager diff --stat test/perf/cardinality-baseline.json || true

# Regenerate the routing-DECISION ratchet baseline (perf-assessment
# Component D) from the current PromQL corpus. Parses every `-- query.promql --`
# fixture under test/spec/promql/**, lowers it on the fixed eval grid
# (end=2026-01-01T00:00:00Z, range=1h, step=15s), optimizes it, and records the
# solver Planner's routing decision {routed, K, reason} plus the classifier's
# cost grid {n_anchors, fanout, cumulative_d, outer_range} under Mode=auto into
# test/perf/solver-decision-baseline.json (deterministic, sorted by query).
# Pure Go — NO chDB — so it runs in the standard `check`/`just test` lane.
# Run this — and REVIEW THE DIFF — whenever the ratchet test reports drift or a
# NEW/REMOVED query. The diff classifies each moved row as ADVANCEMENT vs
# REGRESSION; a REGRESSION (route B->A, K down, or a routed query now rejected)
# MUST be justified in the PR with a real reason (a correctness fix that
# disqualifies the query), never accepted as a silent relaxation. The gating
# assertion is TestSolverDecisionRatchet in the already-required `check` job.
update-solver-decision-baseline:
    UPDATE_SOLVER_DECISION_BASELINE=1 go test -count=1 -run TestSolverDecisionRatchet ./test/perf/
    @echo
    @echo "Diff of regenerated baseline:"
    @git --no-pager diff --stat test/perf/solver-decision-baseline.json || true

# Regenerate the SCALE-WALL pin baseline — the perf guard for the wall /
# scan-amplification regression classes the cardinality ratchet is blind to
# (it pins fan_factor only, so #97's 6x CPU-bound wall regression and the
# anchor-grid sharding's 8x scan amplification both sailed through it). Seeds
# a counter table at scale, lowers `sum(rate(http_requests_total[5m]))` on a
# 1h/15s query_range grid through the real lower -> optimizer -> emit chain,
# and records two bounds into test/perf/scale-wall-baseline.json: the
# deterministic peak-intermediate/scan-rows amplification ceiling (PRONG 1)
# and the in-run query/yardstick wall ratio ceiling (PRONG 2). Both carry
# headroom over the measured floor (1.5x / 2.5x). Requires libchdb.so
# (`just chdb-install`). Run this — and REVIEW THE DIFF — only when a bound
# move is genuinely intended (a real, justified compute-cost increase); a
# silent loosen is exactly the regression the pin exists to catch. The gating
# assertion is TestScaleWallPin in the already-required `perf-guards` job.
update-scale-wall-baseline:
    @test -f "{{CHDB_INSTALL_PATH}}" || { echo "error: {{CHDB_INSTALL_PATH}} not found — run 'just chdb-install' first; the scale-wall bounds are measured by an in-process chDB run" >&2; exit 1; }
    UPDATE_SCALE_WALL_BASELINE=1 go test -tags chdb -count=1 -run TestScaleWallPin ./test/perf/
    @echo
    @echo "Diff of regenerated baseline:"
    @git --no-pager diff --stat test/perf/scale-wall-baseline.json || true

# Regenerate the publishable benchmark document (docs/benchmarks.md) from
# LIVE measurements: optimizer before/after wins, per-construct scaling
# curves, per-stage Go micro-benchmarks, and end-to-end query latency on a
# large synthetic dataset (millions of rows generated server-side via
# numbers(N)). The optimized SQL shapes are driven through the real
# cerberus lowering pipeline (internal/{promql,logql,traceql} -> chsql),
# so the measured SQL is the SQL cerberus emits.
#
# Requires libchdb.so (`just chdb-install`) — all measurements run
# in-process against an embedded chDB engine. This is a MANUALLY-run
# artifact, NOT a CI gate: structural metrics (fan_factor, granules,
# allocs/op) are deterministic and committed; timings are presented as
# speedup ratios + labelled indicative, so re-running yields a clean diff
# on the deterministic parts. Re-run whenever perf improves; review the
# diff before committing.
bench-report:
    @test -f "{{CHDB_INSTALL_PATH}}" || { echo "error: {{CHDB_INSTALL_PATH}} not found — run 'just chdb-install' first; the benchmark document is generated by an in-process chDB measurement pass" >&2; exit 1; }
    go run -tags chdb ./cmd/bench-report -out docs/benchmarks.md
    @echo
    @echo "Diff of regenerated benchmark document:"
    @git --no-pager diff --stat docs/benchmarks.md || true

# Regenerate docs/configuration.md from the single source of truth in
# internal/config: the CERBERUS_* env-key metadata (config.EnvDocs) and the
# LIVE viper loader defaults (config.DocDefaults). docs/configuration.md is a
# GENERATED file — do not hand-edit it; edit the EnvDoc metadata in
# internal/config/envdocs.go (or the preamble in cmd/cerberus/cmd_configdocs.go)
# and rerun this. The config-docs CI gate runs `git diff --exit-code` on the
# regenerated file, so a stale doc (or an undocumented new env var) fails CI.
gen-config-docs:
    go run ./cmd/cerberus config-docs -out docs/configuration.md
    @echo
    @echo "Diff of regenerated configuration document:"
    @git --no-pager diff --stat docs/configuration.md || true

# === Mutation testing ===

# Run gremlins across internal/. Slow; expect minutes. Honors .gremlins.yaml.
mutate:
    gremlins unleash ./internal/...

# Quick mutation pass on a single package: `just mutate-pkg internal/chsql`.
mutate-pkg PATH:
    gremlins unleash ./{{PATH}}

# Run gremlins on internal/optimizer/ + internal/chsql/ with the `chdb`
# build tag enabled so the chDB-backed property test (R8.3) and the
# TXTAR round-trip suite (R8.1) participate in the kill criterion.
#
# `-i` is the integration flag: per mutation, gremlins runs the
# complete `go test -tags chdb ./...` instead of just the mutated
# package's local test file. That brings test/spec/<head>/ round-trip
# tests into scope, so a mutation that changes SQL text but not the
# rendered row set is correctly NOT killed (semantically equivalent),
# which sharpens the score over the default lane.
#
# Slow: hundreds of mutants, each spinning up an ephemeral chDB
# session. Expect tens of minutes. Requires libchdb.so (see
# `just chdb-install`). Not on the PR critical path — informational.
mutate-chdb:
    gremlins unleash -t chdb -i ./internal/optimizer/... ./internal/chsql/...

# === Lint / format ===

# Run Go linters.
lint:
    golangci-lint run ./...

# Validate GitHub Actions workflow files (expression contexts, action
# inputs, shellcheck of run blocks). Deliberately separate from `lint`
# (Go): workflow-file defects otherwise surface only as server-side
# zero-job "invalid workflow file" failure runs, which silently
# prevent required pull_request checks from ever being scheduled —
# the #749 secrets-in-step-if incident left the PR BLOCKED on four
# required contexts that could never report.
lint-actions:
    actionlint

# Lint all Markdown files (run via npm exec; no global Node deps).
lint-md:
    npm exec --yes -- markdownlint-cli2@{{MARKDOWNLINT_VERSION}} "**/*.md" "!compatibility/prometheus/upstream/**" "!**/node_modules/**"

# Auto-fix Markdown lint issues where possible.
fmt-md:
    npm exec --yes -- markdownlint-cli2@{{MARKDOWNLINT_VERSION}} --fix "**/*.md" "!compatibility/prometheus/upstream/**" "!**/node_modules/**"

# Format Go code.
fmt:
    gofumpt -l -w .
    goimports -l -w -local {{MODULE}} .

# === CI entry point ===

# Lint + test + build. Used by ci.yml.
ci: lint test build

# === Dependencies ===

# go mod tidy.
deps-tidy:
    go mod tidy

# === chDB (in-process ClickHouse engine probe) ===

CHDB_VERSION := "v26.5.0"
CHDB_INSTALL_PATH := "/usr/local/lib/libchdb.so"

# Install libchdb.so (the in-process ClickHouse engine shared library)
# used by the chdb-go database/sql driver. Required only for tests that
# carry the `chdb` build tag — the engine-probe test under
# `internal/chclient/` and the native-family parity tests under
# `internal/chsql/`. Production builds never link against this; the
# release binary stays CGO_ENABLED=0.
#
# Pinned to chdb-core v26.5.0 (ClickHouse 26.5). The standalone
# `<platform>-libchdb.tar.gz` assets the chdb-go driver expects moved
# from the `chdb-io/chdb` repo (which stopped shipping them after v4.0.2,
# bundling libchdb inside Python wheels only) to `chdb-io/chdb-core`,
# whose release tags track the ClickHouse version directly. v26.5.0 is
# the newest stable that clears the 25.9 native-timeSeries*ToGrid floor
# while staying below the 26.7 aggregate-state serialization change.
# Mirror update_libchdb.sh shipped inside chdb-go.
#
# Idempotent: skips download if the install path already exists. Override
# CHDB_VERSION at the recipe call (`just chdb-install CHDB_VERSION=v26.5.0`).
chdb-install:
    @if [ -f "{{CHDB_INSTALL_PATH}}" ]; then \
        echo "==> libchdb already present at {{CHDB_INSTALL_PATH}} (delete to reinstall)"; \
        exit 0; \
    fi
    @os="$(uname -s)"; \
        arch="$(uname -m)"; \
        case "$os" in \
            Linux) \
                case "$arch" in \
                    aarch64|arm64) asset="linux-aarch64-libchdb.tar.gz" ;; \
                    *)             asset="linux-x86_64-libchdb.tar.gz" ;; \
                esac ;; \
            Darwin) \
                case "$arch" in \
                    arm64) asset="macos-arm64-libchdb.tar.gz" ;; \
                    *)     asset="macos-x86_64-libchdb.tar.gz" ;; \
                esac ;; \
            *) echo "unsupported platform: $os" >&2; exit 1 ;; \
        esac; \
        url="https://github.com/chdb-io/chdb-core/releases/download/{{CHDB_VERSION}}/$asset"; \
        echo "==> downloading $url"; \
        tmp="$(mktemp -d)"; \
        curl -fsSL -o "$tmp/libchdb.tar.gz" "$url"; \
        tar -C "$tmp" -xzf "$tmp/libchdb.tar.gz"; \
        echo "==> installing to {{CHDB_INSTALL_PATH}} (sudo may prompt)"; \
        sudo install -m 0755 "$tmp/libchdb.so" "{{CHDB_INSTALL_PATH}}"; \
        rm -rf "$tmp"; \
        echo "==> libchdb {{CHDB_VERSION}} installed"

# === E2E (k3d + ClickHouse + Grafana + cerberus) ===

K3D_CLUSTER := "cerberus-e2e"
CERBERUS_IMAGE := "cerberus:e2e"

# Chart deployment topology the e2e stack installs: `monolith` (default — one
# Deployment/Service serving all three heads) or `split` (three per-head
# Deployments + bare-named Services, each pinned via CERBERUS_ENABLED_HEADS).
# The dashboard e2e lane runs the SAME Grafana/Playwright smoke against both,
# driven by a CI matrix axis (E2E_MODE); the split-only isolation spec then
# proves one head can die without severing the others.
E2E_MODE := env_var_or_default("E2E_MODE", "monolith")

# Extra Go `-tags` baked into the cerberus image built by `e2e-up`. Empty
# by default, so the dashboard e2e lane and any local `just e2e-up` build a
# stock binary. ONLY the chaos lane sets CERBERUS_BUILD_TAGS=chaos_sleep
# (via the `chaos` job in .github/workflows/e2e.yml) to link the
# deterministic-sleep injection used by ch-slow-query-timeout. Threaded to
# Dockerfile.local's GO_BUILD_TAGS build-arg.
CERBERUS_BUILD_TAGS := env_var_or_default("CERBERUS_BUILD_TAGS", "")

# External images referenced by test/e2e/k3s/*.yaml. Kept in sync with the
# manifests by convention; a stale entry surfaces as a `Pending` /
# `ImagePullBackOff` pod once that image is no longer pre-loaded. When you
# bump a version in a manifest, bump it here too — both sides MUST agree.
#
# Why pre-pull: a fresh k3d cluster's containerd hits the registry directly
# (DockerHub + GHCR). DockerHub's anonymous-pull rate limit is shared across
# GHA-runner IP pools and intermittently fires at ~1/20 e2e runs, leaving
# ClickHouse stuck in `ImagePullBackOff` past the 180 s deployment wait. By
# pulling on the host docker daemon (which has its own auth + cache) and
# importing into k3d via the API, we never go through containerd's pull
# path. See run 26136032208 for the symptom.
# MUST stay in lock-step with the image pins in test/e2e/k3s/*.yaml —
# a stale entry here means the pod pulls straight from the registry at
# start-up (no pre-pull, no import, full Docker-Hub-flake exposure).
E2E_EXTERNAL_IMAGES := "clickhouse/clickhouse-server:26.5-alpine ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.116.0 grafana/grafana:12.2.9 otel/opentelemetry-collector-contrib:0.152.1 busybox:1.37"

# Extra images the bundled-ClickHouse ("bwc") object-storage lane needs on top
# of E2E_EXTERNAL_IMAGES: the MinIO object store + its `mc` client (the
# bucket-create Job) and the bundled ClickHouse image the Helm chart deploys.
# Same pre-pull+import rationale as E2E_EXTERNAL_IMAGES. MUST stay in lock-step
# with the pins in test/e2e/k3s-bwc/*.yaml (minio + mc) and
# test/e2e/k3s/cerberus-values-bwc.yaml (clickhouse.bundled.image) — the chart
# maps the CH container's pullPolicy to .Values.image.pullPolicy (Never in the
# e2e values), so the exact bundled-CH tag MUST be imported here.
E2E_BWC_IMAGES := "minio/minio:RELEASE.2025-09-07T16-13-09Z minio/mc:RELEASE.2025-08-13T08-35-41Z clickhouse/clickhouse-server:26.3"

# ClickHouse images the `-tags=integration` Go tests start through
# testcontainers. testcontainers does the pull itself, single-attempt, so a slow
# Docker Hub fails those lanes the same way it failed the compose lanes — the
# `schema-ddl` job died on a `clickhouse-server:25.8-alpine` manifest HEAD during
# the v1.13.0 release window. Acquiring the images up front makes that pull
# retryable: testcontainers reuses an image already in the daemon.
#
# CH_TEST_IMAGE_PRIOR is the older server the replicated-DDL test pins to prove
# the DDL still applies on the previous supported line, so only that lane needs
# it. TestIntegrationImagePinsMatchTheJustfile holds both against the literals in
# the test sources.
CH_TEST_IMAGE := "clickhouse/clickhouse-server:25.8-alpine"
CH_TEST_IMAGE_PRIOR := "clickhouse/clickhouse-server:24.8-alpine"

# CH_STRICT_SCAN_IMAGE is the server the strict-scan differential boots. Unlike
# the other integration lanes (which pin 25.8 to reproduce a specific
# strict-scan divergence), this lane must EXECUTE every shape the emitter can
# produce — including the native timeSeries*ToGrid family whose chopt floor is
# 25.9. A pin below that floor does not go red; the server rejects the query
# before a row decodes and the shape silently drops out of scope. Kept at or
# above the highest chopt MinVersion by TestStrictScanImageClearsChoptFloors.
CH_STRICT_SCAN_IMAGE := "clickhouse/clickhouse-server:26.5-alpine"

# k3s node image for the k3d clusters. Pinned (k3d otherwise picks a default tag
# per k3d version) so we pull ONE known tag with retry and hand it to
# `k3d cluster create --image` — k3d then boots from the host-cached copy
# instead of letting cluster creation pull k3s from Docker Hub mid-flight, which
# intermittently times out ("registry-1.docker.io ... context deadline
# exceeded") and fails the whole e2e. To drop the Docker Hub dependency entirely,
# re-tag this into ghcr.io (co-located with the CI runners) and point here.
K3S_IMAGE := "rancher/k3s:v1.31.5-k3s1"

# Extra args appended verbatim to `k3d cluster create` in `e2e-up`. Empty by
# default (CI uses none). Interpolated unquoted, so the value is shell-parsed:
# wrap any single arg containing shell metacharacters (`<`, `%`, `,`) in
# single quotes INSIDE the value.
#
# The motivating use case is dev hosts low on disk: k3s kubelet's default
# eviction thresholds (nodefs.available<10%, imagefs.available<15%) taint the
# node with disk-pressure and nothing schedules. For a throwaway local
# cluster, disable eviction:
#
#   K3D_EXTRA_ARGS="--k3s-arg '--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%@server:0'" just e2e-up
K3D_EXTRA_ARGS := env_var_or_default("K3D_EXTRA_ARGS", "")

# Boot the k3d cluster, build cerberus image, import it, apply manifests, wait for pods.
# Host ports map via the k3d loadbalancer to NodePorts on the k3s nodes:
#   host:8080 -> LB -> NodePort 30080 (cerberus svc)
#   host:3000 -> LB -> NodePort 30030 (grafana svc)
# The k3d loadbalancer publishes on 0.0.0.0, so both ports are reachable on
# every host interface (LAN IP included), not just localhost.
# Pull each image with retry + linear backoff. Docker Hub from CI runners
# intermittently times out the pull ("context deadline exceeded"); retrying
# clears the transient failure instead of failing k3d creation / image staging.
#
# The retry itself is `.github/scripts/build-with-registry-retry.mjs` rather
# than a bash loop, because WHICH failures deserve another attempt is the whole
# question and it has exactly one right answer. A hand-rolled loop retries
# everything: a `manifest unknown` five times (slower red, same verdict), and —
# the failure that made this a rule — a Docker Hub rate-limit refusal five
# times, spending four more pulls out of the quota that is already exhausted.
# `lib/registry.mjs` owns that classification for every call site at once.
_pull-retry +IMAGES:
    @for img in {{IMAGES}}; do \
        IMAGE_BUILD_RETRY_BACKOFF_SECONDS=3 node .github/scripts/build-with-registry-retry.mjs \
            docker pull "$img" || exit 1; \
    done

# Acquire every image a compose stack needs, with retry, BEFORE `up` reaches for
# them. `docker compose up` pulls what it is missing but has no retry of its own,
# so one Docker Hub timeout ("context deadline exceeded" on a manifest HEAD)
# fails the whole lane — which is how the v1.13.0 release's Tier-1 leg died on a
# transient `prom/prometheus` fetch. `_pull-retry` already solved this for the
# k3d lanes; the compose lanes just weren't going through it.
#
# The pre-pull runs `docker pull`, not `docker compose pull`, because the two
# do not share a credential source: measured four seconds apart in one job, the
# CLI pull carried the runner's Docker Hub login and compose's was refused as
# UNAUTHENTICATED. Run through compose, the mechanism built to absorb Docker Hub
# failures was spending the anonymous quota rather than the authenticated one —
# which is why it never helped. `compose-pull-images.mjs` owns the resolution:
# it reads which services are fetchable off the compose model's `build:`
# sections (compose's own `--ignore-buildable` semantics) and pulls each one
# through the shared retry policy. See that module's header for the evidence.
_compose-pull-retry +FILES:
    @echo "==> pre-pulling compose images (retry, over the authenticated pull path)"
    @COMPOSE_PULL_BACKOFF_SECONDS=3 node .github/scripts/compose-pull-images.mjs {{FILES}}

# `_pull-retry` / `_compose-pull-retry` protect the images the HOST daemon
# fetches. The images a BUILD fetches — the `FROM` refs BuildKit resolves while
# running `docker build` / `docker compose up` — are a separate exposure that no
# host-side pre-pull reaches (the `docker-container` driver resolves them from
# the registry regardless of what the daemon holds), and a Docker Hub 429 on
# `golang:1.26` took `e2e` + `migration-e2e` down on main. Every build-invoking
# command below therefore runs through
# `.github/scripts/build-with-registry-retry.mjs`, which retries the command on
# registry/network faults only and fails immediately on a real build error.

e2e-up: e2e-down
    @echo "==> pre-pulling k3s node image (retry — Docker Hub flaky from CI)"
    @just _pull-retry {{K3S_IMAGE}}
    @echo "==> creating k3d cluster {{K3D_CLUSTER}}"
    k3d cluster create {{K3D_CLUSTER}} \
        --image {{K3S_IMAGE}} \
        --port "3000:30030@loadbalancer" \
        --port "8080:30080@loadbalancer" \
        --no-lb=false \
        --k3s-arg "--disable=traefik@server:0" \
        {{K3D_EXTRA_ARGS}} \
        --wait
    @echo "==> building cerberus image (build tags: '{{CERBERUS_BUILD_TAGS}}')"
    node .github/scripts/build-with-registry-retry.mjs \
        docker build -t {{CERBERUS_IMAGE}} --build-arg GO_BUILD_TAGS="{{CERBERUS_BUILD_TAGS}}" -f Dockerfile.local .
    @echo "==> pre-pulling external images on host docker (retry)"
    @just _pull-retry {{E2E_EXTERNAL_IMAGES}}
    @echo "==> importing images into k3d ({{K3D_CLUSTER}}) — one at a time, with retry+verify"
    @# k3d bundles EVERY image into one tarball, mounts it into a transient
    @# tools node, and runs `ctr image import`. Two failure modes bite:
    @#   1. the bundled tarball intermittently vanishes mid-import
    @#      ("ctr: open /k3d/images/...tar: no such file or directory" —
    @#      run 27511641349), a transient image-volume race whose window
    @#      grows with the bundle size, so importing ONE image at a time
    @#      shrinks both the tarball and the race;
    @#   2. `k3d image import` prints "Successfully imported" and exits 0
    @#      even when a node-level import failed (run 27274975563 left
    @#      cerberus:e2e absent → ImagePullBackOff), so success has to be
    @#      VERIFIED against the node's containerd, not trusted.
    @# So: import each image on its own, then verify it actually landed,
    @# retrying the import until it's present or the attempt budget is
    @# spent. Normalise short names the way containerd stores them
    @# (docker.io/ + library/ prefixes).
    @for img in {{CERBERUS_IMAGE}} {{E2E_EXTERNAL_IMAGES}}; do \
        ref="$img"; \
        case "$ref" in \
            *.*/*|*:*/*) ;; \
            */*) ref="docker.io/$ref" ;; \
            *)   ref="docker.io/library/$ref" ;; \
        esac; \
        landed=0; \
        for attempt in 1 2 3 4 5; do \
            k3d image import "$img" -c {{K3D_CLUSTER}} || true; \
            if docker exec k3d-{{K3D_CLUSTER}}-server-0 ctr -n k8s.io images ls -q | grep -qF "$ref"; then \
                landed=1; break; \
            fi; \
            echo "    import attempt $attempt: $ref not in containerd yet, retrying after backoff" >&2; \
            sleep $((attempt * 2)); \
        done; \
        if [ "$landed" != "1" ]; then \
            echo "ERROR: $ref missing from k3d node containerd after 5 import attempts" >&2; \
            exit 1; \
        fi; \
        echo "    ok $ref"; \
    done
    @echo "==> applying fixture manifests (CH, Grafana, collector, sample apps)"
    kubectl apply -k test/e2e/k3s/
    @# In split mode the chart-managed datasource hostnames change (each head
    @# gets its own bare-named Service), so rewrite the Grafana datasource
    @# ConfigMap to point each datasource type at its head Service. In monolith
    @# mode the kustomize-applied ConfigMap (url: http://cerberus:8080) is
    @# already correct.
    @#
    @# Grafana provisions datasources into its DB ONCE, at boot, from the
    @# mounted ConfigMap file — and the kubelet's ConfigMap→volume sync lags a
    @# pod's startup by tens of seconds. So updating the ConfigMap object after
    @# `apply -k` has already created the Grafana Deployment does NOT change what
    @# Grafana provisioned: it boots from the original `http://cerberus:8080`
    @# and never re-reads. In split that URL resolves to the NodePort-alias
    @# Service, which selects ONLY the prometheus head, so loki/tempo datasource
    @# queries hit the prometheus process (no /loki/* or /api/* routes) and 404
    @# — the v1.4.0 split-mode dashboard-lane breakage.
    @#
    @# Fix: after updating the ConfigMap, BLOCK until the kubelet has synced the
    @# per-head URLs into the running Grafana pod's mounted file, THEN restart
    @# Grafana so it re-provisions from the corrected file. Polling the mounted
    @# file (not a fixed sleep) makes the restart race-free: Grafana only
    @# re-reads once the new content is actually on disk.
    @if [ "{{E2E_MODE}}" = "split" ]; then \
        echo "==> [split] rewriting Grafana datasource URLs to per-head Services"; \
        kubectl -n cerberus get configmap grafana-datasources -o jsonpath='{.data.datasources\.yaml}' \
            | awk '/type: prometheus/{t="prometheus"} /type: loki/{t="loki"} /type: tempo/{t="tempo"} \
                   /url: http:\/\/cerberus:8080/{sub(/cerberus/, t)} {print}' > /tmp/cerberus-e2e-ds-split.yaml; \
        kubectl -n cerberus create configmap grafana-datasources \
            --from-file=datasources.yaml=/tmp/cerberus-e2e-ds-split.yaml \
            --dry-run=client -o yaml | kubectl apply -f -; \
        echo "==> [split] waiting for the per-head datasource URLs to sync into the Grafana pod"; \
        kubectl -n cerberus rollout status deployment/grafana --timeout=120s; \
        synced=0; \
        for attempt in $(seq 1 60); do \
            if kubectl -n cerberus exec deploy/grafana -- grep -q 'url: http://loki:8080' /etc/grafana/provisioning/datasources/datasources.yaml 2>/dev/null; then \
                synced=1; break; \
            fi; \
            sleep 2; \
        done; \
        [ "$synced" = "1" ] || { echo "ERROR: per-head datasource URLs never synced into the Grafana pod after 120s" >&2; exit 1; }; \
        echo "==> [split] restarting Grafana so it re-provisions datasources from the corrected file"; \
        kubectl -n cerberus rollout restart deployment/grafana; \
        kubectl -n cerberus rollout status deployment/grafana --timeout=120s; \
    fi
    @# cerberus is deployed via its OWN Helm chart so the e2e cluster dogfoods
    @# the published chart (deploy/helm/cerberus) — a chart bug now fails the
    @# dashboard/chaos lanes, not just `chart-validate`'s static lint. The
    @# kustomize apply above already created the `cerberus` namespace.
    @echo "==> installing cerberus via Helm chart (deploy/helm/cerberus, mode={{E2E_MODE}})"
    @if [ "{{E2E_MODE}}" = "split" ]; then \
        helm upgrade --install cerberus deploy/helm/cerberus \
            --namespace cerberus \
            --values test/e2e/k3s/cerberus-values.yaml \
            --values test/e2e/k3s/cerberus-values-split.yaml \
            --wait --timeout 180s; \
        echo "==> [split] exposing the prometheus head as the host:8080 NodePort alias"; \
        kubectl -n cerberus apply -f test/e2e/k3s/cerberus-split-nodeport.yaml; \
    else \
        helm upgrade --install cerberus deploy/helm/cerberus \
            --namespace cerberus \
            --values test/e2e/k3s/cerberus-values.yaml \
            --wait --timeout 180s; \
    fi
    @echo "==> waiting for pods (up to 3 min)"
    kubectl -n cerberus wait --for=condition=Available deployment/clickhouse              --timeout=180s
    @if [ "{{E2E_MODE}}" = "split" ]; then \
        kubectl -n cerberus wait --for=condition=Available deployment/cerberus-prometheus --timeout=180s; \
        kubectl -n cerberus wait --for=condition=Available deployment/cerberus-loki       --timeout=180s; \
        kubectl -n cerberus wait --for=condition=Available deployment/cerberus-tempo      --timeout=180s; \
    else \
        kubectl -n cerberus wait --for=condition=Available deployment/cerberus            --timeout=180s; \
    fi
    kubectl -n cerberus wait --for=condition=Available deployment/grafana                 --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/otel-collector-gateway  --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/sample-app-traces       --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/sample-app-metrics      --timeout=180s
    kubectl -n cerberus wait --for=condition=Available deployment/sample-app-logs         --timeout=180s
    kubectl -n cerberus rollout status daemonset/otel-collector-agent                     --timeout=180s
    @echo "==> e2e-up done (mode={{E2E_MODE}})"
    @echo "    grafana:    http://localhost:3000 (admin/admin)"
    @echo "    cerberus:   http://localhost:8080/healthz"

# Ingest sample OTel data into ClickHouse. Runs the Go seed program at
# test/e2e/seed/cmd/seed/ which (a) applies the upstream OTel-CH DDL via
# internal/schema/ddl.Apply and (b) inserts the deterministic fixture rows.
# The DDL is the source of truth — the schema can no longer drift from the
# upstream exporter, unlike the previous hand-maintained *.sql scripts.
#
# Connects from the host via a transient kubectl port-forward; CH listens on
# port 9000 inside the cluster.
#
# Dual-data-source model (see test/e2e/k3s/README.md):
#   - `e2e-seed` inserts deterministic synthetic rows used by spec tests
#     that need exact values (e.g. `up` metric with known labels).
#   - The OTel collector DaemonSet+gateway+sample-app trio populates real
#     OTel data continuously for realistic Grafana smoke + dashboard tests.
# Both share the same `otel.*` tables (schema cannot drift — both write
# via the upstream sqltemplates).
e2e-seed:
    @echo "==> seeding OTel data via Go seeder"
    @kubectl -n cerberus port-forward svc/clickhouse 19000:9000 > /tmp/cerberus-e2e-seed-pf.log 2>&1 & \
        pf_pid=$!; \
        trap "kill $pf_pid 2>/dev/null || true" EXIT; \
        for i in 1 2 3 4 5 6 7 8 9 10; do \
            if nc -z 127.0.0.1 19000 2>/dev/null; then break; fi; \
            sleep 1; \
        done; \
        CH_ADDR=127.0.0.1:19000 \
        CH_DATABASE=otel \
        CH_USERNAME=cerberus \
        CH_PASSWORD=cerberus \
            go run ./test/e2e/seed/cmd/seed
    @echo "==> seed done"

# One-shot RE-seed through a fresh, self-contained port-forward — used by the
# chaos lane's heal step after a CH-recreating scenario kills + recreates the
# ClickHouse pod. CH's data dir is PVC-backed (test/e2e/k3s/clickhouse.yaml:
# `strategy: Recreate` + a ReadWriteOnce PersistentVolumeClaim on
# /var/lib/clickhouse), so the recreated pod comes back WITH its schema +
# historical data — this re-seed no longer RESTORES lost tables, it RE-ANCHORS
# the rolling metric/log window on now64(9) so the next scenario's time-windowed
# asserts see fresh data. (The rolling seeder's long-lived port-forward,
# `e2e-seed-rolling`, is bound to a single backing pod, so `ch-pod-kill` breaks
# that tunnel; the reconnecting supervisor respawns it once CH is recreated, and
# this one-shot closes the freshness gap until the next rolling tick lands.)
# This recipe stands up its OWN throwaway forward on a DISTINCT local port (so it
# never races the rolling forward's 19000 socket), re-applies the idempotent
# OTel-CH DDL, re-inserts every fixture, verifies the rowcounts, and tears the
# forward down. Idempotent + re-runnable: the seeder's DDL is CREATE … IF NOT
# EXISTS and the INSERTs re-anchor on now64(9), so running it against either an
# empty or an already-populated CH is safe.
e2e-reseed:
    @echo "==> re-seeding OTel data (one-shot, fresh port-forward) after CH recreation"
    @kubectl -n cerberus port-forward svc/clickhouse 19001:9000 > /tmp/cerberus-e2e-reseed-pf.log 2>&1 & \
        pf_pid=$!; \
        trap "kill $pf_pid 2>/dev/null || true" EXIT; \
        for i in 1 2 3 4 5 6 7 8 9 10; do \
            if nc -z 127.0.0.1 19001 2>/dev/null; then break; fi; \
            sleep 1; \
        done; \
        CH_ADDR=127.0.0.1:19001 \
        CH_DATABASE=otel \
        CH_USERNAME=cerberus \
        CH_PASSWORD=cerberus \
            go run ./test/e2e/seed/cmd/seed
    @echo "==> re-seed done"

# Rolling seeder. Performs the same INSERTs as `e2e-seed` and then stays
# alive, re-anchoring the metric/log windows on now64(9) every 30 s until
# stopped via `just e2e-seed-stop` (or SIGTERM). Replaces the static-window
# arms-race that widened the seed envelope to ±15 min in PRs #590 / #615 /
# #617 / #693 just to survive the ~12 min Playwright suite drift — with
# fresh data arriving continuously the static window only has to cover
# the 30 s gap between two ticks plus the 5 m Prom/Loki staleness lookback.
#
# Background pattern: a single bash command builds + launches the seeder
# under nohup, dissociated from this just-recipe shell (so the recipe
# returns and the next CI step can run while the seeder keeps reseeding).
# PID files at /tmp/cerberus-e2e-seed-*.pid let `e2e-seed-stop` find the
# processes for clean teardown. Logs land at /tmp/cerberus-e2e-seed-rolling.log
# so a failing tick is visible in CI artefacts.
#
# Port-forward durability (chaos lane): the forward is run through a
# RECONNECTING supervisor (test/e2e/seed/port_forward_supervisor.sh) under
# `setsid` so it is its own process-group leader. A bare `kubectl
# port-forward` is bound to one backing pod, so the chaos `ch-pod-kill`
# scenario breaks the tunnel and it never reconnects — the seeder then writes
# into a dead socket for the rest of the run, so the rolling window goes stale
# (the PVC keeps CH's historical data, but no fresh tick re-anchors it at now).
# The supervisor respawns the forward when it dies, so once CH is recreated the
# tunnel re-establishes and the 30 s rolling ticks resume (the seeder's
# clickhouse-go pool re-dials a fresh connection per tick). We stash the
# supervisor's PGID so e2e-seed-stop can kill the whole group (supervisor +
# its current kubectl child) atomically. This complements the chaos heal
# step's one-shot `just e2e-reseed`: the one-shot re-anchors the window
# immediately, the supervised rolling feed keeps data anchored at wall-clock now
# for the time-windowed assertions of the scenarios that follow.
e2e-seed-rolling:
    @echo "==> launching rolling seeder (30s tick) in background"
    @# 1) start the reconnecting port-forward supervisor in its own process
    @#    group (setsid) and stash its PGID (== leader PID under setsid) so
    @#    teardown can signal the whole group.
    @setsid bash test/e2e/seed/port_forward_supervisor.sh \
        cerberus svc/clickhouse 19000:9000 > /tmp/cerberus-e2e-seed-pf.log 2>&1 & \
        echo $! > /tmp/cerberus-e2e-seed-pf.pid
    @# 2) wait for the forward to come up.
    @for i in 1 2 3 4 5 6 7 8 9 10; do \
        if nc -z 127.0.0.1 19000 2>/dev/null; then break; fi; \
        sleep 1; \
    done
    @# 3) build the seeder once so `nohup` launches a real binary
    @#    (a stray `go run` keeps the toolchain attached to the shell that
    @#    spawned it; harder to detach cleanly across CI step boundaries).
    @go build -o /tmp/cerberus-e2e-seeder ./test/e2e/seed/cmd/seed
    @# 4) launch the seeder under nohup with the rolling flag.
    @CH_ADDR=127.0.0.1:19000 \
        CH_DATABASE=otel \
        CH_USERNAME=cerberus \
        CH_PASSWORD=cerberus \
        nohup /tmp/cerberus-e2e-seeder --re-seed-interval=30s \
            > /tmp/cerberus-e2e-seed-rolling.log 2>&1 & \
        echo $! > /tmp/cerberus-e2e-seed-rolling.pid
    @echo "==> rolling seeder pid=$(cat /tmp/cerberus-e2e-seed-rolling.pid) pf-pid=$(cat /tmp/cerberus-e2e-seed-pf.pid)"
    @echo "    initial seed runs synchronously inside the seeder before the loop starts —"
    @echo "    tail /tmp/cerberus-e2e-seed-rolling.log to confirm 'seed: done' lands."
    @# 5) wait for the initial seed to land before returning, so the next
    @#    CI step (e2e-wait-otel / e2e-run) sees a populated database.
    @for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do \
        if grep -q '^.*seed: done' /tmp/cerberus-e2e-seed-rolling.log 2>/dev/null; then \
            echo "==> initial seed landed"; \
            exit 0; \
        fi; \
        sleep 2; \
    done; \
        echo "==> ERROR: initial seed did not complete within 30s. log:"; \
        cat /tmp/cerberus-e2e-seed-rolling.log; \
        exit 1

# Stop the rolling seeder + its port-forward supervisor (idempotent).
# Called from CI teardown so the dashboard job tears down cleanly even when
# the Playwright step failed before reaching `e2e-down`. SIGTERM gives the
# seeder a chance to log the exit reason; the port-forward never has any
# state to flush. The port-forward PID file holds the supervisor's setsid
# leader PID (== its PGID), so `kill -TERM -<pid>` signals the whole group —
# the supervisor AND its current kubectl child — instead of orphaning the
# kubectl child while the supervisor respawns it.
e2e-seed-stop:
    @echo "==> stopping rolling seeder"
    @if [ -f /tmp/cerberus-e2e-seed-rolling.pid ]; then \
        pid=$(cat /tmp/cerberus-e2e-seed-rolling.pid); \
        kill -TERM "$pid" 2>/dev/null || true; \
        rm -f /tmp/cerberus-e2e-seed-rolling.pid; \
    fi
    @if [ -f /tmp/cerberus-e2e-seed-pf.pid ]; then \
        pid=$(cat /tmp/cerberus-e2e-seed-pf.pid); \
        kill -TERM -"$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true; \
        rm -f /tmp/cerberus-e2e-seed-pf.pid; \
    fi

# Wait until the OTel collector has populated real data in every signal
# table (logs / traces / one of the metrics tables) AND the metric table
# carries ≥60 s of history. Bootstraps the pipeline before tests rely on
# it — telemetrygen + kubeletstats take ~30-60s to flush a first batch
# through the gateway, and 1m-windowed queries (rate(x[1m]), up[1m:30s])
# need a 60s span of TimeUnix values before they return a vector.
#
# Polls every 5s for up to 3 min; fails the recipe if any signal stays
# empty or the metric stream never reaches 60 s of spread. Uses
# `kubectl exec` against the ClickHouse pod so it does not need a
# host-side port-forward. Spread is asserted on whichever metric table
# (sum or gauge) carries non-zero rows first.
e2e-wait-otel:
    @echo "==> waiting for real OTel data (incl. clickhouse query_log stream) + ≥60s metric history in ClickHouse"
    @deadline=$(($(date +%s) + 180)); \
        while [ $(date +%s) -lt $deadline ]; do \
            logs=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_logs" 2>/dev/null || echo 0); \
            chlogs=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_logs WHERE ServiceName = 'clickhouse'" 2>/dev/null || echo 0); \
            traces=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_traces" 2>/dev/null || echo 0); \
            sum=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_metrics_sum" 2>/dev/null || echo 0); \
            gauge=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_metrics_gauge" 2>/dev/null || echo 0); \
            histogram=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                --user cerberus --password cerberus --database otel \
                --query "SELECT count() FROM otel_metrics_histogram WHERE MetricName = 'http_server_request_duration'" 2>/dev/null || echo 0); \
            hist_spread=0; \
            if [ "$histogram" -gt 0 ]; then \
                hist_spread=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                    --user cerberus --password cerberus --database otel \
                    --query "SELECT toUInt64(dateDiff('second', min(TimeUnix), max(TimeUnix))) FROM otel_metrics_histogram WHERE MetricName = 'http_server_request_duration'" 2>/dev/null || echo 0); \
            fi; \
            spread=0; \
            if [ "$sum" -gt 0 ]; then \
                spread=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                    --user cerberus --password cerberus --database otel \
                    --query "SELECT toUInt64(dateDiff('second', min(TimeUnix), max(TimeUnix))) FROM otel_metrics_sum" 2>/dev/null || echo 0); \
            elif [ "$gauge" -gt 0 ]; then \
                spread=$(kubectl -n cerberus exec deploy/clickhouse -- clickhouse-client \
                    --user cerberus --password cerberus --database otel \
                    --query "SELECT toUInt64(dateDiff('second', min(TimeUnix), max(TimeUnix))) FROM otel_metrics_gauge" 2>/dev/null || echo 0); \
            fi; \
            echo "    logs=$logs chlogs=$chlogs traces=$traces metrics_sum=$sum metrics_gauge=$gauge metrics_histogram=$histogram spread=${spread}s hist_spread=${hist_spread}s"; \
            if [ "$logs" -gt 0 ] && [ "$chlogs" -gt 0 ] && [ "$traces" -gt 0 ] && { [ "$sum" -gt 0 ] || [ "$gauge" -gt 0 ]; } && [ "$spread" -ge 60 ] && [ "$histogram" -gt 0 ] && [ "$hist_spread" -ge 60 ]; then \
                echo "==> OTel pipeline is live with ≥60s of metric history (incl. histogram companion + clickhouse query_log stream)"; \
                exit 0; \
            fi; \
            sleep 5; \
        done; \
        echo "==> timeout waiting for OTel data / metric history span"; \
        exit 1

# Run Go E2E HTTP tests against the deployed stack.
e2e-run:
    @echo "==> running Go E2E tests"
    go test -tags=e2e ./test/e2e/...

# A godog suite driving `cerberus migrate` over committed fixtures — no
# Docker, no backend, seconds.
# Run the Tier-0 offline migration scenarios.
migration-tier0:
    @echo "==> running the tier-0 migration scenarios"
    go test -tags=migration ./test/e2e/migration/tiers/tier0-offline/... -count=1 -v

# Enumerate the Gherkin features into the JSON the coverage ratchet reads.
migration-scenarios out="build/migration-scenarios.json":
    @echo "==> enumerating migration scenarios into {{out}}"
    mkdir -p build
    go run ./test/e2e/migration/cmd/scenarios --out {{out}}

# A golden is a reviewed artifact, so this refuses to run under CI; diff
# every regenerated byte against the fixture's stated derivation.
# Regenerate the Tier-0 migration goldens from the current tool behaviour.
migration-golden:
    @echo "==> regenerating the tier-0 migration goldens"
    MIGRATION_UPDATE_GOLDENS=1 go test -tags=migration ./test/e2e/migration/tiers/tier0-offline/... -count=1

# Run the startup-speed benchmark: spawn cerberus and measure wall-clock
# time from process-start to first 200 OK on /healthz. Asserts < 2.5 s.
# Requires a reachable ClickHouse at $CH_ADDR (default 127.0.0.1:9000);
# the benchmark sets CERBERUS_AUTO_CREATE_SCHEMA=false so we measure
# pure HTTP-listener bootstrap and not DDL apply time.
#
# Override with CH_ADDR / CH_DATABASE / CH_USERNAME / CH_PASSWORD env
# vars; see test/e2e/startup_bench_test.go for the full list.
startup-bench:
    @echo "==> startup-speed benchmark (target < 2 s to /healthz)"
    go test -tags=startup_bench -v -count=1 -run TestStartupSpeed ./test/e2e/...

# Run the Grafana playwright smoke (lands in M0.2).
e2e-playwright:
    @echo "==> playwright smoke (lands in M0.2)"
    @if [ -d test/e2e/playwright ]; then \
        cd test/e2e/playwright && npm ci && npx playwright test; \
    else \
        echo "    (no playwright suite yet — landing in M0.2)"; \
    fi

# Apply the chaos overlay onto the running cerberus Deployment: the
# resilience knobs in test/e2e/chaos/manifests/chaos-overlay.env (low
# breaker threshold + small CERBERUS_QUERY_TIMEOUT + small admit/pool
# caps) so the live-stack chaos faults trip FAST + DETERMINISTICALLY
# within budget. Patches the Deployment's pod env via `kubectl set env`
# (one rollout), then waits for it to roll out so every cerberus pod
# carries the overlay before fault injection. Idempotent — re-applying
# the same env values is a no-op rollout.
e2e-chaos-overlay:
    @echo "==> applying chaos overlay (resilience knobs) to deploy/cerberus"
    @env_args=""; \
        while IFS= read -r line; do \
            case "$line" in ''|\#*) continue ;; esac; \
            env_args="$env_args $line"; \
        done < test/e2e/chaos/manifests/chaos-overlay.env; \
        echo "    kubectl set env deploy/cerberus$env_args"; \
        kubectl -n cerberus set env deployment/cerberus $env_args
    @echo "==> waiting for the overlay rollout"
    kubectl -n cerberus rollout status deployment/cerberus --timeout=120s

# Run the live-stack chaos lane: fault-inject against the running k3d
# stack and assert the gateway's resilience contracts (circuit breaker,
# per-query wall-clock timeout, admission control, replica resilience)
# hold under REAL faults. Drives .github/scripts/chaos-run.mjs (node ESM,
# kubectl + fetch). Assumes `e2e-up` + `e2e-seed-rolling` + `e2e-wait-otel`
# already ran AND the chaos overlay is applied (`e2e-chaos-overlay`).
# CHAOS_PHASE=phase-1 by default (ch-pod-kill, ch-slow/query-timeout,
# cerberus-pod-kill); CHAOS_PHASE=all adds the phase-2 scenarios.
# Mirrors the `e2e-run` / `e2e-playwright` shape — locally reproducible.
e2e-chaos:
    @echo "==> running live-stack chaos lane (chaos-run.mjs)"
    CERBERUS_URL=http://localhost:8080 \
        CHAOS_PHASE="${CHAOS_PHASE:-phase-1}" \
        node .github/scripts/chaos-run.mjs

# Tear down the cluster. Also stops the rolling seeder + port-forward
# if either was started by `e2e-seed-rolling` — idempotent and silent
# when the PID files don't exist.
e2e-down:
    @if [ -f /tmp/cerberus-e2e-seed-rolling.pid ] || [ -f /tmp/cerberus-e2e-seed-pf.pid ]; then \
        just e2e-seed-stop; \
    fi
    @if k3d cluster list | grep -q "^{{K3D_CLUSTER}} "; then \
        echo "==> deleting k3d cluster {{K3D_CLUSTER}}"; \
        k3d cluster delete {{K3D_CLUSTER}}; \
    fi



# Full lifecycle. Seed first (deterministic rows, rolling so the window
# slides with wall-clock now), then wait for the collector to populate
# real OTel data, then run the test matrix. `e2e-down` stops the rolling
# seeder on teardown.
e2e: e2e-up e2e-seed-rolling e2e-wait-otel e2e-run e2e-playwright e2e-down

# === E2E BWC (bundled ClickHouse on object storage, MinIO-backed) ===
#
# Additive sibling of the `e2e-up` lane that proves the Helm chart's
# `clickhouse.bundled.enabled=true` data tier on REAL object storage:
# cerberus + a chart-deployed ClickHouse StatefulSet whose MergeTree storage
# policy targets an in-cluster MinIO bucket. Same k3d cluster name, same
# cerberus:e2e image, same otel.* tables on clickhouse:9000 — so the seed +
# Go-test recipes (`e2e-seed-rolling`, `e2e-run`) are REUSED UNCHANGED.
#
# Ordering is load-bearing:
#   1. MinIO + the bucket-create Job come up FIRST — a ClickHouse `s3` disk does
#      not create its bucket, so it must exist before CH starts (gotcha #3).
#   2. `helm install` then brings up the bundled CH + cerberus. cerberus runs
#      its auto-create DDL stamping storage_policy=bwc_object_store BEFORE
#      serving, and `--wait` blocks until that is done.
#   3. ONLY THEN do the otel collector / grafana / sample-app fixtures land — so
#      the collector's clickhouseexporter (create_schema=true) can never win a
#      race to create the otel tables UNSTAMPED on the local disk; it finds
#      cerberus's stamped tables already present and its CREATE … IF NOT EXISTS
#      is a no-op.
e2e-bwc-up: e2e-down
    @echo "==> [bwc] pre-pulling k3s node image (retry — Docker Hub flaky from CI)"
    @just _pull-retry {{K3S_IMAGE}}
    @echo "==> [bwc] creating k3d cluster {{K3D_CLUSTER}}"
    k3d cluster create {{K3D_CLUSTER}} \
        --image {{K3S_IMAGE}} \
        --port "3000:30030@loadbalancer" \
        --port "8080:30080@loadbalancer" \
        --no-lb=false \
        --k3s-arg "--disable=traefik@server:0" \
        {{K3D_EXTRA_ARGS}} \
        --wait
    @echo "==> [bwc] building cerberus image (build tags: '{{CERBERUS_BUILD_TAGS}}')"
    node .github/scripts/build-with-registry-retry.mjs \
        docker build -t {{CERBERUS_IMAGE}} --build-arg GO_BUILD_TAGS="{{CERBERUS_BUILD_TAGS}}" -f Dockerfile.local .
    @echo "==> [bwc] pre-pulling external + bwc images on host docker"
    @# The standalone-CH image (the *-alpine tag in E2E_EXTERNAL_IMAGES) backs
    @# test/e2e/k3s/clickhouse.yaml, which the bwc kustomization EXCLUDES — this
    @# lane runs the chart's BUNDLED ClickHouse (the non-alpine tag in
    @# E2E_BWC_IMAGES) instead, so skip importing the unused standalone image.
    @for img in {{E2E_EXTERNAL_IMAGES}} {{E2E_BWC_IMAGES}}; do \
        case "$img" in clickhouse/clickhouse-server:*-alpine) continue ;; esac; \
        just _pull-retry "$img"; \
    done
    @# Import each image individually + VERIFY it landed in the node's
    @# containerd (k3d image import can print success while the node import
    @# silently failed -> ImagePullBackOff with pullPolicy:Never). Same robust
    @# loop as `e2e-up`, extended with the bwc image set.
    @for img in {{CERBERUS_IMAGE}} {{E2E_EXTERNAL_IMAGES}} {{E2E_BWC_IMAGES}}; do \
        case "$img" in clickhouse/clickhouse-server:*-alpine) continue ;; esac; \
        ref="$img"; \
        case "$ref" in \
            *.*/*|*:*/*) ;; \
            */*) ref="docker.io/$ref" ;; \
            *) ref="docker.io/library/$ref" ;; \
        esac; \
        landed=0; \
        for attempt in 1 2 3 4 5; do \
            k3d image import "$img" -c {{K3D_CLUSTER}} || true; \
            if docker exec k3d-{{K3D_CLUSTER}}-server-0 ctr -n k8s.io images ls -q | grep -qF "$ref"; then \
                landed=1; break; \
            fi; \
            echo "  import attempt $attempt: $ref not in containerd yet, retrying with backoff" >&2; \
            sleep $((attempt * 2)); \
        done; \
        if [ "$landed" != "1" ]; then \
            echo "ERROR: $ref missing from k3d node containerd after 5 import attempts" >&2; \
            exit 1; \
        fi; \
    done
    @echo "==> [bwc] phase 1: namespace + MinIO + bucket-create Job (before ClickHouse)"
    kubectl apply -f test/e2e/k3s/namespace.yaml
    kubectl apply -f test/e2e/k3s-bwc/minio.yaml -f test/e2e/k3s-bwc/bucket-job.yaml
    @echo "==> [bwc] waiting for MinIO"
    kubectl -n cerberus rollout status deployment/minio --timeout=120s
    @echo "==> [bwc] waiting for the bucket-create Job to complete"
    kubectl -n cerberus wait --for=condition=complete job/minio-create-bucket --timeout=120s
    @echo "==> [bwc] phase 2: installing cerberus + bundled ClickHouse via Helm (object storage)"
    helm upgrade --install cerberus deploy/helm/cerberus \
        --namespace cerberus \
        --values test/e2e/k3s/cerberus-values.yaml \
        --values test/e2e/k3s/cerberus-values-bwc.yaml \
        --wait --timeout 360s
    @echo "==> [bwc] waiting for bundled ClickHouse StatefulSet + cerberus"
    kubectl -n cerberus rollout status statefulset/cerberus-clickhouse --timeout=300s
    kubectl -n cerberus rollout status deployment/cerberus --timeout=180s
    @echo "==> [bwc] phase 3: applying grafana / collector / sample-app fixtures + clickhouse alias"
    kubectl kustomize --load-restrictor=LoadRestrictionsNone test/e2e/k3s-bwc | kubectl apply -f -
    @echo "==> [bwc] waiting for grafana"
    kubectl -n cerberus rollout status deployment/grafana --timeout=180s
    @echo "==> e2e-bwc-up done (bundled ClickHouse on MinIO object storage)"

# Assert the data tier actually lives on object storage: storage_policy stamped
# on every MergeTree table, active parts on the object/cache disk (not the local
# `default` disk), and the MinIO bucket non-empty after the seed. Run AFTER
# `just e2e-seed-rolling`. Logic lives in the env-driven Node module (per the
# CLAUDE.md "non-trivial step logic in .github/scripts/*.mjs" rule), invoked
# with the pinned mc image so the in-cluster bucket-ls pod matches the lane.
e2e-bwc-verify:
    @echo "==> [bwc] verifying object-storage placement"
    MC_IMAGE="minio/mc:RELEASE.2025-08-13T08-35-41Z" \
        node .github/scripts/e2e-bwc-verify-placement.mjs

# Tear down the bwc lane. Same cluster name + rolling seeder as the standard
# lane, so the standard teardown covers it exactly.
e2e-bwc-down: e2e-down

# Run the compose-stack Grafana catch-net spec locally. Assumes the
# quickstart compose stack is already up (`docker compose up --wait`).
# Drives Grafana through every provisioned dashboard and asserts every
# /api/ds/query + /api/dashboards/* response is 2xx with no tunneled
# per-target error. Mirrors the Playwright step the compose-smoke CI
# job runs.
compose-grafana-smoke:
    @echo "==> compose-grafana-smoke playwright catch-net"
    cd test/e2e/playwright && \
        ( [ -f package-lock.json ] && npm ci || npm install --no-audit --no-fund ) && \
        npx playwright install --with-deps chromium && \
        GRAFANA_BASE_URL=http://localhost:3000 \
        GRAFANA_URL=http://localhost:3000 \
        CERBERUS_URL=http://localhost:8080 \
        npx playwright test compose_grafana_smoke.spec.ts --reporter=list

# === Compatibility (prometheus/compliance differential harness) ===

# Run the PromQL compatibility suite end-to-end. Slow; expect minutes.
# Sets up the Docker Compose stack (reference Prom + cerberus + CH + seeder),
# runs the upstream tester, writes compatibility/prometheus/report.json.
compat-promql:
    ./compatibility/prometheus/scripts/run-compatibility.sh

# Keep the compatibility stack running after the tester finishes (for debugging).
compat-promql-keep:
    COMPOSE_KEEP=1 ./compatibility/prometheus/scripts/run-compatibility.sh

# Tear down the compatibility stack manually.
compat-promql-down:
    cd compatibility/prometheus && docker compose down -v

# === Compatibility (LogQL — Loki compatibility harness) ===

# Run the LogQL compatibility harness end-to-end. Brings up reference
# Loki + cerberus + ClickHouse, seeds both, builds the diff driver from
# the vendored upstream/loki-bench corpus, runs TestRemoteStorageEquality
# against both endpoints, writes compatibility/loki/reports/diff.json.
# See compatibility/loki/README.md for the harness layout.
compat-logql:
    ./compatibility/loki/scripts/run-loki-compatibility.sh

# Run the smoke (compose + seed + /labels assertion) without the diff
# driver. Useful when the seeder is the bisect target.
compat-logql-smoke:
    DRIVER_SKIP=1 ./compatibility/loki/scripts/run-loki-compatibility.sh

# Keep the Loki compatibility stack running after the run finishes
# (for debugging /loki/api/v1/* + ClickHouse manually).
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
compat-all: compat-promql compat-logql compat-traceql

# === Migration lane — Tier-1 dual-backend substrate (Layer 14) ===
#
# The pinned reference stack `cerberus migrate verify` replays a harvested
# corpus against: reference Prometheus + Loki + Tempo alongside one ClickHouse,
# one OTel collector (the sole schema authority) and one cerberus serving all
# three heads. Explicit lifecycle verbs plus a composite, mirroring the e2e-*
# shape; the `tier1` prefix leaves room for the ruler tier without renaming.

# The image tag the migration stacks run when no released artifact is supplied.
# Held equal to the `${CERBERUS_IMAGE:-…}` default in
# tiers/tier1-dual/docker-compose.dual.yml by
# test/regression/migration_tier1_test.go, so the recipe below and the compose
# file cannot disagree about which tag "locally built" means. All three spell
# the suffix interpolation and expand it in their own runtime — the recipe's
# shell here, compose's interpolation there — so the tag stays one string.
#
# The suffix matters more here than anywhere else in the tree: this lane's `up`
# recipes carry no `--build` and the service declares `pull_policy: never`, so
# the tag is resolved a whole pull-retry loop after it is written. A tag shared
# with a second checkout would let that checkout's build land in between, and
# the lane would report on a binary from another tree.
MIGRATION_LOCAL_IMAGE := "cerberus:migration-tier1${COMPOSE_PROJECT_SUFFIX:-}"

# Put the cerberus image the migration stacks run into the local Docker daemon —
# the SINGLE acquisition point, shared by CI and local dev.
#
# Two paths, decided by CERBERUS_IMAGE alone: unset (every local run, every PR /
# dispatch / push CI run) it builds MIGRATION_LOCAL_IMAGE from the repo-root
# Dockerfile.local — whose final unnamed stage IS the cerberus image, so no
# `--target` is needed — and the stack proves this tree. Set (release.yml's
# artifact lane) it pulls that released tag and the stack proves the artifact.
#
# This exists because the compose up path deliberately cannot acquire the image
# itself: `pull_policy: never` and no `--build` on the up recipes are what keep
# a release run from recompiling the source tree over the released image.
migration-cerberus-image:
    @local_tag="{{MIGRATION_LOCAL_IMAGE}}"; \
    img="${CERBERUS_IMAGE:-$local_tag}"; \
    if [ "$img" = "$local_tag" ]; then \
        echo "==> migration cerberus image: build $img from Dockerfile.local"; \
        node .github/scripts/build-with-registry-retry.mjs docker build -f Dockerfile.local -t "$img" .; \
    else \
        echo "==> migration cerberus image: pull $img"; \
        just _pull-retry "$img"; \
    fi

# Bring the Tier-1 stack up and wait for every healthcheck to pass. The cerberus
# image is acquired first by migration-cerberus-image; there is deliberately no
# `--build` here, because `--build` forces a source build regardless of
# `pull_policy` and that is precisely how a released image gets silently
# replaced by a recompile of whatever tree the runner has checked out.
# The teardown is a sub-invocation rather than a `migration-tier1-up:
# migration-tier1-down` dependency on purpose: just runs a recipe at most once
# per invocation, so declaring it as a dependency would consume the trailing
# `migration-tier1-down` of the `migration-tier1` composite and leave the stack
# running after a clean lane. Seeding an already-seeded stack is the failure
# this closes — a run aborted by a failing assertion never reaches teardown,
# and a second seed into the same live window collides on every sample instant.
# migration-cerberus-image is a sub-invocation for the same reason.
migration-tier1-up:
    @just migration-tier1-down
    @just migration-cerberus-image
    @just _compose-pull-retry test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml
    @echo "==> migration tier-1 stack up"
    node .github/scripts/build-with-registry-retry.mjs \
        docker compose -f test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
        up --wait --wait-timeout 300

# Every archetype an `@tier1` scenario actually reads fixture data from:
# MIG-02/06/07/08 read kube-prometheus-stack, every other `@tier1` scenario
# reads three-signal (see the `@archetype:` tags in
# test/e2e/migration/features/*.feature). migration-tier1-seed's empty
# default seeds all of them into ONE compose-stack lifecycle — bringing the
# stack up twice in one CI run just to seed a second archetype costs another
# full rebuild + boot + healthcheck wait for no reason.
MIGRATION_TIER1_ARCHETYPES := "three-signal kube-prometheus-stack"

# The services each migration-tier*-logs recipe dumps, and how many trailing
# lines it quotes per service. The Tier-2 list is Tier-1's plus the ruler leg,
# spelled out rather than derived from `compose config --services` so a service
# silently dropped from a compose file shows up as a missing dump rather than a
# quietly shorter one. 200 lines carries a boot failure's stack or a Grafana
# provisioning rejection while keeping ten services readable in a job log.
MIGRATION_TIER1_SERVICES := "clickhouse otel-collector prometheus loki tempo cerberus"
MIGRATION_TIER2_SERVICES := MIGRATION_TIER1_SERVICES + " grafana relay-prom otel-collector-writeback dead-end-receiver"
MIGRATION_LOG_TAIL := "200"

# Archetypes sharing one live stack are kept apart by their IDENTITIES, not by
# their windows: every archetype in MIGRATION_TIER1_ARCHETYPES declares trace
# service names, metric names and a log job that no other one declares, so all
# of them can occupy the same fixture window without any search seeing another
# archetype's data. test/regression/migration_tier1_test.go enforces that
# pairwise disjointness, so adding a ninth archetype that reuses a name fails
# the required `check` lane rather than a 45-minute Docker job.
#
# Pushing each archetype into its own earlier window (cmd/seed's
# --window-offset) was the previous mechanism and is arithmetically impossible
# for more than one archetype: disjoint windows need an offset of at least
# seed.SeedWindow (30m), and reference Tempo's 1h wal.ingestion_time_range_slack
# rejects any offset where offset + SeedWindow >= 1h. Every value satisfying one
# constraint violates the other, so the second archetype could never seed. The
# flag remains on cmd/seed — it is a real capability with a correct guard — but
# nothing here uses it.

# Load the deterministic all-signal fixture into the running stack: one
# in-memory fixture per archetype, each written twice — directly into
# ClickHouse and into the reference Prometheus / Loki / Tempo — so both sides
# of a parity diff are comparable by construction. Publishes one manifest per
# archetype: three-signal's stays at the historical unsuffixed
# `manifest.json` path (the plain-Go substrate self-check in
# tier1_parity_test.go hardcodes that exact path), every other archetype gets
# `manifest-<archetype>.json`. A Tier-1 scenario tagged `@archetype:<name>`
# reads exactly that archetype's manifest (test/e2e/migration/lib/live.go's
# LoadManifest), so it always sees its own window and metric names.
#
# `archetype` seeds ONLY that one archetype — for iterating on a single story
# locally once the stack is already up, e.g.
# `just migration-tier1-seed kube-prometheus-stack`. Its empty default seeds
# every archetype MIGRATION_TIER1_ARCHETYPES lists, which is what the
# canonical `just migration-tier1` composite (and the CI migration-tier1 job)
# drives — the whole `@tier1` scenario set is green off one seed pass, not
# just whichever archetype happened to be seeded last.
migration-tier1-seed archetype="":
    @archetypes="{{archetype}}"; \
    [ -n "$archetypes" ] || archetypes="{{MIGRATION_TIER1_ARCHETYPES}}"; \
    for a in $archetypes; do \
        manifest="test/e2e/migration/.out/manifest.json"; \
        [ "$a" = "three-signal" ] || manifest="test/e2e/migration/.out/manifest-$a.json"; \
        echo "==> migration tier-1 seed ($a, manifest $manifest)"; \
        go run ./test/e2e/migration/cmd/seed \
            --fixture test/e2e/migration/archetypes/$a/seed/fixture.json \
            --manifest "$manifest"; \
    done

# Assert the Tier-1 substrate contract against the seeded stack: the collector
# provisioned the OTel schema, cerberus serves all three heads off it, each
# reference backend returns exactly what was written to it, both sides hold the
# same telemetry over the manifest window, and a deliberately injected
# disagreement is observed.
#
# Two SEPARATE `go test` invocations, in this exact order, not one
# `./test/e2e/migration/...` sweep: the root package's negative control
# (`TestTier1Parity/negative_control_the_lane_can_see_disagreement`)
# deliberately and PERMANENTLY corrupts ClickHouse with an extra gauge series
# to prove drift is observable — see test/e2e/migration/seed/drift.go. Nothing
# undoes that injection, and go test gives no ordering guarantee across
# packages in one invocation, so a single sweep makes the Gherkin corpus
# replay's diverge-zero assertion order-dependent: it must read the seeded
# stack before the negative control ever touches it, never after.
migration-tier1-run:
    @echo "==> migration tier-1 dual-backend Gherkin scenarios"
    go test -tags=migration_tier1 -count=1 ./test/e2e/migration/tiers/tier1-dual/...
    @echo "==> migration tier-1 substrate + parity assertions"
    go test -tags=migration_tier1 -count=1 ./test/e2e/migration/

# Dump the Tier-1 stack's container state and per-service log tail on demand.
# The CI job runs this on failure BEFORE teardown, so a red run carries the
# evidence instead of a bare assertion message; teardown then deletes the
# containers this reads from, which is why that ordering matters. Every command
# is `|| true`-guarded: a dump must never turn a lane red on its own, nor mask
# the real failure with a container that has already exited.
migration-tier1-logs:
    @echo "==> migration tier-1 compose state"; \
    docker compose -f test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml ps || true; \
    for svc in {{MIGRATION_TIER1_SERVICES}}; do \
        echo "==> migration tier-1 logs: $svc"; \
        docker compose -f test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
            logs --tail={{MIGRATION_LOG_TAIL}} "$svc" || true; \
    done

# Tear the Tier-1 stack down. `-v` is mandatory, not cosmetic: the reference
# images declare their own VOLUMEs, and a surviving volume would carry one
# run's data into the next.
migration-tier1-down:
    @echo "==> migration tier-1 stack down"
    docker compose -f test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
        down -v --remove-orphans

# Full Tier-1 lifecycle. Fails fast on the first non-zero recipe.
migration-tier1: migration-tier1-up migration-tier1-seed migration-tier1-run migration-tier1-down

# === Migration lane — Tier-2 ruler substrate (Layer 14) ===
#
# The Tier-1 stack plus a real query-only external ruler (Grafana-managed
# alerting) pointed at cerberus, and the dead-end webhook receiver its one
# contact point routes to. Mirrors the Tier-1 lifecycle shape exactly — see
# the comments on the migration-tier1-* recipes above for the rationale each
# one shares with its tier2 counterpart.

# Bring the Tier-2 stack up: Tier-1's compose file plus tier2-ruler's, merged
# via the standard multi-`-f` invocation so both sets of services land in the
# SAME compose project (docker-compose.ruler.yml deliberately declares no
# `name:` of its own — see that file's header comment).
#
# Same image contract as migration-tier1-up: acquire once via
# migration-cerberus-image, never `--build`. The ruler tier adds services on top
# of Tier-1's `cerberus`, it does not declare a second one, so this stack runs
# exactly the image that recipe put in the daemon.
migration-tier2-up:
    @just migration-tier2-down
    @just migration-cerberus-image
    @just _compose-pull-retry test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
        test/e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml
    @echo "==> migration tier-2 stack up"
    node .github/scripts/build-with-registry-retry.mjs \
        docker compose -f test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
        -f test/e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml \
        up --wait --wait-timeout 300

# Tier-2 runs against the same seeded window Tier-1 does — the ruler tier
# adds Grafana-managed alerting on top of the SAME cerberus/ClickHouse pair,
# not a second one — so this is the identical seed step, not a second corpus.
migration-tier2-seed:
    @echo "==> migration tier-2 seed"
    go run ./test/e2e/migration/cmd/seed \
        --fixture test/e2e/migration/archetypes/three-signal/seed/fixture.json \
        --manifest test/e2e/migration/.out/manifest.json

# Assert the Tier-2 substrate contract: Grafana provisioned the rule fixture
# (kube-prometheus-stack's recording rule + NodeCPUSaturation alert, plus the
# MIG-18 fire/resolve probe) without error, its alert rules evaluate against
# cerberus for real, and a fired notification actually reaches the dead-end
# receiver.
#
# Two SEPARATE `go test` invocations, in this exact order, not one
# `./test/e2e/migration/...` sweep — the same reason migration-tier1-run is
# split, with a different shared resource. Both packages carry the
# migration_tier2 tag and both drive the ONE dead-end receiver, and each
# asserts a notification arrived by reading the receiver's count before its own
# trigger and polling for a rise. Run concurrently (go test's default is one
# process per package) those two deltas observe each other's notifications, so
# each can be satisfied by the other's traffic. Sequencing them makes each
# delta bracket only its own trigger. Gherkin first, substrate second, mirroring
# migration-tier1-run: the Gherkin corpus reads the receiver's captured stream
# for MIG-18's fire/resolve edges, and the substrate self-check's synthetic
# test notification is one more entry in that stream.
migration-tier2-run:
    @echo "==> migration tier-2 ruler Gherkin scenarios"
    go test -tags=migration_tier2 -count=1 ./test/e2e/migration/tiers/tier2-ruler/...
    @echo "==> migration tier-2 substrate assertions"
    go test -tags=migration_tier2 -count=1 ./test/e2e/migration/

# Dump the Tier-2 stack's container state and per-service log tail — the ruler
# leg included, since a Tier-2 failure is usually Grafana rejecting the rule
# fixture or the write-back bridge dropping a sample. See
# migration-tier1-logs' comment for the ordering-before-teardown rationale.
migration-tier2-logs:
    @echo "==> migration tier-2 compose state"; \
    docker compose -f test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
        -f test/e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml ps || true; \
    for svc in {{MIGRATION_TIER2_SERVICES}}; do \
        echo "==> migration tier-2 logs: $svc"; \
        docker compose -f test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
            -f test/e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml \
            logs --tail={{MIGRATION_LOG_TAIL}} "$svc" || true; \
    done

# Tear the Tier-2 stack down. `-v` is mandatory — see migration-tier1-down's
# comment; the same reference-image VOLUME concern applies here since this
# compose invocation includes the Tier-1 file too.
migration-tier2-down:
    @echo "==> migration tier-2 stack down"
    docker compose -f test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml \
        -f test/e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml \
        down -v --remove-orphans

# Full Tier-2 lifecycle. Fails fast on the first non-zero recipe.
migration-tier2: migration-tier2-up migration-tier2-seed migration-tier2-run migration-tier2-down

# === Release (controlled local cut) ===
#
# These mirror prepare-release.yml for when the workflow_dispatch isn't
# available, and — critically — ENFORCE the canonical release-branch names
# so nobody hand-rolls an inconsistent one:
#   - tip-of-main release PR : release/v<version>-chart-<chartVersion>
#   - maintenance backport   : release/<major>.<minor>.x   (publishing is
#     gated on merge of a validated release PR — resolve-release-trigger.mjs
#     + release-version-gate.mjs key off this branch name, so a wrong one
#     makes the publish step REJECT the run)
# Both wrap the same .github/scripts/prepare-release.mjs + helm-docs the CI
# uses, so the opened PR is drift-clean.

# Stage a tip-of-main release and open its PR on the canonical branch.
# Usage: just release-prep 1.4.0            (chart bump defaults to patch)
#        just release-prep 1.5.0 minor
release-prep version chart_bump="patch":
    #!/usr/bin/env bash
    set -euo pipefail
    git fetch origin main
    git switch -c "release/v{{version}}-staging" origin/main
    VERSION="{{version}}" CHART_BUMP="{{chart_bump}}" PR_BODY_FILE=release-pr-body.md \
        node .github/scripts/prepare-release.mjs
    chart="$(awk '/^version:/{print $2; exit}' deploy/helm/cerberus/Chart.yaml)"
    docker run --rm -v "$PWD/deploy/helm:/helm-docs" -u "$(id -u)" \
        jnorwood/helm-docs:v1.14.2 --chart-search-root=/helm-docs --template-files=README.md.gotmpl
    branch="release/v{{version}}-chart-${chart}"
    git branch -m "$branch"
    git add deploy/helm/cerberus/Chart.yaml deploy/helm/cerberus/README.md CHANGELOG.md
    git commit -m "chore(release): cerberus v{{version}} / chart ${chart}"
    git push -u origin "$branch"
    gh pr create --base main --head "$branch" \
        --title "chore(release): cerberus v{{version}} / chart ${chart}" \
        --body-file release-pr-body.md
    echo "Opened release PR on $branch. MERGING it publishes — release.yml runs on push-to-main, gates on the staged version bumps, and creates v{{version}} itself. Do not tag by hand."

# Create/switch to the release/<major>.<minor>.x maintenance line off the
# latest matching tag, ready for cherry-picking fix: commits to backport.
# Usage: just backport-line 1.3   then  git cherry-pick <sha>...  then
#        just release-prep-backport 1.3.2
backport-line minor:
    #!/usr/bin/env bash
    set -euo pipefail
    git fetch origin --tags --quiet
    base="$(git tag --list 'v{{minor}}.*' --sort=-v:refname | head -1)"
    test -n "$base" || { echo "no v{{minor}}.* tag to branch from"; exit 1; }
    git switch "release/{{minor}}.x" 2>/dev/null || git switch -c "release/{{minor}}.x" "$base"
    echo "On release/{{minor}}.x (off $base). Cherry-pick the fix commits, then: just release-prep-backport <{{minor}}.Z>"

# Stage a backport release on the current release/X.Y.x branch (after the
# fix: commits are cherry-picked) and push the branch. Pushing IS the trigger:
# release.yml runs on `release/*.x` pushes, gates on the staged version bumps,
# and creates the tag itself. No PR, and no manual tag.
# Usage: just release-prep-backport 1.3.2
release-prep-backport version chart_bump="patch":
    #!/usr/bin/env bash
    set -euo pipefail
    case "$(git rev-parse --abbrev-ref HEAD)" in release/*.x) ;; *) echo "not on a release/X.Y.x maintenance branch"; exit 1;; esac
    VERSION="{{version}}" CHART_BUMP="{{chart_bump}}" PR_BODY_FILE=release-pr-body.md \
        node .github/scripts/prepare-release.mjs
    chart="$(awk '/^version:/{print $2; exit}' deploy/helm/cerberus/Chart.yaml)"
    docker run --rm -v "$PWD/deploy/helm:/helm-docs" -u "$(id -u)" \
        jnorwood/helm-docs:v1.14.2 --chart-search-root=/helm-docs --template-files=README.md.gotmpl
    git add deploy/helm/cerberus/Chart.yaml deploy/helm/cerberus/README.md CHANGELOG.md
    git commit -m "chore(release): cerberus v{{version}} / chart ${chart}"
    git push -u origin "$(git rev-parse --abbrev-ref HEAD)"
    echo "Pushed v{{version}} to $(git rev-parse --abbrev-ref HEAD) — that push IS the trigger; release.yml builds, publishes, and tags. Do not tag by hand."

# There is deliberately no `release-tag` recipe. release.yml publishes on
# merge — it has no raw-tag trigger — and `release-version-gate.mjs` decides
# whether to publish by asking whether `v<appVersion>` already exists.
# So pre-creating the tag by hand does not start a release — it PERMANENTLY
# cancels one: the gate sees the tag, sets publish=false, and goreleaser,
# publish and chart-release all skip. No job fails, and nothing ships.
# `TestNoJustfileRecipePushesAReleaseTag` in
# `test/regression/release_required_checks_test.go` keeps it from coming back:
# it rejects a recipe named `release-tag*` and any recipe line that cuts a tag
# (`git tag`, `git push --tags`, `gh release create`).
