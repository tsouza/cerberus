# Tempo / TraceQL compatibility harness

TraceQL parity is measured by seeding reference Tempo and cerberus
from the same deterministic OTLP batch and diffing the wire responses
for every case in a cerberus-owned TXTAR corpus.

## Why this harness exists

Cerberus mirrors the posture taken by `compatibility/prometheus/` for
PromQL parity, but the Tempo / TraceQL surface has **no upstream
"tempo-compliance" repo**. This harness seeds both backends and
drives them from a cerberus-owned driver (`driver/`), and writes JSON + markdown reports whose envelope matches
the Prom harness's so one downstream analyser handles both.

## Ingest path

Both backends are seeded from one in-memory OTLP fixture. Tempo
receives the OTLP push directly (it has no out-of-band ingest path);
cerberus receives a direct ClickHouse INSERT (cerberus is read-only —
its HTTP layer answers Prom / Loki / Tempo queries by reading from a
CH instance whose tables are populated by the OTel-CH exporter in
production). Both writes are derived from one in-memory fixture so
per-span fields stay 1:1 across the two read paths.

## Layout

```text
compatibility/tempo/
  README.md             this file
  docker-compose.yml    tempo + cerberus + clickhouse + driver
  tempo-config.yaml     reference Tempo (local block storage)
  driver/                       cerberus-owned driver binary
    Dockerfile          repo-root context multi-stage build
    main.go             subcommand dispatcher (seed / diff)
    seeder.go           OTLP push to Tempo + CH INSERT for cerberus
    corpus.go           TXTAR corpus loader
    differ.go           TraceID-keyed + relative-epsilon diff
    differ_metrics.go   metrics endpoint diff (samples / labels / timestamps)
    diff.go             `diff` subcommand: HTTP fetch + assertions + report
    corpus/             TXTAR cases — search / per-id / tags / tag-values / metrics
  reports/              driver report output (gitignored)
  UPSTREAM_VERSION      the grafana/tempo commit the harness tracks
```

The harness entry point (`node .github/scripts/run-tempo-compatibility.mjs`,
invoked by `just compat-traceql`) lives at the repo root alongside the
prometheus and loki harnesses and their shared
`.github/scripts/lib/compat-compose-lifecycle.mjs`, not under this
directory's own `scripts/`.

## Differ design

The differ runs each corpus case against both Tempo and cerberus via
`/api/search` (and `/api/traces/<id>` for per-id smoke cases, plus the
tag, tag-values, and metrics endpoints), then compares the responses.
Two complications drive the diff strategy:

1. **TraceIDs match byte-for-byte across backends.**
   `internal/api/tempo/handler.go::toTraceSummaries` emits the real
   hex(TraceId) from ClickHouse, so cerberus and Tempo return identical
   32-hex-char IDs for the same seeded span set. The differ keys its
   trace-summary multisets directly on `TraceSummary.TraceID` — no
   hashing, no canonicalisation. "Different orderings of equal sets
   don't false-positive" still holds because the index is a TraceID
   map, not a positional list.
2. **Per-case expectations are orthogonal to differential equality.** A
   case like `{ resource.service.name = "checkout" }` can carry an
   `expected_min_traces: 1` assertion ("each backend, on its own, must
   return at least one trace") AND still be subject to the structural
   diff between the two responses. The driver reports both in the
   markdown summary so a single regression doesn't mask the other.

Numeric fields (durationMs, startTimeUnixNano) compare under a 1e-9
relative epsilon.

## Corpus categories

The TXTAR corpus is the single source of truth for which Tempo HTTP
shapes the differ exercises.

| Category                | Endpoint kind        | Wire shape                                      | Diff strategy                                          |
| ----------------------- | -------------------- | ----------------------------------------------- | ------------------------------------------------------ |
| Attribute matchers      | `search`             | `{traces:[TraceSummary]}`                       | TraceID set diff (real hex IDs match byte-for-byte)    |
| Intrinsics              | `search`             | same                                            | same                                                   |
| Structural ops          | `search`             | same                                            | same                                                   |
| Set ops                 | `search`             | same                                            | same                                                   |
| Inner aggregates        | `search`             | same                                            | same                                                   |
| Nil comparisons         | `search`/`metrics_*` | same per endpoint                               | same per endpoint (incl. unspecified-kind boundary)    |
| Metrics pipeline        | `metrics_*`          | Prom series envelope                            | Semantic invariants + structural diff                  |
| Per-id round trip       | `traces`             | `{trace:{...}}`                                 | Trace-ID derivation from seeder template + status-2xx  |
| Tag names V1            | `tags_v1`            | `{tagNames:[string]}`                           | Set diff over the flat list                            |
| Tag names V2            | `tags_v2`            | `{scopes:[{name, tags}]}`                       | Set diff over flattened tags + per-scope-name diff     |
| Tag values V1           | `tag_values_v1`      | `{tagValues:[string]}`                          | Set diff over the flat list                            |
| Tag values V2           | `tag_values_v2`      | `{tagValues:[{type, value}]}`                   | Set diff over `Value` + `Type`-field diff on overlap   |

The tag endpoints pin three classes of subset assertion:

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

## Upstream pin

[`UPSTREAM_VERSION`](UPSTREAM_VERSION) records the `grafana/tempo` commit the
harness tracks. The reference image (`grafana/tempo:main-<commit>` in
`docker-compose.yml`) and the in-house wire types in
`internal/thirdparty/tempopb` are both taken from that commit, and
`test/regression/fork_version_skew_test.go` fails when either, or the
`grafana/tempo` requirement in `go.mod`, names a different one.

To move the harness to a new upstream commit, change all three together: the
`upstream_commit` and `upstream_describe` fields, the reference image tag, and
the `go.mod` requirement (with the `tsouza/tempo` fork rebased onto the same
commit, see [`docs/upstream-forks.md`](../../docs/upstream-forks.md)), then
re-sync `internal/thirdparty/tempopb` from that commit as its `README.md`
describes.

## Licensing

`grafana/tempo` is AGPL-3.0, except its `pkg/tempopb` wire types, which carry
their own Apache-2.0 licence. The reference Tempo runs as an unmodified
container image the harness pulls; no Tempo source code is committed here. The
driver under `compatibility/tempo/driver/` is cerberus code, licensed like the
rest of the repository, with one exception: the "Upstream-mined" cases in
`driver/corpus/smoke.txtar` are TraceQL query strings taken from the table
tests in `grafana/tempo`'s AGPL-3.0 `pkg/traceql`, each citing the test it came
from.

## Related docs

- [`docs/compatibility.md`](../../docs/compatibility.md) — cross-head playbook
- [`docs/upstream-forks.md`](../../docs/upstream-forks.md) — the in-house TraceQL parser and the remaining watch-boundary forks
- [`compatibility/prometheus/`](../prometheus/) — sibling Prom harness
- [`compatibility/loki/`](../loki/) — sibling Loki harness
