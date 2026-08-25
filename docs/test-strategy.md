# Test strategy

Cerberus is tested in 14 layers, ordered roughly cheapest-and-fastest
to slowest-and-most-realistic. Each layer pins a different class of
contract: AST shape, plan-IR invariants, optimizer behaviour, emitted
SQL bytes, semantic equivalence under chDB execution, function-surface
parity, HTTP wire conformance, process lifecycle, browser UX,
deterministic failure-mode resilience, performance ceilings,
compute-fan-out guards, and live-stack chaos resilience.

This document is the canonical map of what each layer covers, where
the tests live, which CI gates run them, and how to add a new test
inside each layer.

## At a glance

| Layer   | Name                                       | Lives in                                                                                                                                                                                                                                                                      | Catches                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              | Misses                                                                                                                                                                                                                                                                                                                                            |
| ------- | ------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1       | Parser smoke / AST-shape pinning           | `internal/{promql,logql,traceql}/parser_*_test.go`                                                                                                                                                                                                                            | Upstream parser renames a field, swaps an enum, changes a root-node type after a fork rebase                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | Semantic divergence below the AST surface                                                                                                                                                                                                                                                                                                         |
| 2a      | chplan IR snapshots in TXTAR               | `test/spec/<head>/*.txtar` (`-- chplan --` sections)                                                                                                                                                                                                                          | Lowering regressions that don't change emitted SQL bytes                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             | Optimizer-introduced regressions (covered by `-- chplan_optimized --` pair)                                                                                                                                                                                                                                                                       |
| 2b      | Lowering edge cases                        | `internal/{promql,logql,traceql}/lower_*_test.go`                                                                                                                                                                                                                             | Edge inputs (NaN, empty matrix, scalar coercions) that don't appear in golden fixtures                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               | Combinatoric blow-up — keep table-driven                                                                                                                                                                                                                                                                                                          |
| 3       | chplan IR invariants                       | `internal/chplan/{equal,walk}_invariants_test.go`                                                                                                                                                                                                                             | `Equal()` false-positives / negatives; `Walk` / `Children` ordering drift; pointer-identity                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | Lowering bugs — IR is generic                                                                                                                                                                                                                                                                                                                     |
| 4       | Optimizer rule properties                  | `internal/optimizer/{rule_interaction,termination,decision_pins,regression_bank}_test.go`                                                                                                                                                                                     | Rule-pair commutation, non-termination, mis-rewrites, decision-pin regressions                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       | Cross-rule chDB row drift (covered by Layer 6 chDB property)                                                                                                                                                                                                                                                                                      |
| 5       | chsql Frag + QueryBuilder goldens          | `internal/chsql/{frag_goldens,query_builder_invariants,emit_node_goldens}_test.go`                                                                                                                                                                                            | Frag render shape, slot-ordering invariants, append/replace semantics, Build idempotency                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             | SQL that compiles but executes incorrectly — covered by Layer 6                                                                                                                                                                                                                                                                                   |
| 6a      | PromQL chDB roundtrip                      | `test/spec/promql/*.txtar` + `internal/promql/lower_test.go`                                                                                                                                                                                                                  | Pre-optimizer and post-optimizer row drift, plus live reference parity                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               | Behaviour outside the seeded corpus                                                                                                                                                                                                                                                                                                               |
| 6b      | LogQL chDB roundtrip                       | `test/spec/logql/*.txtar` + `internal/logql/lower_test.go`                                                                                                                                                                                                                    | Same as 6a for LogQL                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | Same as 6a                                                                                                                                                                                                                                                                                                                                        |
| 6c      | TraceQL chDB roundtrip                     | `test/spec/traceql/*.txtar` + `internal/traceql/lower_test.go`                                                                                                                                                                                                                | Same as 6a for TraceQL                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               | Same as 6a                                                                                                                                                                                                                                                                                                                                        |
| 6d      | Function-surface parity ledger             | `test/surface-parity/`, `test/rejection-parity/`, `test/oracle/inventory/`                                                                                                                                                                                                    | A symbol cerberus fails to lower (wrong-reject) or answers when the reference rejects (wrong-accept)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | Whether an accepted symbol returns the *right rows* (Layer 6a-c)                                                                                                                                                                                                                                                                                  |
| 6e      | Strict-scan differential (real CH)         | `test/spec/strictscan_integration_test.go` + `test/spec/metadata_endpoints_realch_integration_test.go` + `internal/routerrules/realch_integration_test.go` + `internal/optcorpus/{queryexit,enummigrate}_realch_integration_test.go`                                          | Emit-type bugs where chDB coerces a column (UInt8/UInt64 -> `*float64`) but prod clickhouse-go strict-scans and 502s. The router-corpus arm extends this to the OFFLINE corpus WRITE + READ seams e2e never touch (#1064). The metadata-endpoints arm covers the non-matrix Loki/Prometheus label-values + index-stats/volume + metadata decoders (`QueryLabelSets` / `QueryIndexStats` / `QueryStrings` / `QueryIndexVolume` / `QueryMetricMeta`), which bypass `engine.QueryPlan` entirely and previously had no fixture corpus at all (#1634). The optcorpus arms cover the two seams a fake passes SILENTLY: a `system.query_log.type` predicate naming a non-member (ClickHouse coerces the comparison to String and matches nothing), and a deployed `exit_status` Enum8 narrower than the member set the binary writes (`CREATE TABLE IF NOT EXISTS` is a no-op against an existing table, so every batch is rejected with `unknown element`) | Tempo search-row decoders (see #1635, which has a hidden pipeline stage to reproduce); rows that scan but are *wrong* (Layer 6a-c)                                                                                                                                                                                                                |
| 6f      | Connection-teardown differential (real CH) | `internal/chclient/conn_teardown_integration_test.go`                                                                                                                                                                                                                         | Whether a pooled connection SURVIVES a cursor teardown: clickhouse-go releases a connection when its query ends on a live context and destroys the socket when it ends on a cancelled one, and the difference is asserted from both ends — the driver's pool census and the server's own live TCP-session count, read over an independent observer client. The probe pins `max_block_size` so the result streams past the driver's block-buffer depth: with a single block the driver's teardown select is a coin flip, and the arms stop diverging                                                                                                                                                                                                                                                                                                                                                                                                  | Which callers use the contract (Layer 10 pins the prom handler seam); the ordering inside `CloseCursor` itself (unit tests). It rides the required `strict-scan` lane, so a real-CH teardown regression blocks the merge; the Layer 8 / Layer 10 pins on the `check` lane hold the ordering and startup sequencing independently of a live server |
| 7       | HTTP handler conformance                   | `internal/api/{prom,loki,tempo}/conformance_test.go`                                                                                                                                                                                                                          | Wire-format drift, error envelope shape, header pins, range-param parsing, admission control                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | Real-network failure modes (Layer 10) and UX flows (Layer 9)                                                                                                                                                                                                                                                                                      |
| 7b      | Consumer-corpus replay                     | `test/consumer-corpus/`                                                                                                                                                                                                                                                       | Consumer-decode drift on captured Grafana request shapes (proto envelopes, bare JSON, drilldown queries)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             | Shapes Grafana hasn't been observed sending — crawler mines captures                                                                                                                                                                                                                                                                              |
| 8       | System / process lifecycle                 | `internal/config/`, `internal/api/health/`, `cmd/cerberus/`, `internal/telemetry/`, `schema/ddl/`, `test/regression/{telemetry_provider_ordering,ch_conn_lifetime_margin}_test.go`                                                                                            | Env-var contract, `/readyz` TTL coalescing, OTel telemetry attributes, signal-driven shutdown                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | Cross-process behaviour — Compose / k3d (Layer 9)                                                                                                                                                                                                                                                                                                 |
| 9       | Playwright UX flows                        | `test/e2e/playwright/*.spec.ts`                                                                                                                                                                                                                                               | Grafana Explore / Logs / Trace panel request sequences against cerberus's three datasource APIs                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | Pure backend logic — Layers 1–8                                                                                                                                                                                                                                                                                                                   |
| 10      | Chaos / failure-mode (deterministic)       | `internal/{chclient,api/{prom,loki,tempo,admit}}/chaos_test.go`, `test/regression/{goleak,cursor_teardown_order}_test.go`                                                                                                                                                     | CH-failure, mid-stream cursor faults, goroutine leaks, panic-mid-handler slot release, CH-disconnect circuit breaker (stubbed-querier injection), cursor-close-before-cancel ordering at the HTTP seam                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               | Long-tail platform-specific failures; real-deployment fault behaviour (Layer 13)                                                                                                                                                                                                                                                                  |
| 11      | Perf benchmarks + alloc regressions        | `internal/*/*_bench_test.go`                                                                                                                                                                                                                                                  | Allocation count regressions per pipeline stage; bounded-RSS streaming cursor                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | Wall-clock perf regressions — left to `perf-benchmark.yml` benchstat                                                                                                                                                                                                                                                                              |
| 12      | Compute fan-out guards                     | `internal/perf/fanout`, `test/perf/` (scaling harness; cardinality / wall / decision ratchets)                                                                                                                                                                                | Upward fan-factor regression, a new unbounded shape (`CrossJoin` / uncapped `WITH RECURSIVE`), super-linear scaling                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  | A fan-out in a construct with no guard and no fixture (the nightly profiler)                                                                                                                                                                                                                                                                      |
| 13      | Live-stack chaos (real faults)             | `.github/scripts/chaos-run.mjs`, `test/e2e/chaos/manifests/`                                                                                                                                                                                                                  | Resilience contracts under REAL faults against the k3d stack: CH-outage breaker trip + recovery, query-timeout breaker-neutrality, replica resilience, admit shed, network partition                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | Pure backend logic (Layers 1-8); steady-state UX (Layer 9) — chaos is fault-injection only                                                                                                                                                                                                                                                        |
| 14      | Migration-scenario e2e (scheduled)         | [`docs/migration-testing.md`](migration-testing.md) (harness under `test/e2e/migration`; Tier-0 offline scenarios driven by godog + `test/e2e/migration/cmd/scenarios`; Tier-1 substrate assertions build-tagged `migration_tier1`, its structural pins in `test/regression`) | The operator migration journey per archetype: offline harvest/explain/classify/rulegraph/gate correctness, live query-result parity (`cerberus migrate verify` diverge-count zero) over a Tier-1 stack whose reference Prometheus/Loki/Tempo, shared reference configs and ClickHouse tag are pinned and single-sourced, fed by one deterministic all-signal fixture written to both sides and read back over a seeder-published closed window (metric matrices, log entries and trace identities compared against an in-process oracle, plus a negative control that proves a disagreement is observable), ingest-bridge type reconstruction, schema/label/histogram fidelity, alert-firing parity via a real shadow ruler                                                                                                                                                                                                                          | Pure backend logic (Layers 1-13) — a workflow lane, not a code lane; a lowering/emitter bug reaches it only when an archetype corpus names that shape; customer topologies beyond the eight seeded archetypes                                                                                                                                     |

## CI gates

Cerberus runs a fixed **two-tier** test fence (#2230) — see
[`operations.md`'s "Two-tier test fence"](operations.md#two-tier-test-fence-merge-gate-vs-release-gate)
for the design rationale. Every ordinary PR waits on the **merge gate**: the
sixteen **required** status checks on `main` — `check`, `lint`, `forbid-skip`,
`forbid-deferral`, `pr-body`, `chart-validate`, `probe`, `agpl-clean`,
`config-docs`, `link-check`, `schema-ddl`, `coverage`, `strict-scan`,
`quickstart`, `CodeQL`, and `property (PromQL + LogQL + TraceQL, rapid
N=500)`. This is a small, fixed set chosen for speed and build/correctness
coverage, not a per-diff selection.

Everything else in this section's table — `roundtrip (<head>)`,
`compatibility/<head>` (all four required-posture heads), `perf-guards`,
`mutation`, `compose-smoke`, `dashboard`, `profile`, and the informational
`integration-<head>` / `chdb-build` / `perf-benchmark` lanes — is a
**release** gate. They short-circuit to a green no-op on an ordinary pull
request and a merge-group entry, and do their real work on push-to-main, a
maintenance `release/*.x` push, a nightly schedule, a manual dispatch, or a
`release/*` head-branch pull request. The heaviest of them (`compose-smoke`,
`dashboard`, `profile`) additionally boot real substrate (a compose stack per
shard, a k3d cluster per shard, a chDB walk of the whole executable corpus)
and were the longest lanes on the board by a wide margin before the split.
Every release-gate lane named in release.yml's `RELEASE_REQUIRED_CHECKS`
blocks a publish: nothing publishes until each has posted a green check-run on
the commit being shipped. `mutation` is the one exception with a real,
non-blanket per-PR mechanism: rather than a blanket no-op, it keeps a
diff-scoped selection (only the gremlins phases whose package the PR touched)
for early author-time signal, but it is informational everywhere — never
required by either gate. The gate moved; it did not disappear.

`quickstart` is the required, one-stack Compose canary. On every main push and
every non-documentation pull request and merge-group projection it checks out the
exact proposed SHA, proves the tree is clean, executes the repository-root
README command with the exact `docker compose up --wait` argument vector, and
probes liveness, readiness, the published Grafana root, Grafana's database,
the three provisioned datasource records, one exact seeded query through each
Grafana datasource proxy, and every actual target expression in the provisioned
home dashboard through its named proxy. The
root README is always in scope even though it is Markdown. An empty or
uncomputable diff runs;
selector failure, missing output, a skipped/cancelled/timed-out selected run,
or teardown failure makes the stable `quickstart` context red. Documentation-
only changes short-circuit only after that explicit successful decision. The
lane registry models this as the first-class `non_documentation` posture, not a
set of impact-package globs: a non-documentation path already owned by another
lane must still select this canary. Non-PR runs use a unique concurrency group,
so rapid main pushes cannot replace a pending canary before it starts.

The exhaustive `compose-smoke` Playwright matrix remains a release gate. It
short-circuits on every ordinary pull request because repeating the same stack
boot per browser shard would duplicate `quickstart` and restore the long merge
tail. Pushes, nightly/manual qualification, and `release/*` pull requests still
run it in full; `.github/scripts/compose-smoke-scope.mjs` owns and tests that
event policy. Every other job below is
informational — it runs (push-to-main, nightly, or dispatch) and reports,
but a red result does not block a merge. Informational does **not** mean
tolerated: a red informational lane is a real failure to fix, it is just
not wired as a branch-protection gate (typically because it needs the chDB
substrate, a Docker stack, or a soak streak before promotion).

Replaceable deep-test workflows coalesce only a `push` or `schedule` run whose
ref is exactly `refs/heads/main`. Main pushes share one `latest-main-push`
group; scheduled runs key their group by the exact cron expression. A routine
push therefore cannot erase nightly-only coverage, and two differently scoped
schedules cannot erase each other. Every pull request, merge group, manual
dispatch, reusable-workflow call, maintenance branch, tag, release event, and
unknown event/ref pair receives a unique run-id group. The exhaustive E2E workflow,
the required `quickstart`, post-merge generated-artifact drift, core gates,
publication, stateful mirroring, and monitors are explicit negative controls.
`.github/scripts/main-coalescing.mjs` binds the enrolled workflow expressions
to the lane registry, and its tests exercise the cancellation decision table.

One subtlety on the three `compatibility/<head>` checks: each gates in two
layers. The harness is *scored* — it accumulates per-case results into
`compat-score.json` plus a per-case roster in `compat-cases.json`, and exits
0 even when an individual case diverges
([#503](https://github.com/tsouza/cerberus/pull/503)), so the harness step
alone reddens the job only on infrastructure breakage. The gate is the step
after it: `.github/scripts/compat-ratchet.mjs` compares the run's roster
against the committed per-head roster in
`compatibility/parity-baseline/` and **fails the required job on any
case that moved** — a recorded case that now diverges, a recorded case that
stopped running, a new case that diverges on arrival, or a new case that
passes but is not yet recorded. Because it gates on case *identity* rather
than on a count, a regression cannot hide behind an unrelated case that
started passing in the same run.
`compatibility/prometheus-forced-route` runs the harness with
`FAIL_ON_DIFF=1`, hard-failing inside the harness step itself.
See [`compatibility.md`](compatibility.md#parity-regression-ratchet-the-gate)
for the rosters themselves and the procedure for moving one.

| Gate                                    | Workflow (job)                                         | Trigger                             | Required? | Scope                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| --------------------------------------- | ------------------------------------------------------ | ----------------------------------- | --------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `check`                                 | `ci.yml` (`check` over `check-test` + `check-build`)   | PR + queue + push                   | Required  | `just test-unit` (`go test -race ./...`) and, concurrently, `just build` + `just vet-tagged` + the oracle module + the `go mod tidy` drift gate. Default-tag lanes: 1, 2a, 2b, 3, 4, 5, **6d** (surface / rejection / inventory parity ratchets), 7, 7b stub, 8, 10, 11, 12 solver-decision ratchet                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `lint`                                  | `ci.yml` (`lint`)                                      | PR + queue + push                   | Required  | `golangci-lint` v2 + `go-arch-lint` + `actionlint` + `commitlint` (PR) + `markdownlint-cli2` + `doc-refs` + the Layer-14 story/scenario coverage ratchet                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `forbid-skip`                           | `ci.yml` (`forbid-skip`)                               | PR + queue + push                   | Required  | `t.Skip*`, "not implemented" in prod code, soft-assertion / silent-recover, escape-hatch patterns, `should_skip` overlay, scenario-suppressing `.feature` tags + godog skip routes, regex self-test, doc-count gate (`doc-counts.mjs`), auto-labeler self-tests (`pr-type-label.mjs --self-test`, `issue-label.test.mjs`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `forbid-deferral`                       | `forbid-deferral.yml` (`forbid-deferral`)              | PR (incl. edited) + queue + push    | Required  | The change's own additions — PR description, commit messages in its range, `+` lines of its diff — scanned for the marker classes in `DEFERRAL_MARKERS` (`.github/scripts/forbid-deferral.mjs`); each hit must cite an OPEN issue (not a pull request) in its scope: a heading's whole section, a paragraph otherwise, `CITATION_WINDOW_LINES` in a diff. Its own workflow, so `edited` re-runs it on a corrected description. A cited number the API cannot resolve is separated from one that names nothing by a one-per-run capability probe (repository read, then issue-list read), so a token without `issues: read` fails as a permission fault instead of as the author's prose. Pinned by `forbid-deferral.test.mjs` + `test/regression/forbid_deferral_trigger_test.go` + `test/regression/forbid_deferral_permission_test.go`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `pr-body`                               | `pr-hygiene.yml` (`pr-body`)                           | PR (incl. edited) + queue + push    | Required  | Rejects a pull request whose description is empty or a stub (`.github/scripts/pr-body-check.mjs`): boilerplate that carries no description — the AI footer, `Co-authored-by:` trailers, HTML/template comments, image-only lines — is stripped, and the remainder must be at least `MIN_CHARS` of meaningful text and not a lone placeholder token. Only the `pull_request` event carries a body in its payload; on the queue and on a push the description is resolved from the head commit's associated pull request through `forbid-deferral`'s own resolver, so the two gates cannot drift on what "the description" is. A commit with NO associated pull request passes — it has no description to be a stub — and `pr-body-check.test.mjs` drives the real resolver to pin that the exemption keys off its origin label rather than off emptiness                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `update-golden-guard`                   | `update-golden-guard.yml` (`update-golden-guard`)      | PR (opened/sync/reopened)           | Required  | Closes the #2350 race: `update-golden.yml` can be dispatched against an open PR's own branch, and nothing previously stopped that PR from merging (deleting its branch) while the dispatch was still regenerating, silently losing the already-computed diff. Polls the Actions API (`.github/scripts/update-golden-guard.mjs`) for an `update-golden.yml` run whose `run-name: update-golden[<branch>]` names this PR's head branch and is still `queued`/`in_progress`; blocks for as long as one is, clears the moment none is, and never reads the run's own conclusion — a finished dispatch is no longer a race hazard whatever it concluded. Pinned by `update-golden-guard.test.mjs`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `probe`                                 | `chdb.yml` (`probe`)                                   | PR + queue + push + nightly         | Required  | chDB driver sanity (`TestChDBProbe`) + `just test-chdb` (api-handler + Layer 7b chdb lane + the `system.query_log` Enum8 resolution probe behind the optimizer corpus reconciler)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `roundtrip (promql)`                    | `chdb.yml` (`roundtrip-promql` aggregator)             | PR + queue + push + nightly         | Release   | Pre-optimizer `test/spec/promql` and post-optimizer `internal/promql` chDB execution plus live reference parity, using `chdb` (Layer 6a). Sharded across separate runners since #2629 — each shard still fans out ~3 in-runner processes (`chdb-roundtrip.mjs`'s `FANOUT.promql`) — with an aggregator posting the exact status-check name the shard matrix itself does not. No-op on an ordinary PR / merge-group entry (`run_heavy`); real on push / schedule / dispatch / a `release/*` PR                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `roundtrip (logql / traceql)`           | `chdb.yml` (matrix)                                    | PR + queue + push + nightly         | Release   | Pre-optimizer `test/spec/<head>` and post-optimizer `internal/<head>` chDB execution plus live reference parity for both heads, using `chdb,agpl_oracle,chdb_agpl_oracle` (Layer 6b-c). No-op on an ordinary PR / merge-group entry (`run_heavy`); real on push / schedule / dispatch / a `release/*` PR                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `compatibility/prometheus`              | `compatibility.yml` (`compatibility/prometheus`)       | PR + queue + push + nightly + disp. | Release   | PromQL differential vs reference Prometheus (`prometheus/compliance` harness) + parity-regression ratchet vs `compatibility/parity-baseline/`. No-op on an ordinary PR / merge-group entry (`run_heavy`); real on push / schedule / dispatch / a `release/*` PR                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `compatibility/loki`                    | `compatibility.yml` (`compatibility/loki`)             | PR + queue + push + nightly + disp. | Release   | LogQL differential vs reference Loki + vendored `loki:pkg/logql/bench` corpus + parity-regression ratchet vs `compatibility/parity-baseline/`. No-op on an ordinary PR / merge-group entry; real on push / schedule / dispatch / a `release/*` PR                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `compatibility/tempo`                   | `compatibility.yml` (`compatibility/tempo`)            | PR + queue + push + nightly + disp. | Release   | TraceQL differential vs reference Tempo (cerberus-owned TXTAR corpus), two transport arms — HTTP (`diff`) and gRPC/h2c `StreamingQuerier` (`diff-grpc`, #1453) — each with its own parity-regression ratchet vs `compatibility/parity-baseline/` (`heads.tempo` / `heads.tempo-grpc`). No-op on an ordinary PR / merge-group entry; real on push / schedule / dispatch / a `release/*` PR                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `quickstart`                            | `quickstart.yml` (`quickstart`)                        | non-doc PR + queue + push           | Required  | One clean projected-trunk checkout; exact repository-root `docker compose up --wait`; `/healthz`, `/readyz`, Grafana `/`, database health, canonical datasource metadata, exact seeded Prom/Loki/Tempo queries, and every provisioned home-dashboard target expression through Grafana; unconditional `down -v --remove-orphans`; fail-closed selector and rollup                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `compose-smoke`                         | `e2e.yml` (`compose-smoke`)                            | release PR + push + nightly         | Release   | Root Compose stack + Playwright dashboard catch-net and `compose` crawl (lean push, full release/nightly); ordinary pull requests use the required single-stack `quickstart` context instead                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `chart-validate`                        | `chart-ci.yml` (`chart-validate`)                      | PR + queue + push + dispatch        | Required  | Helm chart lint + `helm-docs` README drift gate + `kubeconform` render + render assertions (split PDBs / derived `GOMEMLIMIT`) + `ct lint` over `deploy/helm/cerberus`; short-circuits to a green no-op when no chart file changed, so it reports on every PR                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `compatibility/prometheus-forced-route` | `compatibility.yml` (forced-route job)                 | PR + queue + push + nightly + disp. | Release   | Corpus-wide proof that the solver route B (`CERBERUS_EVAL_ROUTE=sharded`) is byte-identical to route A vs reference Prom. No-op on an ordinary PR / merge-group entry; real on push / schedule / dispatch / a `release/*` PR                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `compatibility/promql-surface`          | `compatibility.yml` (`compatibility/promql-surface`)   | PR + queue + push + nightly + disp. | Info      | Re-probes a **flag-ON** reference Prometheus over every `parser.Functions` symbol; asserts cerberus rejects nothing the reference accepts. Pins `test/surface-parity/inventory/` against drift (Layer 6d, live half). No-op on an ordinary PR / merge-group entry; real on push / schedule / dispatch / a `release/*` PR                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `compatibility/prometheus-floor`        | `compatibility.yml` (`compatibility/prometheus-floor`) | PR + queue + push + nightly + disp. | Info      | Same PromQL differential corpus as `compatibility/prometheus`, but against `clickhouse/clickhouse-server:24.8` — cerberus's declared `minCHBase` (`internal/preflight/preflight.go`) — instead of the 26.5 every other lane runs. Forces `internal/chopt`'s auto-picker to resolve every >24.8 feature OFF, so the 24.8-safe fallback branches (`ts_grid_*` fan-outs, `condition_cache` absence) execute end to end instead of the newer-server native paths every other substrate always takes. Gated by the same `compat-ratchet.mjs` against the same `compatibility/parity-baseline/` `prometheus` roster (#1500). This is the ONE deliberate exception to "every compatibility image tracks `versions.yaml`'s `chdb_substrate`": `.github/scripts/clickhouse-version-sync.mjs` check (f) reads this job's `CH_IMAGE` and asserts it equals `min_clickhouse` instead, so the exception is a tracked invariant rather than an unenforced drift point. No-op on an ordinary PR / merge-group entry; real on push / schedule / dispatch / a `release/*` PR                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `perf-guards`                           | `chdb.yml` (`perf-guards` + `perf-guards-shard`)       | PR + queue + push + nightly         | Release   | `just perf-chdb` (`go test -tags chdb ./test/perf/...`): the cardinality / scale-wall ratchets, per-construct scaling harness, and cycle-guards (Layer 12 chdb lanes). SHARDED across 8 runner processes over disjoint slices of the TXTAR corpus (`PERF_SHARD_INDEX` / `PERF_SHARD_COUNT`, partitioned by `test/perf/profile/shard.go`) because `TestCardinalityRatchet`'s runtime is a straight line in corpus size and chdb-go's one-session-per-process cache rules out in-process parallelism (#2002, #1987). `perf-guards` is the AGGREGATOR that posts the release-gate context; the matrix children report as `perf-guards-shard (n)`. No-op on an ordinary PR / merge-group entry (`run_heavy`); real on push / schedule / dispatch / a `release/*` PR                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `perf-profile`                          | `perf-profile.yml` (`profile`)                         | release PR + push + nightly + disp. | Release   | Corpus-wide compute-fan-out profiler over every executable TXTAR fixture (EXPLAIN + per-subquery `count()` fan-factor); top-40 to step summary (Layer 12, Component B)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `perf-benchmark`                        | `perf-benchmark.yml` (`benchstat diff`)                | PR (path-match) + weekly + dispatch | Info      | benchstat wall-clock regression vs baseline (Layer 11). No-op on every pull request, including a path-matching one; real only on the weekly schedule or manual dispatch                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `dashboard`                             | `e2e.yml` (`dashboard`)                                | release PR + push + nightly + disp. | Release   | k3d + cerberus + Grafana + Playwright full smoke + `k3d` crawl (Layer 9)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `chaos`                                 | `e2e.yml` (`chaos`)                                    | push + nightly + dispatch           | Info      | k3d live-stack fault injection — resilience contracts under real faults (Layer 13). Phase-1 on push; full set on nightly/dispatch                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `startup-bench`                         | `e2e.yml` (`startup-bench`)                            | push + nightly + dispatch           | Info      | cerberus reaches `/healthz` under 2 s against an inline ClickHouse                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `strict-scan`                           | `strict-scan.yml` (`strict-scan`)                      | PR + queue + push + nightly + disp. | Required  | Strict-scan differential (Layer 6e): executes the matrix golden SQL corpus against a real ClickHouse (testcontainers) through the production strict scan, then runs the router-corpus WRITE + READ seams and the optcorpus query_log-predicate + exit_status-enum reconciliation arms; fails on any coercion error chDB hides. Also hosts the real-CH differentials that need a driver + server rather than chDB: TraceQL spans-scan resource bounds, the Tempo traces-scan WINDOW partition-pruning differential (#1509), solver memory apportionment, the route-B result differential (the IDENTICAL query answered via route A and forced route B against the same real ClickHouse through the real production emitter, since every other route-B test runs on chDB only or drives a fake emitter), the Loki + Prometheus metadata/label-values endpoints differential (one real case per endpoint through the real `loki.Handler` / `prom.Handler`, #1634), the histogram real-exporter-schema differential (native exp-histogram quantile + columnar-decode fall-back + classic-histogram bucket discovery against the REAL upstream `clickhouseexporter` table shapes, #1642), the connection-teardown pool census (Layer 6f), and the perf-smoke sentinel differential (#2370 PR 1: the real-CH memory-bounding guard for the #2364 incident class — drives the mounted production `prom` + `tempo` handlers over real HTTP at the scale each of #2364's root-cause mechanisms needs to actually engage, and reads peak per-query memory back from `system.query_log` via `optcorpus.CHQueryLogSource`, the same reader the async query_log corpus reconciler uses)                          |
| `mutation` (per phase)                  | `mutation.yml` (matrix)                                | PR + queue + push + nightly + disp. | Info      | gremlins per package at the phase efficacy floor (table in `.github/scripts/mutation-phases.mjs`). On a PR only the phases whose scope the PR changed; push / nightly / dispatch / `release/*` PRs sweep the full matrix. Not required by either gate — informational everywhere; kept diff-scoped rather than a blanket no-op purely for early author-time signal. [De-gated on the publish path](operations.md#de-gated-lanes-on-the-publish-path) too                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `property`                              | `property.yml`                                         | PR + queue + push + nightly + disp. | Required  | rapid-driven oracle property tests, PromQL / LogQL / TraceQL (Layer 4 + 6 cross-check)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `coverage`                              | `coverage.yml`                                         | PR + queue + push + nightly + disp. | Required  | merged default-tag + chdb-tagged cover profile, per-package summary, and the per-package floor gate (`.github/scripts/coverage-summary.mjs` vs `test/coverage-floor/`). The floor is what makes this a gate rather than a report: it compares in BOTH directions — below its floor, carrying statements with no floor, carrying statements with a floor of `0` (a floor nothing can fall through), and a floor whose package contributed nothing all fail — so coverage cannot rot one package at a time behind a green run. The profile is built with `-coverpkg` over the lane's own `go list` package set (which is why the go.mod-`ignore`d vendored upstream trees stay out of the ledger), so a package is measured by every test binary that executes it rather than only its own: extracting a helper into its own package no longer resets its measured coverage to zero. The chdb-tagged half additionally shards `test/perf`'s `TestCardinalityRatchet` corpus walk — the same straight-line-in-corpus-size runtime `perf-guards` shards above, which without sharding here timed out the whole job at 40m — via `.github/scripts/perf-coverage-fanout.mjs`: the Justfile leg runs `PERF_SHARD_INDEX=1`/`PERF_SHARD_COUNT=3` directly, and the script fans the other `RATCHET_FANOUT-1` shards out CONCURRENTLY within the same job, the same process fan-out technique `property-fanout.mjs` established for the identical timeout shape in `test/property` (chdb-go's one-session-per-process cache rules out an in-process `t.Parallel()` alternative). `just update-coverage-floor` ratchets up only, and refuses to record a `0`; a drop must be a hand-edited line in the diff     |
| `migration-tier1`                       | `migration-e2e.yml` (`migration-tier1`)                | push + nightly + dispatch           | Info      | Layer 14 Tier-1 substrate: pinned reference Prometheus / Loki / Tempo + ClickHouse + collector + cerberus, one all-signal fixture seeded into both sides, `migration_tier1`-tagged parity assertions plus the `@tier1` Gherkin scenarios, one compose lifecycle per run                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `migration-tier2`                       | `migration-e2e.yml` (`migration-tier2`)                | push + nightly + dispatch           | Info      | Layer 14 Tier-2 substrate: the Tier-1 stack plus the query-only external ruler (Grafana-managed alerting against cerberus), its recording-rule write-back bridge and the dead-end notification receiver, running the `@tier2` Gherkin scenarios and the `migration_tier2`-tagged substrate self-check. `needs: migration-tier1` — firing parity is not provable before query parity                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `migration-e2e`                         | `migration-e2e.yml` (`migration-e2e`)                  | push + nightly + dispatch           | Info      | Layer-14 migration-scenario lane: the Tier-0 godog suite over committed archetype fixtures (offline, no Docker). Its coverage ratchet + detector guard run on the required `lint` job                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `CodeQL`                                | `codeql.yml` (`CodeQL`)                                | PR + queue + push + weekly          | Required  | Static security analysis (GitHub Advanced Setup, not server-side default setup) over Go, JavaScript/TypeScript, GitHub Actions YAML and Python via a `go, javascript-typescript, actions, python` `analyze` matrix that uploads SARIF per language; the umbrella `CodeQL` job — the required check name — passes only when every language's `analyze` job succeeds, so branch protection still blocks on all four analyses through the one context. `CodeQL` evaluates **new** alerts against the PR, so it never re-reports a pre-existing alert — a dismissed one stays dismissed until the code that produced it changes. Being a workflow the repo owns rather than server-side default setup, it declares `merge_group` alongside `push`, `pull_request` and a weekly `schedule` — see "Merge-queue posture" below                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `agpl-clean`                            | `ci.yml` (`agpl-clean`)                                | PR + queue + push                   | Required  | `go list -deps ./cmd/cerberus` reachability check: fails if any AGPLv3 `grafana/loki/*` or `grafana/tempo/*` package (other than the Apache-licensed `pkg/tempopb` wire types) is reachable from the binary (`.github/scripts/agpl-clean.mjs`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `config-docs`                           | `config-docs.yml` (`config-docs`)                      | PR + queue + push                   | Required  | Regenerates `docs/configuration.md` from `internal/config`'s live `CERBERUS_*` env-key metadata and viper defaults, and fails on any drift; NOT short-circuited on docs-only PRs — a hand-edit to the generated doc is exactly the drift this gate exists to reject                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `link-check`                            | `ci.yml` (`link-check`)                                | PR + queue + push                   | Required  | `lychee --offline --include-fragments`: every relative `[text](./other.md#anchor)` link resolves to a file that exists, and every `#fragment` resolves to a real heading/anchor. No network, so it cannot flake on a slow/blocked host; external `http(s)://` links are checked separately by the schedule-only `link-check-external.yml`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `schema-ddl`                            | `schema-integration.yml` (`schema-ddl`)                | PR + queue + push + nightly         | Required  | `internal/schema/ddl`'s `integration`-tagged tests against a real ClickHouse (testcontainers-go): the `AUTO_CREATE_SCHEMA` DDL actually accepted by a live server, including that a Replicated database really replicates (`system.replicas`) — a class of bug the rendered-SQL unit tests and the chDB lanes cannot see (three production incidents shipped through that blind spot)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |

### Test-fence enrollment contracts

The lane registry describes ownership and policy, but registry metadata alone
is never accepted as proof that a test executes. The required `check` gate
enforces that distinction with
`test/regression/tagged_test_enrollment_test.go`. It parses every
build-constrained root-module `_test.go` file into a Go AST and discovers each
`Test*`, `Fuzz*`, and executable `Example*` symbol (`TestMain` and examples
without an output directive are not assertions). Independently, it reads real
workflow `run` steps, the statically named Just recipes they invoke, direct
`go test` commands, and the migration execution adapter. A registry lane may
bind those two discoveries only when its workflow job, build-tag roster,
package arguments, optional `-run` expression, source globs, and
command/recipe/script entrypoint all match. `go vet`, compile-only test
invocations, prose, and dynamic commands are not execution evidence. Execution
must also be reachable and failure-propagating: a statically disabled workflow
step, unconditional `continue-on-error`, unmodelled shell control flow, a pipeline without
`errexit`/`pipefail`, a non-Linux runner, a non-root working directory, or an
override of Go's build-context environment cannot certify a symbol. Prior
`GITHUB_ENV` writes are included in that environment check. The coverage
recipe's intentional local conditional is accepted only when CI requires both
named coverage lanes and the fail-closed summary join consumes their result.
Adding a tagged assertion without a matching executing lane therefore fails
closed.

Each query head also has a structural oracle floor in
`.github/scripts/ci-lane-contract.mjs`. PromQL Layer 6a, LogQL Layer 6b, and
TraceQL Layer 6c must each retain source-applicable `execution`, `property`, and
`reference` oracle providers that run on `main` and are required before a
release. This is a semantic contract rather than a list of lane IDs: a provider
may be renamed or split, but deleting it, changing its oracle class or posture,
or moving it out of that head's risk domain fails registry validation. One
head's providers cannot satisfy another head's floor.

### Merge-queue posture

A merge queue validates a pull request against the trunk it would actually
create: GitHub builds a projected commit — the PR replayed on top of `main` plus
every entry ahead of it — and dispatches the `merge_group` event against it.
Branch protection then waits for the same required contexts on that projected
commit, which makes two properties load-bearing.

**Every required context posts on the projected trunk.** Each workflow owning a
branch-protection context declares `merge_group:` alongside `pull_request:`, and
the check-run names are byte-identical across the two events. That discipline
holds even though every currently-required context is a single fixed job name
rather than a matrix leg — the merge gate is deliberately small and fast
(#2230) — because a required context still has to be a matrix-safe name the
moment anything promotes one back onto it: `roundtrip (logql)` (or
`roundtrip (traceql)`), a release-gate matrix leg today, is the standing
example of why a matrix-derived name needs the same `merge_group:` posting
discipline as a fixed one the day it is required again. (`roundtrip (promql)`
moved off the matrix in #2629 — it is now a fixed aggregator name rolling up
the `roundtrip-promql-shard` matrix, the same shape `perf-guards` already
used, so it no longer serves as this example.) A required context with no run
on the projected commit
never resolves, so the queue stalls with the PR neither merged nor rejected:
the failure mode is silence, not a red X.

**No merge-group run is ever cancelled.** GitHub reads a cancelled check-run as a
failed one and dequeues the entry, so every `cancel-in-progress:` reachable from
a `merge_group` run is `false` there. The recognised forms are literal `false`,
the pull-request-only expression, and the exact main-push/schedule expression;
all hold, and
`test/regression/merge_queue_test.go` rejects any third form that is not
provably false on the queue.

**A queued entry is held to the pull-request posture, not the push posture.** The
queue answers the contexts branch protection asks a *PR* for, so the lanes that
short-circuit on an ordinary PR (`coverage`, `property`) short-circuit in the
queue too. The diff-scoped mutation selector reads the merge group's own
`base_sha..head_sha`, and `quickstart` selects against the same base and the
projected `${{ github.sha }}` it actually checks out and boots. Those ranges are
the union of the batched pull requests' diffs. Running *more* in the queue than
on the pull request would let a queue
dequeue a PR that was green, which is the livelock a queue exists to remove; the
heavier sweep is not lost either way, because `main` advances to the merge
group's own head SHA and the push trigger runs it there.

The three release lanes (`compose-smoke`, `dashboard`, `profile`) have no
`merge_group` half by design. They gate the *publish*, and the publish preflight
inspects the commit `main` advances to — which is the merge group's head SHA
after a fast-forward — so their check-runs come from the push that lands the
batch, on the identical SHA a queue run would have used.

`CodeQL` runs through `.github/workflows/codeql.yml` (GitHub Advanced Setup)
rather than server-side default setup, so — unlike default setup, which has no
setting that adds `merge_group` — it declares `merge_group` alongside `push`,
`pull_request` and a weekly `schedule`, and posts a check-run on the queue's
projected trunk like every other required context.
`test/regression/merge_queue_test.go` pins the constraint from both sides:
`queueExternalContexts` holds only contexts with no workflow owner in this
repo, so `CodeQL` is asserted to be ABSENT from it, and
`TestEveryRequiredContextPostsOnTheMergeGroup` asserts the workflow itself
declares `merge_group`.

`strict-scan` is a branch-protection gate, so every real-ClickHouse differential
it hosts — strict-scan coercion (Layer 6e), the connection-teardown pool census
(Layer 6f), the spans-scan resource bounds — blocks a merge when it goes red. It
is the only required lane that observes the strict production scan; nothing else
in the required set can catch the emit-type class, because chDB coerces the very
divergence the class is made of.

The `compatibility/promql-surface` lane rides the same `compatibility.yml`
workflow as its four `compatibility/<head>` release-gate siblings and runs on
every PR (short-circuiting to a no-op there like they do), but is not named in
release.yml's `RELEASE_REQUIRED_CHECKS` — it stays informational until it has
held a green soak streak, the same flip discipline `strict-scan` and (in its
narrower window as a branch-protection gate, before #2230 moved the whole
`compatibility/*` category to the release gate for cost reasons) the four
required heads went through. Anything that lane proves must not regress
silently also needs a pin on the required `check` job, since neither gate it
could join runs on every commit the way `check` does.
`perf-guards` went through that same flip once: its cardinality and scale-wall
ratchets went from reporting green without gating to actually blocking a
merge, closing a real gap where the rows it guards had drifted out of step
with the corpus while the lane looked healthy. #2230 later moved it, like the
rest of the chDB/roundtrip and compatibility categories, off the merge gate
and onto the release gate — a cost decision, not a reversion of that
correctness fix: the ratchets still block every publish, just not every PR.
`test/regression/perf_guards_gate_test.go` holds the job name, the recipe and
the release-set membership that guarantee still depends on.

The connection-teardown contract carries exactly that pairing
regardless of gate status: `internal/chclient/conn_teardown_integration_test.go`
proves the driver behaviour against a real server, and the Layer 8 / Layer 10
pins in `test/regression/` hold the ordering, the bound and the startup
sequencing on the required lane. The chdb-free half of the
function-surface ledger (`test/surface-parity/`, `test/rejection-parity/`,
`test/oracle/inventory/` regenerability ratchets) DOES gate on every PR through the
required `check` job, so a wrong-reject / wrong-accept regression fails red
regardless of the live-reference lane's gate status.

## Per-layer guidance

### Layer 1 — Parser smoke / AST-shape pinning

Tests live in `internal/{promql,logql,traceql}/parser_*_test.go`. Each
exercises the upstream parser on a corpus of queries representative of
cerberus's lowering surface and asserts:

- The parse succeeds for valid input and the AST root-node type is the
  expected one.
- Invalid input fails with the documented error class.
- Field accessors used by `lower.go` produce the expected shapes (e.g.
  `LabelMatchers.Type`, `RangeAggregation.Operation`).

Add a case by appending to the table-driven test in the relevant file.
The corpus deliberately overlaps with `test/spec/` fixtures so a
parser-shape change surfaces here AND in the golden TXTAR diff.

### Layer 2a — chplan IR snapshots in TXTAR

`test/spec/<head>/<name>.txtar` carries an `-- input --` section (the
query), a `-- sql --` section (the emitted SQL), and a `-- chplan --`
section (the deterministic IR pretty-printer output from
`test/spec/chplan_print.go`). Lowering changes that alter the IR but
not the SQL surface here.

Use the `/cerberus:add-fixture` skill to scaffold; run `just
update-golden <shard>...` after the lowering lands to fill in the
expected text. The shard argument is required and has no default —
`just update-golden solver promql cardinality` for a PromQL fixture,
`logql cardinality` or `traceql cardinality` for the other two heads.
Name too few and the recipe refuses to start, naming the ones it
computed from the branch's own diff.

### Layer 2b — Lowering edge cases

Table-driven tests in `internal/<head>/lower_*_test.go` cover edge
inputs that don't appear in the golden corpus: NaN scalars, empty
matrices, type coercions, off-by-one anchor counts.

### Layer 3 — chplan IR invariants

`internal/chplan/equal_invariants_test.go` and `walk_invariants_test.go`
pin the generic IR contract: `Equal()` is symmetric and reflexive,
`Walk` and `Children` iterate in the documented order, pointer identity
is not load-bearing.

### Layer 4 — Optimizer rule properties

`internal/optimizer/rule_interaction_test.go` runs every rule pair on a
small corpus and asserts commutativity (or documented
non-commutativity). `termination_test.go` runs the fixpoint loop with
a bounded iteration cap to catch non-terminating rewrites.
`decision_pins_test.go` pins specific input → rewrite outcomes that the
optimizer is committed to.

### Layer 5 — chsql Frag + QueryBuilder goldens

`internal/chsql/frag_goldens_test.go` pins every Frag type's render
output. `query_builder_invariants_test.go` confirms slot-ordering
semantics (`Select` then `From` then `Where` etc.) and Build
idempotency.

### Layer 6 — chDB roundtrip

Fixtures with both `-- seed --` and `-- expected_rows --` sections run
twice against chDB. `test/spec/<head>` executes the pre-optimizer SQL,
while `internal/<head>` executes the post-optimizer SQL. Both compare the
result set to `expected_rows`; the post-optimizer walk then runs live reference
parity. The required roundtrip lane uses `chdb` for PromQL, whose reference
evaluator is available under that tag, and `chdb,agpl_oracle,chdb_agpl_oracle`
for LogQL and TraceQL. For those AGPL-backed heads, the three-tag set keeps each
in-house parser and its independent reference evaluator in the same binary; the
parity seam fails loudly if that evaluator compiles out, so a self-comparison
cannot report green.

PromQL's leg outgrew a single `roundtrip` matrix entry (700+ TXTAR fixtures) and
runs as its own `roundtrip-promql-shard` matrix instead — one dedicated runner
per shard, each still fanning out ~3 in-runner processes — with a
`roundtrip-promql` aggregator posting the `roundtrip (promql)` status check
(#2629). LogQL and TraceQL stay on the shared `roundtrip` matrix.

Each head's `just update-golden` shard regenerates this layer too — it runs a
second, chdb-tagged pass over that head's corpus so `expected_rows` cells can
never go stale behind a `-- sql --` change (it requires libchdb.so; see
`just chdb-install`). `just spec-chdb` verifies the pre-optimizer expected-row
pass locally without rewriting; the workflow runner additionally owns the
post-optimizer and live-reference comparisons.

Live-reference enrollment is baselined independently for every language.
`test/regression/parity_enrolment_test.go` derives the exact sorted identities
of TXTAR fixtures carrying a `-- parity --` section under each head and compares
them with that head's committed `baseline.txt` in
`test/regression/parity-enrolment-baselines/`. The baseline files are the source
of truth; copied counts and event-named checkpoints are not part of the
contract. Removing one fixture and adding another in the same head therefore
fails even though the total is unchanged. A deliberate roster change is made
with `just update-golden parity` and reviewed as an exact identity diff. The
same shard also regenerates the parser-surface and rejection-parity ledgers,
and a fixture change cannot pass its shard-coverage check without naming it.
The generated files refuse line merging so unrelated additions cannot blend
with a stale removal.

Both sections are load-bearing: a fixture with a `-- seed --` but no
`-- expected_rows --` is **inert** — the runner returns before it touches
chDB, and the `GOLDEN_UPDATE=1` rewrite sits downstream of that return, so
regeneration cannot create the missing section either. Such a fixture
contributes a `-- sql --` shape golden while looking like round-trip
coverage. `test/regression/inert_seeded_fixture_test.go` (untagged, so it
runs in the required `check` gate) enumerates the corpus and fails on that
shape. A fixture whose query genuinely needs a ClickHouse feature above
the pinned chDB floor declares it in the fixture itself:

```text
-- above_chdb_floor --
feature: <ClickHouse function + minimum version>
validated: <the lane that proves it instead>
```

Both keys are required and the gate rejects a marker on a fixture that
does round-trip, so the marker cannot decay into a blanket exemption. No
fixture currently needs it — the pinned chDB substrate clears every floor
the corpus exercises.

#### The chdb lane and the race detector

Every chdb-tagged test binary loads libchdb.so — an embedded ClickHouse —
with `dlopen`, and chdb-go caches ONE process-wide session for the life of
the process. Its driver's `(*conn).Close` is a no-op, so `db.Close()` does
not shut the native engine down; nothing on the Go side ever does.

That asymmetry is invisible to a plain `go test`, because Go's `os.Exit`
reaches the `exit_group` syscall directly and libchdb's C++ static
destructors never run. It is NOT invisible under `-race`: `os.Exit` first
calls `runtime_beforeExit` → `runtime.racefini` → `__tsan_fini`, which — per
the Go runtime's own comment on `racefini` — "will run C atexit functions
and C++ destructors". Those destructors then tore libchdb down while its
engine was still live, and the process died with

```text
SIGSEGV: segmentation violation
runtime.racefini()
os.runtime_beforeExit(0x0)
os.Exit(0x0)
```

at a constant offset inside `libchdb.so`, **after** every test in the binary
had already passed — turning a suite in which nothing failed into a failed
lane. `os.Exit(0x0)` in that trace is the tell: the exit code was zero.

`internal/chdbsession.CloseForExit` closes the cached session before the
binary exits, so those destructors run against an already-shut-down engine.
It is per-package by necessity — `TestMain` is the only process-exit seam Go
offers and it is declared per package — so
`test/regression/chdb_race_exit_test.go` is the ratchet: it walks the tree
for chdb-tagged test files and fails any package whose `TestMain` does not
call it. Without that gate a newly added chdb-tagged package would silently
reintroduce the crash.

The `chdb` CI job still runs without `-race`, for cost rather than
correctness. Measured over the four `internal/api` packages, `-race` costs
between 1.5x (`prom`, 131s → 201s — it is already the long pole) and 5x
(`tempo/grpc`, 0.4s → 2.1s) in wall clock. The difference now is that a
developer or agent CAN run
`go test -race -tags chdb,agpl_oracle,chdb_agpl_oracle ./...` to chase a
suspected data race across the complete chDB-tagged surface, which before was
structurally impossible.

### Layer 6d — Function-surface parity ledger

Layers 6a-c prove that an *accepted* query returns the right rows. Layer
6d proves cerberus accepts and rejects the right *set* of grammar symbols
in the first place — the conformance frontier between the three upstream
parsers' grammars and what cerberus's `parse → fold → lower → optimize →
emit` pipeline actually admits. Three sibling packages, all chDB-free, all
gating on every PR through `check`:

- **`test/surface-parity/`** — the authoritative ledger. It enumerates
  every symbol the three upstream parser symbol tables expose (PromQL
  `parser.Functions` + aggregators + binary ops + modifiers, LogQL `Op*`
  consts, TraceQL intrinsics + metrics-ops), runs the cerberus verdict
  (accept / reject) and the reference-backend verdict on each, and
  classifies every pair into a four-way grid: `parity-accept` (both
  accept), `parity-reject` (both reject), `wrong-reject` (cerberus 422s a
  symbol the reference accepts — a real coverage gap), `wrong-accept`
  (cerberus answers a query the reference won't). `inventory.json` is the
  pinned artifact; `inventory_test.go` runs a three-leg ratchet
  (regenerability + a raise-only floor on the wrong-reject and wrong-accept
  sets), so a NEW wrong-reject — a freshly-added grammar symbol cerberus
  doesn't lower, or one that regressed from accept — fails CI red, and a
  burndown that *closes* a gap also fails until the inventory is
  regenerated (`CERBERUS_UPDATE_INVENTORY=1`), keeping the ledger in
  lock-step with the surface.

  The PromQL reference oracle is the **flag-ON** reference Prometheus HTTP
  verdict (started with `--enable-feature=promql-experimental-functions`,
  matching cerberus's own parser config), pinned one shard per symbol under
  `promql-reference-verdicts/` (metadata in the `promql-reference-verdicts.json`
  manifest alongside it). The in-process ratchet reads the pinned artifact
  (Docker-free); the `compatibility/promql-surface` CI job re-probes the
  live reference and fails on drift, so the artifact can never silently
  diverge from the real backend.

- **`test/rejection-parity/`** — the SITE-based complement. Where
  surface-parity starts from the parser's accepted grammar, rejection-parity
  diffs cerberus's KNOWN 422 code-sites against the reference's error class
  for the same probe, so a rejection whose *message / status* drifts from
  the reference is caught even when the accept/reject verdict agrees.
  The `catalogue/` shard directory (one JSON shard per lowering source file)
  - `catalogue_test.go` ratchet it the same way.

  Two harness conditions make that diff mean something, and both are pinned
  by `test/regression/compat_rejection_parity_reference_test.go`: the
  reference Prometheus runs **flag-ON**
  (`--enable-feature=promql-experimental-functions`), matching cerberus's own
  parser config, so "both 4xx" can never record agreement about a feature
  flag instead of about the guard under test; and the driver evaluates at
  `-eval-time` inside the seeded fixture window, so upstream guards that
  validate per series (`double_exponential_smoothing`'s smoothing / trend
  factors) actually run instead of short-circuiting on an empty selector.
  The same test pins every PromQL trigger query onto a family the compat
  seeder writes.

- **`test/oracle/inventory/`** — per-head capability inventories
  (`{promql,logql,traceql}_test.go`), regenerable under the same
  `CERBERUS_UPDATE_INVENTORY=1` convention.

The user-facing translation of this ledger — every function / operator /
intrinsic with its support status — lives in
[`coverage.md`](coverage.md), which is generated from `inventory.json` and
presents the ledger classes in honest support language (an experimental fn
cerberus implements that a reference *without* the experimental-functions
flag would reject is "Supported (experimental)", not a raw "wrong-accept").

### Layer 7 — HTTP handler conformance

`internal/api/{prom,loki,tempo}/conformance_test.go` exercises every
documented HTTP endpoint with representative payloads and asserts the
wire envelope shape (`status`, `data.resultType`, content keys),
response headers, error envelope, and admission-control 503 + `Retry-After`.

### Layer 7b — Consumer-corpus replay

`test/consumer-corpus/` is the shift-left lane for consumer-contract
bugs: a corpus of REAL request shapes Grafana sends
(`grafana-<version>/*.json`, one entry per file with provenance,
the Grafana-side request, and per-entry expectations), replayed
against the in-process handlers and decoded EXACTLY as the consumer
decodes — strict gogo/proto unmarshal into `tempopb` types for the
Tempo proto endpoints, bare logproto-shaped JSON for Loki, Prom API
envelopes for Prometheus. The 2026-06 incident week (bare `Trace` vs
`TraceByIDResponse` #764, enveloped `detected_fields` #774, missing
`spanSets` #770, drilldown `<groupBy> != nil` 422s, blank regex
`__name__` breakdowns #769) was only caught by the e2e browser stack;
every one of those is reproducible here at unit cost.

Two lanes share the corpus: the default-tag lane (runs in `check`)
backs handlers with canned-row stubs and pins routing, status, and
consumer decodability; the `chdb`-tagged lane (runs in the chdb
workflow's `probe` job via `just test-chdb`) executes the full
parse → lower → optimize → emit → chDB pipeline over small seeds and
additionally evaluates each entry's data predicates. A ratchet
meta-test forbids corpus shrink (total + per-datasource entry floors
are raise-only) and rejects entries naming decoders / predicates /
stub fixtures the harness doesn't implement.

Division of labour with Layers 7 and 9: Layer 7 pins cerberus's OWN
wire contract endpoint by endpoint; Layer 7b pins what GRAFANA
actually sends and reads, request by captured request. The e2e
crawler and drilldown specs (Layer 9) are the corpus MINERS — when a
Grafana bump introduces a new request shape, capture it into a new
version-keyed corpus directory rather than widening assertions in
place. Known-unfixed bugs stay as failing entries (never tolerated,
never allow-listed): a red corpus entry is the layer doing its job.

### Layer 8 — System / process lifecycle

Covers env-var parsing (`internal/config/`), `/healthz` + `/readyz`
TTL coalescing, OTel resource attribute composition, schema DDL
idempotency, and signal-driven shutdown.

### Layer 9 — Playwright UX flows

`test/e2e/playwright/*.spec.ts` boots a k3d cluster (cerberus +
ClickHouse + Grafana + telemetrygen), provisions the cerberus
datasources, and walks Explore / Dashboard / Logs / Trace panel
flows.

### Layer 10 — Chaos / failure-mode (deterministic)

`internal/{chclient,api/...}/chaos_test.go` injects CH connection
drops, mid-stream cursor faults, panic-mid-handler, and the
`goleak`-pinned no-goroutine-leak invariant via a **stubbed querier** —
deterministic, in-process, on the required `check` lane. The
CH-disconnect circuit breaker shields cerberus from amplifying transient
CH outages into a 503 storm. This layer proves the *logic* of each
resilience contract; Layer 13 proves the same contracts hold against a
*real deployment* under *real faults*.

### Layer 11 — Perf + alloc regressions

`BenchmarkXxx` measures per-stage allocation counts via
`testing.AllocsPerRun`. `TestAllocs_Xxx` pins documented zero-alloc
hot paths (e.g. emitter slot append, Frag render). benchstat-based
wall-clock comparisons live in `perf-benchmark.yml` (manual dispatch).

### Layer 12 — Compute fan-out guards

The axis Layer 11's read-side benchmarks are blind to: peak intermediate
cardinality and wall-time scaling against a query parameter (step count,
chain depth, recursion depth). The thing cerberus watches is the **fan
factor** — peak intermediate rows ÷ leaf scan rows — and five lanes hold
it flat from cheap-static to broad-corpus (see
[`performance.md`](performance.md#how-fast-is-kept-fast--the-assurance-framework)
for the strategy):

- **Static fan-out lint** (`internal/perf/fanout`, in `check`) — flags
  structurally-unbounded shapes (an unbounded `CrossJoin`, an `arrayJoin`
  feeding a `JOIN`, an uncapped `WITH RECURSIVE`, a correlated subquery) on
  the lowered plan *and* emitted SQL of every corpus fixture. No chDB
  needed.
- **Per-construct scaling harness** (`test/perf/scaling`, chdb `perf-guards`
  job) — sweeps a parameter for a known-hot construct and asserts wall-time
  stays sub-linear *and* peak intermediate cardinality stays bounded.
- **Cardinality + scale-wall ratchets** (`test/perf/cardinality_ratchet_test.go`,
  `scale_wall_pin_chdb_test.go`, chdb `perf-guards` job) — pin every
  fixture's fan factor + structural flags + recursion depth in
  `cardinality-baseline/<head>/<name>.json` / `scale-wall-baseline.json` and
  fail on an upward regression, a new unbounded shape, or a deeper recursion. A
  decrease never blocks; the ceiling tightens only on a deliberate
  `just update-cardinality-baseline` — which is `just update-golden`'s
  `cardinality` shard, demanded by the recipe's coverage check the moment a
  TXTAR fixture moves, so adding one records its ratchet shard in the same
  pass that fills the goldens (closing the recurring "unrecorded fixture →
  red `perf-guards` on main" miss). `scan_rows` and
  `has_array_join` are compared for exact equality rather than ratcheted:
  neither has a "better" direction, so a change is the shard ceasing to
  describe its fixture, and a stored field nothing compares is a hole rather
  than soft coverage.
- **Solver-decision ratchet** (`test/perf/solver_decision_ratchet_test.go`,
  chDB-free, in `check`) — pins the per-fixture route A/B classification
  against `solver-decision-baseline/<query>.json` so a routing-heuristic
  change surfaces in the diff.
- **Corpus-wide fan-out profiler** (`test/perf/profile`, the `perf-profile`
  workflow, nightly + push, informational) — profiles every executable
  TXTAR fixture via in-process chDB `EXPLAIN` + per-subquery `count()`,
  ranks by fan factor, and surfaces the worst as a job step-summary. The
  wide net for a fan-out in a construct nobody wrote a guard for.

### Layer 13 — Live-stack chaos (real faults)

The robustness layer *above* Layer 10's deterministic unit chaos: it
fault-injects against the running k3d e2e stack (cerberus + ClickHouse +
Grafana + OTel collector that `just e2e-up` stood up) and asserts the
landed resilience contracts hold under **real** faults, not
stubbed-querier injection. Lives as the `chaos` job in
`.github/workflows/e2e.yml`, driven by `.github/scripts/chaos-run.mjs`
(node ESM, kubectl + `fetch`) with the overlay/NetworkPolicy manifests
under `test/e2e/chaos/manifests/`. Locally: `just e2e-up &&
just e2e-seed-rolling && just e2e-wait-otel && just e2e-chaos-overlay &&
just e2e-chaos`.

What it proves (the contracts, mapped to their landing PRs):

- **Circuit breaker (#883).** `ch-pod-kill` deletes the single-replica
  CH Deployment (Recreate → clean outage). Under fault, a tight query
  loop forces the shared breaker OPEN: every head returns 503 +
  `Retry-After` (the breaker's own `CERBERUS_CH_BREAKER_OPEN_INTERVAL`, `5`
  at the shipped defaults) `errorType=unavailable` (accepting the documented
  502-then-503 ordering), `/readyz` goes 503 with `circuit` in the body,
  and `/healthz` stays 200 (liveness is breaker-independent — a CH
  outage must NEVER restart cerberus). After CH auto-recreates, the
  HALF-OPEN probe closes the breaker, `/readyz` and all heads return to
  200, `cerberus_ch_breaker_trips_total >= 1` (monotonic) and
  `cerberus_ch_breaker_state == 0`.
- **Per-query wall-clock timeout (#886).** `ch-slow-query-timeout`
  issues a deliberately slow `query_range` (wide range, 1 s step →
  millions of anchors) that blows past the small `CERBERUS_QUERY_TIMEOUT`
  the overlay set → clean 503 `errorType=timeout`. Critically
  **breaker-neutral**: CH code-159 `TIMEOUT_EXCEEDED` is coerced to
  success in `breaker.record`, so a burst of slow queries does NOT trip
  the breaker (`state == 0`, `trips_total` unchanged), `/readyz` stays
  200, and a separate fast query still 200s (admit slot + pooled
  connection released).
- **Replica resilience.** `cerberus-pod-kill` deletes ONE of the ≥2
  HPA-floor replicas (scoped by name, never both); the Service keeps
  serving from the survivor (aggregate success ≥ 95 %, retrying a single
  mid-drain connection reset). The replacement rejoins endpoints; the
  surviving replica set shows no unexpected restart (the killed pod is
  *replaced*, not restarted in place — so the lane deliberately does NOT
  inherit the dashboard job's blanket `restartCount == 0` assert).
- **Network partition (phase-2).** `ch-network-partition` applies a
  deny-egress NetworkPolicy (kube-router) to blackhole cerberus → CH —
  the slower path to the same breaker-OPEN end state. **Gated** on a
  runtime enforcement probe: if kube-router is not enforcing
  NetworkPolicy in the pinned k3d image, the scenario is recorded
  not-applicable (`::notice::`) and the breaker contract is covered by
  `ch-pod-kill` instead — never a vacuous pass.
- **Admission control + pool (phase-2).** `load-admit-saturation`
  bursts concurrency beyond the small overlay admit cap → some requests
  shed cleanly with 503 + `Retry-After: 1` `server saturated` while a
  below-cap request still 200s; `cerberus_admit_rejected_total` climbs;
  the breaker stays CLOSED (admit + pool-acquire rejections are
  breaker-neutral).
- **Handler panic (#885).** Already pinned deterministically by Layer 10
  — the live lane only corroborates that the process recovered cleanly
  from the cumulative fault storm (all 3 heads 200, no lingering 5xx) as
  a passive end-of-run health gate, not a dedicated scenario.
- **Failure-driven route memo (e01ed68d).** `route-memo-activation` fires
  the pinned 24h/15s aggregating `query_range` shape (the same
  `CERBERUS_CH_QUERY_MAX_MEMORY`-crossing tuple `iterate-time-ranges.spec.ts`
  already pins) with `CERBERUS_SOLVER_ADAPTIVE_ENABLED=true` (chaos
  overlay). A route-A dispatch that trips ClickHouse's
  `MEMORY_LIMIT_EXCEEDED` (code 241, breaker-neutral) is retried once on
  route B; a successful retry both returns the client a 200 (instead of
  the pinned 422) and moves
  `cerberus_route_ab_success_total{cerberus_route_choice="b"}`. Closes the
  gap the mechanism had never been run against a real cerberus process
  fielding real ClickHouse traffic — no e2e/chaos/compose manifest had
  ever set the env var before this scenario. Data-volume-dependent like
  the dashboard sweep's own dual contract: if the tuple never crosses the
  cap this run, the scenario records not-applicable rather than a
  vacuous pass.

Both not-applicable exits are individually legitimate — the precondition
genuinely didn't materialise this run — but nothing about a single green run
distinguishes that from the precondition having permanently stopped
materialising, which would let the lane report success forever while
exercising neither contract. `.github/scripts/chaos-not-applicable-rate.mjs`
(weekly `chaos-not-applicable-rate.yml`, its own scheduled lane, never a PR
check — same reasoning as `release-gate-drift.yml`) mines recent `chaos` job
logs and fails if a scenario has recorded not-applicable in every sampled run;
see that script's own header for the full design.

Design notes (flake resistance): every recovery check polls to a
**generous bounded deadline** (never asserts immediately after a fault);
faults are one-shot + idempotent (`kubectl delete pod` / `apply`
policy), retries live on the ASSERT side only; scenarios run
**sequentially with heal-between-each** so one scenario's residue can't
poison the next; metric-based asserts (read back through cerberus's own
Prom head — cerberus has no `/metrics` endpoint) are POST-recovery
corroboration with a settle poll, because OTLP → collector → CH flush
lags the fault by seconds, so during-fault timing keys on immediate HTTP
status + `/readyz` body + kubectl state. The lane is **informational**:
push-to-main + nightly + manual only, never a PR gate, never a
branch-protection required check (k3d is heavy and chaos flakes).

### Layer 14 — Migration-scenario e2e

The only layer whose unit of work is an **operator journey**, not a code
path. Layers 1–13 ask "does this query lower, emit, execute and render
correctly?"; Layer 14 asks "can a team actually move off Prometheus /
Loki / Tempo onto cerberus without losing data, alerts or sleep?" It
encodes the 26 migration user-stories (ASSESS → TEST → VALIDATE → VERIFY
→ CUTOVER → DECOMMISSION) as Gherkin scenarios and runs them across
three infrastructure tiers. `docs/migration-testing.md` is the canonical
reference — story map, tier definitions, honesty contract and phased
build order all live there; this section is the index entry.

Lives in `test/e2e/migration/`: `features/` (the Gherkin corpus),
`steps/` + `lib/` (the godog bindings), `seed/` (the deterministic
all-signal fixture), `archetypes/` (the eight seeded operator
topologies), `tiers/` (per-tier compose stacks) and the tagged Go
entrypoints `tier1_stack_test.go`, `tier1_parity_test.go`,
`tier2_ruler_test.go`. Driven by `.github/workflows/migration-e2e.yml`.

The tiers escalate substrate, not scope:

- **Tier 0 — offline.** No containers. Exercises the `cerberus migrate`
  CLI surface (harvest / explain / classify / rulegraph / gate) against
  recorded inputs. Cheap enough to run on every push.
- **Tier 1 — parity.** Pinned reference Prometheus / Loki / Tempo beside
  ClickHouse + an OTel collector + cerberus, both sides fed from one
  deterministic all-signal fixture, then read back over a
  seeder-published closed window. `cerberus migrate verify` must report
  a zero diverge-count across metric matrices, log entries and trace
  identities. A **negative control** deliberately perturbs one side and
  asserts the harness reports the disagreement — without it a silently
  broken comparator would read as perfect parity.
- **Tier 2 — firing parity.** Tier 1 plus a query-only external ruler
  (Grafana-managed alerting pointed at cerberus), a recording-rule
  write-back bridge and a dead-end notification receiver, so alert
  *firing* — not just query results — is compared. `needs:
  migration-tier1`, because firing parity is meaningless before query
  parity holds.

**Catches:** the failure modes that only appear when a real operator
workflow crosses the whole stack — ingest-bridge type reconstruction
(what the collector writes vs. what the reference backend stored),
schema / label / histogram fidelity across the write seam, rule-graph
classification errors that would silently drop an alert at cutover,
`migrate verify` comparator bugs (via the negative control), and
alert-firing divergence that query-level parity can't see.

**Misses:** everything Layers 1–13 own. Layer 14 is a *workflow* lane,
not a code lane — a lowering or emitter bug only surfaces here if the
archetype corpus happens to name that shape, and it surfaces as an
opaque parity diff rather than a located defect, so fix it at the layer
that pinpoints it. Customer topologies beyond the eight seeded
archetypes are out of scope by construction, as are the declared scope
limits recorded in `docs/migration-testing.md` §6.4.

The lane is **informational**: push-to-main + nightly + manual dispatch
only, with no `pull_request:` trigger and no branch-protection entry.
The compose lifecycles are far too heavy for a PR gate, and the honesty
guardrails in `docs/migration-testing.md` §6.3 — the pinned PASS-assertion
set in `pass-assertions.pin.json` and the coverage baseline in
`coverage-baseline.json` — are what keep an informational lane from
quietly decaying into a green no-op.

## Property tests

`test/property/` runs rapid-driven property tests under the composite
`chdb,agpl_oracle,chdb_agpl_oracle` tag set. `just property` is the canonical
local entry point and includes all three query heads. The architecture is:

```text
test/property/
  framework.go        — rapid.Check driver, deterministic examples,
                         fail-closed validators, and result comparator
  gen/
    shapes.go         — exact stable ShapeID rosters for every family
    *.go              — random data + shape-aware query generators
  oracle/             — from-scratch evaluators (PromQL / LogQL / TraceQL)
                         that do NOT import internal/{promql,logql,traceql}
                         so the oracle is not the SUT
  promql_test.go      — instant PromQL random sweep + exact roster pass
  promql_range_test.go
                      — range PromQL random sweep + exact roster pass
  promql_exp_histogram_test.go
                      — native-histogram random sweep + exact roster pass
  instant_window_test.go
                      — exact instant-window value oracle, random sweep,
                         edge controls, and exact roster pass
  logql_test.go       — LogQL random sweep + exact roster pass
  traceql_test.go     — TraceQL random sweep + exact roster pass
```

Every family runs two complementary modes. The rapid sweep:

1. Draws a dataset (rapid).
2. Draws a stable semantic shape and a query against that dataset.
3. Lowers the query to SQL, runs it under chDB.
4. Runs the from-scratch oracle on the same dataset.
5. Compares result sets.

Failures shrink to a minimal repro via rapid's automatic shrinking and
land in the standard test output. This sweep widens literals, data values,
window geometry, and label combinations; it is not used as probabilistic proof
that every finite query shape happened to run.

The deterministic roster pass executes exactly one live differential for every
published `ShapeID`: a stable bounded seed sequence derived from the ID builds
candidate datasets and queries, the independent oracle selects the first
candidate with non-empty row evidence, and the real HTTP/chDB pipeline evaluates
only that selected input. The sequence is independent of roster position, so
inserting or reordering a shape cannot silently change another shape's evidence.
Empty or duplicate IDs, a generator that cannot render one of its published
IDs, oracle errors, and bounded searches with no row evidence fail the
exact-roster tests.

`test/regression/property_live_roster_floor_test.go` independently discovers
the six deterministic roster runners and the six randomized rapid sweeps from
their Go call sites. It requires exactly one of each family and verifies that
the workflow executes the composite tag set with exactly 500 rapid checks.
Registry prose, a compiled-but-unexecuted test, or a deterministic example left
behind after deleting its randomized sweep cannot satisfy that floor.

Both modes fail closed at the harness boundaries. A metrics dataset must have
non-empty series, metric names, and seed DDL; a logs dataset must have non-empty
records and seed DDL; a deterministic example may carry exactly one model
family. Every query must carry a non-empty stable shape ID and non-empty query
text. An oracle error is a harness failure and a system error is a
product/substrate failure: even when both sides error, the result is red rather
than a false agreement. Rows are compared only after both sides return
successfully.

### Exact semantic-shape roster

`test/property/gen/shapes.go` is the executable source of truth. Its 81 stable
IDs are grouped as follows (brace notation below denotes the exact listed
expansion, not an open-ended prefix):

- **PromQL instant — 5.** `promql.instant.{selector,sum,sum-by,rate,sum-rate}`.
- **PromQL range — 3.** `promql.range.{selector,sum-by,rate}`.
- **PromQL native histogram — 20.**
  `promql.native-histogram.{function.count,function.sum,function.avg,`
  `function.stddev,function.stdvar,fraction,selector,sum,sum-by,rate,increase,`
  `quantile-selector,quantile-sum,quantile-sum-by,quantile-rate,`
  `quantile-increase,quantile-sum-rate,quantile-sum-by-rate,`
  `quantile-sum-increase,quantile-sum-by-increase}`.
- **LogQL — 12.**
  `logql.stream.{selector,line-contains,line-excludes,label-format,ip-contains,`
  `ip-excludes,pattern-contains,pattern-excludes,pattern-contains-prefix,`
  `pattern-contains-suffix,pattern-excludes-prefix,pattern-excludes-suffix}`.
  The unqualified pattern pair uses a floating `<_>token<_>` match; the
  prefix/suffix pairs pin the anchored `token<_>` and `<_>token` semantics.
- **TraceQL — 17.**
  `traceql.selector.{service,resource-attribute,span-attribute,regex,`
  `negated-attribute,conjunction}`;
  `traceql.intrinsic.{duration,status,name}`;
  `traceql.structural.{child,descendant}`; and
  `traceql.pipeline.{count,duration-aggregate,duration-aggregate-min,`
  `duration-aggregate-max,duration-aggregate-sum,select}`. The unsuffixed
  `duration-aggregate` ID is the average-duration shape.
- **PromQL instant window — 24.** The exact Cartesian product
  `promql.instant-window.{wave,positive-increments,monotonic-running-total}.`
  `{sum-over-time,count-over-time,avg-over-time,max-over-time,min-over-time,`
  `rate,increase,delta}`. These prefixes describe the values stored in the
  gauge-table fixture; they are not OTel aggregation-temporality claims.

When a generator widens, update its stable roster and this manifest in the same
change. The independent oracle under `test/property/oracle/<head>/` (or the
inline instant-window oracle for that family) must be able to evaluate every
new shape before the generator publishes it, never the reverse.

## Gremlins mutation

`.gremlins.yaml` carries the floor for an unscoped whole-repo `just
mutate`; the per-phase table — scope, efficacy floor, worker cap,
exclude set, and the rationale behind each — lives in
`.github/scripts/mutation-phases.mjs`, and `.github/workflows/mutation.yml`
expands it into the `mutate` job's matrix. The rolled-up `mutation` context is
informational — not required by either the merge gate or the release gate
(#2230) — but keeps a real, non-blanket per-PR selection (below) rather than a
blanket no-op, for early author-time signal.

On a pull request the lane runs the phases whose scope that PR changed,
and only those. A production-only edit uses gremlins' native merge-base
changed-line filter inside its selected phase. A test, fixture, harness,
delete-only, binary, malformed, or uncomputable projection runs the selected
phase in full; an incremental report with zero executable mutants also reruns
the full phase before it may report. A PR editing only docs runs no leg and the
aggregator passes through honestly. Push-to-main, the nightly, a manual dispatch, a `release/*`
PR, and any PR touching mutation-specific harness material all sweep the FULL
matrix, so no phase's floor is ever load-bearing on some PR happening to touch
its package. Registry edits are projected to the mutation lane's semantic
execution and ownership fields across the base and candidate revisions: an
unrelated metadata edit does not buy the full matrix, while a relevant change
or an unreadable projection fails to the full sweep. Local-only Just recipes do
not select CI mutation legs because the workflow does not consume them.
`.github/scripts/mutation-matrix.mjs` computes that selection from the PR's own
diff against its merge base and is unit-tested in the `forbid-skip` job; when
the diff itself cannot be computed it also falls back to the full matrix rather
than to an empty one.

Mutation ownership is checked bidirectionally before phase selection.
The `quality.mutation` lane's `package_globs` in `.github/ci-lanes.json`
declare an independent source universe; `.github/scripts/mutation-matrix.mjs`
enumerates the mutable non-test Go files in that universe and reconciles them
against the scopes and exclusions in `mutation-phases.mjs`. Every
registry-owned source file must have exactly one phase owner, and no phase may
claim source outside the registry surface, and every phase must own at least one
such mutable file. A third, independently declared
production-scope anchor in `mutation-phases.mjs` prevents a synchronized
registry-and-phase deletion from erasing its own evidence. Deleting a whole
phase, creating an exclusion gap, duplicating ownership, narrowing the registry
around an existing phase, or deleting the same scope from both mutable
declarations therefore fails. On a scoped change, a registry-owned changed path
with no phase owner also fails instead of selecting an empty matrix.

The selector also pins the phase table's efficacy threshold to an independent
95% minimum. Lowering the shared phase constant cannot turn every leg into a
green zero-evidence run.

The phase inventory is not restated here. `mutation-phases.mjs` is the
one place that carries it: leg names and per-leg efficacy floors change
with the packages they cover, and a table duplicated into prose drifts
out of step without anything going red. Read the module.

`internal/logql` is split into four sibling matrix entries (each scoped
to `./internal/logql` but with disjoint `--exclude-files` regexes) to
keep the `go test ./internal/logql` cycle under the ubuntu-latest memory
ceiling. Its `internal/logql/lsyntax` parser
subpackage gets its own pair of dedicated legs (`phase4-logql-parser` /
`phase4-logql-lsyntax`, mirroring the `internal/traceql/ast` split
below) rather than being swept into the four `internal/logql` legs —
gremlins recurses into every subdirectory of a scope, so leaving
`lsyntax` unexcluded there let a relocated, heavier test file bloat
every one of those legs' `go test` cycles (2026-07-25 incident). See
`.github/scripts/mutation-phases.mjs` for the per-phase exclude sets.
Those patterns are handed to Go's regexp, so `mutation-matrix.mjs`
rejects any that reach for lookaround or a backreference — RE2 has
neither, and a leg that only discovers this at run time has already
spent its checkout and toolchain setup.

A surviving mutant is either (a) a legitimately weak assertion that
needs strengthening, (b) a functionally-equivalent mutation (`<` vs
`<=` on a boundary that's never hit, slice-cap arithmetic that
`append` regrows past), or (c) a missing test. The gremlins JSON
artifact on each run names the file + line + mutation kind.

### Surviving-mutant policy

When a mutant survives the phase threshold, pick the remedy in this
order — the goal is to keep production code clear and let the test
suite carry the discipline:

1. **PREFERRED — prove equivalent.** Add a comment in the source
   explaining why the mutated branch is semantically identical to the
   original, then drop the phase efficacy threshold in `.gremlins.yaml`
   by 1 percentage point to absorb the equivalent mutant. The mutation
   count is now defensible and the source stays clear.
2. **ACCEPTABLE — add a distinguishing test.** Write a unit / property
   test whose output differs between the original and the mutated
   branch. This is the right call when the mutation reveals real
   under-tested behaviour.
3. **REJECTED — refactor production code to make the mutant
   distinguishable.** This is pattern #11 (DEFEAT-MUTANT) — the
   codebase loses clarity to satisfy the mutation tool. Don't do it.

Prior PRs #504 and #664 carry pattern-#3 refactors. They are not
reverted (their diffs are now load-bearing for the published
thresholds), but new violations should follow remedy #1 or #2.

## Grafana surface crawler (Layer 9 extension)

`test/e2e/playwright/crawl/` extends Layer 9 from *enumerated* UX
flows to *discovered* ones. Where the iterate-\* specs visit known
surfaces (dashboards, panels, the drilldown-app catalogue), the
crawler (`crawl/crawl.spec.ts`) BFS-walks every same-origin link
reachable from the Grafana root, canonicalizes URLs so the
visited-set converges (path + structural-param keys; dynamic
segments like service names, trace ids, and folder uids
parameterize; `/explore` collapses to one surface), and applies the
same universal oracles on every page — no per-page code:

1. **Zero browser console errors** (no cerberus-origin noise filter,
   ever).
2. **Zero non-2xx responses** on the datasource API families
   (`/api/ds/query`, `/api/dashboards/`,
   `/api/datasources/proxy/uid/`, `…/resources/`) and zero tunneled
   `.results.<refId>.error` bodies — the only sanctioned failures are
   those attributable to a panel's declared
   `cerberus.expect: "error:<substring>"` contract.
3. **Panel tri-state**: every rendered panel ends in
   has-data | declared-empty | declared-error; an undeclared
   "No data" fails with the panel title + URL.
4. **No page-level crash banner** and no visible `role="alert"`
   error banner.

**Interaction sweep** (`crawl/interactions.ts`). Visiting a surface
at its default control state is not enough: the 2026-06-10 maintainer
find — clicking the Traces Drilldown breakdown groupBy "kind"
attribute fired `{… && kind != nil} | rate() by(kind)` and cerberus
422'd on the nil-comparison — was a state no harvested link encodes.
After each surface's base audit the crawler therefore discovers its
view-affecting interactive controls and drives every planned
deviation, each against a **fresh navigation** of the surface
(deterministic provenance: one state = surface default + exactly one
control deviation). Control kinds discovered: tab strips
(`role=tablist`), radio groups (`role=radiogroup`, re-found at drive
time by option-name signature — the generated input ids are
mount-order dependent), select/combobox dropdowns (probed open to
learn their option sets; datasource pickers, sort-bys, level
filters), titled option lists (the Traces Drilldown attribute
picker), metric select tiles, and adhoc-filter builders (driven as
one representative key → value pair). Mutating affordances
(save/delete/create/add-tab), free-text search inputs, and the
time/refresh pickers (owned by iterate-time-ranges) are excluded —
the crawl stays read-only.

State identity follows the URL. A deviation the app encodes into the
URL becomes a **first-class surface**: the canonicalizer retains
*structural* params (`StructuralParamRule` in `crawl/lib.ts`) —
low-cardinality ones verbatim (`?actionView=comparison`,
`?var-groupBy=kind`) with the app's cold-boot default dropped, and
high-cardinality ones parameterized (`?metric={metric}`, the
`{service}` doctrine) — and the BFS visits the discovered state
fresh with the full oracle set. A deviation that does **not** encode
to the URL is audited in place with the same oracles and pins into
the inventory under the state notation
`<canonical>#<control>=<value>` (high-cardinality representatives
record `{rep}` so data-derived values can't flicker the inventory).

Bounding (the locked pairwise design, enforced in
`planInteractions`): structural controls (≤ 12 options) enumerate
**fully**; high-cardinality controls take **one representative**;
cross-control combos form **pairwise via surface chaining** — a
surface pinning one structural param sweeps with the representative
plan (one option per control, each interaction forming a param
pair), and surfaces pinning ≥ 2 params are terminal (visited, never
expanded). Every plan is hard-capped (24 single-sweep / 16 pairwise
per surface); overflow **fails the crawl listing the full plan** —
never a silent truncation. Depth doctrine unchanged: lean sweeps the
configured representative roots (the three drilldown app entries)
with one state per control; full sweeps every eligible surface
exhaustively.

Two sibling specs ride the same lane:

- `crawl/dsquery.spec.ts` — consumer-grade replays through
  `POST /api/ds/query`, the datasource plugin **backend**. The plugin
  decode layer (frames, RFC3339 shapes, enums) fails on wire drift
  the datasource-proxy probes pass through, so this is a distinct
  oracle, not duplication. (Tempo replays only `queryType: traceId`
  — the Tempo plugin backend rejects TraceQL search by design.)
- `crawl/lints.spec.ts` — deterministic data-quality lints. Each lint
  pins a named incident class: histogram degeneracy (all observations
  in one bucket fabricating constant quantiles) on every histogram
  family a quantile panel consumes, and identical-quantile-series
  (p50 ≡ p95 bitwise — the same single-bucket signature).

**One engine, N stack configs.** The crawler is a stack-agnostic
framework: the engine — BFS walk, URL canonicalization, the universal
oracles, the ds/query replays, the lints, and the inventory-ratchet
mechanics — lives in `crawl/lib.ts` + the three specs and never
branches on a stack name. Everything that legitimately differs
between Grafana deployments is declared as data in `crawl/stacks.ts`,
one `CrawlStackConfig` per stack: the default Grafana base URL, the
anonymous-auth assumption (typed `true` — the engine drives no login
flow, and the crawl proves the assumption live before walking), the
crawl scope rules, the per-stack inventory + exclusions file names,
the exact datasource UID set the ds/query replays pin (the live
`/api/datasources` answer must EQUAL it — provisioning drift fails
loudly), the lint input floors (floors on lint *input*, never
tolerances on verdicts), the lean representative seeds, and the hard
page caps. `CRAWL_STACK=<name>` selects the config: unset →
`playwright.config.ts` ignores `crawl/**` entirely (0 crawl tests);
an unknown name → loud error at config-load time, never a silent
skip.

Two stacks are registered: `compose` (the repo-root quickstart stack;
the `compose-smoke` job opts in with `CRAWL_STACK=compose` —
`SWEEP_DEPTH` is `lean` on push-to-main and `full` for release, nightly, or
explicit inventory qualification) and `k3d` (the `dashboard` job in `e2e.yml`; its crawl step
runs on schedule + manual dispatch only, always at
`SWEEP_DEPTH=full`). Ordinary pull requests run neither crawl; their required
Compose proof is the one-stack, browser-free `quickstart` context. Depth changes
states, never rules; `full` crawls exhaustively
under a **hard page cap that fails the run when exceeded**, so
surface growth forces a deliberate cap bump in `stacks.ts`.

**The `compose-smoke` matrix-shard split.** `compose-smoke` is the
slowest lane in the release gate, so it shards across a matrix of jobs. The three
heaviest specs — `iterate-panel-kiosk`, `compose_grafana_smoke`, and
`crawl/crawl` — are each a *single* async `test()` that loops
internally over every dashboard/panel/surface, so Playwright's native
`--shard` (which partitions at `test()` granularity, and CI pins
`workers: 1` in `playwright.config.ts`) leaves them whole and buys
nothing. The split is therefore **logical**: the spec FILES
are partitioned across a matrix of jobs, each booting its OWN isolated
compose stack, balanced by measured wall-clock weight. Whether the lane fans out
at all is the earlier event decision made by
`.github/scripts/compose-smoke-scope.mjs` in the `compose-smoke-scope`
job: ordinary pull requests omit, while push, nightly, release-head pull
requests, and explicit Compose inventory regeneration qualify. The aggregator
reads a skipped setup as green only when that job succeeded AND reported the
event out of scope, so a scope job that crashed fails instead of passing for the
wrong reason. The
partition, the explicit non-compose-smoke exclude list, and the
coverage assertion are the single source of truth in
`.github/scripts/compose-smoke-matrix.mjs` (`compose-smoke-setup`
verifies + emits the matrix; `compose-smoke-shard` runs each shard;
the aggregator job literally named `compose-smoke` needs the matrix
and gates on it so the required-check context still appears). Specs
are **discovered** (`git ls-files`), so a newly added `*.spec.ts` is a
hard CI failure until it is either assigned to a shard or named in the
exclude list — never a silent no-run. `crawl/crawl` uses a
`CRAWL_SHARD_INDEX`/`COUNT` contract: every crawl shard retains BFS
discovery, audits its deterministic owned slice, and uploads that slice
for one merge-and-ratchet step against the pinned inventory. The k3d
`dashboard` job uses the same contract with `CRAWL_STACK=k3d`.

**The surface-inventory ratchet.**
`crawl/grafana-surface-inventory.<stack>.json` pins each stack's
canonical visited set (mirroring `test/oracle/inventory/`'s regenerability
convention). A newly discovered surface — e.g. a Grafana bump adding
an app page — fails the crawl until the inventory is regenerated
deliberately:

```sh
# against a healthy instance of the named stack
CERBERUS_UPDATE_INVENTORY=1 SWEEP_DEPTH=full CRAWL_STACK=<stack> \
  npx playwright test crawl/crawl.spec.ts
```

Coverage shrink (a pinned surface no longer visited) fails
symmetrically and has no regen escape. The per-stack exclusions file
(`crawl/grafana-surface-exclusions.<stack>.json`) is empty by design
and shrink-biased; an entry must document genuine impossibility, and
a URL in both files fails as a stale exclusion.

**Adding a stack.** Register a `CrawlStackConfig` in
`crawl/stacks.ts`, commit an *empty* inventory (`"surfaces": []`,
`stack` field matching the config name, in canonical marshalled form)
plus an empty exclusions file, and wire `CRAWL_STACK=<name>` into the
stack's CI lane. The empty inventory is a **bootstrap convention,
never a steady state**: `assertInventoryBootstrapped` fails every run
loudly — with the regen instructions — until the first exhaustive
crawl's inventory is committed; only the regen run itself
(`CERBERUS_UPDATE_INVENTORY=1`) is exempt. Both registered stacks are
past this now — compose bootstrapped first, and the k3d stack
bootstrapped in tsouza/cerberus#1539 (booting k3d locally is heavy, so
that ran against a local `just e2e-up && just e2e-seed-rolling`
cluster).

**Regenerating from CI.** Both stacks' inventories can be regenerated
without booting anything locally: dispatch the `e2e` workflow
(`workflow_dispatch`) with `update_crawl_inventory` set to `k3d`,
`compose`, or `both`, then download the matching
`grafana-surface-inventory-<stack>` artifact and commit it through a
PR like any other generated file (`grafana-surface-inventory.compose.json`
is `-merge`-gated in `.gitattributes`, so it is never hand-edited).
The selected stack(s) get `CERBERUS_UPDATE_INVENTORY=1` and forced
`SWEEP_DEPTH=full` on their crawl shard — the compose lane otherwise
runs `SWEEP_DEPTH=full` only on the nightly `schedule` event, and a
regen dispatch below full depth would fail
`crawl.spec.ts`'s own exhaustive-crawl assertion by construction. The
default `none` selection leaves an ordinary dispatch behaving exactly
as it always has (tsouza/cerberus#1826). Deliberately regenerating
either stack's inventory after surface growth follows this path — the
red crawl lane in the interim is deliberate pressure that keeps
bootstrap (or a stale regen) from silently becoming permanent.

**Doctrine: the AI sweep generates oracle classes; CI runs them.**
The crawler exists because an off-CI AI screenshot sweep (2026-06-09)
found 34 unique error signatures across 55 BFS-visited pages —
several on surfaces no enumerated spec visits. Every find decomposed
into a deterministic signal once named; the AI's irreplaceable role
is *discovering which invariants to check*. When a future sweep (or a
human) finds a new bug class: name the deterministic signal, cite the
incident in a comment, implement it — as a universal per-page oracle
in `crawl/crawl.spec.ts` if it applies everywhere, or as a lint in
`crawl/lints.spec.ts` if it's an API-level data-quality rule — and
aggregate violations into the existing `failures[]` reporting. Never
per-surface tolerance lists: if a lint can't be deterministic without
one, narrow its scope by *consumption* (see the
histogram-degeneracy lint's quantile-panel scoping for the pattern).

## Regression meta-tests

`test/regression/` pins past CI failures so they can't silently
recur:

- `goleak_test.go` — process-level goroutine leak detection across
  every handler entrypoint.
- `justfile_test.go` — recipe-shape pins so a `just` syntax change
  doesn't silently break CI.
- `seed_test.go` — the e2e seed program's row-count and metric-name
  invariants.
- `generated_artifact_merge_gate_test.go` — pins the `.gitattributes`
  `-merge` gate on every generated baseline, golden and inventory, and
  fails on a new one that arrives ungated.

### Generated baselines never auto-merge

Every baseline, golden and inventory in this document is written by a
regeneration command, and most are sorted arrays of records. Git's
line-based merge blends two such files *without raising a conflict* when
both sides insert entries at nearby offsets: the record boundaries shift,
names pair with the next record's values, and the result still parses as
valid JSON of a plausible length while every blended entry is wrong. A
ratchet fed that file reports green while measuring nothing.

`.gitattributes` marks these paths `-merge`, so git refuses to blend them
and leaves the path conflicted instead — the only correct resolution being
to take the merged code and re-run the generator. That file is also where
the per-artefact regeneration command is recorded, so the conflict names
its own fix. `compatibility/parity-baseline/` is the one exception
worth knowing before you hit it: it is rebuilt from a compatibility run's
`compat-cases.json` artefact and therefore **cannot** be regenerated
locally.

#### `-merge` only protects a LOCAL git merge — verified 2026-08-04

`-merge` is a built-in git merge driver, honoured by any git CLIENT that
reads `.gitattributes`: a local `git merge`, `git rebase`, or `git pull`.
Cerberus's actual merge paths on GitHub are mostly SERVER-SIDE — the "Update
branch" button, `gh pr merge --squash`, and the mergeability precomputation
that decides whether a PR shows conflict-free — and nothing had verified
those honour `.gitattributes` at all (issue #1568, spun out of #1567 while
adding the gate above).

They do not. Verified empirically 2026-08-04: two throwaway branches pushed
to this repo, each inserting one new record at a different, non-overlapping
line offset into one `-merge`-guarded generated baseline off the same base
commit, produced a throwaway PR that `gh pr view --json mergeable` reported
as `"mergeable":"MERGEABLE"` — while a LOCAL `git merge` of the identical
branch pair, run in a scratch worktree, refused with "Cannot merge binary
files" / `CONFLICT (content)`, exactly as `.gitattributes` documents. The
throwaway PR was closed unmerged and both branches deleted immediately after.

Auditing every `-merge` path for what actually protects it turned up good
news: nearly all of them already carry a content-exact ratchet that
regenerates the artefact from source and diffs it against the committed file
— `TestCardinalityRatchet` / `TestSolverDecisionRatchet` / `TestScaleWallPin`
(`perf-guards`), `TestCatalogueIsRegenerable` and the surface-parity
inventory tests (`check`), `compat-ratchet.mjs` (`compatibility/*`),
`coverage-summary.mjs` (`coverage`), and the Tier-0 migration goldens via
`go test -tags=migration ./test/e2e/migration/tiers/tier0-offline/...`
(`lint`). Those are the strong "re-run the generator on the merge commit and
diff it" defence #1568 asked for, and they were already there for most of
the list. `check`, `coverage` and `lint` are REQUIRED and PR-blocking, so
those three ratchets stop a corrupted merge before it lands. `perf-guards`
and `compat-ratchet.mjs` (`compatibility/*`) are release-gate lanes (#2230):
they still run their real ratchet unconditionally on every push to `main`, so
a corrupted merge is still caught and reported, but no longer PRE-merge —
the defence is detection on the landed commit, not prevention of the landing.
That is an accepted, deliberate narrowing of this specific guarantee for
those two ratchets, traded for keeping them off the ordinary-PR critical
path; release.yml's preflight still refuses to publish past a red one.

The residual gap is procedural rather than a missing validator: branch
protection's "require branches to be up to date before merging" is OFF
(`strict: false` as of this writing), so a stale PR's squash-merge computes
its diff against whatever `main` has moved to WITHOUT re-running any of the
checks above against the resulting content. Turning `strict: true` on closes
that window — it forces the "Update branch" step, which re-runs every
required check (including all of the above) against the exact content that
will land — and is recommended to the maintainer as a follow-up; it is a
branch-protection admin setting, not a code change, so no PR flips it
unilaterally.

`.github/scripts/generated-baseline-structural-guard.mjs`, wired into the
required `forbid-skip` job, adds a fast, dependency-free structural
pre-filter over the subset of `-merge` paths whose shape was hand-checked
against the committed content: for a flat array, a unique key in sorted
order; for a sharded tree, the invariant only a tree can state — every
shard's key field equals the path it is filed under — and, on both shapes
where the records were verified uniform, one consistent field set. It is a cheap, always-on second
signal, not a replacement for the content-exact ratchets above — a
structural check cannot tell a value that is merely out of place from one
that is subtly wrong.

`test/e2e/playwright/crawl/grafana-surface-inventory.{compose,k3d}.json` are
the one `-merge` family that lands outside that list. Live content is reached
through the release-gated `dashboard` (k3d) and `compose-smoke` lanes on the
landed commit, release-head pull request, nightly run, or explicit regeneration.
An ordinary pull request performs no live crawl; its required `quickstart`
canary deliberately has no browser-depth work.
`.github/scripts/crawl-surface-inventory-guard.mjs` closes that in
the required `forbid-skip` job. The inventory's content is a live crawl result
that nothing offline can re-derive, but its committed FORM is not: every
inventory is written through `marshalInventory`
(`test/e2e/playwright/crawl/lib.ts`), whose output — keys `{doc, stack,
surfaces}`, rows `{url, lean}`, rows sorted by `byCodepoint` on `url`,
two-space indent, trailing newline — is a pure function of the content. The
guard rebuilds those bytes and demands equality, so a row out of order, a
duplicate row, a missing or extra field, a reordered key, a re-indent, a
dropped newline or a JSON-duplicate key all fail it. Stacks are discovered by
filename pattern, and discovering none is itself a failure, so the gate cannot
quietly disarm itself when a file moves.

What it does not prove is worth stating: a `lean` boolean paired with the
wrong `url`, still sorted and unique, is bytes the generator could legitimately
have emitted. `lean` has no offline source of truth, so that residue belongs to
`diffInventory` on a live stack — the `compose-smoke` crawl and the k3d
`dashboard` full crawl — and the guard's own header and failure text say so.
`crawl-surface-inventory-guard.test.mjs` carries a negative control per check
plus a pin on the documented blind spot, so the gate cannot rot into a rubber
stamp in either direction.

The form ratchet says nothing about whether the inventory's *content* is a
function of the Grafana application, and it was not. Whether a control keyed its
options verbatim was decided by counting how many options happened to render
against a size threshold, and an option count measures the seeded dataset: the
committed rows carried dashboard tags, detected severity levels, a label name and
a span-attribute name, so a full-depth run against a different seed legitimately
produced a different pin and the ratchet could not tell that from a regression.
`.github/scripts/crawl-surface-inventory-purity.mjs` closes the seed axis in the
same required `forbid-skip` job. It fails on any interaction fragment
(`<canonical>#<control>=<value>`) whose value is neither the representative
placeholder nor a member of a closed option set the control *declares* — either
because its discovery key already embeds the vocabulary (`radio[Grid|Rows]`,
`tabs[a|b|c]`, where the value half says nothing the key half has not) or because
`test/e2e/playwright/crawl/control-vocabularies.json` names it with a rationale.
Undeclared controls parameterize, so after the gate passes there is no literal in
the pin that came from a query result. That manifest is the same file
`crawl/interactions.ts` reads when it plans gestures — two copies would let the
gate certify a pin the crawler never produced — and the crawler re-checks every
declared set against the live app on each run, so a declaration that goes stale
fails the crawl loudly instead of quietly readmitting data.

The visit-order axis is the other half of the same property, and it is
deliberately *not* asserted over the committed bytes: `marshalInventory` sorts, so
re-sorting the rows and finding them unchanged is an assertion that cannot fail.
Order independence lives in the crawl's fold — a canonical's representative is
picked from the candidate set rather than from arrival order, and lean membership
is unioned across candidates rather than read off the fold winner — and it is
pinned by the browser-free `crawl: canonicalization pins` specs. Those ran only in
the de-gated compose crawl shard and the nightly k3d lane, so nothing gated them at
PR time; the `forbid-skip` job now runs them directly. No test in that grep takes
a `page` fixture, so the step installs `@playwright/test` with the browser download
suppressed and runs in about a second — no stack, no browser, and nothing off the
network beyond the seven packages `npm ci` fetches.
