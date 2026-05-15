# Loki / LogQL compatibility harness

> Status: **PR 3 (diff driver wiring)** of the rollout described in
> [`docs/loki-compliance-plan.md`](../../docs/loki-compliance-plan.md).
> The Docker Compose stack, the deterministic seeder, the vendored
> `pkg/logql/bench/` corpus, and now the diff-driver wiring all live
> under this directory. The informational CI lane (PR 4) and the
> cerberus-owned driver replacement (PR 5) are still pending.

## Why this harness exists

Cerberus already has `harness/prometheus-compliance/` (PromQL parity vs
reference Prometheus). The Loki / LogQL surface has no upstream
`loki-conformance` repo; the closest analogue is
`grafana/loki:pkg/logql/bench/` — a YAML query corpus plus a
`TestRemoteStorageEquality` build-tagged Go test driver. This harness
vendors that snapshot verbatim (the same posture
`harness/prometheus-compliance/upstream/` takes vs `prometheus/compliance`),
runs reference Loki and cerberus side-by-side, and diffs the responses.

See [`docs/loki-compliance-plan.md`](../../docs/loki-compliance-plan.md)
for the full landscape analysis, the per-PR breakdown, and the
license/AGPL containment strategy.

## Layout (current — PR 3)

```text
harness/loki-compatibility/
  README.md                       this file
  docker-compose.yml              clickhouse + reference loki + cerberus
  loki-config.yaml                reference Loki single-binary config
  cerberus-test-queries.yml       overlay documenting drops for unsupported LogQL features
  dataset_metadata.json           pinned dataset metadata for ${SELECTOR}/${LABEL_*} expansion
  reports/                        diff driver output (PR 3); contents gitignored
  cmd/
    seed/                         deterministic OTel-shape log seeder
  scripts/
    run-loki-compatibility.sh     smoke + diff driver (this PR)
  upstream/
    loki-bench/                   vendored grafana/loki:pkg/logql/bench snapshot
      LICENSE                     AGPL-3.0, copied verbatim from grafana/loki
      VERSION                     exact upstream coordinates of the snapshot
      query_registry.go           YAML loader + template-variable expander
      remote_test.go              TestRemoteStorageEquality (build-tagged remote_correctness)
      metadata.go                 DatasetMetadata + LoadMetadata + bounded sets (PR 3)
      metadata_resolver.go        ${SELECTOR}/${LABEL_*}/${RANGE} resolver (PR 3)
      testcase.go                 TestCase shape consumed by the registry (PR 3)
      assertions_test.go          assertResultNotEmpty + tolerance comparators (PR 3)
      convert_test.go             loghttp -> promql/parser.Value conversions (PR 3)
      generator.go                GeneratorConfig + StreamMetadata referenced by metadata.go (PR 3)
      faker.go                    LogFormat type + helper data referenced by metadata.go (PR 3)
      queries/
        schema.json               JSON schema for query YAMLs
        fast/*.yaml               minimal corpus (basic-selectors, simple-metrics, structured-metadata)
        regression/.gitkeep       placeholder; the driver loads all three suites
        exhaustive/.gitkeep       placeholder; the driver loads all three suites
```

## Layout (planned — PR 4+)

PR 4 lands the informational CI lane:

```text
.github/workflows/loki-compatibility.yml  PR 4 — push:main + nightly cron + workflow_dispatch
```

PR 5 ports `TestRemoteStorageEquality` into a cerberus-owned driver:

```text
harness/loki-compatibility/cmd/loki-compliance-tester/  PR 5 — cerberus-owned driver
```

## What's in `upstream/loki-bench/`

A pure, unmodified snapshot of these paths from `grafana/loki` at the
tag recorded in [`upstream/loki-bench/VERSION`](upstream/loki-bench/VERSION):

- `pkg/logql/bench/queries/fast/*.yaml` — the minimal-coverage corpus
  (basic-selectors, simple-metrics, structured-metadata). The
  `regression/` and `exhaustive/` slices are deferred to PR 6 of the
  plan; the empty placeholder dirs exist so the driver's
  three-suite loader doesn't fatal on a missing path.
- `pkg/logql/bench/queries/schema.json` — JSON Schema the YAMLs
  validate against.
- `pkg/logql/bench/query_registry.go` — `QueryRegistry` plus the
  `${SELECTOR}` / `${LABEL_NAME}` / `${LABEL_VALUE}` template
  expander.
- `pkg/logql/bench/remote_test.go` — `TestRemoteStorageEquality`,
  build-tagged `remote_correctness`. Driven by
  `scripts/run-loki-compatibility.sh`; replaced by a cerberus-owned
  driver later (plan PR 5).
- `pkg/logql/bench/metadata.go`, `metadata_resolver.go`, `testcase.go`,
  `assertions_test.go`, `convert_test.go` — support files
  `remote_test.go` transitively depends on. PR 2 (#369) intentionally
  shipped a partial vendor (driver + corpus only); PR 3 expanded the
  snapshot to the full set so the `go test -c` build resolves
  cleanly without sanitising upstream sources.
- `pkg/logql/bench/generator.go`, `faker.go` — referenced by
  `metadata.go` for the `GeneratorConfig` / `StreamMetadata` /
  `LogFormat` type surface. Never invoked at run time by
  `TestRemoteStorageEquality`; included verbatim so the package
  type-checks without local patches.
- `LICENSE` — AGPL-3.0 from upstream, scoped to this subtree only.

Cerberus's `tsouza/loki` fork (the replace target in `go.mod`) only
tracks `pkg/logql/syntax/`, `pkg/logql/log/`, and `pkg/logqlmodel/` —
`pkg/logql/bench/` is outside the fork's watch boundary, so the
snapshot here pins `grafana/loki` directly rather than the fork tag.

### Why vendor, not import via `go.mod`?

Same reasoning as `harness/tempo-compatibility/upstream/` (see PR #367):

1. **Reference material, not a build dependency.** The driver wiring
   reads the vendored sources directly. Vendoring lets reviewers see
   the corpus and driver shape in the diff without cloning
   grafana/loki.
2. **`remote_test.go` is build-tagged `remote_correctness`.** It's a
   test driver, not library code; cerberus's main build doesn't need
   it.
3. **Snapshot stability.** A future bump of any Loki transitive dep
   won't silently move the corpus shape under us — the snapshot is
   pinned explicitly here.

### How the vendor builds

`remote_test.go` imports `github.com/grafana/loki/v3/pkg/logcli/client`
and `github.com/grafana/loki/v3/pkg/loghttp`, which transitively pull
in the Loki client + dskit + aws-sdk-go-v2 + azure-sdk + OpenAPI
chain. These are not deps cerberus's main module wants in its
`go.mod`. The repo isolates them via two complementary mechanisms:

1. `go.mod`'s `ignore ./harness/loki-compatibility/upstream` directive
   keeps `go build ./...`, `go test ./...`, and `go vet ./...` away
   from the vendored path.
2. `scripts/run-loki-compatibility.sh` invokes
   `GOFLAGS=-mod=mod go test -tags=remote_correctness -c` to compile
   the test binary explicitly. The `-mod=mod` flag is the documented
   override for ignored paths; it transiently writes the extra
   transitive deps into the repo's `go.mod` and `go.sum` so the build
   resolves. A cleanup trap then reverts both files (`git checkout --
   go.mod go.sum`) so the working tree stays clean on every exit path
   (success, driver failure, `set -e` abort, SIGINT).

The `ignore` directive is a Go 1.25+ feature; cerberus pins Go 1.26
via `go.mod`. The cleanup pattern matches the
`harness/prometheus-compliance/scripts/run-compatibility.sh` precedent
for managing transient mutations during integration runs.

## Cerberus overlay files

Two files at the harness root capture cerberus-specific configuration
that lives OUTSIDE the AGPL `upstream/` boundary:

- `cerberus-test-queries.yml` — overlay listing per-query divergences
  cerberus tracks against the upstream corpus. The PR 3 commit
  documents the entire `fast/` set as deferred to PR 6 (selector
  vocabulary mismatch between the seeded fixture and the upstream
  template defaults). The PR 5 cerberus-owned driver will consume
  this file; until then it's documentary surface for reviewers.
- `dataset_metadata.json` — pinned dataset metadata that maps
  `${SELECTOR}` / `${LABEL_NAME}` / `${LABEL_VALUE}` template vars to
  concrete values. The placeholder shipped by PR 2 (#369) is
  preserved verbatim — it predates the PR 1 seeder's actual label
  vocabulary, which is the gap PR 6 closes by extending the seeder
  and regenerating this file via `cmd/discover/`.

## Running the harness

```sh
# Full lifecycle: compose up, seed, build driver, diff, tear down.
just loki-compatibility

# Smoke only (skip the diff driver — useful for bisecting the seeder).
DRIVER_SKIP=1 just loki-compatibility

# Keep the stack up after the run for manual poking.
just loki-compatibility-keep

# Instant-query mode (default is range).
DRIVER_RANGE_TYPE=instant just loki-compatibility

# Tear the stack down manually.
just loki-compatibility-down
```

The run script's contract:

- Exit 0 → no diffs on any query case.
- Exit 1 → at least one diff or run-time failure (informational; the
  CI lane in PR 4 will treat this as non-blocking until the corpus
  shape matches the seeded fixture per PR 6).
- Exit 2+ → harness itself failed (compose, seed, build).

The driver's full stream (`go test -v` output, one
`PASS`/`FAIL`/`SKIP` line per query case) is written to
`reports/diff.json`. PR 5's cerberus-owned driver will switch this to
the same JSON shape that
`harness/prometheus-compliance/report.json` uses so the two harnesses
share a single downstream analyser.

## Licensing

`grafana/loki` is **AGPL-3.0** ([upstream LICENSE](https://github.com/grafana/loki/blob/main/LICENSE)).
The vendored snapshot inherits AGPL-3.0, and
[`upstream/loki-bench/LICENSE`](upstream/loki-bench/LICENSE) is the
verbatim copy.

Cerberus itself is independently licensed (see the repo root
`LICENSE`); the AGPL terms apply only to the vendored subtree under
`upstream/loki-bench/`. The driver scripts (under `scripts/` plus
`cmd/`) live OUTSIDE `upstream/` and are cerberus-licensed.

## Bump procedure

Re-snapshot when:

1. Upstream Loki adds queries to `queries/fast/` that cerberus wants
   to cover, **or**
2. The shape of `QueryRegistry` / `remote_test.go` / any support file
   changes meaningfully (new template var, new diff semantics, new
   transitive dep), **or**
3. The `should_skip:` overlay drifts because upstream renamed or
   removed a query we previously skipped.

To re-snapshot:

```sh
# 1. Pick the new tag (typically matches the `loki:X.Y.Z` Docker image
#    cerberus runs as the reference target in docker-compose.yml).
TAG=v3.7.1

# 2. Shallow clone at that tag.
git clone --depth=1 -b "$TAG" https://github.com/grafana/loki /tmp/loki-upstream

# 3. Wipe + re-copy the vendored paths. The `vendored_paths:` block in
#    upstream/loki-bench/VERSION is the canonical inventory.
rm -rf harness/loki-compatibility/upstream/loki-bench/{queries,LICENSE,*.go}
mkdir -p harness/loki-compatibility/upstream/loki-bench/queries/fast \
         harness/loki-compatibility/upstream/loki-bench/queries/regression \
         harness/loki-compatibility/upstream/loki-bench/queries/exhaustive
touch    harness/loki-compatibility/upstream/loki-bench/queries/regression/.gitkeep \
         harness/loki-compatibility/upstream/loki-bench/queries/exhaustive/.gitkeep
cp /tmp/loki-upstream/pkg/logql/bench/queries/fast/*.yaml \
   harness/loki-compatibility/upstream/loki-bench/queries/fast/
cp /tmp/loki-upstream/pkg/logql/bench/queries/schema.json \
   harness/loki-compatibility/upstream/loki-bench/queries/schema.json
for f in query_registry remote_test metadata metadata_resolver testcase \
         assertions_test convert_test generator faker; do
    cp "/tmp/loki-upstream/pkg/logql/bench/${f}.go" \
       "harness/loki-compatibility/upstream/loki-bench/${f}.go"
done
cp /tmp/loki-upstream/LICENSE \
   harness/loki-compatibility/upstream/loki-bench/LICENSE

# 4. Update upstream/loki-bench/VERSION with the new tag + commit SHA.
$EDITOR harness/loki-compatibility/upstream/loki-bench/VERSION

# 5. PR the diff. Reviewer checks the bump procedure was followed
#    verbatim; no sanitisation of vendored sources is permitted.
```

## Related docs

- [`docs/loki-compliance-plan.md`](../../docs/loki-compliance-plan.md) — the rollout plan
- [`docs/upstream-forks.md`](../../docs/upstream-forks.md) — how the `tsouza/loki` fork is wired (and why the bench corpus is outside it)
- [`harness/prometheus-compliance/`](../prometheus-compliance/) — sibling Prom harness
- [`harness/tempo-compatibility/`](../tempo-compatibility/) — sibling Tempo harness, PR 1 (#367)
