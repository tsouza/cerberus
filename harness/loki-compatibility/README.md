# Loki / LogQL compatibility harness

> Status: **PR 2 (vendor `pkg/logql/bench`)** of the rollout described in
> [`docs/loki-compliance-plan.md`](../../docs/loki-compliance-plan.md).
> This directory currently holds **only** a vendored snapshot of
> Grafana's `pkg/logql/bench/` corpus + driver scaffolding files.
> There is no Compose stack, no diff runner script, and no CI yet —
> those follow in PRs 3 and 4.

## Why this harness exists

Cerberus already has `harness/prometheus-compliance/` (PromQL parity vs
reference Prometheus). The Loki / LogQL surface has no upstream
`loki-conformance` repo; the closest analogue is `grafana/loki:pkg/logql/bench/` —
a YAML query corpus + `TestRemoteStorageEquality` build-tagged Go test
driver. This harness vendors that snapshot verbatim (the same posture
`harness/prometheus-compliance/upstream/` takes vs `prometheus/compliance`),
runs reference Loki + cerberus side-by-side, and diffs the responses.

See [`docs/loki-compliance-plan.md`](../../docs/loki-compliance-plan.md)
for the full landscape analysis, the per-PR breakdown, and the
license/AGPL containment strategy.

## Layout (current — PR 2)

```text
harness/loki-compatibility/
  README.md                       this file
  cerberus-test-queries.yml       overlay documenting drops for unsupported LogQL features
  dataset_metadata.json           pinned dataset metadata for ${SELECTOR}/${LABEL_*} expansion
  upstream/
    loki-bench/                   vendored grafana/loki:pkg/logql/bench snapshot
      LICENSE                     AGPL-3.0, copied verbatim from grafana/loki
      VERSION                     exact upstream coordinates of the snapshot
      query_registry.go           YAML loader + template-variable expander
      remote_test.go              TestRemoteStorageEquality (build-tagged remote_correctness)
      queries/
        schema.json               JSON schema for query YAMLs
        fast/*.yaml               minimal corpus (basic-selectors, simple-metrics, structured-metadata)
```

## Layout (planned — PRs 1, 3, 4)

PR 1 (in parallel, Pool-CA) lands the seed + Compose scaffold:

```text
harness/loki-compatibility/
  docker-compose.yml              PR 1 — clickhouse + reference loki + cerberus + seeder
  loki-config.yaml                PR 1 — reference Loki single-binary config
  test-cerberus.yml               PR 1 — addr-1 (loki) + addr-2 (cerberus) endpoint config
  cmd/seed/                       PR 1 — deterministic OTel-shape log seeder
  cmd/discover/                   later  — dataset_metadata.json regenerator
  scripts/
    run-loki-compatibility.sh     PR 3 — go test -tags=remote_correctness driver
```

PR 4 lands the informational CI lane:

```text
.github/workflows/loki-compatibility.yml  PR 4 — push:main + nightly cron + workflow_dispatch
```

## What's in `upstream/loki-bench/`

A pure, unmodified snapshot of these paths from `grafana/loki` at the tag
recorded in [`upstream/loki-bench/VERSION`](upstream/loki-bench/VERSION):

- `pkg/logql/bench/queries/fast/*.yaml` — the minimal-coverage corpus
  (basic-selectors, simple-metrics, structured-metadata). `regression/`
  + `exhaustive/` slices are deferred to PR 6 of the plan.
- `pkg/logql/bench/queries/schema.json` — JSON Schema the YAMLs validate against.
- `pkg/logql/bench/query_registry.go` — `QueryRegistry` + `${SELECTOR}` /
  `${LABEL_NAME}` / `${LABEL_VALUE}` template expander.
- `pkg/logql/bench/remote_test.go` — `TestRemoteStorageEquality`
  build-tagged `remote_correctness`. Reused verbatim in PR 3 to drive
  the diff loop; replaced by a cerberus-owned driver later (plan PR 5).
- `LICENSE` — AGPL-3.0 from upstream, scoped to this subtree only.

Cerberus's `tsouza/loki` fork (the replace target in `go.mod`) only
tracks `pkg/logql/syntax/`, `pkg/logql/log/`, and `pkg/logqlmodel/` —
`pkg/logql/bench/` is outside the fork's watch boundary, so the
snapshot here pins `grafana/loki` directly rather than the fork tag.

### Why vendor, not import via `go.mod`?

Same reasoning as `harness/tempo-compatibility/upstream/` (see PR #367):

1. **PR 2 is reference material, not a build dependency.** The driver
   wiring lands in PR 3 and can decide which subset to import vs
   adapt. Vendoring lets reviewers read the corpus + driver shape in
   the diff without cloning grafana/loki.
2. **`remote_test.go` is build-tagged `remote_correctness`.** It's a
   test driver, not library code; cerberus's main build doesn't need it.
3. **Snapshot stability.** A future bump of any Loki transitive dep
   won't silently move the corpus shape under us — the snapshot is
   pinned explicitly here.

### Why the vendor isn't compiled

`remote_test.go` imports `github.com/grafana/loki/v3/pkg/logcli/client`
+ `github.com/grafana/loki/v3/pkg/loghttp`, which pull in heavy
transitive deps (the Loki client, dskit, etc.) just to run the diff
loop. Until PR 3 wires the driver explicitly, we exclude the vendor
from the module graph via a `go.mod` `ignore` directive:

```text
ignore ./harness/loki-compatibility/upstream
```

The directive is a Go 1.25+ feature; cerberus already pins Go 1.26
via `go.mod`. With it in place:

- `go build ./...` / `go test ./...` / `go vet ./...` skip the vendor.
- The vendored sources stay on disk for reference + PR 3 to adapt.

## Cerberus overlay files

Two files at the harness root capture cerberus-specific configuration
that lives OUTSIDE the AGPL `upstream/` boundary:

- **`cerberus-test-queries.yml`** — overlay applied on top of the
  vendored corpus. The `should_skip:` list documents which queries
  exercise LogQL features cerberus doesn't implement yet (e.g. v2
  engine forward log queries, dataobj-engine structured-metadata
  fast-paths). PR 3's runner consumes this; the initial commit lists
  zero entries — reviewers add as PR 4's CI lane surfaces gaps.
- **`dataset_metadata.json`** — pinned dataset metadata that maps
  `${SELECTOR}` / `${LABEL_NAME}` / `${LABEL_VALUE}` template vars to
  concrete values seeded by PR 1's deterministic seeder. Once PR 1
  lands, this file gets regenerated against the seeded fixture; the
  placeholder here records the shape and reasonable defaults so the
  vendor PR doesn't depend on PR 1's seed shape landing first.

## Licensing

`grafana/loki` is **AGPL-3.0** ([upstream LICENSE](https://github.com/grafana/loki/blob/main/LICENSE)).
The vendored snapshot inherits AGPL-3.0, and [`upstream/loki-bench/LICENSE`](upstream/loki-bench/LICENSE)
is the verbatim copy.

Cerberus itself is independently licensed (see the repo root `LICENSE`);
the AGPL terms apply only to the vendored subtree under `upstream/loki-bench/`.
The driver scripts that land in PRs 3-4 live OUTSIDE `upstream/`
(under `scripts/` + `cmd/`) and are cerberus-licensed.

## Bump procedure

Re-snapshot when:

1. Upstream Loki adds queries to `queries/fast/` that cerberus wants
   to cover, **or**
2. The shape of `QueryRegistry` / `remote_test.go` changes meaningfully
   (e.g. new template var, new diff semantics), **or**
3. The `should_skip:` overlay drifts because upstream renamed/removed
   a query we previously skipped.

To re-snapshot:

```sh
# 1. Pick the new tag (typically matches the `loki:X.Y.Z` Docker image
#    cerberus runs as the reference target in docker-compose.yml).
TAG=v3.7.1

# 2. Shallow clone at that tag.
git clone --depth=1 -b "$TAG" https://github.com/grafana/loki /tmp/loki-upstream

# 3. Wipe + re-copy the vendored paths.
rm -rf harness/loki-compatibility/upstream/loki-bench/{queries,LICENSE,query_registry.go,remote_test.go}
mkdir -p harness/loki-compatibility/upstream/loki-bench/queries/fast
cp /tmp/loki-upstream/pkg/logql/bench/queries/fast/*.yaml \
   harness/loki-compatibility/upstream/loki-bench/queries/fast/
cp /tmp/loki-upstream/pkg/logql/bench/queries/schema.json \
   harness/loki-compatibility/upstream/loki-bench/queries/schema.json
cp /tmp/loki-upstream/pkg/logql/bench/query_registry.go \
   harness/loki-compatibility/upstream/loki-bench/query_registry.go
cp /tmp/loki-upstream/pkg/logql/bench/remote_test.go \
   harness/loki-compatibility/upstream/loki-bench/remote_test.go
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
