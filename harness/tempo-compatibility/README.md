# Tempo / TraceQL compatibility harness

> Status: **PR 6 (tags + tag-values)** of the rollout described in
> [`docs/tempo-compliance-plan.md`](../../docs/tempo-compliance-plan.md).
> This directory holds the vendored snapshot from PR 1 (`upstream/`),
> the Docker Compose stack standing up reference Tempo + cerberus +
> ClickHouse, the driver binary with two subcommands — `seed` (push
> deterministic OTLP batch to both backends, smoke `/api/traces/<id>`)
> and `diff` (run the TXTAR corpus through both backends, write a
> markdown diff report) — and a nightly / `workflow_dispatch` GitHub
> Actions lane (informational, not a required check). The smoke corpus
> now covers `/api/search`, `/api/traces/<id>`, and the four tag /
> tag-values endpoints (V1 + V2); the metrics endpoints
> (`/api/metrics/query_range` + `/api/metrics/query`) ship in PR 5.
>
> ## Ingest path (PR 3 vs the original plan)
>
> docs/tempo-compliance-plan.md "Open question 1" flagged a choice:
> seed cerberus via OTLP or via direct CH INSERT. **PR 3 settles on
> direct CH INSERT** because cerberus is read-only over OTLP — its
> HTTP layer answers Prom / Loki / Tempo queries by reading from a CH
> instance whose tables are populated by the OTel-CH exporter in real
> deployments. Running a full collector + exporter pipeline inside the
> harness just to seed would re-test the exporter (already covered
> upstream), not cerberus's read path. The Tempo side, by contrast,
> has no out-of-band ingest path and must take OTLP. Both writes are
> derived from one in-memory fixture so per-span fields stay 1:1
> across both read paths.

## Why this harness exists

Cerberus already has `harness/prometheus-compliance/` (PromQL parity vs reference
Prometheus) and a sibling shadow-mode harness for in-process diffs. The
Tempo / TraceQL surface has **no upstream "tempo-compliance" repo** — the
closest analogue is `cmd/tempo-vulture`, Grafana's deterministic seed +
read-back canary. The plan forks vulture's seeder pattern into a
cerberus-owned diff driver, similar to how `harness/prometheus-compliance/`
reuses `prometheus/compliance/promql/`.

See [`docs/tempo-compliance-plan.md`](../../docs/tempo-compliance-plan.md)
for the full landscape analysis, the per-PR breakdown, and the diff
strategy.

## Layout (current — PR 6)

```text
harness/tempo-compatibility/
  README.md             this file
  docker-compose.yml    tempo + cerberus + clickhouse + driver
  tempo-config.yaml     reference Tempo (local block storage)
  scripts/
    run-tempo-compatibility.sh  `docker compose up --wait` + seed + diff + teardown
  driver/                       cerberus-owned driver binary
    Dockerfile          repo-root context multi-stage build
    main.go             subcommand dispatcher (seed / diff)
    seeder.go           OTLP push to Tempo + CH INSERT for cerberus
    corpus.go           TXTAR corpus loader (PR 4 + PR 6 tag sections)
    differ.go           canonical-key + relative-epsilon diff (PR 4) +
                        tag-name / tag-values set diff (PR 6)
    diff.go             `diff` subcommand: HTTP fetch + assertions + report
    corpus/
      smoke.txtar       trace search + per-id (PR 4) + tag / tag-values
                        endpoints (PR 6)
  reports/              driver report output (gitignored)
  upstream/             PR 1 — vendored snapshot (read-only reference)
    LICENSE             AGPL-3.0, copied verbatim from grafana/tempo
    VERSION             exact upstream coordinates of the snapshot
    cmd/tempo-vulture/  long-running canary; reused as seeder pattern
    pkg/httpclient/     Tempo HTTP client; reused by the future driver
```

## Layout (planned — PRs 5 + 7)

```text
harness/tempo-compatibility/
  driver/
    corpus/
      coverage.txtar    PR 5 — metrics endpoints
  expected-failures.json  PR 5+
```

## Differ design (PR 4)

The differ runs each corpus case against both Tempo and cerberus via
`/api/search` (and `/api/traces/<id>` for per-id smoke cases), then
compares the responses. Two complications drive the diff strategy:

1. **Cerberus's `TraceSummary.TraceID` is synthetic today.** See
   `internal/api/tempo/handler.go::toTraceSummaries` — the stub key is
   `MetricName + "|" + Timestamp`, not the real OTLP trace ID hex
   Tempo returns. A literal byte-equal would false-positive on every
   case. The differ canonicalises each summary via
   `H(rootServiceName || rootTraceName)` and compares the canonical-key
   multisets, exactly the plan's "hash trace IDs deterministically
   (different orderings of equal sets don't false-positive)" prescription.
2. **Per-case expectations are orthogonal to differential equality.** A
   case like `{ resource.service.name = "checkout" }` can carry an
   `expected_min_traces: 1` assertion ("each backend, on its own, must
   return at least one trace") AND still be subject to the structural
   diff between the two responses. The driver reports both in the
   markdown summary so a single regression doesn't mask the other.

Numeric fields (durationMs, startTimeUnixNano) compare under a 1e-9
relative epsilon — same defaults as `harness/prometheus-compliance/shadow`.

## Corpus categories

The TXTAR corpus is the single source of truth for which Tempo HTTP
shapes the differ exercises. PR 4 lifted the trace-search categories
from the shadow harness; PR 6 added the four tag / tag-values
endpoints.

| Category                | Endpoint kind     | Wire shape                                      | Diff strategy                                          |
| ----------------------- | ----------------- | ----------------------------------------------- | ------------------------------------------------------ |
| Attribute matchers      | `search`          | `{traces:[TraceSummary]}`                       | Canonical-key (rootSvc, rootName) set diff             |
| Intrinsics              | `search`          | same                                            | same                                                   |
| Structural ops          | `search`          | same                                            | same                                                   |
| Set ops                 | `search`          | same                                            | same                                                   |
| Inner aggregates        | `search`          | same                                            | same                                                   |
| Metrics pipeline        | `search` (PR 5)   | Prom series envelope                            | Semantic invariants + structural diff (lands PR 5)     |
| Per-id round trip       | `traces`          | `{trace:{...}}`                                 | Trace-ID derivation from seeder template + status-2xx  |
| Tag names V1            | `tags_v1`         | `{tagNames:[string]}`                           | Set diff over the flat list                            |
| Tag names V2            | `tags_v2`         | `{scopes:[{name, tags}]}`                       | Set diff over flattened tags + per-scope-name diff     |
| Tag values V1           | `tag_values_v1`   | `{tagValues:[string]}`                          | Set diff over the flat list                            |
| Tag values V2           | `tag_values_v2`   | `{tagValues:[{type, value}]}`                   | Set diff over `Value` + `Type`-field diff on overlap   |

The PR 6 corpus pins three classes of subset assertion on the tag
endpoints:

- **Always-present keys** (`service.name`, `http.method`) — the seeder
  writes these on every span, so both backends MUST surface them in the
  tag-names lists.
- **Always-present values** (`checkout` / `payments` / `search` /
  `shipping` for `service.name`; `compat-test` for `deployment.env`) —
  the seeded value cardinality is exact, so a missing value is a real
  ingest / lowering regression.
- **Empty-result edge case** (`this.tag.does.not.exist`) — both
  backends must return an empty `tagValues` list, exercising the
  graceful-empty branches of each side's lookup code.

Cerberus today ignores the `?scope=` filter on `tags_v2` and returns
every scope regardless. PR 6's `tags_v2_resource` / `tags_v2_span` /
`tags_v2_intrinsic` cases will surface that gap as a `scope_mismatch`
reason in the markdown report — informational on the nightly job, not a
hard fail.

## What's in `upstream/`

A pure, unmodified snapshot of two paths from `github.com/grafana/tempo`,
brought in via the `github.com/tsouza/tempo` fork that already gates
cerberus's tempo dependency (see `docs/upstream-forks.md`). The
cerberus-accessors branch only patches `pkg/traceql/`; the two paths
vendored here (`cmd/tempo-vulture/` and `pkg/httpclient/`) are
byte-identical between the fork tag and the upstream commit it tracks.

Exact coordinates live in [`upstream/VERSION`](upstream/VERSION).

### Why vendor, not import via `go.mod`?

`github.com/grafana/tempo` is already in `go.mod` via the replace
directive that points at `github.com/tsouza/tempo`, so we **could**
just import `pkg/httpclient` directly. We vendor instead because:

1. **PR 1 is reference material, not a build dependency.** The future
   driver (PR 3) will decide which subset to import, copy outright,
   or rewrite. Vendoring lets reviewers read the seeder pattern in
   the diff without grepping `~/go/pkg/mod/`.
2. **`cmd/tempo-vulture` is a `package main`**, not importable. Vendoring
   it keeps the source visible for the driver author to adapt.
3. **Snapshot stability.** A future bump of the tsouza/tempo replace
   tag won't silently move the seeder pattern under our feet; the
   snapshot here is pinned and explicit.

### Why the vendor isn't compiled

The vendor's imports (e.g. `github.com/go-test/deep`, `go.uber.org/zap`,
`github.com/jsternberg/zap-logfmt`, `github.com/grafana/tempo/integration/util`)
are not in cerberus's `go.mod` and would pull in heavy transitive deps
just to compile vulture's `main` package. Until the driver (PR 3)
actually imports a subset of this code, we exclude the vendor from
the module graph via a `go.mod` `ignore` directive:

```text
ignore ./harness/tempo-compatibility/upstream
```

The directive is a Go 1.25+ feature; cerberus already pins Go 1.26
via `go.mod`. With it in place:

- `go build ./...` and `go test ./...` skip the vendor entirely.
- `go vet ./...` skips the vendor entirely.
- The vendored sources remain on disk for reference + future PRs.

## Bump procedure

The vendor is **not** automatically refreshed when `go.mod`'s
`tsouza/tempo` tag bumps — the cerberus-accessors branch only patches
`pkg/traceql/`, so a fork-tag bump rarely touches vendored paths. Do a
manual re-snapshot only when:

1. Upstream Grafana adds a meaningful capability to `cmd/tempo-vulture`
   or `pkg/httpclient` that the cerberus driver wants to mirror, **or**
2. The cerberus driver (PR 3+) starts depending on a specific behaviour
   of the vendored code and we want a known-good baseline.

To re-snapshot:

```sh
# 1. Read the current replace target in go.mod.
grep '^replace github.com/grafana/tempo' go.mod
#    => replace github.com/grafana/tempo => github.com/tsouza/tempo vX.Y.Z

# 2. Clone the fork at that tag.
git clone --depth=1 -b vX.Y.Z https://github.com/tsouza/tempo /tmp/tempo-upstream

# 3. Wipe the existing snapshot and re-copy.
rm -rf harness/tempo-compatibility/upstream/{cmd,pkg,LICENSE}
mkdir -p harness/tempo-compatibility/upstream/{cmd,pkg}
cp -r /tmp/tempo-upstream/cmd/tempo-vulture  harness/tempo-compatibility/upstream/cmd/
cp -r /tmp/tempo-upstream/pkg/httpclient     harness/tempo-compatibility/upstream/pkg/
cp    /tmp/tempo-upstream/LICENSE            harness/tempo-compatibility/upstream/LICENSE

# 4. Update upstream/VERSION with the new fork tag + commit SHA and
#    the matching upstream/main base commit.
$EDITOR harness/tempo-compatibility/upstream/VERSION

# 5. PR the diff. Reviewer checks the bump procedure was followed
#    verbatim; no sanitisation of vendored sources is permitted.
```

## Licensing

`grafana/tempo` was relicensed from Apache-2.0 to **AGPL-3.0** in
[grafana/tempo#660](https://github.com/grafana/tempo/pull/660). The
vendored snapshot inherits AGPL-3.0, and [`upstream/LICENSE`](upstream/LICENSE)
is the verbatim copy.

Cerberus itself is independently licensed (see the repo root `LICENSE`);
the AGPL terms apply only to the vendored subtree under `upstream/`.
The driver code that lands in PRs 3+ will live OUTSIDE `upstream/`
(under `harness/tempo-compatibility/driver/`) and is cerberus-licensed.

## Related docs

- [`docs/tempo-compliance-plan.md`](../../docs/tempo-compliance-plan.md) — the rollout plan
- [`docs/upstream-forks.md`](../../docs/upstream-forks.md) — how the `tsouza/tempo` fork is wired
- [`harness/prometheus-compliance/`](../prometheus-compliance/) — sibling harness, the template this one mirrors
