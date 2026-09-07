# === Test ===

# Every `go test` below that CI runs carries an explicit `-timeout`, and it is
# always shorter than the `timeout-minutes` of the job that runs it.
#
# Both halves matter. `go test` stops a binary after 10 minutes when the command
# line does not say otherwise, so a job declaring a longer budget advertises
# headroom the tests can never reach — the binary panics at 10:00 and the job
# fails at half its stated budget with nothing in the log explaining the gap.
# Raising the job's number looks like a fix and is not. And the ordering is what
# makes a red lane useful: Go's abort dumps every goroutine's stack, naming the
# test that hung; the runner's kill takes the container away and uploads
# nothing.
#
# `test/regression/go_test_timeout_budget_test.go` derives the pairing from
# these recipes and the workflows and fails on any drift, so the numbers below
# do not have to be remembered.

# Run unit + spec tests with race detector, execute the focused chaos-tag
# assertions, then type-check the build-tagged lanes no other recipe compiles.
# This is the whole `check` gate in one command, which is what you want locally;
# CI may schedule the constituent recipes independently, so keep this one a
# pure composition rather than a copy of their commands.

# Run the whole check gate: race-detected unit + spec, chaos assertions, tagged vet.
test: test-unit test-chaos-sleep vet-tagged

# The race-detected unit + spec suite. CI's `check-test` job runs exactly this
# and nothing else: it is the long pole of the gate, and pairing it with the
# quick type-check/build work would serialise two things that have no reason to
# wait on each other.

# Run the race-detected unit + spec suite.
test-unit:
    go test -timeout 15m -race ./...

# Run the deterministic chaos_sleep injection assertions. The live chaos lane
# builds this tag into the binary, but only this focused recipe executes the six
# tagged unit assertions that prove header propagation and SQL splicing.

# Run the deterministic chaos_sleep injection assertions.
test-chaos-sleep:
    go test -timeout 2m -race -tags=chaos_sleep -count=1 -run '^(TestApplyChaosSleep_|TestChaosSleep_)' ./internal/api/prom/... ./internal/chsql/...

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

# Type-check the build-tagged migration and AGPL-oracle lanes no other recipe compiles.
vet-tagged:
    go vet -tags=migration_tier1 ./test/e2e/migration/...
    go vet -tags=migration_tier2 ./test/e2e/migration/...
    go vet -tags=agpl_oracle ./internal/... ./test/agpl_oracle/...

# Run only the tests and compile probes whose evidence depends on the
# agpl_oracle tag. The focused filters avoid re-running the entire untagged
# internal and TXTAR suites after the required check gate already ran them.
# Requires no CGO, Docker, or libchdb.

# Run the tests whose evidence depends on the agpl_oracle tag. No CGO, Docker or chDB.
test-agpl-oracle:
    go test -timeout 12m -race -tags agpl_oracle -count=1 -run '^(TestAGPLOracle_|TestJSONPathParse_MatchesLokiJSONExpr$|TestParseExprPermissive_MatchAllAccepted$|TestPattern_)' ./internal/logql/...
    go test -timeout 12m -race -tags agpl_oracle -count=1 ./test/agpl_oracle/... ./test/spec/parityoracle/logql/... ./test/spec/parityoracle/traceql/... ./test/property/oracle/logql/...
    go test -timeout 12m -race -tags agpl_oracle -count=1 -run '^Test(LogQL|TraceQL)ReferenceVerdictsAreCurrent$' ./test/surface-parity/...

# Run the schema DDL and command auto-create integration tests against real
# ClickHouse containers (spun up via testcontainers-go). `-p=1` keeps the two
# packages from competing for the same Docker substrate. Requires Docker and is
# gated behind `integration`, so regular `just test` stays container-free.

# Run the schema DDL and auto-create integration tests on real ClickHouse. Needs Docker.
schema-ddl-test:
    @just _pull-retry {{CH_TEST_IMAGE}} {{CH_TEST_IMAGE_PRIOR}}
    go test -timeout 20m -p=1 -race -tags=integration ./internal/schema/ddl/... ./cmd/cerberus/...

# Run the TXTAR spec suite with the chDB-backed round-trip assertion
# layer enabled. Requires libchdb.so (see `just chdb-install`). The
# default `just test` lane stays CGO_ENABLED=0 and never compiles the
# chdb-go driver. Only fixtures that declare both `seed:` and
# `expected_rows:` are executed against chDB; everything else still
# runs through the text-equality golden path.

# Run the TXTAR spec suite with the chDB round-trip layer on. Needs libchdb.so.
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
# over the emitted SQL can prove that. Includes ./internal/chsql/... so the
# emitter's OWN chDB round-trip suite runs on a PR: it had never executed in
# CI at all — `chdb-build` is compile-only by design and the mutation lane
# builds untagged — so a cross-test interaction that failed 100% of the time
# on a developer's laptop sat on `main` unseen (#2074). The suite is the only
# layer that executes emitted SQL against a real engine from inside the
# emitter package, where the unexported emitter entry points are reachable.

# Run the chdb-tagged handler, emitter, solver and corpus lanes. Needs libchdb.so.
test-chdb:
    go test -timeout 10m -tags chdb -count=1 ./internal/chclienttest/... ./internal/api/... ./internal/chsql/... ./internal/optcorpus/... ./internal/routerrules/... ./internal/schema/ddl/... ./internal/solver/... ./test/consumer-corpus/...

# Run the chDB-tagged property tests (rapid + from-scratch oracle) for all three
# query heads. The synthetic composite tag activates the LogQL live runner.
# Requires libchdb.so (see `just chdb-install`). Local default is rapid's
# 100 iterations; the nightly `property` CI workflow overrides to 500.

# Run the chdb-tagged property tests for all three heads. Needs libchdb.so.
property:
    go test -tags chdb,agpl_oracle,chdb_agpl_oracle -count=1 ./test/property/...

# Run the chDB-tagged perf regression guards (test/perf). These are
# deterministic ASSERTION pins — not wall-clock benchmarks — that bite a
# regression of the landed perf wins: the metrics-table MetricName-first
# ORDER BY granule prune (EXPLAIN indexes=1 ratio floor) and the /series
# fan-out round-trip baseline. Requires libchdb.so (see `just chdb-install`).
# Mirrors the `perf-guards-shard` CI job in chdb.yml. Distinct from the
# informational `perf-benchmark.yml` lane, which only reports benchstat
# deltas and never gates.
#
# This recipe runs the guards over the corpus slice named by the environment:
# PERF_SHARD_INDEX / PERF_SHARD_COUNT (test/perf/profile/shard.go). Both unset —
# the local default — is the WHOLE corpus, exactly as before sharding existed.
# CI sets them, one leg per slice, because TestCardinalityRatchet's runtime is a
# straight line in corpus size and cannot be parallelised inside one process
# (chdb-go caches one session per process, #1987); see chdb.yml for the topology
# and issue #2002 for why the serial lane had to stop.
#
# The `-timeout` is sized for the recipe's WORST case, and sharding did NOT make
# that case smaller: it is the unsharded whole-corpus sweep, the one invocation
# a matrix cannot speed up. The lane was ~8 minutes when this comment was first
# written, measured 867s on 2026-08-09 shortly before that day's
# promql-parity-enrolment wave grew the corpus further, and a CI run under a
# genuinely isolated runner hit ITS OWN -timeout at exactly 1020s (17m)
# afterward. 27m is that worst case with room to keep growing. A CI LEG, by
# contrast, finishes 6-8x under this ceiling, so for the matrix the number is a
# hang detector rather than an estimate. It must also stay at least 3 minutes
# under the job's `timeout-minutes` so Go aborts first and dumps goroutine
# stacks — test/regression/go_test_timeout_budget_test.go enforces that ordering.
#
# Do not trust a local re-measurement taken while another chdb-tagged test
# process is running concurrently on the same machine: TestCardinalityRatchet
# is CPU- and chDB-engine-heavy, and two concurrent runs starve each other
# on a resource-constrained box in a way that looks exactly like a hang
# (long silent gaps between fixtures) but is not one — confirmed via
# SIGQUIT goroutine dump showing an ordinary in-progress chDB query, and
# `ps`/`uptime` showing another agent's own perf.test process pinning a
# CPU core. Only a CI run, or a local run with nothing else chdb-tagged
# active, is a trustworthy timing signal for this recipe. That caveat applies
# to the per-shard figures too: the local sweep that first timed the eight
# slices ran against exactly such a competing process.

# Run the chdb-tagged perf regression guards. Up to ~27 min; needs libchdb.so.
perf-chdb:
    go test -timeout 27m -tags chdb -count=1 ./test/perf/...

# Profile the TXTAR corpus for compute fan-out (perf-assessment Component B).
# Walks every executable fixture under test/spec/** (those with `seed:` +
# `expected_rows:` + `sql:`) and, per fixture, runs EXPLAIN PLAN actions=1
# (CROSS JOIN / ARRAY JOIN / recursive-CTE detection) + a per-subquery-level
# count() decomposition (peak intermediate cardinality vs leaf scan rows) in
# process via chDB. Writes a JSON profile array and prints the top fan_factor
# fixtures. Requires libchdb.so (see `just chdb-install`).
#
# Like `perf-chdb`, this recipe runs the corpus slice named by
# PERF_SHARD_INDEX / PERF_SHARD_COUNT (test/perf/profile/shard.go) — both
# unset, the local default, is the WHOLE corpus. perf-profile.yml's
# `profile-shard` matrix sets both, one leg per slice, and its `profile`
# aggregator then runs `go run ./cmd/perf-profile -merge '<dir>/*.json' ...`
# (no chDB, no `chdb` tag — see cmd/perf-profile/report.go) to fold the legs'
# JSON outputs back into one combined report — the same shape the lane
# produced before sharding existed (tsouza/cerberus#2375). NOT a per-PR gate:
# corpus-wide breadth over ~950 fixtures is too heavy for every PR. Override
# OUT / TOP to taste.

# Profile the TXTAR corpus for compute fan-out into a JSON report. Needs libchdb.so.
perf-profile OUT="perf-profile.json" TOP="40":
    go run -tags chdb ./cmd/perf-profile -spec test/spec -out {{OUT}} -top {{TOP}}

# Run the chclient testcontainers integration tests against a real
# ClickHouse container. Requires Docker. Gated behind the `integration`
# build tag so regular `just test` doesn't pull in Docker.

# Run the chclient integration tests against a real ClickHouse container. Needs Docker.
chclient-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -race -tags=integration ./internal/chclient/...

# Run the strict-scan differential: execute the matrix-shaped spec golden
# SQL corpus against a REAL ClickHouse (testcontainers-go) through the
# production cursor's strict positional scan, failing on any type-coercion
# error. This catches the prod-vs-chDB divergence the chdb-tagged goldens
# are structurally blind to (chDB leniently coerces e.g. UInt8 -> *float64;
# clickhouse-go strict-scans and 502s). Requires Docker; gated behind the
# `integration` build tag. See test/spec/strictscan_integration_test.go and
# .github/workflows/strict-scan.yml.

# Run the strict-scan differential over the golden SQL corpus on real CH. Needs Docker.
strict-scan-test:
    @just _pull-retry {{CH_STRICT_SCAN_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run TestStrictScanDifferential ./test/spec/...

# Run the router-corpus real-CH integration tests: the offline corpus WRITE
# path (optcorpus.CHTableSink: real CREATE DDL + columnar Enum8 batch) and READ
# path (routerrules.chCorpusSource: strict-scanned aggregates) executed against
# a REAL ClickHouse (testcontainers-go) through clickhouse-go/v2. These offline
# paths never touch the data plane, so compose-smoke / e2e never exercise them,
# and their only other tests use a fake batch / chDB (which leniently coerces
# integer columns into *float64 — exactly the #1064 strict-scan blind spot).
# Requires Docker; gated behind the `integration` build tag. See
# internal/routerrules/realch_integration_test.go and strict-scan.yml.

# Run the router-corpus write and read paths against real ClickHouse. Needs Docker.
router-corpus-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run 'RealClickHouse' ./internal/routerrules/... ./internal/optcorpus/...

# Run the TraceQL spans-scan resource-bound real-CH guard (PR #1154): lowers +
# emits the Grafana Traces Drilldown Structure + Comparison queries through the
# real cerberus path and executes them against a REAL ClickHouse
# (testcontainers-go) seeded with a multi-partition otel_traces. Proves the
# bounded recursive-CTE drilldown SQL runs WITHOUT error 49 (the seam chDB
# cannot validate), that partition pruning fires at runtime, and that the
# compare matrix scan fails closed on an absent window. Requires Docker; gated
# behind the `integration` build tag. See
# test/spec/traces_scan_resource_bound_integration_test.go and strict-scan.yml.

# Run the TraceQL spans-scan resource-bound guard on real ClickHouse. Needs Docker.
traces-scan-bound-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run TestTracesScanResourceBoundRealCH ./test/spec/...

# Run the Tempo HTTP traces-scan WINDOW real-CH guard (#1509): drives the
# real /api/search + /api/metrics/query_range handlers against a REAL
# ClickHouse (testcontainers-go) seeded with otel_traces spread across many
# daily partitions, and asserts each drilldown request (i) completes without
# tripping a session memory cap and (ii) reads strictly fewer rows than the
# whole table — proving the request-window predicate prunes partitions on the
# recursive-numbering / structural-closure / compare-root-lookup scans, not
# just the (non-pruning) `TraceId IN (<seed>)` membership check #1154 added.
# This test existed fully written but wired into ZERO CI lane before #1509;
# it is INFORMATIONAL (not a required PR gate) because it duplicates
# traces-scan-bound-integration's real-CH substrate rather than adding a new
# invariant class. Requires Docker; gated behind the `integration` build tag.
# See internal/api/tempo/traces_scan_window_integration_test.go and
# strict-scan.yml.

# Run the Tempo traces-scan request-window pruning guard. Needs Docker.
traces-scan-window-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run TestTracesScanWindowRealCH ./internal/api/tempo/...

# Run the structural two-phase external-trace-id-table real-CH verification
# (issue #2783): the phase-B literal `TraceId IN (...)` splice vs. the
# native-protocol external-table push restrictStructural switches to on wide
# closures. Three tests share this lane (they all seed the same otel_traces
# shape at real-server scale, so one recipe/CI step covers them without
# tripling the container-boot overhead each pays independently):
#   - TestStructuralTwoPhase_ExternalTraceIDTable_RealCH — drives the real
#     /api/search handler with h.ExternalTraceIDPush toggled and asserts the
#     external-table run returns the EXACT same ordered traces as the literal
#     run, with a comparable (not blown-up) read_rows count.
#   - TestStructuralTwoPhase_ExternalTraceIDTable_ExplainIndexes — the
#     issue's own EXPLAIN indexes=1 mandate: asserts idx_trace_id drops the
#     SAME granule count for both forms.
#   - TestExternalTraceIDs_ReusableAcrossDispatches — proves a
#     chclient.WithExternalTraceIDs ctx is safe to dispatch more than once
#     (the shape both a transport retry and a route-B shard take).
# Requires Docker; gated behind the `integration` build tag. See
# internal/api/tempo/structural_two_phase_external_table_integration_test.go
# and strict-scan.yml.

# Run the structural two-phase external trace-id table verification. Needs Docker.
structural-two-phase-external-table-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run 'TestStructuralTwoPhase_ExternalTraceIDTable|TestExternalTraceIDs_ReusableAcrossDispatches' ./internal/api/tempo/...

# Run the perf-smoke real-ClickHouse sentinel differential (#2370 PR 1): the
# real-CH memory-bounding guard for the #2364 incident class. PR #2358
# disabled ClickHouse's query analyzer for native-histogram plan shapes,
# validated only by a manual timing comparison on CI-fixture-scale data;
# 6h8m later prod hit MEMORY_LIMIT_EXCEEDED (#2364) because the fix silently
# removed a CSE fold, invisible at CI scale (fixed in #2366). Drives the
# mounted production prom + tempo handlers over real HTTP against a REAL
# ClickHouse (testcontainers-go) seeded at the scale each incident's root
# cause needs to actually engage, and reads peak per-query memory back from
# system.query_log via optcorpus.CHQueryLogSource. Requires Docker; gated
# behind the `integration` build tag. See
# test/perf/smoke/realch_perfsmoke_integration_test.go and strict-scan.yml.

# Run the perf-smoke real-ClickHouse memory sentinel differential. Needs Docker.
perf-smoke-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    @# The join-spill tier (smoke.FloorJoinSpill) boots a SECOND container:
    @# chopt.FeatureJoinSpill floors at 26.4, above the 25.9 CH_TEST_IMAGE, and
    @# max_bytes_before_external_join does not exist below it. The tag reused
    @# here is CH_STRICT_SCAN_IMAGE, which this repo already pins at or above
    @# every chopt floor — the same reuse the ts_grid_instant lane in
    @# test/perf/nightly makes.
    @just _pull-retry {{CH_STRICT_SCAN_IMAGE}}
    go test -timeout 20m -tags=integration -count=1 -run TestPerfSmoke ./test/perf/smoke/...

# Run the ts_grid_* native range-window lowerer activation lane (#2487): wires
# a *prom.Handler the way cmd/cerberus's own boot path does (via
# internal/chopttest, extracted from main.go's nativeRangeLowerers) against a
# REAL ClickHouse, then proves each of the six native families — rate,
# staleness (resample), changes, resets, deriv, predict_linear — actually
# fires by reading the emitted SQL back out of system.query_log, not just
# that the request returns HTTP 200. Requires Docker; gated behind the
# `integration` build tag. See
# test/perf/smoke/realch_native_lowerers_integration_test.go and
# strict-scan.yml.

# Run the native range-window lowerer activation lane on real ClickHouse. Needs Docker.
native-lowerers-smoke-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run TestNativeRangeLowerers_RealCH_Integration ./test/perf/smoke/...

# Run the #2370 nightly measurement + regression gate: loads the real,
# trimmed production sample (test/perf/nightly/testdata/samples/*.parquet,
# LFS — needs `git lfs pull` first if not already fetched) into a real
# ClickHouse and issues a curated sentinel corpus, gating each one's
# max-of-N peak memory against a committed baseline (nightly-baseline.json)
# plus an absolute cap-relative ceiling, and its HTTP status against the
# outcome each sentinel was calibrated to expect (PR B of #2370). Requires
# Docker; gated behind the `integration` build tag. See
# test/perf/nightly/realch_perfnightly_integration_test.go and
# .github/workflows/perf-nightly.yml.

# Run the nightly memory gate over the real trimmed sample. Needs Docker and git-lfs.
perf-nightly-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -v -timeout 15m -tags=integration -count=1 -run TestPerfNightlyRealCH ./test/perf/nightly/...

# Run the ts_grid_instant real-data memory measurement (#2748): reuses this
# package's own real, trimmed production sample
# (test/perf/nightly/testdata/samples/svc_http_requests_total.parquet) in a
# SECOND, dedicated ClickHouse container (CH_STRICT_SCAN_IMAGE, 26.6-alpine —
# ts_grid_instant floors at 26.5, ABOVE this package's own 25.9-alpine
# perfNightlyCHImage), issuing the SAME instant `rate(m[1d])` query once
# through the fan-out and once through the new instant native lowering
# (chplan.RangeWindowGridNativeInstant), reading real peak memory_usage back
# from system.query_log for both. Requires Docker; gated behind the
# `integration` build tag. See
# test/perf/nightly/realch_ts_grid_instant_memory_integration_test.go and
# .github/workflows/perf-nightly.yml.

# Measure ts_grid_instant fan-out vs native peak memory on real data. Needs Docker, git-lfs.
ts-grid-instant-memory-integration:
    @just _pull-retry {{CH_STRICT_SCAN_IMAGE}}
    go test -v -timeout 15m -tags=integration -count=1 -run TestTSGridInstantMemory_RealCH_Integration ./test/perf/nightly/...

# Regenerate nightly-baseline.json: for each #2370 nightly sentinel the
# calibration-time max-of-N peak memory_usage plus the headroom-multiplied
# ceiling (nightlyBaselineHeadroom) the gate actually asserts against, and
# the HTTP status it was calibrated to expect. Requires Docker and (once,
# if not already fetched) `git lfs pull` for the trimmed sample parquet
# files. Run this — and REVIEW THE DIFF — only when a bound move is
# genuinely intended (a real, justified memory-cost increase, or a
# deliberate change to which sentinels are expected to be rejected by a
# resource bound); a silent loosen is exactly the regression this lane
# exists to catch. The gating assertion is TestPerfNightlyRealCH, wired
# into .github/workflows/perf-nightly.yml.

# Regenerate the nightly per-sentinel memory ceilings. Needs Docker; review the diff.
update-nightly-perf-baseline:
    @just _pull-retry {{CH_TEST_IMAGE}}
    UPDATE_NIGHTLY_PERF_BASELINE=1 go test -timeout 15m -tags=integration -count=1 -run TestPerfNightlyRealCH ./test/perf/nightly/...
    @node .github/scripts/lib/regen-diff.mjs --label baseline test/perf/nightly-baseline.json

# Run #2437's periodic self-check: deliberately breaks two real
# memory-bounding mechanisms (the #2429 fanout resource bound, the spill
# safety net) one at a time and asserts `just perf-nightly-integration`
# actually fails under each — proving the nightly gate is still armed rather
# than having silently bit-rotted since the last real regression tripped it.
# Reverts every mutation via `git checkout --` even on failure; never
# commits or pushes anything. Requires Docker and a clean working tree (it
# refuses to mutate on top of unrelated uncommitted changes). Weekly, not
# nightly — see .github/workflows/perf-nightly-selfcheck.yml.

# Prove the nightly perf gate is still armed by breaking two bounds. Needs Docker, clean tree.
perf-nightly-selfcheck:
    @just _pull-retry {{CH_TEST_IMAGE}}
    node --test .github/scripts/perf-nightly-selfcheck.test.mjs
    node .github/scripts/perf-nightly-selfcheck.mjs

# Run the solver's mandatory per-shard memory-apportionment real-CH guard:
# proves Executor.runShard's max_memory_usage WithQuerySetting override is
# actually honored by a real ClickHouse over clickhouse-go/v2, not just
# accepted and silently ignored (chDB is known-lenient about settings, the
# same class of blind spot the strict-scan lane documents for type
# coercion). A deliberately tiny per-shard budget must fail with CH's own
# memory-limit error; a generous one on the identical query must succeed.
# Requires Docker; gated behind the `integration` build tag. See
# internal/solver/executor_realch_integration_test.go and strict-scan.yml.

# Prove the solver's per-shard memory cap is honored by real ClickHouse. Needs Docker.
solver-memory-apportion-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run TestExecutor_PerShardMaxMemoryUsage_RealClickHouse ./internal/solver/...

# Run the route-B real-CH result differential: the IDENTICAL query_range
# request answered through the REAL production PromQL lowering + emitter,
# once with the solver disabled (route A) and once forced sharded (route B),
# both against the SAME real ClickHouse (testcontainers-go). Asserts route B
# genuinely fired (X-Cerberus-Route-Decision, K>=2 shards) and that its
# decoded matrix result is byte-identical to route A's. Every other route-B
# test either runs on chDB only or drives a fake emitter that returns
# identical hand-written SQL for every shard regardless of the input plan —
# this is the one lane that lowers a genuine query and strict-scans its real,
# per-shard-sliced SQL. Requires Docker; gated behind the `integration` build
# tag. See internal/api/prom/handler_route_b_realch_integration_test.go and
# strict-scan.yml.

# Run the route-A vs route-B result differential on real ClickHouse. Needs Docker.
route-b-differential-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run TestRouteB_MatchesRouteA_RealClickHouse ./internal/api/prom/...

# Run the Loki + Prometheus metadata/label-values endpoints real-CH
# differential: /loki/api/v1/{series,labels,label/*/values,index/stats,
# index/volume} and /api/v1/{labels,label/*/values,metadata} all call a
# chclient.Client decoder method (QueryLabelSets / QueryIndexStats /
# QueryStrings / QueryIndexVolume / QueryMetricMeta) directly from the
# handler, bypassing engine.QueryPlan entirely — the SQL these handlers
# build is exactly what production sends. Unlike every other spec fixture
# (query-body-shaped: a PromQL/LogQL/TraceQL string that lowers into a
# chplan), these endpoints are matcher + time-range driven and carried no
# fixture corpus at all before this lane (#1634). Drives a REAL
# loki.Handler + prom.Handler mounted on an httptest.Server against a REAL
# ClickHouse (testcontainers-go), asserting the decoded response of one
# real case per endpoint. Requires Docker; gated behind the `integration`
# build tag. See test/spec/metadata_endpoints_realch_integration_test.go
# and strict-scan.yml.

# Run the metadata / label-values endpoint differential on real ClickHouse. Needs Docker.
metadata-endpoints-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run TestMetadataEndpoints_RealClickHouse ./test/spec/...

# Run the histogram real-exporter-schema differential (#1642): provisions the
# table shapes the REAL upstream clickhouseexporter creates — LowCardinality-
# keyed attribute maps (Map(LowCardinality(String), String), not the plain
# Map(String, String) every other fixture uses) and the exponential-histogram
# table under its true name, otel_metrics_exponential_histogram — and drives
# the Prometheus HTTP surface with columnar_result_decode ON. Every other
# layer validates cerberus against a fixture cerberus itself created, so the
# seed DDL and the reader always agree; this test is the only one that pins a
# native exp-histogram quantile against the real exporter table name, the
# columnar-decode fall-back off a LowCardinality-keyed map, and classic-
# histogram _bucket/_count/_sum discovery off the real schema. Requires
# Docker; gated behind the `integration` build tag. See
# internal/api/prom/handler_histogram_integration_test.go and strict-scan.yml.

# Run the histogram real-exporter-schema differential against real ClickHouse. Needs Docker.
histogram-realexporter-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run TestHistogram_RealExporterSchema_Integration ./internal/api/prom/...

# Run the ts_grid_* native range-window lowerer activation lane (#2487) for
# internal/api/prom's own real-CH integration coverage — the package-local
# sibling of native-lowerers-smoke-integration, exercising the identical six
# families (rate, staleness/resample, changes, resets, deriv,
# predict_linear) through this package's OWN *prom.Handler and real exporter
# schema, so a package-local emit-type or wiring bug can't hide behind
# test/perf/smoke's coverage alone. Requires Docker; gated behind the
# `integration` build tag. See
# internal/api/prom/handler_native_lowerers_integration_test.go and
# strict-scan.yml.

# Run internal/api/prom's native range-window lowerer activation lane. Needs Docker.
native-lowerers-prom-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run TestNativeRangeLowerers_RealCH_Integration ./internal/api/prom/...

# Run the quantile_prom_histogram real-CH differential (#2755): for every
# representative classic-histogram bucket layout (a normal crossing, a
# duplicate-bound layout, the equal-length/no-overflow-rung shape, an empty
# histogram, a first-bucket non-positive upper bound, an all-zero-count
# histogram, a single bucket) and the full phi domain (below range, the two
# saturating edges, interior crossings, above range, a runtime NaN phi), emits
# BOTH the hand-rolled rank walk and the native quantilePrometheusHistogram
# aggregate for the identical chplan.HistogramQuantile plan and asserts they
# agree exactly against a REAL ClickHouse. quantilePrometheusHistogram shipped
# in ClickHouse 25.10 — above every other integration lane's pin — so this
# lane boots its own dedicated CH_QUANTILE_PROM_HISTOGRAM_IMAGE server rather
# than reusing CH_TEST_IMAGE. Requires Docker; gated behind the `integration`
# build tag. See
# internal/chsql/histogram_quantile_rankwalk_native_realch_integration_test.go
# and strict-scan.yml.

# Run the rank-walk vs native histogram-quantile differential. Needs Docker.
quantile-prom-histogram-rankwalk-integration:
    @just _pull-retry {{CH_QUANTILE_PROM_HISTOGRAM_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run TestHistogramQuantile_RankWalkNative_DifferentialRealCH ./internal/chsql/...

# Run the ts_tag_groups real-CH differential (cerberus issue #2750): the
# duplicate-labelset guard's instant-mode name-drop collapse
# (guardNameDropCollisionByTagGroup) answers BYTE-IDENTICAL to the existing
# Map-grouped path across the collision-reject, distinct-labelsets
# pass-through, and pinned-name no-guard shapes, driven through the REAL
# production HTTP handler + emitter against a REAL ClickHouse — the proof
# chopt.FeatureTSGridTagGroups's registry entry rests on.
# timeSeriesThrowDuplicateSeriesIf's floor (26.2) is above every other
# integration lane's pin, so this lane boots its own dedicated server
# (CH_TAG_GROUPS_IMAGE) rather than reusing CH_TEST_IMAGE. Requires Docker;
# gated behind the `integration` build tag. See
# internal/api/prom/handler_tag_groups_realch_integration_test.go and
# strict-scan.yml.

# Run the ts_tag_groups duplicate-labelset differential on real ClickHouse. Needs Docker.
tag-groups-differential-integration:
    @just _pull-retry {{CH_TAG_GROUPS_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run TestTagGroups_MatchesMapGrouping_RealClickHouse ./internal/api/prom/...

# Run the ts_grid_last_over_time real-CH regression pin (#2747): reproduces
# upstream's own #106577 regression fixture shape (a staleness window
# narrower than the query_range grid step) end to end through cerberus's
# lowering + emitter, and asserts the native timeSeriesResampleToGridWithStaleness
# path agrees with the fan-out on both a sample that must come back absent
# (the pre-#106577 bug) and one that must be included (a control). Needs a
# real ClickHouse >= 26.6 — chopt.FeatureTSGridLastOverTime's own floor,
# ABOVE the family's usual 25.9 (see that registry entry) — so this lane
# reuses CH_STRICT_SCAN_IMAGE rather than pinning its own dedicated image:
# both now floor at exactly 26.6. Requires Docker; gated behind the
# `integration` build tag. See
# internal/chsql/range_window_last_over_time_realch_integration_test.go and
# strict-scan.yml.

# Run the ts_grid_last_over_time staleness-window regression pin. Needs Docker.
ts-grid-last-over-time-integration:
    @just _pull-retry {{CH_STRICT_SCAN_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run TestLastOverTime_NativeResample_WindowNarrowerThanStep_RealCH ./internal/chsql/...

# Run the ts_grid_group_array real-CH precondition + regression pin (#2749):
# verifies timeSeriesGroupArray accepts a DateTime64(9) timestamp losslessly
# (contrary to the function's own documented UInt32/DateTime/UInt64
# signature), that a NaN-bearing duplicate timestamp collapses in an
# insertion-order-DEPENDENT way (the reason
# chopt.FeatureTSGridGroupArray ships AutoSelect: false), and — end to end
# through cerberus's own lowering + emitter — that rate()'s native array-fold
# assembly matches the fan-out exactly on a genuine duplicate, non-NaN
# timestamp. The feature's own floor (25.9) is the family's usual one, so
# this pins CH_TEST_IMAGE directly rather than reusing CH_STRICT_SCAN_IMAGE's
# higher 26.6 pin. Requires Docker; gated behind the `integration` build tag.
# See internal/chsql/range_window_group_array_realch_integration_test.go.

# Run the ts_grid_group_array precondition and rate() regression pins. Needs Docker.
ts-grid-group-array-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run '^(TestTimeSeriesGroupArray_|TestRate_NativeGroupArray_)' ./internal/chsql/...

# Run the timeSeries*ToGrid duplicate-timestamp survivor pins (#2798): measures,
# for every member of the emitter's own nativeTSGridFn registry, which sample
# survives a NaN-bearing duplicate (series, timestamp) under each encounter
# order; asserts the production dedupWindowPairsByTsFrag Frag elects the same
# survivor under both; and reproduces the divergence end to end through
# cerberus's own lowering + emitter over one MergeTree table holding two series
# with the identical sample multiset. The family's own floor (25.9) is the
# usual one, so this pins CH_TEST_IMAGE directly rather than reusing
# CH_STRICT_SCAN_IMAGE's higher 26.6 pin. Requires Docker; gated behind the
# `integration` build tag. See
# internal/chsql/range_window_grid_native_nan_duplicate_realch_integration_test.go.

# Run the timeSeries*ToGrid duplicate-timestamp survivor pins. Needs Docker.
ts-grid-nan-duplicate-integration:
    @just _pull-retry {{CH_TEST_IMAGE}}
    go test -timeout 15m -tags=integration -count=1 -run '^(TestTSGridFamily_NaNDuplicate|TestFanoutDedup_NaNDuplicate|TestRate_NativeGrid_NaNDuplicate)' ./internal/chsql/...

# Run the FuzzParse target for one parser head for a bounded duration.
# Usage: `just fuzz QL=promql DURATION=60s` (defaults).

# Fuzz one parser head's FuzzParse target for a bounded duration.
fuzz QL="promql" DURATION="60s":
    go test -run='^$' -fuzz=FuzzParse -fuzztime={{DURATION}} ./internal/{{QL}}/...

# Run all Go benchmarks (no tests). Short benchtime for local use.
bench:
    go test -bench=. -benchmem -benchtime=5x -run='^$' ./...

# `-coverpkg` (used by both lanes below) is what makes the number a fact about
# the tests rather than about the file layout. Without it a package is
# measured only by its OWN test binary, so a helper package with no _test.go
# of its own lands in the profile fully instrumented and entirely unexecuted
# no matter how hard the rest of the suite drives it — and the floor derived
# from that zero cannot fail. With it, every test binary reports coverage for
# every listed package it links, so extracting a helper into its own package
# no longer resets its measured coverage to nothing. The cost is that each
# block is then reported once per test binary that linked it; the fold below
# (and coverage-summary.mjs itself) collapse those duplicates by taking the
# widest count per block.
#
# The pattern is `go list`'s expansion rather than the literal `./...`, because
# the two are NOT the same set here: go.mod `ignore`s the vendored upstream
# Loki and Tempo trees, so `./...` skips them when choosing what to test — but
# a literal `-coverpkg=./...` still instruments them, since they reach the
# build graph as dependencies of the compliance harnesses. That would put
# vendored third-party code into the coverage ledger and hold cerberus's gate
# to the test coverage of somebody else's benchmark suite. Asking `go list` per
# lane keeps the measured set identical to the tested set, with no exclusion
# list to maintain or forget.

# The pgregory.net/rapid PRNG seed both coverage lanes measure with.
#
# rapid's own default is 0, which it documents as "use a random one", and a
# ratchet cannot be fed a measurement that moves: test/coverage-floor/ only
# raises a floor, so one enrolled from a lucky draw is never corrected by the
# unlucky draw that follows it. Measured on tsouza/cerberus#3000,
# test/property/oracle/traceql drew 242, 243, 244 and 245 of its 272 statements
# across fifteen full-lane profiles of an unchanged tree — a 1.10-point spread
# against a 1.0-point slack.
#
# Exported to the two `go test` sweeps below, and read by the per-binary init
# each rapid-linked test package carries (see internal/chplan/rapid_seed_test.go
# for the canonical copy and test/regression/rapid_seed_pin_test.go for the
# scan that keeps the set complete). It is deliberately NOT set by `just
# property`: that lane's job is to SEARCH for counterexamples, which needs a
# fresh sample every run. This lane's job is to MEASURE, which needs the same
# one.
#
# The value is arbitrary but permanent. Changing it re-rolls every rapid-driven
# package's coverage in one step, against floors derived from the old draw, so
# treat it as fixed unless a floor is being deliberately re-derived.
COVERAGE_RAPID_SEED := "20260903"

# Fold a raw -coverpkg profile in place. -coverpkg makes EVERY test binary
# emit a row for every block it linked, so a raw lane profile is ~200 MB of
# repeats where the useful content is ~2 MB. One row per block, keeping the
# widest count, is `mode: set`'s union and loses nothing — it is the same
# fold `coverage-merge` does, just applied a step earlier, so the on-disk
# file and the uploaded artifact stay the size they were. The fold itself is
# `foldProfile()` in `.github/scripts/lib/coverage-fold.mjs` (issue #3095) —
# the one implementation this recipe and `coverage-merge`'s own two folds
# all share, rather than three hand-copies of the same awk.
_coverage-fold FILE:
    @node .github/scripts/lib/coverage-fold.mjs {{FILE}} {{FILE}}

# Default-tag coverage lane: writes cover.out. Split out of the chdb-tagged
# lane (below) and the merge/gate step (`coverage-merge`) so CI can shard the
# two lanes tsouza/cerberus's coverage.yml used to run serially on one
# runner across separate jobs/runners (tsouza/cerberus#2634) — that job had
# repeatedly hit its timeout as the package/fixture corpus grew. `coverage`
# below still chains all three for a single local command with the same
# behaviour the old monolithic recipe had.

# Run the default-tag coverage lane into cover.out.
coverage-default:
    @echo "==> default-tag coverage"
    # Both coverage lanes fail closed. The repository pins one supported Go
    # toolchain, which includes covdata; a partial profile produced after any
    # package test failure is not evidence and must never reach the floor gate.
    # 40m, not the 25m this lane used before -coverpkg: the heaviest binary
    # (test/perf) took 933s of that budget on CI while instrumenting only its
    # own package, and instrumenting every package into it measured 1266s
    # locally. A binary that overruns fails here and names the timeout directly
    # instead of silently dropping its whole contribution from the profile.
    # The awk trims go test's echo of the -coverpkg list back to what the
    # bare `./...` form used to print. An explicit list of every package is
    # ~5 KiB, repeated on EVERY package's result line, which buries the
    # ok/FAIL the line exists to report under a megabyte of restated flag.
    # It rewrites only the tail of that one sentence; results, failures and
    # everything on stderr pass through untouched. `fflush()` keeps the pipe
    # line-buffered — a block-buffered filter would show nothing at all until
    # a 4 KiB chunk fills, which on a lane this long reads as a hung job.
    CERBERUS_RAPID_SEED={{COVERAGE_RAPID_SEED}} go test -timeout 40m -coverpkg="$(go list ./... | paste -sd, -)" -coverprofile=cover.out ./... | awk '{ sub(/of statements in github\.com\/.*/, "of statements"); print; fflush() }'
    @test -s cover.out
    @just _coverage-fold cover.out

# chdb-tagged coverage lane: writes cover-chdb.out. Requires libchdb.so
# (`just chdb-install`) — unlike the default-tag lane, this one skips itself
# (rather than failing) when the library is absent, so a bare local `just
# coverage` without chDB installed still produces a default-tag-only profile,
# same as before this recipe was split out of `coverage`. CI's caller sets
# COVERAGE_REQUIRE_LANES=default+chdb, which turns that same skip into a hard
# failure — enforced inside the script below now, not this recipe body.
#
# Extracted to .github/scripts/coverage-chdb.mjs (tsouza/cerberus#3113,
# following coverage-merge's own extraction in #3095/#3091): the libchdb.so
# check, the `go list`-derived -coverpkg, the go test invocation and its
# streaming output filter, the SKIP_RATCHET_FANOUT branch, and the
# COVERAGE_REQUIRE_LANES fail-closed tail all live there now — see that
# module's header for the full behaviour and for exactly what
# test/regression/tagged_test_enrollment_test.go,
# coverage_recipe_fail_closed_test.go, rapid_seed_pin_test.go, and
# perf-coverage-fanout.test.mjs each now read as execution evidence in its
# place. CERBERUS_RAPID_SEED stays a literal env assignment HERE rather than
# moving into the script: rapid_seed_pin_test.go's static scanner looks for
# CERBERUS_RAPID_SEED={{COVERAGE_RAPID_SEED}} in the RECIPE body, the same
# place it has always looked for both coverage lanes.

# Run the chdb-tagged coverage lane into cover-chdb.out. Skips itself without libchdb.so.
coverage-chdb:
    @CHDB_INSTALL_PATH="{{CHDB_INSTALL_PATH}}" CERBERUS_RAPID_SEED={{COVERAGE_RAPID_SEED}} node .github/scripts/coverage-chdb.mjs

# Merge cover.out (default-tag lane, required) with cover-chdb.out
# (chdb-tagged lane, optional) into cover-merged.out and run the floor gate.
# Reads whatever cover.out / cover-chdb.out already sit on disk, so it works
# equally after running `coverage-default` + `coverage-chdb` locally in the
# same tree, or after a CI job downloads each lane's profile artifact into
# one directory (tsouza/cerberus#2634) — either way the merge is a fact about
# the files, not about how they got there.
#
# The lane set computed below is passed to coverage-summary.mjs, which records
# it beside the profile as cover-merged.out.lanes.json, bound to the profile's
# own SHA-256. That record is the ONLY thing `update-coverage-floor` will accept
# as proof that a profile carries both lanes — this recipe is the one step that
# legitimately knows, because it is the step that decided.
#
# Extracted to .github/scripts/coverage-merge.mjs (issue #3095, epic #3091) —
# see that module's header for the ratchet-shard fold and the merge-vs-copy
# lane decision. `coverage-chdb`'s own recipe body stays untouched bash:
# test/regression/tagged_test_enrollment_test.go statically parses its exact
# shell text as chdb-tagged test execution evidence. Extracting it too is
# tracked separately as tsouza/cerberus#3113.

# Merge the two lane profiles into cover-merged.out and run the floor gate.
coverage-merge:
    @node .github/scripts/coverage-merge.mjs

# Generate the coverage profile and hold it to the committed per-package
# floors, chaining the three recipes above: `coverage-default` writes
# cover.out, `coverage-chdb` writes cover-chdb.out (skipped without
# libchdb.so), and `coverage-merge` folds them into cover-merged.out and
# runs coverage-summary.mjs's floor gate.
#
# CI does not call this recipe directly: coverage.yml runs `coverage-default`
# and `coverage-chdb` as two parallel jobs and `coverage-merge` in a third
# that depends on both (tsouza/cerberus#2634), so a local `just coverage`
# and the CI pipeline share every line of test/merge logic without sharing a
# process.

# Profile coverage over both tag lanes and gate it on the floors. ~45 min; needs libchdb.so.
coverage: coverage-default coverage-chdb coverage-merge

# Raise test/coverage-floor/ to what the tree currently achieves. Reads the
# profile `just coverage` leaves behind, so run that first. The ledger is a
# ratchet: this recipe never lowers a floor — a package that has dropped is
# reported and the run fails, because a tool that rewrites a floor to match a
# regression launders the regression into a green run. Restore the tests, or
# lower the floor by hand so the drop is a reviewable line in the diff.
#
# It also never records a 0. A package too thinly exercised to justify any
# floor above 0 fails the run instead: 0 is not a floor, it is an entry that
# looks measured while nothing can fall through it — every statement in the
# package could be deleted and the gate would stay green. Give the package a
# test (the profile is built with `-coverpkg` over the whole module, so a test
# in ANY package counts), or delete the code nothing reaches.
#
# And it never records floors from a profile it cannot prove carries BOTH
# lanes. coverage-summary.mjs reads cover-merged.out.lanes.json — the record
# `coverage-merge` stamps onto the profile, bound to its bytes — and refuses
# anything narrower, unstamped, or stamped for some other profile.
#
# This recipe used to test for libchdb.so instead, which proves the chdb lane
# COULD have run on this machine and nothing at all about the profile on disk
# (tsouza/cerberus#2988). That test is gone rather than demoted to a warning:
# it fires on a condition that is neither necessary nor sufficient for what
# this recipe does. It would print in the one flow the fix exists to keep
# working — enrolling from a downloaded CI `coverage-profile` artifact, which
# is complete on a machine that has never installed chDB — and stay silent
# when a stale or narrow record is about to be refused on a machine that has
# it. The early diagnostic lives where the long run is: `coverage-chdb` says
# "libchdb.so not found, skipping chdb lane" as it skips, and the refusal here
# names `just chdb-install` in its own remedy text.

# Raise test/coverage-floor/ to what the current profile achieves. Run `coverage` first.
update-coverage-floor:
    @test -s cover-merged.out || { echo "error: cover-merged.out not found — run 'just coverage' first; the floors are derived from the profile it writes" >&2; exit 1; }
    @COVERAGE_UPDATE_FLOORS=1 node .github/scripts/coverage-summary.mjs
    @node .github/scripts/lib/regen-diff.mjs --label floors test/coverage-floor/

# Regenerate the exact per-head roster of TXTAR fixtures carrying a live
# reference parity contract. The assertion mode never writes; this recipe is
# the explicit, review-visible way to accept a deliberate roster change.

# Regenerate the per-head roster of fixtures carrying a live parity contract.
update-parity-enrolment-baseline:
    UPDATE_PARITY_ENROLMENT_BASELINE=1 go test -count=1 -run '^TestParityEnrolmentBaseline$' ./test/regression
    @node .github/scripts/lib/regen-diff.mjs test/regression/parity-enrolment-baselines/

# Regenerate the fixture-derived artefacts named by SHARD, one or more of:
#
#     promql logql traceql chsql optimizer codegen migration parity solver
#     cardinality all
#
#     just update-golden promql
#     just update-golden promql cardinality
#     just update-golden all
#
# The shard argument is REQUIRED and has no default: bare `just update-golden`
# is an error that prints the vocabulary. Whole-corpus regeneration walks the
# 1012-fixture TXTAR corpus through chDB twice and costs ~14 minutes even when
# it produces a zero diff (#1898), and chDB is single-threaded per process, so
# nobody should pay for the heads they did not touch by accident.
#
# A REMOTE `gh workflow run update-golden.yml -f shards=...` dispatch does not
# need a precisely-guessed list: its `plan` job (manual-golden-update.mjs's
# `buildPlan`) merges in whatever the target branch's own diff implies is
# stale — the same derivation this recipe runs locally — BEFORE building the
# regeneration matrix, and logs a `::notice::` naming what it added. A change
# touching `internal/promql` or `internal/chplan` routinely implies most or
# all of the other shards, because the coupling is REAL (`internal/chplan` is
# the shared plan IR every head lowers to and nearly every generator's package
# closure imports it; `internal/chsql`'s own generator test files import
# `internal/promql` directly), so a narrow `-f shards=` guess there now
# self-corrects on the first dispatch instead of failing a downstream CI step
# and costing a round trip.
#
# The obvious hazard is that naming a shard reintroduces the trap the recipe's
# unconditional chaining was built to close (#1573; hit by #1571 and again by
# #1592): a contributor regenerates one artefact, sees zero remaining churn,
# pushes, and arrives at a red lane because a DIFFERENT artefact went stale. CI
# can only DETECT that drift — the harness refuses to regenerate a golden when
# `CI` is set, because a golden is a reviewed artifact and a lane that rewrites
# its own expectations asserts nothing — which is exactly what made the omission
# a trap.
#
# So the runner computes which shards the branch's own diff implies are stale
# and REFUSES to start when the chosen set does not cover them, naming the
# command that does. The relation is derived, never declared: a per-fixture
# shard tree names the fixtures it came from in its own file layout, and
# `go list -deps -test` names the packages whose source can move each shard.
# `.github/scripts/lib/golden-shards.mjs` holds both derivations and the stage
# ordering below; it is pinned by test/regression/golden_shard_coverage_test.go
# and test/regression/update_golden_baseline_order_test.go.
#
# The stage ordering is load-bearing and survives sharding unchanged, because
# the stages read opposite ends of a fixture:
#
# - `solver` runs BEFORE the fixture body. It re-derives every decision from the
#   fixture's `-- query.promql --` INPUT (parse -> lower -> optimize ->
#   classify), so the golden rewrite cannot change what it records — and the
#   body's default-tag lane runs TestSolverDecisionRatchet in ASSERT mode, which
#   fails on a brand-new fixture not yet in the routing baseline and would abort
#   before the body could finish.
# - `migration` and `parity` also run BEFORE it. `migration` re-derives every
#   Tier-0 explain report from its own committed dashboards and rules. `parity`
#   re-probes the parsers from `internal/**` and records which TXTAR fixtures
#   carry `-- parity --`; the body rewrites neither of those inputs. Running
#   them first puts their output inside the closing diff-stat, which is the
#   point: drift has to appear in the diff a contributor is asked to review,
#   not just in a file that quietly changed. Both are seconds of pure Go.
# - `cardinality` runs AFTER it. It profiles the fixture's RECORDED `-- sql --`
#   section verbatim: spec.PrepareRoundTrip rewrites that literal text and hands
#   it to profile.ProfileFixture. Run BEFORE the body it profiles the very SQL
#   this recipe is about to overwrite — and on a brand-new fixture `-- sql --`
#   is still EMPTY, so PrepareRoundTrip returns ok=false, the fixture is not
#   discovered as executable at all, and the row the chaining exists to record
#   is silently skipped (#1096 native_resample_offset, #1098
#   increase_left_edge_scan_bound).
#
# Within a shard, generators still run in declaration order, because that order
# IS a data dependency. One of them — promql's `TestRoundTripChDB`, which walks
# ~570 fixtures through chDB and was the longest single step of the whole recipe
# — additionally fans OUT across several processes over disjoint slices of its
# own corpus, and the next generator waits for all of them. Processes rather than
# goroutines because chdb-go caches one session per PROCESS and the runner tears
# that singleton down between fixtures for isolation (#1987), so in-process
# parallelism would race the teardown. `.github/scripts/lib/golden-shards.mjs`
# declares the leg count; test/spec/shard.go partitions the corpus by a hash of
# the fixture name, so enrolling fixtures does not re-shuffle the existing ones.
#
# Every chdb-tagged shard (promql, logql, traceql, cardinality) requires
# libchdb.so (`just chdb-install`); the runner fails fast without it rather than
# leaving stale `-- expected_rows --` behind (the PR #758 failure mode).
#
# Review the closing diff-stat before committing.

# Regenerate the fixture-derived artefacts named by SHARD. The shard argument is required.
update-golden *shards:
    @GOLDEN_SHARDS="{{shards}}" CHDB_INSTALL_PATH="{{CHDB_INSTALL_PATH}}" JUST_EXECUTABLE="{{just_executable()}}" node .github/scripts/golden-update.mjs

# Regenerate the parser-surface parity ledgers: test/surface-parity/inventory/
# (every PromQL / LogQL / TraceQL symbol the three parsers expose, with the
# in-process cerberus verdict for each, one JSON shard per (head, symbol) pair)
# and test/rejection-parity/catalogue/ (every rejection site in
# internal/{promql,logql,traceql}, one JSON shard per lowering source file).
# Both re-probe the parsers directly, so they drift on a PARSER-SURFACE change
# rather than a plan-shape one — a different trigger from the TXTAR goldens,
# the same failure shape if nobody regenerates them (#1595).
#
# Curation survives: a site already in the catalogue keeps its class, trigger
# query, endpoint and rationale. A site whose key MOVED — the key is derived
# from the rejection message, so editing that message relocates the entry —
# lands unclassified, and the verify test refuses an unclassified entry. That
# is the loud outcome, and it is why this is worth chaining rather than leaving
# to be rediscovered on a CI lane.
#
# test/surface-parity/promql-reference-verdicts.json / promql-reference-verdicts/
# are deliberately NOT here: `REGENERATE=1 node .github/scripts/promql-surface-gate.mjs`
# probes a live prom/prometheus container started with
# --enable-feature=promql-experimental-functions, so it needs Docker and a
# network pull. That is a heavier prerequisite than `update-golden` assumes,
# which puts it with the k3d/compose inventories and the compat parity
# baseline rather than in this recipe.
#
# test/surface-parity/logql-reference-verdicts.json and
# traceql-reference-verdicts.json are ALSO deliberately not here, for the
# analogous reason: they are computed by calling the AGPL grafana/loki /
# grafana/tempo parsers live, gated behind the `agpl_oracle` build tag so
# this (untagged) recipe — and the default `check` gate that also reaches
# this ledger via TestInventoryIsRegenerable — stay AGPL-free (#1520).
# Regenerate them explicitly when a probe changes:
#
#   CERBERUS_UPDATE_LOGQL_REFERENCE_VERDICTS=1 go test -tags agpl_oracle -run TestLogQLReferenceVerdictsAreCurrent ./test/surface-parity/
#   CERBERUS_UPDATE_TRACEQL_REFERENCE_VERDICTS=1 go test -tags agpl_oracle -run TestTraceQLReferenceVerdictsAreCurrent ./test/surface-parity/

# Regenerate the parser-surface and rejection parity ledgers.
update-parity-ledgers: update-parity-enrolment-baseline
    CERBERUS_UPDATE_INVENTORY=1 go test -count=1 ./test/surface-parity/ ./test/rejection-parity/
    @node .github/scripts/lib/regen-diff.mjs --label "parity ledgers" test/surface-parity/ test/rejection-parity/

CARDINALITY_BASELINE_TIMEOUT := "60m"

# Regenerate the cardinality/fan-factor ratchet baseline (perf-assessment
# Component C) from the current corpus profile. Re-profiles every executable
# TXTAR fixture under test/spec/** in-process via chDB and rewrites
# test/perf/cardinality-baseline/<head>/<name>.json — one shard per fixture,
# with the shards of fixtures the corpus no longer holds pruned.
# Requires libchdb.so (`just chdb-install`). Run this — and review the diff —
# whenever the ratchet test reports a NEW/REMOVED fixture or a deliberately
# intended fan_factor change; the diff is the built-in cost review (it shows
# each construct's absolute fan_factor). The gating assertion is
# TestCardinalityRatchet in the already-required `perf-guards` job.
#
# The profile pass is FANNED OUT across processes over disjoint slices of the
# corpus and closed by a step that asserts the slices together covered it
# (#2122) — see .github/scripts/cardinality-baseline-update.mjs for why a leg
# cannot prune another leg's rows, and why the closing step is what turns "every
# leg exited 0" into "the tree matches the corpus". The timeout below is per LEG,
# and stays generous because it is a hang detector, not a budget.

# Regenerate the cardinality / fan-factor ratchet baseline. Slow; needs libchdb.so.
update-cardinality-baseline:
    @CHDB_INSTALL_PATH="{{CHDB_INSTALL_PATH}}" CARDINALITY_BASELINE_TIMEOUT="{{CARDINALITY_BASELINE_TIMEOUT}}" node .github/scripts/cardinality-baseline-update.mjs
    @node .github/scripts/lib/regen-diff.mjs --label baseline test/perf/cardinality-baseline/

# Freeze the CURRENT cardinality baseline (test/perf/cardinality-baseline/)
# as test/perf/release-baseline/<version>/cardinality/ — the fixed
# reference TestReleasePerfRegression (release_regression_test.go) compares
# a future release candidate against, so a release-cycle's worth of
# individually-justified rolling re-baselines can still be caught in sum
# (cerberus issue #3150). Called automatically by `just release-prep` for
# the version being cut; call it by hand only to backfill a PAST release
# from its tag, e.g. `just capture-release-perf-baseline 1.19.0 v1.19.0`.
# Refuses to overwrite an existing capture — see the script's own doc.
capture-release-perf-baseline version ref="":
    node .github/scripts/capture-release-perf-baseline.mjs --version {{version}} {{ if ref != "" { "--ref " + ref } else { "" } }}
    @node .github/scripts/lib/regen-diff.mjs --label "release perf baseline" test/perf/release-baseline/{{version}}/

# Regenerate the routing-DECISION ratchet baseline (perf-assessment
# Component D) from the current PromQL corpus. Parses every `-- query.promql --`
# fixture under test/spec/promql/**, lowers it on the fixed eval grid
# (end=2026-01-01T00:00:00Z, range=1h, step=15s), optimizes it, and records the
# solver Planner's routing decision {routed, K, reason} plus the classifier's
# cost grid {n_anchors, fanout, cumulative_d, outer_range} under Mode=auto into
# test/perf/solver-decision-baseline/<name>.json — one shard per query, with
# the shards of queries the corpus no longer holds pruned.
# Pure Go — NO chDB — so it runs in the standard `check`/`just test` lane.
# Run this — and REVIEW THE DIFF — whenever the ratchet test reports drift or a
# NEW/REMOVED query. The diff classifies each moved row as ADVANCEMENT vs
# REGRESSION; a REGRESSION (route B->A, K down, or a routed query now rejected)
# MUST be justified in the PR with a real reason (a correctness fix that
# disqualifies the query), never accepted as a silent relaxation. The gating
# assertion is TestSolverDecisionRatchet in the already-required `check` job.

# Regenerate the routing-decision ratchet baseline. Pure Go; review the diff.
update-solver-decision-baseline:
    UPDATE_SOLVER_DECISION_BASELINE=1 go test -count=1 -run TestSolverDecisionRatchet ./test/perf/
    @node .github/scripts/lib/regen-diff.mjs --label baseline test/perf/solver-decision-baseline/

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

# Regenerate the scale-wall amplification and wall-ratio bounds. Needs libchdb.so.
update-scale-wall-baseline:
    @test -f "{{CHDB_INSTALL_PATH}}" || { echo "error: {{CHDB_INSTALL_PATH}} not found — run 'just chdb-install' first; the scale-wall bounds are measured by an in-process chDB run" >&2; exit 1; }
    UPDATE_SCALE_WALL_BASELINE=1 go test -tags chdb -count=1 -run TestScaleWallPin ./test/perf/
    @node .github/scripts/lib/regen-diff.mjs --label baseline test/perf/scale-wall-baseline.json

# Regenerate the PERF-SMOKE baseline (#2370 PR 1) — the committed per-sentinel
# memory ceilings the real-ClickHouse sentinel differential asserts against.
# Boots a real ClickHouse (testcontainers-go), seeds all three sentinels at
# their calibrated scale, issues each sentinel-N (max-of-N) times, and writes
# perf-smoke-baseline.json: for each sentinel the calibration-time
# max-of-N peak memory_usage plus the headroom-multiplied ceiling
# (sentinelBaselineHeadroom) the gate actually asserts against. Requires
# Docker. Run this — and REVIEW THE DIFF — only when a bound move is
# genuinely intended (a real, justified memory-cost increase); a silent
# loosen is exactly the regression this lane exists to catch. The gating
# assertion is TestPerfSmokeRealCH in the required `strict-scan` job.

# Regenerate the perf-smoke per-sentinel memory ceilings. Needs Docker; review the diff.
update-perf-smoke-baseline:
    @just _pull-retry {{CH_TEST_IMAGE}}
    @# Second tier — see the perf-smoke-integration recipe for why.
    @just _pull-retry {{CH_STRICT_SCAN_IMAGE}}
    UPDATE_PERF_SMOKE_BASELINE=1 go test -timeout 20m -tags=integration -count=1 -run TestPerfSmokeRealCH ./test/perf/smoke/...
    @node .github/scripts/lib/regen-diff.mjs --label baseline test/perf/perf-smoke-baseline.json

# Regenerate the METADATA QUERY-SIZE budget — the perf guard for the rendered
# SIZE of the combined /api/v1/series query, the axis the cardinality and
# scale-wall ratchets do not measure at all (they profile what ClickHouse does
# with a query; this measures how big the query text is before it is sent).
# Drives a broad metrics-explorer-shaped probe through the real in-process Prom
# handler over a recording Querier and records three bounds into
# test/perf/metadata-query-size-baseline.json: the total bound SQL per match[]
# selector, the largest single bound query, and the round-trip count. Bounds
# carry 1.25x headroom over the measurement, which is deterministic (no engine,
# no timing) — so this is pure Go and needs no chDB. Run this — and REVIEW THE
# DIFF — only when a bound move is genuinely intended; the two incidents this
# guards (PR #790, #799) both reached ClickHouse's 256KB max_query_size as a
# hard 502, and a silent loosen spends the margin they bought. The gating
# assertion is TestMetadataQuerySizeRatchet in the already-required `check` job.

# Regenerate the metadata query-size budget. Pure Go; review the diff.
update-metadata-query-size-baseline:
    UPDATE_METADATA_QUERY_SIZE_BASELINE=1 go test -count=1 -run TestMetadataQuerySizeRatchet ./test/perf/
    @node .github/scripts/lib/regen-diff.mjs --label baseline test/perf/metadata-query-size-baseline.json

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

# Regenerate docs/benchmarks.md from live measurements. Manual, not a gate; needs libchdb.so.
bench-report:
    @test -f "{{CHDB_INSTALL_PATH}}" || { echo "error: {{CHDB_INSTALL_PATH}} not found — run 'just chdb-install' first; the benchmark document is generated by an in-process chDB measurement pass" >&2; exit 1; }
    go run -tags chdb ./cmd/bench-report -out docs/benchmarks.md
    @node .github/scripts/lib/regen-diff.mjs --label "benchmark document" docs/benchmarks.md

# Regenerate docs/configuration.md from the single source of truth in
# internal/config: the CERBERUS_* env-key metadata (config.EnvDocs) and the
# LIVE viper loader defaults (config.DocDefaults). docs/configuration.md is a
# GENERATED file — do not hand-edit it; edit the EnvDoc metadata in
# internal/config/envdocs.go (or the preamble in cmd/cerberus/cmd_configdocs.go)
# and rerun this. The config-docs CI gate runs `git diff --exit-code` on the
# regenerated file, so a stale doc (or an undocumented new env var) fails CI.

# Regenerate docs/configuration.md from the internal/config env metadata and defaults.
gen-config-docs:
    go run ./cmd/cerberus config-docs -out docs/configuration.md
    @node .github/scripts/lib/regen-diff.mjs --label "configuration document" docs/configuration.md

