# Tempo / TraceQL compatibility harness

TraceQL parity is measured by seeding reference Tempo and cerberus
from the same deterministic OTLP batch and diffing the wire responses
for every case in a cerberus-owned TXTAR corpus.

## Why this harness exists

Cerberus mirrors the posture taken by `compatibility/prometheus/` for
PromQL parity, but the Tempo / TraceQL surface has **no upstream
"tempo-compliance" repo**. The closest analogue is
`cmd/tempo-vulture`, Grafana's deterministic seed + read-back canary.
This harness vendors `cmd/tempo-vulture/` and `pkg/httpclient/` from
`grafana/tempo` as reference material under `upstream/`,
drives both backends from a cerberus-owned driver
(`driver/`), and writes JSON + markdown reports whose envelope matches
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
  scripts/
    run-tempo-compatibility.sh  `docker compose up --wait` + seed + diff + teardown
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
  upstream/             vendored grafana/tempo snapshot (read-only reference)
    LICENSE             AGPL-3.0, copied verbatim from grafana/tempo
    VERSION             exact upstream coordinates of the snapshot
    cmd/tempo-vulture/  long-running canary; reused as seeder pattern
    pkg/httpclient/     Tempo HTTP client; consumed by the driver
```

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

## What's in `upstream/`

A pure, unmodified snapshot of two paths from
`github.com/grafana/tempo` (`cmd/tempo-vulture/` and `pkg/httpclient/`),
taken directly from the upstream commit `go.mod` pins for the Apache
`pkg/tempopb` wire types.

Exact coordinates live in [`upstream/VERSION`](upstream/VERSION).

### Why vendor, not import via `go.mod`

`github.com/grafana/tempo` is already a direct require in `go.mod` (for
the Apache `pkg/tempopb` wire types), so we **could** just import
`pkg/httpclient` directly. We vendor instead because:

1. **Reference material, not a build dependency.** The driver imports
   only a narrow subset of this code; vendoring lets reviewers read
   the seeder pattern in the diff without grepping `~/go/pkg/mod/`.
2. **`cmd/tempo-vulture` is a `package main`**, not importable.
   Vendoring it keeps the source visible for the driver to adapt.
3. **Snapshot stability.** A future bump of the grafana/tempo version
   in go.mod won't silently move the seeder pattern under our feet; the
   snapshot here is pinned and explicit.

### Why the vendor isn't compiled

The vendor's imports (e.g. `github.com/go-test/deep`,
`go.uber.org/zap`, `github.com/jsternberg/zap-logfmt`,
`github.com/grafana/tempo/integration/util`) are not in cerberus's
`go.mod` and would pull in heavy transitive deps just to compile
vulture's `main` package. The vendor is excluded from the module
graph via a `go.mod` `ignore` directive:

```text
ignore ./compatibility/tempo/upstream
```

With it in place:

- `go build ./...` and `go test ./...` skip the vendor entirely.
- `go vet ./...` skips the vendor entirely.
- The vendored sources remain on disk for reference.

## Bump procedure

The vendor is **not** automatically refreshed when `go.mod`'s
`grafana/tempo` version bumps — a version bump rarely touches the
vendored `cmd/tempo-vulture/` and `pkg/httpclient/` paths. Do a
manual re-snapshot only when:

1. Upstream Grafana adds a meaningful capability to `cmd/tempo-vulture`
   or `pkg/httpclient` that the cerberus driver wants to mirror, **or**
2. The cerberus driver starts depending on a specific behaviour of
   the vendored code and we want a known-good baseline.

To re-snapshot:

```sh
# 1. Read the current grafana/tempo version in go.mod.
grep '^\s*github.com/grafana/tempo ' go.mod
#    => github.com/grafana/tempo vX.Y.Z-0.<timestamp>-<commit>

# 2. Clone grafana/tempo at that commit.
git clone --depth=1 https://github.com/grafana/tempo /tmp/tempo-upstream
git -C /tmp/tempo-upstream fetch --depth=1 origin <commit> && git -C /tmp/tempo-upstream checkout <commit>

# 3. Wipe the existing snapshot and re-copy.
rm -rf compatibility/tempo/upstream/{cmd,pkg,LICENSE}
mkdir -p compatibility/tempo/upstream/{cmd,pkg}
cp -r /tmp/tempo-upstream/cmd/tempo-vulture  compatibility/tempo/upstream/cmd/
cp -r /tmp/tempo-upstream/pkg/httpclient     compatibility/tempo/upstream/pkg/
cp    /tmp/tempo-upstream/LICENSE            compatibility/tempo/upstream/LICENSE

# 4. Update upstream/VERSION with the new grafana/tempo commit SHA and
#    describe.
$EDITOR compatibility/tempo/upstream/VERSION

# 5. PR the diff. Reviewer checks the bump procedure was followed
#    verbatim; no sanitisation of vendored sources is permitted.
```

## Licensing

`grafana/tempo` was relicensed from Apache-2.0 to **AGPL-3.0** in
[grafana/tempo#660](https://github.com/grafana/tempo/pull/660). The
vendored snapshot inherits AGPL-3.0, and
[`upstream/LICENSE`](upstream/LICENSE) is the verbatim copy.

Cerberus itself is independently licensed (see the repo root
`LICENSE`); the AGPL terms apply only to the vendored subtree under
`upstream/`. The driver code under `compatibility/tempo/driver/`
lives OUTSIDE `upstream/` and is cerberus-licensed.

## Related docs

- [`docs/compatibility.md`](../../docs/compatibility.md) — cross-head playbook
- [`docs/upstream-forks.md`](../../docs/upstream-forks.md) — the in-house TraceQL parser and the remaining watch-boundary forks
- [`compatibility/prometheus/`](../prometheus/) — sibling Prom harness
- [`compatibility/loki/`](../loki/) — sibling Loki harness
