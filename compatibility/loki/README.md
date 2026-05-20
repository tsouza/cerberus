# Loki / LogQL compatibility harness

LogQL parity is measured by running reference Loki and cerberus
side-by-side against the same deterministic OTel-shape log fixture and
diffing the wire responses for every query in the corpus.

## Why this harness exists

Cerberus mirrors the posture taken by `compatibility/prometheus/` for
PromQL parity, but the Loki / LogQL surface has no upstream
`loki-conformance` repo. The closest analogue is
`grafana/loki:pkg/logql/bench/` — a YAML query corpus plus a
`TestRemoteStorageEquality` build-tagged Go test driver. This harness
vendors that snapshot verbatim under `upstream/loki-bench/`, drives
both backends from a cerberus-owned compliance tester
(`cmd/loki-compliance-tester/`), and emits a JSON report whose
envelope matches the Prom harness's so one downstream analyser handles
both.

## Layout

```text
compatibility/loki/
  README.md                       this file
  docker-compose.yml              clickhouse + reference loki + cerberus
  loki-config.yaml                reference Loki single-binary config
  cerberus-test-queries.yml       overlay: per-query drops + reasons
  dataset_metadata.json           pinned dataset metadata for ${SELECTOR}/${LABEL_*}
  reports/                        diff driver output (gitignored)
  cmd/
    seed/                         deterministic OTel-shape log seeder
    loki-compliance-tester/       cerberus-owned diff driver
  scripts/
    run-loki-compatibility.sh     compose + seed + diff + tear-down
  upstream/
    loki-bench/                   vendored grafana/loki:pkg/logql/bench snapshot
      LICENSE                     AGPL-3.0, copied verbatim from grafana/loki
      VERSION                     exact upstream coordinates of the snapshot
      query_registry.go           YAML loader + template-variable expander
      remote_test.go              upstream TestRemoteStorageEquality (vestigial)
      metadata.go                 DatasetMetadata + LoadMetadata + bounded sets
      metadata_resolver.go        ${SELECTOR}/${LABEL_*}/${RANGE} resolver
      testcase.go                 TestCase shape consumed by the registry
      assertions_test.go          assertResultNotEmpty + tolerance comparators
      convert_test.go             loghttp -> promql/parser.Value conversions
      generator.go                GeneratorConfig + StreamMetadata
      faker.go                    LogFormat type + helper data
      queries/
        schema.json               JSON schema for query YAMLs
        fast/*.yaml               minimal corpus (basic-selectors, simple-metrics, structured-metadata)
        regression/*.yaml         regression slice
        exhaustive/*.yaml         exhaustive slice
```

## Upstream corpus

The snapshot pins `grafana/loki` directly: the `tsouza/loki` fork
tracks `pkg/logql/syntax/`, `pkg/logql/log/`, and `pkg/logqlmodel/`,
so `pkg/logql/bench/` is outside the fork's watch boundary.

Vendored paths (the authoritative inventory is in
`upstream/loki-bench/VERSION`):

- `pkg/logql/bench/queries/fast/*.yaml`, `regression/*.yaml`,
  `exhaustive/*.yaml` — the query corpus.
- `pkg/logql/bench/queries/schema.json` — JSON Schema the YAMLs
  validate against.
- `pkg/logql/bench/query_registry.go` — `QueryRegistry` plus the
  `${SELECTOR}` / `${LABEL_NAME}` / `${LABEL_VALUE}` template
  expander.
- `pkg/logql/bench/remote_test.go` — upstream
  `TestRemoteStorageEquality`, build-tagged `remote_correctness`.
  Preserved verbatim as upstream reference; the cerberus-owned driver
  is now the active entry point.
- `metadata.go`, `metadata_resolver.go`, `testcase.go`,
  `assertions_test.go`, `convert_test.go`, `generator.go`, `faker.go`
  — support files `remote_test.go` transitively depends on.
- `LICENSE` — AGPL-3.0 from upstream, scoped to this subtree only.

### Why vendor, not import via `go.mod`

1. **Reference material, not a build dependency.** The driver wiring
   reads the vendored sources directly. Vendoring lets reviewers see
   the corpus and driver shape in the diff without cloning
   grafana/loki.
2. **`remote_test.go` is build-tagged `remote_correctness`.** It's a
   test driver, not library code; cerberus's main build doesn't need
   it.
3. **Snapshot stability.** A Loki transitive-dep bump cannot silently
   move the corpus shape — the snapshot is pinned explicitly here.

### How the vendor builds

The cerberus-owned driver (`cmd/loki-compliance-tester/`) imports the
vendored `bench` package for corpus loading (`QueryRegistry`,
`LoadMetadata`, `MetadataVariableResolver`, `TestCase`). The
transitive deps `bench` carries (`logproto`, `logql/syntax`,
`yaml.v3`) are already direct entries in the root `go.mod`, so the
driver builds with a plain `go build`.

The `ignore ./compatibility/loki/upstream` directive in `go.mod`
keeps `go build ./...`, `go test ./...`, and `go vet ./...` from
walking the vendored path as a build target; the bench package is
still resolvable when imported by path.

## Cerberus overlay files

Two files at the harness root capture cerberus-specific configuration
that lives OUTSIDE the AGPL `upstream/` boundary:

- `cerberus-test-queries.yml` — overlay listing per-query divergences
  cerberus tracks against the upstream corpus. Entries under
  `should_skip:` are suppressed before the wire call (recorded in the
  report as `skipReason` with no failure flag flipped);
  `should_fail:` is reserved for the Prom-shape `unexpectedSuccess`
  semantics (expected hard failures). Every entry requires a non-empty
  `reason:` plus a `jira:` reference; the CI gate at
  `scripts/check-skip-additions.sh` rejects new entries that omit
  either.
- `dataset_metadata.json` — pinned dataset metadata that maps
  `${SELECTOR}` / `${LABEL_NAME}` / `${LABEL_VALUE}` template vars to
  concrete values produced by the seeder under `cmd/seed/`.

## Running the harness

```sh
# Full lifecycle: compose up, seed, build driver, diff, tear down.
just compat-logql

# Smoke only (skip the diff driver — useful for bisecting the seeder).
DRIVER_SKIP=1 just compat-logql

# Keep the stack up after the run for manual poking.
just compat-logql-keep

# Instant-query mode (default is range).
DRIVER_RANGE_TYPE=instant just compat-logql

# Tear the stack down manually.
just compat-logql-down
```

The run script's exit codes:

- Exit 0 → no diffs on any query case (overlay-skipped cases count
  as passing).
- Exit 1 → at least one diff or run-time failure.
- Exit 2+ → harness itself failed (compose, seed, build).

The driver writes a structured JSON report to `reports/diff.json`
whose envelope matches `compatibility/prometheus/report.json`:

```json
{
  "totalResults": 14,
  "includePassing": true,
  "results": [
    {
      "testCase": {
        "query": "{service=\"checkout\"}",
        "source": "fast/basic-selectors.yaml",
        "description": "Basic label selector",
        "kind": "log", "direction": "backward",
        "start": "2026-05-15T00:00:00Z", "end": "2026-05-15T00:10:00Z",
        "instant": false
      },
      "diff": "",
      "unexpectedFailure": "",
      "unexpectedSuccess": false,
      "unsupported": false,
      "skipReason": ""
    }
  ]
}
```

Sharing the envelope with the Prom harness means one analyser (and
one expected-failures reconciliation script) consumes both.

## Licensing

`grafana/loki` is **AGPL-3.0**
([upstream LICENSE](https://github.com/grafana/loki/blob/main/LICENSE)).
The vendored snapshot inherits AGPL-3.0, and
[`upstream/loki-bench/LICENSE`](upstream/loki-bench/LICENSE) is the
verbatim copy.

Cerberus itself is independently licensed (see the repo root
`LICENSE`); the AGPL terms apply only to the vendored subtree under
`upstream/loki-bench/`. The driver scripts (`scripts/` plus `cmd/`)
live OUTSIDE `upstream/` and are cerberus-licensed.

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
rm -rf compatibility/loki/upstream/loki-bench/{queries,LICENSE,*.go}
mkdir -p compatibility/loki/upstream/loki-bench/queries/{fast,regression,exhaustive}
cp /tmp/loki-upstream/pkg/logql/bench/queries/fast/*.yaml \
   compatibility/loki/upstream/loki-bench/queries/fast/
cp /tmp/loki-upstream/pkg/logql/bench/queries/schema.json \
   compatibility/loki/upstream/loki-bench/queries/schema.json
for f in query_registry remote_test metadata metadata_resolver testcase \
         assertions_test convert_test generator faker; do
    cp "/tmp/loki-upstream/pkg/logql/bench/${f}.go" \
       "compatibility/loki/upstream/loki-bench/${f}.go"
done
cp /tmp/loki-upstream/LICENSE \
   compatibility/loki/upstream/loki-bench/LICENSE

# 4. Update upstream/loki-bench/VERSION with the new tag + commit SHA.
$EDITOR compatibility/loki/upstream/loki-bench/VERSION

# 5. PR the diff. Reviewer checks the bump procedure was followed
#    verbatim; no sanitisation of vendored sources is permitted.
```

## Related docs

- [`docs/compatibility.md`](../../docs/compatibility.md) — cross-head playbook
- [`docs/upstream-forks.md`](../../docs/upstream-forks.md) — how the `tsouza/loki` fork is wired (and why the bench corpus is outside it)
- [`compatibility/prometheus/`](../prometheus/) — sibling Prom harness
- [`compatibility/tempo/`](../tempo/) — sibling Tempo harness
